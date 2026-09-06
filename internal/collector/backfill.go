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
// the segment end is older than backfill_depth.
type backfillCursor struct {
	Done       bool     `json:"done"`
	DepthStart uint64   `json:"depthStart"`
	Active     bool     `json:"active"`
	SegStart   uint64   `json:"segStart"`
	Next       uint64   `json:"next"`
	End        uint64   `json:"end"`
	PrevTs     uint64   `json:"prevTs"`
	Backlogs   []uint64 `json:"backlogs"`
	SetID      int64    `json:"setId"`
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
// spends RPC budget the fast loop is not using.
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
	rows := f.replaySegment(st, c, headers)
	f.mu.Lock()
	buckets := foldBuckets(rows, func(uint64) sql.NullInt64 { return setID })
	f.mu.Unlock()

	c.Next += n
	c.PrevTs = headers[len(headers)-1].Timestamp
	c.Backlogs = st.Backlogs()
	if c.Next >= c.End {
		c.Active = false
		c.End = c.SegStart
	}
	err = f.store.WithTx(ctx, func(s db.Store) error {
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

// replaySegment replays headers, splitting at minimum base fee changes.
func (f *Follower) replaySegment(st *pricer.State, c *backfillCursor, headers []nitro.Header) []db.Block {
	f.mu.Lock()
	var live *big.Int
	if f.lastSample != nil {
		live = f.lastSample.MinBaseFee
	}
	f.mu.Unlock()
	rows := make([]db.Block, 0, len(headers))
	prevTs := c.PrevTs
	start := 0
	for start < len(headers) {
		f.mu.Lock()
		st.MinBaseFee = f.minFeeAt(headers[start].Number, live)
		f.mu.Unlock()
		end := start + 1
		for end < len(headers) {
			f.mu.Lock()
			fee := f.minFeeAt(headers[end].Number, live)
			f.mu.Unlock()
			if fee.Cmp(st.MinBaseFee) != 0 {
				break
			}
			end++
		}
		chunk := headers[start:end]
		blocks := make([]pricer.Block, len(chunk))
		for i, h := range chunk {
			blocks[i] = pricer.Block{Number: h.Number, Timestamp: h.Timestamp, GasUsed: h.GasUsed, BaseFee: h.BaseFee}
		}
		results := pricer.Replay(st, prevTs, blocks, nil)
		rows = append(rows, blockRows(f.chainID, chunk, results)...)
		prevTs = chunk[len(chunk)-1].Timestamp
		start = end
	}
	return rows
}

// startSegment picks the next segment to replay, walking backwards through
// constraint sets. Returns BackfillDone when the depth is reached and
// BackfillIdle when nothing can be decided yet.
func (f *Follower) startSegment(ctx context.Context, c *backfillCursor) (BackfillStatus, error) {
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
		// No constraint set is known before this point: replay the live
		// model from the depth boundary with empty backlogs.
		c.SegStart = c.DepthStart
		c.SetID = 0
		c.Backlogs = make([]uint64, len(sampleBacklogs(sample)))
	default:
		return BackfillIdle, nil
	}
	c.Active = true
	c.Next = c.SegStart
	c.PrevTs = 0
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
