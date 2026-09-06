package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/db/dbtest"
	"github.com/tirante-dev/gascurve/internal/model"
	"github.com/tirante-dev/gascurve/internal/nitro"
	"github.com/tirante-dev/gascurve/internal/pricer"
)

func lastSnapshot(t *testing.T, store *dbtest.MemStore) model.LiveSnapshot {
	t.Helper()
	n, ok := store.LastNotification(db.ChannelLive)
	if !ok {
		t.Fatal("no live notification")
	}
	var snap model.LiveSnapshot
	if err := json.Unmarshal([]byte(n.Payload), &snap); err != nil {
		t.Fatalf("snapshot json: %v", err)
	}
	return snap
}

func TestTickFreshStartAndCatchUp(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)

	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	blocks, _ := store.RecentBlocks(ctx, 4663, 100)
	if len(blocks) != initialBlocks || blocks[0].Number != 1000 || blocks[len(blocks)-1].Number != 991 {
		t.Fatalf("fresh start stored %d blocks (%v)", len(blocks), blocks)
	}
	head := blocks[0]
	if !head.Anchored || head.Backlogs[1] != 11_194_391_810_886 || head.Backlogs[0] != 3_111_506 {
		t.Fatalf("head not anchored to the sample: %+v", head)
	}
	if blocks[1].Anchored {
		t.Fatal("only the head block is anchored")
	}
	if f.Head() != 1000 {
		t.Fatalf("Head() = %d", f.Head())
	}
	n, _ := store.NetworkByRef(ctx, "robinhood")
	if n == nil || n.HeadBlock.Int64 != 1000 || n.DisplayName != "Robinhood Chain" || !n.LastSampleAt.Valid {
		t.Fatalf("network row: %+v", n)
	}
	if v, _, _ := store.GetState(ctx, 4663, db.StateHead); v != "1000" {
		t.Fatalf("head checkpoint = %q", v)
	}
	if store.BucketCount(4663, db.Resolution1m) != 1 || store.BucketCount(4663, db.Resolution1h) != 1 {
		t.Fatal("buckets not folded")
	}
	bk, _ := store.Buckets(ctx, 4663, db.Resolution1m, baseTime, baseTime.Add(time.Hour))
	if bk[0].Blocks != 10 || bk[0].LastBlock != 1000 || bk[0].BacklogsEnd[1] != 11_194_391_810_886 || bk[0].FeesWei.Sign() <= 0 {
		t.Fatalf("bucket: %+v", bk[0])
	}
	sample, _ := store.LatestStateSample(ctx, 4663, false)
	if sample == nil || sample.BlockNumber != 1000 || sample.L1 != nil {
		t.Fatalf("sample: %+v", sample)
	}
	snap := lastSnapshot(t, store)
	if snap.ChainID != 4663 || snap.Model != model.ModelConstraints || len(snap.Constraints) != 2 || snap.Block.Number != 1000 {
		t.Fatalf("snapshot: %+v", snap)
	}
	if snap.ExponentBips != 32_425 || snap.Constraints[1].ExponentBips != 32_391 || snap.MinBaseFee != "20000000" || snap.MultiplierBips != 10_000 {
		t.Fatalf("snapshot pricer values: %+v", snap)
	}
	if snap.Prices.PerArbGasTotal != "6" || snap.GasPerSecond.S10 == 0 || snap.L1 != nil || snap.Legacy != nil {
		t.Fatalf("snapshot prices/gps: %+v", snap)
	}
	if f.Snapshot() == nil || f.Snapshot().Block.Number != 1000 {
		t.Fatal("Snapshot() not cached")
	}

	// Same head: sample only, no new blocks, but a new notification.
	before := len(store.Notifications)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if blocks, _ := store.RecentBlocks(ctx, 4663, 100); len(blocks) != initialBlocks {
		t.Fatal("sample-only tick must not add blocks")
	}
	if len(store.Notifications) != before+1 {
		t.Fatal("sample-only tick must notify")
	}

	// Head advances by 25 with a batch size of 10: three header batches.
	rpc.setHead(1025)
	rpc.headerCalls = nil
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rpc.headerCalls) != 3 || len(rpc.headerCalls[0]) != 10 || len(rpc.headerCalls[2]) != 4 || rpc.headerCalls[0][0] != 1001 {
		t.Fatalf("header batches = %v", rpc.headerCalls)
	}
	blocks, _ = store.RecentBlocks(ctx, 4663, 100)
	if len(blocks) != 35 || blocks[0].Number != 1025 {
		t.Fatalf("after catch-up: %d blocks, head %d", len(blocks), blocks[0].Number)
	}
	// Replay continuity: block 1001 drained by dt=0 from the anchored 1000
	// state, so its backlogs are the anchored ones plus its own gas.
	var b1001 db.Block
	for _, b := range blocks {
		if b.Number == 1001 {
			b1001 = b
		}
	}
	if b1001.Backlogs[0] != 3_111_506+int64(gasFor(1001)) || b1001.Anchored {
		t.Fatalf("block 1001 replay: %+v", b1001)
	}

	// Head going backwards is ignored.
	rpc.setHead(1020)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if f.Head() != 1025 {
		t.Fatal("head must not move backwards")
	}
}

