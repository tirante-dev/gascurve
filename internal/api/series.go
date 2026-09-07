package api

import (
	"context"
	"encoding/json"
	"math/big"
	"time"

	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/model"
)

// maxBlockPoints is the point count above which the 1h range steps down
// from per-block points to 5 second buckets.
const maxBlockPoints = 2000

const stepDownWidth = 5 * time.Second

// seriesRange describes one supported range and its resolutions.
type seriesRange struct {
	name            string
	duration        time.Duration // 0 = everything
	resolution      string        // bucket resolution, "" = per block
	cache           string
	batchStep       time.Duration // 0 = one point per batch report
	batchResolution string
	l1Step          time.Duration
}

// Range names.
const (
	rangeHour  = "1h"
	rangeDay   = "24h"
	rangeMonth = "30d"
	rangeAll   = "all"
)

var ranges = map[string]seriesRange{
	rangeHour:  {name: rangeHour, duration: time.Hour, resolution: "", cache: cacheHour, batchStep: 0, batchResolution: "batch", l1Step: time.Second},
	rangeDay:   {name: rangeDay, duration: 24 * time.Hour, resolution: db.Resolution1m, cache: cacheDay, batchStep: time.Minute, batchResolution: db.Resolution1m, l1Step: time.Minute},
	rangeMonth: {name: rangeMonth, duration: 30 * 24 * time.Hour, resolution: db.Resolution15m, cache: cacheMonth, batchStep: 15 * time.Minute, batchResolution: db.Resolution15m, l1Step: 15 * time.Minute},
	rangeAll:   {name: rangeAll, duration: 0, resolution: db.Resolution1h, cache: cacheAll, batchStep: time.Hour, batchResolution: db.Resolution1h, l1Step: time.Hour},
}

func parseRange(raw string) (seriesRange, bool) {
	if raw == "" {
		raw = rangeHour
	}
	r, ok := ranges[raw]
	return r, ok
}

// window returns [from, to) for the range ending now.
func (r seriesRange) window(now time.Time) (from, to time.Time) {
	to = now.Add(time.Second)
	if r.duration == 0 {
		return time.Unix(0, 0).UTC(), to
	}
	return now.Add(-r.duration), to
}

// bounds is the window as the response reports it, in unix seconds: the
// requested window for a bounded range; for the all range, which has no
// start of its own, the first indexed point (or the end when there is
// none), so a chart never draws an axis from 1970.
func (r seriesRange) bounds(from, to time.Time, first int64, indexed bool) (start, end int64) {
	if r.duration == 0 {
		if !indexed {
			return to.Unix(), to.Unix()
		}
		return first, to.Unix()
	}
	return from.Unix(), to.Unix()
}

// buildSeries assembles a Series for a range.
func (s *Server) buildSeries(ctx context.Context, chainID uint64, rng seriesRange) (*model.Series, error) {
	now := s.now()
	from, to := rng.window(now)
	sets, err := s.store.ConstraintSets(ctx, chainID)
	if err != nil {
		return nil, err
	}
	actions, err := s.store.OwnerActions(ctx, chainID, from, to, 0)
	if err != nil {
		return nil, err
	}
	out := &model.Series{
		Range: rng.name, Resolution: rng.resolution,
		ConstraintSets: []model.ConstraintSet{}, OwnerActions: []model.OwnerAction{}, Points: []model.SeriesPoint{},
	}
	for _, a := range actions {
		out.OwnerActions = append(out.OwnerActions, ownerActionModel(a))
	}
	inForce := setsInForce(sets, from)
	for _, cs := range inForce {
		m, err := constraintSetModel(cs)
		if err != nil {
			return nil, err
		}
		out.ConstraintSets = append(out.ConstraintSets, m)
	}
	if rng.resolution == "" {
		blocks, err := s.store.BlocksBetween(ctx, chainID, from, to)
		if err != nil {
			return nil, err
		}
		if len(blocks) > maxBlockPoints {
			out.Resolution = "5s"
			out.Points = stepDown(blocks, sets, stepDownWidth, now)
		} else {
			out.Resolution = "block"
			out.Points = blockPoints(blocks, sets)
		}
		out.From, out.To = rng.bounds(from, to, firstPoint(out.Points), len(out.Points) > 0)
		return out, nil
	}
	buckets, err := s.store.Buckets(ctx, chainID, rng.resolution, from, to)
	if err != nil {
		return nil, err
	}
	liveStart, err := s.liveStart(ctx, chainID)
	if err != nil {
		return nil, err
	}
	width := db.Resolutions[rng.resolution]
	for _, b := range buckets {
		out.Points = append(out.Points, bucketPoint(b, width, now, liveStart))
	}
	out.From, out.To = rng.bounds(from, to, firstPoint(out.Points), len(out.Points) > 0)
	return out, nil
}

