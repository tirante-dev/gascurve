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

// BucketBuilder aggregates block rows into one bucket. It is the single
// definition of the bucket arithmetic, shared by the collector's folds, the
// in-memory store and the tests that pin the SQL rebuild to it.
type BucketBuilder struct {
	b Bucket
}

// NewBucketBuilder starts an empty bucket.
func NewBucketBuilder(chainID uint64, resolution string, start time.Time) *BucketBuilder {
	return &BucketBuilder{b: Bucket{
		ChainID: chainID, Resolution: resolution, BucketStart: start,
		FeesWei: NewWei(nil), BaseFeeMin: NewWei(nil), BaseFeeAvg: NewWei(nil), BaseFeeMax: NewWei(nil), BaseFeeSum: NewWei(nil),
		MinBaseFee: NewWei(nil), FloorFeesWei: NewWei(nil), SurplusFeesWei: NewWei(nil),
		BacklogsEnd: Uint64Array{}, BacklogsMax: Uint64Array{}, ConstraintBipsEnd: pq.Int64Array{},
	}}
}

// Add folds one block in. setID is the constraint set in force at the block.
func (a *BucketBuilder) Add(blk Block, setID sql.NullInt64) {
	fee := blk.BaseFee.BigInt()
	if a.b.Blocks == 0 || fee.Cmp(a.b.BaseFeeMin.BigInt()) < 0 {
		a.b.BaseFeeMin = NewWei(new(big.Int).Set(fee))
	}
	if fee.Cmp(a.b.BaseFeeMax.BigInt()) > 0 {
		a.b.BaseFeeMax = NewWei(new(big.Int).Set(fee))
	}
	a.b.Blocks++
	a.b.GasUsed += blk.GasUsed
	a.b.BaseFeeSum = NewWei(new(big.Int).Add(a.b.BaseFeeSum.BigInt(), fee))
	gas := new(big.Int).SetUint64(blk.GasUsed)
	fees := new(big.Int).Mul(fee, gas)
	a.b.FeesWei = NewWei(new(big.Int).Add(fees, a.b.FeesWei.BigInt()))
	floor := new(big.Int).Mul(blk.MinBaseFee.BigInt(), gas)
	a.b.FloorFeesWei = NewWei(new(big.Int).Add(floor, a.b.FloorFeesWei.BigInt()))
	a.b.SurplusFeesWei = NewWei(new(big.Int).Sub(a.b.FeesWei.BigInt(), a.b.FloorFeesWei.BigInt()))
	if blk.Number >= a.b.LastBlock {
		a.b.LastBlock = blk.Number
		a.b.ExponentEndBips = blk.ExponentBips
		a.b.BacklogsEnd = append(Uint64Array{}, blk.Backlogs...)
		a.b.ConstraintBipsEnd = append(pq.Int64Array{}, blk.ConstraintBips...)
		a.b.MinBaseFee = NewWei(new(big.Int).Set(blk.MinBaseFee.BigInt()))
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
}

// Bucket returns the aggregate; the average is derived from the exact sum.
func (a *BucketBuilder) Bucket() Bucket {
	if a.b.Blocks > 0 {
		a.b.BaseFeeAvg = NewWei(new(big.Int).Div(a.b.BaseFeeSum.BigInt(), big.NewInt(a.b.Blocks)))
	}
	return a.b
}

// Blocks returns how many blocks were folded.
func (a *BucketBuilder) Blocks() int64 { return a.b.Blocks }

// ReplayErrorBips is |predicted - actual| in bips for a stored block.
func ReplayErrorBips(blk Block) int64 {
	return pricer.ErrorBips(blk.PredictedBaseFee.BigInt(), blk.BaseFee.BigInt())
}

// BucketStarts returns the distinct window starts of rows at a resolution,
// ascending.
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

// FoldBlocks groups consecutive blocks (ascending by number) into partial
// buckets for every resolution, in ResolutionOrder.
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

// MergeBuckets adds partial bucket b into stored bucket old exactly like the
// SQL fold: counters and sums add, min/max combine, *_end fields follow the
// higher last block, array maxima are element-wise.
func MergeBuckets(old, b Bucket) Bucket {
	merged := old
	merged.Blocks = old.Blocks + b.Blocks
	merged.GasUsed += b.GasUsed
	merged.FeesWei = NewWei(new(big.Int).Add(old.FeesWei.BigInt(), b.FeesWei.BigInt()))
	merged.BaseFeeSum = NewWei(new(big.Int).Add(old.BaseFeeSum.BigInt(), b.BaseFeeSum.BigInt()))
	merged.FloorFeesWei = NewWei(new(big.Int).Add(old.FloorFeesWei.BigInt(), b.FloorFeesWei.BigInt()))
	merged.SurplusFeesWei = NewWei(new(big.Int).Add(old.SurplusFeesWei.BigInt(), b.SurplusFeesWei.BigInt()))
	if old.Blocks == 0 || b.BaseFeeMin.BigInt().Cmp(old.BaseFeeMin.BigInt()) < 0 {
		merged.BaseFeeMin = b.BaseFeeMin
	}
	if b.BaseFeeMax.BigInt().Cmp(old.BaseFeeMax.BigInt()) > 0 {
		merged.BaseFeeMax = b.BaseFeeMax
	}
	if merged.Blocks > 0 {
		merged.BaseFeeAvg = NewWei(new(big.Int).Div(merged.BaseFeeSum.BigInt(), big.NewInt(merged.Blocks)))
	} else {
		merged.BaseFeeAvg = NewWei(nil)
	}
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
	return merged
}
