package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// Tick runs one fast iteration: sample the head, catch up on headers,
// replay forward from the last committed state, write blocks, buckets and
// the state sample in one transaction, then NOTIFY. Nothing in memory
// advances before that transaction commits.
func (f *Follower) Tick(ctx context.Context) error {
	return f.tickWith(ctx, f.rpc.FastSample)
}

// TickAt is Tick with the sample pinned to one block number, used when a
// newHeads event names the head so state and header match exactly.
func (f *Follower) TickAt(ctx context.Context, number uint64) error {
	return f.tickWith(ctx, func(ctx context.Context) (*nitro.Sample, error) {
		return f.rpc.FastSampleAt(ctx, number)
	})
}

// tickWith runs a tick around sampleFn. The sample (the head number, the
// head-pinned header and the state calls) is the latency critical part
// of the tick and goes through the endpoint's fast lane; the catch-up
// headers and everything after them are bulk work.
func (f *Follower) tickWith(ctx context.Context, sampleFn func(context.Context) (*nitro.Sample, error)) error {
	if err := f.ensureInit(ctx); err != nil {
		return f.fail(ctx, err)
	}
	sample, err := sampleFn(nitro.WithClass(ctx, nitro.Fast))
	if err != nil {
		return f.fail(ctx, fmt.Errorf("sample: %w", err))
	}
	head := sample.Header.Number

	f.mu.Lock()
	stored, storedHash := f.head, f.headHash
	f.mu.Unlock()

	switch {
	case stored == 0:
		// Fresh database: nothing before the sampled head is known, so
		// the head is persisted alone and replay starts after it.
		return f.seed(ctx, sample, nil, nil)
	case head < stored:
		return f.headBehind(ctx, sample, stored)
	case head == stored && !hashMismatch(storedHash, sample.Header.Hash):
		return f.sampleOnly(ctx, sample)
	}

	f.catchingUp.Store(true)
	defer f.catchingUp.Store(false)

	if head == stored {
		// Same height, different hash: the stored head is off-chain.
		if stored, err = f.rewindToAncestor(ctx, stored-1); err != nil {
			return f.fail(ctx, err)
		}
	}
	headers, err := f.catchUp(ctx, sample, stored, f.policy())
	if err != nil {
		return f.fail(ctx, err)
	}
	if headers == nil {
		return nil // the gap was skipped and the head seeded
	}
	if hashMismatch(f.headHashOf(stored), headers[0].ParentHash) {
		// The first missing block does not build on the stored head.
		ancestor, err := f.rewindToAncestor(ctx, stored-1)
		if err != nil {
			return f.fail(ctx, err)
		}
		if headers, err = f.catchUp(ctx, sample, ancestor, f.policy()); err != nil {
			return f.fail(ctx, err)
		}
		if headers == nil {
			return nil
		}
	}
	if err := verifyChain(headers); err != nil {
		return f.fail(ctx, err)
	}
	return f.process(ctx, sample, headers)
}

// headBehind handles a reported head below the stored one. An endpoint
// that has simply not caught up still reports the same hashes for the
// blocks it does have, so the stored row at the reported head is compared
// with the header the node reports there (the sample carries it): only a
// hash that differs proves the stored chain is off-chain. A confirmed
// rollback is rewound like any other reorg; a lagging endpoint is skipped.
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
	f.catchingUp.Store(true)
	defer f.catchingUp.Store(false)
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

// hashMismatch reports whether two hashes are both known and differ. Rows
// written before hashes were stored cannot be checked and pass.
func hashMismatch(a, b string) bool {
	return a != "" && b != "" && a != b
}

// verifyChain checks that consecutive headers link by parent hash, so a
// reorg during the fetch is noticed instead of replayed.
func verifyChain(headers []nitro.Header) error {
	for i := 1; i < len(headers); i++ {
		if hashMismatch(headers[i-1].Hash, headers[i].ParentHash) {
			return fmt.Errorf("headers %d and %d do not link, retrying", headers[i-1].Number, headers[i].Number)
		}
	}
	return nil
}