func TestTickGapSkipAndParameterChange(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// Gap of 150 with batch 10 (max catch-up 100): 50 blocks are skipped.
	rpc.setHead(1150)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	blocks, _ := store.BlocksAfter(ctx, 4663, 1000, 1000)
	if len(blocks) != 100 || blocks[0].Number != 1051 {
		t.Fatalf("gap skip: %d blocks from %d", len(blocks), blocks[0].Number)
	}
	// The owner changes the constraint set: the replay restarts from the
	// sampled backlogs and the snapshot reflects the new shape.
	rpc.mu.Lock()
	rpc.constraints = []nitro.Constraint{{Target: 60_000_000, Window: 15, Backlog: 7}}
	rpc.mu.Unlock()
	rpc.setHead(1151)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	snap := lastSnapshot(t, store)
	if len(snap.Constraints) != 1 || snap.Constraints[0].Backlog != 7 {
		t.Fatalf("snapshot after change: %+v", snap)
	}
	latest, _ := store.LatestBlock(ctx, 4663)
	if len(latest.Backlogs) != 1 {
		t.Fatalf("block backlogs after change: %+v", latest)
	}
}

func TestTickLegacy(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(500)
	rpc.legacy = &nitro.LegacyParams{SpeedLimit: 7_000_000, Inertia: 102, Tolerance: 10, Backlog: 5}
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	snap := lastSnapshot(t, store)
	if snap.Model != model.ModelLegacy || snap.Legacy == nil || snap.Legacy.Backlog != 5 || len(snap.Constraints) != 0 {
		t.Fatalf("legacy snapshot: %+v", snap)
	}
	latest, _ := store.LatestBlock(ctx, 4663)
	if len(latest.Backlogs) != 1 || latest.Backlogs[0] != 5 || !latest.Anchored {
		t.Fatalf("legacy block: %+v", latest)
	}
	sample, _ := store.LatestStateSample(ctx, 4663, false)
	if sample.Legacy == nil {
		t.Fatal("legacy json missing")
	}
	// Restarting a follower resumes from the stored head and backlogs.
	rpc.setHead(505)
	f2 := newTestFollower(t, rpc, store)
	if err := f2.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if f2.Head() != 505 {
		t.Fatalf("resumed head = %d", f2.Head())
	}
	blocks, _ := store.BlocksAfter(ctx, 4663, 500, 10)
	if len(blocks) != 5 {
		t.Fatalf("resumed blocks = %d", len(blocks))
	}
}

func TestTickErrors(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)

	rpc.errs["FastSample"] = errRPC
	if err := f.Tick(ctx); !errors.Is(err, errRPC) {
		t.Fatalf("expected rpc error, got %v", err)
	}
	n, _ := store.NetworkByRef(ctx, "robinhood")
	if n == nil || !n.LastError.Valid {
		t.Fatal("error must be recorded on the network row")
	}
	delete(rpc.errs, "FastSample")

	rpc.errs["HeadersByNumbers"] = errRPC
	if err := f.Tick(ctx); !errors.Is(err, errRPC) {
		t.Fatalf("expected header error, got %v", err)
	}
	delete(rpc.errs, "HeadersByNumbers")

	for _, method := range []string{"UpsertBlocks", "FoldBuckets", "GasUsedBetween", "InsertStateSample", "UpdateNetworkHead", "SetState", "Notify"} {
		store.FailOn[method] = true
		if err := f.Tick(ctx); !errors.Is(err, dbtest.ErrInjected) {
			t.Fatalf("%s: expected injected error, got %v", method, err)
		}
		delete(store.FailOn, method)
	}
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// Errors are cleared on success.
	n, _ = store.NetworkByRef(ctx, "robinhood")
	if n.LastError.Valid {
		t.Fatal("error should clear after a good tick")
	}
	// Sample-only path with a failing store.
	store.FailOn["Notify"] = true
	if err := f.Tick(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("sample-only failure: %v", err)
	}
	delete(store.FailOn, "Notify")
	// A canceled context does not try to record the error.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	rpc.errs["FastSample"] = errRPC
	if err := f.Tick(cctx); err == nil {
		t.Fatal("expected error")
	}
	delete(rpc.errs, "FastSample")

	// Init failures.
	fresh := dbtest.New()
	fresh.FailOn["UpsertNetwork"] = true
	if err := newTestFollower(t, rpc, fresh).Tick(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("init failure: %v", err)
	}
	fresh = dbtest.New()
	fresh.FailOn["LatestBlock"] = true
	if err := newTestFollower(t, rpc, fresh).Tick(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("init latest block failure: %v", err)
	}
	fresh = dbtest.New()
	fresh.FailOn["ConstraintSets"] = true
	if err := newTestFollower(t, rpc, fresh).Tick(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("init sets failure: %v", err)
	}
	fresh = dbtest.New()
	fresh.FailOn["OwnerActions"] = true
	if err := newTestFollower(t, rpc, fresh).Tick(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("init actions failure: %v", err)
	}
}

