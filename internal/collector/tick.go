package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"time"

	"github.com/lib/pq"

	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/model"
	"github.com/tirante-dev/gascurve/internal/nitro"
	"github.com/tirante-dev/gascurve/internal/pricer"
)

// errReorgTooDeep is returned when no common ancestor is found within
// maxReorgDepth stored blocks.
var errReorgTooDeep = errors.New("reorg deeper than the stored ancestry")

// Tick runs one fast iteration: sample the head, catch up on headers, replay forward from the last
// committed state, write blocks, buckets and the state sample in one transaction, then NOTIFY.
// Nothing in memory advances before that transaction commits.
func (f *Follower) Tick(ctx context.Context) error {
	return f.tickWith(ctx, false, f.rpc.FastSample)
}

// TickAt is Tick with the sample pinned to one block number, used when a newHeads event names the
// head so state and header match exactly.
func (f *Follower) TickAt(ctx context.Context, number uint64) error {
	return f.tickWith(ctx, true, func(ctx context.Context) (*nitro.Sample, error) {
		return f.rpc.FastSampleAt(ctx, number)
	})
}

// tickWith runs a tick around sampleFn. The sample is the latency critical part and goes through the
// endpoint's fast lane; the catch-up headers and everything after them are bulk work.
func (f *Follower) tickWith(ctx context.Context, pinned bool, sampleFn func(context.Context) (*nitro.Sample, error)) (err error) {
	// The histogram covers the whole tick, a failed one included: a tick that keeps timing out is
	// exactly what the duration is watched for.
	start := f.now()
	defer func() { f.metrics.ObserveTick(f.now().Sub(start)) }()
	if err := f.ensureInit(ctx); err != nil {
		return f.fail(ctx, err)
	}
	// A tick that ends without error has the stored head at the sampled one, so history work may have
	// the spare budget again.
	defer func() {
		if err == nil {
			f.behind.Store(0)
		}
	}()
	sample, err := sampleFn(nitro.WithClass(ctx, nitro.Fast))
	if err != nil {
		return f.fail(ctx, fmt.Errorf("sample: %w", err))
	}
	head := sample.Header.Number

	f.mu.Lock()
	stored, storedHash := f.head, f.headHash
	f.mu.Unlock()
	if f.monitor != nil {
		f.monitor.observeHead(f.chainID, head, stored)
	}
	capacity := f.rpcCapacity(sample, stored, pinned)
	if head >= stored {
		// The lag is what history work yields to, and it is recorded for the whole tick. Nothing else
		// marks an ordinary catch-up: a chain producing blocks faster than a tick completes is mid-tick
		// almost always, so a flag raised on every tick that moved the head is a permanent one.
		f.behind.Store(head - stored)
	}

	switch {
	case stored == 0:
		// Fresh database: nothing before the sampled head is known, so it is persisted alone.
		return f.seed(ctx, sample, nil, nil, capacity)
	case head < stored:
		return f.headBehind(ctx, sample, stored)
	case head == stored && !hashMismatch(storedHash, sample.Header.Hash):
		return f.sampleOnly(ctx, sample, capacity)
	}

	if head == stored {
		// Same height, different hash: the stored head is off-chain.
		f.rewinding.Store(true)
		defer f.rewinding.Store(false)
		if stored, err = f.rewindToAncestor(ctx, stored-1); err != nil {
			return f.fail(ctx, err)
		}
	}
	headers, err := f.catchUp(ctx, sample, stored, f.policy(), capacity)
	if err != nil {
		return f.fail(ctx, err)
	}
	if headers == nil {
		return nil // the gap was skipped and the head seeded
	}
	if hashMismatch(f.headHashOf(stored), headers[0].ParentHash) {
		// The first missing block does not build on the stored head.
		f.rewinding.Store(true)
		defer f.rewinding.Store(false)
		ancestor, err := f.rewindToAncestor(ctx, stored-1)
		if err != nil {
			return f.fail(ctx, err)
		}
		if headers, err = f.catchUp(ctx, sample, ancestor, f.policy(), capacity); err != nil {
			return f.fail(ctx, err)
		}
		if headers == nil {
			return nil
		}
	}
	if err := verifyChain(headers); err != nil {
		return f.fail(ctx, err)
	}
	return f.process(ctx, sample, headers, capacity)
}

// rpcCapacity estimates the sustained JSON-RPC rate this observed head interval needed. A constraints
// sample costs five calls when pinned to a newHeads block and six when polling must resolve
// eth_blockNumber first; legacy sampling costs four more. Every intervening block costs a header and
// a receipt call, because public endpoints meter batch items individually.
func (f *Follower) rpcCapacity(sample *nitro.Sample, stored uint64, pinned bool) model.RPCCapacity {
	sampleCalls := uint64(5)
	if !pinned {
		sampleCalls++
	}
	if sample.IsLegacy() {
		sampleCalls += 4
	}
	needed := sampleCalls
	if stored > 0 && sample.Header.Number > stored+1 {
		needed += 2 * (sample.Header.Number - stored - 1)
	}
	f.mu.Lock()
	var previous time.Time
	if f.lastSample != nil {
		previous = f.lastSample.SampledAt
	}
	f.mu.Unlock()
	elapsed := sample.SampledAt.Sub(previous)
	if previous.IsZero() || elapsed <= 0 {
		elapsed = f.tickInterval
	}
	if elapsed <= 0 {
		elapsed = time.Second
	}
	required := roundedRate(float64(needed) / elapsed.Seconds())
	pol := f.policy()
	configured := roundedRate(pol.rate)
	observed := roundedRate(float64(f.rpc.Stats().CallsLast10s) / 10)
	at := sample.SampledAt.UTC().Format(time.RFC3339)
	out := model.RPCCapacity{
		ConfiguredCallsPerSecond: configured, RequiredCallsPerSecond: required,
		ObservedCallsPerSecond: observed, Saturated: !pol.unlimited && required > configured,
		At: &at,
	}
	if !pol.unlimited {
		headroom := roundedRate(configured - required)
		out.HeadroomCallsPerSecond = &headroom
	}
	return out
}

func roundedRate(v float64) float64 { return math.Round(v*1000) / 1000 }

