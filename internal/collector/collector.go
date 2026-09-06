// Package collector follows Arbitrum Nitro chains: one Follower per network
// samples the precompiles every tick, fetches the headers it missed, replays
// the pricer forward from a known state with re-anchoring, rebuilds buckets,
// records owner actions, batch posting reports and L1 pricer state, runs a
// resumable backfill and publishes a LiveSnapshot with NOTIFY after each
// tick.
package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/logger"
	"github.com/tirante-dev/gascurve/internal/model"
	"github.com/tirante-dev/gascurve/internal/nitro"
	"github.com/tirante-dev/gascurve/internal/pricer"
)

// RPC is the subset of nitro.Client the collector uses, so loops can be
// unit-tested with a fake.
type RPC interface {
	FastSample(ctx context.Context) (*nitro.Sample, error)
	FastSampleAt(ctx context.Context, number uint64) (*nitro.Sample, error)
	HeadersByNumbers(ctx context.Context, numbers []uint64) ([]nitro.Header, error)
	HeaderByNumber(ctx context.Context, number uint64) (*nitro.Header, error)
	BlocksWithTxs(ctx context.Context, numbers []uint64) ([]nitro.Block, error)
	OwnerActsLogs(ctx context.Context, from, to uint64) ([]nitro.Log, error)
	L1Sample(ctx context.Context) (*nitro.L1Sample, error)
	FeeAccounts(ctx context.Context) (*nitro.FeeAccounts, error)
	ArbOSVersion(ctx context.Context) (uint64, error)
	BlockNumber(ctx context.Context) (uint64, error)
	ChainID(ctx context.Context) (uint64, error)
	Stats() nitro.Stats
	Available() int
}

var _ RPC = (*nitro.Client)(nil)

// HeadSource delivers newHeads events, normally a nitro.HeadSubscriber.
// Connected tells the fast loop whether to trust it or poll on the timer.
type HeadSource interface {
	Run(ctx context.Context, fn func(nitro.Head))
	Connected() bool
}

var _ HeadSource = (*nitro.HeadSubscriber)(nil)

const (
	// defaultCatchUpBatches is the fallback for collector.max_catch_up_batches.
	defaultCatchUpBatches = 10
	// defaultAnchorInterval is the fallback for collector.backfill_anchor_interval.
	defaultAnchorInterval = 1000
	// ownerLogChunk is the widest eth_getLogs range.
	ownerLogChunk = 100_000
	// smallChainBlocks: chains below this head start the owner scan at 0.
	smallChainBlocks = 100_000_000
	// largeChainLookback: bigger chains start the owner scan this far back.
	largeChainLookback = 50_000_000
	// genesisBlockLimit: constraint sets from owner actions at or below this
	// block are the chain's genesis configuration.
	genesisBlockLimit = 1_000
	// batchScanLimit caps the two-transaction blocks inspected per slow tick.
	batchScanLimit = 100
	// batchFetchChunk is the full-transaction batch size (weighted heavier
	// by public endpoints).
	batchFetchChunk = 20
	// minBackfillBatch is the smallest header batch the backfill sends.
	minBackfillBatch = 5
	// backfillIdle is the pause when the backfill has nothing to do.
	backfillIdle = time.Second
	// restartDelay is the pause before a failed follower is restarted.
	restartDelay = 5 * time.Second
	// maxReorgDepth bounds the common-ancestor search on a reorg.
	maxReorgDepth = 128
	// boundaryWidth is the widest bucket: buckets from the hour of the
	// first live block on are rebuilt from block rows.
	boundaryWidth = time.Hour
)

// Options configure a Follower.
type Options struct {
	Network   config.NetworkConfig
	Collector config.CollectorConfig
	RPC       RPC
	Store     db.Store
	Log       *logger.Logger
	Now       func() time.Time
	Sleep     func(context.Context, time.Duration) error
	// Heads overrides the newHeads source. Nil with a Network.WSURL means a
	// nitro.HeadSubscriber for that URL; nil without one means polling.
	Heads HeadSource
}