func firstPoint(points []model.SeriesPoint) int64 {
	if len(points) == 0 {
		return 0
	}
	return points[0].T
}

// setsInForce returns the sets active during a window starting at from:
// the latest set effective before from plus every later one.
func setsInForce(sets []db.ConstraintSet, from time.Time) []db.ConstraintSet {
	var out []db.ConstraintSet
	var before *db.ConstraintSet
	for i := range sets {
		cs := &sets[i]
		if cs.EffectiveAt.Before(from) {
			before = cs
			continue
		}
		if before != nil && len(out) == 0 {
			out = append(out, *before)
		}
		out = append(out, *cs)
	}
	if len(out) == 0 && before != nil {
		out = append(out, *before)
	}
	return out
}

// setIDAt returns the set in force at a block among those whose constraint
// count matches the block's backlogs (its live model); a block is never
// tagged with a set of another shape. 0 when none.
func setIDAt(sets []db.ConstraintSet, number uint64, backlogs int) int64 {
	var id int64
	for _, cs := range sets {
		if cs.EffectiveBlock <= number && setSize(cs) == backlogs {
			id = cs.ID
		}
	}
	return id
}

// setSize is the number of constraints in a set document, -1 when it
// cannot be read (which matches nothing).
func setSize(cs db.ConstraintSet) int {
	var entries []json.RawMessage
	if err := cs.Constraints.Unmarshal(&entries); err != nil {
		return -1
	}
	return len(entries)
}

// bucketPoint renders a bucket. The average comes from the exact sum when
// the bucket carries one and from the stored average otherwise; the fee
// split and the exponents are null when the bucket predates them.
// liveStart is when the collector's live loop started storing this chain,
// from the live_start checkpoint; the zero time when there is none.
func (s *Server) liveStart(ctx context.Context, chainID uint64) (time.Time, error) {
	raw, ok, err := s.store.GetState(ctx, chainID, db.StateLiveStart)
	if err != nil {
		return time.Time{}, err
	}
	if !ok {
		return time.Time{}, nil
	}
	var v struct {
		TS int64 `json:"ts"`
	}
	// An unreadable checkpoint reads as no known start, not as a failed request.
	_ = json.Unmarshal([]byte(raw), &v)
	if v.TS <= 0 {
		return time.Time{}, nil
	}
	return time.Unix(v.TS, 0).UTC(), nil
}

// coverage is the part of a bucket the collector has indexed: what lies
// after the live start and before now. Rates are taken over that span, so
// the bucket in progress, or the first one after the collector started,
// reads as the rate the chain ran at rather than as a fraction of a full
// bucket, and the share is reported so a chart can mark the bucket as
// partial. The span is never below one second nor above the width.
func coverage(start time.Time, width time.Duration, now, liveStart time.Time) (secs uint64, share float64) {
	covStart, covEnd := start, start.Add(width)
	if liveStart.After(covStart) {
		covStart = liveStart
	}
	if now.Before(covEnd) {
		covEnd = now
	}
	covered := min(max(covEnd.Sub(covStart), time.Second), width)
	return max(uint64(covered/time.Second), 1), float64(covered) / float64(width)
}

