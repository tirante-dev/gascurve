package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"

	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/nitro"
	"github.com/tirante-dev/gascurve/internal/pricer"
)

// FillStatus is the outcome of one gap fill step.
type FillStatus int

// Gap fill step outcomes.
const (
	// FillProgressed means one batch of a hole was fetched, replayed and
	// committed, or a finished hole was removed.
	FillProgressed FillStatus = iota
	// FillIdle means there is fillable work but this step did none: the
	// fast loop is catching up and history yields to it.
	FillIdle
	// FillNone means no recorded hole can be filled, so the caller is free
	// to spend the step on the backfill.
	FillNone
)

// loadHoles reads the recorded holes. An unreadable checkpoint is reported
// as none: it is a record of what is missing, never chain state, so it is
// rewritten rather than made fatal.
func (f *Follower) loadHoles(ctx context.Context, s db.Store) ([]hole, error) {
	raw, ok, err := s.GetState(ctx, f.chainID, db.StateHoles)
	if err != nil || !ok {
		return nil, err
	}
	var holes []hole
	if err := json.Unmarshal([]byte(raw), &holes); err != nil {
		f.log.Warn("unreadable holes checkpoint, starting over", "err", err.Error())
		return nil, nil
	}
	return holes, nil
}

// saveHoles writes the holes checkpoint, dropping it when none are left.
func (f *Follower) saveHoles(ctx context.Context, s db.Store, holes []hole) error {
	if len(holes) == 0 {
		return s.DeleteState(ctx, f.chainID, db.StateHoles)
	}
	b, err := json.Marshal(holes)
	if err != nil {
		return err
	}
	return s.SetState(ctx, f.chainID, db.StateHoles, string(b))
}

// fillTarget is the hole the next fill step works on, with the stored
// block it replays forward from and the pricer state in force at the end
// of that block.
type fillTarget struct {
	h     hole
	prev  db.Block
	state *pricer.State
}

// FillStep advances the newest fillable hole by at most one header batch.
// Skipped ranges are queued work: the headers come back through the bulk
// lane (the fast tick's reserve is untouched), the pricer replays forward
// from the stored block before the range exactly as the live path does,
// and the blocks and their buckets are written under the chain lock with
// the generation re-checked, so a rewind during the fetch discards the
// work instead of writing it on top of the canonical chain.
func (f *Follower) FillStep(ctx context.Context) (FillStatus, error) {
	status, err := f.fillStep(ctx)
	if errors.Is(err, errStaleGeneration) {
		// The fast loop rewound while this step was fetching: its headers
		// describe the old fork, so nothing is written and the next step
		// starts again from the committed cursor.
		f.log.Warn("gap fill step discarded after a rewind", "err", err.Error())
		return FillIdle, nil
	}
	return status, err
}

func (f *Follower) fillStep(ctx context.Context) (FillStatus, error) {
	if err := f.ensureInit(ctx); err != nil {
		return FillNone, err
	}
	// The generation is captured before any network call and compared
	// inside the transaction that commits this step.
	gen, err := f.generation(ctx, f.store)
	if err != nil {
		return FillNone, err
	}
	holes, err := f.loadHoles(ctx, f.store)
	if err != nil || len(holes) == 0 {
		return FillNone, err
	}
	target, err := f.pickHole(ctx, holes)
	if err != nil || target == nil {
		return FillNone, err
	}
	if f.historyMustWait() {
		// The fast loop needs the budget for the head: history waits.
		return FillIdle, nil
	}
	return f.fillBatch(ctx, gen, target)
}