// Follower drives one network.
type Follower struct {
	net     config.NetworkConfig
	cfg     config.CollectorConfig
	rpc     RPC
	store   db.Store
	log     *logger.Logger
	now     func() time.Time
	sleep   func(context.Context, time.Duration) error
	heads   HeadSource
	chainID uint64

	catchingUp atomic.Bool

	mu          sync.Mutex
	initialized bool
	// head, headHash, prevTs, state and lastResult describe the last
	// committed block; they are only published after the transaction that
	// wrote it committed.
	head            uint64
	headHash        string
	prevTs          uint64
	state           *pricer.State
	lastSample      *nitro.Sample
	lastResult      *pricer.Result
	sets            []db.ConstraintSet
	minFeeChanges   []minFeeChange
	liveStart       *liveStart
	l1              *model.L1
	accounts        *model.Accounts
	slowPending     bool
	ownerScanDone   bool
	observedChecked bool
	cursorChecked   bool
	snapshot        *model.LiveSnapshot
}

type minFeeChange struct {
	block uint64
	fee   *big.Int
}

// liveStart is the first block the live loop stored (collector_state
// live_start). Buckets from the hour containing it on are rebuilt from
// block rows; older buckets belong to the backfill's additive folds.
type liveStart struct {
	Block uint64 `json:"block"`
	TS    int64  `json:"ts"`
}

// hole is a block range the replay skipped (collector_state holes).
type hole struct {
	From uint64 `json:"from"`
	To   uint64 `json:"to"`
	At   string `json:"at"`
}

// NewFollower builds a follower; the network's chain id is the key for
// every table.
func NewFollower(o Options) *Follower {
	f := &Follower{
		net:     o.Network,
		cfg:     o.Collector,
		rpc:     o.RPC,
		store:   o.Store,
		log:     o.Log,
		now:     o.Now,
		sleep:   o.Sleep,
		heads:   o.Heads,
		chainID: o.Network.ChainID,
	}
	if f.log == nil {
		f.log = logger.Nop()
	}
	f.log = f.log.With("network", o.Network.Name, "chainId", o.Network.ChainID)
	if f.now == nil {
		f.now = time.Now
	}
	if f.sleep == nil {
		f.sleep = sleepContext
	}
	if f.heads == nil && o.Network.WSURL != "" {
		f.heads = nitro.NewHeadSubscriber(o.Network.WSURL, nitro.WithHeadLogger(f.log))
	}
	if f.cfg.HeaderBatchSize <= 0 || f.cfg.HeaderBatchSize > nitro.MaxBatch {
		f.cfg.HeaderBatchSize = nitro.MaxBatch
	}
	if f.cfg.MaxCatchUpBatches <= 0 {
		f.cfg.MaxCatchUpBatches = defaultCatchUpBatches
	}
	if f.cfg.BackfillAnchorInterval <= 0 {
		f.cfg.BackfillAnchorInterval = defaultAnchorInterval
	}
	return f
}

// unlimited reports whether the network has no call budget, which turns
// off gap skipping and the two-transaction batch report prefilter.
func (f *Follower) unlimited() bool { return f.net.Unlimited() }

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Snapshot returns the last published snapshot, if any.
func (f *Follower) Snapshot() *model.LiveSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshot
}

// Head returns the last stored block number.
func (f *Follower) Head() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.head
}

// ensureInit registers the network and loads persisted state once.
func (f *Follower) ensureInit(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.initialized {
		return nil
	}
	if err := f.store.UpsertNetwork(ctx, db.Network{
		ChainID: f.chainID, Name: f.net.Name, DisplayName: f.net.DisplayName, ExplorerURL: f.net.ExplorerURL, Enabled: f.net.Enabled,
	}); err != nil {
		return fmt.Errorf("register network: %w", err)
	}
	if err := f.reloadHeadLocked(ctx); err != nil {
		return err
	}
	if err := f.reloadSetsLocked(ctx); err != nil {
		return err
	}
	if err := f.loadLiveStartLocked(ctx); err != nil {
		return err
	}
	f.initialized = true
	return nil
}

// reloadHeadLocked reads the last committed block from the database and
// forgets the in-memory replay state, so the next tick rebuilds it from
// the stored row. Used at start and after a failed commit.
func (f *Follower) reloadHeadLocked(ctx context.Context) error {
	last, err := f.store.LatestBlock(ctx, f.chainID)
	if err != nil {
		return fmt.Errorf("latest block: %w", err)
	}
	f.head, f.headHash, f.prevTs, f.state, f.lastResult = 0, "", 0, nil, nil
	if last != nil {
		f.head = last.Number
		f.headHash = last.Hash
		f.prevTs = uint64(last.TS.Unix())
	}
	return nil
}

