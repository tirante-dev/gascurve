package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"math/big"
	"slices"
	"sort"
	"time"

	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/model"
)

// maxBlockPoints is the point count above which the 1h range steps down
// from per-block points to 5 second buckets.
const maxBlockPoints = 2000

const stepDownWidth = 5 * time.Second

// The resolutions the 1h range serves from block rows rather than from stored buckets.
const (
	resolutionBlock   = "block"
	resolutionStepped = "5s"
)

// seriesRange describes one supported range and its resolutions.
type seriesRange struct {
	name            string
	duration        time.Duration // 0 = everything
	resolution      string        // bucket resolution, "" = per block
	cache           string
	batchStep       time.Duration // 0 = one point per batch report
	batchResolution string
	l1Step          time.Duration
	// spreadUnit is the width the points' compute rate spread is measured over: a second, read from
	// the block rows, or the width of the stored resolution one step finer. Zero where the points are
	// already that fine, which is the per-block resolution: a point with no interior has no spread.
	spreadUnit time.Duration
}

// Range names.
const (
	rangeHour  = "1h"
	rangeDay   = "24h"
	rangeMonth = "30d"
	rangeAll   = "all"
)

var ranges = map[string]seriesRange{
	rangeHour:  {name: rangeHour, duration: time.Hour, resolution: "", cache: cacheHour, batchStep: 0, batchResolution: "batch", l1Step: time.Second, spreadUnit: time.Second},
	rangeDay:   {name: rangeDay, duration: 24 * time.Hour, resolution: db.Resolution1m, cache: cacheHistory, batchStep: time.Minute, batchResolution: db.Resolution1m, l1Step: time.Minute, spreadUnit: time.Second},
	rangeMonth: {name: rangeMonth, duration: 30 * 24 * time.Hour, resolution: db.Resolution15m, cache: cacheHistory, batchStep: 15 * time.Minute, batchResolution: db.Resolution15m, l1Step: 15 * time.Minute, spreadUnit: time.Minute},
	rangeAll:   {name: rangeAll, duration: 0, resolution: db.Resolution1h, cache: cacheHistory, batchStep: time.Hour, batchResolution: db.Resolution1h, l1Step: time.Hour, spreadUnit: 15 * time.Minute},
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

// bounds is the window as the response reports it, in unix seconds. The all range has no start of
// its own, so it reports the first indexed point instead, and a chart never draws an axis from 1970.
func (r seriesRange) bounds(from, to time.Time, first int64, indexed bool) (start, end int64) {
	if r.duration == 0 {
		if !indexed {
			return to.Unix(), to.Unix()
		}
		return first, to.Unix()
	}
	return from.Unix(), to.Unix()
}

// buildSeries assembles a Series inside one repeatable-read transaction: the bucket rows and the
// missing ranges that qualify them must describe the same database moment, because the collector
// rebuilds buckets and removes a completed range in one commit.
func (s *Server) buildSeries(ctx context.Context, chainID uint64, rng seriesRange) (*model.Series, error) {
	var series *model.Series
	err := s.store.WithSnapshotTx(ctx, func(store db.Store) error {
		var err error
		series, err = buildSeriesIn(ctx, store, chainID, rng, s.now())
		return err
	})
	return series, err
}

func buildSeriesIn(ctx context.Context, store db.Store, chainID uint64, rng seriesRange, now time.Time) (*model.Series, error) {
	from, to := rng.window(now)
	sets, err := store.ConstraintSets(ctx, chainID)
	if err != nil {
		return nil, err
	}
	actions, err := store.OwnerActions(ctx, chainID, from, to, 0)
	if err != nil {
		return nil, err
	}
	missing, err := store.MissingRanges(ctx, chainID)
	if err != nil {
		return nil, err
	}
	timeline := newMissingTimeline(missing)
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
		blocks, err := store.BlocksBetween(ctx, chainID, from, to)
		if err != nil {
			return nil, err
		}
		if len(blocks) > maxBlockPoints {
			var numbers []uint64
			out.Resolution = resolutionStepped
			// The steps are folded from the rows in hand, so their spread is folded with them rather
			// than asked of the store a second time.
			out.Points, numbers = stepDown(blocks, sets, now)
			out.SpreadSeconds = spreadSeconds(rng.spreadUnit)
			timeline.apply(out.Points, numbers, stepDownWidth)
		} else {
			out.Resolution = resolutionBlock
			out.Points = blockPoints(blocks, sets)
			timeline.mark(out.Points, blockNumbers(blocks))
		}
		out.From, out.To = rng.bounds(from, to, firstPoint(out.Points), len(out.Points) > 0)
		return out, nil
	}
	buckets, err := store.Buckets(ctx, chainID, rng.resolution, from, to)
	if err != nil {
		return nil, err
	}
	liveStart, err := liveStart(ctx, store, chainID)
	if err != nil {
		return nil, err
	}
	width := db.Resolutions[rng.resolution]
	numbers := make([]uint64, 0, len(buckets))
	for _, b := range buckets {
		if p, ok := bucketPoint(b, width, now, liveStart); ok {
			out.Points = append(out.Points, p)
			numbers = append(numbers, b.LastBlock)
		}
	}
	timeline.apply(out.Points, bucketWatermarks(numbers), width)
	if err := addRateSpread(ctx, store, chainID, out, from, to, width, rng.spreadUnit, now); err != nil {
		return nil, err
	}
	out.From, out.To = rng.bounds(from, to, firstPoint(out.Points), len(out.Points) > 0)
	return out, nil
}

