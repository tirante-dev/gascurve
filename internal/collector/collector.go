// Package collector follows Arbitrum Nitro chains: one Follower per network
// samples the precompiles every tick, fetches the headers it missed, replays
// the pricer forward from a known state with re-anchoring, rebuilds buckets,
// records owner actions, batch posting reports and L1 pricer state, refills
// the ranges it had to skip, runs a resumable backfill and publishes a
// LiveSnapshot with NOTIFY after each tick.
package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/logger"
	"github.com/tirante-dev/gascurve/internal/model"
	"github.com/tirante-dev/gascurve/internal/nitro"
	"github.com/tirante-dev/gascurve/internal/pricer"
	"github.com/tirante-dev/gascurve/internal/prices"
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

// ArchiveRPC serves historical state: FastSampleAt at any block, not just
// the last few minutes. The backfill anchors and the archive minimum fee
// samples go through it.
type ArchiveRPC interface {
	FastSampleAt(ctx context.Context, number uint64) (*nitro.Sample, error)
}

// EndpointPool is what a nitro.Pool adds to RPC: chain id verification of
// every endpoint, capability routing (the endpoint serving newHeads, the
// one serving historical state), the active endpoint's call policy and the
// routing state for /status. Capability routing is managed: only verified
// endpoints are used, and both paths move to the next capable endpoint
// when the one they used fails.
type EndpointPool interface {
	RPC
	Verify(ctx context.Context) error
	// HasWS reports whether any endpoint is configured with a ws_url.
	HasWS() bool
	// WSEndpoint leases the WebSocket endpoint to dial next, verifying it
	// first, so a subscriber rebinds when its endpoint is disabled. The
	// lease carries the failures back, so an endpoint whose socket does
	// not work is cooled down and the next attempt resolves another one.
	WSEndpoint(ctx context.Context) (*nitro.WSLease, error)
	// Archive returns the managed historical-state path, nil when no
	// endpoint serves it.
	Archive() *nitro.ArchivePool
	Status() nitro.PoolStatus
	// Policy is the active endpoint's call policy.
	Policy() nitro.Policy
}

var (
	_ EndpointPool = (*nitro.Pool)(nil)
	_ ArchiveRPC   = (*nitro.ArchivePool)(nil)
	_ ArchiveRPC   = (*nitro.Endpoint)(nil)
)

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
	// genesisBlockLimit: constraint sets from owner actions at or below this
	// block are the chain's genesis configuration.
	genesisBlockLimit = 1_000
	// batchScanLimit caps the two-transaction blocks inspected per slow tick.
	batchScanLimit = 100
	// batchFetchChunk is the full-transaction batch size (weighted heavier
	// by public endpoints).
	batchFetchChunk = 20
	// minBackfillBatch is the smallest header batch the backfill sends:
	// with less spare budget than this it queues for it at the pacer
	// rather than shrinking the batch further or idling.
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

// Owner methods that change the pricer.
const (
	methodSetMinimumL2BaseFee      = "setMinimumL2BaseFee"
	methodSetSpeedLimit            = "setSpeedLimit"
	methodSetL2GasPricingInertia   = "setL2GasPricingInertia"
	methodSetL2GasBacklogTolerance = "setL2GasBacklogTolerance"
)

// Options configure a Follower.
type Options struct {
	Network   config.NetworkConfig
	Collector config.CollectorConfig
	// RPC serves ordinary calls: a nitro.Pool in production. When it is an
	// EndpointPool the follower verifies every endpoint at start and, unless
	// Archive or Heads override them, routes anchors to the pool's archive
	// endpoint and heads to its WebSocket endpoint.
	RPC RPC
	// Archive serves the backfill anchors and archive minimum fee samples.
	// Nil means the pool's archive endpoint, or, for a plain RPC, RPC itself
	// when Network.Archive is set; otherwise the backfill is a pure replay.
	Archive ArchiveRPC
	Store   db.Store
	Log     *logger.Logger
	Now     func() time.Time
	Sleep   func(context.Context, time.Duration) error
	// Heads overrides the newHeads source. Nil means a nitro.HeadSubscriber
	// for the pool's WebSocket endpoint (or Network.WSURL with a plain
	// RPC); polling when there is none.
	Heads HeadSource
}