// pickHole chooses the newest hole that can be filled: the block before
// the range (the last filled one once the cursor has moved) must be
// stored with the whole pricing breakdown, and the pricer shape in force
// there must be known. Holes recorded as unfillable are examined only
// once nothing else is fillable, and one whose state has since appeared
// (the backfill reached it, or an archive endpoint arrived) loses that
// mark. Deciding costs database reads only, never an RPC call.
func (f *Follower) pickHole(ctx context.Context, holes []hole) (*fillTarget, error) {
	order := make([]int, 0, len(holes))
	for i := range holes {
		order = append(order, i)
	}
	// A hole already being filled comes first, so one is finished before
	// the next is begun (a network that keeps skipping would otherwise
	// start every new hole and complete none); then newest first, since
	// the recent charts are the ones being looked at.
	sort.SliceStable(order, func(a, b int) bool {
		ha, hb := holes[order[a]], holes[order[b]]
		if (ha.Next > 0) != (hb.Next > 0) {
			return ha.Next > 0
		}
		return ha.From > hb.From
	})
	for _, unfillable := range []bool{false, true} {
		for _, i := range order {
			h := holes[i]
			if (h.Reason != "") != unfillable {
				continue
			}
			target, err := f.targetFor(ctx, h)
			if err != nil {
				return nil, err
			}
			if target == nil {
				continue
			}
			if h.Reason != "" {
				f.log.Info("a hole recorded without state can be replayed now", "from", h.From, "to", h.To)
				target.h.Reason = ""
			}
			return target, nil
		}
	}
	return nil, nil
}

// targetFor reports whether one hole can be filled now, returning the
// stored block to replay forward from. Nil means not now.
func (f *Follower) targetFor(ctx context.Context, h hole) (*fillTarget, error) {
	start := h.Start()
	if start == 0 || h.To < start {
		return nil, nil
	}
	prev, err := f.store.BlockByNumber(ctx, f.chainID, start-1)
	if err != nil {
		return nil, fmt.Errorf("block %d: %w", start-1, err)
	}
	if prev == nil || !prev.Known() || len(prev.Backlogs) == 0 {
		return nil, nil
	}
	f.mu.Lock()
	st := f.fillStateLocked(*prev)
	f.mu.Unlock()
	if st == nil {
		return nil, nil
	}
	return &fillTarget{h: h, prev: *prev, state: st}, nil
}

// fillStateLocked builds the pricer state at the end of a stored block:
// the shape in force there (the recorded constraint set, or the sampled
// parameters with the recorded changes applied on a legacy chain), the
// row's end-of-block backlogs and the row's floor. Nil when no shape is
// known yet, which is the case until the first sample of the process.
func (f *Follower) fillStateLocked(prev db.Block) *pricer.State {
	if f.lastSample == nil {
		return nil
	}
	st := f.stateAtLocked(prev.Number, f.lastSample)
	if st == nil || len(st.Backlogs()) != len(prev.Backlogs) {
		return nil
	}
	st.SetBacklogs(prev.Backlogs)
	st.MinBaseFee = new(big.Int).Set(prev.MinBaseFee.Wei.BigInt())
	return st
}

// fillBatch fetches, replays and commits one batch of a hole. The batch is
// sized to the budget the other loops leave spare, exactly like the
// backfill, so the filler never holds the pacer's turnstile through a long
// sleep. The headers must link to the stored block before the range and,
// on the last batch, to the stored block after it: a chain that does not
// link is retried rather than written.
func (f *Follower) fillBatch(ctx context.Context, gen uint64, t *fillTarget) (FillStatus, error) {
	h := t.h
	from := h.Start()
	if from == h.From {
		f.log.Info("filling a gap", "from", h.From, "to", h.To, "blocks", h.Blocks())
	}
	remaining := h.To - from + 1
	n := min(uint64(f.cfg.HeaderBatchSize), remaining)
	if avail := uint64(max(f.rpc.Available(), 0)); avail < n {
		n = max(avail, min(minBackfillBatch, remaining))
	}
	numbers := make([]uint64, 0, n)
	for i := uint64(0); i < n; i++ {
		numbers = append(numbers, from+i)
	}
	headers, err := f.rpc.HeadersByNumbers(ctx, numbers)
	if err != nil {
		return FillIdle, fmt.Errorf("gap headers %d..%d: %w", from, from+n-1, err)
	}
	if len(headers) == 0 {
		return FillIdle, fmt.Errorf("gap headers %d..%d: none returned", from, from+n-1)
	}
	// The cursor tracks what was actually replayed, so an endpoint that
	// answers with less than the batch asked for shortens the step rather
	// than leaving a hole inside the hole.
	n = uint64(len(headers))
	if headers[0].Number != from {
		return FillIdle, fmt.Errorf("gap headers start at %d, asked for %d, retrying", headers[0].Number, from)
	}
	if hashMismatch(t.prev.Hash, headers[0].ParentHash) {
		return FillIdle, fmt.Errorf("gap header %d does not build on the stored block %d, retrying", headers[0].Number, t.prev.Number)
	}
	if err := verifyChain(headers); err != nil {
		return FillIdle, err
	}
	last := from+n-1 == h.To
	var tail *db.Block
	if last {
		if tail, err = f.holeTail(ctx, h, headers[len(headers)-1]); err != nil {
			return FillIdle, err
		}
	}
	rows, filledTail := f.replayHole(t, headers, tail)
	h.Next = from + n
	return FillProgressed, f.commitFill(ctx, gen, h, rows, filledTail, h.Next > h.To)
}

