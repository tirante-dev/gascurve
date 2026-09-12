package db

import (
	"database/sql"
	"math/big"
	"time"

	"github.com/lib/pq"

	"github.com/tirante-dev/gascurve/internal/pricer"
)

// ResolutionOrder fixes the fold order so output is deterministic.
var ResolutionOrder = []string{Resolution1m, Resolution15m, Resolution1h}

// ResolutionOf names the stored resolution of a width, when one of them is that wide.
func ResolutionOf(width time.Duration) (string, bool) {
	for _, name := range ResolutionOrder {
		if Resolutions[name] == width {
			return name, true
		}
	}
	return "", false
}

// BucketBuilder aggregates block rows into one bucket. It is the single definition of the bucket
// arithmetic, shared by the collector's folds, the in-memory store and the tests that pin the SQL
// rebuild to it.
type BucketBuilder struct {
	b Bucket
}

func NewBucketBuilder(chainID uint64, resolution string, start time.Time) *BucketBuilder {
	return &BucketBuilder{b: Bucket{
		ChainID: chainID, Resolution: resolution, BucketStart: start,
		PosterGas: sql.NullInt64{Valid: true}, FeesWei: NewWei(nil), PosterFeesWei: NewNullWei(nil),
		BaseFeeMin: NewWei(nil), BaseFeeAvg: NewWei(nil), BaseFeeMax: NewWei(nil), BaseFeeSum: NewNullWei(nil),
		MinBaseFee: NewNullWei(nil), FloorFeesWei: NewNullWei(nil), SurplusFeesWei: NewNullWei(nil),
		BacklogsEnd: Uint64Array{}, BacklogsMax: Uint64Array{}, ConstraintBipsEnd: pq.Int64Array{},
		PricingVersion: PricingFull,
	}}
}

// Add folds one block in. setID is the constraint set in force at the block. A block without the
// pricing breakdown makes the bucket's floor, surplus and minimum fee unknown: one block of guessed
// history must not be presented as an exact split for the whole window.
func (a *BucketBuilder) Add(blk Block, setID sql.NullInt64) {
	fee := blk.BaseFee.BigInt()
	if !blk.Known() {
		a.b.PricingVersion = PricingUnknown
	}
	if a.b.Blocks == 0 || fee.Cmp(a.b.BaseFeeMin.BigInt()) < 0 {
		a.b.BaseFeeMin = NewWei(new(big.Int).Set(fee))
	}
	if fee.Cmp(a.b.BaseFeeMax.BigInt()) > 0 {
		a.b.BaseFeeMax = NewWei(new(big.Int).Set(fee))
	}
	a.b.Blocks++
	a.b.GasUsed += blk.GasUsed
	a.b.BaseFeeSum = NewNullWei(new(big.Int).Add(a.b.BaseFeeSum.Wei.BigInt(), fee))
	gas := new(big.Int).SetUint64(blk.GasUsed)
	fees := new(big.Int).Mul(fee, gas)
	a.b.FeesWei = NewWei(new(big.Int).Add(fees, a.b.FeesWei.BigInt()))
	posterKnown := blk.PosterGas.Valid && blk.PosterGas.Int64 >= 0 && uint64(blk.PosterGas.Int64) <= blk.GasUsed
	if posterKnown && a.b.PosterGas.Valid {
		posterGas := uint64(blk.PosterGas.Int64)
		a.b.PosterGas.Int64 += blk.PosterGas.Int64
		posterFees := new(big.Int).Mul(fee, new(big.Int).SetUint64(posterGas))
		a.b.PosterFeesWei = NewNullWei(new(big.Int).Add(posterFees, a.b.PosterFeesWei.Wei.BigInt()))
	} else {
		a.b.PosterGas = sql.NullInt64{}
		a.b.PosterFeesWei = NullWei{}
	}
	if blk.DestinationsKnown() && a.b.FloorFeesWei.Valid && a.b.SurplusFeesWei.Valid {
		posterGas := uint64(blk.PosterGas.Int64)
		compute := new(big.Int).SetUint64(blk.GasUsed - posterGas)
		rate := new(big.Int).Set(blk.MinBaseFee.Wei.BigInt())
		if fee.Cmp(rate) < 0 {
			rate.Set(fee)
		}
		floor := new(big.Int).Mul(rate, compute)
		computeFees := new(big.Int).Mul(fee, compute)
		a.b.FloorFeesWei = NewNullWei(new(big.Int).Add(floor, a.b.FloorFeesWei.Wei.BigInt()))
		a.b.SurplusFeesWei = NewNullWei(new(big.Int).Add(new(big.Int).Sub(computeFees, floor), a.b.SurplusFeesWei.Wei.BigInt()))
	} else {
		a.b.FloorFeesWei, a.b.SurplusFeesWei = NullWei{}, NullWei{}
	}
	if blk.Number >= a.b.LastBlock {
		a.b.LastBlock = blk.Number
		a.b.ExponentEndBips = blk.ExponentBips
		a.b.BacklogsEnd = append(Uint64Array{}, blk.Backlogs...)
		a.b.ConstraintBipsEnd = append(pq.Int64Array{}, blk.ConstraintBips...)
		a.b.MinBaseFee = blk.MinBaseFee
		if setID.Valid {
			a.b.ConstraintSetID = setID
		}
	}
	for i, v := range blk.Backlogs {
		if i >= len(a.b.BacklogsMax) {
			a.b.BacklogsMax = append(a.b.BacklogsMax, v)
		} else if v > a.b.BacklogsMax[i] {
			a.b.BacklogsMax[i] = v
		}
	}
	if e := ReplayErrorBips(blk); e > a.b.ReplayErrorBips {
		a.b.ReplayErrorBips = e
	}
	a.b.ArbOSVersionMin, a.b.ArbOSVersionMax = extendArbOSRange(a.b, blk.ArbOSVersion, a.b.Blocks == 1)
}

