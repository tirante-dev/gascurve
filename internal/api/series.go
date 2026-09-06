package api

import (
	"context"
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
	batchStep       time.Duration
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
	rangeHour:  {name: rangeHour, duration: time.Hour, resolution: "", cache: cacheHour, batchStep: time.Second, batchResolution: "batch", l1Step: time.Second},
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
			out.Points = stepDown(blocks, sets, stepDownWidth)
		} else {
			out.Resolution = "block"
			out.Points = blockPoints(blocks, sets)
		}
		return out, nil
	}
	buckets, err := s.store.Buckets(ctx, chainID, rng.resolution, from, to)
	if err != nil {
		return nil, err
	}
	width := db.Resolutions[rng.resolution]
	for _, b := range buckets {
		out.Points = append(out.Points, bucketPoint(b, width))
	}
	return out, nil
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

func setIDAt(sets []db.ConstraintSet, number uint64) int64 {
	var id int64
	for _, cs := range sets {
		if cs.EffectiveBlock <= number {
			id = cs.ID
		}
	}
	return id
}

func bucketPoint(b db.Bucket, width time.Duration) model.SeriesPoint {
	secs := max(uint64(width/time.Second), 1)
	var setID int64
	if b.ConstraintSetID.Valid {
		setID = b.ConstraintSetID.Int64
	}
	return model.SeriesPoint{
		T: b.BucketStart.Unix(), Blocks: b.Blocks, GasUsed: b.GasUsed, GasPerSecond: b.GasUsed / secs,
		FeesWei: b.FeesWei.String(), BaseFeeMin: b.BaseFeeMin.String(), BaseFeeAvg: b.BaseFeeAvg.String(), BaseFeeMax: b.BaseFeeMax.String(),
		ExponentBips: b.ExponentEndBips, Backlogs: db.Uint64s(b.BacklogsEnd), BacklogsMax: db.Uint64s(b.BacklogsMax),
		ConstraintSetID: setID, ReplayErrorBips: b.ReplayErrorBips,
	}
}

// blockPoints renders one point per block. gasPerSecond is the total gas of
// all blocks sharing the block's timestamp second.
func blockPoints(blocks []db.Block, sets []db.ConstraintSet) []model.SeriesPoint {
	perSecond := map[int64]uint64{}
	for _, b := range blocks {
		perSecond[b.TS.Unix()] += b.GasUsed
	}
	out := make([]model.SeriesPoint, 0, len(blocks))
	for _, b := range blocks {
		fees := new(big.Int).Mul(b.BaseFee.BigInt(), new(big.Int).SetUint64(b.GasUsed))
		out = append(out, model.SeriesPoint{
			T: b.TS.Unix(), Blocks: 1, GasUsed: b.GasUsed, GasPerSecond: perSecond[b.TS.Unix()],
			FeesWei: fees.String(), BaseFeeMin: b.BaseFee.String(), BaseFeeAvg: b.BaseFee.String(), BaseFeeMax: b.BaseFee.String(),
			ExponentBips: b.ExponentBips, Backlogs: db.Uint64s(b.Backlogs), BacklogsMax: db.Uint64s(b.Backlogs),
			ConstraintSetID: setIDAt(sets, b.Number), ReplayErrorBips: replayError(b),
		})
	}
	return out
}

// stepDown folds blocks into fixed width buckets.
func stepDown(blocks []db.Block, sets []db.ConstraintSet, width time.Duration) []model.SeriesPoint {
	secs := int64(width / time.Second)
	var out []model.SeriesPoint
	var cur *acc
	for _, b := range blocks {
		start := (b.TS.Unix() / secs) * secs
		if cur == nil || cur.start != start {
			if cur != nil {
				out = append(out, cur.point(secs))
			}
			cur = &acc{start: start, sum: new(big.Int), fees: new(big.Int)}
		}
		cur.add(b, setIDAt(sets, b.Number))
	}
	if cur != nil {
		out = append(out, cur.point(secs))
	}
	return out
}

type acc struct {
	start      int64
	blocks     int64
	gas        uint64
	sum, fees  *big.Int
	minFee     *big.Int
	maxFee     *big.Int
	exponent   int64
	backlogs   []uint64
	maxBacklog []uint64
	setID      int64
	errBips    int64
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
	a.fees.Add(a.fees, new(big.Int).Mul(fee, new(big.Int).SetUint64(b.GasUsed)))
	a.exponent = b.ExponentBips
	a.backlogs = db.Uint64s(b.Backlogs)
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

func (a *acc) point(secs int64) model.SeriesPoint {
	avg := new(big.Int).Div(a.sum, big.NewInt(a.blocks))
	if a.maxBacklog == nil {
		a.maxBacklog = []uint64{}
	}
	return model.SeriesPoint{
		T: a.start, Blocks: a.blocks, GasUsed: a.gas, GasPerSecond: a.gas / uint64(secs),
		FeesWei: a.fees.String(), BaseFeeMin: a.minFee.String(), BaseFeeAvg: avg.String(), BaseFeeMax: a.maxFee.String(),
		ExponentBips: a.exponent, Backlogs: a.backlogs, BacklogsMax: a.maxBacklog,
		ConstraintSetID: a.setID, ReplayErrorBips: a.errBips,
	}
}