// Follower drives one network.
type Follower struct {
	net     config.NetworkConfig
	cfg     config.CollectorConfig
	rpc     RPC
	pool    EndpointPool // rpc when it is a pool, else nil
	archive ArchiveRPC   // nil without an archive endpoint
	store   db.Store
	log     *logger.Logger
	now     func() time.Time
	sleep   func(context.Context, time.Duration) error
	heads   HeadSource
	chainID uint64
	// ethUsd is the process-wide ETH/USD cache, nil when the source is
	// disabled or unusable.
	ethUsd *ethUsdCache
	// tickInterval is the fast loop's cadence: the network's tick_interval
	// when set, else collector.tick_interval.
	tickInterval time.Duration

	catchingUp atomic.Bool
	// behind is how many blocks the stored head trailed the sampled head at
	// the last tick. History work (the gap filler and the backfill) waits
	// while it exceeds a header batch: on a small budget their batches
	// queue ahead of the catch-up's and turn a lag into a skipped gap.
	behind atomic.Uint64

	mu          sync.Mutex
	initialized bool
	// head, headHash, prevTs, state, lastSample and lastResult describe
	// the last committed tick; they are only published after the
	// transaction that wrote it committed.
	head          uint64
	headHash      string
	prevTs        uint64
	state         *pricer.State
	lastSample    *nitro.Sample
	lastResult    *pricer.Result
	sets          []db.ConstraintSet
	minFeeChanges []minFeeChange
	legacyChanges []legacyChange
	liveStart     *liveStart
	// ownerScanThrough is the owner_scan_through checkpoint: the block
	// through which the recorded owner-action timeline is complete, 0 until
	// a scan pass has reached its head.
	ownerScanThrough uint64
	// scanOrigin is the owner_scan_origin checkpoint, nil for a chain
	// scanned from genesis.
	scanOrigin *scanOrigin
	l1         *model.L1
	accounts   *model.Accounts
	// ethUsdPrice is the last quote the slow loop obtained, published in a
	// tick only while it is younger than cfg.EthUsdMaxAge.
	ethUsdPrice *prices.Price
	// slowGen counts the slow samples taken and slowSaved the newest one a
	// tick has persisted. A tick clears only the generation it wrote, so a
	// sample published while its transaction ran is still persisted by the
	// next tick.
	slowGen       uint64
	slowSaved     uint64
	cursorChecked bool
	snapshot      *model.LiveSnapshot
}

// minFeeChange is a recorded setMinimumL2BaseFee.
type minFeeChange struct {
	block uint64
	fee   *big.Int
}

// legacyChange is a recorded legacy pricer parameter change: the method
// names the parameter.
type legacyChange struct {
	block  uint64
	method string
	value  uint64
}

// setChange is a constraint set taking effect at a block; a set with the
// same shape as its predecessor still resets the backlogs to its starting
// values, exactly as setGasPricingConstraints does.
type setChange struct {
	block   uint64
	entries []model.ConstraintSetEntry
}

// liveStart is the first block the live loop stored (collector_state
// live_start). Buckets from the hour containing it on are rebuilt from
// block rows; older buckets belong to the backfill's additive folds.
type liveStart struct {
	Block uint64 `json:"block"`
	TS    int64  `json:"ts"`
}

// hole is a block range the collector did not index (collector_state
// holes). A hole without a reason is queued work: the gap filler replays
// it forward from the state at the block before it, tracking its progress
// in Next and carrying the replay state in State. A hole with
// reasonNoState has nothing to replay from and is only re-examined when a
// stored state appears before it; one with reasonExpired left the queue
// because it was full.
type hole = model.Hole

// Reasons a hole is not queued work.
const (
	reasonNoState = model.HoleReasonNoState
	reasonExpired = model.HoleReasonExpired
)

// scanOrigin is where a deliberately truncated owner scan began
// (collector_state owner_scan_origin). With Archive the complete pricer
// state in force at the end of Block was sampled there: the minimum base
// fee, and either a constraint set recorded at Block+1 with the sampled
// backlogs or, on a legacy chain, Legacy. History from Block+1 on then
// replays from real values. Without an archive endpoint nothing before
// Block can be priced and the range is recorded as a hole.
type scanOrigin struct {
	Block      uint64 `json:"block"`
	MinBaseFee string `json:"minBaseFee,omitempty"`
	Archive    bool   `json:"archive"`
	// Legacy is the sampled legacy pricer state at the end of Block
	// (parameters and backlog), nil on a constraints chain or without an
	// archive endpoint.
	Legacy *model.LegacyParams `json:"legacy,omitempty"`
}

