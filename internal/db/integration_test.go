//go:build integration

package db

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/tirante-dev/gascurve/internal/logger"
)

func openIntegration(t *testing.T) *Postgres {
	t.Helper()
	url := os.Getenv("TEST_DB_URL")
	if url == "" {
		t.Skip("TEST_DB_URL not set")
	}
	d, err := Open(url, 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := ResetSchema(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	return NewPostgres(d)
}

func TestIntegrationMigrator(t *testing.T) {
	p := openIntegration(t)
	m, err := NewMigrator(p.DB().DB)
	if err != nil {
		t.Fatal(err)
	}
	v, dirty, err := m.Version()
	if err != nil || dirty || v != 5 {
		t.Fatalf("version = %d dirty=%v err=%v", v, dirty, err)
	}
	// Every migration rolls back and re-applies; the unsigned backlog
	// migration recovers values that an int64 cast wrapped.
	ctx := context.Background()
	if err := m.Down(4); err != nil {
		t.Fatal(err)
	}
	if v, _, err := m.Version(); err != nil || v != 1 {
		t.Fatalf("after down 4: %d %v", v, err)
	}
	if _, err := p.DB().ExecContext(ctx, `INSERT INTO blocks (chain_id, number, ts, gas_used, base_fee, backlogs) VALUES (1, 1, now(), 0, 0, '{-1,5}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DB().ExecContext(ctx, `INSERT INTO buckets (chain_id, resolution, bucket_start, blocks, base_fee_avg, backlogs_end, backlogs_max) VALUES (1, '1m', now(), 3, 7, '{-1}', '{-2,1}')`); err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil {
		t.Fatal(err)
	}
	b, err := p.BlockByNumber(ctx, 1, 1)
	if err != nil || b == nil || b.Backlogs[0] != math.MaxUint64 || b.Backlogs[1] != 5 || b.Hash != "" {
		t.Fatalf("migrated block: %+v %v", b, err)
	}
	var sum string
	if err := p.DB().QueryRowContext(ctx, `SELECT base_fee_sum::TEXT FROM buckets WHERE chain_id = 1`).Scan(&sum); err != nil || sum != "21" {
		t.Fatalf("base_fee_sum backfilled from the average: %q %v", sum, err)
	}
	bk, err := p.Buckets(ctx, 1, Resolution1m, time.Unix(0, 0), time.Now().Add(time.Hour))
	if err != nil || len(bk) != 1 || bk[0].BacklogsEnd[0] != math.MaxUint64 || bk[0].BacklogsMax[0] != math.MaxUint64-1 || bk[0].BacklogsMax[1] != 1 {
		t.Fatalf("migrated bucket: %+v %v", bk, err)
	}
	// Down clamps values above the BIGINT range.
	if err := m.Down(3); err != nil {
		t.Fatal(err)
	}
	var clamped string
	if err := p.DB().QueryRowContext(ctx, `SELECT backlogs::TEXT FROM blocks WHERE chain_id = 1`).Scan(&clamped); err != nil || clamped != "{9223372036854775807,5}" {
		t.Fatalf("clamped backlogs: %q %v", clamped, err)
	}
	if err := m.Up(); err != nil {
		t.Fatal(err)
	}
	if err := m.Down(5); err != nil {
		t.Fatal(err)
	}
	if v, _, err := m.Version(); err != nil || v != 0 {
		t.Fatalf("after down: %d %v", v, err)
	}
	if err := m.Down(0); err == nil {
		t.Fatal("Down(0) should fail")
	}
	if err := m.Up(); err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil {
		t.Fatal("Up twice should be a no-op")
	}
}

func TestIntegrationStore(t *testing.T) {
	p := openIntegration(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 6, 7, 0, 0, 0, time.UTC)

	if err := p.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.UpsertNetwork(ctx, Network{ChainID: 4663, Name: "robinhood", DisplayName: "Robinhood Chain", ExplorerURL: "https://x", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := p.UpsertNetwork(ctx, Network{ChainID: 4663, Name: "robinhood", DisplayName: "Robinhood", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	n, err := p.NetworkByRef(ctx, "robinhood")
	if err != nil || n == nil || n.DisplayName != "Robinhood" {
		t.Fatalf("NetworkByRef: %+v %v", n, err)
	}
	if n, err := p.NetworkByRef(ctx, "4663"); err != nil || n == nil {
		t.Fatalf("NetworkByRef by id: %+v %v", n, err)
	}
	if n, err := p.NetworkByRef(ctx, "missing"); err != nil || n != nil {
		t.Fatalf("NetworkByRef missing: %+v %v", n, err)
	}
	// A decimal beyond BIGINT is not an internal error, and a numeric name
	// never shadows a chain id.
	if n, err := p.NetworkByRef(ctx, "9223372036854775808"); err != nil || n != nil {
		t.Fatalf("NetworkByRef huge: %+v %v", n, err)
	}
	if err := p.UpsertNetwork(ctx, Network{ChainID: 7, Name: "4663", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if n, err := p.NetworkByRef(ctx, "4663"); err != nil || n == nil || n.ChainID != 4663 {
		t.Fatalf("NetworkByRef prefers the chain id: %+v %v", n, err)
	}
	if n, err := p.NetworkByRef(ctx, "7"); err != nil || n == nil || n.Name != "4663" {
		t.Fatalf("NetworkByRef by id 7: %+v %v", n, err)
	}
	if err := p.SetNetworkError(ctx, 4663, "oops"); err != nil {
		t.Fatal(err)
	}
	if err := p.UpdateNetworkHead(ctx, 4663, 110, base.Add(10*time.Second), base.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}
	nets, err := p.Networks(ctx)
	if err != nil || len(nets) != 2 || nets[0].ChainID != 7 || nets[1].HeadBlock.Int64 != 110 || nets[1].LastError.Valid {
		t.Fatalf("Networks: %+v %v", nets, err)
	}

	// Blocks: 100..110, one per second, two of them with two transactions.
	blocks := make([]Block, 0, 11)
	for i := uint64(0); i <= 10; i++ {
		txs := 3
		if i == 2 || i == 7 {
			txs = 2
		}
		blocks = append(blocks, Block{
			ChainID: 4663, Number: 100 + i, Hash: fmt.Sprintf("0x%x", 100+i), ParentHash: fmt.Sprintf("0x%x", 99+i),
			TS: base.Add(time.Duration(i) * time.Second), GasUsed: 1_000_000 * (i + 1),
			BaseFee: WeiFromUint64(20_000_000 + i), L1Block: 50, TxCount: txs, Backlogs: Uint64Array{i, math.MaxUint64 - i},
			ConstraintBips: pq.Int64Array{int64(i), 1}, MinBaseFee: WeiFromUint64(10_000_000),
			ExponentBips: int64(i), PredictedBaseFee: WeiFromUint64(20_000_000), Anchored: i == 10,
		})
	}
	if err := p.UpsertBlocks(ctx, blocks); err != nil {
		t.Fatal(err)
	}
	blocks[10].GasUsed = 99
	if err := p.UpsertBlocks(ctx, blocks[10:]); err != nil {
		t.Fatal(err)
	}
	latest, err := p.LatestBlock(ctx, 4663)
	if err != nil || latest.Number != 110 || latest.GasUsed != 99 || !latest.Anchored || latest.Backlogs[1] != math.MaxUint64-10 {
		t.Fatalf("LatestBlock: %+v %v", latest, err)
	}
	if latest.Hash != "0x6e" || latest.ParentHash != "0x6d" || latest.ConstraintBips[0] != 10 || latest.MinBaseFee.Int64() != 10_000_000 {
		t.Fatalf("LatestBlock new columns: %+v", latest)
	}
	if b, err := p.BlockByNumber(ctx, 4663, 105); err != nil || b == nil || b.Number != 105 || b.Backlogs[1] != math.MaxUint64-5 {
		t.Fatalf("BlockByNumber: %+v %v", b, err)
	}
	if b, err := p.BlockByNumber(ctx, 4663, 999); err != nil || b != nil {
		t.Fatalf("BlockByNumber missing: %+v %v", b, err)
	}
	if o, err := p.OldestBlock(ctx, 4663); err != nil || o.Number != 100 {
		t.Fatalf("OldestBlock: %+v %v", o, err)
	}
	if bs, err := p.RecentBlocks(ctx, 4663, 3); err != nil || len(bs) != 3 || bs[0].Number != 110 || bs[2].Number != 108 {
		t.Fatalf("RecentBlocks: %+v %v", bs, err)
	}
	if bs, err := p.BlocksAfter(ctx, 4663, 108, 10); err != nil || len(bs) != 2 || bs[0].Number != 109 {
		t.Fatalf("BlocksAfter: %+v %v", bs, err)
	}
	if bs, err := p.BlocksBetween(ctx, 4663, base.Add(2*time.Second), base.Add(4*time.Second)); err != nil || len(bs) != 2 || bs[0].Number != 102 {
		t.Fatalf("BlocksBetween: %+v %v", bs, err)
	}
	// (base+0, base+2] covers blocks 101 and 102: 2M + 3M gas.
	if g, err := p.GasUsedBetween(ctx, 4663, base, base.Add(2*time.Second)); err != nil || g != 5_000_000 {
		t.Fatalf("GasUsedBetween: %d %v", g, err)
	}
	if nums, err := p.TwoTxBlocks(ctx, 4663, 100, 10); err != nil || len(nums) != 2 || nums[0] != 102 || nums[1] != 107 {
		t.Fatalf("TwoTxBlocks: %v %v", nums, err)
	}

	// Buckets fold incrementally, including array-wise maxima, the exact
	// sum behind the average, and the last_block guard on the *_end fields.
	// The window is far from the block rows so RebuildBuckets below does
	// not touch it.
	fstart := base.Add(-24 * time.Hour)
	b1 := Bucket{ChainID: 4663, Resolution: Resolution1m, BucketStart: fstart, Blocks: 2, GasUsed: 100, FeesWei: WeiFromUint64(1000),
		BaseFeeMin: WeiFromUint64(10), BaseFeeAvg: WeiFromUint64(1), BaseFeeMax: WeiFromUint64(30), BaseFeeSum: WeiFromUint64(3), ExponentEndBips: 5,
		BacklogsEnd: Uint64Array{1, math.MaxUint64}, BacklogsMax: Uint64Array{5, math.MaxUint64}, ConstraintBipsEnd: pq.Int64Array{5, 0}, MinBaseFee: WeiFromUint64(2),
		FloorFeesWei: WeiFromUint64(200), SurplusFeesWei: WeiFromUint64(800), ReplayErrorBips: 10, LastBlock: 200}
	if err := p.FoldBuckets(ctx, []Bucket{b1}); err != nil {
		t.Fatal(err)
	}
	b2 := b1
	b2.Blocks = 1
	b2.GasUsed = 50
	b2.FeesWei = WeiFromUint64(500)
	b2.BaseFeeMin = WeiFromUint64(5)
	b2.BaseFeeAvg = WeiFromUint64(3)
	b2.BaseFeeSum = WeiFromUint64(3)
	b2.BaseFeeMax = WeiFromUint64(25)
	b2.ExponentEndBips = 9
	b2.BacklogsEnd = Uint64Array{7, 8}
	b2.BacklogsMax = Uint64Array{3, 9}
	b2.ConstraintBipsEnd = pq.Int64Array{9, 0}
	b2.MinBaseFee = WeiFromUint64(4)
	b2.FloorFeesWei = WeiFromUint64(100)
	b2.SurplusFeesWei = WeiFromUint64(400)
	b2.ReplayErrorBips = 4
	b2.LastBlock = 150 // older than the stored fold: *_end must not change
	if err := p.FoldBuckets(ctx, []Bucket{b2}); err != nil {
		t.Fatal(err)
	}
	bk, err := p.Buckets(ctx, 4663, Resolution1m, fstart, fstart.Add(time.Minute))
	if err != nil || len(bk) != 1 {
		t.Fatalf("Buckets: %+v %v", bk, err)
	}
	got := bk[0]
	// Average from the exact sum: floor(6/3) = 2, not floor((1*2+3*1)/3) = 1.
	if got.Blocks != 3 || got.GasUsed != 150 || got.FeesWei.Int64() != 1500 || got.BaseFeeMin.Int64() != 5 || got.BaseFeeMax.Int64() != 30 || got.BaseFeeSum.Int64() != 6 || got.BaseFeeAvg.Int64() != 2 {
		t.Fatalf("fold counters: %+v", got)
	}
	if got.FloorFeesWei.Int64() != 300 || got.SurplusFeesWei.Int64() != 1200 || got.MinBaseFee.Int64() != 2 || got.ConstraintBipsEnd[0] != 5 {
		t.Fatalf("fold fee split: %+v", got)
	}
	if got.ExponentEndBips != 5 || got.BacklogsEnd[0] != 1 || got.BacklogsEnd[1] != math.MaxUint64 || got.BacklogsMax[0] != 5 || got.BacklogsMax[1] != math.MaxUint64 || got.ReplayErrorBips != 10 || got.LastBlock != 200 {
		t.Fatalf("fold end fields: %+v", got)
	}
	b3 := b2
	b3.LastBlock = 300
	b3.ConstraintSetID.Valid = true
	b3.ConstraintSetID.Int64 = 1
	if err := p.FoldBuckets(ctx, []Bucket{b3}); err != nil {
		t.Fatal(err)
	}
	bk, _ = p.Buckets(ctx, 4663, Resolution1m, fstart, fstart.Add(time.Minute))
	if bk[0].ExponentEndBips != 9 || bk[0].BacklogsEnd[1] != 8 || bk[0].ConstraintSetID.Int64 != 1 || bk[0].LastBlock != 300 || bk[0].MinBaseFee.Int64() != 4 || bk[0].ConstraintBipsEnd[0] != 9 {
		t.Fatalf("newer fold should replace end fields: %+v", bk[0])
	}

	// RebuildBuckets recomputes a window from its rows exactly like the
	// Go builder, uses the set in force at the last block, is idempotent,
	// and drops a bucket whose window has no rows.
	setID, err := p.InsertConstraintSet(ctx, ConstraintSet{ChainID: 4663, EffectiveBlock: 105, EffectiveAt: base, Constraints: JSONB(`[]`), Source: "owner_action"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.RebuildBuckets(ctx, 4663, Resolution1m, []time.Time{base, base.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := p.RebuildBuckets(ctx, 4663, Resolution1m, []time.Time{base}); err != nil {
		t.Fatal(err)
	}
	rows, _ := p.BlocksBetween(ctx, 4663, base, base.Add(time.Minute))
	want := FoldBlocks(rows, func(n uint64) sql.NullInt64 {
		if n >= 105 {
			return sql.NullInt64{Int64: setID, Valid: true}
		}
		return sql.NullInt64{}
	})[0]
	rb, err := p.Buckets(ctx, 4663, Resolution1m, base, base.Add(2*time.Minute))
	if err != nil || len(rb) != 1 {
		t.Fatalf("rebuilt buckets: %+v %v", rb, err)
	}
	r := rb[0]
	if r.Blocks != want.Blocks || r.GasUsed != want.GasUsed || r.FeesWei.String() != want.FeesWei.String() || r.BaseFeeSum.String() != want.BaseFeeSum.String() ||
		r.BaseFeeAvg.String() != want.BaseFeeAvg.String() || r.BaseFeeMin.String() != want.BaseFeeMin.String() || r.BaseFeeMax.String() != want.BaseFeeMax.String() {
		t.Fatalf("rebuilt counters: %+v\nwant %+v", r, want)
	}
	if r.FloorFeesWei.String() != want.FloorFeesWei.String() || r.SurplusFeesWei.String() != want.SurplusFeesWei.String() || r.MinBaseFee.String() != want.MinBaseFee.String() {
		t.Fatalf("rebuilt fee split: %+v\nwant %+v", r, want)
	}
	if r.LastBlock != 110 || r.ExponentEndBips != 10 || r.BacklogsEnd[1] != math.MaxUint64-10 || r.BacklogsMax[0] != 10 || r.BacklogsMax[1] != math.MaxUint64 ||
		r.ConstraintBipsEnd[0] != 10 || r.ConstraintSetID.Int64 != setID || r.ReplayErrorBips != want.ReplayErrorBips {
		t.Fatalf("rebuilt end fields: %+v\nwant %+v", r, want)
	}
	// Reorg helpers: rows above a block go, their buckets follow.
	removed, err := p.DeleteBlocksAfter(ctx, 4663, 108)
	if err != nil || len(removed) != 2 || removed[0].Number != 109 || removed[1].Number != 110 {
		t.Fatalf("DeleteBlocksAfter: %+v %v", removed, err)
	}
	if err := p.RebuildBuckets(ctx, 4663, Resolution1m, BucketStarts(removed, Resolution1m)); err != nil {
		t.Fatal(err)
	}
	rb, _ = p.Buckets(ctx, 4663, Resolution1m, base, base.Add(2*time.Minute))
	if len(rb) != 1 || rb[0].Blocks != 9 || rb[0].LastBlock != 108 {
		t.Fatalf("bucket after rewind: %+v", rb)
	}
	if err := p.UpsertBlocks(ctx, removed); err != nil {
		t.Fatal(err)
	}
	if n, err := p.DeleteBucketsBefore(ctx, 4663, base); err != nil || n != 1 {
		t.Fatalf("DeleteBucketsBefore: %d %v", n, err)
	}
	if err := p.RewindAfter(ctx, 4663, 104); err != nil {
		t.Fatal(err)
	}
	if cs, _ := p.ConstraintSets(ctx, 4663); len(cs) != 0 {
		t.Fatalf("RewindAfter must drop the set at 105: %+v", cs)
	}

	// State samples with and without L1 data.
	s1 := StateSample{ChainID: 4663, SampledAt: base, BlockNumber: 100, BaseFee: WeiFromUint64(1), MinBaseFee: WeiFromUint64(1), Constraints: JSONB(`[]`), Prices: JSONB(`{}`)}
	s2 := s1
	s2.SampledAt = base.Add(time.Minute)
	s2.L1 = JSONB(`{"baseFeeEstimate":"1"}`)
	s2.Accounts = JSONB(`{"infra":{"address":"0x1","balance":"2"}}`)
	s2.Legacy = JSONB(`{"speedLimit":1}`)
	s3 := s1
	s3.SampledAt = base.Add(2 * time.Minute)
	for _, s := range []StateSample{s1, s2, s3} {
		if err := p.InsertStateSample(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.InsertStateSample(ctx, s3); err != nil {
		t.Fatal("re-insert should upsert")
	}
	ls, err := p.LatestStateSample(ctx, 4663, false)
	if err != nil || ls.SampledAt.Unix() != s3.SampledAt.Unix() || ls.L1 != nil {
		t.Fatalf("LatestStateSample: %+v %v", ls, err)
	}
	ls, err = p.LatestStateSample(ctx, 4663, true)
	if err != nil || ls.SampledAt.Unix() != s2.SampledAt.Unix() || string(ls.L1) != `{"baseFeeEstimate": "1"}` || ls.Legacy == nil {
		t.Fatalf("LatestStateSample l1: %+v %v", ls, err)
	}
	if ss, err := p.L1Samples(ctx, 4663, base, base.Add(time.Hour), 15*time.Minute); err != nil || len(ss) != 1 {
		t.Fatalf("L1Samples: %+v %v", ss, err)
	}
	if n, err := p.PruneStateSamples(ctx, 4663, base.Add(30*time.Second)); err != nil || n != 1 {
		t.Fatalf("PruneStateSamples: %d %v", n, err)
	}

	// Owner actions and constraint sets.
	acts := []OwnerAction{
		{ChainID: 4663, BlockNumber: 28, TxHash: "0xa", LogIndex: 0, TS: base.Add(-time.Hour), Method: "setGasPricingConstraints", Selector: "0xcc0d556a", Args: JSONB(`{"constraints":[]}`)},
		{ChainID: 4663, BlockNumber: 174150, TxHash: "0xb", LogIndex: 1, TS: base, Method: "setMinimumL2BaseFee", Selector: "0xa0188cdb", Args: JSONB(`{"priceInWei":"20000000"}`)},
	}
	if n, err := p.InsertOwnerActions(ctx, acts); err != nil || n != 2 {
		t.Fatalf("InsertOwnerActions: %d %v", n, err)
	}
	if n, err := p.InsertOwnerActions(ctx, acts); err != nil || n != 0 {
		t.Fatalf("duplicate InsertOwnerActions: %d %v", n, err)
	}
	if as, err := p.OwnerActions(ctx, 4663, time.Time{}, time.Time{}, 0); err != nil || len(as) != 2 || as[0].BlockNumber != 174150 {
		t.Fatalf("OwnerActions: %+v %v", as, err)
	}
	if as, err := p.OwnerActions(ctx, 4663, base.Add(-time.Minute), base.Add(time.Minute), 1); err != nil || len(as) != 1 || as[0].TxHash != "0xb" {
		t.Fatalf("OwnerActions ranged: %+v %v", as, err)
	}
	if as, err := p.OwnerActionsSince(ctx, 4663, 29); err != nil || len(as) != 1 || as[0].TxHash != "0xb" {
		t.Fatalf("OwnerActionsSince: %+v %v", as, err)
	}
	id, err := p.InsertConstraintSet(ctx, ConstraintSet{ChainID: 4663, EffectiveBlock: 28, EffectiveAt: base, Constraints: JSONB(`[{"target":1,"window":2,"startingBacklog":0}]`), Source: "genesis"})
	if err != nil || id == 0 {
		t.Fatalf("InsertConstraintSet: %d %v", id, err)
	}
	id2, err := p.InsertConstraintSet(ctx, ConstraintSet{ChainID: 4663, EffectiveBlock: 28, EffectiveAt: base, Constraints: JSONB(`[]`), Source: "genesis"})
	if err != nil || id2 != id {
		t.Fatalf("upsert should keep the id: %d vs %d %v", id2, id, err)
	}
	if cs, err := p.ConstraintSets(ctx, 4663); err != nil || len(cs) != 1 || string(cs[0].Constraints) != `[]` {
		t.Fatalf("ConstraintSets: %+v %v", cs, err)
	}

	// Batch reports aggregate per step.
	reports := []BatchReport{
		{ChainID: 4663, BlockNumber: 102, BatchNumber: 1, BatchTS: base, Poster: "0xp", CalldataLen: 100, CalldataNonzero: 80, ExtraGas: 10, L1BaseFee: WeiFromUint64(100), GasSpent: 1370, WeiSpent: WeiFromUint64(137_000)},
		{ChainID: 4663, BlockNumber: 107, BatchNumber: 2, BatchTS: base.Add(20 * time.Second), Poster: "0xp", CalldataLen: 50, CalldataNonzero: 40, ExtraGas: 10, L1BaseFee: WeiFromUint64(300), GasSpent: 690, WeiSpent: WeiFromUint64(207_000)},
	}
	if err := p.UpsertBatchReports(ctx, reports); err != nil {
		t.Fatal(err)
	}
	if err := p.UpsertBatchReports(ctx, reports[:1]); err != nil {
		t.Fatal(err)
	}
	bb, err := p.BatchBuckets(ctx, 4663, base.Add(-time.Hour), base.Add(time.Hour), time.Minute)
	if err != nil || len(bb) != 1 || bb[0].Batches != 2 || bb[0].GasSpent != 2060 || bb[0].WeiSpent.Int64() != 344_000 || bb[0].L1BaseFeeAvg.Int64() != 200 || bb[0].CalldataBytes != 150 {
		t.Fatalf("BatchBuckets: %+v %v", bb, err)
	}
	if bb[0].T.Unix() != base.Unix() {
		t.Fatalf("bucket start = %v", bb[0].T)
	}
	// Reports sharing a second stay separate rows, ordered by time then block.
	if err := p.UpsertBatchReports(ctx, []BatchReport{{ChainID: 4663, BlockNumber: 101, BatchNumber: 0, BatchTS: base, Poster: "0xp", L1BaseFee: WeiFromUint64(1), WeiSpent: WeiFromUint64(1)}}); err != nil {
		t.Fatal(err)
	}
	rs, err := p.BatchReports(ctx, 4663, base, base.Add(time.Minute))
	if err != nil || len(rs) != 3 || rs[0].BlockNumber != 101 || rs[1].BlockNumber != 102 || rs[2].BlockNumber != 107 {
		t.Fatalf("BatchReports: %+v %v", rs, err)
	}
	if err := p.RewindAfter(ctx, 4663, 105); err != nil {
		t.Fatal(err)
	}
	if rs, _ := p.BatchReports(ctx, 4663, base, base.Add(time.Minute)); len(rs) != 2 {
		t.Fatalf("RewindAfter must drop the report at 107: %+v", rs)
	}

	// Collector state.
	if _, ok, err := p.GetState(ctx, 4663, StateHead); err != nil || ok {
		t.Fatalf("GetState empty: %v %v", ok, err)
	}
	if err := p.SetState(ctx, 4663, StateHead, "110"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetState(ctx, 4663, StateHead, "111"); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := p.GetState(ctx, 4663, StateHead); err != nil || !ok || v != "111" {
		t.Fatalf("GetState: %s %v %v", v, ok, err)
	}
	if m, err := p.States(ctx, 4663); err != nil || m[StateHead] != "111" {
		t.Fatalf("States: %v %v", m, err)
	}

	// Transactions roll back on error.
	err = p.WithTx(ctx, func(s Store) error {
		if err := s.SetState(ctx, 4663, "tx", "1"); err != nil {
			return err
		}
		return context.Canceled
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok, _ := p.GetState(ctx, 4663, "tx"); ok {
		t.Fatal("rolled back write should not exist")
	}
	if err := p.WithTx(ctx, func(s Store) error { return s.SetState(ctx, 4663, "tx", "2") }); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := p.GetState(ctx, 4663, "tx"); v != "2" {
		t.Fatal("committed write missing")
	}

	// Pruning.
	if n, err := p.PruneBlocks(ctx, 4663, base.Add(5*time.Second)); err != nil || n != 5 {
		t.Fatalf("PruneBlocks: %d %v", n, err)
	}
	// A window whose rows were all pruned loses its bucket on rebuild.
	if err := p.RebuildBuckets(ctx, 4663, Resolution1m, []time.Time{base}); err != nil {
		t.Fatal(err)
	}
	if bk, _ := p.Buckets(ctx, 4663, Resolution1m, base, base.Add(time.Minute)); len(bk) != 1 || bk[0].Blocks != 6 {
		t.Fatalf("bucket after prune: %+v", bk)
	}
	if _, err := p.PruneBlocks(ctx, 4663, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := p.RebuildBuckets(ctx, 4663, Resolution1m, []time.Time{base}); err != nil {
		t.Fatal(err)
	}
	if bk, _ := p.Buckets(ctx, 4663, Resolution1m, base, base.Add(time.Minute)); len(bk) != 0 {
		t.Fatalf("empty window keeps a bucket: %+v", bk)
	}
}

func TestIntegrationNotifyListen(t *testing.T) {
	p := openIntegration(t)
	url := os.Getenv("TEST_DB_URL")
	lctx, lcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer lcancel()
	l, err := NewListener(lctx, url, []string{ChannelLive, ChannelOwnerAction}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal("second close should be a no-op")
	}
	select {
	case _, ok := <-l.Notifications():
		if ok {
			t.Fatal("expected closed channel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pump did not stop")
	}
	l, err = NewListener(lctx, url, []string{ChannelLive, ChannelOwnerAction}, logger.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	// Give the listener a moment to connect, then notify until it arrives.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := p.Notify(context.Background(), ChannelLive, `{"chainId":4663}`); err != nil {
			t.Fatal(err)
		}
		select {
		case n := <-l.Notifications():
			if n.Channel != ChannelLive || n.Payload != `{"chainId":4663}` {
				t.Fatalf("notification = %+v", n)
			}
			return
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("notification not delivered")
		}
	}
}