// extendArbOSRange widens a bucket's ArbOS bounds with one block's version. A block that recorded none
// clears them: a range over some of the blocks would read as a range over all of them. `first` starts
// the range rather than widening the zero value.
func extendArbOSRange(b Bucket, v sql.NullInt64, first bool) (low, high sql.NullInt64) {
	if !v.Valid || (!first && !b.ArbOSVersionMin.Valid) {
		return sql.NullInt64{}, sql.NullInt64{}
	}
	if first {
		return v, v
	}
	lo, hi := b.ArbOSVersionMin, b.ArbOSVersionMax
	lo.Int64 = min(lo.Int64, v.Int64)
	hi.Int64 = max(hi.Int64, v.Int64)
	return lo, hi
}

// Bucket returns the aggregate; the average is derived from the exact sum. A window holding any block
// without the pricing breakdown reports no fee split and no floor at all.
func (a *BucketBuilder) Bucket() Bucket {
	if a.b.Blocks > 0 {
		a.b.BaseFeeAvg = NewWei(new(big.Int).Div(a.b.BaseFeeSum.Wei.BigInt(), big.NewInt(a.b.Blocks)))
	}
	if a.b.PricingVersion == PricingUnknown {
		a.b.MinBaseFee, a.b.FloorFeesWei, a.b.SurplusFeesWei = NullWei{}, NullWei{}, NullWei{}
		a.b.ConstraintBipsEnd = nil
	}
	return a.b
}

// baseFeeSumOf is the sum behind a bucket's average: the exact one when known, otherwise the rounded
// average times the block count.
func baseFeeSumOf(b Bucket) *big.Int {
	if b.BaseFeeSum.Valid {
		return b.BaseFeeSum.Wei.BigInt()
	}
	return new(big.Int).Mul(b.BaseFeeAvg.BigInt(), big.NewInt(b.Blocks))
}

// addNullWei adds two nullable values; the sum is unknown when either is.
func addNullWei(a, b NullWei) NullWei {
	if !a.Valid || !b.Valid {
		return NullWei{}
	}
	return NewNullWei(new(big.Int).Add(a.Wei.BigInt(), b.Wei.BigInt()))
}

// Blocks returns how many blocks were folded.
func (a *BucketBuilder) Blocks() int64 { return a.b.Blocks }

// ReplayErrorBips is |predicted - actual| in bips for a stored block, and zero for a block that
// carries no prediction: an absent one is not a perfect one, so it must not widen the bucket's max.
func ReplayErrorBips(blk Block) int64 {
	if !blk.PredictedBaseFee.Valid {
		return 0
	}
	return pricer.ErrorBips(blk.PredictedBaseFee.Wei.BigInt(), blk.BaseFee.BigInt())
}

// BucketStarts returns the distinct window starts of rows at a resolution, ascending.
func BucketStarts(rows []Block, resolution string) []time.Time {
	width := Resolutions[resolution]
	var out []time.Time
	seen := map[int64]bool{}
	for _, b := range rows {
		start := b.TS.UTC().Truncate(width)
		if !seen[start.Unix()] {
			seen[start.Unix()] = true
			out = append(out, start)
		}
	}
	return out
}

// FoldBlocks groups consecutive blocks into partial buckets for every resolution, in ResolutionOrder.
func FoldBlocks(rows []Block, setID func(uint64) sql.NullInt64) []Bucket {
	var out []Bucket
	for _, res := range ResolutionOrder {
		width := Resolutions[res]
		var cur *BucketBuilder
		for _, blk := range rows {
			start := blk.TS.UTC().Truncate(width)
			if cur == nil || !cur.b.BucketStart.Equal(start) {
				if cur != nil {
					out = append(out, cur.Bucket())
				}
				cur = NewBucketBuilder(blk.ChainID, res, start)
			}
			cur.Add(blk, setID(blk.Number))
		}
		if cur != nil {
			out = append(out, cur.Bucket())
		}
	}
	return out
}