// headBehind handles a reported head below the stored one. An endpoint that has simply not caught up
// still reports the same hashes for the blocks it does have, so the stored row at the reported head is
// compared with the header the node reports there: only a differing hash proves the stored chain is
// off-chain. A confirmed rollback is rewound like any other reorg; a lagging endpoint is skipped.
func (f *Follower) headBehind(ctx context.Context, sample *nitro.Sample, stored uint64) error {
	head := sample.Header.Number
	row, err := f.store.BlockByNumber(ctx, f.chainID, head)
	if err != nil {
		return f.fail(ctx, fmt.Errorf("block %d: %w", head, err))
	}
	if row == nil || !hashMismatch(row.Hash, sample.Header.Hash) {
		f.log.Warn("head behind the stored head, endpoint is lagging, skipping tick", "head", head, "stored", stored)
		return nil
	}
	f.log.Warn("head behind the stored head and its hash differs, rewinding the rollback", "head", head, "stored", stored)
	f.rewinding.Store(true)
	defer f.rewinding.Store(false)
	if _, err := f.rewindToAncestor(ctx, head); err != nil {
		return f.fail(ctx, err)
	}
	return nil
}

// headHashOf returns the stored hash of the current head when it is number.
func (f *Follower) headHashOf(number uint64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.head == number {
		return f.headHash
	}
	return ""
}

// hashMismatch reports whether two hashes are both known and differ. Rows written before hashes were
// stored cannot be checked and pass.
func hashMismatch(a, b string) bool {
	return a != "" && b != "" && a != b
}

// verifyChain checks that consecutive headers link by parent hash, so a
// reorg during the fetch is noticed instead of replayed.
func verifyChain(headers []nitro.Header) error {
	for i := 1; i < len(headers); i++ {
		if headers[i].Number != headers[i-1].Number+1 {
			return fmt.Errorf("headers %d and %d are not consecutive, retrying", headers[i-1].Number, headers[i].Number)
		}
		if hashMismatch(headers[i-1].Hash, headers[i].ParentHash) {
			return fmt.Errorf("headers %d and %d do not link, retrying", headers[i-1].Number, headers[i].Number)
		}
	}
	return nil
}

// errEndpointChanged aborts an operation whose endpoint failed over, so the decision is made again
// against the endpoint that will serve the rest of the work.
var errEndpointChanged = errors.New("the pool moved to another endpoint, deciding again")

// catchUp fetches the headers after stored up to the sampled head and appends the sampled header. On a
// paced network a gap over budget is skipped: the head is seeded alone and the hole recorded, but the
// owner actions the gap crosses are still fetched and committed with the seed, so the pricing timeline
// stays complete. A failover to a paced endpoint mid-fetch makes the decision again, so an unlimited
// catch-up is never carried on to a public fallback that has to pay for it.
func (f *Follower) catchUp(ctx context.Context, sample *nitro.Sample, stored uint64, pol policy, capacity model.RPCCapacity) ([]nitro.Header, error) {
	head := sample.Header.Number
	if stored == 0 {
		return nil, f.seed(ctx, sample, nil, nil, capacity)
	}
	from := stored + 1
	maxGap := uint64(f.cfg.HeaderBatchSize) * uint64(f.cfg.MaxCatchUpBatches)
	gap := head - stored
	skip := func() ([]nitro.Header, error) {
		f.mu.Lock()
		predecessor := timestampString(f.prevTs)
		f.mu.Unlock()
		h := hole{
			From: from, To: head - 1, Lifecycle: rangePending, Reason: reasonCatchUpLimit,
			PredecessorAt: predecessor, SuccessorAt: timestampString(sample.Header.Timestamp),
		}
		f.log.Warn("catch-up gap exceeds budget, skipping blocks and restarting from the sampled head", "from", h.From, "to", h.To)
		pending, err := f.fetchOwnerRange(ctx, from, head)
		if err != nil {
			return nil, err
		}
		if err := f.seed(ctx, sample, &h, pending, capacity); err != nil {
			return nil, err
		}
		// Counted only now: the seed is the transaction that records the hole and moves the head, so a
		// failed fetch or refused commit must not report a gap history will not have to fill.
		f.metrics.GapSkipped()
		return nil, nil
	}
	if !pol.unlimited && gap > maxGap {
		return skip()
	}
	headers, err := f.fetchHeaders(ctx, from, head-1, func() error {
		cur, changed := f.repolicy(pol)
		if !changed || cur.unlimited || gap <= maxGap {
			return nil
		}
		return errEndpointChanged
	})
	if errors.Is(err, errEndpointChanged) {
		f.log.Warn("the endpoint changed to a paced one during the catch-up, skipping the rest of the gap", "from", from, "to", head-1)
		return skip()
	}
	if err != nil {
		return nil, fmt.Errorf("headers %d..%d: %w", from, head-1, err)
	}
	return append(headers, sample.Header), nil
}

// fetchHeaders reads [from, to] in header_batch_size batches. recheck, when given, runs before every
// batch after the first and aborts the fetch with its error, so a decision the whole range depends on
// can be revisited between batches.
func (f *Follower) fetchHeaders(ctx context.Context, from, to uint64, recheck func() error) ([]nitro.Header, error) {
	if to < from {
		return nil, nil
	}
	out := make([]nitro.Header, 0, to-from+1)
	for start := from; start <= to; start += uint64(f.cfg.HeaderBatchSize) {
		if start > from && recheck != nil {
			if err := recheck(); err != nil {
				return nil, err
			}
		}
		end := min(start+uint64(f.cfg.HeaderBatchSize)-1, to)
		numbers := make([]uint64, 0, end-start+1)
		for n := start; n <= end; n++ {
			numbers = append(numbers, n)
		}
		hs, err := f.rpc.HeadersByNumbers(ctx, numbers)
		if err != nil {
			return nil, err
		}
		out = append(out, hs...)
	}
	return out, nil
}

