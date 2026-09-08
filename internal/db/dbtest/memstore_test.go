package dbtest

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"testing"
	"time"

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
	ranges := []db.MissingRange{{ChainID: 99, From: 10, To: 20, DetectedAt: base, ReplayState: db.JSONB(`{"block":9}`)}}
	if err := m.ReplaceMissingRanges(ctx, 1, ranges); err != nil {
		t.Fatal(err)
	}
	ranges[0].ReplayState[0] = '['
	gotRanges, err := m.MissingRanges(ctx, 1)
	if err != nil || len(gotRanges) != 1 || gotRanges[0].ChainID != 1 || string(gotRanges[0].ReplayState) != `{"block":9}` {
		t.Fatalf("missing ranges: %+v %v", gotRanges, err)
	}
	if err := m.ReplaceMissingRanges(ctx, 1, nil); err != nil {
		t.Fatal(err)
	}
	if gotRanges, _ := m.MissingRanges(ctx, 1); len(gotRanges) != 0 {
		t.Fatalf("missing ranges were not cleared: %+v", gotRanges)
	}
	if n, _ := m.NetworkByRef(ctx, "a"); !n.LastError.Valid {
		t.Fatal("error not set")
	}

	for i := uint64(1); i <= 5; i++ {
		txc := 3
		if i == 3 {
			txc = 2
		}
		posterGas := int64(0)
		if i == 3 {
			posterGas = 1
		}
		if err := m.UpsertBlocks(ctx, []db.Block{{ChainID: 1, Number: i, TS: base.Add(time.Duration(i) * time.Second), GasUsed: i, PosterGas: sql.NullInt64{Int64: posterGas, Valid: true}, TxCount: txc}}); err != nil {
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
	if total, compute, _ := m.GasBetween(ctx, 1, base.Add(time.Second), base.Add(3*time.Second)); total != 5 || compute == nil || *compute != 4 {
		t.Fatalf("gas = %d, compute = %v", total, compute)
	}
	if nums, _ := m.TwoTxBlocks(ctx, 1, 0, 10); len(nums) != 1 || nums[0] != 3 {
		t.Fatal("two tx")
	}
	if n, _ := m.PruneBlocks(ctx, 1, base.Add(2*time.Second)); n != 1 {
		t.Fatal("prune blocks")
	}

	b1 := db.Bucket{ChainID: 1, Resolution: "1m", BucketStart: base, Blocks: 1, GasUsed: 10, FeesWei: db.WeiFromUint64(10), BaseFeeMin: db.WeiFromUint64(10), BaseFeeAvg: db.WeiFromUint64(10), BaseFeeMax: db.WeiFromUint64(10), ExponentEndBips: 1, BaseFeeSum: db.NullWeiFromUint64(10), BacklogsEnd: db.Uint64Array{1}, BacklogsMax: db.Uint64Array{1, 5}, ReplayErrorBips: 3, LastBlock: 5}
	b2 := b1
	b2.Blocks, b2.GasUsed, b2.FeesWei = 3, 30, db.WeiFromUint64(30)
	b2.BaseFeeMin, b2.BaseFeeAvg, b2.BaseFeeMax, b2.BaseFeeSum = db.WeiFromUint64(2), db.WeiFromUint64(30), db.WeiFromUint64(40), db.NullWeiFromUint64(90)
	b2.ExponentEndBips, b2.BacklogsEnd, b2.BacklogsMax, b2.ReplayErrorBips, b2.LastBlock = 9, db.Uint64Array{7}, db.Uint64Array{3, 2, 8}, 1, 9
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
	if len(bk) != 1 || bk[0].Blocks != 5 || bk[0].GasUsed != 50 || bk[0].BaseFeeMin.Int64() != 2 || bk[0].BaseFeeMax.Int64() != 40 || bk[0].BaseFeeAvg.Int64() != 22 || bk[0].BaseFeeSum.Wei.Int64() != 110 {
		t.Fatalf("fold: %+v", bk)
	}
	if bk[0].ExponentEndBips != 9 || bk[0].BacklogsMax[2] != 8 || bk[0].BacklogsMax[1] != 5 || bk[0].ConstraintSetID.Int64 != 4 || bk[0].ReplayErrorBips != 3 || bk[0].LastBlock != 9 {
		t.Fatalf("fold end: %+v", bk[0])
	}
	if m.BucketCount(1, "1m") != 1 {
		t.Fatal("bucket count")
	}
	// Rebuilding from rows replaces the bucket with the rows' aggregate
	// (blocks 2..5 remain after the prune above), using the set in force
	// at the last block whose constraint count matches the block's
	// backlogs (the rows have none, so only an empty set applies); a window
	// without rows loses its bucket.
	_, _ = m.InsertConstraintSet(ctx, db.ConstraintSet{ChainID: 1, EffectiveBlock: 4, Source: "genesis", Constraints: db.JSONB(`[]`)})
	_, _ = m.InsertConstraintSet(ctx, db.ConstraintSet{ChainID: 1, EffectiveBlock: 5, Source: "owner_action", Constraints: db.JSONB(`[{"target":1}]`)})
	if err := m.RebuildBuckets(ctx, 1, "1m", []time.Time{base, base.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := m.RebuildBuckets(ctx, 1, "2m", nil); err == nil {
		t.Fatal("unknown resolution")
	}
	bk, _ = m.Buckets(ctx, 1, "1m", base, base.Add(2*time.Hour))
	if len(bk) != 1 || bk[0].Blocks != 4 || bk[0].GasUsed != 14 || bk[0].LastBlock != 5 || bk[0].ConstraintSetID.Int64 != 1 {
		t.Fatalf("rebuilt: %+v", bk)
	}
	if setSize(db.ConstraintSet{Constraints: db.JSONB(`{bad`)}) != -1 || setSize(db.ConstraintSet{Constraints: db.JSONB(`[1,2]`)}) != 2 {
		t.Fatal("setSize")
	}
	if b, _ := m.BlockByNumber(ctx, 1, 5); b == nil || b.Number != 5 {
		t.Fatal("BlockByNumber")
	}
	if b, _ := m.BlockByNumber(ctx, 1, 99); b != nil {
		t.Fatal("BlockByNumber missing")
	}
	removed, _ := m.DeleteBlocksAfter(ctx, 1, 3)
	if len(removed) != 2 || removed[0].Number != 4 {
		t.Fatalf("DeleteBlocksAfter: %+v", removed)
	}
	if n, _ := m.DeleteBucketsBefore(ctx, 1, base.Add(time.Minute)); n != 1 || m.BucketCount(1, "1m") != 0 {
		t.Fatal("DeleteBucketsBefore")
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
	_ = m.InsertStateSample(ctx, db.StateSample{ChainID: 1, SampledAt: base.Add(2 * time.Minute), BlockNumber: 9})
	if n, _ := m.DeleteStateSamplesAfter(ctx, 1, 5); n != 1 {
		t.Fatal("delete samples after")
	}
	if ls, _ := m.LatestStateSample(ctx, 1, false); ls.BlockNumber == 9 {
		t.Fatal("sample above the block must go")
	}

	acts := []db.OwnerAction{{ChainID: 1, TxHash: "a", LogIndex: 0, BlockNumber: 5, TS: base}, {ChainID: 1, TxHash: "a", LogIndex: 1, BlockNumber: 5, TS: base.Add(time.Hour)}}
	if n, _ := m.InsertOwnerActions(ctx, acts); n != 2 {
		t.Fatal("insert actions")
	}
	if n, _ := m.InsertOwnerActions(ctx, acts); n != 0 {
		t.Fatal("duplicate actions")
	}
	enriched := acts[0]
	enriched.TxIndex = sql.NullInt64{Int64: 7, Valid: true}
	if n, _ := m.InsertOwnerActions(ctx, []db.OwnerAction{enriched}); n != 0 || !m.ActionRows["1/a/0"].TxIndex.Valid || m.ActionRows["1/a/0"].TxIndex.Int64 != 7 {
		t.Fatal("duplicate action transaction index enrichment")
	}
	if as, _ := m.OwnerActions(ctx, 1, time.Time{}, time.Time{}, 0); len(as) != 2 || as[0].LogIndex != 1 {
		t.Fatal("actions order")
	}
	if as, _ := m.OwnerActions(ctx, 1, base.Add(-time.Minute), base.Add(time.Minute), 1); len(as) != 1 || as[0].LogIndex != 0 {
		t.Fatal("actions range")
	}
	if as, _ := m.OwnerActionsSince(ctx, 1, 5); len(as) != 2 || as[0].LogIndex != 0 || as[1].LogIndex != 1 {
		t.Fatal("actions since")
	}
	if as, _ := m.OwnerActionsSince(ctx, 1, 6); len(as) != 0 {
		t.Fatal("actions since beyond")
	}
	id, _ := m.InsertConstraintSet(ctx, db.ConstraintSet{ChainID: 1, EffectiveBlock: 1, Source: "genesis"})
	id2, _ := m.InsertConstraintSet(ctx, db.ConstraintSet{ChainID: 1, EffectiveBlock: 1, Source: "genesis"})
	id3, _ := m.InsertConstraintSet(ctx, db.ConstraintSet{ChainID: 1, EffectiveBlock: 0, Source: "observed"})
	if id != id2 || id3 == id {
		t.Fatal("constraint set ids")
	}
	if cs, _ := m.ConstraintSets(ctx, 1); len(cs) != 4 || cs[0].EffectiveBlock != 0 || cs[2].EffectiveBlock != 4 {
		t.Fatalf("constraint sets: %+v", cs)
	}
	// UpdateConstraintSet rewrites a row in place, keeping its id; an
	// unknown id is a no-op.
	if err := m.UpdateConstraintSet(ctx, db.ConstraintSet{ID: id3, ChainID: 1, EffectiveBlock: 2, Source: "owner_action", Constraints: db.JSONB(`[]`)}); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateConstraintSet(ctx, db.ConstraintSet{ID: 99, ChainID: 1}); err != nil {
		t.Fatal(err)
	}
	if cs, _ := m.ConstraintSets(ctx, 1); len(cs) != 4 || cs[1].ID != id3 || cs[1].EffectiveBlock != 2 || cs[1].Source != "owner_action" {
		t.Fatalf("updated set: %+v", cs)
	}

	reps := []db.BatchReport{
		{ChainID: 1, BlockNumber: 1, BatchTS: base, GasSpent: 10, WeiSpent: db.WeiFromUint64(100), L1BaseFee: db.WeiFromUint64(10), CalldataLen: 5, CostCalculationVersion: 1},
		{ChainID: 1, BlockNumber: 2, BatchTS: base.Add(10 * time.Second), GasSpent: 20, WeiSpent: db.WeiFromUint64(200), L1BaseFee: db.WeiFromUint64(30), CalldataLen: 5, CostCalculationVersion: 1},
		{ChainID: 1, BlockNumber: 3, BatchTS: base.Add(20 * time.Second), GasSpent: 999, WeiSpent: db.WeiFromUint64(999), L1BaseFee: db.WeiFromUint64(1), CalldataLen: 5},
	}
	_ = m.UpsertBatchReports(ctx, reps)
	bb, _ := m.BatchBuckets(ctx, 1, base.Add(-time.Hour), base.Add(time.Hour), time.Minute)
	if len(bb) != 1 || bb[0].Batches != 2 || bb[0].GasSpent != 30 || bb[0].WeiSpent.Int64() != 300 || bb[0].L1BaseFeeAvg.Int64() != 20 || bb[0].CalldataBytes != 10 {
		t.Fatalf("batch buckets: %+v", bb)
	}
	if rs, _ := m.BatchReports(ctx, 1, base, base.Add(time.Hour)); len(rs) != 2 || rs[0].BlockNumber != 1 || rs[1].BlockNumber != 2 {
		t.Fatalf("batch reports: %+v", rs)
	}
	// RewindAfter drops actions, sets and reports above a block.
	if err := m.RewindAfter(ctx, 1, 1); err != nil {
		t.Fatal(err)
	}
	if as, _ := m.OwnerActions(ctx, 1, time.Time{}, time.Time{}, 0); len(as) != 0 {
		t.Fatal("actions after rewind")
	}
	if cs, _ := m.ConstraintSets(ctx, 1); len(cs) != 1 || cs[0].EffectiveBlock != 1 {
		t.Fatalf("sets after rewind: %+v", cs)
	}
	if rs, _ := m.BatchReports(ctx, 1, base, base.Add(time.Hour)); len(rs) != 1 {
		t.Fatal("reports after rewind")
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
	_ = m.SetState(ctx, 1, "gone", "x")
	if err := m.DeleteState(ctx, 1, "gone"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := m.GetState(ctx, 1, "gone"); ok {
		t.Fatal("deleted state")
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
	if err := m.WithSnapshotTx(ctx, func(s db.Store) error { _, _, err := s.GetState(ctx, 1, "t"); return err }); err != nil {
		t.Fatal(err)
	}
	// Chain transactions nest and serialize per chain: a second one on the
	// same chain waits for the first, another chain does not.
	entered := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = m.WithChainTx(ctx, 1, func(s db.Store) error {
			close(entered)
			<-release
			return s.WithChainTx(ctx, 1, func(inner db.Store) error {
				return inner.WithTx(ctx, func(in db.Store) error { return in.SetState(ctx, 1, "chain", "1") })
			})
		})
	}()
	<-entered
	other := make(chan struct{})
	go func() {
		_ = m.WithChainTx(ctx, 2, func(db.Store) error { return nil })
		close(other)
	}()
	select {
	case <-other:
	case <-time.After(5 * time.Second):
		t.Fatal("another chain must not wait")
	}
	same := make(chan struct{})
	go func() {
		_ = m.WithChainTx(ctx, 1, func(db.Store) error { return nil })
		close(same)
	}()
	select {
	case <-same:
		t.Fatal("the same chain must wait for the open transaction")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-same:
	case <-time.After(5 * time.Second):
		t.Fatal("chain transaction not released")
	}
	if v, _, _ := m.GetState(ctx, 1, "chain"); v != "1" {
		t.Fatal("nested chain write")
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
	names := []string{"WithTx", "WithChainTx", "WithSnapshotTx", "DeleteStateSamplesAfter", "MissingRanges", "ReplaceMissingRanges", "UpdateConstraintSet", "DeleteState", "UpsertNetwork", "Networks", "NetworkByRef", "UpdateNetworkHead", "SetNetworkError", "UpsertBlocks", "BlockByNumber", "DeleteBlocksAfter", "LatestBlock", "OldestBlock", "RecentBlocks", "BlocksAfter", "BlocksBetween", "GasBetween", "TwoTxBlocks", "PruneBlocks", "FoldBuckets", "RebuildBuckets", "DiscardBucketsBelowFrontier", "BelowFrontier", "DeleteBucketsBefore", "Buckets", "InsertStateSample", "LatestStateSample", "L1Samples", "PruneStateSamples", "InsertOwnerActions", "OwnerActions", "OwnerActionsSince", "RewindAfter", "InsertConstraintSet", "ConstraintSets", "UpsertBatchReports", "BatchReports", "BatchBuckets", "GetState", "SetState", "States", "Notify"}
	for _, n := range names {
		m.FailOn[n] = true
	}
	now := time.Now()
	calls := map[string]func() error{
		"WithTx":         func() error { return m.WithTx(ctx, func(db.Store) error { return nil }) },
		"WithChainTx":    func() error { return m.WithChainTx(ctx, 1, func(db.Store) error { return nil }) },
		"WithSnapshotTx": func() error { return m.WithSnapshotTx(ctx, func(db.Store) error { return nil }) },
		"DeleteState":    func() error { return m.DeleteState(ctx, 1, "") },
		"DeleteStateSamplesAfter": func() error {
			_, err := m.DeleteStateSamplesAfter(ctx, 1, 1)
			return err
		},
		"MissingRanges": func() error {
			_, err := m.MissingRanges(ctx, 1)
			return err
		},
		"ReplaceMissingRanges":        func() error { return m.ReplaceMissingRanges(ctx, 1, nil) },
		"UpdateConstraintSet":         func() error { return m.UpdateConstraintSet(ctx, db.ConstraintSet{}) },
		"UpsertNetwork":               func() error { return m.UpsertNetwork(ctx, db.Network{}) },
		"Networks":                    func() error { _, err := m.Networks(ctx); return err },
		"NetworkByRef":                func() error { _, err := m.NetworkByRef(ctx, ""); return err },
		"UpdateNetworkHead":           func() error { return m.UpdateNetworkHead(ctx, 1, 1, now, now) },
		"SetNetworkError":             func() error { return m.SetNetworkError(ctx, 1, "") },
		"UpsertBlocks":                func() error { return m.UpsertBlocks(ctx, nil) },
		"BlockByNumber":               func() error { _, err := m.BlockByNumber(ctx, 1, 1); return err },
		"DeleteBlocksAfter":           func() error { _, err := m.DeleteBlocksAfter(ctx, 1, 1); return err },
		"RebuildBuckets":              func() error { return m.RebuildBuckets(ctx, 1, "1m", nil) },
		"DiscardBucketsBelowFrontier": func() error { return m.DiscardBucketsBelowFrontier(ctx, 1, "1m", nil) },
		"BelowFrontier":               func() error { _, err := m.BelowFrontier(ctx, 1, nil); return err },
		"DeleteBucketsBefore": func() error {
			_, err := m.DeleteBucketsBefore(ctx, 1, now)
			return err
		},
		"OwnerActionsSince":  func() error { _, err := m.OwnerActionsSince(ctx, 1, 1); return err },
		"RewindAfter":        func() error { return m.RewindAfter(ctx, 1, 1) },
		"BatchReports":       func() error { _, err := m.BatchReports(ctx, 1, now, now); return err },
		"LatestBlock":        func() error { _, err := m.LatestBlock(ctx, 1); return err },
		"OldestBlock":        func() error { _, err := m.OldestBlock(ctx, 1); return err },
		"RecentBlocks":       func() error { _, err := m.RecentBlocks(ctx, 1, 1); return err },
		"BlocksAfter":        func() error { _, err := m.BlocksAfter(ctx, 1, 1, 1); return err },
		"BlocksBetween":      func() error { _, err := m.BlocksBetween(ctx, 1, now, now); return err },
		"GasBetween":         func() error { _, _, err := m.GasBetween(ctx, 1, now, now); return err },
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

// TestMemStoreNesting: the in-memory store mirrors the Postgres nesting
// rules, so a unit test that nests transactions fails the same way
// production would rather than silently getting the outer one.
func TestMemStoreNesting(t *testing.T) {
	ctx := context.Background()
	m := New()
	// The same chain reuses, a higher one adds its lock, and both are then
	// reused.
	err := m.WithChainTx(ctx, 10, func(s db.Store) error {
		if err := s.WithChainTx(ctx, 10, func(inner db.Store) error { return inner.SetState(ctx, 10, "a", "1") }); err != nil {
			return err
		}
		return s.WithChainTx(ctx, 20, func(inner db.Store) error {
			return inner.WithChainTx(ctx, 10, func(x db.Store) error { return x.SetState(ctx, 20, "b", "2") })
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if v, _, _ := m.GetState(ctx, 20, "b"); v != "2" {
		t.Fatalf("nested write: %q", v)
	}
	// A lower chain id after a higher one inverts the lock order.
	err = m.WithChainTx(ctx, 20, func(s db.Store) error {
		return s.WithChainTx(ctx, 10, func(db.Store) error { return nil })
	})
	if !errors.Is(err, db.ErrLockOrder) {
		t.Fatalf("lock order: %v", err)
	}
	// A snapshot inside a writing transaction is refused.
	err = m.WithChainTx(ctx, 10, func(s db.Store) error {
		return s.WithSnapshotTx(ctx, func(db.Store) error { return nil })
	})
	if !errors.Is(err, db.ErrNestedSnapshot) {
		t.Fatalf("nested snapshot: %v", err)
	}
	// A plain transaction inside a chain one reuses it.
	if err := m.WithChainTx(ctx, 10, func(s db.Store) error {
		return s.WithTx(ctx, func(inner db.Store) error { return inner.SetState(ctx, 10, "c", "3") })
	}); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := m.GetState(ctx, 10, "c"); v != "3" {
		t.Fatalf("nested plain write: %q", v)
	}
	// A top-level snapshot still runs.
	if err := m.WithSnapshotTx(ctx, func(s db.Store) error { _, _, err := s.GetState(ctx, 10, "c"); return err }); err != nil {
		t.Fatal(err)
	}
}

// The store refuses to rebuild a window starting below the recorded prune
// frontier, the same floor Postgres applies: rows there have been deleted,
// and recomputing the window would sum only what survived. A window at or
// above the frontier rebuilds as before, and an absent or unreadable frontier
// constrains nothing.
func TestMemStoreRebuildBucketsHonoursThePruneFrontier(t *testing.T) {
	ctx := context.Background()
	m := New()
	base := time.Date(2026, 9, 6, 7, 0, 0, 0, time.UTC)
	rows := []db.Block{
		{ChainID: 1, Number: 1, TS: base, GasUsed: 10, BaseFee: db.WeiFromUint64(1), PredictedBaseFee: db.NullWeiFromUint64(1)},
		{ChainID: 1, Number: 2, TS: base.Add(time.Minute), GasUsed: 20, BaseFee: db.WeiFromUint64(1), PredictedBaseFee: db.NullWeiFromUint64(1)},
	}
	if err := m.UpsertBlocks(ctx, rows); err != nil {
		t.Fatal(err)
	}
	starts := []time.Time{base, base.Add(time.Minute)}
	// The frontier sits on the second window: the first has lost rows.
	if err := m.SetState(ctx, 1, db.StatePruneFrontier, base.Add(time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := m.RebuildBuckets(ctx, 1, "1m", starts); err != nil {
		t.Fatal(err)
	}
	bk, _ := m.Buckets(ctx, 1, "1m", base, base.Add(time.Hour))
	if len(bk) != 1 || !bk[0].BucketStart.Equal(base.Add(time.Minute)) {
		t.Fatalf("rebuilt below the frontier: %+v", bk)
	}
	// Remove the frontier and both windows rebuild.
	if err := m.DeleteState(ctx, 1, db.StatePruneFrontier); err != nil {
		t.Fatal(err)
	}
	if err := m.RebuildBuckets(ctx, 1, "1m", starts); err != nil {
		t.Fatal(err)
	}
	if bk, _ = m.Buckets(ctx, 1, "1m", base, base.Add(time.Hour)); len(bk) != 2 {
		t.Fatalf("without a frontier: %d buckets, want 2", len(bk))
	}
	// One that will not parse is no frontier at all.
	if err := m.SetState(ctx, 1, db.StatePruneFrontier, "not a time"); err != nil {
		t.Fatal(err)
	}
	if err := m.RebuildBuckets(ctx, 1, "1m", starts); err != nil {
		t.Fatal(err)
	}
	if bk, _ = m.Buckets(ctx, 1, "1m", base, base.Add(time.Hour)); len(bk) != 2 {
		t.Fatalf("with an unreadable frontier: %d buckets, want 2", len(bk))
	}
}

// DiscardBucketsBelowFrontier removes exactly the starts RebuildBuckets
// declines: those below the recorded frontier. Above it, or with no
// frontier, nothing is touched.
func TestMemStoreDiscardBucketsBelowFrontier(t *testing.T) {
	ctx := context.Background()
	m := New()
	base := time.Date(2026, 9, 6, 7, 0, 0, 0, time.UTC)
	starts := []time.Time{base, base.Add(time.Minute)}
	for _, s := range starts {
		m.BucketRows[bucketKey(1, "1m", s)] = db.Bucket{ChainID: 1, Resolution: "1m", BucketStart: s, Blocks: 1}
	}
	// No frontier: nothing lies below it.
	if err := m.DiscardBucketsBelowFrontier(ctx, 1, "1m", starts); err != nil {
		t.Fatal(err)
	}
	if bk, _ := m.Buckets(ctx, 1, "1m", base, base.Add(time.Hour)); len(bk) != 2 {
		t.Fatalf("discarded with no frontier: %d left", len(bk))
	}
	// Frontier on the second start: only the first is below it.
	if err := m.SetState(ctx, 1, db.StatePruneFrontier, base.Add(time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := m.DiscardBucketsBelowFrontier(ctx, 1, "1m", starts); err != nil {
		t.Fatal(err)
	}
	bk, _ := m.Buckets(ctx, 1, "1m", base, base.Add(time.Hour))
	if len(bk) != 1 || !bk[0].BucketStart.Equal(base.Add(time.Minute)) {
		t.Fatalf("discard below the frontier: %+v", bk)
	}
	if err := m.DiscardBucketsBelowFrontier(ctx, 1, "2m", starts); err == nil {
		t.Fatal("unknown resolution")
	}
}

// SetPosterGas refuses a number or value past the BIGINT range, as the Postgres store does, rather
// than wrapping it negative: a test store that accepted it could pass on a value production rejects.
func TestMemStoreSetPosterGasRefusesValuesTheColumnCannotHold(t *testing.T) {
	ctx := context.Background()
	m := New()
	if err := m.UpsertBlocks(ctx, []db.Block{{ChainID: 1, Number: 7, GasUsed: 100}}); err != nil {
		t.Fatal(err)
	}
	for name, gas := range map[string]map[uint64]uint64{
		"a block number past the column": {math.MaxInt64 + 1: 10},
		"a gas value past the column":    {7: math.MaxInt64 + 1},
	} {
		if err := m.SetPosterGas(ctx, 1, gas); err == nil {
			t.Fatalf("%s: accepted a value the column cannot hold", name)
		}
	}
	// And within range it records, subject to the row's own gas total.
	if err := m.SetPosterGas(ctx, 1, map[uint64]uint64{7: 40}); err != nil {
		t.Fatal(err)
	}
	if b, _ := m.BlockByNumber(ctx, 1, 7); b == nil || !b.PosterGas.Valid || b.PosterGas.Int64 != 40 {
		t.Fatalf("in-range poster gas not recorded: %+v", b)
	}
}

// The memory store measures a spread exactly as the SQL does: whole units only, one unrated unit
// voiding the window it falls in, and per-second units read from blocks against per-bucket ones read
// from the stored resolution of that width.
func TestMemStoreComputeRateSpread(t *testing.T) {
	ctx := context.Background()
	m := New()
	base := time.Date(2026, 9, 6, 7, 0, 0, 0, time.UTC)
	block := func(n uint64, ts time.Time, gas uint64, poster sql.NullInt64) db.Block {
		return db.Block{ChainID: 1, Number: n, TS: ts, GasUsed: gas, PosterGas: poster, BaseFee: db.WeiFromUint64(1)}
	}
	known := func(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }
	if err := m.UpsertBlocks(ctx, []db.Block{
		block(1, base, 100, known(10)),
		block(2, base, 250, known(40)),
		block(3, base.Add(time.Second), 50, known(5)),
		block(4, base.Add(59*time.Second), 500, known(0)),
		block(5, base.Add(time.Minute), 100, known(10)),
		block(6, base.Add(61*time.Second), 100, sql.NullInt64{}),
		// Poster gas the column would refuse is not authoritative here either.
		block(7, base.Add(2*time.Minute), 100, known(400)),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := m.ComputeRateSpread(ctx, 1, base, base.Add(3*time.Minute), time.Minute, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Start.Equal(base) || got[0].MinRate != 45 || got[0].MaxRate != 500 {
		t.Fatalf("per-second spread: %+v", got)
	}

	for i, compute := range []uint64{600, 120, 6_000} {
		start := base.Add(time.Duration(i) * time.Minute)
		m.BucketRows[bucketKey(1, "1m", start)] = db.Bucket{ChainID: 1, Resolution: "1m", BucketStart: start, Blocks: 1, GasUsed: compute + 7, PosterGas: known(7)}
	}
	unknown := base.Add(20 * time.Minute)
	m.BucketRows[bucketKey(1, "1m", unknown)] = db.Bucket{ChainID: 1, Resolution: "1m", BucketStart: unknown, Blocks: 1, GasUsed: 900}
	spread, err := m.ComputeRateSpread(ctx, 1, base, base.Add(30*time.Minute), 15*time.Minute, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(spread) != 1 || !spread[0].Start.Equal(base) || spread[0].MinRate != 2 || spread[0].MaxRate != 100 {
		t.Fatalf("per-minute spread: %+v", spread)
	}
	if _, err := m.ComputeRateSpread(ctx, 1, base, base.Add(time.Hour), time.Hour, 7*time.Minute); err == nil {
		t.Fatal("a unit no stored resolution is wide must be refused")
	}
	if rows, err := m.ComputeRateSpread(ctx, 1, base, base.Add(time.Hour), time.Minute, time.Minute); err != nil || rows != nil {
		t.Fatalf("unit as wide as the step: %+v %v", rows, err)
	}
	m.FailOn["ComputeRateSpread"] = true
	if _, err := m.ComputeRateSpread(ctx, 1, base, base.Add(time.Hour), time.Minute, time.Second); err == nil {
		t.Fatal("a failing store must report it")
	}
}
