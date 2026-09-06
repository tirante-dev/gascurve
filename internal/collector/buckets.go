package collector

import (
	"database/sql"
	"math/big"
	"time"

	"github.com/lib/pq"

	"github.com/tirante-dev/gascurve/internal/db"
)

// resolutionOrder fixes the fold order so output is deterministic.
var resolutionOrder = []string{db.Resolution1m, db.Resolution15m, db.Resolution1h}

type bucketAcc struct {
	b   db.Bucket
	sum *big.Int
}

func newBucketAcc(chainID uint64, res string, start time.Time) *bucketAcc {
	return &bucketAcc{
		b: db.Bucket{
			ChainID: chainID, Resolution: res, BucketStart: start,
			FeesWei: db.NewWei(nil), BaseFeeMin: db.NewWei(nil), BaseFeeAvg: db.NewWei(nil), BaseFeeMax: db.NewWei(nil),
			BacklogsEnd: pq.Int64Array{}, BacklogsMax: pq.Int64Array{},
		},
		sum: new(big.Int),
	}
}

func (a *bucketAcc) add(blk db.Block, setID sql.NullInt64) {
	fee := blk.BaseFee.BigInt()
	if a.b.Blocks == 0 || fee.Cmp(a.b.BaseFeeMin.BigInt()) < 0 {
		a.b.BaseFeeMin = db.NewWei(new(big.Int).Set(fee))
	}
	if fee.Cmp(a.b.BaseFeeMax.BigInt()) > 0 {
		a.b.BaseFeeMax = db.NewWei(new(big.Int).Set(fee))
	}
	a.b.Blocks++
	a.b.GasUsed += blk.GasUsed
	a.sum.Add(a.sum, fee)
	fees := new(big.Int).Mul(fee, new(big.Int).SetUint64(blk.GasUsed))
	a.b.FeesWei = db.NewWei(fees.Add(fees, a.b.FeesWei.BigInt()))
	if blk.Number >= a.b.LastBlock {
		a.b.LastBlock = blk.Number
		a.b.ExponentEndBips = blk.ExponentBips
		a.b.BacklogsEnd = append(pq.Int64Array{}, blk.Backlogs...)
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
	if e := replayError(blk); e > a.b.ReplayErrorBips {
		a.b.ReplayErrorBips = e
	}
}

func (a *bucketAcc) finish() db.Bucket {
	if a.b.Blocks > 0 {
		a.b.BaseFeeAvg = db.NewWei(new(big.Int).Div(a.sum, big.NewInt(a.b.Blocks)))
	}
	return a.b
}

// replayError is |predicted - actual| in bips for a stored block.
func replayError(blk db.Block) int64 {
	actual := blk.BaseFee.BigInt()
	if actual.Sign() == 0 {
		return 0
	}
	diff := new(big.Int).Sub(blk.PredictedBaseFee.BigInt(), actual)
	diff.Abs(diff)
	diff.Mul(diff, big.NewInt(10_000))
	diff.Div(diff, actual)
	if !diff.IsInt64() {
		return 1<<63 - 1
	}
	return diff.Int64()
}

// foldBuckets groups consecutive blocks into partial buckets for every
// resolution. Blocks must be ascending by number.
func foldBuckets(rows []db.Block, setID func(uint64) sql.NullInt64) []db.Bucket {
	var out []db.Bucket
	for _, res := range resolutionOrder {
		width := db.Resolutions[res]
		var cur *bucketAcc
		for _, blk := range rows {
			start := blk.TS.UTC().Truncate(width)
			if cur == nil || !cur.b.BucketStart.Equal(start) {
				if cur != nil {
					out = append(out, cur.finish())
				}
				cur = newBucketAcc(blk.ChainID, res, start)
			}
			cur.add(blk, setID(blk.Number))
		}
		if cur != nil {
			out = append(out, cur.finish())
		}
	}
	return out
}