// loadLiveStartLocked loads the live_start checkpoint. A database written
// before the checkpoint existed derives it from the oldest stored block.
func (f *Follower) loadLiveStartLocked(ctx context.Context) error {
	raw, ok, err := f.store.GetState(ctx, f.chainID, db.StateLiveStart)
	if err != nil {
		return err
	}
	if ok {
		ls := &liveStart{}
		if err := json.Unmarshal([]byte(raw), ls); err != nil {
			return fmt.Errorf("live start: %w", err)
		}
		f.liveStart = ls
		return nil
	}
	oldest, err := f.store.OldestBlock(ctx, f.chainID)
	if err != nil {
		return err
	}
	if oldest == nil {
		return nil
	}
	ls := &liveStart{Block: oldest.Number, TS: oldest.TS.Unix()}
	if err := f.saveLiveStart(ctx, f.store, ls); err != nil {
		return err
	}
	f.liveStart = ls
	return nil
}

func (f *Follower) saveLiveStart(ctx context.Context, s db.Store, ls *liveStart) error {
	b, err := json.Marshal(ls)
	if err != nil {
		return err
	}
	return s.SetState(ctx, f.chainID, db.StateLiveStart, string(b))
}

// boundaryLocked returns the start of the hour containing the first live
// block: buckets from there on are row-backed.
func (f *Follower) boundaryLocked() (time.Time, bool) {
	if f.liveStart == nil {
		return time.Time{}, false
	}
	return time.Unix(f.liveStart.TS, 0).UTC().Truncate(boundaryWidth), true
}

// recordHole appends a skipped range to collector_state holes.
func (f *Follower) recordHole(ctx context.Context, s db.Store, h hole) error {
	raw, ok, err := s.GetState(ctx, f.chainID, db.StateHoles)
	if err != nil {
		return err
	}
	holes := make([]hole, 0, 1)
	if ok {
		if err := json.Unmarshal([]byte(raw), &holes); err != nil {
			f.log.Warn("unreadable holes checkpoint, starting over", "err", err.Error())
			holes = nil
		}
	}
	h.At = f.now().UTC().Format(time.RFC3339)
	holes = append(holes, h)
	b, err := json.Marshal(holes)
	if err != nil {
		return err
	}
	return s.SetState(ctx, f.chainID, db.StateHoles, string(b))
}

// reloadSetsLocked refreshes the constraint set and min fee caches.
func (f *Follower) reloadSetsLocked(ctx context.Context) error {
	sets, err := f.store.ConstraintSets(ctx, f.chainID)
	if err != nil {
		return fmt.Errorf("constraint sets: %w", err)
	}
	f.sets = sets
	actions, err := f.store.OwnerActions(ctx, f.chainID, time.Time{}, time.Time{}, 0)
	if err != nil {
		return fmt.Errorf("owner actions: %w", err)
	}
	f.minFeeChanges = f.minFeeChanges[:0]
	for i := len(actions) - 1; i >= 0; i-- { // ascending by block
		a := actions[i]
		if a.Method != "setMinimumL2BaseFee" {
			continue
		}
		var args struct {
			PriceInWei string `json:"priceInWei"`
		}
		if err := a.Args.Unmarshal(&args); err != nil {
			continue
		}
		if fee, ok := new(big.Int).SetString(args.PriceInWei, 10); ok {
			f.minFeeChanges = append(f.minFeeChanges, minFeeChange{block: a.BlockNumber, fee: fee})
		}
	}
	return nil
}

// setIDFor returns the constraint set in force at a block.
func (f *Follower) setIDFor(number uint64) sql.NullInt64 {
	var out sql.NullInt64
	for _, s := range f.sets {
		if s.EffectiveBlock <= number {
			out = sql.NullInt64{Int64: s.ID, Valid: true}
		}
	}
	return out
}

// setAt returns the constraint set in force at a block, or nil.
func (f *Follower) setAt(number uint64) *db.ConstraintSet {
	var out *db.ConstraintSet
	for i := range f.sets {
		if f.sets[i].EffectiveBlock <= number {
			out = &f.sets[i]
		}
	}
	return out
}