// rewindToAncestor finds the highest stored block at or below from whose hash the node still reports
// and, holding the chain lock, deletes everything after it in one transaction: blocks and their bucket
// contributions, owner actions, constraint sets, batch reports, state samples, the cursors and
// checkpoints, and, when they lie above the ancestor, the live start and the backfill cursor. Every
// hole the rewind reaches into goes back to the start of its range. It returns the reloaded head.
func (f *Follower) rewindToAncestor(ctx context.Context, from uint64) (uint64, error) {
	ancestor, err := f.findAncestor(ctx, from)
	if err != nil {
		return 0, err
	}
	f.log.Warn("reorg detected, rewinding", "ancestor", ancestor)
	f.mu.Lock()
	ls := f.liveStart
	boundary, hasBoundary := f.boundaryLocked()
	f.mu.Unlock()
	clearedStart := false
	err = f.store.WithChainTx(ctx, f.chainID, func(s db.Store) error {
		// Every writer that fetched from the old fork compares this counter inside its own transaction.
		if err := f.bumpGeneration(ctx, s); err != nil {
			return err
		}
		removed, err := s.DeleteBlocksAfter(ctx, f.chainID, ancestor)
		if err != nil {
			return fmt.Errorf("delete blocks: %w", err)
		}
		for _, res := range db.ResolutionOrder {
			starts := db.BucketStarts(removed, res)
			if err := s.RebuildBuckets(ctx, f.chainID, res, starts); err != nil {
				return err
			}
			// A window the store declines to rebuild has lost its early
			// rows to prune, and this rewind has just orphaned its retained
			// ones: no correct aggregate exists for it, and the one stored
			// describes the dead fork. Every other caller is right to keep
			// what is stored over a short recompute; here what is stored is
			// wrong, so the same windows are discarded and served as a gap.
			if err := s.DiscardBucketsBelowFrontier(ctx, f.chainID, res, starts); err != nil {
				return err
			}
		}
		if err := s.RewindAfter(ctx, f.chainID, ancestor); err != nil {
			return err
		}
		if _, err := s.DeleteStateSamplesAfter(ctx, f.chainID, ancestor); err != nil {
			return fmt.Errorf("delete samples: %w", err)
		}
		for _, key := range []string{db.StateOwnerLogCursor, db.StateBatchScanCursor, db.StateOwnerScanThrough} {
			if err := rewindCursor(ctx, s, f.chainID, key, ancestor); err != nil {
				return err
			}
		}
		cleared, err := f.rewindBackfill(ctx, s, ancestor, boundary, hasBoundary)
		if err != nil {
			return err
		}
		// A hole the filler had started is restarted: what it wrote above the ancestor went with the
		// orphaned chain. Its fold watermark is kept unless the additive buckets went too.
		if err := f.rewindHoles(ctx, s, ancestor, cleared); err != nil {
			return err
		}
		if ls != nil && ls.Block > ancestor {
			// The live history starts over at the next committed head.
			clearedStart = true
			if err := s.DeleteState(ctx, f.chainID, db.StateLiveStart); err != nil {
				return fmt.Errorf("live start: %w", err)
			}
		}
		// The network row moves in the same transaction, so /status never shows the orphaned head after
		// a rewind whose replacement fetch then fails.
		if err := f.rewindNetworkHead(ctx, s, ancestor); err != nil {
			return err
		}
		return s.SetState(ctx, f.chainID, db.StateHead, fmt.Sprint(ancestor))
	})
	if err != nil {
		return 0, fmt.Errorf("rewind to %d: %w", ancestor, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if clearedStart {
		f.liveStart = nil
	}
	f.ownerScanThrough = min(f.ownerScanThrough, ancestor)
	f.clearHeadStateLocked()
	if err := f.reloadHeadLocked(ctx); err != nil {
		return 0, err
	}
	if err := f.reloadSetsLocked(ctx); err != nil {
		return 0, err
	}
	return f.head, nil
}

// rewindNetworkHead moves networks.head_block and head_at down to the surviving ancestor inside the
// rewind transaction. last_sample_at comes from the newest surviving state sample and is cleared when
// none did, rather than from the clock, which would report the rewind as a successful sample. An
// ancestor with no row left nulls the head and its time instead of storing an epoch one.
func (f *Follower) rewindNetworkHead(ctx context.Context, s db.Store, ancestor uint64) error {
	var head *uint64
	var at *time.Time
	if ancestor > 0 {
		row, err := s.BlockByNumber(ctx, f.chainID, ancestor)
		if err != nil {
			return fmt.Errorf("ancestor block %d: %w", ancestor, err)
		}
		if row != nil {
			head, at = &ancestor, &row.TS
		}
	}
	var sampledAt *time.Time
	sample, err := s.LatestStateSample(ctx, f.chainID, false)
	if err != nil {
		return fmt.Errorf("surviving sample: %w", err)
	}
	if sample != nil {
		sampledAt = &sample.SampledAt
	}
	if err := s.SetNetworkHead(ctx, f.chainID, head, at, sampledAt); err != nil {
		return fmt.Errorf("network head: %w", err)
	}
	return nil
}

// rewindBackfill resets the backfill when its range reaches above the ancestor: everything it folded
// came from the old fork or was cut off, so its backfill-only buckets are dropped and the cursor starts
// over. It reports whether those additive buckets were deleted, since everything a gap filler folded
// into them went with them.
func (f *Follower) rewindBackfill(ctx context.Context, s db.Store, ancestor uint64, boundary time.Time, hasBoundary bool) (cleared bool, err error) {
	raw, ok, err := s.GetState(ctx, f.chainID, db.StateBackfillCursor)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	c := &backfillCursor{}
	if err := json.Unmarshal([]byte(raw), c); err != nil {
		return false, fmt.Errorf("backfill cursor: %w", err)
	}
	if c.Top == 0 || c.Top-1 <= ancestor {
		return false, nil
	}
	f.log.Warn("backfill range reaches above the reorg ancestor, restarting the backfill", "top", c.Top, "ancestor", ancestor)
	if hasBoundary {
		if _, err := s.DeleteBucketsBefore(ctx, f.chainID, boundary); err != nil {
			return false, err
		}
		cleared = true
	}
	return cleared, f.saveCursor(ctx, s, &backfillCursor{})
}

// rewindCursor lowers a numeric checkpoint to block when it is beyond it.
func rewindCursor(ctx context.Context, s db.Store, chainID uint64, key string, block uint64) error {
	raw, ok, err := s.GetState(ctx, chainID, key)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	cur, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return fmt.Errorf("%s %q: %w", key, raw, err)
	}
	if cur <= block {
		return nil
	}
	return s.SetState(ctx, chainID, key, strconv.FormatUint(block, 10))
}

// findAncestor walks down from a block comparing stored hashes with the node's until they agree. A row
// without a stored hash is unknown rather than trusted: it is the ancestor only when the node's header
// matches every field the row holds. A height without any row is the ancestor: nothing below it can be
// off-chain.
func (f *Follower) findAncestor(ctx context.Context, from uint64) (uint64, error) {
	rows, err := f.store.RecentBlocks(ctx, f.chainID, maxReorgDepth+1)
	if err != nil {
		return 0, err
	}
	byNumber := make(map[uint64]db.Block, len(rows))
	for _, r := range rows {
		byNumber[r.Number] = r
	}
	for n := from; from-n < maxReorgDepth; n-- {
		if n == 0 {
			return 0, nil
		}
		row, ok := byNumber[n]
		if !ok {
			return n, nil
		}
		h, err := f.rpc.HeaderByNumber(ctx, n)
		if err != nil {
			return 0, fmt.Errorf("header %d: %w", n, err)
		}
		if row.Hash == "" {
			if h.Timestamp == uint64(row.TS.Unix()) && h.GasUsed == row.GasUsed && h.BaseFee != nil && h.BaseFee.Cmp(row.BaseFee.BigInt()) == 0 {
				return n, nil
			}
			continue
		}
		if h.Hash == row.Hash {
			return n, nil
		}
	}
	return 0, fmt.Errorf("%w (%d blocks)", errReorgTooDeep, maxReorgDepth)
}

// seed persists the sampled head alone: its backlogs come from the sample and nothing before it is
// replayed. A skipped range is recorded as a hole and pending owner actions found in it are kept.
func (f *Follower) seed(ctx context.Context, sample *nitro.Sample, h *hole, pending []*nitro.OwnerAction, capacity model.RPCCapacity) error {
	st := stateFromSample(sample)
	// The start-of-block exponents that priced the seed are approximated from the sampled end-of-block
	// backlogs minus the block's own gas.
	start := st.Clone()
	backlogs := start.Backlogs()
	for i := range backlogs {
		backlogs[i] = pricer.SaturatingUSub(backlogs[i], sample.Header.ComputeGas())
	}
	start.SetBacklogs(backlogs)
	hdr := sample.Header
	results := pricer.Replay(start, 0, []pricer.Block{{Number: hdr.Number, Timestamp: hdr.Timestamp, GasUsed: hdr.ComputeGas()}},
		func(uint64) ([]uint64, bool) { return sampleBacklogs(sample), true })
	r := results[0]
	rows := blockRows(f.chainID, []nitro.Header{hdr}, []pricer.Result{r}, func(uint64) *big.Int { return st.MinBaseFee })
	// No known state preceded the seed, so the seeded block itself carries no prediction. Its own Step
	// output prices the next block and reaches it as the carry, through the published result.
	shiftPredictions(rows, prediction{})
	if err := f.persist(ctx, sample, rows, h, pending, capacity); err != nil {
		return f.fail(ctx, err)
	}
	f.publish(sample, st, &r, f.headRowError(rows, hdr.Number))
	return nil
}

// process replays headers (the last one is the sampled head) forward from the last committed state and
// persists. The whole interval is scanned for owner actions first (one eth_getLogs): every pricing
// change in it splits the replay at its block, and the actions commit with the tick.
func (f *Follower) process(ctx context.Context, sample *nitro.Sample, headers []nitro.Header, capacity model.RPCCapacity) error {
	head := sample.Header.Number
	f.mu.Lock()
	st, prevTs, err := f.replayStateLocked(ctx, sample)
	carry := f.carryLocked(headers[0].Number)
	f.mu.Unlock()
	if err != nil {
		return f.fail(ctx, err)
	}
	if st == nil {
		h := hole{
			From: headers[0].Number, To: head - 1, Lifecycle: rangeBlocked, Reason: reasonNoState,
			SuccessorAt: timestampString(sample.Header.Timestamp),
		}
		f.log.Warn("no known replay state before the first missing block, restarting from the sampled head", "from", h.From, "to", h.To)
		return f.seed(ctx, sample, &h, nil, capacity)
	}
	pending, err := f.fetchOwnerActions(ctx, headers[0].Number, head)
	if err != nil {
		return f.fail(ctx, err)
	}
	if from, to, ok := unsupportedModel(st, headers); ok {
		var h *hole
		if headers[0].Number < head {
			h = &hole{
				From: headers[0].Number, To: head - 1, Lifecycle: rangeBlocked, Reason: reasonUnsupportedModel,
				SuccessorAt: timestampString(sample.Header.Timestamp),
			}
		}
		f.log.Warn("blocks predate the multi-constraint pricer, leaving them unpriced rather than replaying a model the chain did not run",
			"from", from, "to", to, "arbosBelow", pricer.FirstConstraintVersion)
		return f.seed(ctx, sample, h, pending, capacity)
	}
	f.mu.Lock()
	tl := f.timelineLocked(pending)
	f.mu.Unlock()
	actions, err := f.resolveActionBlocks(ctx, headers, tl)
	if err != nil {
		return f.fail(ctx, err)
	}
	rows, results, ok := replayLive(f.chainID, st, prevTs, headers, sample, tl, actions)
	if !ok {
		h := hole{
			From: headers[0].Number, To: head - 1, Lifecycle: rangePending, Reason: reasonReplayDiscontinuity,
			PredecessorAt: timestampString(prevTs), SuccessorAt: timestampString(sample.Header.Timestamp),
		}
		f.log.Warn("pricer parameters changed without a recorded owner action, restarting from the sampled head", "from", h.From, "to", h.To)
		return f.seed(ctx, sample, &h, pending, capacity)
	}
	shiftPredictions(rows, carry)
	headResult := results[len(results)-1]
	if err := f.persist(ctx, sample, rows, nil, pending, capacity); err != nil {
		return f.fail(ctx, err)
	}
	f.publish(sample, st, &headResult, f.headRowError(rows, sample.Header.Number))
	return nil
}

func timestampString(ts uint64) string {
	if ts == 0 {
		return ""
	}
	return time.Unix(int64(ts), 0).UTC().Format(time.RFC3339)
}

// replayLive replays headers through st, splitting at every boundary of the timeline and anchoring the
// head to the sample. ok is false when the shape or minimum fee still differs from the sample's
// afterwards: a change happened that no recorded action explains.
func replayLive(chainID uint64, st *pricer.State, prevTs uint64, headers []nitro.Header, sample *nitro.Sample, tl *timeline, actions actionBlocks) (rows []db.Block, results []pricer.Result, ok bool) {
	head := sample.Header.Number
	anchor := func(n uint64) ([]uint64, bool) {
		if n == head {
			return sampleBacklogs(sample), true
		}
		return nil, false
	}
	rows, results = replayForward(chainID, st, prevTs, headers, tl, anchor, actions)
	if !sameShape(st, sample) || st.MinBaseFee.Cmp(bigOrZero(sample.MinBaseFee)) != 0 {
		return nil, nil, false
	}
	return rows, results, true
}

// replayForward replays headers through st. Actionless observed-set boundaries retain their block-start
// fallback. Recorded owner pricing actions are applied at their transaction boundaries after Step priced
// the block, with receipt gas split around each action.
func replayForward(chainID uint64, st *pricer.State, prevTs uint64, headers []nitro.Header, tl *timeline, anchor pricer.Anchor, actions actionBlocks) ([]db.Block, []pricer.Result) {
	results := make([]pricer.Result, 0, len(headers))
	fees := map[uint64]*big.Int{}
	prev := prevTs
	for _, header := range headers {
		for _, change := range tl.changesAt(header.Number, false) {
			applyPricingChange(st, change)
		}
		fees[header.Number] = new(big.Int).Set(st.MinBaseFee)
		var dt uint64
		if prev != 0 && header.Timestamp > prev {
			dt = header.Timestamp - prev
		}
		prev = header.Timestamp
		predicted, exponent, per := st.Step(dt)
		boundaries := actions[header.Number]
		if len(boundaries) > 0 {
			boundaries = append([]actionBoundary(nil), boundaries...)
			for i := range boundaries {
				if gasBefore, ok := header.ComputeGasBeforeTx(boundaries[i].txIndex); ok {
					boundaries[i].gasBefore = gasBefore
				}
			}
		}
		applyActionGas(st, header.ComputeGas(), boundaries)
		result := pricer.Result{
			Number: header.Number, Exponent: exponent, PerConstraint: per, Predicted: predicted,
		}
		if anchor != nil {
			if backlogs, ok := anchor(header.Number); ok {
				st.SetBacklogs(backlogs)
				result.Anchored = true
			}
		}
		result.Backlogs = st.Backlogs()
		results = append(results, result)
	}
	rows := blockRows(chainID, headers, results, func(n uint64) *big.Int { return fees[n] })
	return rows, results
}

// replayStateLocked returns a clone of the committed replay state, or rebuilds it from the stored head
// row after a restart or failed commit. Nil means the state before the first missing block is unknown.
func (f *Follower) replayStateLocked(ctx context.Context, sample *nitro.Sample) (*pricer.State, uint64, error) {
	if f.state != nil {
		return f.state.Clone(), f.prevTs, nil
	}
	if f.head == 0 {
		return nil, 0, nil
	}
	last, err := f.store.BlockByNumber(ctx, f.chainID, f.head)
	if err != nil {
		return nil, 0, err
	}
	if last == nil {
		return nil, 0, nil
	}
	st := f.stateAtLocked(last.Number, sample)
	if st == nil || len(st.Backlogs()) != len(last.Backlogs) {
		return nil, 0, nil
	}
	st.SetBacklogs(last.Backlogs)
	// The floor at the stored head: the row's when a recorded change covers it; when nothing recorded
	// explains the block the live value stands in for the unknown history, since the catch-up scan finds
	// any change after the row and a default is no evidence.
	tl := f.timelineLocked(nil)
	switch {
	case tl.feeChangesInBlock(last.Number):
		st.MinBaseFee = tl.minFeeAfter(last.Number)
	case tl.minFeeChangeBlock(last.Number) == 0:
		st.MinBaseFee = bigOrZero(sample.MinBaseFee)
	case last.MinBaseFee.Valid && last.MinBaseFee.Wei.BigInt().Sign() > 0:
		st.MinBaseFee = new(big.Int).Set(last.MinBaseFee.Wei.BigInt())
	default:
		st.MinBaseFee = tl.minFeeAt(last.Number)
	}
	return st, uint64(last.TS.Unix()), nil
}

// stateAtLocked builds the pricer shape in force at a block: the recorded constraint set, or the
// sampled shape when none is recorded; a legacy chain takes the sample's parameters with the recorded
// changes up to the block applied.
func (f *Follower) stateAtLocked(number uint64, sample *nitro.Sample) *pricer.State {
	if sample.IsLegacy() {
		if sample.Legacy == nil {
			return nil
		}
		base := &pricer.Legacy{SpeedLimit: sample.Legacy.SpeedLimit, Inertia: sample.Legacy.Inertia, Tolerance: sample.Legacy.Tolerance}
		return &pricer.State{MinBaseFee: new(big.Int), Legacy: f.timelineLocked(nil).legacyAt(number, base)}
	}
	if cs := f.setAt(number); cs != nil {
		if entries, err := setEntries(*cs); err == nil {
			st := stateFromEntries(entries, new(big.Int))
			st.SetBacklogs(make([]uint64, len(entries)))
			return st
		}
	}
	st := stateFromSample(sample)
	st.SetBacklogs(make([]uint64, len(st.Constraints)))
	st.MinBaseFee = new(big.Int)
	return st
}

// sampleOnly handles a tick where no new block appeared: the replay state is re-anchored and a fresh
// snapshot published.
func (f *Follower) sampleOnly(ctx context.Context, sample *nitro.Sample, capacity model.RPCCapacity) error {
	f.mu.Lock()
	var st *pricer.State
	if f.state != nil {
		st = f.state.Clone()
	}
	last := f.lastResult
	f.mu.Unlock()
	if st == nil || !sameShape(st, sample) {
		st = stateFromSample(sample)
	} else {
		st.SetBacklogs(sampleBacklogs(sample))
		st.MinBaseFee = bigOrZero(sample.MinBaseFee)
	}
	if last == nil {
		last = &pricer.Result{Number: sample.Header.Number}
	}
	if err := f.persist(ctx, sample, nil, nil, nil, capacity); err != nil {
		return f.fail(ctx, err)
	}
	f.publish(sample, st, last, f.headRowError(nil, sample.Header.Number))
	return nil
}

// publish makes a committed tick the follower's state: head, replay state, sample and result move
// together, only after the commit.
func (f *Follower) publish(sample *nitro.Sample, st *pricer.State, headResult *pricer.Result, errBips int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.head = sample.Header.Number
	f.headHash = sample.Header.Hash
	f.prevTs = sample.Header.Timestamp
	f.state = st
	f.lastSample = sample
	f.lastResult = headResult
	f.lastErrBips = errBips
	if f.liveStart == nil {
		f.liveStart = &liveStart{Block: sample.Header.Number, TS: int64(sample.Header.Timestamp)}
	}
	f.metrics.ObserveHead(sample.Header.Number, time.Unix(int64(sample.Header.Timestamp), 0), sample.SampledAt, f.now())
}

// observedSetLocked returns the observed constraint set to record with a tick: the sampled constraints
// whenever the latest known set has another shape. Nil for legacy samples and when it matches; an
// unreadable latest set is left alone rather than shadowed every tick.
func (f *Follower) observedSetLocked(sample *nitro.Sample, pending []*nitro.OwnerAction) *db.ConstraintSet {
	if sample.IsLegacy() {
		return nil
	}
	var latest []model.ConstraintSetEntry
	known := false
	if len(f.sets) > 0 {
		entries, err := setEntries(f.sets[len(f.sets)-1])
		if err != nil {
			return nil
		}
		latest, known = entries, true
	}
	for _, a := range pending {
		if a.Constraints != nil {
			latest, known = entriesOf(a.Constraints), true
		}
	}
	if known && sameEntries(latest, sample.Constraints) {
		return nil
	}
	return &db.ConstraintSet{
		ChainID: f.chainID, EffectiveBlock: sample.Header.Number, EffectiveAt: time.Unix(int64(sample.Header.Timestamp), 0).UTC(),
		Constraints: entriesJSON(entriesFromSample(sample)), Source: model.SourceObserved,
	}
}

// persist writes everything for a tick in one chain-locked transaction and notifies: the owner actions
// found in the catch-up interval with their sets and notifications, an observed set when the sampled
// shape is not the latest known one, the blocks, their buckets (rebuilt from the rows in their windows,
// so a retried fold changes nothing), the state sample, the head and the live start. Caches follow.
func (f *Follower) persist(ctx context.Context, sample *nitro.Sample, rows []db.Block, h *hole, pending []*nitro.OwnerAction, capacity model.RPCCapacity) error {
	head := sample.Header.Number
	headAt := time.Unix(int64(sample.Header.Timestamp), 0).UTC()
	f.mu.Lock()
	l1, accounts, ls := f.l1, f.accounts, f.liveStart
	// The staleness rule is applied at publication, not at the fetch: a quote older than eth_usd_max_age
	// is dropped from this snapshot and the NOTIFY payload rather than shown as live.
	ethUsd := ethUsdModel(f.ethUsdPrice, f.now(), f.cfg.EthUsdMaxAge)
	slowGen := f.slowGen
	includeSlow := slowGen > f.slowSaved
	observed := f.observedSetLocked(sample, pending)
	f.mu.Unlock()
	lastErr := f.endpointErrorNow()
	capacityJSON, err := json.Marshal(capacity)
	if err != nil {
		return fmt.Errorf("encode rpc capacity: %w", err)
	}

	var snapshot *model.LiveSnapshot
	reload := false
	err = f.store.WithChainTx(ctx, f.chainID, func(s db.Store) error {
		recorded, err := f.storeOwnerActions(ctx, s, pending)
		if err != nil {
			return err
		}
		reload = recorded
		if observed != nil {
			f.log.Info("live constraint set differs from the latest known, recording an observed set", "block", observed.EffectiveBlock)
			if _, err := s.InsertConstraintSet(ctx, *observed); err != nil {
				return fmt.Errorf("observed set: %w", err)
			}
			reload = true
		}
		if len(rows) > 0 {
			if err := s.UpsertBlocks(ctx, rows); err != nil {
				return fmt.Errorf("upsert blocks: %w", err)
			}
			for _, res := range db.ResolutionOrder {
				if err := s.RebuildBuckets(ctx, f.chainID, res, db.BucketStarts(rows, res)); err != nil {
					return fmt.Errorf("rebuild buckets: %w", err)
				}
			}
		}
		g10, c10, err := s.GasBetween(ctx, f.chainID, headAt.Add(-10*time.Second), headAt)
		if err != nil {
			return fmt.Errorf("gas per second: %w", err)
		}
		g60, c60, err := s.GasBetween(ctx, f.chainID, headAt.Add(-60*time.Second), headAt)
		if err != nil {
			return fmt.Errorf("gas per second: %w", err)
		}
		snap := buildSnapshot(f.chainID, sample, f.headRowError(rows, sample.Header.Number),
			model.GasPerSecond{S10: g10 / 10, S60: g60 / 60},
			model.NullableGasPerSecond{S10: dividedPtr(c10, 10), S60: dividedPtr(c60, 60)},
			l1, accounts, ethUsd)
		snapshot = &snap
		row, err := sampleRow(f.chainID, sample, &snap, includeSlow)
		if err != nil {
			return err
		}
		if err := s.InsertStateSample(ctx, row); err != nil {
			return fmt.Errorf("state sample: %w", err)
		}
		if err := s.UpdateNetworkHead(ctx, f.chainID, head, headAt, sample.SampledAt); err != nil {
			return fmt.Errorf("network head: %w", err)
		}
		// A tick clears networks.last_error, but a disabled endpoint is a standing error even while the
		// network keeps running, so it is written back in the same transaction.
		if lastErr != "" {
			if err := s.SetNetworkError(ctx, f.chainID, lastErr); err != nil {
				return fmt.Errorf("endpoint error: %w", err)
			}
		}
		if err := s.SetState(ctx, f.chainID, db.StateHead, fmt.Sprint(head)); err != nil {
			return fmt.Errorf("head checkpoint: %w", err)
		}
		if err := s.SetState(ctx, f.chainID, db.StateRPCCapacity, string(capacityJSON)); err != nil {
			return fmt.Errorf("rpc capacity: %w", err)
		}
		if ls == nil {
			if err := f.saveLiveStart(ctx, s, &liveStart{Block: head, TS: int64(sample.Header.Timestamp)}); err != nil {
				return fmt.Errorf("live start: %w", err)
			}
		}
		if h != nil {
			if err := f.recordHole(ctx, s, *h); err != nil {
				return fmt.Errorf("record hole: %w", err)
			}
		}
		payload, err := json.Marshal(snap)
		if err != nil {
			return fmt.Errorf("encode snapshot: %w", err)
		}
		return s.Notify(ctx, db.ChannelLive, string(payload))
	})
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshot = snapshot
	if includeSlow {
		// Only the generation this transaction wrote is cleared: a slow sample published while it ran
		// keeps its own claim on the next tick.
		f.slowSaved = max(f.slowSaved, slowGen)
	}
	if reload {
		if err := f.reloadSetsLocked(ctx); err != nil {
			return fmt.Errorf("after commit: %w", err)
		}
	}
	return nil
}

// fail records the error on the network row, reloads the committed head and state from the database
// (the transaction may have failed after the server committed) and returns the error.
func (f *Follower) fail(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return err
	}
	f.log.Warn("tick failed", "err", err.Error())
	if serr := f.store.SetNetworkError(ctx, f.chainID, err.Error()); serr != nil {
		f.log.Warn("record error", "err", serr.Error())
	}
	f.mu.Lock()
	if f.initialized {
		if rerr := f.reloadHeadLocked(ctx); rerr != nil {
			f.log.Warn("reload head", "err", rerr.Error())
		}
	}
	f.mu.Unlock()
	return err
}