func bucketPoint(b db.Bucket, width time.Duration, now, liveStart time.Time) model.SeriesPoint {
	secs, share := coverage(b.BucketStart, width, now, liveStart)
	var setID int64
	if b.ConstraintSetID.Valid {
		setID = b.ConstraintSetID.Int64
	}
	avg := b.BaseFeeAvg.String()
	if b.BaseFeeSum.Valid && b.Blocks > 0 {
		avg = new(big.Int).Div(b.BaseFeeSum.Wei.BigInt(), big.NewInt(b.Blocks)).String()
	}
	return model.SeriesPoint{
		T: b.BucketStart.Unix(), Blocks: b.Blocks, GasUsed: b.GasUsed, GasPerSecond: b.GasUsed / secs, Coverage: share,
		FeesWei: b.FeesWei.String(), BaseFeeMin: b.BaseFeeMin.String(), BaseFeeAvg: avg, BaseFeeMax: b.BaseFeeMax.String(),
		ExponentBips: b.ExponentEndBips, ConstraintBips: int64s(b.ConstraintBipsEnd), Backlogs: b.BacklogsEnd.Uint64s(), BacklogsMax: b.BacklogsMax.Uint64s(),
		MinBaseFee: b.MinBaseFee.StringPtr(), FloorFeesWei: b.FloorFeesWei.StringPtr(), SurplusFeesWei: b.SurplusFeesWei.StringPtr(),
		ConstraintSetID: setID, ReplayErrorBips: b.ReplayErrorBips,
	}
}

// blockPoints renders one point per block. gasPerSecond is the total gas of
// all blocks sharing the block's timestamp second. A block stored before
// its exponents and floor were recorded (pricing version 0) has no known
// floor and therefore no known fee split.
func blockPoints(blocks []db.Block, sets []db.ConstraintSet) []model.SeriesPoint {
	perSecond := map[int64]uint64{}
	for _, b := range blocks {
		perSecond[b.TS.Unix()] += b.GasUsed
	}
	out := make([]model.SeriesPoint, 0, len(blocks))
	for _, b := range blocks {
		gas := new(big.Int).SetUint64(b.GasUsed)
		fees := new(big.Int).Mul(b.BaseFee.BigInt(), gas)
		p := model.SeriesPoint{
			T: b.TS.Unix(), Blocks: 1, GasUsed: b.GasUsed, GasPerSecond: perSecond[b.TS.Unix()], Coverage: 1,
			FeesWei: fees.String(), BaseFeeMin: b.BaseFee.String(), BaseFeeAvg: b.BaseFee.String(), BaseFeeMax: b.BaseFee.String(),
			ExponentBips: b.ExponentBips, ConstraintBips: int64s(b.ConstraintBips), Backlogs: b.Backlogs.Uint64s(), BacklogsMax: b.Backlogs.Uint64s(),
			MinBaseFee: b.MinBaseFee.StringPtr(), ConstraintSetID: setIDAt(sets, b.Number, len(b.Backlogs)), ReplayErrorBips: replayError(b),
		}
		if b.Known() {
			floor := new(big.Int).Mul(b.MinBaseFee.Wei.BigInt(), gas)
			p.FloorFeesWei, p.SurplusFeesWei = stringPtr(floor), stringPtr(new(big.Int).Sub(fees, floor))
		}
		out = append(out, p)
	}
	return out
}

func stringPtr(v *big.Int) *string {
	s := v.String()
	return &s
}

