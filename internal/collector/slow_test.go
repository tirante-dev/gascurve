package collector

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/db/dbtest"
	"github.com/tirante-dev/gascurve/internal/model"
	"github.com/tirante-dev/gascurve/internal/nitro"
)

func TestSlowTickOwnerActions(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(250_000)
	rpc.logs = fixtureLogs(t)
	// Move the constraint-changing logs into the fake chain's range.
	for i := range rpc.logs {
		if rpc.logs[i].BlockNumber > 250_000 {
			rpc.logs[i].BlockNumber = 100_000 + uint64(i)
		}
	}
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	// Three chunks of 100k blocks starting at 0.
	if len(rpc.logRanges) != 3 || rpc.logRanges[0] != [2]uint64{0, 99_999} || rpc.logRanges[2] != [2]uint64{200_000, 250_000} {
		t.Fatalf("log ranges = %v", rpc.logRanges)
	}
	actions, _ := store.OwnerActions(ctx, 4663, time.Time{}, time.Time{}, 0)
	if len(actions) != 16 {
		t.Fatalf("owner actions = %d", len(actions))
	}
	for _, a := range actions {
		if a.TS.IsZero() || a.TS.Unix() != int64(tsFor(a.BlockNumber)) {
			t.Fatalf("timestamp resolved via header: %+v", a)
		}
	}
	sets, _ := store.ConstraintSets(ctx, 4663)
	if len(sets) != 6 || sets[0].Source != model.SourceGenesis || sets[0].EffectiveBlock != 28 || sets[5].Source != model.SourceOwnerAction {
		t.Fatalf("constraint sets: %+v", sets)
	}
	var entries []model.ConstraintSetEntry
	if err := sets[5].Constraints.Unmarshal(&entries); err != nil || len(entries) != 2 || entries[1].StartingBacklog != 9_989_222_400_000 {
		t.Fatalf("latest set entries: %+v %v", entries, err)
	}
	if v, _, _ := store.GetState(ctx, 4663, db.StateOwnerLogCursor); v != "250000" {
		t.Fatalf("owner cursor = %q", v)
	}
	if n, ok := store.LastNotification(db.ChannelOwnerAction); !ok || !strings.Contains(n.Payload, `"method":"setGasPricingConstraints"`) {
		t.Fatalf("owner action notification: %+v", n)
	}
	var notif model.OwnerActionNotification
	n, _ := store.LastNotification(db.ChannelOwnerAction)
	if err := json.Unmarshal([]byte(n.Payload), &notif); err != nil || notif.ChainID != 4663 || notif.Action.Selector != "0xcc0d556a" {
		t.Fatalf("notification shape: %+v %v", notif, err)
	}
	// The live set equals the latest known one: no observed row.
	if sets, _ := store.ConstraintSets(ctx, 4663); len(sets) != 6 {
		t.Fatal("observed set must not be inserted when the live set matches")
	}
	// Min fee changes are cached for the backfill.
	if len(f.minFeeChanges) != 1 || f.minFeeChanges[0].block != 174150 || f.minFeeChanges[0].fee.Int64() != 20_000_000 {
		t.Fatalf("min fee changes: %+v", f.minFeeChanges)
	}
	// Second run: only the new range is scanned, nothing new inserted.
	rpc.logRanges = nil
	rpc.setHead(250_010)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rpc.logRanges) != 1 || rpc.logRanges[0] != [2]uint64{250_001, 250_010} {
		t.Fatalf("incremental log ranges = %v", rpc.logRanges)
	}
	if v, _, _ := store.GetState(ctx, 4663, db.StateArbOSVersion); v != "61" {
		t.Fatalf("arbos version = %q", v)
	}
	if v, _, _ := store.GetState(ctx, 4663, db.StateRateLimitEvents); v != "0" {
		t.Fatalf("rate limit events = %q", v)
	}

	// Observed set: the live constraints differ from the latest known.
	rpc.mu.Lock()
	rpc.constraints = []nitro.Constraint{{Target: 50_000_000, Window: 15, Backlog: 1}, {Target: 40_000_000, Window: 86_400, Backlog: 2}}
	rpc.mu.Unlock()
	f2 := newTestFollower(t, rpc, store)
	if err := f2.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f2.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	sets, _ = store.ConstraintSets(ctx, 4663)
	if len(sets) != 7 || sets[6].Source != model.SourceObserved || sets[6].EffectiveBlock != 250_010 {
		t.Fatalf("observed set: %+v", sets)
	}
	if err := sets[6].Constraints.Unmarshal(&entries); err != nil || entries[0].Target != 50_000_000 || entries[1].StartingBacklog != 2 {
		t.Fatalf("observed entries: %+v", entries)
	}
	// Only once per process.
	if err := f2.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	if sets, _ := store.ConstraintSets(ctx, 4663); len(sets) != 7 {
		t.Fatal("observed set inserted twice")
	}
	// ArbOS upgrade is noticed without failing.
	rpc.arbos = 62
	if err := f2.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := store.GetState(ctx, 4663, db.StateArbOSVersion); v != "62" {
		t.Fatalf("arbos version = %q", v)
	}
}