// minFeeAt returns the minimum base fee in force at a block from the
// recorded setMinimumL2BaseFee actions, with nitro's genesis default before
// the first recorded change. It never uses the live value.
func (f *Follower) minFeeAt(number uint64) *big.Int {
	fee := big.NewInt(pricer.InitialMinimumBaseFeeWei)
	for _, c := range f.minFeeChanges {
		if c.block <= number {
			fee = c.fee
		}
	}
	return new(big.Int).Set(fee)
}

// minFeeChangeBlock returns the block of the last recorded change at or
// before number (0 when none).
func (f *Follower) minFeeChangeBlock(number uint64) uint64 {
	var block uint64
	for _, c := range f.minFeeChanges {
		if c.block <= number {
			block = c.block
		}
	}
	return block
}

// stateFromSample builds a pricer state carrying the sampled backlogs.
func stateFromSample(s *nitro.Sample) *pricer.State {
	st := &pricer.State{MinBaseFee: new(big.Int)}
	if s.MinBaseFee != nil {
		st.MinBaseFee.Set(s.MinBaseFee)
	}
	if s.IsLegacy() {
		if s.Legacy != nil {
			st.Legacy = &pricer.Legacy{SpeedLimit: s.Legacy.SpeedLimit, Inertia: s.Legacy.Inertia, Tolerance: s.Legacy.Tolerance, Backlog: s.Legacy.Backlog}
		}
		return st
	}
	st.Constraints = make([]pricer.Constraint, len(s.Constraints))
	for i, c := range s.Constraints {
		st.Constraints[i] = pricer.Constraint{Target: c.Target, Window: c.Window, Backlog: c.Backlog}
	}
	return st
}

// stateFromEntries builds a pricer state for a constraint set document,
// backlogs at the set's starting values.
func stateFromEntries(entries []model.ConstraintSetEntry, minFee *big.Int) *pricer.State {
	st := &pricer.State{MinBaseFee: new(big.Int).Set(minFee), Constraints: make([]pricer.Constraint, len(entries))}
	for i, e := range entries {
		st.Constraints[i] = pricer.Constraint{Target: e.Target, Window: e.Window, Backlog: e.StartingBacklog}
	}
	return st
}

// sampleBacklogs returns the live backlogs of a sample.
func sampleBacklogs(s *nitro.Sample) []uint64 {
	if s.IsLegacy() {
		if s.Legacy == nil {
			return nil
		}
		return []uint64{s.Legacy.Backlog}
	}
	out := make([]uint64, len(s.Constraints))
	for i, c := range s.Constraints {
		out[i] = c.Backlog
	}
	return out
}

// sameShape reports whether the state's parameters match the sample's.
func sameShape(st *pricer.State, s *nitro.Sample) bool {
	if st == nil {
		return false
	}
	if s.IsLegacy() {
		if !st.IsLegacy() || st.Legacy == nil || s.Legacy == nil {
			return false
		}
		return st.Legacy.SpeedLimit == s.Legacy.SpeedLimit && st.Legacy.Inertia == s.Legacy.Inertia && st.Legacy.Tolerance == s.Legacy.Tolerance
	}
	if len(st.Constraints) != len(s.Constraints) {
		return false
	}
	for i, c := range s.Constraints {
		if st.Constraints[i].Target != c.Target || st.Constraints[i].Window != c.Window {
			return false
		}
	}
	return true
}

// setEntries converts a constraint set document into pricer constraints.
func setEntries(cs db.ConstraintSet) ([]model.ConstraintSetEntry, error) {
	var entries []model.ConstraintSetEntry
	if err := cs.Constraints.Unmarshal(&entries); err != nil {
		return nil, fmt.Errorf("constraint set %d: %w", cs.ID, err)
	}
	return entries, nil
}

// sameEntries compares a set document with live constraints.
func sameEntries(entries []model.ConstraintSetEntry, live []nitro.Constraint) bool {
	if len(entries) != len(live) {
		return false
	}
	for i, e := range entries {
		if e.Target != live[i].Target || e.Window != live[i].Window {
			return false
		}
	}
	return true
}

func entriesJSON(entries []model.ConstraintSetEntry) db.JSONB {
	if entries == nil {
		entries = []model.ConstraintSetEntry{}
	}
	b, _ := json.Marshal(entries)
	return db.JSONB(b)
}
