package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/big"
	"time"

	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/model"
	"github.com/tirante-dev/gascurve/internal/nitro"
	"github.com/tirante-dev/gascurve/internal/pricer"
)

// BackfillStatus is the outcome of one backfill step.
type BackfillStatus int

// Backfill step outcomes.
const (
	BackfillProgressed BackfillStatus = iota
	BackfillIdle
	BackfillDone
)

// backfillCursor is the resumable checkpoint stored in collector_state.
// Segments are bounded by constraint sets and replayed forward from the
// set's starting backlogs; the job walks segments backwards in time until
// the segment end is older than backfill_depth. On archive networks
// LastAnchor, LastAnchorErrorBips and AnchorMinFee record the most recent
// state anchor, the replay error observed just before it and the minimum
// base fee sampled there. Verified marks a segment chosen after the
// initial owner-action scan completed; an unverified live-model segment
// left by an older collector is discarded at start.
type backfillCursor struct {
	Done                bool     `json:"done"`
	DepthStart          uint64   `json:"depthStart"`
	Active              bool     `json:"active"`
	Verified            bool     `json:"verified,omitempty"`
	SegStart            uint64   `json:"segStart"`
	Next                uint64   `json:"next"`
	End                 uint64   `json:"end"`
	PrevTs              uint64   `json:"prevTs"`
	Backlogs            []uint64 `json:"backlogs"`
	SetID               int64    `json:"setId"`
	LastAnchor          uint64   `json:"lastAnchor,omitempty"`
	LastAnchorErrorBips int64    `json:"lastAnchorErrorBips,omitempty"`
	AnchorMinFee        string   `json:"anchorMinFee,omitempty"`
}

func (f *Follower) loadCursor(ctx context.Context) (*backfillCursor, error) {
	raw, ok, err := f.store.GetState(ctx, f.chainID, db.StateBackfillCursor)
	if err != nil {
		return nil, err
	}
	c := &backfillCursor{}
	if ok {
		if err := json.Unmarshal([]byte(raw), c); err != nil {
			return nil, fmt.Errorf("backfill cursor: %w", err)
		}
	}
	return c, nil
}

func (f *Follower) saveCursor(ctx context.Context, s db.Store, c *backfillCursor) error {
	b, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode cursor: %w", err)
	}
	return s.SetState(ctx, f.chainID, db.StateBackfillCursor, string(b))
}