// holeTail returns the stored block just after a hole, the one carrying
// the real sampled backlogs: the replay ends on it so the error of the
// whole reconstructed range is recorded where it can be seen. Nil when
// that block is not stored (a rewind cut the chain there, or retention
// pruned it). A block whose parent is not the last header of the range is
// an error: the range and the stored head belong to different chains.
func (f *Follower) holeTail(ctx context.Context, h hole, prevHeader nitro.Header) (*db.Block, error) {
	row, err := f.store.BlockByNumber(ctx, f.chainID, h.To+1)
	if err != nil {
		return nil, fmt.Errorf("block %d: %w", h.To+1, err)
	}
	if row == nil {
		return nil, nil
	}
	if hashMismatch(row.ParentHash, prevHeader.Hash) {
		return nil, fmt.Errorf("gap header %d does not link to the stored block %d, retrying", prevHeader.Number, row.Number)
	}
	return row, nil
}

// replayHole replays a batch of a hole forward from the state at the block
// before it, splitting at the owner actions the range crosses exactly as
// the catch-up does. When the batch ends the hole, the stored block after
// it is replayed too, anchored to its sampled backlogs, and the prediction
// and start-of-block exponents of that replay are written back onto it
// (its own header fields, floor and sampled backlogs stay as they are), so
// the error of the whole reconstruction is recorded against a block whose
// state was really sampled. A stored block carrying another pricer shape
// than the replay ends on cannot be joined to it: the range is still
// written, its error simply stays unrecorded.
func (f *Follower) replayHole(t *fillTarget, headers []nitro.Header, tail *db.Block) (rows []db.Block, filled *db.Block) {
	f.mu.Lock()
	tl := f.timelineLocked(nil)
	f.mu.Unlock()
	st := t.state
	rows, _ = replayForward(f.chainID, st, uint64(t.prev.TS.Unix()), headers, tl, nil)
	if tail == nil {
		return rows, nil
	}
	if len(tail.Backlogs) != len(st.Backlogs()) {
		f.log.Warn("the block after the gap carries another pricer shape, leaving its replay error unrecorded",
			"block", tail.Number, "backlogs", len(tail.Backlogs), "replay", len(st.Backlogs()))
		return rows, nil
	}
	backlogs := tail.Backlogs.Uint64s()
	head := nitro.Header{
		Number: tail.Number, Hash: tail.Hash, ParentHash: tail.ParentHash, Timestamp: uint64(tail.TS.Unix()),
		GasUsed: tail.GasUsed, BaseFee: tail.BaseFee.BigInt(), L1BlockNumber: tail.L1Block, TxCount: tail.TxCount,
	}
	anchored, _ := replayForward(f.chainID, st, headers[len(headers)-1].Timestamp, []nitro.Header{head}, tl,
		func(uint64) ([]uint64, bool) { return backlogs, true })
	merged := *tail
	merged.PredictedBaseFee = anchored[0].PredictedBaseFee
	merged.ExponentBips, merged.ConstraintBips = anchored[0].ExponentBips, anchored[0].ConstraintBips
	f.log.Info("gap replay reached the sampled head", "block", merged.Number, "replayErrorBips", db.ReplayErrorBips(merged))
	return rows, &merged
}