// MergeBuckets adds partial bucket b into stored bucket old exactly like the SQL fold: counters and
// sums add, min/max combine, *_end fields follow the higher last block, array maxima are element-wise,
// and the pricing version is the lower of the two. A sum or split unknown on either side stays unknown,
// and a merged bucket holding any block without the breakdown reports no split and no floor.
func MergeBuckets(old, b Bucket) Bucket {
	merged := old
	merged.Blocks = old.Blocks + b.Blocks
	merged.GasUsed += b.GasUsed
	if old.PosterGas.Valid && b.PosterGas.Valid {
		merged.PosterGas = sql.NullInt64{Int64: old.PosterGas.Int64 + b.PosterGas.Int64, Valid: true}
	} else {
		merged.PosterGas = sql.NullInt64{}
	}
	merged.FeesWei = NewWei(new(big.Int).Add(old.FeesWei.BigInt(), b.FeesWei.BigInt()))
	merged.PosterFeesWei = addNullWei(old.PosterFeesWei, b.PosterFeesWei)
	merged.BaseFeeSum = addNullWei(old.BaseFeeSum, b.BaseFeeSum)
	merged.FloorFeesWei = addNullWei(old.FloorFeesWei, b.FloorFeesWei)
	merged.SurplusFeesWei = addNullWei(old.SurplusFeesWei, b.SurplusFeesWei)
	if old.Blocks == 0 || b.BaseFeeMin.BigInt().Cmp(old.BaseFeeMin.BigInt()) < 0 {
		merged.BaseFeeMin = b.BaseFeeMin
	}
	if b.BaseFeeMax.BigInt().Cmp(old.BaseFeeMax.BigInt()) > 0 {
		merged.BaseFeeMax = b.BaseFeeMax
	}
	if merged.Blocks > 0 {
		sum := new(big.Int).Add(baseFeeSumOf(old), baseFeeSumOf(b))
		merged.BaseFeeAvg = NewWei(sum.Div(sum, big.NewInt(merged.Blocks)))
	} else {
		merged.BaseFeeAvg = NewWei(nil)
	}
	merged.PricingVersion = min(old.PricingVersion, b.PricingVersion)
	if b.LastBlock >= old.LastBlock {
		merged.ExponentEndBips = b.ExponentEndBips
		merged.BacklogsEnd = b.BacklogsEnd
		merged.ConstraintBipsEnd = b.ConstraintBipsEnd
		merged.MinBaseFee = b.MinBaseFee
		if b.ConstraintSetID.Valid {
			merged.ConstraintSetID = b.ConstraintSetID
		}
		merged.LastBlock = b.LastBlock
	}
	if merged.PricingVersion == PricingUnknown {
		merged.MinBaseFee, merged.FloorFeesWei, merged.SurplusFeesWei = NullWei{}, NullWei{}, NullWei{}
		merged.ConstraintBipsEnd = nil
	}
	n := max(len(old.BacklogsMax), len(b.BacklogsMax))
	mx := make(Uint64Array, n)
	for i := range n {
		if i < len(old.BacklogsMax) {
			mx[i] = old.BacklogsMax[i]
		}
		if i < len(b.BacklogsMax) && b.BacklogsMax[i] > mx[i] {
			mx[i] = b.BacklogsMax[i]
		}
	}
	merged.BacklogsMax = mx
	merged.ReplayErrorBips = max(old.ReplayErrorBips, b.ReplayErrorBips)
	merged.ArbOSVersionMin, merged.ArbOSVersionMax = mergeArbOSRange(old, b)
	return merged
}

// mergeArbOSRange combines two ArbOS bounds the way the SQL fold does: an empty stored bucket takes the
// incoming range whole, and a side that recorded no version leaves the merged range unknown.
func mergeArbOSRange(old, b Bucket) (low, high sql.NullInt64) {
	if old.Blocks == 0 {
		return b.ArbOSVersionMin, b.ArbOSVersionMax
	}
	if b.Blocks == 0 {
		return old.ArbOSVersionMin, old.ArbOSVersionMax
	}
	if !old.ArbOSVersionMin.Valid || !b.ArbOSVersionMin.Valid {
		return sql.NullInt64{}, sql.NullInt64{}
	}
	lo := sql.NullInt64{Int64: min(old.ArbOSVersionMin.Int64, b.ArbOSVersionMin.Int64), Valid: true}
	hi := sql.NullInt64{Int64: max(old.ArbOSVersionMax.Int64, b.ArbOSVersionMax.Int64), Valid: true}
	return lo, hi
}