// endpointErrorNow summarizes the pool's disabled endpoints, "" without a pool or while all are usable.
func (f *Follower) endpointErrorNow() string {
	if f.pool == nil {
		return ""
	}
	return endpointError(f.pool.Status())
}

// prediction is what the pricer computed while replaying one block: the fee ArbOS stores at that
// block and writes into the next block's header, with the exponents that produced it. The zero value
// means no predecessor was replayed, so the block it lands on carries no prediction.
type prediction struct {
	fee           *big.Int
	exponent      int64
	perConstraint pq.Int64Array
	known         bool
}

// encode renders the group for a JSON checkpoint; an unknown one is the empty triple.
func (p prediction) encode() (fee string, exponent int64, bips []int64) {
	if !p.known || p.fee == nil {
		return "", 0, nil
	}
	return p.fee.String(), p.exponent, p.perConstraint
}

// decodeCarry reads a group back from a checkpoint. An unparsable fee is treated as absent, so a
// checkpoint written by an older collector simply starts its next block without a prediction.
func decodeCarry(fee string, exponent int64, bips []int64) prediction {
	v, ok := new(big.Int).SetString(fee, 10)
	if !ok {
		return prediction{}
	}
	return prediction{fee: v, exponent: exponent, perConstraint: bips, known: true}
}

