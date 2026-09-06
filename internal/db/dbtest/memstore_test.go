package dbtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/tirante-dev/gascurve/internal/db"
)

func TestMemStore(t *testing.T) {
	m := New()
	ctx := context.Background()
	base := time.Date(2026, 9, 6, 7, 0, 0, 0, time.UTC)

	if err := m.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.UpsertNetwork(ctx, db.Network{ChainID: 1, Name: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateNetworkHead(ctx, 1, 10, base, base); err != nil {
		t.Fatal(err)
	}
	if err := m.UpsertNetwork(ctx, db.Network{ChainID: 1, Name: "a", DisplayName: "A"}); err != nil {
		t.Fatal(err)
	}
	if err := m.UpsertNetwork(ctx, db.Network{ChainID: 2, Name: "b"}); err != nil {
		t.Fatal(err)
	}
	nets, _ := m.Networks(ctx)
	if len(nets) != 2 || nets[0].DisplayName != "A" || nets[0].HeadBlock.Int64 != 10 {
		t.Fatalf("networks: %+v", nets)
	}
	if n, _ := m.NetworkByRef(ctx, "2"); n == nil || n.Name != "b" {
		t.Fatal("by id")
	}
	if n, _ := m.NetworkByRef(ctx, "a"); n == nil {
		t.Fatal("by name")
	}
	if n, _ := m.NetworkByRef(ctx, "zz"); n != nil {
		t.Fatal("missing")
	}
	if err := m.SetNetworkError(ctx, 1, "x"); err != nil {
		t.Fatal(err)
	}
	if n, _ := m.NetworkByRef(ctx, "a"); !n.LastError.Valid {
		t.Fatal("error not set")
	}

	for i := uint64(1); i <= 5; i++ {
		txc := 3
		if i == 3 {
			txc = 2
		}
		if err := m.UpsertBlocks(ctx, []db.Block{{ChainID: 1, Number: i, TS: base.Add(time.Duration(i) * time.Second), GasUsed: i, TxCount: txc}}); err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := m.LatestBlock(ctx, 1); b.Number != 5 {
		t.Fatal("latest")
	}
	if b, _ := m.OldestBlock(ctx, 1); b.Number != 1 {
		t.Fatal("oldest")
	}
	if b, _ := m.LatestBlock(ctx, 9); b != nil {
		t.Fatal("latest empty")
	}
	if b, _ := m.OldestBlock(ctx, 9); b != nil {
		t.Fatal("oldest empty")
	}
	if bs, _ := m.RecentBlocks(ctx, 1, 2); len(bs) != 2 || bs[0].Number != 5 {
		t.Fatal("recent")
	}
	if bs, _ := m.BlocksAfter(ctx, 1, 3, 10); len(bs) != 2 || bs[0].Number != 4 {
		t.Fatal("after")
	}
	if bs, _ := m.BlocksBetween(ctx, 1, base.Add(2*time.Second), base.Add(4*time.Second)); len(bs) != 2 {
		t.Fatal("between")
	}
	if g, _ := m.GasUsedBetween(ctx, 1, base.Add(time.Second), base.Add(3*time.Second)); g != 5 {
		t.Fatalf("gas = %d", g)
	}
	if nums, _ := m.TwoTxBlocks(ctx, 1, 0, 10); len(nums) != 1 || nums[0] != 3 {
		t.Fatal("two tx")
	}
	if n, _ := m.PruneBlocks(ctx, 1, base.Add(2*time.Second)); n != 1 {
		t.Fatal("prune blocks")
	}

	b1 := db.Bucket{ChainID: 1, Resolution: "1m", BucketStart: base, Blocks: 1, GasUsed: 10, FeesWei: db.WeiFromUint64(10), BaseFeeMin: db.WeiFromUint64(10), BaseFeeAvg: db.WeiFromUint64(10), BaseFeeMax: db.WeiFromUint64(10), ExponentEndBips: 1, BacklogsEnd: pq.Int64Array{1}, BacklogsMax: pq.Int64Array{1, 5}, ReplayErrorBips: 3, LastBlock: 5}
	b2 := b1
	b2.Blocks, b2.GasUsed, b2.FeesWei = 3, 30, db.WeiFromUint64(30)
	b2.BaseFeeMin, b2.BaseFeeAvg, b2.BaseFeeMax = db.WeiFromUint64(2), db.WeiFromUint64(30), db.WeiFromUint64(40)
	b2.ExponentEndBips, b2.BacklogsEnd, b2.BacklogsMax, b2.ReplayErrorBips, b2.LastBlock = 9, pq.Int64Array{7}, pq.Int64Array{3, 2, 8}, 1, 9
	b2.ConstraintSetID.Valid, b2.ConstraintSetID.Int64 = true, 4
	if err := m.FoldBuckets(ctx, []db.Bucket{b1, b2}); err != nil {
		t.Fatal(err)
	}
	old := b1
	old.LastBlock = 2
	old.ExponentEndBips = 99
	if err := m.FoldBuckets(ctx, []db.Bucket{old}); err != nil {
		t.Fatal(err)
	}
	bk, _ := m.Buckets(ctx, 1, "1m", base, base.Add(time.Hour))
	if len(bk) != 1 || bk[0].Blocks != 5 || bk[0].GasUsed != 50 || bk[0].BaseFeeMin.Int64() != 2 || bk[0].BaseFeeMax.Int64() != 40 || bk[0].BaseFeeAvg.Int64() != 22 {
		t.Fatalf("fold: %+v", bk)
	}
	if bk[0].ExponentEndBips != 9 || bk[0].BacklogsMax[2] != 8 || bk[0].BacklogsMax[1] != 5 || bk[0].ConstraintSetID.Int64 != 4 || bk[0].ReplayErrorBips != 3 || bk[0].LastBlock != 9 {
		t.Fatalf("fold end: %+v", bk[0])
	}
	if m.BucketCount(1, "1m") != 1 {
		t.Fatal("bucket count")
	}

	s := db.StateSample{ChainID: 1, SampledAt: base, L1: db.JSONB(`{}`)}
	_ = m.InsertStateSample(ctx, s)
	_ = m.InsertStateSample(ctx, s)
	_ = m.InsertStateSample(ctx, db.StateSample{ChainID: 1, SampledAt: base.Add(time.Second)})
	_ = m.InsertStateSample(ctx, db.StateSample{ChainID: 1, SampledAt: base.Add(time.Minute), L1: db.JSONB(`{}`)})
	if ls, _ := m.LatestStateSample(ctx, 1, false); ls.SampledAt != base.Add(time.Minute) {
		t.Fatal("latest sample")
	}
	if ls, _ := m.LatestStateSample(ctx, 1, true); ls.SampledAt != base.Add(time.Minute) {
		t.Fatal("latest l1 sample")
	}
	if ls, _ := m.LatestStateSample(ctx, 9, true); ls != nil {
		t.Fatal("latest empty")
	}
	if ss, _ := m.L1Samples(ctx, 1, base, base.Add(time.Hour), 15*time.Minute); len(ss) != 1 {
		t.Fatalf("l1 samples: %d", len(ss))
	}
	if n, _ := m.PruneStateSamples(ctx, 1, base.Add(time.Second)); n != 1 {
		t.Fatal("prune samples")
	}

	acts := []db.OwnerAction{{ChainID: 1, TxHash: "a", LogIndex: 0, BlockNumber: 5, TS: base}, {ChainID: 1, TxHash: "a", LogIndex: 1, BlockNumber: 5, TS: base.Add(time.Hour)}}
	if n, _ := m.InsertOwnerActions(ctx, acts); n != 2 {
		t.Fatal("insert actions")
	}
	if n, _ := m.InsertOwnerActions(ctx, acts); n != 0 {
		t.Fatal("duplicate actions")
	}
	if as, _ := m.OwnerActions(ctx, 1, time.Time{}, time.Time{}, 0); len(as) != 2 || as[0].LogIndex != 1 {
		t.Fatal("actions order")
	}
	if as, _ := m.OwnerActions(ctx, 1, base.Add(-time.Minute), base.Add(time.Minute), 1); len(as) != 1 || as[0].LogIndex != 0 {
		t.Fatal("actions range")
	}
	id, _ := m.InsertConstraintSet(ctx, db.ConstraintSet{ChainID: 1, EffectiveBlock: 1, Source: "genesis"})
	id2, _ := m.InsertConstraintSet(ctx, db.ConstraintSet{ChainID: 1, EffectiveBlock: 1, Source: "genesis"})
	id3, _ := m.InsertConstraintSet(ctx, db.ConstraintSet{ChainID: 1, EffectiveBlock: 0, Source: "observed"})
	if id != id2 || id3 == id {
		t.Fatal("constraint set ids")
	}
	if cs, _ := m.ConstraintSets(ctx, 1); len(cs) != 2 || cs[0].EffectiveBlock != 0 {
		t.Fatal("constraint sets")
	}

	reps := []db.BatchReport{
		{ChainID: 1, BlockNumber: 1, BatchTS: base, GasSpent: 10, WeiSpent: db.WeiFromUint64(100), L1BaseFee: db.WeiFromUint64(10), CalldataLen: 5},
		{ChainID: 1, BlockNumber: 2, BatchTS: base.Add(10 * time.Second), GasSpent: 20, WeiSpent: db.WeiFromUint64(200), L1BaseFee: db.WeiFromUint64(30), CalldataLen: 5},
	}
	_ = m.UpsertBatchReports(ctx, reps)
	bb, _ := m.BatchBuckets(ctx, 1, base.Add(-time.Hour), base.Add(time.Hour), time.Minute)
	if len(bb) != 1 || bb[0].Batches != 2 || bb[0].GasSpent != 30 || bb[0].WeiSpent.Int64() != 300 || bb[0].L1BaseFeeAvg.Int64() != 20 || bb[0].CalldataBytes != 10 {
		t.Fatalf("batch buckets: %+v", bb)
	}

	if _, ok, _ := m.GetState(ctx, 1, "k"); ok {
		t.Fatal("state empty")
	}
	_ = m.SetState(ctx, 1, "k", "v")
	if v, ok, _ := m.GetState(ctx, 1, "k"); !ok || v != "v" {
		t.Fatal("state")
	}
	if st, _ := m.States(ctx, 1); st["k"] != "v" {
		t.Fatal("states")
	}
	_ = m.Notify(ctx, "c", "p")
	if n, ok := m.LastNotification("c"); !ok || n.Payload != "p" {
		t.Fatal("notify")
	}
	if _, ok := m.LastNotification("zz"); ok {
		t.Fatal("no notify")
	}
	if err := m.WithTx(ctx, func(s db.Store) error { return s.SetState(ctx, 1, "t", "1") }); err != nil {
		t.Fatal(err)
	}
	m.SetFailure("Ping", true)
	if !m.FailOn["Ping"] {
		t.Fatal("SetFailure on")
	}
	m.SetFailure("Ping", false)
	if m.FailOn["Ping"] {
		t.Fatal("SetFailure off")
	}
}

func TestMemStoreFailures(t *testing.T) {
	m := New()
	ctx := context.Background()
	m.PingErr = ErrInjected
	if err := m.Ping(ctx); !errors.Is(err, ErrInjected) {
		t.Fatal("ping")
	}
	names := []string{"WithTx", "UpsertNetwork", "Networks", "NetworkByRef", "UpdateNetworkHead", "SetNetworkError", "UpsertBlocks", "LatestBlock", "OldestBlock", "RecentBlocks", "BlocksAfter", "BlocksBetween", "GasUsedBetween", "TwoTxBlocks", "PruneBlocks", "FoldBuckets", "Buckets", "InsertStateSample", "LatestStateSample", "L1Samples", "PruneStateSamples", "InsertOwnerActions", "OwnerActions", "InsertConstraintSet", "ConstraintSets", "UpsertBatchReports", "BatchBuckets", "GetState", "SetState", "States", "Notify"}
	for _, n := range names {
		m.FailOn[n] = true
	}
	now := time.Now()
	calls := map[string]func() error{
		"WithTx":             func() error { return m.WithTx(ctx, func(db.Store) error { return nil }) },
		"UpsertNetwork":      func() error { return m.UpsertNetwork(ctx, db.Network{}) },
		"Networks":           func() error { _, err := m.Networks(ctx); return err },
		"NetworkByRef":       func() error { _, err := m.NetworkByRef(ctx, ""); return err },
		"UpdateNetworkHead":  func() error { return m.UpdateNetworkHead(ctx, 1, 1, now, now) },
		"SetNetworkError":    func() error { return m.SetNetworkError(ctx, 1, "") },
		"UpsertBlocks":       func() error { return m.UpsertBlocks(ctx, nil) },
		"LatestBlock":        func() error { _, err := m.LatestBlock(ctx, 1); return err },
		"OldestBlock":        func() error { _, err := m.OldestBlock(ctx, 1); return err },
		"RecentBlocks":       func() error { _, err := m.RecentBlocks(ctx, 1, 1); return err },
		"BlocksAfter":        func() error { _, err := m.BlocksAfter(ctx, 1, 1, 1); return err },
		"BlocksBetween":      func() error { _, err := m.BlocksBetween(ctx, 1, now, now); return err },
		"GasUsedBetween":     func() error { _, err := m.GasUsedBetween(ctx, 1, now, now); return err },
		"TwoTxBlocks":        func() error { _, err := m.TwoTxBlocks(ctx, 1, 1, 1); return err },
		"PruneBlocks":        func() error { _, err := m.PruneBlocks(ctx, 1, now); return err },
		"FoldBuckets":        func() error { return m.FoldBuckets(ctx, nil) },
		"Buckets":            func() error { _, err := m.Buckets(ctx, 1, "1m", now, now); return err },
		"InsertStateSample":  func() error { return m.InsertStateSample(ctx, db.StateSample{}) },
		"LatestStateSample":  func() error { _, err := m.LatestStateSample(ctx, 1, false); return err },
		"L1Samples":          func() error { _, err := m.L1Samples(ctx, 1, now, now, 0); return err },
		"PruneStateSamples":  func() error { _, err := m.PruneStateSamples(ctx, 1, now); return err },
		"InsertOwnerActions": func() error { _, err := m.InsertOwnerActions(ctx, nil); return err },
		"OwnerActions":       func() error { _, err := m.OwnerActions(ctx, 1, now, now, 0); return err },
		"InsertConstraintSet": func() error {
			_, err := m.InsertConstraintSet(ctx, db.ConstraintSet{})
			return err
		},
		"ConstraintSets":     func() error { _, err := m.ConstraintSets(ctx, 1); return err },
		"UpsertBatchReports": func() error { return m.UpsertBatchReports(ctx, nil) },
		"BatchBuckets":       func() error { _, err := m.BatchBuckets(ctx, 1, now, now, 0); return err },
		"GetState":           func() error { _, _, err := m.GetState(ctx, 1, ""); return err },
		"SetState":           func() error { return m.SetState(ctx, 1, "", "") },
		"States":             func() error { _, err := m.States(ctx, 1); return err },
		"Notify":             func() error { return m.Notify(ctx, "", "") },
	}
	for _, n := range names {
		if err := calls[n](); !errors.Is(err, ErrInjected) {
			t.Errorf("%s: expected injected failure, got %v", n, err)
		}
	}
}
