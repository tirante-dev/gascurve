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
	"github.com/tirante-dev/gascurve/internal/model"
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

const (
	// maxPendingHoles bounds the queued gap-filling work. Filling is best
	// effort on a paced endpoint, so a network that keeps skipping can
	// queue ranges faster than they are filled, and the whole queue lives
	// in one JSON checkpoint. Past this many queued ranges the oldest
	// leave the queue with reasonExpired: their blocks are still counted
	// as not indexed in /status, but no filler will fetch them again.
	maxPendingHoles = 64
	// maxHoles bounds the checkpoint itself, unfillable and expired ranges
	// included. Past this the oldest entries are forgotten entirely rather
	// than growing a row without end.
	maxHoles = 256
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

// saveHoles writes the holes checkpoint normalized (see normalizeHoles),
// dropping it when none are left. Every write goes through here, so the
// stored list is always ordered, free of overlaps and bounded.
func (f *Follower) saveHoles(ctx context.Context, s db.Store, holes []hole) error {
	holes = f.normalizeHoles(holes)
	if len(holes) == 0 {
		return s.DeleteState(ctx, f.chainID, db.StateHoles)
	}
	b, err := json.Marshal(holes)
	if err != nil {
		return err
	}
	return s.SetState(ctx, f.chainID, db.StateHoles, string(b))
}

// normalizeHoles orders the recorded ranges by start, merges the ones that
// overlap or touch, and bounds the list. Only ranges of the same class are
// merged (queued work with queued work, unfillable with unfillable), so a
// range nothing can be replayed into never swallows fillable work. A reorg
// or a second skip that records a range overlapping one already queued
// therefore merges into it instead of duplicating it, and the merged entry
// keeps the progress of the range it starts at together with the highest
// fold watermark of the entries, so nothing is folded into a bucket twice.
func (f *Follower) normalizeHoles(holes []hole) []hole {
	out := make([]hole, 0, len(holes))
	for _, h := range holes {
		if h.To >= h.From {
			out = append(out, h)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	merged := make([]hole, 0, len(out))
	for _, h := range out {
		if n := len(merged); n > 0 {
			prev := &merged[n-1]
			if prev.Reason == h.Reason && prev.To < ^uint64(0) && h.From <= prev.To+1 {
				mergeHole(prev, h)
				continue
			}
		}
		merged = append(merged, h)
	}
	return f.capHoles(merged)
}

// mergeHole folds h into the entry that starts at or before it. The cursor
// and its replay state belong to that earlier entry, since the blocks
// below its cursor are the ones already filled; the fold watermark is the
// higher of the two, because a block folded into an additive bucket by
// either entry must not be folded again.
func mergeHole(into *hole, h hole) {
	into.To = max(into.To, h.To)
	into.Folded = max(into.Folded, h.Folded)
	if into.At == "" || (h.At != "" && h.At < into.At) {
		into.At = h.At
	}
}

// capHoles bounds the queue and the checkpoint. The filler works newest
// first, so the ranges dropped are the oldest ones, which are also the
// least likely to still have a stored state to replay from.
func (f *Follower) capHoles(holes []hole) []hole {
	pending := 0
	for _, h := range holes {
		if h.Reason == "" {
			pending++
		}
	}
	for i := range holes {
		if pending <= maxPendingHoles {
			break
		}
		if holes[i].Reason != "" {
			continue
		}
		f.log.Warn("the gap fill queue is full, dropping the oldest range out of it",
			"from", holes[i].From, "to", holes[i].To, "queued", pending)
		holes[i].Reason, holes[i].Next, holes[i].State = reasonExpired, 0, nil
		pending--
	}
	if len(holes) <= maxHoles {
		return holes
	}
	drop := len(holes) - maxHoles
	f.log.Warn("the holes checkpoint is full, forgetting the oldest ranges", "dropped", drop)
	return holes[drop:]
}

// fillTarget is the hole the next fill step works on: the block the replay
// continues from (its number, hash and timestamp) and the pricer state in
// force at the end of it. The block is the stored one before the range
// when it is still there, otherwise the one the hole's own replay state
// describes.
type fillTarget struct {
	h      hole
	prev   uint64
	hash   string
	prevTs uint64
	state  *pricer.State
}

// FillStep advances the newest fillable hole by at most one header batch.
// Skipped ranges are queued work: the headers come back through the bulk
// lane (the fast tick's reserve is untouched), the pricer replays forward
// from the state at the block before the range exactly as the live path
// does, and the blocks and their buckets are written under the chain lock
// with the generation re-checked, so a rewind during the fetch discards
// the work instead of writing it on top of the canonical chain.
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
	target, reasons, err := f.pickHole(ctx, holes)
	if err != nil {
		return FillNone, err
	}
	if len(reasons) > 0 {
		// A range whose anchor is gone is reclassified straight away, so
		// it stops being counted as work that will never be done. It is
		// still examined once per pass and takes the mark off again if a
		// state ever appears before it.
		if err := f.store.WithChainTx(ctx, f.chainID, func(s db.Store) error {
			return f.markHoles(ctx, s, reasons)
		}); err != nil {
			return FillNone, err
		}
	}
	if target == nil {
		return FillNone, nil
	}
	if f.historyMustWait() {
		// The fast loop needs the budget for the head: history waits.
		return FillIdle, nil
	}
	return f.fillBatch(ctx, gen, target)
}

// pickHole chooses the newest hole that can be filled: the state at the
// block before the range (the last filled one once the cursor has moved)
// must be known, either from the hole's own replay checkpoint or from the
// stored block there. Holes recorded as unfillable are examined only once
// nothing else is fillable, and one whose state has since appeared (the
// backfill reached it, or an archive endpoint arrived) loses that mark.
// Deciding costs database reads only, never an RPC call. The second return
// value carries the ranges that must be marked unfillable, keyed by their
// start.
func (f *Follower) pickHole(ctx context.Context, holes []hole) (*fillTarget, map[uint64]string, error) {
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
	var reasons map[uint64]string
	for _, unfillable := range []bool{false, true} {
		for _, i := range order {
			h := holes[i]
			if (h.Reason != "") != unfillable {
				continue
			}
			target, stranded, err := f.targetFor(ctx, h)
			if err != nil {
				return nil, reasons, err
			}
			if target == nil {
				if stranded && h.Reason == "" {
					f.log.Warn("nothing is left to replay a queued range forward from, recording it as unfillable",
						"from", h.From, "to", h.To)
					if reasons == nil {
						reasons = map[uint64]string{}
					}
					reasons[h.From] = reasonNoState
				}
				continue
			}
			if h.Reason != "" {
				f.log.Info("a hole recorded without state can be replayed now", "from", h.From, "to", h.To)
				target.h.Reason = ""
			}
			return target, reasons, nil
		}
	}
	return nil, reasons, nil
}

// markHoles records the reasons pickHole decided on, matching entries by
// their start so a range recorded meanwhile is left alone.
func (f *Follower) markHoles(ctx context.Context, s db.Store, reasons map[uint64]string) error {
	holes, err := f.loadHoles(ctx, s)
	if err != nil {
		return err
	}
	changed := false
	for i := range holes {
		reason, ok := reasons[holes[i].From]
		if !ok || holes[i].Reason == reason {
			continue
		}
		holes[i].Reason = reason
		changed = true
	}
	if !changed {
		return nil
	}
	return f.saveHoles(ctx, s, holes)
}

// targetFor reports whether one hole can be filled now, returning the
// state to replay forward from. The stored block before the range is
// preferred when it is still there, since it is the chain's own record,
// and the hole's carried replay state is cross-checked against its hash: a
// checkpoint describing another block at that height came from a fork that
// is gone. When the row has been pruned the carried state stands on its
// own, which is the whole point of carrying it. Nil with stranded set
// means neither exists and the range is not work any more.
func (f *Follower) targetFor(ctx context.Context, h hole) (target *fillTarget, stranded bool, err error) {
	start := h.Start()
	if start == 0 || h.To < start {
		return nil, false, nil
	}
	prev, err := f.store.BlockByNumber(ctx, f.chainID, start-1)
	if err != nil {
		return nil, false, fmt.Errorf("block %d: %w", start-1, err)
	}
	carried := h.State
	if carried != nil && carried.Block != start-1 {
		// The checkpoint describes another position than the cursor does.
		carried = nil
	}
	if prev != nil && prev.Known() && len(prev.Backlogs) > 0 {
		if carried != nil && hashMismatch(carried.Hash, prev.Hash) {
			f.log.Warn("the hole's replay state describes another block than the stored one, taking the stored block",
				"block", prev.Number, "from", h.From, "to", h.To)
			carried = nil
		}
		st, err := f.storedFillState(ctx, *prev)
		if err != nil {
			return nil, false, err
		}
		if st != nil {
			return &fillTarget{h: h, prev: prev.Number, hash: prev.Hash, prevTs: uint64(prev.TS.Unix()), state: st}, false, nil
		}
	}
	if carried != nil {
		if st := carriedFillState(carried); st != nil {
			return &fillTarget{h: h, prev: carried.Block, hash: carried.Hash, prevTs: carried.PrevTS, state: st}, false, nil
		}
	}
	// A stored predecessor whose state cannot be built yet is a wait, not
	// a dead end: the shape may still be recorded. A predecessor that is
	// not there at all never comes back, so the range is reclassified.
	return nil, prev == nil, nil
}

// storedFillState builds the pricer state at the end of a stored block:
// the shape that was really in force there, the row's end-of-block
// backlogs and the row's floor. The shape comes from the recorded
// constraint set covering the block, or from the newest state sample taken
// at or before it, never from the live sample: a legacy chain whose speed
// limit, inertia or backlog tolerance changed after the gap would
// otherwise be replayed with today's parameters, and the recorded changes
// can only be applied forward, never reversed. Nil when nothing describes
// the pricer there.
func (f *Follower) storedFillState(ctx context.Context, prev db.Block) (*pricer.State, error) {
	sample, err := f.store.StateSampleAt(ctx, f.chainID, prev.Number)
	if err != nil {
		return nil, fmt.Errorf("state sample at %d: %w", prev.Number, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.shapeAtLocked(prev.Number, sample)
	if st == nil || len(st.Backlogs()) != len(prev.Backlogs) {
		return nil, nil
	}
	st.SetBacklogs(prev.Backlogs)
	st.MinBaseFee = new(big.Int).Set(prev.MinBaseFee.Wei.BigInt())
	return st, nil
}

// shapeAtLocked builds the pricer shape in force at a block from what was
// recorded there: a stored state sample taken at or before it on a legacy
// chain (its parameters, with the recorded changes up to the block
// applied), the recorded constraint set when one covers the block, else
// the sample's own constraints. Backlogs are zero: the caller sets the
// real ones. Nil when neither a set nor a sample describes the block.
func (f *Follower) shapeAtLocked(number uint64, sample *db.StateSample) *pricer.State {
	if sample != nil && len(sample.Legacy) > 0 {
		var l model.LegacyParams
		if err := sample.Legacy.Unmarshal(&l); err != nil {
			return nil
		}
		base := &pricer.Legacy{SpeedLimit: l.SpeedLimit, Inertia: l.Inertia, Tolerance: l.Tolerance}
		return &pricer.State{MinBaseFee: new(big.Int), Legacy: f.timelineLocked(nil).legacyAt(number, base)}
	}
	if cs := f.setAt(number); cs != nil {
		if entries, err := setEntries(*cs); err == nil {
			st := stateFromEntries(entries, new(big.Int))
			st.SetBacklogs(make([]uint64, len(entries)))
			return st
		}
	}
	if sample == nil || len(sample.Constraints) == 0 {
		return nil
	}
	var constraints []model.Constraint
	if err := sample.Constraints.Unmarshal(&constraints); err != nil || len(constraints) == 0 {
		return nil
	}
	st := &pricer.State{MinBaseFee: new(big.Int), Constraints: make([]pricer.Constraint, len(constraints))}
	for i, c := range constraints {
		st.Constraints[i] = pricer.Constraint{Target: c.Target, Window: c.Window}
	}
	return st
}

// carriedFillState rebuilds the pricer state a hole carries. Nil when the
// checkpoint describes no model at all.
func carriedFillState(s *model.HoleState) *pricer.State {
	st := &pricer.State{MinBaseFee: new(big.Int)}
	if s.MinBaseFee != "" {
		v, ok := new(big.Int).SetString(s.MinBaseFee, 10)
		if !ok {
			return nil
		}
		st.MinBaseFee = v
	}
	switch {
	case s.Legacy != nil:
		st.Legacy = &pricer.Legacy{SpeedLimit: s.Legacy.SpeedLimit, Inertia: s.Legacy.Inertia,
			Tolerance: s.Legacy.Tolerance, Backlog: s.Legacy.Backlog}
	case len(s.Constraints) > 0:
		st.Constraints = make([]pricer.Constraint, len(s.Constraints))
		for i, c := range s.Constraints {
			st.Constraints[i] = pricer.Constraint{Target: c.Target, Window: c.Window, Backlog: c.Backlog}
		}
	default:
		return nil
	}
	return st
}

// holeStateOf captures the replay state at the end of a filled batch, so
// the next one continues from it without reading a stored block.
func holeStateOf(st *pricer.State, last nitro.Header, setID int64) *model.HoleState {
	out := &model.HoleState{Block: last.Number, Hash: last.Hash, PrevTS: last.Timestamp, SetID: setID}
	if st.MinBaseFee != nil {
		out.MinBaseFee = st.MinBaseFee.String()
	}
	if st.Legacy != nil {
		out.Legacy = &model.LegacyParams{SpeedLimit: st.Legacy.SpeedLimit, Inertia: st.Legacy.Inertia,
			Tolerance: st.Legacy.Tolerance, Backlog: st.Legacy.Backlog}
		return out
	}
	out.Constraints = make([]model.Constraint, len(st.Constraints))
	for i, c := range st.Constraints {
		out.Constraints[i] = model.Constraint{Target: c.Target, Window: c.Window, Backlog: c.Backlog}
	}
	return out
}

// fillBatch fetches, replays and commits one batch of a hole. The batch is
// sized to the budget the other loops leave spare, exactly like the
// backfill, so the filler never holds the pacer's turnstile through a long
// sleep. The headers must link to the block the replay continues from and,
// on the last batch, to the stored block after the range: a chain that
// does not link is retried rather than written.
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
	if hashMismatch(t.hash, headers[0].ParentHash) {
		return FillIdle, fmt.Errorf("gap header %d does not build on the block %d it continues from, retrying", headers[0].Number, t.prev)
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
	rows, filledTail, end := f.replayHole(t, headers, tail)
	h.Next, h.State = from+n, end
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
// the catch-up does, and returns the replay state at the end of the batch
// for the hole's checkpoint. When the batch ends the hole, the stored
// block after it is replayed too, anchored to its sampled backlogs, and
// the prediction and start-of-block exponents of that replay are written
// back onto it (its own header fields, floor and sampled backlogs stay as
// they are), so the error of the whole reconstruction is recorded against
// a block whose state was really sampled. A stored block carrying another
// pricer shape than the replay ends on cannot be joined to it: the range
// is still written, its error simply stays unrecorded.
func (f *Follower) replayHole(t *fillTarget, headers []nitro.Header, tail *db.Block) (rows []db.Block, filled *db.Block, end *model.HoleState) {
	f.mu.Lock()
	tl := f.timelineLocked(nil)
	f.mu.Unlock()
	st := t.state
	rows, _ = replayForward(f.chainID, st, t.prevTs, headers, tl, nil)
	lastHeader := headers[len(headers)-1]
	var setID int64
	f.mu.Lock()
	if cs := f.setAt(lastHeader.Number); cs != nil {
		setID = cs.ID
	}
	f.mu.Unlock()
	// The checkpoint is taken before the tail is replayed through the same
	// state: the tail belongs to the block after the range, not to it.
	end = holeStateOf(st, lastHeader, setID)
	if tail == nil {
		return rows, nil, end
	}
	if len(tail.Backlogs) != len(st.Backlogs()) {
		f.log.Warn("the block after the gap carries another pricer shape, leaving its replay error unrecorded",
			"block", tail.Number, "backlogs", len(tail.Backlogs), "replay", len(st.Backlogs()))
		return rows, nil, end
	}
	backlogs := tail.Backlogs.Uint64s()
	head := nitro.Header{
		Number: tail.Number, Hash: tail.Hash, ParentHash: tail.ParentHash, Timestamp: uint64(tail.TS.Unix()),
		GasUsed: tail.GasUsed, BaseFee: tail.BaseFee.BigInt(), L1BlockNumber: tail.L1Block, TxCount: tail.TxCount,
	}
	anchored, _ := replayForward(f.chainID, st, lastHeader.Timestamp, []nitro.Header{head}, tl,
		func(uint64) ([]uint64, bool) { return backlogs, true })
	merged := *tail
	merged.PredictedBaseFee = anchored[0].PredictedBaseFee
	merged.ExponentBips, merged.ConstraintBips = anchored[0].ExponentBips, anchored[0].ConstraintBips
	f.log.Info("gap replay reached the sampled head", "block", merged.Number, "replayErrorBips", db.ReplayErrorBips(merged))
	return rows, &merged, end
}

// commitFill writes one batch of a hole and its progress in one chain
// transaction, but only when the chain has not been rewound since the
// headers were fetched. Blocks from the hour of the first live block on
// are rows the buckets are rebuilt from, exactly as the live path writes
// them; anything older folds additively, so a hole that has aged past that
// boundary cannot wipe the backfill's folds. A block below the hole's fold
// watermark was already counted into its bucket before a rewind reset the
// cursor: it is replayed for the state it carries forward but never folded
// twice. The stored block after the range is classified by the same
// boundary rule as the rest: above it, it is a row whose bucket is rebuilt
// from rows; below it, its gas is already in an additive bucket, so only
// the row is rewritten and the bucket is left alone. The hole entry gains
// its cursor and replay state, or disappears when the range is complete.
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
	rowBacked, additive, keptBuckets := []db.Block{}, []db.Block{}, []db.Block{}
	for _, r := range rows {
		switch {
		case hasBoundary && !r.TS.Before(boundary):
			rowBacked = append(rowBacked, r)
		case r.Number < h.Folded:
			f.log.Debug("skipping a bucket fold that is already committed", "block", r.Number, "folded", h.Folded)
		default:
			additive = append(additive, r)
		}
	}
	if tail != nil {
		if hasBoundary && !tail.TS.Before(boundary) {
			rowBacked = append(rowBacked, *tail)
		} else {
			keptBuckets = append(keptBuckets, *tail)
		}
	}
	h.Folded = max(h.Folded, h.Next)
	buckets := db.FoldBlocks(additive, func(n uint64) sql.NullInt64 { return setIDs[n] })
	err := f.withGeneration(ctx, gen, func(s db.Store) error {
		if stored := append(append([]db.Block{}, rowBacked...), keptBuckets...); len(stored) > 0 {
			if err := s.UpsertBlocks(ctx, stored); err != nil {
				return fmt.Errorf("gap blocks: %w", err)
			}
		}
		if len(rowBacked) > 0 {
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
// one meanwhile, and normalizing may have merged a newer range into it, so
// it is matched by its start and may have grown), then updated with the
// cursor, the replay state and the fold watermark, or removed when the
// range it covers is complete. An entry that is no longer there was
// dropped by a rewind and is not written back.
func (f *Follower) advanceHole(ctx context.Context, s db.Store, h hole, done bool) error {
	holes, err := f.loadHoles(ctx, s)
	if err != nil {
		return err
	}
	out := make([]hole, 0, len(holes))
	for _, cur := range holes {
		if cur.From != h.From || cur.To < h.To {
			out = append(out, cur)
			continue
		}
		if done && cur.To == h.To {
			continue
		}
		cur.Next, cur.State, cur.Reason = h.Next, h.State, h.Reason
		cur.Folded = max(cur.Folded, h.Folded)
		out = append(out, cur)
	}
	return f.saveHoles(ctx, s, out)
}

// rewindHoles restarts every hole a rewind reached into: a filler had
// written the blocks below the hole's cursor, and the ones above the
// ancestor went with the rest of the orphaned chain, so the cursor and the
// replay state describing them go back to the start of the range and the
// whole gap is fetched again from the canonical chain. The fold watermark
// deliberately survives that reset: the additive buckets below the bucket
// boundary are not rebuilt from rows, so the blocks already counted into
// them must not be counted again when the range is refilled. It is cleared
// only when the rewind deleted those buckets with the backfill's.
func (f *Follower) rewindHoles(ctx context.Context, s db.Store, ancestor uint64, bucketsCleared bool) error {
	holes, err := f.loadHoles(ctx, s)
	if err != nil || len(holes) == 0 {
		return err
	}
	changed := false
	for i, h := range holes {
		if bucketsCleared && h.Folded > 0 {
			holes[i].Folded = 0
			changed = true
		}
		if h.Next == 0 || h.Next-1 <= ancestor {
			continue
		}
		f.log.Warn("reorg reaches into a hole being filled, restarting it", "from", h.From, "to", h.To, "ancestor", ancestor)
		holes[i].Next, holes[i].State = 0, nil
		changed = true
	}
	if !changed {
		return nil
	}
	return f.saveHoles(ctx, s, holes)
}