// headRowError is the replay error to publish with a tick: the sampled head's own, once its
// prediction has been shifted into place. A tick that wrote no row for the head (no new block, or a
// batch ending elsewhere) keeps the last published value rather than reporting a fresh zero.
func (f *Follower) headRowError(rows []db.Block, head uint64) int64 {
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Number == head {
			return db.ReplayErrorBips(rows[i])
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastErrBips
}

// carryLocked is the pricing group for first, taken from the last committed tick. It is known only
// when that tick replayed first's parent: after a restart, a reorg or across a gap the committed
// result is gone or belongs to another block, and first starts a new chain without a prediction.
func (f *Follower) carryLocked(first uint64) prediction {
	if f.lastResult == nil || f.lastResult.Predicted == nil || f.head == 0 || f.head+1 != first {
		return prediction{}
	}
	return predictionOf(*f.lastResult)
}

// predictionOf reads the group a replay wrote onto its own row, before shiftPredictions moves it.
func predictionOf(r pricer.Result) prediction {
	bips := make(pq.Int64Array, len(r.PerConstraint))
	for k, v := range r.PerConstraint {
		bips[k] = int64(v)
	}
	return prediction{fee: r.Predicted, exponent: int64(r.Exponent), perConstraint: bips, known: true}
}

// shiftPredictions moves each row's pricing group onto the block it prices. ArbOS computes the fee
// while processing block N and writes it into the header of N+1, so a replay's output for N describes
// N+1 and comparing it against N's own header measures how far the fee moved rather than how wrong the
// model is. carry is the group from the block before rows[0]; the zero value leaves the first row
// without a prediction, which is what a cold start or the block after a gap gets. The group of the
// last row is returned to carry into the next contiguous batch. A break in numbering ends the chain:
// the row after it starts without a prediction.
func shiftPredictions(rows []db.Block, carry prediction) prediction {
	next := carry
	var prev uint64
	for i := range rows {
		if i > 0 && rows[i].Number != prev+1 {
			next = prediction{}
		}
		prev = rows[i].Number
		cur := prediction{
			fee: rows[i].PredictedBaseFee.Wei.BigInt(), exponent: rows[i].ExponentBips,
			perConstraint: rows[i].ConstraintBips, known: rows[i].PredictedBaseFee.Valid,
		}
		rows[i].PredictedBaseFee, rows[i].ExponentBips, rows[i].ConstraintBips = db.NullWei{}, 0, nil
		if next.known {
			rows[i].PredictedBaseFee = db.NewNullWei(next.fee)
			rows[i].ExponentBips, rows[i].ConstraintBips = next.exponent, next.perConstraint
		}
		next = cur
	}
	return next
}

// blockRows joins headers with replay results. Rows written here always carry the full pricing
// breakdown, so their fee split is exact. The pricing group each row receives here is the one the
// replay produced at that block, which prices the next one; shiftPredictions moves it into place.
// arbOSVersionOf reads the version Nitro wrote into the header mix digest. A synthesized header or a
// node that omits mixHash is stored as NULL; a present version zero remains zero.
func arbOSVersionOf(h nitro.Header) sql.NullInt64 {
	if !h.HasArbOSVersion() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(h.ArbOSVersion), Valid: true}
}

