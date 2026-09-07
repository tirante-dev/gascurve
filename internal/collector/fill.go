package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/metrics"
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

// holesState summarizes the recorded holes for the instruments, the same
// way /status summarizes them.
func holesState(holes []hole) metrics.HolesState {
	s := model.SummarizeHoles(holes)
	return metrics.HolesState{Pending: s.Pending, Blocks: s.Blocks, Unfillable: s.Unfillable}
}

// loadHoles reads every durable missing range. A replay state or timestamp
// that cannot be decoded is an error: the row remains in place and recovery
// stops instead of silently treating incomplete history as complete.
func (f *Follower) loadHoles(ctx context.Context, s db.Store) ([]hole, error) {
	rows, err := s.MissingRanges(ctx, f.chainID)
	if err != nil {
		return nil, err
	}
	holes := make([]hole, len(rows))
	for i, row := range rows {
		h, err := holeFromRow(row)
		if err != nil {
			return nil, fmt.Errorf("missing range %d..%d: %w", row.From, row.To, err)
		}
		holes[i] = h
	}
	return holes, nil
}

// saveHoles writes normalized durable rows. Every field is encoded before the
// replacement starts, so a malformed in-memory checkpoint cannot remove the
// rows already committed. Collector callers hold the chain transaction.
func (f *Follower) saveHoles(ctx context.Context, s db.Store, holes []hole) error {
	holes = f.normalizeHoles(holes)
	rows := make([]db.MissingRange, len(holes))
	for i, h := range holes {
		if h.At == "" {
			h.At = f.now().UTC().Format(time.RFC3339)
		}
		row, err := f.rowFromHole(h)
		if err != nil {
			return fmt.Errorf("missing range %d..%d: %w", h.From, h.To, err)
		}
		rows[i] = row
	}
	return s.ReplaceMissingRanges(ctx, f.chainID, rows)
}

func holeFromRow(row db.MissingRange) (hole, error) {
	h := hole{
		From: row.From, To: row.To, At: row.DetectedAt.UTC().Format(time.RFC3339), Lifecycle: row.Lifecycle,
		Next: row.Cursor, Folded: row.Folded, Reason: row.Reason, RetryCount: row.RetryCount,
		LastAttemptAt: formatNullTime(row.LastAttemptAt), NextRetryAt: formatNullTime(row.NextRetryAt),
		PredecessorAt: formatNullTime(row.PredecessorAt), SuccessorAt: formatNullTime(row.SuccessorAt),
		CursorAt: formatNullTime(row.CursorAt),
	}
	if row.LastError.Valid {
		h.LastError = row.LastError.String
	}
	if row.ReplayState != nil {
		var state model.HoleState
		if err := row.ReplayState.Unmarshal(&state); err != nil {
			return hole{}, fmt.Errorf("replay state: %w", err)
		}
		h.State = &state
	}
	return h, nil
}

func (f *Follower) rowFromHole(h hole) (db.MissingRange, error) {
	detected, err := requiredTime(h.At, "detected at")
	if err != nil {
		return db.MissingRange{}, err
	}
	var state db.JSONB
	if h.State != nil {
		state, err = db.MarshalJSONB(h.State)
		if err != nil {
			return db.MissingRange{}, fmt.Errorf("replay state: %w", err)
		}
	}
	lastAttempt, err := optionalTime(h.LastAttemptAt, "last attempt at")
	if err != nil {
		return db.MissingRange{}, err
	}
	nextRetry, err := optionalTime(h.NextRetryAt, "next retry at")
	if err != nil {
		return db.MissingRange{}, err
	}
	predecessor, err := optionalTime(h.PredecessorAt, "predecessor at")
	if err != nil {
		return db.MissingRange{}, err
	}
	successor, err := optionalTime(h.SuccessorAt, "successor at")
	if err != nil {
		return db.MissingRange{}, err
	}
	cursorAt, err := optionalTime(h.CursorAt, "cursor at")
	if err != nil {
		return db.MissingRange{}, err
	}
	lifecycle := effectiveLifecycle(h)
	return db.MissingRange{
		ChainID: f.chainID, From: h.From, To: h.To, DetectedAt: detected, Lifecycle: lifecycle, Reason: h.Reason,
		Cursor: h.Next, ReplayState: state, Folded: h.Folded, RetryCount: h.RetryCount,
		LastAttemptAt: lastAttempt, NextRetryAt: nextRetry, LastError: sql.NullString{String: h.LastError, Valid: h.LastError != ""},
		PredecessorAt: predecessor, SuccessorAt: successor, CursorAt: cursorAt, CreatedAt: detected,
	}, nil
}

func requiredTime(raw, field string) (time.Time, error) {
	v, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s %q: %w", field, raw, err)
	}
	return v.UTC(), nil
}

