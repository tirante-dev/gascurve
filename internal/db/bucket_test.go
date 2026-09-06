package db

import (
	"database/sql"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/lib/pq"
)

func mkBlock(n uint64, ts time.Time, fee int64, gas uint64, minFee int64, backlogs ...uint64) Block {
	return Block{ChainID: 1, Number: n, TS: ts, GasUsed: gas, BaseFee: WeiFromUint64(uint64(fee)), PredictedBaseFee: WeiFromUint64(uint64(fee + 1)),
		MinBaseFee: WeiFromUint64(uint64(minFee)), Backlogs: Uint64Array(backlogs), ConstraintBips: pq.Int64Array{int64(n), 1}, ExponentBips: int64(n)}
}

func TestFoldBlocks(t *testing.T) {
	t0 := time.Date(2026, 9, 6, 7, 59, 58, 0, time.UTC)
	rows := []Block{
		mkBlock(1, t0, 100, 10, 40, 5, 50),
		mkBlock(2, t0.Add(time.Second), 300, 20, 40, 9, 40),
		mkBlock(3, t0.Add(3*time.Second), 200, 30, 50, 1, 60), // next minute and next hour
	}
	setID := func(n uint64) sql.NullInt64 { return sql.NullInt64{Int64: int64(n * 10), Valid: n > 1} }
	buckets := FoldBlocks(rows, setID)
	if len(buckets) != 6 {
		t.Fatalf("expected 2 buckets per resolution, got %d", len(buckets))
	}
	first := buckets[0]
	if first.Resolution != Resolution1m || first.Blocks != 2 || first.GasUsed != 30 || first.BaseFeeMin.Int64() != 100 || first.BaseFeeMax.Int64() != 300 || first.BaseFeeAvg.Int64() != 200 || first.BaseFeeSum.Int64() != 400 {
		t.Fatalf("first bucket: %+v", first)
	}
	if first.FeesWei.Int64() != 100*10+300*20 || first.ExponentEndBips != 2 || first.BacklogsEnd[0] != 9 || first.BacklogsMax[0] != 9 || first.BacklogsMax[1] != 50 || first.LastBlock != 2 {
		t.Fatalf("first bucket aggregates: %+v", first)
	}
	if first.FloorFeesWei.Int64() != 40*10+40*20 || first.SurplusFeesWei.Int64() != 100*10+300*20-40*30 || first.MinBaseFee.Int64() != 40 || first.ConstraintBipsEnd[0] != 2 {
		t.Fatalf("first bucket fee split: %+v", first)
	}
	if !first.ConstraintSetID.Valid || first.ConstraintSetID.Int64 != 20 {
		t.Fatalf("constraint set id: %+v", first.ConstraintSetID)
	}
	if first.ReplayErrorBips != 100 { // block 1: |101-100|*10000/100
		t.Fatalf("replay error: %d", first.ReplayErrorBips)
	}
	second := buckets[1]
	if second.Blocks != 1 || !second.BucketStart.Equal(t0.Add(3*time.Second).Truncate(time.Minute)) || second.ConstraintSetID.Int64 != 30 || second.MinBaseFee.Int64() != 50 {
		t.Fatalf("second bucket: %+v", second)
	}
	hour := buckets[5]
	if hour.Resolution != Resolution1h || hour.BucketStart.Hour() != 8 {
		t.Fatalf("hour bucket: %+v", hour)
	}
	if FoldBlocks(nil, setID) != nil {
		t.Fatal("no rows, no buckets")
	}
	zero := Block{BaseFee: NewWei(nil), PredictedBaseFee: WeiFromUint64(5)}
	if ReplayErrorBips(zero) != 0 {
		t.Fatal("zero actual fee yields zero error")
	}
	huge := Block{BaseFee: WeiFromUint64(1), PredictedBaseFee: NewWei(new(big.Int).Lsh(big.NewInt(1), 90))}
	if ReplayErrorBips(huge) != math.MaxInt64 {
		t.Fatal("saturated error")
	}
	if starts := BucketStarts(rows, Resolution1m); len(starts) != 2 || !starts[0].Equal(t0.Truncate(time.Minute)) {
		t.Fatalf("bucket starts: %v", starts)
	}
	acc := NewBucketBuilder(1, Resolution1m, t0)
	if acc.Blocks() != 0 {
		t.Fatal("empty builder")
	}
	acc.Add(rows[0], sql.NullInt64{})
	if acc.Blocks() != 1 || acc.Bucket().Blocks != 1 {
		t.Fatal("builder count")
	}
	// Backlogs above MaxInt64 survive the fold exactly.
	big1 := mkBlock(4, t0, 1, 1, 1, math.MaxUint64)
	if b := FoldBlocks([]Block{big1}, setID); b[0].BacklogsEnd[0] != math.MaxUint64 || b[0].BacklogsMax[0] != math.MaxUint64 {
		t.Fatalf("uint64 backlogs: %+v", b[0])
	}
}

// TestMergeBucketsExactAverage: merging derives the average from the exact
// sum, so folding [1,2] then [3] yields floor(6/3) = 2, not floor((1*2+3)/3) = 1.
func TestMergeBucketsExactAverage(t *testing.T) {
	t0 := time.Date(2026, 9, 6, 7, 0, 0, 0, time.UTC)
	a := FoldBlocks([]Block{mkBlock(1, t0, 1, 1, 0, 1), mkBlock(2, t0, 2, 1, 0, 2)}, func(uint64) sql.NullInt64 { return sql.NullInt64{} })[0]
	b := FoldBlocks([]Block{mkBlock(3, t0, 3, 1, 0, 3)}, func(uint64) sql.NullInt64 { return sql.NullInt64{Int64: 4, Valid: true} })[0]
	if a.BaseFeeAvg.Int64() != 1 {
		t.Fatalf("floored partial average: %+v", a)
	}
	m := MergeBuckets(a, b)
	if m.Blocks != 3 || m.BaseFeeSum.Int64() != 6 || m.BaseFeeAvg.Int64() != 2 || m.BaseFeeMin.Int64() != 1 || m.BaseFeeMax.Int64() != 3 {
		t.Fatalf("merged: %+v", m)
	}
	if m.LastBlock != 3 || m.BacklogsEnd[0] != 3 || m.BacklogsMax[0] != 3 || m.ConstraintSetID.Int64 != 4 || m.MinBaseFee.Int64() != 0 || m.ConstraintBipsEnd[0] != 3 {
		t.Fatalf("merged end fields: %+v", m)
	}
	// An older fold keeps the newer end fields; an empty stored bucket takes
	// the fold's minimum.
	old := MergeBuckets(m, a)
	if old.LastBlock != 3 || old.ExponentEndBips != 3 || old.Blocks != 5 || old.BaseFeeAvg.Int64() != 1 {
		t.Fatalf("older fold: %+v", old)
	}
	empty := MergeBuckets(Bucket{FeesWei: NewWei(nil), BaseFeeSum: NewWei(nil), FloorFeesWei: NewWei(nil), SurplusFeesWei: NewWei(nil), BaseFeeMin: NewWei(nil), BaseFeeMax: NewWei(nil)}, b)
	if empty.BaseFeeMin.Int64() != 3 || empty.Blocks != 1 {
		t.Fatalf("empty merge: %+v", empty)
	}
	if z := MergeBuckets(Bucket{}, Bucket{}); z.BaseFeeAvg.Sign() != 0 {
		t.Fatal("zero blocks average")
	}
}