func blockRows(chainID uint64, headers []nitro.Header, results []pricer.Result, minFee func(uint64) *big.Int) []db.Block {
	rows := make([]db.Block, len(headers))
	for i, h := range headers {
		r := results[i]
		bips := make(pq.Int64Array, len(r.PerConstraint))
		for k, v := range r.PerConstraint {
			bips[k] = int64(v)
		}
		posterGas := sql.NullInt64{}
		if h.PosterGas != nil {
			posterGas = sql.NullInt64{Int64: int64(*h.PosterGas), Valid: true}
		}
		rows[i] = db.Block{
			ChainID:          chainID,
			Number:           h.Number,
			Hash:             h.Hash,
			ParentHash:       h.ParentHash,
			TS:               time.Unix(int64(h.Timestamp), 0).UTC(),
			GasUsed:          h.GasUsed,
			PosterGas:        posterGas,
			BaseFee:          db.NewWei(h.BaseFee),
			L1Block:          h.L1BlockNumber,
			TxCount:          h.TxCount,
			Backlogs:         db.Uint64Array(append([]uint64{}, r.Backlogs...)),
			ConstraintBips:   bips,
			ExponentBips:     int64(r.Exponent),
			PredictedBaseFee: db.NewNullWei(r.Predicted),
			MinBaseFee:       db.NewNullWei(minFee(h.Number)),
			Anchored:         r.Anchored,
			PricingVersion:   db.PricingFull,
			ArbOSVersion:     arbOSVersionOf(h),
		}
	}
	return rows
}