func optionalTime(raw, field string) (sql.NullTime, error) {
	if raw == "" {
		return sql.NullTime{}, nil
	}
	v, err := requiredTime(raw, field)
	return sql.NullTime{Time: v, Valid: err == nil}, err
}

func formatNullTime(v sql.NullTime) string {
	if !v.Valid {
		return ""
	}
	return v.Time.UTC().Format(time.RFC3339)
}

// importLegacyHoles moves the old JSON checkpoint into durable rows under the
// chain lock. The checkpoint is deleted only after every range and replay
// state decoded and the replacement succeeded. An unreadable document is a
// startup error and remains available for repair instead of being overwritten.
func (f *Follower) importLegacyHoles(ctx context.Context) error {
	return f.store.WithChainTx(ctx, f.chainID, func(s db.Store) error {
		raw, ok, err := s.GetState(ctx, f.chainID, db.StateHoles)
		if err != nil || !ok {
			return err
		}
		var legacy []hole
		if err := json.Unmarshal([]byte(raw), &legacy); err != nil {
			return fmt.Errorf("decode legacy holes checkpoint: %w", err)
		}
		existing, err := f.loadHoles(ctx, s)
		if err != nil {
			return err
		}
		for i := range legacy {
			if legacy[i].At == "" {
				legacy[i].At = f.now().UTC().Format(time.RFC3339)
			}
			switch legacy[i].Reason {
			case reasonNoState:
				legacy[i].Lifecycle = rangeBlocked
			case reasonExpired:
				// The old queue cap made this range permanently unfillable even
				// when it had an anchor. Durable storage restores it to work.
				legacy[i].Lifecycle = rangePending
				legacy[i].Reason = reasonCatchUpLimit
			default:
				legacy[i].Lifecycle = rangePending
			}
		}
		if err := f.saveHoles(ctx, s, append(existing, legacy...)); err != nil {
			return err
		}
		return s.DeleteState(ctx, f.chainID, db.StateHoles)
	})
}

// normalizeHoles orders the recorded ranges by start and merges the ones that
// overlap or touch. Only ranges in the same recovery class and with the same
// reason are merged (fillable work with fillable work, blocked with blocked),
// so a range nothing can be replayed into never swallows fillable work. A reorg
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
			if compatibleLifecycle(*prev, h) && prev.Reason == h.Reason && prev.To < ^uint64(0) && h.From <= prev.To+1 {
				mergeHole(prev, h)
				continue
			}
		}
		merged = append(merged, h)
	}
	return merged
}

func effectiveLifecycle(h hole) string {
	if h.Lifecycle != "" {
		return h.Lifecycle
	}
	if h.Reason == reasonNoState {
		return rangeBlocked
	}
	return rangePending
}

// compatibleLifecycle treats pending and retrying as the same recovery class.
// A repeated skip can overlap a range whose last attempt failed, and those
// intervals must still normalize into one row. Blocked work stays separate.
func compatibleLifecycle(a, b hole) bool {
	return (effectiveLifecycle(a) == rangeBlocked) == (effectiveLifecycle(b) == rangeBlocked)
}

