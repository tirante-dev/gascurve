//go:build integration

package db

import (
	"context"
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
	if err != nil || dirty || v != 1 {
		t.Fatalf("version = %d dirty=%v err=%v", v, dirty, err)
	}
	if err := m.Down(1); err != nil {
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
	if err := p.SetNetworkError(ctx, 4663, "oops"); err != nil {
		t.Fatal(err)
	}
	if err := p.UpdateNetworkHead(ctx, 4663, 110, base.Add(10*time.Second), base.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}
	nets, err := p.Networks(ctx)
	if err != nil || len(nets) != 1 || nets[0].HeadBlock.Int64 != 110 || nets[0].LastError.Valid {
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
			ChainID: 4663, Number: 100 + i, TS: base.Add(time.Duration(i) * time.Second), GasUsed: 1_000_000 * (i + 1),
			BaseFee: WeiFromUint64(20_000_000 + i), L1Block: 50, TxCount: txs, Backlogs: pq.Int64Array{int64(i), 10},
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
	if err != nil || latest.Number != 110 || latest.GasUsed != 99 || !latest.Anchored || latest.Backlogs[1] != 10 {
		t.Fatalf("LatestBlock: %+v %v", latest, err)
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

	// Buckets fold incrementally, including array-wise maxima and the
	// last_block guard on the *_end fields.
	b1 := Bucket{ChainID: 4663, Resolution: Resolution1m, BucketStart: base, Blocks: 2, GasUsed: 100, FeesWei: WeiFromUint64(1000),
		BaseFeeMin: WeiFromUint64(10), BaseFeeAvg: WeiFromUint64(20), BaseFeeMax: WeiFromUint64(30), ExponentEndBips: 5,
		BacklogsEnd: pq.Int64Array{1, 2}, BacklogsMax: pq.Int64Array{5, 2}, ReplayErrorBips: 10, LastBlock: 200}
	if err := p.FoldBuckets(ctx, []Bucket{b1}); err != nil {
		t.Fatal(err)
	}
	b2 := b1
	b2.Blocks = 2
	b2.GasUsed = 50
	b2.FeesWei = WeiFromUint64(500)
	b2.BaseFeeMin = WeiFromUint64(5)
	b2.BaseFeeAvg = WeiFromUint64(40)
	b2.BaseFeeMax = WeiFromUint64(25)
	b2.ExponentEndBips = 9
	b2.BacklogsEnd = pq.Int64Array{7, 8}
	b2.BacklogsMax = pq.Int64Array{3, 9}
	b2.ReplayErrorBips = 4
	b2.LastBlock = 150 // older than the stored fold: *_end must not change
	if err := p.FoldBuckets(ctx, []Bucket{b2}); err != nil {
		t.Fatal(err)
	}
	bk, err := p.Buckets(ctx, 4663, Resolution1m, base, base.Add(time.Minute))
	if err != nil || len(bk) != 1 {
		t.Fatalf("Buckets: %+v %v", bk, err)
	}
	got := bk[0]
	if got.Blocks != 4 || got.GasUsed != 150 || got.FeesWei.Int64() != 1500 || got.BaseFeeMin.Int64() != 5 || got.BaseFeeMax.Int64() != 30 || got.BaseFeeAvg.Int64() != 30 {
		t.Fatalf("fold counters: %+v", got)
	}
	if got.ExponentEndBips != 5 || got.BacklogsEnd[0] != 1 || got.BacklogsMax[0] != 5 || got.BacklogsMax[1] != 9 || got.ReplayErrorBips != 10 || got.LastBlock != 200 {
		t.Fatalf("fold end fields: %+v", got)
	}
	b3 := b2
	b3.LastBlock = 300
	b3.ConstraintSetID.Valid = true
	b3.ConstraintSetID.Int64 = 1
	if err := p.FoldBuckets(ctx, []Bucket{b3}); err != nil {
		t.Fatal(err)
	}
	bk, _ = p.Buckets(ctx, 4663, Resolution1m, base, base.Add(time.Minute))
	if bk[0].ExponentEndBips != 9 || bk[0].BacklogsEnd[1] != 8 || bk[0].ConstraintSetID.Int64 != 1 || bk[0].LastBlock != 300 {
		t.Fatalf("newer fold should replace end fields: %+v", bk[0])
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