// buildSnapshot assembles the LiveSnapshot from a sample. ethUsd is the spot the caller already checked
// for staleness, nil when there is none.
// liveArbOSVersion and liveFidelity read the sampled head's own version. A header without one leaves
// the fidelity unknown rather than claiming a verified replay.
func liveArbOSVersion(h nitro.Header) *uint64 {
	if !h.HasArbOSVersion() {
		return nil
	}
	v := h.ArbOSVersion
	return &v
}

func liveFidelity(h nitro.Header, pricingModel pricer.Model) string {
	v := liveArbOSVersion(h)
	return model.ReplayFidelity(v, v, func(low, high uint64) bool {
		return pricer.VerifiedRange(pricingModel, low, high)
	})
}

func buildSnapshot(chainID uint64, sample *nitro.Sample, replayErrorBips int64, gps model.GasPerSecond, computeGPS model.NullableGasPerSecond, l1 *model.L1, accounts *model.Accounts, ethUsd *model.EthUsd) model.LiveSnapshot {
	live := stateFromSample(sample)
	_, exponent, per := live.Step(0)
	h := sample.Header
	minFee := sample.MinBaseFee
	if minFee == nil {
		minFee = new(big.Int)
	}
	pricingModel := pricer.ModelConstraints
	if sample.IsLegacy() {
		pricingModel = pricer.ModelLegacy
	}
	snap := model.LiveSnapshot{
		ChainID:   chainID,
		SampledAt: sample.SampledAt.UTC().Format(time.RFC3339),
		Block: model.LiveBlock{
			Number: h.Number, TS: h.Timestamp, GasUsed: h.GasUsed, PosterGas: h.PosterGas, BaseFee: weiString(h.BaseFee), TxCount: h.TxCount,
		},
		BaseFee:        weiString(h.BaseFee),
		MinBaseFee:     minFee.String(),
		MultiplierBips: multiplierBips(h.BaseFee, minFee),
		ExponentBips:   int64(exponent),
		Model:          model.ModelConstraints,
		Constraints:    make([]model.Constraint, 0, len(sample.Constraints)),
		Prices: model.Prices{
			PerL2Tx: weiString(sample.Prices.PerL2Tx), PerL1CalldataByte: weiString(sample.Prices.PerL1CalldataByte),
			PerL2Storage: weiString(sample.Prices.PerL2Storage), PerArbGasBase: weiString(sample.Prices.PerArbGasBase),
			PerArbGasCongestion: weiString(sample.Prices.PerArbGasCongestion), PerArbGasTotal: weiString(sample.Prices.PerArbGasTotal),
		},
		GasPerSecond:        gps,
		ComputeGasPerSecond: computeGPS,
		L1:                  l1,
		Accounts:            accounts,
		ReplayErrorBips:     replayErrorBips,
		ArbOSVersion:        liveArbOSVersion(h),
		ReplayFidelity:      liveFidelity(h, pricingModel),
		EthUsd:              ethUsd,
	}
	for i, c := range sample.Constraints {
		snap.Constraints = append(snap.Constraints, model.Constraint{Target: c.Target, Window: c.Window, Backlog: c.Backlog, ExponentBips: int64(per[i])})
	}
	if sample.IsLegacy() {
		snap.Model = model.ModelLegacy
		if sample.Legacy != nil {
			snap.Legacy = &model.LegacyParams{SpeedLimit: sample.Legacy.SpeedLimit, Inertia: sample.Legacy.Inertia, Tolerance: sample.Legacy.Tolerance, Backlog: sample.Legacy.Backlog}
		}
	}
	return snap
}

