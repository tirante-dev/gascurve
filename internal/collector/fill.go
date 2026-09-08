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

const (
	// One batch was fetched, replayed and committed, or a finished hole removed.
	FillProgressed FillStatus = iota
	// There is fillable work, but the fast loop is catching up and history yields.
	FillIdle
	// No hole can be filled, so the caller may spend the step on the backfill.
	FillNone
)

func holesState(holes []hole) metrics.HolesState {
	s := model.SummarizeHoles(holes)
	return metrics.HolesState{Pending: s.Pending, Blocks: s.Blocks, Unfillable: s.Unfillable}
}

// loadHoles reads every durable missing range. A row that cannot be decoded stops
// recovery rather than letting incomplete history look complete.
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

// saveHoles replaces the durable rows under the caller's chain transaction. Every field
// is encoded before the replacement starts, so a malformed hole cannot drop committed rows.
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

// importLegacyHoles moves the old JSON checkpoint into durable rows. The checkpoint is
// deleted only once everything decoded and the replacement succeeded, so an unreadable
// document stays available for repair.
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
				// The old queue cap made this range unfillable even when it had an anchor.
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

// normalizeHoles sorts by start and merges overlapping or touching ranges, but only within
// the same recovery class and reason, so blocked work never swallows fillable work. The
// merged entry keeps the progress of the range it starts at and the highest fold watermark,
// so no block is folded into a bucket twice.
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
	if h.Reason == reasonNoState || h.Reason == reasonUnsupportedModel {
		return rangeBlocked
	}
	return rangePending
}

// compatibleLifecycle treats pending and retrying as one recovery class, so a repeated skip
// merges with a range whose last attempt failed. Blocked work stays separate.
func compatibleLifecycle(a, b hole) bool {
	return (effectiveLifecycle(a) == rangeBlocked) == (effectiveLifecycle(b) == rangeBlocked)
}

// mergeHole folds h into the entry starting at or before it. The cursor belongs to that
// earlier entry; the fold watermark is the higher of the two, so no block is folded twice.
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
	// PredecessorAt describes the block below From, so it only carries over when both ranges
	// start at the same block.
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

// fillTarget is the hole the next fill step works on: the block the replay continues from
// and the pricer state at the end of it.
type fillTarget struct {
	h      hole
	prev   uint64
	hash   string
	prevTs uint64
	state  *pricer.State
	// carry is the pricing group prev computed, when a checkpoint recorded it. A stored row no longer
	// holds it (its own group prices itself), so a fill resumed from block rows alone starts without
	// one and leaves its first block unpredicted.
	carry prediction
}

// carriedCarry reads the pending pricing group from a hole checkpoint, empty when there is none.
func carriedCarry(c *model.HoleState) prediction {
	if c == nil {
		return prediction{}
	}
	return decodeCarry(c.PendingFee, c.PendingExponent, c.PendingBips)
}

// FillStep advances the newest fillable hole by at most one header batch, through the bulk
// lane. The commit re-checks the generation, so a rewind during the fetch discards the work
// instead of writing it over the canonical chain.
func (f *Follower) FillStep(ctx context.Context) (FillStatus, error) {
	status, err := f.fillStep(ctx)
	if errors.Is(err, errStaleGeneration) {
		// The headers describe the old fork, so nothing is written.
		f.log.Warn("gap fill step discarded after a rewind", "err", err.Error())
		return FillIdle, nil
	}
	return status, err
}