// catchUp fetches the headers after stored up to the sampled head and
// appends the sampled header. On a paced network (pol is the active
// endpoint's policy, taken once for the whole tick) a gap over budget is
// skipped: the owner actions the gap crosses are still fetched and
// committed with the seed, so the pricing timeline stays complete and the
// notifications are not held back until the slow scan; the head is seeded
// alone, the hole recorded, and nil returned.
func (f *Follower) catchUp(ctx context.Context, sample *nitro.Sample, stored uint64, pol policy) ([]nitro.Header, error) {
	head := sample.Header.Number
	if stored == 0 {
		return nil, f.seed(ctx, sample, nil, nil)
	}
	from := stored + 1
	maxGap := uint64(f.cfg.HeaderBatchSize) * uint64(f.cfg.MaxCatchUpBatches)
	if gap := head - stored; !pol.unlimited && gap > maxGap {
		h := hole{From: from, To: head - 1}
		f.log.Warn("catch-up gap exceeds budget, skipping blocks and restarting from the sampled head", "from", h.From, "to", h.To)
		pending, err := f.fetchOwnerRange(ctx, from, head)
		if err != nil {
			return nil, err
		}
		return nil, f.seed(ctx, sample, &h, pending)
	}
	headers, err := f.fetchHeaders(ctx, from, head-1)
	if err != nil {
		return nil, fmt.Errorf("headers %d..%d: %w", from, head-1, err)
	}
	return append(headers, sample.Header), nil
}