// stepDown folds blocks into fixed width buckets.
func stepDown(blocks []db.Block, sets []db.ConstraintSet, width time.Duration, now time.Time) []model.SeriesPoint {
	secs := int64(width / time.Second)
	var out []model.SeriesPoint
	var cur *acc
	for _, b := range blocks {
		start := (b.TS.Unix() / secs) * secs
		if cur == nil || cur.start != start {
			if cur != nil {
				out = append(out, cur.point(width, now))
			}
			cur = &acc{start: start, sum: new(big.Int), fees: new(big.Int), floor: new(big.Int)}
		}
		cur.add(b, setIDAt(sets, b.Number, len(b.Backlogs)))
	}
	if cur != nil {
		out = append(out, cur.point(width, now))
	}
	return out
}

type acc struct {
	start          int64
	blocks         int64
	gas            uint64
	sum, fees      *big.Int
	floor          *big.Int
	unknownFloor   bool // a block without a recorded floor makes the split unknown
	minFee         *big.Int
	maxFee         *big.Int
	exponent       int64
	constraintBips []int64
	backlogs       []uint64
	maxBacklog     []uint64
	minBaseFee     *string
	setID          int64
	errBips        int64
}

func (a *acc) add(b db.Block, setID int64) {
	fee := b.BaseFee.BigInt()
	if a.blocks == 0 || fee.Cmp(a.minFee) < 0 {
		a.minFee = fee
	}
	if a.maxFee == nil || fee.Cmp(a.maxFee) > 0 {
		a.maxFee = fee
	}
	a.blocks++
	a.gas += b.GasUsed
	a.sum.Add(a.sum, fee)
	gas := new(big.Int).SetUint64(b.GasUsed)
	a.fees.Add(a.fees, new(big.Int).Mul(fee, gas))
	a.floor.Add(a.floor, new(big.Int).Mul(b.MinBaseFee.Wei.BigInt(), gas))
	a.unknownFloor = a.unknownFloor || !b.Known()
	a.exponent = b.ExponentBips
	a.constraintBips = int64s(b.ConstraintBips)
	a.minBaseFee = b.MinBaseFee.StringPtr()
	a.backlogs = b.Backlogs.Uint64s()
	for i, v := range a.backlogs {
		if i >= len(a.maxBacklog) {
			a.maxBacklog = append(a.maxBacklog, v)
		} else if v > a.maxBacklog[i] {
			a.maxBacklog[i] = v
		}
	}
	a.setID = setID
	a.errBips = max(a.errBips, replayError(b))
}

// point renders the step; the rate covers the part of the step before now
// (the blocks are the whole coverage, so no live start applies here).
func (a *acc) point(width time.Duration, now time.Time) model.SeriesPoint {
	secs, share := coverage(time.Unix(a.start, 0), width, now, time.Time{})
	avg := new(big.Int).Div(a.sum, big.NewInt(a.blocks))
	if a.maxBacklog == nil {
		a.maxBacklog = []uint64{}
	}
	if a.backlogs == nil {
		a.backlogs = []uint64{}
	}
	if a.floor == nil {
		a.floor = new(big.Int)
	}
	p := model.SeriesPoint{
		T: a.start, Blocks: a.blocks, GasUsed: a.gas, GasPerSecond: a.gas / secs, Coverage: share,
		FeesWei: a.fees.String(), BaseFeeMin: a.minFee.String(), BaseFeeAvg: avg.String(), BaseFeeMax: a.maxFee.String(),
		ExponentBips: a.exponent, ConstraintBips: a.constraintBips, Backlogs: a.backlogs, BacklogsMax: a.maxBacklog,
		MinBaseFee: a.minBaseFee, ConstraintSetID: a.setID, ReplayErrorBips: a.errBips,
	}
	if a.unknownFloor {
		// One block of history without a recorded floor makes the whole
		// step's floor, split and exponents unknown.
		p.MinBaseFee, p.ConstraintBips = nil, nil
	} else {
		p.FloorFeesWei, p.SurplusFeesWei = stringPtr(a.floor), stringPtr(new(big.Int).Sub(a.fees, a.floor))
	}
	return p
}
