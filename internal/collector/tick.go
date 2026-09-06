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

func (f *Follower) tickWith(ctx context.Context, sampleFn func(context.Context) (*nitro.Sample, error)) error {
	if err := f.ensureInit(ctx); err != nil {
		return f.fail(ctx, err)
	}
	sample, err := sampleFn(ctx)
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
		return f.seed(ctx, sample, nil)
	case head < stored:
		f.log.Warn("head behind the stored head, skipping tick", "head", head, "stored", stored)
		return nil
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
	headers, err := f.catchUp(ctx, sample, stored)
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
		if headers, err = f.catchUp(ctx, sample, ancestor); err != nil {
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
// appends the sampled header. On a paced network a gap over budget is
// skipped: the head is seeded alone, the hole recorded, and nil returned.
func (f *Follower) catchUp(ctx context.Context, sample *nitro.Sample, stored uint64) ([]nitro.Header, error) {
	head := sample.Header.Number
	if stored == 0 {
		return nil, f.seed(ctx, sample, nil)
	}
	from := stored + 1
	maxGap := uint64(f.cfg.HeaderBatchSize) * uint64(f.cfg.MaxCatchUpBatches)
	if gap := head - stored; !f.unlimited() && gap > maxGap {
		h := hole{From: from, To: head - 1}
		f.log.Warn("catch-up gap exceeds budget, skipping blocks and restarting from the sampled head", "from", h.From, "to", h.To)
		return nil, f.seed(ctx, sample, &h)
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
// hash the node still reports, deletes everything after it (blocks, their
// bucket contributions, owner actions, constraint sets, batch reports and
// cursors) in one transaction and reloads the head from the database. It
// returns the new head.
func (f *Follower) rewindToAncestor(ctx context.Context, from uint64) (uint64, error) {
	ancestor, err := f.findAncestor(ctx, from)
	if err != nil {
		return 0, err
	}
	f.log.Warn("reorg detected, rewinding", "ancestor", ancestor)
	err = f.store.WithTx(ctx, func(s db.Store) error {
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
		for _, key := range []string{db.StateOwnerLogCursor, db.StateBatchScanCursor} {
			if err := rewindCursor(ctx, s, f.chainID, key, ancestor); err != nil {
				return err
			}
		}
		return s.SetState(ctx, f.chainID, db.StateHead, fmt.Sprint(ancestor))
	})
	if err != nil {
		return 0, fmt.Errorf("rewind to %d: %w", ancestor, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.reloadHeadLocked(ctx); err != nil {
		return 0, err
	}
	if err := f.reloadSetsLocked(ctx); err != nil {
		return 0, err
	}
	f.observedChecked = false
	return f.head, nil
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
// node's until they agree. A block without a stored hash cannot be checked
// and is taken as the ancestor.
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
		if !ok || row.Hash == "" {
			return n, nil
		}
		h, err := f.rpc.HeaderByNumber(ctx, n)
		if err != nil {
			return 0, fmt.Errorf("header %d: %w", n, err)
		}
		if h.Hash == row.Hash {
			return n, nil
		}
	}
	return 0, fmt.Errorf("%w (%d blocks)", errReorgTooDeep, maxReorgDepth)
}

// seed persists the sampled head alone: its backlogs come from the
// sample, nothing before it is replayed. A skipped range is recorded as a
// hole. The replay continues forward from this block.
func (f *Follower) seed(ctx context.Context, sample *nitro.Sample, h *hole) error {
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
	if err := f.persist(ctx, sample, rows, &r, h); err != nil {
		return f.fail(ctx, err)
	}
	f.publish(sample, st, &r)
	return nil
}

// process replays headers (the last one is the sampled head) forward from
// the last committed state and persists.
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
		return f.seed(ctx, sample, &h)
	}
	// A model or fee change since the stored head happened somewhere in
	// the range: fetch the owner actions now so the replay can split at
	// their blocks instead of guessing.
	if !sameShape(st, sample) || st.MinBaseFee.Cmp(bigOrZero(sample.MinBaseFee)) != 0 {
		if err := f.syncOwnerActions(ctx, headers[0].Number, head); err != nil {
			return f.fail(ctx, err)
		}
	}
	rows, results, ok := f.replayLive(st, prevTs, headers, sample)
	if !ok {
		h := hole{From: headers[0].Number, To: head - 1}
		f.log.Warn("pricer parameters changed without a recorded owner action, restarting from the sampled head", "from", h.From, "to", h.To)
		return f.seed(ctx, sample, &h)
	}
	headResult := results[len(results)-1]
	if err := f.persist(ctx, sample, rows, &headResult, nil); err != nil {
		return f.fail(ctx, err)
	}
	f.publish(sample, st, &headResult)
	return nil
}

// replayLive replays headers through st, splitting at recorded owner
// action boundaries (constraint sets and minimum fee changes) and
// anchoring the head to the sample. ok is false when the state's shape
// still differs from the sample's after the replay.
func (f *Follower) replayLive(st *pricer.State, prevTs uint64, headers []nitro.Header, sample *nitro.Sample) (rows []db.Block, results []pricer.Result, ok bool) {
	head := sample.Header.Number
	anchor := func(n uint64) ([]uint64, bool) {
		if n == head {
			return sampleBacklogs(sample), true
		}
		return nil, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fees := map[uint64]*big.Int{}
	start := 0
	for start < len(headers) {
		if cs := f.setAt(headers[start].Number); cs != nil && cs.EffectiveBlock == headers[start].Number && !sample.IsLegacy() {
			if entries, err := setEntries(*cs); err == nil {
				st.Constraints = stateFromEntries(entries, st.MinBaseFee).Constraints
			}
		}
		if f.minFeeChangeBlock(headers[start].Number) == headers[start].Number {
			st.MinBaseFee = f.minFeeAt(headers[start].Number)
		}
		end := start + 1
		for end < len(headers) && !f.boundaryAt(headers[end].Number) {
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
	if !sameShape(st, sample) {
		return nil, nil, false
	}
	st.MinBaseFee = bigOrZero(sample.MinBaseFee)
	rows = blockRows(f.chainID, headers, results, func(n uint64) *big.Int { return fees[n] })
	return rows, results, true
}

// boundaryAt reports whether a recorded owner action changes the pricer at
// a block.
func (f *Follower) boundaryAt(number uint64) bool {
	if cs := f.setAt(number); cs != nil && cs.EffectiveBlock == number {
		return true
	}
	return f.minFeeChangeBlock(number) == number
}

// replayStateLocked returns a clone of the committed replay state, or
// rebuilds it from the stored head row after a restart or a failed commit.
// Nil means the state before the first missing block is unknown.
func (f *Follower) replayStateLocked(ctx context.Context, sample *nitro.Sample) (*pricer.State, uint64, error) {
	f.lastSample = sample
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
	if last.MinBaseFee.BigInt().Sign() > 0 {
		st.MinBaseFee = new(big.Int).Set(last.MinBaseFee.BigInt())
	} else {
		st.MinBaseFee = f.minFeeAt(last.Number)
	}
	return st, uint64(last.TS.Unix()), nil
}

// stateAtLocked builds the pricer shape in force at a block: the recorded
// constraint set, or the sampled shape when none is recorded (a legacy
// chain always takes its parameters from the sample).
func (f *Follower) stateAtLocked(number uint64, sample *nitro.Sample) *pricer.State {
	if sample.IsLegacy() {
		if sample.Legacy == nil {
			return nil
		}
		return &pricer.State{MinBaseFee: new(big.Int), Legacy: &pricer.Legacy{SpeedLimit: sample.Legacy.SpeedLimit, Inertia: sample.Legacy.Inertia, Tolerance: sample.Legacy.Tolerance}}
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
	f.lastSample = sample
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
	if err := f.persist(ctx, sample, nil, last, nil); err != nil {
		return f.fail(ctx, err)
	}
	f.publish(sample, st, last)
	return nil
}

// publish makes a committed tick the follower's state.
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

// syncOwnerActions fetches and records the owner actions in [from, to]
// right away (the slow loop would only see them later) and refreshes the
// set and fee caches. The slow loop's cursor is untouched.
func (f *Follower) syncOwnerActions(ctx context.Context, from, to uint64) error {
	actions, err := f.fetchOwnerActions(ctx, from, to)
	if err != nil {
		return err
	}
	if len(actions) > 0 {
		if err := f.store.WithTx(ctx, func(s db.Store) error { return f.storeOwnerActions(ctx, s, actions) }); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reloadSetsLocked(ctx)
}

// persist writes everything for a tick in one transaction and notifies.
// Buckets touched by new rows are rebuilt from the rows in their windows,
// so a retried or overlapping fold changes nothing.
func (f *Follower) persist(ctx context.Context, sample *nitro.Sample, rows []db.Block, headResult *pricer.Result, h *hole) error {
	head := sample.Header.Number
	headAt := time.Unix(int64(sample.Header.Timestamp), 0).UTC()
	f.mu.Lock()
	l1, accounts, includeSlow, ls := f.l1, f.accounts, f.slowPending, f.liveStart
	f.mu.Unlock()

	var snapshot *model.LiveSnapshot
	err := f.store.WithTx(ctx, func(s db.Store) error {
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
		snap := buildSnapshot(f.chainID, sample, headResult, model.GasPerSecond{S10: g10 / 10, S60: g60 / 60}, l1, accounts)
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
	f.snapshot = snapshot
	if includeSlow {
		f.slowPending = false
	}
	f.mu.Unlock()
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

// blockRows joins headers with replay results. minFee gives the minimum
// base fee in force at each block.
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
			MinBaseFee:       db.NewWei(minFee(h.Number)),
			Anchored:         r.Anchored,
		}
	}
	return rows
}

// buildSnapshot assembles the LiveSnapshot from a sample.
func buildSnapshot(chainID uint64, sample *nitro.Sample, headResult *pricer.Result, gps model.GasPerSecond, l1 *model.L1, accounts *model.Accounts) model.LiveSnapshot {
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