// spreadSeconds is the unit a series reports its spread in, absent when it measures none.
func spreadSeconds(unit time.Duration) *int64 {
	if unit <= 0 {
		return nil
	}
	secs := int64(unit / time.Second)
	return &secs
}

// addRateSpread hangs the load band on the bucketed points: the lowest and highest compute rate any
// unit inside a bucket carried. The window is cut back to the last whole unit, so the unit the
// serving clock is still inside is not read as a quiet one.
func addRateSpread(ctx context.Context, store db.Store, chainID uint64, out *model.Series, from, to time.Time, width, unit time.Duration, now time.Time) error {
	if unit <= 0 || unit >= width {
		return nil
	}
	whole := now.Truncate(unit)
	if whole.Before(to) {
		to = whole
	}
	spreads, err := store.ComputeRateSpread(ctx, chainID, from, to, width, unit)
	if err != nil {
		return err
	}
	out.SpreadSeconds = spreadSeconds(unit)
	byStart := make(map[int64]db.RateSpread, len(spreads))
	for _, s := range spreads {
		byStart[s.Start.Unix()] = s
	}
	for i := range out.Points {
		s, ok := byStart[out.Points[i].T]
		if !ok {
			continue
		}
		out.Points[i].ComputeGasPerSecondMin = uint64ValuePtr(uint64(max(s.MinRate, 0)))
		out.Points[i].ComputeGasPerSecondMax = uint64ValuePtr(uint64(max(s.MaxRate, 0)))
	}
	return nil
}

// bucketWatermarks is the highest block of every bucket, or nothing when one of them was not recorded.
// last_block defaults to zero, so a zero is a row stored before the column was written, or the rarest
// of genuine buckets: one holding genesis alone. The two cannot be told apart, and reading the first
// as the second would place ranges against a watermark that was never written, so neither is placed.
func bucketWatermarks(numbers []uint64) []uint64 {
	if slices.Contains(numbers, 0) {
		return nil
	}
	return numbers
}

// missingInterval is the time envelope between the indexed blocks on either side of a durable missing
// range. It can have zero duration when blocks share a timestamp, but still makes that bucket partial.
type missingInterval struct {
	from time.Time
	to   time.Time
}

// blockSpan is the still-missing block interval of a range the collector could not put a time on.
type blockSpan struct {
	from uint64
	to   uint64
}