func (f *Follower) fillStep(ctx context.Context) (FillStatus, error) {
	if err := f.ensureInit(ctx); err != nil {
		return FillNone, err
	}
	// Captured before any network call and compared inside the commit transaction.
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
		// Reclassify straight away so it stops counting as work that will never be done. It is
		// still examined once per pass and loses the mark if a state appears before it.
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

// pickHole chooses the newest hole whose predecessor state is known, from the hole's own
// replay checkpoint or the stored block there. Holes marked unfillable are examined only
// when nothing else is, and lose the mark once a state appears. Costs database reads only.
// The second return carries the ranges to mark unfillable, keyed by start.
func (f *Follower) pickHole(ctx context.Context, holes []hole) (*fillTarget, map[uint64]string, error) {
	order := make([]int, 0, len(holes))
	for i := range holes {
		order = append(order, i)
	}
	// A hole already being filled comes first, so a network that keeps skipping finishes one
	// instead of starting every new hole; then newest first.
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
			explained, err := f.endsInKnownShape(ctx, h, target.state)
			if err != nil {
				return nil, reasons, err
			}
			if !explained {
				f.log.Warn("the block after a queued range carries a pricer shape no recorded set explains, leaving the range for the set that does",
					"from", h.From, "to", h.To)
				if reasons == nil {
					reasons = map[uint64]string{}
				}
				reasons[h.From] = reasonReplayDiscontinuity
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

// endsInKnownShape reports whether the shape the replay reaches the end of a range with agrees with
// what is stored after it: the state it continues from, with every recorded set through the block
// after the range applied. A change no recorded set explains would otherwise be filled with the
// shape in force before it, pricing every block after it with the wrong model. The range waits
// instead, and fills when the set explaining it is recorded. The sample taken at that block settles
// it exactly, since it carries the targets and windows really in force; without one only the block
// row is left, and it holds backlogs alone, so a change that keeps the constraint count passes
// unseen, as it does wherever else only slots are known.
func (f *Follower) endsInKnownShape(ctx context.Context, h hole, st *pricer.State) (bool, error) {
	next, err := f.store.BlockByNumber(ctx, f.chainID, h.To+1)
	if err != nil {
		return false, fmt.Errorf("block %d: %w", h.To+1, err)
	}
	if next == nil || !next.Known() || len(next.Backlogs) == 0 {
		return true, nil
	}
	sample, err := f.store.StateSampleAt(ctx, f.chainID, h.To+1)
	if err != nil {
		return false, fmt.Errorf("state sample at %d: %w", h.To+1, err)
	}
	end := st.Clone()
	f.mu.Lock()
	for _, cs := range f.sets {
		if cs.EffectiveBlock < h.Start() || cs.EffectiveBlock > h.To+1 {
			continue
		}
		// An observed set at the block after the range is the shape someone saw there, not a change
		// pinned to it: it is the record of the very change this range cannot place. Only a set from
		// an owner call says the block after the range is where the pricer changed.
		if cs.EffectiveBlock == h.To+1 && cs.Source == model.SourceObserved {
			continue
		}
		entries, decodeErr := setEntries(cs)
		if decodeErr != nil {
			continue
		}
		applyPricingChange(end, pricingChange{set: &setChange{block: cs.EffectiveBlock, entries: entries}})
	}
	f.mu.Unlock()
	if sample != nil && sample.BlockNumber == h.To+1 {
		known, ok := sampledConstraints(sample)
		if ok {
			return sameConstraints(end, known), nil
		}
	}
	return len(end.Backlogs()) == len(next.Backlogs), nil
}

// sampledConstraints reads a stored sample's constraints, false when it recorded none (a legacy
// sample, or one written before the shape was stored).
func sampledConstraints(sample *db.StateSample) ([]model.Constraint, bool) {
	if len(sample.Constraints) == 0 {
		return nil, false
	}
	var out []model.Constraint
	if err := sample.Constraints.Unmarshal(&out); err != nil || len(out) == 0 {
		return nil, false
	}
	return out, true
}

// sameConstraints reports whether a replay state carries exactly the constraints a sample recorded,
// backlogs aside.
func sameConstraints(st *pricer.State, constraints []model.Constraint) bool {
	if len(st.Constraints) != len(constraints) {
		return false
	}
	for i, c := range constraints {
		if st.Constraints[i].Target != c.Target || st.Constraints[i].Window != c.Window {
			return false
		}
	}
	return true
}

// markHoles applies the reasons pickHole decided on, matching entries by start so a range
// recorded meanwhile is left alone.
func (f *Follower) markHoles(ctx context.Context, s db.Store, reasons map[uint64]string) error {
	holes, err := f.loadHoles(ctx, s)
	if err != nil {
		return err
	}
	changed := false
	for i := range holes {
		reason, ok := reasons[holes[i].From]
		// A range already carrying the reason may still be queued: the tick records a discontinuity
		// as pending work, and it is this pass that decides nothing can replay it yet.
		if !ok || (holes[i].Reason == reason && holes[i].Lifecycle == rangeBlocked) {
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

// recordFillFailure moves one range into a durable retry lifecycle, matching the interval
// that still contains the attempted range under the chain lock so a concurrent extension
// cannot resurrect stale cursor state.
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

// targetFor reports whether one hole can be filled now and returns the state to replay
// forward from. The stored block before the range wins when it is still there; the hole's
// carried state is cross-checked against its hash, since a checkpoint at that height with
// another hash came from a fork that is gone. Nil with stranded set means neither exists.
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
			return &fillTarget{h: h, prev: prev.Number, hash: prev.Hash, prevTs: uint64(prev.TS.Unix()), state: st, carry: carriedCarry(carried)}, false, nil
		}
	}
	if carried != nil {
		if st := carriedFillState(carried); st != nil {
			return &fillTarget{h: h, prev: carried.Block, hash: carried.Hash, prevTs: carried.PrevTS, state: st, carry: carriedCarry(carried)}, false, nil
		}
	}
	// A stored predecessor whose state cannot be built yet is a wait: the shape may still be
	// recorded. One that is not there at all never comes back, so the range is reclassified.
	return nil, prev == nil, nil
}

// storedFillState builds the pricer state at the end of a stored block: the shape really in force
// there, the row's end-of-block backlogs and its floor. The shape comes from the recorded constraint
// set or the newest state sample at or before the block, never from the live sample: a legacy chain
// whose parameters changed after the gap would otherwise be replayed with today's. Nil when nothing
// describes the pricer there.
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

// carriedFillState rebuilds the pricer state a hole carries. Nil when it describes no model.
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

// holeStateOf captures the replay state at the end of a batch, so the next one continues
// from it without reading a stored block.
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

// fillBatch fetches, replays and commits one batch of a hole. The batch is sized to the
// budget the other loops leave spare, so the filler never holds the pacer's turnstile
// through a long sleep. Headers that do not link up are retried rather than written.
func (f *Follower) fillBatch(ctx context.Context, gen uint64, t *fillTarget) (FillStatus, error) {
	h := t.h
	from := h.Start()
	if from == h.From {
		f.log.Info("filling a gap", "from", h.From, "to", h.To, "blocks", h.Blocks())
	}
	remaining := h.To - from + 1
	n := min(uint64(f.cfg.HeaderBatchSize), remaining)
	if avail := uint64(max(f.rpc.Available(), 0)) / 2; avail < n {
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
	// The cursor tracks what was actually replayed, so a short answer shortens the step
	// instead of leaving a hole inside the hole.
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
	if lo, hi, bad := unsupportedModel(t.state, headers); bad {
		f.log.Warn("a queued range predates the multi-constraint pricer, recording it as unfillable rather than pricing it with a model the chain did not run",
			"from", lo, "to", hi, "arbosBelow", pricer.FirstConstraintVersion)
		if err := f.store.WithChainTx(ctx, f.chainID, func(s db.Store) error {
			return f.markHoles(ctx, s, map[uint64]string{h.From: reasonUnsupportedModel})
		}); err != nil {
			return FillNone, err
		}
		return FillNone, nil
	}
	if at, ok := spansArbOSUpgrade(headers); ok {
		f.log.Info("the gap replay crossed an ArbOS upgrade, so the buckets over it carry a version range",
			"block", at, "from", headers[0].ArbOSVersion, "to", headers[len(headers)-1].ArbOSVersion)
	}
	last := from+n-1 == h.To
	var tail *db.Block
	if last {
		if tail, err = f.holeTail(ctx, h, headers[len(headers)-1]); err != nil {
			return FillIdle, err
		}
	}
	f.mu.Lock()
	tl := f.timelineLocked(nil)
	f.mu.Unlock()
	actionHeaders := append([]nitro.Header(nil), headers...)
	if tail != nil {
		actionHeaders = append(actionHeaders, headerOf(*tail))
	}
	actions, err := f.resolveActionBlocks(ctx, actionHeaders, tl)
	if err != nil {
		return FillIdle, err
	}
	rows, filledTail, end := f.replayHole(t, headers, tail, tl, actions)
	h.Next, h.State = from+n, end
	h.CursorAt = time.Unix(int64(headers[len(headers)-1].Timestamp), 0).UTC().Format(time.RFC3339)
	h.Lifecycle, h.NextRetryAt, h.LastError = rangePending, "", ""
	if err := f.commitFill(ctx, gen, h, rows, filledTail, h.Next > h.To); err != nil {
		return FillProgressed, err
	}
	f.noteFilledHead(filledTail)
	return FillProgressed, nil
}

// noteFilledHead republishes the replay error when the fill gave the current head its prediction.
// Without it the next snapshot keeps reporting the head's old error until a new block arrives.
func (f *Follower) noteFilledHead(filled *db.Block) {
	if filled == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if filled.Number == f.head {
		f.lastErrBips = db.ReplayErrorBips(*filled)
	}
}

// startAmong reports whether start is one of starts.
func startAmong(start time.Time, starts []time.Time) bool {
	for _, s := range starts {
		if s.Equal(start) {
			return true
		}
	}
	return false
}

// holeTail returns the stored block just after a hole, the one carrying the real sampled
// backlogs the replay ends on. Nil when it is not stored. A parent that is not the last
// header of the range means the two belong to different chains.
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

// replayHole replays one batch forward from the state before the hole, splitting at owner
// actions as the catch-up does, and returns the replay state at the end of the batch. On the
// last batch the stored block after the range is replayed too, anchored to its sampled
// backlogs, and gains the prediction and exponents, so the error of the whole reconstruction
// is recorded against a block whose state was really sampled.
func (f *Follower) replayHole(t *fillTarget, headers []nitro.Header, tail *db.Block, tl *timeline, actions actionBlocks) (rows []db.Block, filled *db.Block, end *model.HoleState) {
	st := t.state
	rows, _ = replayForward(f.chainID, st, t.prevTs, headers, tl, nil, actions)
	carry := shiftPredictions(rows, t.carry)
	lastHeader := headers[len(headers)-1]
	var setID int64
	f.mu.Lock()
	if cs := f.setAt(lastHeader.Number); cs != nil {
		setID = cs.ID
	}
	f.mu.Unlock()
	// Taken before the tail is replayed: the tail belongs to the block after the range.
	end = holeStateOf(st, lastHeader, setID)
	end.PendingFee, end.PendingExponent, end.PendingBips = carry.encode()
	if tail == nil {
		return rows, nil, end
	}
	// The tail is the block after the range, so the group the last gap header computed is exactly its
	// prediction: the reconstruction is scored against a block whose backlogs were really sampled.
	if len(rows) == 0 || !carry.known || len(tail.Backlogs) != len(rows[len(rows)-1].Backlogs) {
		f.log.Warn("the block after the gap carries another pricer shape, leaving its replay error unrecorded",
			"block", tail.Number, "backlogs", len(tail.Backlogs), "replay", len(rows[len(rows)-1].Backlogs))
		return rows, nil, end
	}
	merged := *tail
	merged.PredictedBaseFee = db.NewNullWei(carry.fee)
	merged.ExponentBips, merged.ConstraintBips = carry.exponent, carry.perConstraint
	f.log.Info("gap replay reached the sampled head", "block", merged.Number, "replayErrorBips", db.ReplayErrorBips(merged))
	return rows, &merged, end
}

// headerOf synthesizes a header from a stored block. Poster gas is deliberately nil: a row
// carries the block total, never per-transaction cumulative compute gas, so an owner action
// inside the block would otherwise be split at a boundary in the wrong unit.
func headerOf(block db.Block) nitro.Header {
	return nitro.Header{
		Number: block.Number, Hash: block.Hash, ParentHash: block.ParentHash, Timestamp: uint64(block.TS.Unix()),
		GasUsed: block.GasUsed, BaseFee: block.BaseFee.BigInt(), L1BlockNumber: block.L1Block, TxCount: block.TxCount,
	}
}

// commitFill writes one batch of a hole and its progress in one chain transaction, but only
// when the chain has not been rewound since the headers were fetched. Blocks from the hour of
// the first live block on are rows the buckets are rebuilt from; anything older folds
// additively, so an aged hole cannot wipe the backfill's folds. Blocks below the hole's fold
// watermark are replayed for the state they carry but never folded twice.
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
	// Row-backed hole rows a fold may add to a bucket the store will not rebuild, under the same
	// watermark as the additive rows so a retry cannot count them twice. Never the tail: it was
	// already in its bucket.
	foldable := []db.Block{}
	for _, r := range rows {
		switch {
		case hasBoundary && !r.TS.Before(boundary):
			rowBacked = append(rowBacked, r)
			if r.Number >= h.Folded {
				foldable = append(foldable, r)
			}
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
	setID := func(n uint64) sql.NullInt64 { return setIDs[n] }
	buckets := db.FoldBlocks(additive, setID)
	rowFolds := db.FoldBlocks(foldable, setID)
	err := f.withGeneration(ctx, gen, func(s db.Store) error {
		if stored := append(append([]db.Block{}, rowBacked...), keptBuckets...); len(stored) > 0 {
			if err := s.UpsertBlocks(ctx, stored); err != nil {
				return fmt.Errorf("gap blocks: %w", err)
			}
		}
		toFold := append([]db.Bucket(nil), buckets...)
		if len(rowBacked) > 0 {
			for _, res := range db.ResolutionOrder {
				starts := db.BucketStarts(rowBacked, res)
				if err := s.RebuildBuckets(ctx, f.chainID, res, starts); err != nil {
					return fmt.Errorf("gap buckets: %w", err)
				}
				// A window the store declines has lost rows to prune, and the rows recovered here were
				// never in its bucket. Below the frontier a window is only ever added to, never replaced,
				// so they are folded in as the region below the boundary always has been. Treating the
				// silent decline as success would report the gap filled with the buckets unchanged.
				declined, err := s.BelowFrontier(ctx, f.chainID, starts)
				if err != nil {
					return fmt.Errorf("gap buckets: %w", err)
				}
				// Only into a bucket that is still there. After a rewind discarded the window there is
				// nothing to add to, and a fold would insert a bucket holding the recovered rows alone:
				// pruned prefix and canonical suffix both absent, served as whole. Absent stays absent.
				for _, b := range rowFolds {
					if b.Resolution != res || !startAmong(b.BucketStart, declined) {
						continue
					}
					existing, err := s.Buckets(ctx, f.chainID, res, b.BucketStart, b.BucketStart.Add(db.Resolutions[res]))
					if err != nil {
						return fmt.Errorf("gap buckets: %w", err)
					}
					if len(existing) > 0 {
						toFold = append(toFold, b)
					}
				}
			}
		}
		if err := s.FoldBuckets(ctx, toFold); err != nil {
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

// advanceHole records a hole's progress inside the commit. The entry is matched by start
// because another writer may have appended or merged one meanwhile, so it may have grown.
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

// rewindHoles sends every hole the rewind reached into back to the start of its range: the
// blocks above the ancestor went with the orphaned chain. The fold watermark deliberately
// survives, since additive buckets are not rebuilt from rows and their blocks must not be
// counted twice. It is cleared only when the rewind deleted those buckets too.
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