// commitFill writes one batch of a hole and its progress in one chain
// transaction, but only when the chain has not been rewound since the
// headers were fetched. Blocks from the hour of the first live block on
// are rows the buckets are rebuilt from, exactly as the live path writes
// them; anything older folds additively, so a hole that has aged past that
// boundary cannot wipe the backfill's folds. The tail is the stored block
// after the range and is always a row. The hole entry gains its cursor, or
// disappears when the range is complete.
func (f *Follower) commitFill(ctx context.Context, gen uint64, h hole, rows []db.Block, tail *db.Block, done bool) error {
	f.mu.Lock()
	boundary, hasBoundary := f.boundaryLocked()
	setIDs := make(map[uint64]sql.NullInt64, len(rows))
	for _, r := range rows {
		if cs := f.setAt(r.Number); cs != nil {
			setIDs[r.Number] = sql.NullInt64{Int64: cs.ID, Valid: true}
		}
	}
	f.mu.Unlock()
	var rowBacked, additive []db.Block
	for _, r := range rows {
		if hasBoundary && !r.TS.Before(boundary) {
			rowBacked = append(rowBacked, r)
		} else {
			additive = append(additive, r)
		}
	}
	if tail != nil {
		rowBacked = append(rowBacked, *tail)
	}
	buckets := db.FoldBlocks(additive, func(n uint64) sql.NullInt64 { return setIDs[n] })
	err := f.withGeneration(ctx, gen, func(s db.Store) error {
		if len(rowBacked) > 0 {
			if err := s.UpsertBlocks(ctx, rowBacked); err != nil {
				return fmt.Errorf("gap blocks: %w", err)
			}
			for _, res := range db.ResolutionOrder {
				if err := s.RebuildBuckets(ctx, f.chainID, res, db.BucketStarts(rowBacked, res)); err != nil {
					return fmt.Errorf("gap buckets: %w", err)
				}
			}
		}
		if err := s.FoldBuckets(ctx, buckets); err != nil {
			return fmt.Errorf("gap buckets: %w", err)
		}
		return f.advanceHole(ctx, s, h, done)
	})
	if err != nil {
		return err
	}
	if done {
		f.log.Info("gap filled", "from", h.From, "to", h.To)
	}
	return nil
}

// advanceHole records a hole's progress inside the commit: the entry is
// found again in the stored checkpoint (another writer may have appended
// one meanwhile), then updated with the cursor or removed when the range
// is complete. An entry that is no longer there was dropped by a rewind
// and is not written back.
func (f *Follower) advanceHole(ctx context.Context, s db.Store, h hole, done bool) error {
	holes, err := f.loadHoles(ctx, s)
	if err != nil {
		return err
	}
	out := make([]hole, 0, len(holes))
	for _, cur := range holes {
		switch {
		case cur.From != h.From || cur.To != h.To:
			out = append(out, cur)
		case done:
		default:
			cur.Next, cur.Reason = h.Next, h.Reason
			out = append(out, cur)
		}
	}
	return f.saveHoles(ctx, s, out)
}

// rewindHoles restarts every hole a rewind reached into: a filler had
// written the blocks below the hole's cursor, and the ones above the
// ancestor went with the rest of the orphaned chain, so the cursor goes
// back to the start of the range and the whole gap is fetched again from
// the canonical chain.
func (f *Follower) rewindHoles(ctx context.Context, s db.Store, ancestor uint64) error {
	holes, err := f.loadHoles(ctx, s)
	if err != nil || len(holes) == 0 {
		return err
	}
	changed := false
	for i, h := range holes {
		if h.Next == 0 || h.Next-1 <= ancestor {
			continue
		}
		f.log.Warn("reorg reaches into a hole being filled, restarting it", "from", h.From, "to", h.To, "ancestor", ancestor)
		holes[i].Next = 0
		changed = true
	}
	if !changed {
		return nil
	}
	return f.saveHoles(ctx, s, holes)
}