// BackfillStep advances the backfill by at most one header batch. It only
// spends RPC budget the fast loop is not using. On archive networks the
// replay is pinned to the real state every backfill_anchor_interval
// blocks; otherwise it is a pure replay from the segment's starting
// backlogs. Buckets in the hour of the first live block are rebuilt from
// block rows (the live loop shares them); older buckets are folded
// additively, committed together with the cursor so every block counts
// exactly once.
func (f *Follower) BackfillStep(ctx context.Context) (BackfillStatus, error) {
	if err := f.ensureInit(ctx); err != nil {
		return BackfillIdle, err
	}
	c, err := f.loadCursor(ctx)
	if err != nil {
		return BackfillIdle, err
	}
	if c.Done {
		return BackfillDone, nil
	}
	if c, err = f.checkCursor(ctx, c); err != nil {
		return BackfillIdle, err
	}
	if !c.Active {
		status, err := f.startSegment(ctx, c)
		if err != nil || status != BackfillProgressed {
			return status, err
		}
	}
	if f.catchingUp.Load() {
		return BackfillIdle, nil
	}
	remaining := c.End - c.Next
	n := min(uint64(f.cfg.HeaderBatchSize), remaining)
	if avail := uint64(max(f.rpc.Available(), 0)); avail < n {
		n = avail
	}
	if n < min(minBackfillBatch, remaining) {
		return BackfillIdle, nil
	}
	numbers := make([]uint64, 0, n)
	for i := uint64(0); i < n; i++ {
		numbers = append(numbers, c.Next+i)
	}
	headers, err := f.rpc.HeadersByNumbers(ctx, numbers)
	if err != nil {
		return BackfillIdle, fmt.Errorf("backfill headers %d..%d: %w", c.Next, c.Next+n-1, err)
	}
	st, setID, err := f.segmentState(ctx, c)
	if err != nil {
		return BackfillIdle, err
	}
	anchor, anchorFees, err := f.backfillAnchors(ctx, st, numbers)
	if err != nil {
		return BackfillIdle, err
	}
	rows := f.replaySegment(st, c, headers, anchor, anchorFees)
	for _, r := range rows {
		if r.Anchored {
			c.LastAnchor, c.LastAnchorErrorBips = r.Number, db.ReplayErrorBips(r)
			if fee := anchorFees[r.Number]; fee != nil {
				c.AnchorMinFee = fee.String()
			}
			f.log.Info("backfill anchored to archive state", "block", r.Number, "replayErrorBips", c.LastAnchorErrorBips)
		}
	}
	f.mu.Lock()
	boundary, hasBoundary := f.boundaryLocked()
	f.mu.Unlock()
	var rowBacked, additive []db.Block
	for _, r := range rows {
		if hasBoundary && !r.TS.Before(boundary) {
			rowBacked = append(rowBacked, r)
		} else {
			additive = append(additive, r)
		}
	}
	buckets := db.FoldBlocks(additive, func(uint64) sql.NullInt64 { return setID })

	c.Next += n
	c.PrevTs = headers[len(headers)-1].Timestamp
	c.Backlogs = st.Backlogs()
	if c.Next >= c.End {
		c.Active = false
		c.End = c.SegStart
	}
	err = f.store.WithTx(ctx, func(s db.Store) error {
		if len(rowBacked) > 0 {
			if err := s.UpsertBlocks(ctx, rowBacked); err != nil {
				return fmt.Errorf("backfill blocks: %w", err)
			}
			for _, res := range db.ResolutionOrder {
				if err := s.RebuildBuckets(ctx, f.chainID, res, db.BucketStarts(rowBacked, res)); err != nil {
					return fmt.Errorf("backfill buckets: %w", err)
				}
			}
		}
		if err := s.FoldBuckets(ctx, buckets); err != nil {
			return fmt.Errorf("backfill buckets: %w", err)
		}
		return f.saveCursor(ctx, s, c)
	})
	if err != nil {
		return BackfillIdle, err
	}
	return BackfillProgressed, nil
}

// checkCursor runs once per process: an active live-model segment (SetID
// 0) that was not verified against a completed owner-action scan came from
// an older collector and may carry the wrong constraints. Its buckets
// (every backfill-only bucket) are deleted and the backfill starts over.
func (f *Follower) checkCursor(ctx context.Context, c *backfillCursor) (*backfillCursor, error) {
	f.mu.Lock()
	checked := f.cursorChecked
	f.cursorChecked = true
	boundary, hasBoundary := f.boundaryLocked()
	f.mu.Unlock()
	if checked || !c.Active || c.SetID != 0 || c.Verified {
		return c, nil
	}
	f.log.Warn("discarding an unverified live-model backfill segment, its buckets are re-backfilled", "from", c.SegStart, "next", c.Next)
	fresh := &backfillCursor{}
	err := f.store.WithTx(ctx, func(s db.Store) error {
		if hasBoundary {
			if _, err := s.DeleteBucketsBefore(ctx, f.chainID, boundary); err != nil {
				return err
			}
		}
		return f.saveCursor(ctx, s, fresh)
	})
	if err != nil {
		return nil, fmt.Errorf("discard backfill cursor: %w", err)
	}
	return fresh, nil
}

// backfillAnchors reads the real pricer state at every anchor block among
// numbers on an archive network and returns the Anchor for the replay
// plus the minimum base fee sampled at each anchor. The anchor is nil
// without archive (pure replay) and when no anchor block is in range. An
// anchor whose model differs from the segment's constraint set is skipped
// with a warning: the replay state cannot change shape mid segment.
func (f *Follower) backfillAnchors(ctx context.Context, st *pricer.State, numbers []uint64) (pricer.Anchor, map[uint64]*big.Int, error) {
	if !f.net.Archive {
		return nil, nil, nil
	}
	interval := uint64(f.cfg.BackfillAnchorInterval)
	backlogs := map[uint64][]uint64{}
	fees := map[uint64]*big.Int{}
	for _, n := range numbers {
		if n%interval != 0 {
			continue
		}
		sample, err := f.rpc.FastSampleAt(ctx, n)
		if err != nil {
			return nil, nil, fmt.Errorf("backfill anchor %d: %w", n, err)
		}
		if !sameShape(st, sample) {
			f.log.Warn("backfill anchor skipped, sampled model differs from the segment's set", "block", n)
			continue
		}
		backlogs[n] = sampleBacklogs(sample)
		if sample.MinBaseFee != nil {
			fees[n] = new(big.Int).Set(sample.MinBaseFee)
		}
	}
	if len(backlogs) == 0 {
		return nil, nil, nil
	}
	return func(n uint64) ([]uint64, bool) {
		b, ok := backlogs[n]
		return b, ok
	}, fees, nil
}

