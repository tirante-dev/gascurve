package collector

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/db/dbtest"
	"github.com/tirante-dev/gascurve/internal/logger"
	"github.com/tirante-dev/gascurve/internal/model"
	"github.com/tirante-dev/gascurve/internal/nitro"
	"github.com/tirante-dev/gascurve/internal/pricer"
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
	// A chain younger than the configured history depth is scanned from
	// genesis: the fake chain runs at ten blocks a second, so a day of
	// history covers all 250k of its blocks.
	f.cfg.BackfillDepth = 24 * time.Hour
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
		if !a.TxIndex.Valid {
			t.Fatalf("transaction index not recorded: %+v", a)
		}
	}
	// The seed had recorded the live shape as an observed set (id 1) at
	// the head; the action that explains it moved that row in place, so
	// the id is stable and no second set exists.
	sets, _ := store.ConstraintSets(ctx, 4663)
	if len(sets) != 6 || sets[0].Source != model.SourceGenesis || sets[0].EffectiveBlock != 28 || sets[5].Source != model.SourceOwnerAction {
		t.Fatalf("constraint sets: %+v", sets)
	}
	moved := false
	for _, cs := range sets {
		if cs.ID == 1 && cs.Source != model.SourceObserved && cs.EffectiveBlock < 250_000 {
			moved = true
		}
		if cs.Source == model.SourceObserved {
			t.Fatalf("no observed set may remain once the action is known: %+v", cs)
		}
	}
	if !moved {
		t.Fatalf("the observed set must keep its id: %+v", sets)
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
	// The scan reached its head: the timeline is complete through it.
	if v, _, _ := store.GetState(ctx, 4663, db.StateOwnerScanThrough); v != "250000" || f.ownerScanThrough != 250_000 {
		t.Fatalf("owner scan through = %q (%d)", v, f.ownerScanThrough)
	}
	// Second run: the fast loop scans its catch-up interval, the slow loop
	// only the range after its cursor; nothing new is inserted.
	rpc.logRanges = nil
	rpc.setHead(250_010)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rpc.logRanges) != 2 || rpc.logRanges[0] != [2]uint64{250_001, 250_010} || rpc.logRanges[1] != [2]uint64{250_001, 250_010} {
		t.Fatalf("incremental log ranges = %v", rpc.logRanges)
	}
	if v, _, _ := store.GetState(ctx, 4663, db.StateOwnerScanThrough); v != "250010" {
		t.Fatalf("owner scan through = %q", v)
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

// scanCutoff is the block the large-chain tests expect a fresh owner scan
// to start at.
const scanCutoff = 70_000_000

// largeChainHead is the head of the large-chain tests' fake chain.
const largeChainHead = 120_000_000

// depthPastCutoff sets backfill_depth so that the history window, measured
// back from the head's own timestamp with the origin margin added, reaches
// exactly scanCutoff: a fresh owner scan then starts there rather than at
// genesis. The window comes from the chain, never from the host clock, so
// the clock is left alone.
func depthPastCutoff(f *Follower) {
	f.cfg.BackfillDepth = time.Duration(tsFor(largeChainHead)-tsFor(scanCutoff))*time.Second - originMargin
}

func TestSlowTickLargeChainAndLegacy(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(largeChainHead)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	depthPastCutoff(f)
	// Without a fast tick the head comes from eth_blockNumber. The scan
	// starts at the block backfill_depth (and a margin) before now, found by
	// a header search, never at genesis of a chain this long. Without an
	// archive endpoint nothing before the cutoff can be priced: the range
	// is a hole and the origin says so.
	if err := f.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	if rpc.calledTimes("BlockNumber") != 1 || rpc.logRanges[0][0] != 70_000_000 {
		t.Fatalf("large chain scan start: %v", rpc.logRanges)
	}
	if v, _, _ := store.GetState(ctx, 4663, db.StateOwnerScanOrigin); v != `{"block":70000000,"archive":false}` || f.scanOrigin == nil || f.scanOrigin.Archive {
		t.Fatalf("origin without archive: %q", v)
	}
	if holes := holesOf(t, store); len(holes) != 1 || holes[0].From != 0 || holes[0].To != 69_999_999 {
		t.Fatalf("pre-cutoff hole: %+v", holes)
	}
	if f.minFeeAt(70_000_000).Int64() != pricer.InitialMinimumBaseFeeWei {
		t.Fatal("no fee is guessed for an unreconstructable origin")
	}
	if sets, _ := store.ConstraintSets(ctx, 4663); len(sets) != 0 {
		t.Fatalf("no set without archive state: %+v", sets)
	}
	// With an archive endpoint the state at the cutoff is sampled once:
	// its fee is the baseline from the cutoff on and its constraints an
	// observed set there; a second pass does not sample again.
	archive := newFakeRPC(largeChainHead)
	archive.minFee = big.NewInt(30_000_000)
	storeA := dbtest.New()
	fa := newTestFollower(t, rpc, storeA)
	depthPastCutoff(fa)
	fa.archive = archive
	rpc.logRanges = nil
	if err := fa.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(archive.sampleAt) != 1 || archive.sampleAt[0] != 70_000_000 {
		t.Fatalf("origin sample = %v", archive.sampleAt)
	}
	if v, _, _ := storeA.GetState(ctx, 4663, db.StateOwnerScanOrigin); v != `{"block":70000000,"minBaseFee":"30000000","archive":true}` {
		t.Fatalf("origin with archive: %q", v)
	}
	if fa.scanOrigin.replayFrom() != 70_000_001 || !fa.scanOrigin.fullState() {
		t.Fatalf("the origin sample is end-of-block state: %+v", fa.scanOrigin)
	}
	if fa.minFeeAt(69_999_999).Int64() != pricer.InitialMinimumBaseFeeWei || fa.minFeeAt(70_000_000).Int64() != 30_000_000 || fa.minFeeChangeBlock(80_000_000) != 70_000_000 {
		t.Fatalf("origin fee: %s", fa.minFeeAt(70_000_000))
	}
	// The sampled constraints are the state at the end of the cutoff, so
	// the set takes effect at the block after it: replaying the cutoff
	// again would add its own gas to backlogs that already contain it.
	sets, _ := storeA.ConstraintSets(ctx, 4663)
	if len(sets) != 1 || sets[0].EffectiveBlock != 70_000_001 || sets[0].Source != model.SourceObserved || len(holesOf(t, storeA)) != 0 {
		t.Fatalf("origin set: %+v holes %+v", sets, holesOf(t, storeA))
	}
	delete(storeA.StateRows, "4663/"+db.StateOwnerLogCursor)
	if err := fa.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(archive.sampleAt) != 1 {
		t.Fatal("the origin is established once")
	}
	// A restarted follower reloads it; a failing archive sample or a
	// failing checkpoint write fail the scan without an origin.
	fb := newTestFollower(t, rpc, storeA)
	if err := fb.ensureInit(ctx); err != nil || fb.scanOrigin == nil || fb.scanOrigin.MinBaseFee != "30000000" {
		t.Fatalf("reloaded origin: %+v %v", fb.scanOrigin, err)
	}
	archive.errs["FastSampleAt"] = errRPC
	fc := newTestFollower(t, rpc, dbtest.New())
	depthPastCutoff(fc)
	fc.archive = archive
	if err := fc.SlowTick(ctx); !errors.Is(err, errRPC) || fc.scanOrigin != nil {
		t.Fatalf("origin sample failure: %v", err)
	}
	delete(archive.errs, "FastSampleAt")
	failing := dbtest.New()
	failing.FailOn["WithChainTx"] = true
	fd := newTestFollower(t, rpc, failing)
	depthPastCutoff(fd)
	if err := fd.SlowTick(ctx); !errors.Is(err, dbtest.ErrInjected) || fd.scanOrigin != nil {
		t.Fatalf("origin write failure: %v", err)
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
	sets, _ = store3.ConstraintSets(ctx, 4663)
	if len(sets) != 1 || sets[0].Source != model.SourceObserved {
		t.Fatalf("observed set on a chain without actions: %+v", sets)
	}
}

// TestOriginRecordsLegacyState: on a legacy chain the archive origin
// records the whole sampled pricer state, parameters and backlog, not the
// minimum fee alone. Without it the backfill would price history from the
// current parameters, which have nothing to do with what was in force.
func TestOriginRecordsLegacyState(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(largeChainHead)
	rpc.legacy = &nitro.LegacyParams{SpeedLimit: 7_000_000, Inertia: 102, Tolerance: 10, Backlog: 42}
	archive := newFakeRPC(largeChainHead)
	archive.legacy = &nitro.LegacyParams{SpeedLimit: 3_000_000, Inertia: 50, Tolerance: 5, Backlog: 900_000}
	archive.minFee = big.NewInt(30_000_000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	depthPastCutoff(f)
	f.archive = archive
	if err := f.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	o := f.scanOrigin
	if o == nil || o.Legacy == nil {
		t.Fatalf("the legacy state must be persisted: %+v", o)
	}
	if o.Legacy.SpeedLimit != 3_000_000 || o.Legacy.Inertia != 50 || o.Legacy.Tolerance != 5 || o.Legacy.Backlog != 900_000 {
		t.Fatalf("sampled legacy state: %+v", o.Legacy)
	}
	if o.MinBaseFee != "30000000" || !o.fullState() || o.replayFrom() != 70_000_001 {
		t.Fatalf("origin: %+v", o)
	}
	// A legacy chain records no constraint set.
	if sets, _ := store.ConstraintSets(ctx, 4663); len(sets) != 0 {
		t.Fatalf("legacy origin must record no set: %+v", sets)
	}
	// A restarted follower reloads the whole state.
	g := newTestFollower(t, rpc, store)
	if err := g.ensureInit(ctx); err != nil {
		t.Fatal(err)
	}
	if g.scanOrigin == nil || g.scanOrigin.Legacy == nil || g.scanOrigin.Legacy.Backlog != 900_000 {
		t.Fatalf("reloaded legacy origin: %+v", g.scanOrigin)
	}
	// From the origin on the sampled parameters are the base, not the live
	// ones, so historical prices come from what was really in force.
	tl := g.timelineLocked(nil)
	base := &pricer.Legacy{SpeedLimit: 7_000_000, Inertia: 102, Tolerance: 10}
	if got := tl.legacyAt(70_000_001, base); got.SpeedLimit != 3_000_000 || got.Inertia != 50 || got.Tolerance != 5 {
		t.Fatalf("parameters at the origin: %+v", got)
	}
	if got := tl.legacyAt(69_999_999, base); got.SpeedLimit != 7_000_000 {
		t.Fatalf("below the origin nothing is known: %+v", got)
	}
	// A legacy sample without parameters is an error rather than a
	// half-recorded origin.
	archive.legacy = nil
	archive.constraints = nil
	bad := newTestFollower(t, rpc, dbtest.New())
	depthPastCutoff(bad)
	bad.archive = archive
	if err := bad.SlowTick(ctx); err == nil || bad.scanOrigin != nil {
		t.Fatalf("a legacy origin without parameters must fail: %v", err)
	}
}

func TestSlowTickBatchReportsAndPrune(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(990)
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
	rpc.setHead(1000)
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
	// Pruning removes blocks and samples older than the retention, except
	// the rows of the first live block's hour while the backfill still
	// runs: its last segment rebuilds those buckets from rows.
	rpc.setHead(1005)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	f.cfg.BlockRetention = time.Nanosecond
	f.cfg.SampleRetention = time.Nanosecond
	if err := f.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	if bs, _ := store.RecentBlocks(ctx, 4663, 100); len(bs) != 16 {
		t.Fatalf("boundary rows must be kept while the backfill runs: %d", len(bs))
	}
	if s, _ := store.LatestStateSample(ctx, 4663, false); s != nil {
		t.Fatal("samples not pruned")
	}
	_ = store.SetState(ctx, 4663, db.StateBackfillCursor, `{"done":true}`)
	if err := f.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	// Only the blocks whose timestamp equals now (1000..1005) may survive.
	if bs, _ := store.RecentBlocks(ctx, 4663, 100); len(bs) > 6 {
		t.Fatalf("blocks not pruned: %d", len(bs))
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
	rpc.fullBlocks[1000] = nitro.Block{Header: rpc.header(1000), Txs: []nitro.Tx{
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
	for _, method := range []string{"GetState", "SetState", "InsertOwnerActions", "InsertConstraintSet", "TwoTxBlocks", "UpsertBatchReports", "PruneBlocks", "PruneStateSamples", "OldestBlock", "WithChainTx"} {
		fresh := dbtest.New()
		fresh.FailOn[method] = true
		ff := newTestFollower(t, rpc, fresh)
		if method == "InsertConstraintSet" {
			// The seed's observed set is part of the tick's transaction.
			if err := ff.Tick(ctx); !errors.Is(err, dbtest.ErrInjected) {
				t.Fatalf("%s: tick must fail, got %v", method, err)
			}
			continue
		}
		// The tick itself needs the checkpoint writes and the chain
		// transaction, so those run without one.
		if method != "SetState" && method != "GetState" && method != "WithChainTx" {
			if method == "OldestBlock" {
				delete(fresh.FailOn, method)
			}
			if err := ff.Tick(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if method == "OldestBlock" {
			// Only reached without a cursor and with stored blocks; the
			// tick above stored blocks, the scan then asks for the oldest.
			delete(fresh.StateRows, "4663/"+db.StateBatchScanCursor)
			fresh.FailOn[method] = true
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
	// A malformed OwnerActs log fails the range: the cursor must not move
	// past an event that was not understood.
	_ = store.SetState(ctx, 4663, db.StateOwnerLogCursor, "0")
	_ = store.SetState(ctx, 4663, db.StateBatchScanCursor, "1000")
	rpc.logs = []nitro.Log{{Topics: []string{"0x1"}, BlockNumber: 5}}
	err = f.SlowTick(ctx)
	if err == nil || !strings.Contains(err.Error(), "owner action in 1..1000") {
		t.Fatalf("malformed log: %v", err)
	}
	if v, _, _ := store.GetState(ctx, 4663, db.StateOwnerLogCursor); v != "0" {
		t.Fatalf("cursor advanced past a malformed log: %q", v)
	}
	rpc.logs = nil
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
	// The observed set decision: an unreadable latest set is left alone, a
	// missing or different one yields an observed set at the sampled block,
	// a matching one (recorded, or pending in the tick) yields none, and a
	// legacy sample never does.
	sample := rpc.sampleLocked(1000, rpc.constraints, nil)
	f.mu.Lock()
	f.sets = []db.ConstraintSet{{ID: 1, Constraints: db.JSONB(`{bad`)}}
	if f.observedSetLocked(sample, nil) != nil {
		t.Fatal("unreadable latest set must not be shadowed")
	}
	f.sets = nil
	o := f.observedSetLocked(sample, nil)
	if o == nil || o.EffectiveBlock != 1000 || o.Source != model.SourceObserved || o.EffectiveAt.Unix() != int64(tsFor(1000)) {
		t.Fatalf("observed set: %+v", o)
	}
	f.sets = []db.ConstraintSet{{ID: 1, EffectiveBlock: 1, Constraints: entriesJSON([]model.ConstraintSetEntry{{Target: 1, Window: 1}})}}
	if f.observedSetLocked(sample, nil) == nil {
		t.Fatal("a different latest set yields an observed set")
	}
	pending := []*nitro.OwnerAction{{BlockNumber: 999, Constraints: []nitro.ConstraintParam{{GasTargetPerSecond: 60_000_000, AdjustmentWindowSeconds: 15}, {GasTargetPerSecond: 40_000_000, AdjustmentWindowSeconds: 86_400}}}}
	if f.observedSetLocked(sample, pending) != nil {
		t.Fatal("a pending action with the live shape explains the sample")
	}
	f.sets = []db.ConstraintSet{{ID: 1, EffectiveBlock: 1, Constraints: entriesJSON(entriesFromSample(sample))}}
	if f.observedSetLocked(sample, nil) != nil {
		t.Fatal("a matching latest set yields nothing")
	}
	lp := legacyParams()
	if f.observedSetLocked(&nitro.Sample{Legacy: &lp}, nil) != nil {
		t.Fatal("legacy samples never yield observed sets")
	}
	f.mu.Unlock()
	// The observed set is part of the tick's transaction.
	fresh := dbtest.New()
	fresh.FailOn["InsertConstraintSet"] = true
	if err := newTestFollower(t, rpc, fresh).Tick(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("observed insert failure: %v", err)
	}
	if len(fresh.BlockRows[4663]) != 0 {
		t.Fatal("nothing of the tick may commit without its observed set")
	}
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
	rpc := newFakeRPC(990)
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
	rpc.setHead(1000)
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

// TestBatchScanReEvaluatesAfterFailover: a batch scan decided on an
// unlimited endpoint that fails over to a paced one stops reading every
// block with full transactions and goes back to the two-transaction
// prefilter, so the public fallback does not pay for the fast endpoint's
// decision.
func TestBatchScanReEvaluatesAfterFailover(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	pool := &fakePool{fakeRPC: rpc, pol: nitro.Policy{Unlimited: true}}
	store := dbtest.New()
	f := NewFollower(Options{
		Network:   config.NetworkConfig{Name: "robinhood", ChainID: 4663, CallsPerSecond: 4, Enabled: true},
		Collector: testConfig(), RPC: pool, Store: store, Log: logger.Nop(),
		Now:   func() time.Time { return baseTime.Add(140 * time.Second) },
		Sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
	})
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// An unlimited catch-up stores enough blocks for the scan to need more
	// than one round.
	rpc.setHead(1400)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// The pool moves to a paced endpoint while the first round is reading
	// its blocks.
	rpc.hooks["BlocksWithTxs"] = func() {
		pool.pol = nitro.Policy{Unlimited: false, Rate: 4}
		pool.status = nitro.PoolStatus{Active: 1, Failovers: 1}
	}
	if err := f.scanBatchReports(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := store.GetState(ctx, 4663, db.StateBatchScanCursor); v != "1099" {
		t.Fatalf("the scan must stop after the round the failover landed in, cursor = %q", v)
	}
	if calls := rpc.calledTimes("BlocksWithTxs"); calls != 1 {
		t.Fatalf("no further full-transaction round may run on the paced endpoint: %d", calls)
	}
	// Back on an unlimited endpoint the scan catches up again.
	delete(rpc.hooks, "BlocksWithTxs")
	pool.pol = nitro.Policy{Unlimited: true}
	pool.status = nitro.PoolStatus{Active: 0, Failovers: 2}
	if err := f.scanBatchReports(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := store.GetState(ctx, 4663, db.StateBatchScanCursor); v != "1400" {
		t.Fatalf("cursor after catching up = %q", v)
	}
}