func dividedPtr(v *uint64, divisor uint64) *uint64 {
	if v == nil {
		return nil
	}
	result := *v / divisor
	return &result
}

// sampleRow converts a snapshot into a state_samples row.
func sampleRow(chainID uint64, sample *nitro.Sample, snap *model.LiveSnapshot, includeSlow bool) (db.StateSample, error) {
	constraints, err := db.MarshalJSONB(snap.Constraints)
	if err != nil {
		return db.StateSample{}, fmt.Errorf("encode constraints: %w", err)
	}
	prices, err := db.MarshalJSONB(snap.Prices)
	if err != nil {
		return db.StateSample{}, fmt.Errorf("encode prices: %w", err)
	}
	row := db.StateSample{
		ChainID:     chainID,
		SampledAt:   sample.SampledAt.UTC(),
		BlockNumber: sample.Header.Number,
		BaseFee:     db.NewWei(sample.Header.BaseFee),
		MinBaseFee:  db.NewWei(sample.MinBaseFee),
		Constraints: constraints,
		Prices:      prices,
	}
	if snap.Legacy != nil {
		if row.Legacy, err = db.MarshalJSONB(snap.Legacy); err != nil {
			return db.StateSample{}, fmt.Errorf("encode legacy: %w", err)
		}
	}
	if includeSlow {
		if snap.L1 != nil {
			if row.L1, err = db.MarshalJSONB(snap.L1); err != nil {
				return db.StateSample{}, fmt.Errorf("encode l1: %w", err)
			}
		}
		if snap.Accounts != nil {
			if row.Accounts, err = db.MarshalJSONB(snap.Accounts); err != nil {
				return db.StateSample{}, fmt.Errorf("encode accounts: %w", err)
			}
		}
	}
	return row, nil
}

func bigOrZero(v *big.Int) *big.Int {
	if v == nil {
		return new(big.Int)
	}
	return new(big.Int).Set(v)
}

func weiString(v *big.Int) string {
	if v == nil {
		return "0"
	}
	return v.String()
}

func multiplierBips(baseFee, minFee *big.Int) int64 {
	if baseFee == nil || minFee == nil || minFee.Sign() == 0 {
		return 0
	}
	m := new(big.Int).Mul(baseFee, big.NewInt(int64(pricer.OneInBips)))
	m.Div(m, minFee)
	if !m.IsInt64() {
		return 1<<63 - 1
	}
	return m.Int64()
}
