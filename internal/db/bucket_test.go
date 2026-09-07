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
	return Block{ChainID: 1, Number: n, TS: ts, GasUsed: gas, PosterGas: sql.NullInt64{Valid: true}, BaseFee: WeiFromUint64(uint64(fee)), PredictedBaseFee: WeiFromUint64(uint64(fee + 1)),
		MinBaseFee: NullWeiFromUint64(uint64(minFee)), PricingVersion: PricingFull, Backlogs: Uint64Array(backlogs), ConstraintBips: pq.Int64Array{int64(n), 1}, ExponentBips: int64(n)}
}

func TestBucketPosterGasVector(t *testing.T) {
	t0 := time.Date(2026, 9, 6, 8, 0, 0, 0, time.UTC)
	b := mkBlock(1_000_000, t0, 20_036_000, 422_716, 20_000_000)
	b.PosterGas = sql.NullInt64{Int64: 767, Valid: true}
	got := FoldBlocks([]Block{b}, func(uint64) sql.NullInt64 { return sql.NullInt64{} })[0]
	if got.FeesWei.String() != "8469537776000" || got.FloorFeesWei.StringPtr() == nil || *got.FloorFeesWei.StringPtr() != "8438980000000" || *got.SurplusFeesWei.StringPtr() != "15190164000" || *got.PosterFeesWei.StringPtr() != "15367612000" {
		t.Fatalf("receipt-backed fee split: %+v", got)
	}
	if got.PosterGas.Int64 != 767 {
		t.Fatalf("poster gas: %+v", got.PosterGas)
	}
	sum := new(big.Int).Add(got.FloorFeesWei.Wei.BigInt(), got.SurplusFeesWei.Wei.BigInt())
	sum.Add(sum, got.PosterFeesWei.Wei.BigInt())
	if sum.Cmp(got.FeesWei.BigInt()) != 0 {
		t.Fatalf("destination sum %s, total %s", sum, got.FeesWei.String())
	}
	belowFloor := mkBlock(1_000_001, t0.Add(time.Second), 10, 100, 20)
	belowFloor.PosterGas = sql.NullInt64{Int64: 10, Valid: true}
	clamped := FoldBlocks([]Block{belowFloor}, func(uint64) sql.NullInt64 { return sql.NullInt64{} })[0]
	if clamped.FloorFeesWei.Wei.Int64() != 900 || clamped.SurplusFeesWei.Wei.Sign() != 0 || clamped.PosterFeesWei.Wei.Int64() != 100 || clamped.FeesWei.Int64() != 1_000 {
		t.Fatalf("base fee below minimum must clamp the infrastructure rate: %+v", clamped)
	}

	unknown := b
	unknown.PosterGas = sql.NullInt64{}
	mixed := FoldBlocks([]Block{b, unknown}, func(uint64) sql.NullInt64 { return sql.NullInt64{} })[0]
	if mixed.PosterGas.Valid || mixed.FloorFeesWei.Valid || mixed.SurplusFeesWei.Valid || mixed.PosterFeesWei.Valid {
		t.Fatalf("unknown receipt input must null destinations: %+v", mixed)
	}
	if !mixed.MinBaseFee.Valid || mixed.PricingVersion != PricingFull {
		t.Fatalf("receipt availability must not erase pricing data: %+v", mixed)
	}
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
	if first.Resolution != Resolution1m || first.Blocks != 2 || first.GasUsed != 30 || first.BaseFeeMin.Int64() != 100 || first.BaseFeeMax.Int64() != 300 || first.BaseFeeAvg.Int64() != 200 || first.BaseFeeSum.Wei.Int64() != 400 || !first.BaseFeeSum.Valid {
		t.Fatalf("first bucket: %+v", first)
	}
	if first.FeesWei.Int64() != 100*10+300*20 || first.ExponentEndBips != 2 || first.BacklogsEnd[0] != 9 || first.BacklogsMax[0] != 9 || first.BacklogsMax[1] != 50 || first.LastBlock != 2 {
		t.Fatalf("first bucket aggregates: %+v", first)
	}
	if first.FloorFeesWei.Wei.Int64() != 40*10+40*20 || first.SurplusFeesWei.Wei.Int64() != 100*10+300*20-40*30 || !first.SurplusFeesWei.Valid || first.MinBaseFee.Wei.Int64() != 40 || first.ConstraintBipsEnd[0] != 2 {
		t.Fatalf("first bucket fee split: %+v", first)
	}
	if !first.ConstraintSetID.Valid || first.ConstraintSetID.Int64 != 20 {
		t.Fatalf("constraint set id: %+v", first.ConstraintSetID)
	}
	if first.ReplayErrorBips != 100 { // block 1: |101-100|*10000/100
		t.Fatalf("replay error: %d", first.ReplayErrorBips)
	}
	second := buckets[1]
	if second.Blocks != 1 || !second.BucketStart.Equal(t0.Add(3*time.Second).Truncate(time.Minute)) || second.ConstraintSetID.Int64 != 30 || second.MinBaseFee.Wei.Int64() != 50 {
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
	if m.Blocks != 3 || m.BaseFeeSum.Wei.Int64() != 6 || m.BaseFeeAvg.Int64() != 2 || m.BaseFeeMin.Int64() != 1 || m.BaseFeeMax.Int64() != 3 {
		t.Fatalf("merged: %+v", m)
	}
	if m.LastBlock != 3 || m.BacklogsEnd[0] != 3 || m.BacklogsMax[0] != 3 || m.ConstraintSetID.Int64 != 4 || m.MinBaseFee.Wei.Int64() != 0 || m.ConstraintBipsEnd[0] != 3 {
		t.Fatalf("merged end fields: %+v", m)
	}
	// An older fold keeps the newer end fields; an empty stored bucket takes
	// the fold's minimum.
	old := MergeBuckets(m, a)
	if old.LastBlock != 3 || old.ExponentEndBips != 3 || old.Blocks != 5 || old.BaseFeeAvg.Int64() != 1 {
		t.Fatalf("older fold: %+v", old)
	}
	empty := MergeBuckets(Bucket{FeesWei: NewWei(nil), BaseFeeSum: NewNullWei(nil), FloorFeesWei: NewNullWei(nil), SurplusFeesWei: NewNullWei(nil), BaseFeeMin: NewWei(nil), BaseFeeMax: NewWei(nil)}, b)
	if empty.BaseFeeMin.Int64() != 3 || empty.Blocks != 1 {
		t.Fatalf("empty merge: %+v", empty)
	}
	if z := MergeBuckets(Bucket{}, Bucket{}); z.BaseFeeAvg.Sign() != 0 {
		t.Fatal("zero blocks average")
	}
}

// TestMergeBucketsUnknown: a stored bucket whose sum and fee split predate
// the columns (NULL) keeps them unknown after a fold, and its average is
// carried forward from the rounded reconstruction rather than invented.
func TestMergeBucketsUnknown(t *testing.T) {
	t0 := time.Date(2026, 9, 6, 7, 0, 0, 0, time.UTC)
	old := Bucket{Blocks: 2, BaseFeeAvg: WeiFromUint64(4), FeesWei: WeiFromUint64(100), BaseFeeMin: WeiFromUint64(3), BaseFeeMax: WeiFromUint64(5)}
	b := FoldBlocks([]Block{mkBlock(3, t0, 7, 1, 1, 3)}, func(uint64) sql.NullInt64 { return sql.NullInt64{} })[0]
	m := MergeBuckets(old, b)
	if m.BaseFeeSum.Valid || m.FloorFeesWei.Valid || m.SurplusFeesWei.Valid {
		t.Fatalf("unknown fields must stay unknown: %+v", m)
	}
	// The average is floor((4 times 2 + 7) / 3) = 5.
	if m.Blocks != 3 || m.BaseFeeAvg.Int64() != 5 || m.FeesWei.Int64() != 107 {
		t.Fatalf("merged average from the reconstruction: %+v", m)
	}
	if baseFeeSumOf(old).Int64() != 8 || baseFeeSumOf(b).Int64() != 7 {
		t.Fatal("baseFeeSumOf")
	}
	if got := addNullWei(NewNullWei(big.NewInt(1)), NullWei{}); got.Valid {
		t.Fatal("addNullWei with an unknown side")
	}
	if got := addNullWei(NewNullWei(big.NewInt(1)), NewNullWei(big.NewInt(2))); !got.Valid || got.Wei.Int64() != 3 {
		t.Fatal("addNullWei")
	}
}

// TestBucketPricingVersion: a window holding one block whose pricing
// breakdown was never recorded reports no floor, no fee split and no
// exponents at all, instead of summing zeros for what was never
// recorded and presenting the result as exact.
func TestBucketPricingVersion(t *testing.T) {
	t0 := time.Date(2026, 9, 6, 8, 0, 0, 0, time.UTC)
	known := mkBlock(1, t0, 100, 10, 40, 5)
	unknown := mkBlock(2, t0.Add(time.Second), 300, 20, 0, 9)
	unknown.PricingVersion, unknown.MinBaseFee, unknown.ConstraintBips = PricingUnknown, NullWei{}, nil
	if !known.Known() || unknown.Known() {
		t.Fatal("Known follows the pricing version and the recorded floor")
	}
	setID := func(uint64) sql.NullInt64 { return sql.NullInt64{} }
	mixed := FoldBlocks([]Block{known, unknown}, setID)[0]
	if mixed.PricingVersion != PricingUnknown {
		t.Fatalf("one unknown block makes the bucket unknown: %+v", mixed)
	}
	if mixed.FloorFeesWei.Valid || mixed.SurplusFeesWei.Valid || mixed.MinBaseFee.Valid || mixed.ConstraintBipsEnd != nil {
		t.Fatalf("no split may be reported for a window with unknown history: %+v", mixed)
	}
	// The additive fields are still exact: only the breakdown is unknown.
	if mixed.Blocks != 2 || mixed.GasUsed != 30 || !mixed.BaseFeeSum.Valid || mixed.BaseFeeSum.Wei.Int64() != 400 {
		t.Fatalf("additive fields stay exact: %+v", mixed)
	}
	// A window of known blocks alone keeps its split.
	full := FoldBlocks([]Block{known}, setID)[0]
	if full.PricingVersion != PricingFull || !full.FloorFeesWei.Valid || full.MinBaseFee.Wei.Int64() != 40 {
		t.Fatalf("known window: %+v", full)
	}
	// Merging a known partial bucket into an unknown stored one keeps the
	// whole row unknown, whichever side the unknown block came from.
	merged := MergeBuckets(mixed, full)
	if merged.PricingVersion != PricingUnknown || merged.FloorFeesWei.Valid || merged.MinBaseFee.Valid || merged.ConstraintBipsEnd != nil {
		t.Fatalf("a fold into unknown history stays unknown: %+v", merged)
	}
	if merged.Blocks != 3 || merged.GasUsed != 40 {
		t.Fatalf("merged counters: %+v", merged)
	}
	if back := MergeBuckets(full, mixed); back.PricingVersion != PricingUnknown || back.FloorFeesWei.Valid {
		t.Fatalf("unknown history folded into a known row: %+v", back)
	}
	// Two known buckets keep their split.
	if both := MergeBuckets(full, full); both.PricingVersion != PricingFull || !both.FloorFeesWei.Valid || both.FloorFeesWei.Wei.Int64() != 800 {
		t.Fatalf("known merge: %+v", both)
	}
}