// backfillFees returns the minimum base fee in force at every header: the
// recorded owner actions (genesis default before the first), overridden
// after an archive anchor by the fee sampled there until the next recorded
// change. anchorFees are the anchors inside this batch; the cursor carries
// the last one from earlier batches.
func (f *Follower) backfillFees(c *backfillCursor, headers []nitro.Header, anchorFees map[uint64]*big.Int) []*big.Int {
	f.mu.Lock()
	defer f.mu.Unlock()
	lastAnchor := c.LastAnchor
	var anchorFee *big.Int
	if c.AnchorMinFee != "" {
		anchorFee, _ = new(big.Int).SetString(c.AnchorMinFee, 10)
	}
	out := make([]*big.Int, len(headers))
	for i, h := range headers {
		fee := f.minFeeAt(h.Number)
		if anchorFee != nil && lastAnchor < h.Number && f.minFeeChangeBlock(h.Number) <= lastAnchor {
			fee = new(big.Int).Set(anchorFee)
		}
		out[i] = fee
		if af, ok := anchorFees[h.Number]; ok {
			lastAnchor, anchorFee = h.Number, af
		}
	}
	return out
}

// replaySegment replays headers, splitting at minimum base fee changes and
// pinning backlogs wherever anchor says so.
func (f *Follower) replaySegment(st *pricer.State, c *backfillCursor, headers []nitro.Header, anchor pricer.Anchor, anchorFees map[uint64]*big.Int) []db.Block {
	fees := f.backfillFees(c, headers, anchorFees)
	rows := make([]db.Block, 0, len(headers))
	prevTs := c.PrevTs
	start := 0
	for start < len(headers) {
		st.MinBaseFee = new(big.Int).Set(fees[start])
		end := start + 1
		for end < len(headers) && fees[end].Cmp(st.MinBaseFee) == 0 {
			end++
		}
		chunk := headers[start:end]
		blocks := make([]pricer.Block, len(chunk))
		for i, h := range chunk {
			blocks[i] = pricer.Block{Number: h.Number, Timestamp: h.Timestamp, GasUsed: h.GasUsed, BaseFee: h.BaseFee}
		}
		results := pricer.Replay(st, prevTs, blocks, anchor)
		fee := st.MinBaseFee
		rows = append(rows, blockRows(f.chainID, chunk, results, func(uint64) *big.Int { return fee })...)
		prevTs = chunk[len(chunk)-1].Timestamp
		start = end
	}
	return rows
}

