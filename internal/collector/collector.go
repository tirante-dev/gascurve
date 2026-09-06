// Package collector follows Arbitrum Nitro chains: one Follower per network
// samples the precompiles every tick, fetches the headers it missed, replays
// the pricer with re-anchoring, folds buckets, records owner actions, batch
// posting reports and L1 pricer state, runs a resumable backfill and
// publishes a LiveSnapshot with NOTIFY after each tick.
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
	HeadersByNumbers(ctx context.Context, numbers []uint64) ([]nitro.Header, error)
	HeaderByNumber(ctx context.Context, number uint64) (*nitro.Header, error)
	BlocksWithTxs(ctx context.Context, numbers []uint64) ([]nitro.Block, error)
	OwnerActsLogs(ctx context.Context, from, to uint64) ([]nitro.Log, error)
	L1Sample(ctx context.Context) (*nitro.L1Sample, error)
	FeeAccounts(ctx context.Context) (*nitro.FeeAccounts, error)
	ArbOSVersion(ctx context.Context) (uint64, error)
	BlockNumber(ctx context.Context) (uint64, error)
	Stats() nitro.Stats
	Available() int
}

var _ RPC = (*nitro.Client)(nil)

const (
	// maxCatchUpFactor bounds a single tick's catch-up to this many header
	// batches; a larger gap is skipped so the follower never falls behind
	// forever on a budget below the chain's block rate.
	maxCatchUpFactor = 10
	// initialBlocks is how many blocks are fetched on a fresh database.
	initialBlocks = 10
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
	chainID uint64

	catchingUp atomic.Bool

	mu              sync.Mutex
	initialized     bool
	head            uint64
	prevTs          uint64
	state           *pricer.State
	lastSample      *nitro.Sample
	lastResult      *pricer.Result
	sets            []db.ConstraintSet
	minFeeChanges   []minFeeChange
	l1              *model.L1
	accounts        *model.Accounts
	slowPending     bool
	ownerScanDone   bool
	observedChecked bool
	snapshot        *model.LiveSnapshot
}

type minFeeChange struct {
	block uint64
	fee   *big.Int
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
	if f.cfg.HeaderBatchSize <= 0 || f.cfg.HeaderBatchSize > nitro.MaxBatch {
		f.cfg.HeaderBatchSize = nitro.MaxBatch
	}
	return f
}

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
	last, err := f.store.LatestBlock(ctx, f.chainID)
	if err != nil {
		return fmt.Errorf("latest block: %w", err)
	}
	if last != nil {
		f.head = last.Number
		f.prevTs = uint64(last.TS.Unix())
	}
	if err := f.reloadSetsLocked(ctx); err != nil {
		return err
	}
	f.initialized = true
	return nil
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

// minFeeAt returns the minimum base fee in force at a block, falling back
// to the live value.
func (f *Follower) minFeeAt(number uint64, live *big.Int) *big.Int {
	fee := live
	for _, c := range f.minFeeChanges {
		if c.block <= number {
			fee = c.fee
		}
	}
	if fee == nil {
		return new(big.Int)
	}
	return new(big.Int).Set(fee)
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