func TestSlowTickLargeChainAndLegacy(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(120_000_000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	// Without a fast tick the head comes from eth_blockNumber.
	if err := f.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	if rpc.calledTimes("BlockNumber") != 1 || rpc.logRanges[0][0] != 70_000_000 {
		t.Fatalf("large chain scan start: %v", rpc.logRanges)
	}
	// Legacy chains never record observed sets.
	rpc2 := newFakeRPC(1000)
	rpc2.legacy = &nitro.LegacyParams{SpeedLimit: 7_000_000, Inertia: 102, Tolerance: 10}
	store2 := dbtest.New()
	f2 := newTestFollower(t, rpc2, store2)
	if err := f2.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f2.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	if sets, _ := store2.ConstraintSets(ctx, 4663); len(sets) != 0 {
		t.Fatal("legacy chain should have no sets")
	}
	// A constraints chain with no owner actions at all records an observed set.
	rpc3 := newFakeRPC(1000)
	store3 := dbtest.New()
	f3 := newTestFollower(t, rpc3, store3)
	if err := f3.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f3.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	sets, _ := store3.ConstraintSets(ctx, 4663)
	if len(sets) != 1 || sets[0].Source != model.SourceObserved {
		t.Fatalf("observed set on a chain without actions: %+v", sets)
	}
}

func TestSlowTickBatchReportsAndPrune(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	rpc.txCount = func(n uint64) int {
		if n == 995 || n == 998 {
			return 2
		}
		return 3
	}
	report := nitro.BatchPostingReport{Version: 2, BatchTimestamp: uint64(baseTime.Unix()), Poster: "0xdaa5260800000000000000000000000000000000", BatchNumber: 201_900,
		CalldataLen: 137, CalldataNonZeros: 113, ExtraGas: 27_132, L1BaseFee: big.NewInt(73_500_000)}
	rpc.fullBlocks[995] = nitro.Block{Header: rpc.header(995), Txs: []nitro.Tx{
		{Hash: "0x1", Type: nitro.InternalTxType, To: nitro.ArbosAddress, Input: nitro.EncodeStartBlock(nitro.StartBlock{L1BaseFee: big.NewInt(0)})},
		{Hash: "0x2", Type: nitro.InternalTxType, To: nitro.ArbosAddress, Input: nitro.EncodeBatchPostingReport(report)},
	}}
	// Block 998 has two transactions but no report (a normal user tx).
	rpc.fullBlocks[998] = nitro.Block{Header: rpc.header(998), Txs: []nitro.Tx{
		{Hash: "0x1", Type: nitro.InternalTxType, To: nitro.ArbosAddress, Input: nitro.EncodeStartBlock(nitro.StartBlock{L1BaseFee: big.NewInt(0)})},
		{Hash: "0x3", Type: 2, To: "0xdead", Input: []byte{1, 2, 3, 4}},
	}}
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(store.ReportRows) != 1 {
		t.Fatalf("batch reports = %d", len(store.ReportRows))
	}
	r := store.ReportRows["4663/995"]
	if r.BatchNumber != 201_900 || r.GasSpent != 29_036 || r.WeiSpent.Int64() != 73_500_000*29_036 || r.CalldataNonzero != 113 || r.BatchTS.Unix() != baseTime.Unix() {
		t.Fatalf("report row: %+v", r)
	}
	if v, _, _ := store.GetState(ctx, 4663, db.StateBatchScanCursor); v != "998" {
		t.Fatalf("batch cursor = %q", v)
	}
	if rpc.calledTimes("BlocksWithTxs") != 1 {
		t.Fatalf("BlocksWithTxs calls = %d", rpc.calledTimes("BlocksWithTxs"))
	}
	// Nothing new: no fetch.
	if err := f.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	if rpc.calledTimes("BlocksWithTxs") != 1 {
		t.Fatal("no new two-tx blocks should mean no fetch")
	}
	// Pruning removes blocks and samples older than the retention.
	f.cfg.BlockRetention = time.Nanosecond
	f.cfg.SampleRetention = time.Nanosecond
	if err := f.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	// Only the head block, whose timestamp equals now, may survive.
	if bs, _ := store.RecentBlocks(ctx, 4663, 100); len(bs) > 1 {
		t.Fatalf("blocks not pruned: %d", len(bs))
	}
	if s, _ := store.LatestStateSample(ctx, 4663, false); s != nil {
		t.Fatal("samples not pruned")
	}
	// Without any stored block the scan is a no-op.
	delete(store.StateRows, "4663/"+db.StateBatchScanCursor)
	if err := f.scanBatchReports(ctx); err != nil {
		t.Fatal(err)
	}
	// Rate limit stats are persisted.
	rpc.stats = nitro.Stats{RateLimitEvents: 3, Last429At: baseTime}
	if err := f.persistStats(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := store.GetState(ctx, 4663, db.StateRateLimitEvents); v != "3" {
		t.Fatalf("rate limit events = %q", v)
	}
	if v, _, _ := store.GetState(ctx, 4663, db.StateLast429At); v != baseTime.Format(time.RFC3339) {
		t.Fatalf("last 429 = %q", v)
	}
}

func TestSlowTickErrors(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	rpc.logs = fixtureLogs(t)[:2]
	rpc.txCount = func(uint64) int { return 2 }
	rpc.fullBlocks[995] = nitro.Block{Header: rpc.header(995), Txs: []nitro.Tx{
		{Hash: "0x2", Type: nitro.InternalTxType, To: nitro.ArbosAddress, Input: nitro.EncodeBatchPostingReport(nitro.BatchPostingReport{Version: 2, L1BaseFee: big.NewInt(1)})},
	}}
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"L1Sample", "FeeAccounts", "ArbOSVersion", "OwnerActsLogs", "HeaderByNumber", "BlocksWithTxs"} {
		// Reset the cursors so every step has work to do.
		delete(store.StateRows, "4663/"+db.StateOwnerLogCursor)
		delete(store.StateRows, "4663/"+db.StateBatchScanCursor)
		rpc.errs[method] = errRPC
		err := f.SlowTick(ctx)
		if !errors.Is(err, errRPC) {
			t.Fatalf("%s: expected rpc error, got %v", method, err)
		}
		delete(rpc.errs, method)
	}
	for _, method := range []string{"GetState", "SetState", "InsertOwnerActions", "InsertConstraintSet", "TwoTxBlocks", "UpsertBatchReports", "PruneBlocks", "PruneStateSamples", "OldestBlock"} {
		fresh := dbtest.New()
		fresh.FailOn[method] = true
		ff := newTestFollower(t, rpc, fresh)
		if method != "SetState" && method != "GetState" {
			if err := ff.Tick(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if method == "OldestBlock" {
			// Only reached without a cursor and with stored blocks; the
			// tick above stored blocks, the scan then asks for the oldest.
			delete(fresh.StateRows, "4663/"+db.StateBatchScanCursor)
		}
		if err := ff.SlowTick(ctx); !errors.Is(err, dbtest.ErrInjected) {
			t.Fatalf("%s: expected injected error, got %v", method, err)
		}
	}
	// Corrupt cursors are reported.
	_ = store.SetState(ctx, 4663, db.StateOwnerLogCursor, "x")
	_ = store.SetState(ctx, 4663, db.StateBatchScanCursor, "y")
	err := f.SlowTick(ctx)
	if err == nil || !strings.Contains(err.Error(), "owner cursor") || !strings.Contains(err.Error(), "batch cursor") {
		t.Fatalf("corrupt cursors: %v", err)
	}
	// Undecodable logs are skipped, not fatal.
	_ = store.SetState(ctx, 4663, db.StateOwnerLogCursor, "0")
	_ = store.SetState(ctx, 4663, db.StateBatchScanCursor, "1000")
	rpc.logs = []nitro.Log{{Topics: []string{"0x1"}, BlockNumber: 5}}
	if err := f.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	// Canceled context stops the loop early.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := f.SlowTick(cctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
	// Init failure surfaces.
	bad := dbtest.New()
	bad.FailOn["UpsertNetwork"] = true
	if err := newTestFollower(t, rpc, bad).SlowTick(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("init failure: %v", err)
	}
	// Observed check with an unreadable latest set.
	f.mu.Lock()
	f.sets = []db.ConstraintSet{{ID: 1, Constraints: db.JSONB(`{bad`)}}
	f.observedChecked = false
	f.ownerScanDone = true
	err = f.checkObservedLocked(ctx)
	f.mu.Unlock()
	if err == nil {
		t.Fatal("expected entries error")
	}
	f.mu.Lock()
	f.sets = nil
	store.FailOn["InsertConstraintSet"] = true
	err = f.checkObservedLocked(ctx)
	f.mu.Unlock()
	if !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("observed insert failure: %v", err)
	}
	delete(store.FailOn, "InsertConstraintSet")
}

func TestBatchReportOf(t *testing.T) {
	b := nitro.Block{Txs: []nitro.Tx{
		{Type: 2, To: nitro.ArbosAddress, Input: []byte{1}},
		{Type: nitro.InternalTxType, To: nitro.ArbosAddress, Input: []byte{1, 2}},
		{Type: nitro.InternalTxType, To: nitro.ArbosAddress, Input: nitro.EncodeStartBlock(nitro.StartBlock{L1BaseFee: big.NewInt(1)})},
	}}
	if batchReportOf(1, b) != nil {
		t.Fatal("no report expected")
	}
	b.Txs = append(b.Txs, nitro.Tx{Type: nitro.InternalTxType, To: nitro.ArbosAddress, Input: nitro.EncodeBatchPostingReport(nitro.BatchPostingReport{Version: 1, BatchNumber: 4, L1BaseFee: big.NewInt(2), ExtraGas: 3})})
	r := batchReportOf(1, b)
	if r == nil || r.BatchNumber != 4 || r.GasSpent != 3 || r.WeiSpent.Int64() != 6 {
		t.Fatalf("report: %+v", r)
	}
}

func TestScanBatchReportsUnlimited(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	// No block has exactly two transactions, so the budgeted prefilter
	// would never look at block 995's report.
	report := nitro.BatchPostingReport{Version: 2, BatchTimestamp: uint64(baseTime.Unix()), Poster: "0xdaa5260800000000000000000000000000000000", BatchNumber: 7, CalldataLen: 1, CalldataNonZeros: 1, ExtraGas: 1, L1BaseFee: big.NewInt(1)}
	rpc.fullBlocks[995] = nitro.Block{Header: rpc.header(995), Txs: []nitro.Tx{
		{Hash: "0x1", Type: nitro.InternalTxType, To: nitro.ArbosAddress, Input: nitro.EncodeStartBlock(nitro.StartBlock{L1BaseFee: big.NewInt(0)})},
		{Hash: "0x2", Type: nitro.InternalTxType, To: nitro.ArbosAddress, Input: nitro.EncodeBatchPostingReport(report)},
		{Hash: "0x3", Type: 2, To: "0xdead"},
	}}
	store := dbtest.New()
	budgeted := newTestFollower(t, rpc, store)
	if err := budgeted.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := budgeted.scanBatchReports(ctx); err != nil {
		t.Fatal(err)
	}
	if len(store.ReportRows) != 0 || rpc.calledTimes("BlocksWithTxs") != 0 {
		t.Fatal("budgeted scan must keep the two-transaction prefilter")
	}

	f := newTestFollower(t, rpc, dbtest.New())
	f.net.CallsPerSecond = 0
	f.store = store
	if err := f.scanBatchReports(ctx); err != nil {
		t.Fatal(err)
	}
	if len(store.ReportRows) != 1 || store.ReportRows["4663/995"].BatchNumber != 7 {
		t.Fatalf("unlimited scan reports: %+v", store.ReportRows)
	}
	if v, _, _ := store.GetState(ctx, 4663, db.StateBatchScanCursor); v != "1000" {
		t.Fatalf("batch cursor = %q", v)
	}
	if rpc.calledTimes("BlocksWithTxs") != 1 {
		t.Fatalf("BlocksWithTxs calls = %d", rpc.calledTimes("BlocksWithTxs"))
	}
	// More than batchScanLimit new blocks: the scan loops until it has
	// caught up, in batches of nitro.MaxBatch.
	f.net.CallsPerSecond = 0
	rpc.setHead(1150)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.scanBatchReports(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := store.GetState(ctx, 4663, db.StateBatchScanCursor); v != "1150" {
		t.Fatalf("batch cursor after loop = %q", v)
	}
	if rpc.calledTimes("BlocksWithTxs") != 3 {
		t.Fatalf("BlocksWithTxs calls = %d", rpc.calledTimes("BlocksWithTxs"))
	}
	// A canceled context stops the loop between rounds.
	rpc.setHead(1400)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := f.scanBatchReports(cctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled scan: %v", err)
	}
	if v, _, _ := store.GetState(ctx, 4663, db.StateBatchScanCursor); v != "1250" {
		t.Fatalf("cursor after canceled round = %q", v)
	}
	store.FailOn["BlocksAfter"] = true
	if err := f.scanBatchReports(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("BlocksAfter failure: %v", err)
	}
}