func TestSlowDataAttachedToNextSample(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.sampleSlow(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	sample, _ := store.LatestStateSample(ctx, 4663, true)
	if sample == nil || sample.L1 == nil || sample.Accounts == nil {
		t.Fatalf("slow data not attached: %+v", sample)
	}
	var l1 model.L1
	if err := sample.L1.Unmarshal(&l1); err != nil || l1.BaseFeeEstimate != "2369608" || l1.Surplus != "-5" || l1.PerBatchGasCharge != 210_000 {
		t.Fatalf("l1 json: %+v %v", l1, err)
	}
	snap := lastSnapshot(t, store)
	if snap.L1 == nil || snap.Accounts == nil || snap.Accounts.Network.Balance != "10706" {
		t.Fatalf("snapshot slow data: %+v", snap)
	}
	// The next tick does not re-attach until the slow loop runs again.
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	latest, _ := store.LatestStateSample(ctx, 4663, false)
	if latest.L1 != nil {
		t.Fatal("l1 should only be attached once per slow tick")
	}
}

func TestFoldBuckets(t *testing.T) {
	mk := func(n uint64, ts time.Time, fee int64, gas uint64, backlogs ...int64) db.Block {
		return db.Block{ChainID: 1, Number: n, TS: ts, GasUsed: gas, BaseFee: db.WeiFromUint64(uint64(fee)), PredictedBaseFee: db.WeiFromUint64(uint64(fee + 1)), Backlogs: pq.Int64Array(backlogs), ExponentBips: int64(n)}
	}
	t0 := time.Date(2026, 9, 6, 7, 59, 58, 0, time.UTC)
	rows := []db.Block{
		mk(1, t0, 100, 10, 5, 50),
		mk(2, t0.Add(time.Second), 300, 20, 9, 40),
		mk(3, t0.Add(3*time.Second), 200, 30, 1, 60), // next minute and next hour
	}
	setID := func(n uint64) sql.NullInt64 { return sql.NullInt64{Int64: int64(n * 10), Valid: n > 1} }
	buckets := foldBuckets(rows, setID)
	if len(buckets) != 6 {
		t.Fatalf("expected 2 buckets per resolution, got %d", len(buckets))
	}
	first := buckets[0]
	if first.Resolution != db.Resolution1m || first.Blocks != 2 || first.GasUsed != 30 || first.BaseFeeMin.Int64() != 100 || first.BaseFeeMax.Int64() != 300 || first.BaseFeeAvg.Int64() != 200 {
		t.Fatalf("first bucket: %+v", first)
	}
	if first.FeesWei.Int64() != 100*10+300*20 || first.ExponentEndBips != 2 || first.BacklogsEnd[0] != 9 || first.BacklogsMax[0] != 9 || first.BacklogsMax[1] != 50 || first.LastBlock != 2 {
		t.Fatalf("first bucket aggregates: %+v", first)
	}
	if !first.ConstraintSetID.Valid || first.ConstraintSetID.Int64 != 20 {
		t.Fatalf("constraint set id: %+v", first.ConstraintSetID)
	}
	if first.ReplayErrorBips != 100 { // block 1: |101-100|*10000/100
		t.Fatalf("replay error: %d", first.ReplayErrorBips)
	}
	second := buckets[1]
	if second.Blocks != 1 || second.BucketStart != t0.Add(3*time.Second).Truncate(time.Minute) || second.ConstraintSetID.Int64 != 30 {
		t.Fatalf("second bucket: %+v", second)
	}
	hour := buckets[5]
	if hour.Resolution != db.Resolution1h || hour.BucketStart.Hour() != 8 {
		t.Fatalf("hour bucket: %+v", hour)
	}
	if foldBuckets(nil, setID) != nil {
		t.Fatal("no rows, no buckets")
	}
	zero := db.Block{BaseFee: db.NewWei(nil), PredictedBaseFee: db.WeiFromUint64(5)}
	if replayError(zero) != 0 {
		t.Fatal("zero actual fee yields zero error")
	}
	huge := db.Block{BaseFee: db.WeiFromUint64(1), PredictedBaseFee: db.NewWei(new(big.Int).Lsh(big.NewInt(1), 90))}
	if replayError(huge) != 1<<63-1 {
		t.Fatal("saturated error")
	}
}

func TestHelpers(t *testing.T) {
	s := &nitro.Sample{Constraints: []nitro.Constraint{{Target: 1, Window: 2, Backlog: 3}}, MinBaseFee: big.NewInt(9)}
	st := stateFromSample(s)
	if !sameShape(st, s) || sameShape(nil, s) || st.Backlogs()[0] != 3 {
		t.Fatal("constraint shape")
	}
	s2 := &nitro.Sample{Constraints: []nitro.Constraint{{Target: 1, Window: 3}}}
	if sameShape(st, s2) {
		t.Fatal("window change is a different shape")
	}
	leg := &nitro.Sample{Legacy: &nitro.LegacyParams{SpeedLimit: 1, Inertia: 2, Tolerance: 3, Backlog: 4}}
	lst := stateFromSample(leg)
	if !sameShape(lst, leg) || sameShape(st, leg) || sameShape(lst, s) || lst.Backlogs()[0] != 4 {
		t.Fatal("legacy shape")
	}
	if sameShape(lst, &nitro.Sample{Legacy: &nitro.LegacyParams{SpeedLimit: 2, Inertia: 2, Tolerance: 3}}) {
		t.Fatal("legacy param change")
	}
	empty := &nitro.Sample{}
	if stateFromSample(empty).Legacy != nil || sampleBacklogs(empty) != nil || sameShape(stateFromSample(empty), empty) {
		t.Fatal("empty sample")
	}
	if multiplierBips(big.NewInt(30), big.NewInt(10)) != 30_000 || multiplierBips(big.NewInt(1), nil) != 0 || multiplierBips(nil, big.NewInt(1)) != 0 || multiplierBips(big.NewInt(1), big.NewInt(0)) != 0 {
		t.Fatal("multiplierBips")
	}
	if multiplierBips(new(big.Int).Lsh(big.NewInt(1), 90), big.NewInt(1)) != 1<<63-1 {
		t.Fatal("multiplierBips saturation")
	}
	if weiString(nil) != "0" || weiString(big.NewInt(7)) != "7" {
		t.Fatal("weiString")
	}
	snap := buildSnapshot(1, s, &pricer.Result{ErrorBips: 3}, model.GasPerSecond{}, nil, nil)
	if snap.ReplayErrorBips != 3 || snap.MultiplierBips != 0 || snap.MinBaseFee != "9" {
		t.Fatalf("buildSnapshot: %+v", snap)
	}
	nilFee := &nitro.Sample{Constraints: []nitro.Constraint{{Target: 1, Window: 2, Backlog: 3}}}
	if snap := buildSnapshot(1, nilFee, &pricer.Result{}, model.GasPerSecond{}, nil, nil); snap.MinBaseFee != "0" {
		t.Fatalf("nil min fee: %+v", snap)
	}
	f := newTestFollower(t, newFakeRPC(1), dbtest.New())
	f.sets = []db.ConstraintSet{{ID: 1, EffectiveBlock: 10}, {ID: 2, EffectiveBlock: 20}}
	if id := f.setIDFor(5); id.Valid {
		t.Fatal("no set before 10")
	}
	if id := f.setIDFor(25); !id.Valid || id.Int64 != 2 {
		t.Fatal("set at 25")
	}
	f.minFeeChanges = []minFeeChange{{block: 100, fee: big.NewInt(5)}}
	if f.minFeeAt(50, big.NewInt(9)).Int64() != 9 || f.minFeeAt(150, big.NewInt(9)).Int64() != 5 || f.minFeeAt(50, nil).Sign() != 0 {
		t.Fatal("minFeeAt")
	}
	if _, err := setEntries(db.ConstraintSet{Constraints: db.JSONB(`{bad`)}); err == nil {
		t.Fatal("bad entries")
	}
	if !sameEntries([]model.ConstraintSetEntry{{Target: 1, Window: 2}}, []nitro.Constraint{{Target: 1, Window: 2}}) || sameEntries(nil, []nitro.Constraint{{}}) || sameEntries([]model.ConstraintSetEntry{{Target: 1}}, []nitro.Constraint{{Target: 2}}) {
		t.Fatal("sameEntries")
	}
	if string(entriesJSON(nil)) != "[]" {
		t.Fatal("entriesJSON nil")
	}
	if err := sleepContext(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if err := sleepContext(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepContext(cctx, time.Hour); err == nil {
		t.Fatal("expected cancellation")
	}
	nf := NewFollower(Options{Network: config.NetworkConfig{ChainID: 1}, Collector: config.CollectorConfig{HeaderBatchSize: 500}, RPC: newFakeRPC(1), Store: dbtest.New()})
	if nf.cfg.HeaderBatchSize != nitro.MaxBatch || nf.log == nil || nf.now == nil || nf.sleep == nil {
		t.Fatal("defaults")
	}
}

func TestTickUnlimitedNeverSkips(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	f.net.CallsPerSecond = 0
	if !f.unlimited() {
		t.Fatal("calls_per_second 0 is unlimited")
	}
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// The same gap that a budgeted follower skips is fetched in full: 150
	// blocks in 15 batches of 10, and the replay stays continuous.
	rpc.setHead(1150)
	rpc.headerCalls = nil
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rpc.headerCalls) != 15 || rpc.headerCalls[0][0] != 1001 || rpc.headerCalls[14][8] != 1149 {
		t.Fatalf("header batches = %d", len(rpc.headerCalls))
	}
	blocks, _ := store.BlocksAfter(ctx, 4663, 1000, 1000)
	if len(blocks) != 150 || blocks[0].Number != 1001 || blocks[0].Anchored {
		t.Fatalf("unlimited catch-up: %d blocks from %d", len(blocks), blocks[0].Number)
	}
	if blocks[0].Backlogs[0] != 3_111_506+int64(gasFor(1001)) {
		t.Fatalf("replay must continue from the anchored state: %+v", blocks[0])
	}
	// A custom catch-up bound applies on a budgeted network.
	f.net.CallsPerSecond = 4
	f.cfg.MaxCatchUpBatches = 2
	rpc.setHead(1250)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	blocks, _ = store.BlocksAfter(ctx, 4663, 1150, 1000)
	if len(blocks) != 20 || blocks[0].Number != 1231 {
		t.Fatalf("custom catch-up bound: %d blocks from %d", len(blocks), blocks[0].Number)
	}
}

func TestTickAt(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// A head event for 1005 samples state at 1005 even though the node's
	// latest is already 1008: the sample and the header agree.
	rpc.setHead(1008)
	if err := f.TickAt(ctx, 1005); err != nil {
		t.Fatal(err)
	}
	if len(rpc.sampleAt) != 1 || rpc.sampleAt[0] != 1005 || rpc.calledTimes("FastSample") != 1 {
		t.Fatalf("FastSampleAt calls = %v, FastSample calls = %d", rpc.sampleAt, rpc.calledTimes("FastSample"))
	}
	if f.Head() != 1005 {
		t.Fatalf("head = %d", f.Head())
	}
	latest, _ := store.LatestBlock(ctx, 4663)
	if latest.Number != 1005 || !latest.Anchored {
		t.Fatalf("head block: %+v", latest)
	}
	if snap := lastSnapshot(t, store); snap.Block.Number != 1005 {
		t.Fatalf("snapshot block = %d", snap.Block.Number)
	}
	// The same head again is a sample-only tick.
	before := len(store.Notifications)
	if err := f.TickAt(ctx, 1005); err != nil {
		t.Fatal(err)
	}
	if len(store.Notifications) != before+1 || f.Head() != 1005 {
		t.Fatal("repeated head should only re-sample")
	}
	// An older head is ignored, a head the node does not have yet fails.
	if err := f.TickAt(ctx, 1003); err != nil || f.Head() != 1005 {
		t.Fatalf("older head: %v head %d", err, f.Head())
	}
	if err := f.TickAt(ctx, 1010); err == nil {
		t.Fatal("head beyond the node should fail")
	}
	if err := f.TickAt(ctx, 1008); err != nil || f.Head() != 1008 {
		t.Fatalf("catch up to 1008: %v head %d", err, f.Head())
	}
}