// missingTimeline is the durable gap ledger reduced to what qualifies a point. A range the collector
// bounded with the timestamps of the blocks on either side is measured against the clock; one without
// two usable timestamps keeps its block numbers, which rise with time, and is placed among the points
// by number instead. Prehistory no reachable chain state can reconstruct is recorded with no bounds at
// all and sits below everything a window serves, so letting it stand for "somewhere" would leave every
// point of every range unknown for as long as the row exists.
type missingTimeline struct {
	bounded   []missingInterval
	unlocated []blockSpan
}

func newMissingTimeline(ranges []db.MissingRange) missingTimeline {
	var out missingTimeline
	for _, r := range ranges {
		b, active := remainingMissingBounds(r)
		if !active {
			continue
		}
		// Inverted timestamps cannot locate the range against the clock either.
		if b.lowerOK && b.upperOK && !b.upper.Before(b.lower) {
			out.bounded = append(out.bounded, missingInterval{from: b.lower, to: b.upper})
			continue
		}
		out.unlocated = append(out.unlocated, blockSpan{from: b.start, to: r.To})
	}
	out.bounded = mergeMissingIntervals(out.bounded)
	return out
}

// missingBounds is the still-missing suffix of a range: the first block of it and whatever is known
// about the time on either side.
type missingBounds struct {
	start   uint64
	lower   time.Time
	lowerOK bool
	upper   time.Time
	upperOK bool
}

// remainingMissingBounds describes the still-missing suffix of a range. Once Cursor advances, CursorAt
// describes Cursor-1 and supersedes the original predecessor.
func remainingMissingBounds(r db.MissingRange) (missingBounds, bool) {
	start := r.From
	lowerBound := r.PredecessorAt
	if r.Cursor > r.From {
		start = r.Cursor
		lowerBound = r.CursorAt
	}
	if start > r.To {
		return missingBounds{}, false
	}
	return missingBounds{
		start: start, lower: lowerBound.Time, lowerOK: lowerBound.Valid,
		upper: r.SuccessorAt.Time, upperOK: r.SuccessorAt.Valid,
	}, true
}

func mergeMissingIntervals(intervals []missingInterval) []missingInterval {
	sort.Slice(intervals, func(i, j int) bool {
		if intervals[i].from.Equal(intervals[j].from) {
			return intervals[i].to.Before(intervals[j].to)
		}
		return intervals[i].from.Before(intervals[j].from)
	})
	merged := make([]missingInterval, 0, len(intervals))
	for _, interval := range intervals {
		if len(merged) == 0 || interval.from.After(merged[len(merged)-1].to) {
			merged = append(merged, interval)
			continue
		}
		if interval.to.After(merged[len(merged)-1].to) {
			merged[len(merged)-1].to = interval.to
		}
	}
	return merged
}

// apply qualifies aggregate points that span width, numbers holding the highest block each one
// aggregates. Numeric coverage can only be adjusted when the original boundary span was whole and
// every overlapping gap envelope is bounded; otherwise coverage becomes null rather than combining
// spans whose overlap is not measurable.
func (m missingTimeline) apply(points []model.SeriesPoint, numbers []uint64, width time.Duration) {
	m.qualify(points, numbers, width, true)
}

// mark qualifies per-block points, whose window is the block's own second. A block covers itself and
// its rate is the gas of every block in that second, so a neighboring gap only changes completeness.
func (m missingTimeline) mark(points []model.SeriesPoint, numbers []uint64) {
	m.qualify(points, numbers, time.Second, false)
}