// fetchHeaders reads [from, to] in header_batch_size batches.
func (f *Follower) fetchHeaders(ctx context.Context, from, to uint64) ([]nitro.Header, error) {
	if to < from {
		return nil, nil
	}
	out := make([]nitro.Header, 0, to-from+1)
	for start := from; start <= to; start += uint64(f.cfg.HeaderBatchSize) {
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

// rewindToAncestor finds the highest stored block at or below from whose
// hash the node still reports and, holding the chain lock so no slow scan
// or backfill transaction from the old fork can interleave, deletes
// everything after it in one transaction: blocks and their bucket
// contributions, owner actions, constraint sets, batch reports, state
// samples, the owner and batch cursors, the owner scan checkpoint, and,
// when they lie above the ancestor, the live start and the backfill cursor
// (whose backfill-only buckets are dropped for re-backfilling). It reloads
// the head from the database and returns it.
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
		// Every writer that fetched from the old fork compares this counter
		// inside its own transaction and discards its work.
		if err := f.bumpGeneration(ctx, s); err != nil {
			return err
		}
		removed, err := s.DeleteBlocksAfter(ctx, f.chainID, ancestor)
		if err != nil {
			return fmt.Errorf("delete blocks: %w", err)
		}
		for _, res := range db.ResolutionOrder {
			if err := s.RebuildBuckets(ctx, f.chainID, res, db.BucketStarts(removed, res)); err != nil {
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
		if err := f.rewindBackfill(ctx, s, ancestor, boundary, hasBoundary); err != nil {
			return err
		}
		if ls != nil && ls.Block > ancestor {
			// The live history starts over at the next committed head.
			clearedStart = true
			if err := s.DeleteState(ctx, f.chainID, db.StateLiveStart); err != nil {
				return fmt.Errorf("live start: %w", err)
			}
		}
		// The network row moves to the surviving ancestor in the same
		// transaction, so /status never shows the orphaned head after a
		// rewind whose replacement fetch or persistence then fails.
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

// rewindNetworkHead moves networks.head_block and head_at down to the
// surviving ancestor inside the rewind transaction. An ancestor with no
// row left (everything was orphaned) leaves the head at zero.
func (f *Follower) rewindNetworkHead(ctx context.Context, s db.Store, ancestor uint64) error {
	at := time.Unix(0, 0).UTC()
	if ancestor > 0 {
		row, err := s.BlockByNumber(ctx, f.chainID, ancestor)
		if err != nil {
			return fmt.Errorf("ancestor block %d: %w", ancestor, err)
		}
		if row != nil {
			at = row.TS
		}
	}
	if err := s.UpdateNetworkHead(ctx, f.chainID, ancestor, at, f.now().UTC()); err != nil {
		return fmt.Errorf("network head: %w", err)
	}
	return nil
}

// rewindBackfill resets the backfill to a safe checkpoint when its range
// reaches above the ancestor: everything it folded came from the old fork
// or was cut off, so its backfill-only buckets are dropped and the cursor
// starts over (the row-backed buckets above the boundary are rebuilt from
// the surviving rows by the rewind itself).
func (f *Follower) rewindBackfill(ctx context.Context, s db.Store, ancestor uint64, boundary time.Time, hasBoundary bool) error {
	raw, ok, err := s.GetState(ctx, f.chainID, db.StateBackfillCursor)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	c := &backfillCursor{}
	if err := json.Unmarshal([]byte(raw), c); err != nil {
		return fmt.Errorf("backfill cursor: %w", err)
	}
	if c.Top == 0 || c.Top-1 <= ancestor {
		return nil
	}
	f.log.Warn("backfill range reaches above the reorg ancestor, restarting the backfill", "top", c.Top, "ancestor", ancestor)
	if hasBoundary {
		if _, err := s.DeleteBucketsBefore(ctx, f.chainID, boundary); err != nil {
			return err
		}
	}
	return f.saveCursor(ctx, s, &backfillCursor{})
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

// findAncestor walks down from a block comparing stored hashes with the
// node's until they agree. A row without a stored hash (written before
// hashes were kept) is unknown rather than trusted: it is the ancestor
// only when the node's header at that height matches every field the row
// holds (timestamp, gas used, base fee). A height without any row is the
// ancestor: nothing below it can be off-chain.
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

// seed persists the sampled head alone: its backlogs come from the
// sample, nothing before it is replayed. A skipped range is recorded as a
// hole and pending owner actions found in it are kept. The replay
// continues forward from this block.
func (f *Follower) seed(ctx context.Context, sample *nitro.Sample, h *hole, pending []*nitro.OwnerAction) error {
	st := stateFromSample(sample)
	// The start-of-block exponents that priced the seed are approximated
	// from the sampled end-of-block backlogs minus the block's own gas.
	start := st.Clone()
	backlogs := start.Backlogs()
	for i := range backlogs {
		backlogs[i] = pricer.SaturatingUSub(backlogs[i], sample.Header.GasUsed)
	}
	start.SetBacklogs(backlogs)
	hdr := sample.Header
	results := pricer.Replay(start, 0, []pricer.Block{{Number: hdr.Number, Timestamp: hdr.Timestamp, GasUsed: hdr.GasUsed, BaseFee: hdr.BaseFee}},
		func(uint64) ([]uint64, bool) { return sampleBacklogs(sample), true })
	r := results[0]
	// No known state preceded the seed, so it carries no prediction.
	r.Predicted, r.ErrorBips = new(big.Int).Set(hdr.BaseFee), 0
	rows := blockRows(f.chainID, []nitro.Header{hdr}, []pricer.Result{r}, func(uint64) *big.Int { return st.MinBaseFee })
	if err := f.persist(ctx, sample, rows, &r, h, pending); err != nil {
		return f.fail(ctx, err)
	}
	f.publish(sample, st, &r)
	return nil
}

// process replays headers (the last one is the sampled head) forward from
// the last committed state and persists. The whole interval is scanned for
// owner actions first (one eth_getLogs): every pricing change in it,
// constraint sets (including a reset that leaves the shape unchanged), the
// minimum fee and the legacy parameters, splits the replay at its block,
// and the actions commit with the tick.
func (f *Follower) process(ctx context.Context, sample *nitro.Sample, headers []nitro.Header) error {
	head := sample.Header.Number
	f.mu.Lock()
	st, prevTs, err := f.replayStateLocked(ctx, sample)
	f.mu.Unlock()
	if err != nil {
		return f.fail(ctx, err)
	}
	if st == nil {
		h := hole{From: headers[0].Number, To: head - 1}
		f.log.Warn("no known replay state before the first missing block, restarting from the sampled head", "from", h.From, "to", h.To)
		return f.seed(ctx, sample, &h, nil)
	}
	pending, err := f.fetchOwnerActions(ctx, headers[0].Number, head)
	if err != nil {
		return f.fail(ctx, err)
	}
	f.mu.Lock()
	tl := f.timelineLocked(pending)
	f.mu.Unlock()
	rows, results, ok := replayLive(f.chainID, st, prevTs, headers, sample, tl)
	if !ok {
		h := hole{From: headers[0].Number, To: head - 1}
		f.log.Warn("pricer parameters changed without a recorded owner action, restarting from the sampled head", "from", h.From, "to", h.To)
		return f.seed(ctx, sample, &h, pending)
	}
	headResult := results[len(results)-1]
	if err := f.persist(ctx, sample, rows, &headResult, nil, pending); err != nil {
		return f.fail(ctx, err)
	}
	f.publish(sample, st, &headResult)
	return nil
}

// replayLive replays headers through st, splitting at every boundary of
// the timeline and anchoring the head to the sample. ok is false when the
// state's shape or minimum fee still differs from the sample's after the
// replay: a change happened that no recorded action explains.
func replayLive(chainID uint64, st *pricer.State, prevTs uint64, headers []nitro.Header, sample *nitro.Sample, tl *timeline) (rows []db.Block, results []pricer.Result, ok bool) {
	head := sample.Header.Number
	anchor := func(n uint64) ([]uint64, bool) {
		if n == head {
			return sampleBacklogs(sample), true
		}
		return nil, false
	}
	fees := map[uint64]*big.Int{}
	start := 0
	for start < len(headers) {
		tl.applyAt(st, headers[start].Number)
		end := start + 1
		for end < len(headers) && !tl.boundaryAt(headers[end].Number) {
			end++
		}
		chunk := headers[start:end]
		blocks := make([]pricer.Block, len(chunk))
		for i, h := range chunk {
			blocks[i] = pricer.Block{Number: h.Number, Timestamp: h.Timestamp, GasUsed: h.GasUsed, BaseFee: h.BaseFee}
			fees[h.Number] = new(big.Int).Set(st.MinBaseFee)
		}
		results = append(results, pricer.Replay(st, prevTs, blocks, anchor)...)
		prevTs = chunk[len(chunk)-1].Timestamp
		start = end
	}
	if !sameShape(st, sample) || st.MinBaseFee.Cmp(bigOrZero(sample.MinBaseFee)) != 0 {
		return nil, nil, false
	}
	rows = blockRows(chainID, headers, results, func(n uint64) *big.Int { return fees[n] })
	return rows, results, true
}

// replayStateLocked returns a clone of the committed replay state, or
// rebuilds it from the stored head row after a restart or a failed commit.
// Nil means the state before the first missing block is unknown.
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
	// The floor at the stored head: the row's when a recorded change covers
	// it (a backfill row carries the timeline's fee, a live row the fee in
	// force when it was written); when nothing recorded explains the block
	// the live value stands in for the unknown history, since the catch-up
	// scan finds any change after the row and a default is no evidence.
	tl := f.timelineLocked(nil)
	switch {
	case tl.minFeeChangeBlock(last.Number) == 0:
		st.MinBaseFee = bigOrZero(sample.MinBaseFee)
	case last.MinBaseFee.Valid && last.MinBaseFee.Wei.BigInt().Sign() > 0:
		st.MinBaseFee = new(big.Int).Set(last.MinBaseFee.Wei.BigInt())
	default:
		st.MinBaseFee = tl.minFeeAt(last.Number)
	}
	return st, uint64(last.TS.Unix()), nil
}

// stateAtLocked builds the pricer shape in force at a block: the recorded
// constraint set, or the sampled shape when none is recorded; a legacy
// chain takes the sample's parameters with the recorded changes up to the
// block applied.
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

// sampleOnly handles a tick where no new block appeared: the replay state
// is re-anchored and a fresh snapshot is published.
func (f *Follower) sampleOnly(ctx context.Context, sample *nitro.Sample) error {
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
	if err := f.persist(ctx, sample, nil, last, nil, nil); err != nil {
		return f.fail(ctx, err)
	}
	f.publish(sample, st, last)
	return nil
}

// publish makes a committed tick the follower's state: head, replay state,
// sample and result move together, only after the commit.
func (f *Follower) publish(sample *nitro.Sample, st *pricer.State, headResult *pricer.Result) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.head = sample.Header.Number
	f.headHash = sample.Header.Hash
	f.prevTs = sample.Header.Timestamp
	f.state = st
	f.lastSample = sample
	f.lastResult = headResult
	if f.liveStart == nil {
		f.liveStart = &liveStart{Block: sample.Header.Number, TS: int64(sample.Header.Timestamp)}
	}
}

// observedSetLocked returns the observed constraint set to record with a
// tick: the sampled constraints at the sampled block whenever the latest
// known set (recorded, or pending in this tick) has another shape. It is
// nil for legacy samples and when the latest set matches; an unreadable
// latest set is left alone rather than shadowed every tick.
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

// persist writes everything for a tick in one chain-locked transaction and
// notifies: the owner actions found in the catch-up interval with their
// constraint sets and notifications, an observed set when the sampled
// shape is not the latest known one, the blocks, their buckets (rebuilt
// from the rows in their windows, so a retried or overlapping fold changes
// nothing), the state sample, the head and the live start. The caches
// follow the commit.
func (f *Follower) persist(ctx context.Context, sample *nitro.Sample, rows []db.Block, headResult *pricer.Result, h *hole, pending []*nitro.OwnerAction) error {
	head := sample.Header.Number
	headAt := time.Unix(int64(sample.Header.Timestamp), 0).UTC()
	f.mu.Lock()
	l1, accounts, ls := f.l1, f.accounts, f.liveStart
	// The staleness rule is applied at publication, not at the fetch: a
	// quote older than eth_usd_max_age is dropped from this snapshot and
	// the NOTIFY payload rather than shown as live.
	ethUsd := ethUsdModel(f.ethUsdPrice, f.now(), f.cfg.EthUsdMaxAge)
	slowGen := f.slowGen
	includeSlow := slowGen > f.slowSaved
	observed := f.observedSetLocked(sample, pending)
	f.mu.Unlock()
	lastErr := f.endpointErrorNow()

	var snapshot *model.LiveSnapshot
	reload := false
	err := f.store.WithChainTx(ctx, f.chainID, func(s db.Store) error {
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
		g10, err := s.GasUsedBetween(ctx, f.chainID, headAt.Add(-10*time.Second), headAt)
		if err != nil {
			return fmt.Errorf("gas per second: %w", err)
		}
		g60, err := s.GasUsedBetween(ctx, f.chainID, headAt.Add(-60*time.Second), headAt)
		if err != nil {
			return fmt.Errorf("gas per second: %w", err)
		}
		snap := buildSnapshot(f.chainID, sample, headResult, model.GasPerSecond{S10: g10 / 10, S60: g60 / 60}, l1, accounts, ethUsd)
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
		// A tick clears networks.last_error; a disabled endpoint is a
		// standing error even while the network keeps running on another
		// one, so it is written back in the same transaction.
		if lastErr != "" {
			if err := s.SetNetworkError(ctx, f.chainID, lastErr); err != nil {
				return fmt.Errorf("endpoint error: %w", err)
			}
		}
		if err := s.SetState(ctx, f.chainID, db.StateHead, fmt.Sprint(head)); err != nil {
			return fmt.Errorf("head checkpoint: %w", err)
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
		// Only the generation this transaction actually wrote is cleared: a
		// slow sample published while it ran keeps its own claim on the
		// next tick.
		f.slowSaved = max(f.slowSaved, slowGen)
	}
	if reload {
		if err := f.reloadSetsLocked(ctx); err != nil {
			return fmt.Errorf("after commit: %w", err)
		}
	}
	return nil
}

// fail records the error on the network row, reloads the committed head
// and state from the database (the transaction may have failed after the
// server committed) and returns the error.
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

// endpointErrorNow summarizes the pool's disabled endpoints, "" without a
// pool or while every endpoint is usable.
func (f *Follower) endpointErrorNow() string {
	if f.pool == nil {
		return ""
	}
	return endpointError(f.pool.Status())
}

// blockRows joins headers with replay results. minFee gives the minimum
// base fee in force at each block. Rows written here always carry the full
// pricing breakdown, so their fee split is exact.
func blockRows(chainID uint64, headers []nitro.Header, results []pricer.Result, minFee func(uint64) *big.Int) []db.Block {
	rows := make([]db.Block, len(headers))
	for i, h := range headers {
		r := results[i]
		bips := make(pq.Int64Array, len(r.PerConstraint))
		for k, v := range r.PerConstraint {
			bips[k] = int64(v)
		}
		rows[i] = db.Block{
			ChainID:          chainID,
			Number:           h.Number,
			Hash:             h.Hash,
			ParentHash:       h.ParentHash,
			TS:               time.Unix(int64(h.Timestamp), 0).UTC(),
			GasUsed:          h.GasUsed,
			BaseFee:          db.NewWei(h.BaseFee),
			L1Block:          h.L1BlockNumber,
			TxCount:          h.TxCount,
			Backlogs:         db.Uint64Array(append([]uint64{}, r.Backlogs...)),
			ConstraintBips:   bips,
			ExponentBips:     int64(r.Exponent),
			PredictedBaseFee: db.NewWei(r.Predicted),
			MinBaseFee:       db.NewNullWei(minFee(h.Number)),
			Anchored:         r.Anchored,
			PricingVersion:   db.PricingFull,
		}
	}
	return rows
}

// buildSnapshot assembles the LiveSnapshot from a sample. ethUsd is the
// spot the caller already checked for staleness, nil when there is none.
func buildSnapshot(chainID uint64, sample *nitro.Sample, headResult *pricer.Result, gps model.GasPerSecond, l1 *model.L1, accounts *model.Accounts, ethUsd *model.EthUsd) model.LiveSnapshot {
	live := stateFromSample(sample)
	_, exponent, per := live.Step(0)
	h := sample.Header
	minFee := sample.MinBaseFee
	if minFee == nil {
		minFee = new(big.Int)
	}
	snap := model.LiveSnapshot{
		ChainID:   chainID,
		SampledAt: sample.SampledAt.UTC().Format(time.RFC3339),
		Block: model.LiveBlock{
			Number: h.Number, TS: h.Timestamp, GasUsed: h.GasUsed, BaseFee: weiString(h.BaseFee), TxCount: h.TxCount,
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
		GasPerSecond:    gps,
		L1:              l1,
		Accounts:        accounts,
		ReplayErrorBips: headResult.ErrorBips,
		EthUsd:          ethUsd,
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

// bigOrZero copies v, treating nil as zero.
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