// mergeHole folds h into the entry that starts at or before it. The cursor
// and its replay state belong to that earlier entry, since the blocks
// below its cursor are the ones already filled; the fold watermark is the
// higher of the two, because a block folded into an additive bucket by
// either entry must not be folded again.
func mergeHole(into *hole, h hole) {
	oldTo := into.To
	into.To = max(into.To, h.To)
	into.Folded = max(into.Folded, h.Folded)
	if effectiveLifecycle(*into) == rangeRetrying || effectiveLifecycle(h) == rangeRetrying {
		into.Lifecycle = rangeRetrying
	} else {
		into.Lifecycle = effectiveLifecycle(*into)
	}
	if h.From == into.From && h.Next > into.Next {
		into.Next, into.State, into.CursorAt = h.Next, h.State, h.CursorAt
	}
	if into.At == "" || (h.At != "" && h.At < into.At) {
		into.At = h.At
	}
	// PredecessorAt describes the block below From. The merged entry starts at
	// into.From, so the incoming bound only applies when both ranges start at
	// the same block. Otherwise it names a block inside the merged interval.
	if into.PredecessorAt == "" && h.From == into.From {
		into.PredecessorAt = h.PredecessorAt
	}
	if h.To >= oldTo && h.SuccessorAt != "" {
		into.SuccessorAt = h.SuccessorAt
	}
	if h.LastAttemptAt > into.LastAttemptAt {
		into.LastAttemptAt, into.NextRetryAt, into.LastError = h.LastAttemptAt, h.NextRetryAt, h.LastError
	}
	into.RetryCount = max(into.RetryCount, h.RetryCount)
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
	if err != nil {
		return FillNone, err
	}
	f.metrics.ObserveHoles(holesState(holes))
	if len(holes) == 0 {
		return FillNone, nil
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
	if f.recoveryMustWait() {
		// The fast loop needs the budget for the head: history waits.
		return FillIdle, nil
	}
	status, err := f.fillBatch(ctx, gen, target)
	if err == nil || errors.Is(err, errStaleGeneration) {
		return status, err
	}
	if retryErr := f.recordFillFailure(ctx, target.h, err); retryErr != nil {
		return status, errors.Join(err, fmt.Errorf("record missing-range retry: %w", retryErr))
	}
	return status, err
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
	for _, blocked := range []bool{false, true} {
		for _, i := range order {
			h := holes[i]
			if (effectiveLifecycle(h) == rangeBlocked) != blocked {
				continue
			}
			if effectiveLifecycle(h) == rangeRetrying && h.NextRetryAt != "" {
				next, err := requiredTime(h.NextRetryAt, "next retry at")
				if err != nil {
					return nil, reasons, err
				}
				if next.After(f.now()) {
					continue
				}
			}
			target, stranded, err := f.targetFor(ctx, h)
			if err != nil {
				return nil, reasons, err
			}
			if target == nil {
				if stranded && effectiveLifecycle(h) != rangeBlocked {
					f.log.Warn("nothing is left to replay a queued range forward from, recording it as unfillable",
						"from", h.From, "to", h.To)
					if reasons == nil {
						reasons = map[uint64]string{}
					}
					reasons[h.From] = reasonNoState
				}
				continue
			}
			if effectiveLifecycle(h) == rangeBlocked {
				f.log.Info("a hole recorded without state can be replayed now", "from", h.From, "to", h.To)
				target.h.Reason = ""
				target.h.Lifecycle = rangePending
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
		holes[i].Lifecycle = rangeBlocked
		holes[i].NextRetryAt = ""
		changed = true
	}
	if !changed {
		return nil
	}
	return f.saveHoles(ctx, s, holes)
}

// recordFillFailure moves one range into a durable retry lifecycle. It matches
// the interval that still contains the attempted range under the chain lock,
// so a concurrent extension or merge cannot resurrect stale cursor state. The
// backoff is bounded and survives restart with the count, time and RPC error.
func (f *Follower) recordFillFailure(ctx context.Context, attempted hole, cause error) error {
	return f.store.WithChainTx(ctx, f.chainID, func(s db.Store) error {
		holes, err := f.loadHoles(ctx, s)
		if err != nil {
			return err
		}
		changed := false
		for i := range holes {
			if holes[i].From > attempted.From || holes[i].To < attempted.To {
				continue
			}
			holes[i].Lifecycle = rangeRetrying
			holes[i].RetryCount++
			now := f.now().UTC()
			holes[i].LastAttemptAt = now.Format(time.RFC3339)
			holes[i].NextRetryAt = now.Add(retryDelay(holes[i].RetryCount)).Format(time.RFC3339)
			holes[i].LastError = cause.Error()
			changed = true
			break
		}
		if !changed {
			return nil
		}
		return f.saveHoles(ctx, s, holes)
	})
}

func retryDelay(count uint64) time.Duration {
	delay := restartDelay
	for i := uint64(1); i < count && delay < 5*time.Minute; i++ {
		delay *= 2
	}
	return min(delay, 5*time.Minute)
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
	h.CursorAt = time.Unix(int64(headers[len(headers)-1].Timestamp), 0).UTC().Format(time.RFC3339)
	h.Lifecycle, h.NextRetryAt, h.LastError = rangePending, "", ""
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
		f.metrics.HoleFilled()
		f.log.Info("gap filled", "from", h.From, "to", h.To)
	}
	return nil
}

// advanceHole records a hole's progress inside the commit: the entry is
// found again in the stored checkpoint (another writer may have appended
// one meanwhile, and normalizing may have merged a newer range into it, so
// it is matched by its start and may have grown), then updated with the
// cursor, the replay state and the fold watermark, or removed when the
// range it covers is complete. An entry another writer already completed
// is not recreated.
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
		cur.Next, cur.State, cur.Reason, cur.Lifecycle = h.Next, h.State, h.Reason, h.Lifecycle
		cur.CursorAt, cur.NextRetryAt, cur.LastError = h.CursorAt, h.NextRetryAt, h.LastError
		cur.RetryCount, cur.LastAttemptAt = max(cur.RetryCount, h.RetryCount), h.LastAttemptAt
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
		holes[i].Next, holes[i].State, holes[i].CursorAt = 0, nil, ""
		changed = true
	}
	if !changed {
		return nil
	}
	return f.saveHoles(ctx, s, holes)
}