func (m missingTimeline) qualify(points []model.SeriesPoint, numbers []uint64, width time.Duration, measure bool) {
	unlocated := m.unlocatedPoints(points, numbers)
	for i := range points {
		point := &points[i]
		start := time.Unix(point.T, 0).UTC()
		end := start.Add(width)
		first := sort.Search(len(m.bounded), func(j int) bool { return !m.bounded[j].to.Before(start) })
		known := false
		missing := time.Duration(0)
		for _, interval := range m.bounded[first:] {
			if !interval.from.Before(end) {
				break
			}
			known = true
			from := maxTime(start, interval.from)
			to := minTime(end, interval.to)
			if to.After(from) {
				missing += to.Sub(from)
			}
		}
		uncertain := unlocated[i]
		if !known && !uncertain {
			continue
		}
		if known {
			point.Completeness = model.SeriesPartial
		} else if point.Completeness == model.SeriesComplete {
			point.Completeness = model.SeriesUnknown
		}
		if !measure {
			continue
		}
		if uncertain || point.Coverage == nil || *point.Coverage < 1 {
			point.Coverage = nil
			continue
		}
		covered := max(width-missing, 0)
		share := float64(covered) / float64(width)
		point.Coverage = &share
		if covered <= 0 {
			point.GasPerSecond = 0
			if point.ComputeGasPerSecond != nil {
				point.ComputeGasPerSecond = uint64ValuePtr(0)
			}
			continue
		}
		seconds := max(uint64(covered/time.Second), 1)
		point.GasPerSecond = point.GasUsed / seconds
		if point.ComputeGasPerSecond != nil && point.PosterGas != nil && *point.PosterGas <= point.GasUsed {
			point.ComputeGasPerSecond = uint64ValuePtr((point.GasUsed - *point.PosterGas) / seconds)
		}
	}
}

// unlocatedPoints marks the points each unlocated range can overlap. numbers is the highest block each
// point indexed, which rises with time. The missing blocks are in no point, so the run spans the two
// watermarks straddling the range: it can start in the point whose watermark sits below it, whose
// window has not ended there, and end in the first whose watermark reaches it. A range above every
// watermark is the collector falling behind inside the last point, not a range clear of the window.
// Points the caller could not put a watermark on are not placeable, and every one of them is then
// uncertain, as they were before ranges were located.
func (m missingTimeline) unlocatedPoints(points []model.SeriesPoint, numbers []uint64) []bool {
	out := make([]bool, len(points))
	if len(m.unlocated) == 0 || len(points) == 0 {
		return out
	}
	if len(numbers) != len(points) || !slices.IsSorted(numbers) {
		for i := range out {
			out[i] = true
		}
		return out
	}
	last := len(points) - 1
	for _, span := range m.unlocated {
		hi := min(sort.Search(len(numbers), func(i int) bool { return numbers[i] >= span.to }), last)
		// A watermark inside the range contradicts the ledger, which says those blocks are absent. The
		// run then holds the point that claims them rather than collapsing to nothing.
		lo := min(max(sort.Search(len(numbers), func(i int) bool { return numbers[i] > span.from })-1, 0), hi)
		// Points sharing a second are one window: a per-block point's rate is the gas of every block in
		// that second, so a sibling of a point the range reaches is missing the same blocks.
		for lo > 0 && points[lo-1].T == points[lo].T {
			lo--
		}
		for hi < last && points[hi+1].T == points[hi].T {
			hi++
		}
		for i := lo; i <= hi; i++ {
			out[i] = true
		}
	}
	return out
}