// startSegment picks the next segment to replay, walking backwards through
// constraint sets. Nothing starts before the initial owner-action scan has
// completed: only then are the historical sets known. Returns
// BackfillDone when the depth is reached and BackfillIdle when nothing
// can be decided yet.
func (f *Follower) startSegment(ctx context.Context, c *backfillCursor) (BackfillStatus, error) {
	f.mu.Lock()
	scanned := f.ownerScanDone
	f.mu.Unlock()
	if !scanned {
		return BackfillIdle, nil
	}
	if c.End == 0 {
		oldest, err := f.store.OldestBlock(ctx, f.chainID)
		if err != nil {
			return BackfillIdle, err
		}
		if oldest == nil {
			return BackfillIdle, nil
		}
		c.End = oldest.Number
	}
	if c.DepthStart == 0 {
		start, err := f.findBlockAt(ctx, f.now().Add(-f.cfg.BackfillDepth))
		if err != nil {
			return BackfillIdle, err
		}
		c.DepthStart = max(start, 1)
	}
	if c.End <= c.DepthStart {
		c.Done = true
		f.log.Info("backfill complete", "depthStart", c.DepthStart)
		return BackfillDone, f.saveCursor(ctx, f.store, c)
	}
	f.mu.Lock()
	var chosen *db.ConstraintSet
	for i := range f.sets {
		if f.sets[i].EffectiveBlock < c.End {
			chosen = &f.sets[i]
		}
	}
	sample := f.lastSample
	f.mu.Unlock()

	switch {
	case chosen != nil:
		entries, err := setEntries(*chosen)
		if err != nil {
			return BackfillIdle, err
		}
		c.SegStart = max(chosen.EffectiveBlock, 1)
		c.SetID = chosen.ID
		c.Backlogs = make([]uint64, len(entries))
		for i, e := range entries {
			c.Backlogs[i] = e.StartingBacklog
		}
	case sample != nil:
		// No constraint set is known before this point (a legacy chain,
		// or a chain whose actions predate the scan window): replay the
		// live model from the depth boundary with empty backlogs.
		c.SegStart = c.DepthStart
		c.SetID = 0
		c.Backlogs = make([]uint64, len(sampleBacklogs(sample)))
	default:
		return BackfillIdle, nil
	}
	c.Active = true
	c.Verified = true
	c.Next = c.SegStart
	c.PrevTs = 0
	c.LastAnchor, c.LastAnchorErrorBips, c.AnchorMinFee = 0, 0, ""
	f.log.Info("backfill segment", "from", c.SegStart, "to", c.End, "setId", c.SetID)
	return BackfillProgressed, f.saveCursor(ctx, f.store, c)
}

// segmentState builds the replay state for the active segment.
func (f *Follower) segmentState(ctx context.Context, c *backfillCursor) (*pricer.State, sql.NullInt64, error) {
	st := &pricer.State{MinBaseFee: new(big.Int)}
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.SetID != 0 {
		var chosen *db.ConstraintSet
		for i := range f.sets {
			if f.sets[i].ID == c.SetID {
				chosen = &f.sets[i]
			}
		}
		if chosen == nil {
			if err := f.reloadSetsLocked(ctx); err != nil {
				return nil, sql.NullInt64{}, err
			}
			for i := range f.sets {
				if f.sets[i].ID == c.SetID {
					chosen = &f.sets[i]
				}
			}
		}
		if chosen == nil {
			return nil, sql.NullInt64{}, fmt.Errorf("backfill: constraint set %d disappeared", c.SetID)
		}
		entries, err := setEntries(*chosen)
		if err != nil {
			return nil, sql.NullInt64{}, err
		}
		st.Constraints = make([]pricer.Constraint, len(entries))
		for i, e := range entries {
			st.Constraints[i] = pricer.Constraint{Target: e.Target, Window: e.Window}
		}
		st.SetBacklogs(c.Backlogs)
		return st, sql.NullInt64{Int64: c.SetID, Valid: true}, nil
	}
	if f.lastSample == nil {
		return nil, sql.NullInt64{}, fmt.Errorf("backfill: no live sample to derive the model from")
	}
	st = stateFromSample(f.lastSample)
	st.SetBacklogs(c.Backlogs)
	return st, sql.NullInt64{}, nil
}

// findBlockAt binary-searches headers for the first block at or after t.
func (f *Follower) findBlockAt(ctx context.Context, t time.Time) (uint64, error) {
	head, err := f.currentHead(ctx)
	if err != nil {
		return 0, err
	}
	target := uint64(max(t.Unix(), 0))
	lo, hi := uint64(1), head
	for lo < hi {
		mid := lo + (hi-lo)/2
		h, err := f.rpc.HeaderByNumber(ctx, mid)
		if err != nil {
			return 0, fmt.Errorf("header %d: %w", mid, err)
		}
		if h.Timestamp < target {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, nil
}

// entriesFromSample renders live constraints as set entries (used by tests
// and the observed-set logic).
func entriesFromSample(s *nitro.Sample) []model.ConstraintSetEntry {
	out := make([]model.ConstraintSetEntry, len(s.Constraints))
	for i, c := range s.Constraints {
		out[i] = model.ConstraintSetEntry{Target: c.Target, Window: c.Window, StartingBacklog: c.Backlog}
	}
	return out
}