// replayFrom is the first block a replay may start at: the origin sample
// is end-of-block state for Block, whose gas is already counted in it, so
// the block after it is the first one to replay.
func (o *scanOrigin) replayFrom() uint64 {
	if o == nil {
		return 0
	}
	return o.Block + 1
}

// fullState reports whether the origin carries a complete pricer state, so
// history above it can be replayed rather than guessed.
func (o *scanOrigin) fullState() bool { return o != nil && o.Archive && o.MinBaseFee != "" }

// fee returns the sampled minimum base fee, nil when none was sampled.
func (o *scanOrigin) fee() *big.Int {
	if o == nil || o.MinBaseFee == "" {
		return nil
	}
	v, ok := new(big.Int).SetString(o.MinBaseFee, 10)
	if !ok {
		return nil
	}
	return v
}

// NewFollower builds a follower; the network's chain id is the key for
// every table.
func NewFollower(o Options) *Follower {
	f := &Follower{
		net:     o.Network,
		cfg:     o.Collector,
		rpc:     o.RPC,
		archive: o.Archive,
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
	if p, ok := o.RPC.(EndpointPool); ok {
		f.pool = p
	} else {
		// A plain client: the network's own capabilities apply to it.
		if f.heads == nil && o.Network.WSURL != "" {
			f.heads = nitro.NewHeadSubscriber(o.Network.WSURL, nitro.WithHeadLogger(f.log))
		}
		if f.archive == nil && o.Network.Archive {
			f.archive = o.RPC
		}
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
	if f.cfg.EthUsdMaxAge <= 0 {
		f.cfg.EthUsdMaxAge = defaultEthUsdMaxAge
	}
	if f.cfg.EthUsdSource != "" {
		f.ethUsd = ethUsdCacheFor(f.cfg.EthUsdSource, f.cfg.SlowInterval, f.log)
	}
	f.tickInterval = o.Network.EffectiveTickInterval(f.cfg)
	return f
}

// TickInterval returns the fast loop's cadence for this network.
func (f *Follower) TickInterval() time.Duration { return f.tickInterval }

// bindPool routes capabilities once the pool's endpoints are verified.
// Neither binding names an endpoint: the head subscriber asks the pool for
// a verified WebSocket endpoint before every connection attempt, so it
// rebinds when the one it followed is disabled or stops verifying, and the
// archive path picks and verifies its endpoint per call, failing over to
// the next one that serves historical state. Explicit Options win and a
// binding is made only once, so a restarted follower keeps its subscriber.
func (f *Follower) bindPool() {
	if f.heads == nil {
		if f.pool.HasWS() {
			f.heads = nitro.NewHeadSubscriber("", nitro.WithHeadLogger(f.log), nitro.WithHeadLease(f.pool.WSEndpoint))
			f.log.Info("newHeads routed to the pool's verified WebSocket endpoints")
		} else {
			f.log.Info("no endpoint has a ws_url, polling on the timer")
		}
	}
	if f.archive == nil {
		if ar := f.pool.Archive(); ar != nil {
			f.archive = ar
			f.log.Info("backfill anchors routed to the pool's archive endpoints")
		} else {
			f.log.Info("no archive endpoint, backfill is a pure replay")
		}
	}
}

// endpointsStatus renders the pool's routing state for /status. A disabled
// endpoint carries the sanitized reason it was disabled for; nothing here
// is derived from a URL.
func endpointsStatus(st nitro.PoolStatus) model.EndpointsStatus {
	out := model.EndpointsStatus{ActiveEndpoint: st.Active, Failovers: st.Failovers, Endpoints: make([]model.EndpointStatus, len(st.Endpoints))}
	for i, e := range st.Endpoints {
		out.Endpoints[i] = model.EndpointStatus{Index: e.Index, WS: e.WS, Archive: e.Archive, Disabled: e.Disabled, WSCooling: e.WSCooling}
		if e.Error != "" {
			msg := e.Error
			out.Endpoints[i].Error = &msg
		}
		if e.WSError != "" {
			msg := e.WSError
			out.Endpoints[i].WSError = &msg
		}
	}
	return out
}

// endpointError summarizes the disabled endpoints of a pool for
// networks.last_error, so a network that keeps running on a fallback still
// reports the mismatch as an error. It names indexes and reasons only,
// never a URL.
func endpointError(st nitro.PoolStatus) string {
	var parts []string
	for _, e := range st.Endpoints {
		if e.Disabled {
			parts = append(parts, fmt.Sprintf("endpoint %d disabled: %s", e.Index, e.Error))
		}
	}
	return strings.Join(parts, "; ")
}

// endpointGen identifies the endpoint an operation's policy came from:
// the pool's active index together with its failover counter, so both a
// failover and a return to the probed primary change it. A plain client
// has one endpoint and the zero value never changes.
type endpointGen struct {
	active    int
	failovers uint64
}

// policy is the call policy one operation works to: taken from the active
// endpoint, so a tick, a scan or a backfill step never mixes two
// endpoints' budgets halfway through, and bound to the endpoint
// generation it came from, so an operation whose endpoint changed under
// it can decide again rather than run an unlimited catch-up on a paced
// fallback.
type policy struct {
	unlimited bool
	rate      float64
	gen       endpointGen
}

// policy snapshots the active endpoint's policy, falling back to the
// network's own configuration for a plain client.
func (f *Follower) policy() policy {
	if f.pool != nil {
		p := f.pool.Policy()
		st := f.pool.Status()
		return policy{unlimited: p.Unlimited, rate: p.Rate, gen: endpointGen{active: st.Active, failovers: st.Failovers}}
	}
	return policy{unlimited: f.net.Unlimited(), rate: f.net.CallsPerSecond}
}

// repolicy re-reads the policy when the pool has moved to another endpoint
// since pol was taken, so the rest of a multi-step operation works to the
// budget of the endpoint that will actually serve it. It reports whether
// the endpoint changed.
func (f *Follower) repolicy(pol policy) (policy, bool) {
	cur := f.policy()
	if cur.gen == pol.gen {
		return pol, false
	}
	return cur, true
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
	if err := f.reloadHeadLocked(ctx); err != nil {
		return err
	}
	if err := f.loadScanStateLocked(ctx); err != nil {
		return err
	}
	if err := f.reloadSetsLocked(ctx); err != nil {
		return err
	}
	if err := f.loadLiveStartLocked(ctx); err != nil {
		return err
	}
	if err := f.loadEthUsdLocked(ctx); err != nil {
		return err
	}
	// Last, so a raised history_epoch resets checkpoints that are already
	// loaded rather than being overwritten by the load that follows it.
	if err := f.applyHistoryEpochLocked(ctx); err != nil {
		return err
	}
	f.initialized = true
	return nil
}

// loadEthUsdLocked restores the recorded ETH/USD spot, so a restarted
// collector keeps publishing it until it ages out rather than waiting for
// the first slow tick. An unreadable row is dropped, never fatal: the price
// is decoration on the snapshot, not chain state. Nothing is restored while
// the source is disabled.
func (f *Follower) loadEthUsdLocked(ctx context.Context) error {
	if f.ethUsd == nil {
		return nil
	}
	raw, ok, err := f.store.GetState(ctx, f.chainID, db.StateEthUsd)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	var v model.EthUsd
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		f.log.Warn("unreadable eth/usd checkpoint, ignoring it", "err", err.Error())
		return nil
	}
	at, err := time.Parse(time.RFC3339, v.At)
	if err != nil {
		f.log.Warn("unreadable eth/usd timestamp, ignoring it", "err", err.Error())
		return nil
	}
	f.ethUsdPrice = &prices.Price{Price: v.Price, At: at, Source: v.Source}
	return nil
}

// reloadHeadLocked reads the last committed block from the database and
// forgets the in-memory replay state, so the next tick rebuilds it from
// the stored row. Used at start, after a failed commit and after a rewind
// (which also clears the sample and snapshot, see clearHeadStateLocked).
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

// clearHeadStateLocked drops everything in memory that describes the head
// a rewind orphaned: the last sample and the published snapshot as well as
// the replay state. Only the tick that commits the replacement head
// publishes them again, so nothing reads state from a fork that is gone.
func (f *Follower) clearHeadStateLocked() {
	f.lastSample, f.snapshot = nil, nil
}

// loadScanStateLocked loads the owner scan checkpoints: how far the
// timeline is complete and where a truncated scan began.
func (f *Follower) loadScanStateLocked(ctx context.Context) error {
	raw, ok, err := f.store.GetState(ctx, f.chainID, db.StateOwnerScanThrough)
	if err != nil {
		return err
	}
	f.ownerScanThrough = 0
	if ok {
		if _, err := fmt.Sscanf(raw, "%d", &f.ownerScanThrough); err != nil {
			return fmt.Errorf("owner scan through %q: %w", raw, err)
		}
	}
	raw, ok, err = f.store.GetState(ctx, f.chainID, db.StateOwnerScanOrigin)
	if err != nil {
		return err
	}
	f.scanOrigin = nil
	if ok {
		o := &scanOrigin{}
		if err := json.Unmarshal([]byte(raw), o); err != nil {
			return fmt.Errorf("owner scan origin: %w", err)
		}
		f.scanOrigin = o
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

// recordHole appends an un-indexed range to collector_state holes, where
// saveHoles merges it into any queued range it overlaps or touches rather
// than duplicating it. A hole without a reason is queued for the gap
// filler; one recorded with reasonNoState is only work if a state ever
// appears before it.
func (f *Follower) recordHole(ctx context.Context, s db.Store, h hole) error {
	holes, err := f.loadHoles(ctx, s)
	if err != nil {
		return err
	}
	h.At = f.now().UTC().Format(time.RFC3339)
	return f.saveHoles(ctx, s, append(holes, h))
}

// reloadSetsLocked refreshes the constraint set, minimum fee and legacy
// parameter caches from the recorded owner actions.
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
	f.legacyChanges = f.legacyChanges[:0]
	for i := len(actions) - 1; i >= 0; i-- { // ascending by block
		a := actions[i]
		if c, ok := minFeeChangeOf(a.BlockNumber, a.Method, a.Args); ok {
			f.minFeeChanges = append(f.minFeeChanges, c)
		}
		if c, ok := legacyChangeOf(a.BlockNumber, a.Method, a.Args); ok {
			f.legacyChanges = append(f.legacyChanges, c)
		}
	}
	return nil
}

// minFeeChangeOf decodes a recorded setMinimumL2BaseFee.
func minFeeChangeOf(block uint64, method string, args db.JSONB) (minFeeChange, bool) {
	if method != methodSetMinimumL2BaseFee {
		return minFeeChange{}, false
	}
	var a struct {
		PriceInWei string `json:"priceInWei"`
	}
	if err := args.Unmarshal(&a); err != nil {
		return minFeeChange{}, false
	}
	fee, ok := new(big.Int).SetString(a.PriceInWei, 10)
	if !ok {
		return minFeeChange{}, false
	}
	return minFeeChange{block: block, fee: fee}, true
}

// legacyChangeOf decodes a recorded legacy parameter change: the speed
// limit (setSpeedLimit), the inertia (setL2GasPricingInertia) or the
// tolerance (setL2GasBacklogTolerance).
func legacyChangeOf(block uint64, method string, args db.JSONB) (legacyChange, bool) {
	var a struct {
		Limit *uint64 `json:"limit"`
		Sec   *uint64 `json:"sec"`
	}
	if err := args.Unmarshal(&a); err != nil {
		return legacyChange{}, false
	}
	switch method {
	case methodSetSpeedLimit:
		if a.Limit != nil {
			return legacyChange{block: block, method: method, value: *a.Limit}, true
		}
	case methodSetL2GasPricingInertia, methodSetL2GasBacklogTolerance:
		if a.Sec != nil {
			return legacyChange{block: block, method: method, value: *a.Sec}, true
		}
	}
	return legacyChange{}, false
}

// timeline is the pricer-affecting owner history a replay splits at:
// constraint sets (a set resets the backlogs to its starting values even
// when its shape is unchanged), minimum base fee changes and legacy
// parameter changes, plus the scan origin's sampled fee. It merges the
// recorded history with actions fetched for the current tick but not yet
// committed, so a replay can split at them before they are stored.
type timeline struct {
	sets   []setChange
	fees   []minFeeChange
	legacy []legacyChange
	origin *scanOrigin
}

// timelineLocked builds the timeline from the caches plus pending actions.
func (f *Follower) timelineLocked(pending []*nitro.OwnerAction) *timeline {
	tl := &timeline{
		origin: f.scanOrigin,
		fees:   append([]minFeeChange(nil), f.minFeeChanges...),
		legacy: append([]legacyChange(nil), f.legacyChanges...),
	}
	for _, cs := range f.sets {
		entries, err := setEntries(cs)
		if err != nil {
			continue
		}
		tl.sets = append(tl.sets, setChange{block: cs.EffectiveBlock, entries: entries})
	}
	for _, a := range pending {
		if a.Constraints != nil {
			tl.sets = append(tl.sets, setChange{block: a.BlockNumber, entries: entriesOf(a.Constraints)})
		}
		args, _ := db.MarshalJSONB(a.Args)
		if c, ok := minFeeChangeOf(a.BlockNumber, a.Method, args); ok {
			tl.fees = append(tl.fees, c)
		}
		if c, ok := legacyChangeOf(a.BlockNumber, a.Method, args); ok {
			tl.legacy = append(tl.legacy, c)
		}
	}
	sort.SliceStable(tl.sets, func(i, j int) bool { return tl.sets[i].block < tl.sets[j].block })
	sort.SliceStable(tl.fees, func(i, j int) bool { return tl.fees[i].block < tl.fees[j].block })
	sort.SliceStable(tl.legacy, func(i, j int) bool { return tl.legacy[i].block < tl.legacy[j].block })
	return tl
}

// minFeeAt returns the minimum base fee in force at a block from the
// recorded setMinimumL2BaseFee actions, with the scan origin's sampled fee
// from the origin block on and nitro's genesis default before the first
// recorded change. It never uses the live value.
func (tl *timeline) minFeeAt(number uint64) *big.Int {
	fee := big.NewInt(pricer.InitialMinimumBaseFeeWei)
	if of := tl.origin.fee(); of != nil && number >= tl.origin.Block {
		fee = of
	}
	for _, c := range tl.fees {
		if c.block <= number {
			fee = c.fee
		}
	}
	return new(big.Int).Set(fee)
}

// minFeeChangeBlock returns the block of the last fee change at or before
// number (the origin counts as one; 0 when none).
func (tl *timeline) minFeeChangeBlock(number uint64) uint64 {
	var block uint64
	if tl.origin.fee() != nil && number >= tl.origin.Block {
		block = tl.origin.Block
	}
	for _, c := range tl.fees {
		if c.block <= number {
			block = c.block
		}
	}
	return block
}

// legacyAt returns base with every recorded legacy change at or before
// number applied (the latest per parameter wins). At and above a scan
// origin that sampled the legacy state, that sample is the base instead of
// the caller's, so history is replayed from what was really in force
// rather than from the current parameters.
func (tl *timeline) legacyAt(number uint64, base *pricer.Legacy) *pricer.Legacy {
	if base == nil {
		return nil
	}
	out := *base
	if o := tl.origin; o != nil && o.Legacy != nil && number >= o.Block {
		out.SpeedLimit, out.Inertia, out.Tolerance = o.Legacy.SpeedLimit, o.Legacy.Inertia, o.Legacy.Tolerance
	}
	for _, c := range tl.legacy {
		if c.block <= number {
			applyLegacy(&out, c)
		}
	}
	return &out
}

func applyLegacy(l *pricer.Legacy, c legacyChange) {
	switch c.method {
	case methodSetSpeedLimit:
		l.SpeedLimit = c.value
	case methodSetL2GasPricingInertia:
		l.Inertia = c.value
	case methodSetL2GasBacklogTolerance:
		l.Tolerance = c.value
	}
}

// boundaryAt reports whether any recorded change takes effect at a block.
func (tl *timeline) boundaryAt(number uint64) bool {
	for _, s := range tl.sets {
		if s.block == number {
			return true
		}
	}
	for _, c := range tl.fees {
		if c.block == number {
			return true
		}
	}
	for _, c := range tl.legacy {
		if c.block == number {
			return true
		}
	}
	return false
}

// applyAt applies every change effective exactly at number to st: a
// constraint set replaces the constraints and resets the backlogs (on the
// constraints model only), a fee change replaces the floor, a legacy change
// replaces its parameter.
func (tl *timeline) applyAt(st *pricer.State, number uint64) {
	for _, s := range tl.sets {
		if s.block == number && !st.IsLegacy() {
			st.Constraints = stateFromEntries(s.entries, st.MinBaseFee).Constraints
		}
	}
	for _, c := range tl.fees {
		if c.block == number {
			st.MinBaseFee = new(big.Int).Set(c.fee)
		}
	}
	if st.Legacy != nil {
		for _, c := range tl.legacy {
			if c.block == number {
				applyLegacy(st.Legacy, c)
			}
		}
	}
}

// setAt returns the recorded constraint set in force at a block, or nil.
func (f *Follower) setAt(number uint64) *db.ConstraintSet {
	var out *db.ConstraintSet
	for i := range f.sets {
		if f.sets[i].EffectiveBlock <= number {
			out = &f.sets[i]
		}
	}
	return out
}

// minFeeAt is the recorded minimum base fee in force at a block (see
// timeline.minFeeAt). The caller holds f.mu or is a test.
func (f *Follower) minFeeAt(number uint64) *big.Int {
	return f.timelineLocked(nil).minFeeAt(number)
}

// minFeeChangeBlock is the block of the last recorded fee change at or
// before number. The caller holds f.mu or is a test.
func (f *Follower) minFeeChangeBlock(number uint64) uint64 {
	return f.timelineLocked(nil).minFeeChangeBlock(number)
}

// boundaryAt reports whether a recorded owner action changes the pricer at
// a block. The caller holds f.mu or is a test.
func (f *Follower) boundaryAt(number uint64) bool {
	return f.timelineLocked(nil).boundaryAt(number)
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

// sameSets compares two set documents by target and window (the starting
// backlogs may differ: an observed set records the live ones).
func sameSets(a, b []model.ConstraintSetEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Target != b[i].Target || a[i].Window != b[i].Window {
			return false
		}
	}
	return true
}

// entriesOf converts an owner action's constraints into set entries.
func entriesOf(params []nitro.ConstraintParam) []model.ConstraintSetEntry {
	entries := make([]model.ConstraintSetEntry, len(params))
	for i, c := range params {
		entries[i] = model.ConstraintSetEntry{Target: c.GasTargetPerSecond, Window: c.AdjustmentWindowSeconds, StartingBacklog: c.StartingBacklog}
	}
	return entries
}

func entriesJSON(entries []model.ConstraintSetEntry) db.JSONB {
	if entries == nil {
		entries = []model.ConstraintSetEntry{}
	}
	b, _ := json.Marshal(entries)
	return db.JSONB(b)
}

// errStaleGeneration marks work that was fetched from the chain before a
// rewind and must not be committed on top of the canonical chain.
var errStaleGeneration = errors.New("chain rewound while fetching, discarding the work")

// generation reads the chain's rewind counter (collector_state
// generation), 0 before the first rewind.
func (f *Follower) generation(ctx context.Context, s db.Store) (uint64, error) {
	raw, ok, err := s.GetState(ctx, f.chainID, db.StateGeneration)
	if err != nil || !ok {
		return 0, err
	}
	gen, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("generation %q: %w", raw, err)
	}
	return gen, nil
}

// bumpGeneration increments the rewind counter inside the rewind's own
// transaction, so every writer that captured the old value aborts.
func (f *Follower) bumpGeneration(ctx context.Context, s db.Store) error {
	gen, err := f.generation(ctx, s)
	if err != nil {
		return err
	}
	return s.SetState(ctx, f.chainID, db.StateGeneration, strconv.FormatUint(gen+1, 10))
}

// withGeneration runs fn in the chain transaction, but only when the chain
// has not been rewound since gen was captured. A writer captures the
// generation before its network calls and commits under this check, so
// work fetched from a fork that has since been rewound is discarded and
// retried instead of being written back on top of the canonical chain.
// Holding the lock across the network calls instead would block the fast
// loop for as long as the RPC takes.
func (f *Follower) withGeneration(ctx context.Context, gen uint64, fn func(db.Store) error) error {
	return f.store.WithChainTx(ctx, f.chainID, func(s db.Store) error {
		cur, err := f.generation(ctx, s)
		if err != nil {
			return err
		}
		if cur != gen {
			return fmt.Errorf("%w (generation %d, now %d)", errStaleGeneration, gen, cur)
		}
		return fn(s)
	})
}

// historyMustWait reports whether the gap filler and the backfill should
// hold off: the fast loop is catching up right now, or its last tick found
// the stored head more than a header batch behind the chain, so every
// spare call belongs to the live path until it has caught up.
func (f *Follower) historyMustWait() bool {
	return f.catchingUp.Load() || f.behind.Load() > uint64(f.cfg.HeaderBatchSize)
}