// blockNumbers is the block number of every per-block point, in the order blockPoints renders them.
func blockNumbers(blocks []db.Block) []uint64 {
	out := make([]uint64, len(blocks))
	for i, b := range blocks {
		out[i] = b.Number
	}
	return out
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
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

// setIDAt returns the set in force at a block among those whose constraint count matches the block's
// backlogs, so a block is never tagged with a set of another shape. 0 when none.
func setIDAt(sets []db.ConstraintSet, number uint64, backlogs int) int64 {
	var id int64
	for _, cs := range sets {
		if cs.EffectiveBlock <= number && setSize(cs) == backlogs {
			id = cs.ID
		}
	}
	return id
}

// setSize is the number of constraints in a set document, -1 when it cannot be read.
func setSize(cs db.ConstraintSet) int {
	var entries []json.RawMessage
	if err := cs.Constraints.Unmarshal(&entries); err != nil {
		return -1
	}
	return len(entries)
}

// bucketPoint renders a bucket. The average comes from the exact sum when the bucket carries one;
// the fee split and exponents are null when the inputs were not recorded. liveStart is when the
// collector's live loop started storing this chain, the zero time when there is none.
func liveStart(ctx context.Context, store db.Store, chainID uint64) (time.Time, error) {
	raw, ok, err := store.GetState(ctx, chainID, db.StateLiveStart)
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

// coveredSpan is the part of a bucket the collector has indexed: what lies before now, and, for the
// bucket the live start falls inside, what lies after that start. Rates are taken over that span, so a
// bucket in progress reads as the rate the chain ran at rather than a fraction of a full bucket. A
// bucket ending at or before the live start keeps a whole span: it came from the backfiller, and
// trimming it to the live start would report a complete hour as a fraction of a second. A zero or
// negative span means the bucket is entirely in the future and is not returned.
func coveredSpan(start time.Time, width time.Duration, now, liveStart time.Time) time.Duration {
	covStart, covEnd := start, start.Add(width)
	if liveStart.After(covStart) && liveStart.Before(covEnd) {
		covStart = liveStart
	}
	if now.Before(covEnd) {
		covEnd = now
	}
	return min(covEnd.Sub(covStart), width)
}

// coverage renders a positive covered span: the divisor a rate is taken over, and the share of the
// bucket it is. The divisor is never zero, so a span shorter than a second still counts as one.
func coverage(covered, width time.Duration) (secs uint64, share float64) {
	return max(uint64(covered/time.Second), 1), float64(covered) / float64(width)
}

// measuredCoverage pairs a known time share with the aggregate state clients use. A share below one
// is partial even when every block so far is present, because the bucket itself is not finished.
func measuredCoverage(share float64) (coverage *float64, completeness string) {
	state := model.SeriesComplete
	if share < 1 {
		state = model.SeriesPartial
	}
	return &share, state
}

// bucketPoint renders a bucket, or reports false for one entirely in the future.
func bucketPoint(b db.Bucket, width time.Duration, now, liveStart time.Time) (model.SeriesPoint, bool) {
	covered := coveredSpan(b.BucketStart, width, now, liveStart)
	if covered <= 0 {
		return model.SeriesPoint{}, false
	}
	secs, share := coverage(covered, width)
	var setID int64
	if b.ConstraintSetID.Valid {
		setID = b.ConstraintSetID.Int64
	}
	pointCoverage, completeness := measuredCoverage(share)
	avg := b.BaseFeeAvg.String()
	if b.BaseFeeSum.Valid && b.Blocks > 0 {
		avg = new(big.Int).Div(b.BaseFeeSum.Wei.BigInt(), big.NewInt(b.Blocks)).String()
	}
	floorFees, surplusFees, posterFees := b.FloorFeesWei.StringPtr(), b.SurplusFeesWei.StringPtr(), b.PosterFeesWei.StringPtr()
	if !b.PosterGas.Valid {
		floorFees, surplusFees, posterFees = nil, nil, nil
	}
	return model.SeriesPoint{
		T: b.BucketStart.Unix(), Blocks: b.Blocks, GasUsed: b.GasUsed, PosterGas: uint64Ptr(b.PosterGas), GasPerSecond: b.GasUsed / secs,
		ComputeGasPerSecond: computeRate(b.GasUsed, b.PosterGas, secs), Coverage: pointCoverage, Completeness: completeness,
		FeesWei: b.FeesWei.String(), BaseFeeMin: b.BaseFeeMin.String(), BaseFeeAvg: avg, BaseFeeMax: b.BaseFeeMax.String(),
		ExponentBips: b.ExponentEndBips, ConstraintBips: int64s(b.ConstraintBipsEnd), Backlogs: b.BacklogsEnd.Uint64s(), BacklogsMax: b.BacklogsMax.Uint64s(),
		MinBaseFee: b.MinBaseFee.StringPtr(), FloorFeesWei: floorFees, SurplusFeesWei: surplusFees, PosterFeesWei: posterFees,
		ConstraintSetID: setID, ReplayErrorBips: b.ReplayErrorBips,
	}, true
}

// blockPoints renders one point per block. gasPerSecond is the total gas of all blocks sharing the
// block's timestamp second; computeGasPerSecond is the receipt-backed pricer input for that second.
// A block stored before its exponents and floor were recorded has no known fee split.
func blockPoints(blocks []db.Block, sets []db.ConstraintSet) []model.SeriesPoint {
	perSecond := map[int64]uint64{}
	computePerSecond := map[int64]uint64{}
	unknownPoster := map[int64]bool{}
	for _, b := range blocks {
		second := b.TS.Unix()
		perSecond[second] += b.GasUsed
		if b.PosterGas.Valid && b.PosterGas.Int64 >= 0 && uint64(b.PosterGas.Int64) <= b.GasUsed {
			computePerSecond[second] += b.GasUsed - uint64(b.PosterGas.Int64)
		} else {
			unknownPoster[second] = true
		}
	}
	out := make([]model.SeriesPoint, 0, len(blocks))
	for _, b := range blocks {
		gas := new(big.Int).SetUint64(b.GasUsed)
		fees := new(big.Int).Mul(b.BaseFee.BigInt(), gas)
		pointCoverage, completeness := measuredCoverage(1)
		var computeGasPerSecond *uint64
		if !unknownPoster[b.TS.Unix()] {
			computeGasPerSecond = uint64ValuePtr(computePerSecond[b.TS.Unix()])
		}
		p := model.SeriesPoint{
			T: b.TS.Unix(), Blocks: 1, GasUsed: b.GasUsed, PosterGas: uint64Ptr(b.PosterGas), GasPerSecond: perSecond[b.TS.Unix()], Coverage: pointCoverage, Completeness: completeness,
			ComputeGasPerSecond: computeGasPerSecond,
			FeesWei:             fees.String(), BaseFeeMin: b.BaseFee.String(), BaseFeeAvg: b.BaseFee.String(), BaseFeeMax: b.BaseFee.String(),
			ExponentBips: b.ExponentBips, ConstraintBips: int64s(b.ConstraintBips), Backlogs: b.Backlogs.Uint64s(), BacklogsMax: b.Backlogs.Uint64s(),
			MinBaseFee: b.MinBaseFee.StringPtr(), ConstraintSetID: setIDAt(sets, b.Number, len(b.Backlogs)), ReplayErrorBips: replayError(b),
		}
		if b.DestinationsKnown() {
			posterGas := new(big.Int).SetInt64(b.PosterGas.Int64)
			compute := new(big.Int).Sub(gas, posterGas)
			rate := new(big.Int).Set(b.MinBaseFee.Wei.BigInt())
			if b.BaseFee.BigInt().Cmp(rate) < 0 {
				rate.Set(b.BaseFee.BigInt())
			}
			floor := new(big.Int).Mul(rate, compute)
			computeFees := new(big.Int).Mul(b.BaseFee.BigInt(), compute)
			posterFees := new(big.Int).Mul(b.BaseFee.BigInt(), posterGas)
			p.FloorFeesWei, p.SurplusFeesWei, p.PosterFeesWei = stringPtr(floor), stringPtr(new(big.Int).Sub(computeFees, floor)), stringPtr(posterFees)
		} else if b.PosterGas.Valid && b.PosterGas.Int64 >= 0 && uint64(b.PosterGas.Int64) <= b.GasUsed {
			posterGas := new(big.Int).SetInt64(b.PosterGas.Int64)
			p.PosterFeesWei = stringPtr(new(big.Int).Mul(b.BaseFee.BigInt(), posterGas))
		}
		out = append(out, p)
	}
	return out
}

func stringPtr(v *big.Int) *string {
	s := v.String()
	return &s
}

func computeRate(gas uint64, poster sql.NullInt64, secs uint64) *uint64 {
	if !poster.Valid || poster.Int64 < 0 || uint64(poster.Int64) > gas || secs == 0 {
		return nil
	}
	return uint64ValuePtr((gas - uint64(poster.Int64)) / secs)
}

func uint64ValuePtr(v uint64) *uint64 { return &v }

// stepDown folds blocks into stepDownWidth buckets, taking each step's rate over the span it covers
// exactly as a stored bucket does. numbers is the highest block of each step, so a missing range the
// collector could not put a time on can still be placed among the steps.
func stepDown(blocks []db.Block, sets []db.ConstraintSet, now time.Time) (points []model.SeriesPoint, numbers []uint64) {
	const width = stepDownWidth
	secs := int64(width / time.Second)
	var cur *acc
	for _, b := range blocks {
		start := (b.TS.Unix() / secs) * secs
		if cur == nil || cur.start != start {
			if cur != nil {
				points, numbers = append(points, cur.point(width, now)), append(numbers, cur.lastBlock)
			}
			cur = &acc{start: start, cutoff: now.Truncate(time.Second).Unix(), sum: new(big.Int), fees: new(big.Int), floor: new(big.Int), posterFees: new(big.Int)}
		}
		cur.add(b, setIDAt(sets, b.Number, len(b.Backlogs)))
	}
	if cur != nil {
		points, numbers = append(points, cur.point(width, now)), append(numbers, cur.lastBlock)
	}
	return points, numbers
}

type acc struct {
	start int64
	// last is the newest block second the step carries: a step whose blocks reach or pass the serving
	// clock is covered to the end of that second rather than truncated to nothing.
	last int64
	// lastBlock is the highest block number the step carries, the same watermark a stored bucket keeps.
	lastBlock           uint64
	blocks              int64
	gas                 uint64
	posterGas           uint64
	unknownPoster       bool
	sum, fees           *big.Int
	floor, posterFees   *big.Int
	unknownDestinations bool
	unknownPricing      bool
	minFee              *big.Int
	maxFee              *big.Int
	exponent            int64
	constraintBips      []int64
	backlogs            []uint64
	maxBacklog          []uint64
	minBaseFee          *string
	setID               int64
	errBips             int64
	// The spread of the step, over the whole seconds of it: cutoff is the second the serving clock is
	// inside, which is not one of them, second and secondGas the second being folded, and voided
	// records that one of the seconds had no authoritative poster gas, which leaves the step with no
	// spread at all rather than an understated one.
	cutoff           int64
	second           int64
	secondGas        uint64
	secondUnknown    bool
	seconds          int
	voided           bool
	minRate, maxRate uint64
}

// closeSecond folds the second in hand into the step's spread. A second without authoritative poster
// gas cannot be rated, and one unrated second voids the step's spread.
func (a *acc) closeSecond() {
	if a.second == 0 {
		return
	}
	switch {
	case a.secondUnknown:
		a.voided = true
	case a.seconds == 0:
		a.minRate, a.maxRate = a.secondGas, a.secondGas
	default:
		a.minRate, a.maxRate = min(a.minRate, a.secondGas), max(a.maxRate, a.secondGas)
	}
	if !a.secondUnknown {
		a.seconds++
	}
	a.second, a.secondGas, a.secondUnknown = 0, 0, false
}

// rate folds one block into the second it is stamped with, unless the serving clock is still inside
// that second: a second still being filled would read as a quiet one.
func (a *acc) rate(b db.Block) {
	second := b.TS.Unix()
	if second >= a.cutoff {
		return
	}
	if second != a.second {
		a.closeSecond()
		a.second = second
	}
	if b.PosterGas.Valid && b.PosterGas.Int64 >= 0 && uint64(b.PosterGas.Int64) <= b.GasUsed {
		a.secondGas += b.GasUsed - uint64(b.PosterGas.Int64)
		return
	}
	a.secondUnknown = true
}

func (a *acc) add(b db.Block, setID int64) {
	a.rate(b)
	fee := b.BaseFee.BigInt()
	if a.blocks == 0 || fee.Cmp(a.minFee) < 0 {
		a.minFee = fee
	}
	if a.maxFee == nil || fee.Cmp(a.maxFee) > 0 {
		a.maxFee = fee
	}
	a.blocks++
	a.last = max(a.last, b.TS.Unix())
	a.lastBlock = max(a.lastBlock, b.Number)
	a.gas += b.GasUsed
	if b.PosterGas.Valid && b.PosterGas.Int64 >= 0 && uint64(b.PosterGas.Int64) <= b.GasUsed {
		a.posterGas += uint64(b.PosterGas.Int64)
		a.posterFees.Add(a.posterFees, new(big.Int).Mul(fee, new(big.Int).SetInt64(b.PosterGas.Int64)))
	} else {
		a.unknownPoster = true
	}
	a.sum.Add(a.sum, fee)
	gas := new(big.Int).SetUint64(b.GasUsed)
	a.fees.Add(a.fees, new(big.Int).Mul(fee, gas))
	if b.DestinationsKnown() {
		posterGas := new(big.Int).SetInt64(b.PosterGas.Int64)
		compute := new(big.Int).Sub(gas, posterGas)
		rate := new(big.Int).Set(b.MinBaseFee.Wei.BigInt())
		if fee.Cmp(rate) < 0 {
			rate.Set(fee)
		}
		a.floor.Add(a.floor, new(big.Int).Mul(rate, compute))
	} else {
		a.unknownDestinations = true
	}
	a.unknownPricing = a.unknownPricing || !b.Known()
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

// point renders the step; the rate covers the part of the step before now. A step exists only because
// a block fell inside it, and that block's second is covered whether or not the serving clock has
// reached it, so a step always has a positive span.
func (a *acc) point(width time.Duration, now time.Time) model.SeriesPoint {
	end := time.Unix(a.last+1, 0)
	if end.Before(now) {
		end = now
	}
	secs, share := coverage(coveredSpan(time.Unix(a.start, 0), width, end, time.Time{}), width)
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
	if a.posterFees == nil {
		a.posterFees = new(big.Int)
	}
	pointCoverage, completeness := measuredCoverage(share)
	a.closeSecond()
	p := model.SeriesPoint{
		T: a.start, Blocks: a.blocks, GasUsed: a.gas, GasPerSecond: a.gas / secs, Coverage: pointCoverage, Completeness: completeness,
		FeesWei: a.fees.String(), BaseFeeMin: a.minFee.String(), BaseFeeAvg: avg.String(), BaseFeeMax: a.maxFee.String(),
		ExponentBips: a.exponent, ConstraintBips: a.constraintBips, Backlogs: a.backlogs, BacklogsMax: a.maxBacklog,
		MinBaseFee: a.minBaseFee, ConstraintSetID: a.setID, ReplayErrorBips: a.errBips,
	}
	if !a.unknownPoster {
		p.PosterGas = &a.posterGas
		p.PosterFeesWei = stringPtr(a.posterFees)
		p.ComputeGasPerSecond = uint64ValuePtr((a.gas - a.posterGas) / secs)
	}
	if a.unknownPricing {
		// One block of history without a recorded floor makes the whole step's pricing unknown.
		p.MinBaseFee, p.ConstraintBips = nil, nil
	}
	if !a.unknownDestinations {
		p.FloorFeesWei = stringPtr(a.floor)
		p.SurplusFeesWei = stringPtr(new(big.Int).Sub(new(big.Int).Sub(a.fees, a.floor), a.posterFees))
	}
	if a.seconds > 0 && !a.voided {
		p.ComputeGasPerSecondMin, p.ComputeGasPerSecondMax = uint64ValuePtr(a.minRate), uint64ValuePtr(a.maxRate)
	}
	return p
}
