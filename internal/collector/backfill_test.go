package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/db/dbtest"
	"github.com/tirante-dev/gascurve/internal/model"
	"github.com/tirante-dev/gascurve/internal/nitro"
	"github.com/tirante-dev/gascurve/internal/pricer"
)

func TestBackfillConstraintResetUsesTransactionGas(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	rpc.constraints = []nitro.Constraint{{Target: 60_000_000, Window: 15, Backlog: 9}}
	store := dbtest.New()
	oldEntries := []model.ConstraintSetEntry{{Target: 40_000_000, Window: 30, StartingBacklog: 1_000}}
	newParams := []nitro.ConstraintParam{{GasTargetPerSecond: 60_000_000, AdjustmentWindowSeconds: 15, StartingBacklog: 5}}
	for _, set := range []db.ConstraintSet{
		{ChainID: 4663, EffectiveBlock: 100, EffectiveAt: baseTime, Source: model.SourceGenesis, Constraints: entriesJSON(oldEntries)},
		{ChainID: 4663, EffectiveBlock: 500, EffectiveAt: baseTime, Source: model.SourceOwnerAction, Constraints: entriesJSON(entriesOf(newParams))},
	} {
		if _, err := store.InsertConstraintSet(ctx, set); err != nil {
			t.Fatal(err)
		}
	}
	args, err := db.MarshalJSONB(map[string]any{"constraints": newParams})
	if err != nil {
		t.Fatal(err)
	}
	action := db.OwnerAction{
		ChainID: 4663, BlockNumber: 500, TxHash: "0xlate", TxIndex: sql.NullInt64{Int64: 2, Valid: true},
		LogIndex: 9, TS: time.Unix(int64(tsFor(500)), 0).UTC(), Method: "setGasPricingConstraints", Args: args,
	}
	if _, err := store.InsertOwnerActions(ctx, []db.OwnerAction{action}); err != nil {
		t.Fatal(err)
	}
	rpc.receipts[action.TxHash] = nitro.Receipt{
		TxHash: action.TxHash, BlockNumber: action.BlockNumber, TxIndex: 2,
		GasUsed: 100_000, CumulativeGasUsed: 900_000,
	}
	f := newTestFollower(t, rpc, store)
	f.cfg.BackfillDepth = 70 * time.Second
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.ownerScanThrough = 999
	f.mu.Unlock()
	if status, err := f.BackfillStep(ctx); err != nil || status != BackfillProgressed {
		t.Fatalf("backfill: %v %v", status, err)
	}
	block, err := store.BlockByNumber(ctx, 4663, 500)
	if err != nil || block == nil {
		t.Fatalf("block 500: %+v %v", block, err)
	}
	if want := 5 + gasFor(500) - 800_000; block.Backlogs[0] != want {
		t.Fatalf("block 500 backlog = %d, want %d", block.Backlogs[0], want)
	}
	if rpc.calledTimes("TransactionReceipts") != 1 {
		t.Fatalf("receipt calls = %d", rpc.calledTimes("TransactionReceipts"))
	}
}

func seedSets(t *testing.T, store *dbtest.MemStore) {
	t.Helper()
	ctx := context.Background()
	for _, cs := range []db.ConstraintSet{
		{ChainID: 4663, EffectiveBlock: 100, EffectiveAt: baseTime.Add(10 * time.Second), Source: model.SourceGenesis,
			Constraints: entriesJSON([]model.ConstraintSetEntry{{Target: 60_000_000, Window: 15}, {Target: 20_000_000, Window: 86_400, StartingBacklog: 1_000_000}})},
		{ChainID: 4663, EffectiveBlock: 500, EffectiveAt: baseTime.Add(50 * time.Second), Source: model.SourceOwnerAction,
			Constraints: entriesJSON([]model.ConstraintSetEntry{{Target: 60_000_000, Window: 15}, {Target: 40_000_000, Window: 86_400, StartingBacklog: 7_000_000}})},
	} {
		if _, err := store.InsertConstraintSet(ctx, cs); err != nil {
			t.Fatal(err)
		}
	}
}

// scanned marks the owner-action timeline complete through the followed
// head, which the backfill requires before it starts any segment.
func scanned(f *Follower) {
	f.mu.Lock()
	f.ownerScanThrough = max(f.ownerScanThrough, f.head)
	f.mu.Unlock()
}

func runBackfill(t *testing.T, f *Follower, maxSteps int) (steps int) {
	t.Helper()
	scanned(f)
	for steps < maxSteps {
		status, err := f.BackfillStep(context.Background())
		if err != nil {
			t.Fatalf("step %d: %v", steps, err)
		}
		steps++
		switch status {
		case BackfillDone:
			return steps
		case BackfillIdle:
			t.Fatalf("unexpected idle at step %d", steps)
		case BackfillProgressed:
		}
	}
	t.Fatalf("backfill did not finish in %d steps", maxSteps)
	return steps
}

func TestBackfillSegments(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedSets(t, store)
	f := newTestFollower(t, rpc, store)
	// now is base+100s; a depth of 70s lands on block 300 (ts base+30s).
	f.cfg.BackfillDepth = 70 * time.Second
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.minFeeChanges = []minFeeChange{{block: 300, fee: big.NewInt(30_000_000)}}
	f.mu.Unlock()
	// Nothing starts before the owner-action timeline covers the block
	// before the first live block: the historical constraint sets and fees
	// are unknown until then, and a scan that stopped short of the live
	// start is not enough.
	if st, err := f.BackfillStep(ctx); err != nil || st != BackfillIdle {
		t.Fatalf("before the owner scan: %v %v", st, err)
	}
	f.mu.Lock()
	f.ownerScanThrough = 998
	f.mu.Unlock()
	if st, err := f.BackfillStep(ctx); err != nil || st != BackfillIdle {
		t.Fatalf("scan short of the live start: %v %v", st, err)
	}
	if _, ok, _ := store.GetState(ctx, 4663, db.StateBackfillCursor); ok {
		t.Fatal("no cursor may be written before the owner scan")
	}
	f.mu.Lock()
	f.ownerScanThrough = 999
	f.mu.Unlock()
	// Nothing happens while the fast loop is rewinding a reorg.
	f.rewinding.Store(true)
	if st, err := f.BackfillStep(ctx); err != nil || st != BackfillIdle {
		t.Fatalf("rewinding: %v %v", st, err)
	}
	f.rewinding.Store(false)
	// Nor while the last tick found the stored head over a batch behind.
	f.behind.Store(uint64(f.cfg.HeaderBatchSize) + 1)
	if st, err := f.BackfillStep(ctx); err != nil || st != BackfillIdle {
		t.Fatalf("behind the chain: %v %v", st, err)
	}
	f.behind.Store(0)
	// Without spare budget the smallest batch still goes, queued for its
	// turn at the pacer, so a demanding fast tick slows the backfill down
	// rather than stopping it.
	rpc.available = 0
	if st, err := f.BackfillStep(ctx); err != nil || st != BackfillProgressed {
		t.Fatalf("no spare budget: %v %v", st, err)
	}
	if calls := rpc.headerCalls; len(calls) == 0 || len(calls[len(calls)-1]) != minBackfillBatch {
		t.Fatalf("the smallest batch goes without spare budget: %v", calls)
	}
	rpc.available = 14
	if st, err := f.BackfillStep(ctx); err != nil || st != BackfillProgressed {
		t.Fatalf("receipt-weighted spare budget: %v %v", st, err)
	}
	if calls := rpc.headerCalls; len(calls[len(calls)-1]) != 7 {
		t.Fatalf("14 spare calls must fetch 7 blocks: %v", calls)
	}
	cross, _ := f.loadCursor(ctx)
	rpc.mu.Lock()
	if rpc.parentOverride == nil {
		rpc.parentOverride = map[uint64]string{}
	}
	rpc.parentOverride[cross.Next] = "0xwrong"
	rpc.mu.Unlock()
	if _, err := f.BackfillStep(ctx); err == nil {
		t.Fatal("a batch that does not link to the prior backfill batch must fail")
	}
	rpc.mu.Lock()
	delete(rpc.parentOverride, cross.Next)
	rpc.mu.Unlock()
	rpc.available = 1000

	steps := runBackfill(t, f, 200)
	if steps < 90 {
		t.Fatalf("expected around 92 steps, got %d", steps)
	}
	c, err := f.loadCursor(ctx)
	if err != nil || !c.Done || c.DepthStart != 300 || c.Top != 1000 {
		t.Fatalf("cursor: %+v %v", c, err)
	}
	// Every block from the genesis set (100) to the first stored block is
	// folded exactly once: 900 backfilled plus the live head.
	if total := blockCountIn(store, db.Resolution1m); total != 901 {
		t.Fatalf("1m bucket blocks = %d", total)
	}
	// Blocks in the hour of the first live block are row-backed (the live
	// loop rebuilds those buckets from rows), so they exist as rows and
	// carry the minimum fee in force: the genesis default before block
	// 300, the recorded change from there.
	b299, _ := store.BlockByNumber(ctx, 4663, 299)
	b300, _ := store.BlockByNumber(ctx, 4663, 300)
	if b299 == nil || b300 == nil || b299.MinBaseFee.Wei.Int64() != pricer.InitialMinimumBaseFeeWei || b300.MinBaseFee.Wei.Int64() != 30_000_000 {
		t.Fatalf("backfill rows and fees: %+v %+v", b299, b300)
	}
	// A block's prediction is computed while replaying its parent, so the floor recorded at 300 first
	// reaches a prediction at 301. Row 300 therefore holds the floor in force at 300 (which its own fee
	// split needs) beside a prediction the genesis floor produced: the two coincide everywhere except
	// at a floor change.
	b301, _ := store.BlockByNumber(ctx, 4663, 301)
	if b301 == nil || b300.PredictedBaseFee.Wei.Cmp(b301.PredictedBaseFee.Wei.BigInt()) <= 0 {
		t.Fatalf("the higher genesis floor must price higher: %+v vs %+v", b300, b301)
	}
	bk, _ := store.Buckets(ctx, 4663, db.Resolution1m, baseTime, baseTime.Add(time.Minute))
	if len(bk) != 1 || !bk[0].ConstraintSetID.Valid || bk[0].ConstraintSetID.Int64 != 2 || bk[0].LastBlock != 599 {
		t.Fatalf("first minute bucket: %+v", bk[0])
	}
	if bk[0].BacklogsMax[1] < 7_000_000 {
		t.Fatalf("starting backlog from the set should seed the replay: %+v", bk[0])
	}
	// The live bucket keeps its head as the end block.
	last, _ := store.Buckets(ctx, 4663, db.Resolution1m, baseTime.Add(time.Minute), baseTime.Add(2*time.Minute))
	if len(last) != 1 || last[0].LastBlock != 1000 || last[0].Blocks != 401 {
		t.Fatalf("live minute bucket: %+v", last)
	}
	// Once done the step is a no-op.
	if st, err := f.BackfillStep(ctx); err != nil || st != BackfillDone {
		t.Fatalf("after done: %v %v", st, err)
	}
	// A restarted follower resumes from the checkpoint.
	f2 := newTestFollower(t, rpc, store)
	if st, err := f2.BackfillStep(ctx); err != nil || st != BackfillDone {
		t.Fatalf("resume: %v %v", st, err)
	}
}

// TestBackfillAdditiveBeyondBoundary: buckets before the hour of the first
// live block have no rows and are folded additively, committed with the
// cursor, so a replayed batch never double counts.
func TestBackfillAdditiveBeyondBoundary(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(40_000) // ts base + 4000 s
	store := dbtest.New()
	// The backfill replays only from a known constraint set; this one
	// covers everything from the depth boundary on.
	if _, err := store.InsertConstraintSet(ctx, db.ConstraintSet{ChainID: 4663, EffectiveBlock: 30_000, EffectiveAt: baseTime.Add(3000 * time.Second), Source: model.SourceOwnerAction,
		Constraints: entriesJSON([]model.ConstraintSetEntry{{Target: 60_000_000, Window: 15}, {Target: 40_000_000, Window: 86_400}})}); err != nil {
		t.Fatal(err)
	}
	f := newTestFollower(t, rpc, store)
	f.now = func() time.Time { return baseTime.Add(4000 * time.Second) }
	f.cfg.BackfillDepth = 1000 * time.Second // block 30000, an hour before the live start's hour
	f.cfg.HeaderBatchSize = 100
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runBackfill(t, f, 200)
	if total := blockCountIn(store, db.Resolution1h); total != 10_001 {
		t.Fatalf("1h bucket blocks = %d", total)
	}
	boundary := baseTime.Add(3600 * time.Second)
	if b, _ := store.BlockByNumber(ctx, 4663, 35_999); b != nil {
		t.Fatal("blocks before the boundary hour must not become rows")
	}
	if b, _ := store.BlockByNumber(ctx, 4663, 36_000); b == nil || b.TS.Before(boundary) {
		t.Fatalf("blocks from the boundary hour on are rows: %+v", b)
	}
	early, _ := store.Buckets(ctx, 4663, db.Resolution1h, baseTime, boundary)
	if len(early) != 1 || early[0].Blocks != 6000 || early[0].LastBlock != 35_999 || early[0].BaseFeeSum.Wei.Sign() <= 0 {
		t.Fatalf("additive hour bucket: %+v", early)
	}
	if early[0].BaseFeeAvg.Cmp(new(big.Int).Div(early[0].BaseFeeSum.Wei.BigInt(), big.NewInt(6000))) != 0 {
		t.Fatalf("average must derive from the exact sum: %+v", early[0])
	}
	// Discarding an unverified live-model cursor drops only the
	// backfill-only buckets and starts over.
	f.mu.Lock()
	f.cursorChecked = false
	f.mu.Unlock()
	c, _ := f.loadCursor(ctx)
	c.Done, c.Active, c.SetID, c.Verified = false, true, 0, false
	if err := f.saveCursor(ctx, store, c); err != nil {
		t.Fatal(err)
	}
	if st, err := f.BackfillStep(ctx); err != nil || st != BackfillProgressed {
		t.Fatalf("after discard: %v %v", st, err)
	}
	// The discarded buckets are gone; the step that followed rebuilt the
	// first batch of the fresh segment only.
	if early, _ := store.Buckets(ctx, 4663, db.Resolution1h, baseTime, boundary); len(early) != 1 || early[0].Blocks != 100 || early[0].LastBlock != 30_099 {
		t.Fatalf("backfill-only buckets must be deleted and re-backfilled: %+v", early)
	}
	if late, _ := store.Buckets(ctx, 4663, db.Resolution1h, boundary, boundary.Add(time.Hour)); len(late) != 1 {
		t.Fatalf("row-backed buckets must survive: %+v", late)
	}
	c, _ = f.loadCursor(ctx)
	if !c.Active || !c.Verified || c.Done {
		t.Fatalf("fresh cursor: %+v", c)
	}
	// The discard runs once per process; a verified cursor is kept.
	f.mu.Lock()
	f.cursorChecked = false
	f.mu.Unlock()
	if _, err := f.BackfillStep(ctx); err != nil {
		t.Fatal(err)
	}
	if c2, _ := f.loadCursor(ctx); c2.Next <= c.Next {
		t.Fatalf("verified cursor must not be discarded: %+v", c2)
	}
	f.mu.Lock()
	f.cursorChecked = false
	f.mu.Unlock()
	c.Verified, c.SetID = false, 0
	_ = f.saveCursor(ctx, store, c)
	store.FailOn["DeleteBucketsBefore"] = true
	if _, err := f.BackfillStep(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("discard failure: %v", err)
	}
	delete(store.FailOn, "DeleteBucketsBefore")
	// A failed cleanup does not count as checked: the next step retries it
	// instead of continuing from the unverified cursor.
	f.mu.Lock()
	checked := f.cursorChecked
	f.mu.Unlock()
	if checked {
		t.Fatal("cursorChecked must only be set once the cleanup committed")
	}
	if st, err := f.BackfillStep(ctx); err != nil || st != BackfillProgressed {
		t.Fatalf("retried cleanup: %v %v", st, err)
	}
	if c2, _ := f.loadCursor(ctx); !c2.Verified || c2.SegStart != 30_000 {
		t.Fatalf("the retried cleanup must start over: %+v", c2)
	}
	f.mu.Lock()
	checked = f.cursorChecked
	f.mu.Unlock()
	if !checked {
		t.Fatal("cursorChecked after a committed cleanup")
	}
}

// TestBackfillStopsAtScanOrigin: on a chain whose owner scan started at a
// cutoff, the backfill never reaches below it: with an archive origin it
// replays from the observed set and sampled fee recorded there, without
// one the range is a hole.
func TestBackfillStopsAtScanOrigin(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	f.cfg.BackfillDepth = 70 * time.Second // block 300 without an origin
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// The origin sample is end-of-block state for block 500, so the set it
	// establishes is effective at 501.
	if _, err := store.InsertConstraintSet(ctx, db.ConstraintSet{ChainID: 4663, EffectiveBlock: 501, EffectiveAt: baseTime.Add(50 * time.Second), Source: model.SourceObserved,
		Constraints: entriesJSON([]model.ConstraintSetEntry{{Target: 60_000_000, Window: 15}, {Target: 40_000_000, Window: 86_400, StartingBacklog: 7}})}); err != nil {
		t.Fatal(err)
	}
	_ = store.SetState(ctx, 4663, db.StateOwnerScanOrigin, `{"block":500,"minBaseFee":"30000000","archive":true}`)
	f2 := newTestFollower(t, rpc, store)
	if err := f2.ensureInit(ctx); err != nil {
		t.Fatal(err)
	}
	if f2.scanOrigin == nil || f2.scanOrigin.Block != 500 || f2.minFeeAt(499).Int64() != pricer.InitialMinimumBaseFeeWei || f2.minFeeAt(500).Int64() != 30_000_000 || f2.minFeeChangeBlock(700) != 500 {
		t.Fatalf("origin fee: %+v %s", f2.scanOrigin, f2.minFeeAt(500))
	}
	f2.cfg.BackfillDepth = 70 * time.Second
	runBackfill(t, f2, 200)
	c, _ := f2.loadCursor(ctx)
	if !c.Done || c.DepthStart != 501 {
		t.Fatalf("depth clamped to the block after the origin: %+v", c)
	}
	// The origin block itself is not replayed: its gas is already in the
	// sampled backlogs, so adding it again would inflate every price above.
	for _, n := range []uint64{499, 500} {
		if b, _ := store.BlockByNumber(ctx, 4663, n); b != nil {
			t.Fatalf("block %d is at or below the origin and must not be priced: %+v", n, b)
		}
	}
	b501, _ := store.BlockByNumber(ctx, 4663, 501)
	if b501 == nil || b501.MinBaseFee.Wei.Int64() != 30_000_000 || b501.Backlogs[1] != 7+gasFor(501) {
		t.Fatalf("replay starts at the block after the origin: %+v", b501)
	}
	// A corrupt origin checkpoint fails initialization.
	bad := dbtest.New()
	_ = bad.SetState(ctx, 4663, db.StateOwnerScanOrigin, "{bad")
	if err := newTestFollower(t, rpc, bad).ensureInit(ctx); err == nil {
		t.Fatal("corrupt origin")
	}
	bad2 := dbtest.New()
	_ = bad2.SetState(ctx, 4663, db.StateOwnerScanThrough, "x")
	if err := newTestFollower(t, rpc, bad2).ensureInit(ctx); err == nil {
		t.Fatal("corrupt scan through")
	}
	if (&scanOrigin{MinBaseFee: "x"}).fee() != nil || (*scanOrigin)(nil).fee() != nil {
		t.Fatal("origin fee parsing")
	}
}

// TestBackfillWithoutKnownState: with no constraint set covering the
// range and no sampled origin state, the backfill records the range as a
// hole and stops instead of replaying the current model with empty
// backlogs, which would present invented history as replayed.
func TestBackfillWithoutKnownState(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	f.cfg.BackfillDepth = 30 * time.Second // block 700
	// No sample yet and no blocks: idle.
	scanned(f)
	if st, err := f.BackfillStep(ctx); err != nil || st != BackfillIdle {
		t.Fatalf("no blocks: %v %v", st, err)
	}
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if steps := runBackfill(t, f, 100); steps != 1 {
		t.Fatalf("nothing can be replayed, so one step finishes it: %d", steps)
	}
	if holes := holesOf(t, store); len(holes) != 1 || holes[0].From != 700 || holes[0].To != 999 {
		t.Fatalf("the unreconstructable range must be a hole: %+v", holes)
	}
	for _, n := range []uint64{700, 999} {
		if b, _ := store.BlockByNumber(ctx, 4663, n); b != nil {
			t.Fatalf("block %d has no known state and must not be priced: %+v", n, b)
		}
	}
	if total := blockCountIn(store, db.Resolution1h); total != 1 { // the live head alone
		t.Fatalf("hour bucket blocks = %d", total)
	}
	// A legacy chain without an archive origin is the same: its historical
	// parameters are not known, so nothing is replayed.
	rpcL := newFakeRPC(1000)
	lp := legacyParams()
	rpcL.legacy = &lp
	storeL := dbtest.New()
	fl := newTestFollower(t, rpcL, storeL)
	fl.cfg.BackfillDepth = 30 * time.Second
	if err := fl.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if steps := runBackfill(t, fl, 100); steps != 1 {
		t.Fatalf("legacy without an origin: %d steps", steps)
	}
	if holes := holesOf(t, storeL); len(holes) != 1 || holes[0].From != 700 {
		t.Fatalf("legacy hole: %+v", holes)
	}
}

// TestBackfillLegacyFromArchiveOrigin: a legacy chain whose whole state
// was sampled at an archive scan origin replays forward from those
// parameters and that backlog, splitting at the recorded legacy parameter
// changes rather than pricing history with the current ones.
func TestBackfillLegacyFromArchiveOrigin(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	lp := legacyParams()
	rpc.legacy = &lp
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	f.cfg.BackfillDepth = 70 * time.Second // block 300
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// The origin sampled a different speed limit and a real backlog at
	// block 500; a recorded action raises the speed limit again at 800.
	origin := `{"block":500,"minBaseFee":"30000000","archive":true,"legacy":{"speedLimit":3000000,"inertia":50,"tolerance":5,"backlog":900000}}`
	if err := store.SetState(ctx, 4663, db.StateOwnerScanOrigin, origin); err != nil {
		t.Fatal(err)
	}
	f2 := newTestFollower(t, rpc, store)
	if err := f2.ensureInit(ctx); err != nil {
		t.Fatal(err)
	}
	f2.mu.Lock()
	f2.legacyChanges = []legacyChange{{block: 800, method: methodSetSpeedLimit, value: 9_000_000}}
	f2.mu.Unlock()
	f2.cfg.BackfillDepth = 70 * time.Second
	runBackfill(t, f2, 200)
	c, _ := f2.loadCursor(ctx)
	if !c.Done || c.DepthStart != 501 {
		t.Fatalf("legacy origin cursor: %+v", c)
	}
	if b, _ := store.BlockByNumber(ctx, 4663, 500); b != nil {
		t.Fatal("the origin block itself is not replayed")
	}
	b501, _ := store.BlockByNumber(ctx, 4663, 501)
	if b501 == nil || b501.MinBaseFee.Wei.Int64() != 30_000_000 {
		t.Fatalf("the sampled floor applies from the origin on: %+v", b501)
	}
	// The sampled backlog, not zero, is where the replay starts: the first
	// replayed block carries the sampled backlog minus one block of speed
	// limit plus its own gas.
	if b501.Backlogs[0] == gasFor(501) {
		t.Fatalf("the replay must start from the sampled backlog: %+v", b501.Backlogs)
	}
	// The recorded speed limit change splits the replay: the parameters in
	// force below 800 are the origin's, above it the changed one.
	f2.mu.Lock()
	tl := f2.timelineLocked(nil)
	f2.mu.Unlock()
	base := &pricer.Legacy{SpeedLimit: lp.SpeedLimit, Inertia: lp.Inertia, Tolerance: lp.Tolerance}
	if got := tl.legacyAt(700, base); got.SpeedLimit != 3_000_000 || got.Inertia != 50 || got.Tolerance != 5 {
		t.Fatalf("origin parameters below the change: %+v", got)
	}
	if got := tl.legacyAt(900, base); got.SpeedLimit != 9_000_000 || got.Inertia != 50 {
		t.Fatalf("recorded change above it: %+v", got)
	}
	// Below the origin nothing is replayed at all.
	if got := tl.legacyAt(400, base); got.SpeedLimit != lp.SpeedLimit {
		t.Fatalf("below the origin the caller's base stands: %+v", got)
	}
	if !sameLegacyParams(nil, nil) || sameLegacyParams(base, nil) {
		t.Fatal("legacy parameter comparison")
	}
	applyLegacyParams(&pricer.State{}, base)
	applyLegacyParams(&pricer.State{Legacy: base}, nil)
}

func TestBackfillErrors(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedSets(t, store)
	f := newTestFollower(t, rpc, store)
	f.cfg.BackfillDepth = 70 * time.Second
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	scanned(f)
	store.FailOn["GetState"] = true
	if _, err := f.BackfillStep(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("GetState: %v", err)
	}
	delete(store.FailOn, "GetState")
	_ = store.SetState(ctx, 4663, db.StateBackfillCursor, "{bad")
	if _, err := f.BackfillStep(ctx); err == nil {
		t.Fatal("corrupt cursor should fail")
	}
	delete(store.StateRows, "4663/"+db.StateBackfillCursor)
	store.FailOn["OldestBlock"] = true
	if _, err := f.BackfillStep(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("OldestBlock: %v", err)
	}
	delete(store.FailOn, "OldestBlock")
	rpc.errs["HeaderByNumber"] = errRPC
	if _, err := f.BackfillStep(ctx); !errors.Is(err, errRPC) {
		t.Fatalf("findBlockAt: %v", err)
	}
	delete(rpc.errs, "HeaderByNumber")
	store.FailOn["SetState"] = true
	if _, err := f.BackfillStep(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("save cursor: %v", err)
	}
	delete(store.FailOn, "SetState")
	// Segment starts, then the header fetch fails.
	rpc.errs["HeadersByNumbers"] = errRPC
	if _, err := f.BackfillStep(ctx); !errors.Is(err, errRPC) {
		t.Fatalf("headers: %v", err)
	}
	delete(rpc.errs, "HeadersByNumbers")
	rpc.headerShift = 1
	if _, err := f.BackfillStep(ctx); err == nil {
		t.Fatal("headers for other block numbers must fail")
	}
	rpc.headerShift = 0
	for _, method := range []string{"FoldBuckets", "UpsertBlocks", "RebuildBuckets"} {
		store.FailOn[method] = true
		if _, err := f.BackfillStep(ctx); !errors.Is(err, dbtest.ErrInjected) {
			t.Fatalf("%s: %v", method, err)
		}
		delete(store.FailOn, method)
	}
	// A cursor pointing at an unknown set is reported.
	c, _ := f.loadCursor(ctx)
	c.SetID = 99
	if err := f.saveCursor(ctx, store, c); err != nil {
		t.Fatal(err)
	}
	if _, err := f.BackfillStep(ctx); err == nil {
		t.Fatal("unknown set should fail")
	}
	// Unreadable set documents fail both at segment start and in the state.
	c.SetID = 2
	if err := f.saveCursor(ctx, store, c); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.sets[1].Constraints = db.JSONB(`{bad`)
	f.mu.Unlock()
	if _, err := f.BackfillStep(ctx); err == nil {
		t.Fatal("bad set document should fail")
	}
	c.Active = false
	if err := f.saveCursor(ctx, store, c); err != nil {
		t.Fatal(err)
	}
	if _, err := f.BackfillStep(ctx); err == nil {
		t.Fatal("bad set document at segment start should fail")
	}
	// Segment without a set and without a sample is idle; with no live
	// sample the state cannot be derived.
	f.mu.Lock()
	f.sets = nil
	f.lastSample = nil
	f.mu.Unlock()
	// No set and no sampled origin state: the range becomes a hole rather
	// than a replay of the live model.
	if st, err := f.BackfillStep(ctx); err != nil || st != BackfillDone {
		t.Fatalf("no set, no sampled state: %v %v", st, err)
	}
	c.Active, c.SetID, c.Done = true, 0, false
	if err := f.saveCursor(ctx, store, c); err != nil {
		t.Fatal(err)
	}
	if _, err := f.BackfillStep(ctx); err == nil {
		t.Fatal("a live-model segment without sampled origin state should fail")
	}
	// Init failure surfaces.
	bad := dbtest.New()
	bad.FailOn["UpsertNetwork"] = true
	if _, err := newTestFollower(t, rpc, bad).BackfillStep(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("init: %v", err)
	}
	// The cursor round-trips through JSON.
	var back backfillCursor
	raw, _ := json.Marshal(c)
	if err := json.Unmarshal(raw, &back); err != nil || back.Next != c.Next {
		t.Fatal("cursor json")
	}
}

// TestHistoryStart: the history window is measured back from the sampled
// head's own timestamp, a chain younger than the window resolves to
// genesis, and a depth beyond the supported maximum is capped rather than
// wrapped round by the duration arithmetic.
func TestHistoryStart(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	f := newTestFollower(t, rpc, dbtest.New())
	// The head is block 1000 at baseTime+100s: 70 seconds of history is
	// block 300.
	n, err := f.findHistoryStart(ctx, 70*time.Second)
	if err != nil || n != 300 {
		t.Fatalf("historyStart = %d %v", n, err)
	}
	// More history than the chain has resolves to genesis, and costs one
	// header for each boundary rather than a whole search.
	rpc.calls["HeaderByNumber"] = 0
	if n, err := f.findHistoryStart(ctx, time.Hour); err != nil || n != 1 {
		t.Fatalf("younger than the window = %d %v", n, err)
	}
	if calls := rpc.calledTimes("HeaderByNumber"); calls != 2 {
		t.Fatalf("both boundaries are checked, and nothing else: %d calls", calls)
	}
	// A depth beyond the supported maximum is capped where the cutoff is
	// computed, so adding originMargin to it cannot overflow.
	if got := f.historyDepth(maxHistoryDepth + time.Hour); got != maxHistoryDepth {
		t.Fatalf("capped depth = %v", got)
	}
	if got := f.historyDepth(-time.Hour); got != 0 {
		t.Fatalf("negative depth = %v", got)
	}
	if n, err := f.findHistoryStart(ctx, f.historyDepth(time.Duration(1<<62))+originMargin); err != nil || n != 1 {
		t.Fatalf("an extreme depth must still resolve to genesis: %d %v", n, err)
	}
	rpc.errs["BlockNumber"] = errRPC
	if _, err := f.findHistoryStart(ctx, time.Second); !errors.Is(err, errRPC) {
		t.Fatalf("head error: %v", err)
	}
	delete(rpc.errs, "BlockNumber")
	rpc.errs["HeaderByNumber"] = errRPC
	if _, err := f.findHistoryStart(ctx, time.Second); !errors.Is(err, errRPC) {
		t.Fatalf("head header error: %v", err)
	}
}

// TestBlockAtAfterTheHead: a target later than the head is a retryable
// error, never the head itself, which the owner scan would persist as an
// origin and mark all the history before it unavailable for good.
func TestBlockAtAfterTheHead(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	f := newTestFollower(t, rpc, dbtest.New())
	top := rpc.header(1000)
	if _, err := f.blockAt(ctx, 1000, &top, baseTime.Add(time.Hour)); !errors.Is(err, errTargetAfterHead) {
		t.Fatalf("a target after the head must be retryable, got %v", err)
	}
	// The clock is an hour ahead of a stalled chain: the owner scan
	// derives its cutoff from the head, so it still starts at genesis
	// instead of recording an origin at the head.
	f.now = func() time.Time { return baseTime.Add(time.Hour) }
	f.cfg.BackfillDepth = 70 * time.Second
	if err := f.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	origin := f.scanOrigin
	f.mu.Unlock()
	if origin != nil {
		t.Fatalf("no origin may be recorded from a clock ahead of the chain: %+v", origin)
	}
	if got, _ := f.findHistoryStart(ctx, 70*time.Second); got != 300 {
		t.Fatalf("the window is measured from the head, not the clock: %d", got)
	}
}

func TestBackfillArchiveAnchors(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	// The archive endpoint is a separate RPC: it reports enormous backlogs
	// so anchored stretches are unmistakable next to the pure replay.
	archive := newFakeRPC(1000)
	archive.backlogsAt = func(n uint64) []uint64 { return []uint64{n * 1_000_000_000, n * 2_000_000_000} }
	// Historical state carries the constraint set in force at that block.
	archive.constraintsAt = func(n uint64) []nitro.Constraint {
		if n < 500 {
			return []nitro.Constraint{{Target: 60_000_000, Window: 15}, {Target: 20_000_000, Window: 86_400}}
		}
		return archive.constraints
	}
	store := dbtest.New()
	seedSets(t, store)
	f := newTestFollower(t, rpc, store)
	f.archive = archive
	f.cfg.BackfillAnchorInterval = 100
	f.cfg.BackfillDepth = 70 * time.Second // block 300
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runBackfill(t, f, 200)
	// Segment [500, 991) then [100, 500): one anchor per 100 blocks, in
	// replay order, and none for the live tick's own blocks. Every anchor
	// is sampled on the archive endpoint, never on the ordinary one.
	if fmt.Sprint(archive.sampleAt) != "[500 600 700 800 900 100 200 300 400]" || len(rpc.sampleAt) != 0 {
		t.Fatalf("anchor samples = %v (ordinary endpoint: %v)", archive.sampleAt, rpc.sampleAt)
	}
	if archive.calledTimes("HeadersByNumbers") != 0 || rpc.calledTimes("HeadersByNumbers") == 0 {
		t.Fatal("headers must come from the ordinary endpoint")
	}
	c, err := f.loadCursor(ctx)
	if err != nil || !c.Done || c.LastAnchor != 400 || c.AnchorMinFee != "20000000" {
		t.Fatalf("cursor: %+v %v", c, err)
	}
	// The minimum fee sampled at an anchor is used from the next block on
	// (no change is recorded, so the genesis default applied before).
	b100, _ := store.BlockByNumber(ctx, 4663, 100)
	b101, _ := store.BlockByNumber(ctx, 4663, 101)
	if b100.MinBaseFee.Wei.Int64() != pricer.InitialMinimumBaseFeeWei || b101.MinBaseFee.Wei.Int64() != 20_000_000 {
		t.Fatalf("anchor min fee: %s then %s", b100.MinBaseFee.Wei, b101.MinBaseFee.Wei)
	}
	// A recorded change after the anchor wins over the anchor.
	f.mu.Lock()
	f.minFeeChanges = []minFeeChange{{block: 150, fee: big.NewInt(7)}}
	f.mu.Unlock()
	f.mu.Lock()
	tl := f.timelineLocked(nil)
	f.mu.Unlock()
	fees := f.backfillFees(tl, &backfillCursor{LastAnchor: 100, AnchorMinFee: "20000000"}, []nitro.Header{{Number: 149}, {Number: 150}, {Number: 200}, {Number: 201}}, map[uint64]*big.Int{200: big.NewInt(9)})
	if fees[0].Int64() != 20_000_000 || fees[1].Int64() != 7 || fees[2].Int64() != 7 || fees[3].Int64() != 9 {
		t.Fatalf("backfill fees: %v", fees)
	}
	if c.LastAnchorErrorBips != replayErrorAt(t, f, 400) {
		t.Fatalf("anchor error %d should be the pre-anchor replay error", c.LastAnchorErrorBips)
	}
	bk, _ := store.Buckets(ctx, 4663, db.Resolution1m, baseTime, baseTime.Add(time.Minute))
	if len(bk) != 1 || bk[0].BacklogsMax[0] < 500_000_000_000 || bk[0].BacklogsEnd[1] < 400_000_000_000 {
		t.Fatalf("first minute bucket should carry the anchored backlogs: %+v", bk[0])
	}
	// The cursor round-trips the anchor fields.
	raw, _ := json.Marshal(c)
	var back backfillCursor
	if err := json.Unmarshal(raw, &back); err != nil || back.LastAnchor != 400 || back.LastAnchorErrorBips != c.LastAnchorErrorBips {
		t.Fatalf("cursor json: %+v %v", back, err)
	}

	// Without archive the same backfill never samples state.
	rpc2 := newFakeRPC(1000)
	rpc2.backlogsAt = archive.backlogsAt
	rpc2.constraintsAt = archive.constraintsAt
	store2 := dbtest.New()
	seedSets(t, store2)
	f2 := newTestFollower(t, rpc2, store2)
	f2.cfg.BackfillAnchorInterval = 100
	f2.cfg.BackfillDepth = 70 * time.Second
	if err := f2.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	rpc2.sampleAt = nil
	runBackfill(t, f2, 200)
	if len(rpc2.sampleAt) != 0 {
		t.Fatalf("pure replay must not sample state: %v", rpc2.sampleAt)
	}
	c2, _ := f2.loadCursor(ctx)
	if c2.LastAnchor != 0 {
		t.Fatalf("no anchor expected: %+v", c2)
	}
	bk2, _ := store2.Buckets(ctx, 4663, db.Resolution1m, baseTime, baseTime.Add(time.Minute))
	if bk2[0].BacklogsMax[0] >= 500_000_000_000 {
		t.Fatalf("pure replay bucket carries archive backlogs: %+v", bk2[0])
	}
}

// replayErrorAt recomputes the replay error the anchor recorded for a block
// by replaying the anchored segment up to it.
func replayErrorAt(t *testing.T, f *Follower, number uint64) int64 {
	t.Helper()
	ctx := context.Background()
	var set db.ConstraintSet
	for _, s := range f.sets {
		if s.EffectiveBlock <= number {
			set = s
		}
	}
	c := &backfillCursor{SetID: set.ID}
	entries, err := setEntries(set)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		c.Backlogs = append(c.Backlogs, e.StartingBacklog)
	}
	st, _, err := f.segmentState(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	var numbers []uint64
	for n := set.EffectiveBlock; n <= number; n++ {
		numbers = append(numbers, n)
	}
	headers, _ := f.rpc.HeadersByNumbers(ctx, numbers)
	anchor, fees, err := f.backfillAnchors(ctx, st, headers)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := f.replaySegment(context.Background(), st, c, headers, anchor, fees)
	if err != nil {
		t.Fatal(err)
	}
	last := rows[len(rows)-1]
	if last.Number != number || !last.Anchored {
		t.Fatalf("expected anchored row %d: %+v", number, last)
	}
	return db.ReplayErrorBips(last)
}

func TestBackfillAnchorEdgeCases(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	rpc.backlogsAt = func(n uint64) []uint64 { return []uint64{n, n} }
	store := dbtest.New()
	seedSets(t, store)
	f := newTestFollower(t, rpc, store)
	f.archive = rpc
	f.cfg.BackfillAnchorInterval = 5
	f.cfg.BackfillDepth = 70 * time.Second
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	st := &pricer.State{MinBaseFee: big.NewInt(1), Constraints: []pricer.Constraint{{Target: 60_000_000, Window: 15}, {Target: 40_000_000, Window: 86_400}}}
	numbers := []uint64{98, 99, 100, 101, 102, 103, 104, 105}
	headers, _ := rpc.HeadersByNumbers(ctx, numbers)
	anchor, fees, err := f.backfillAnchors(ctx, st, headers)
	if err != nil || anchor == nil || fees[100].Int64() != 20_000_000 {
		t.Fatalf("anchors: %v %v", fees, err)
	}
	if b, ok := anchor(100); !ok || b[0] != 100 {
		t.Fatalf("anchor at 100: %v %v", b, ok)
	}
	if _, ok := anchor(101); ok {
		t.Fatal("101 is not an anchor block")
	}
	rows, err := f.replaySegment(context.Background(), st, &backfillCursor{}, headers, anchor, fees)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		anchored := r.Number == 100 || r.Number == 105
		if r.Anchored != anchored {
			t.Fatalf("row %d anchored = %v", r.Number, r.Anchored)
		}
		if anchored && (r.Backlogs[0] != r.Number || r.Backlogs[1] != r.Number) {
			t.Fatalf("anchored row %d backlogs = %v", r.Number, r.Backlogs)
		}
	}
	// No anchor block in range: nil anchor, no calls.
	rpc.sampleAt = nil
	plain, _ := rpc.HeadersByNumbers(ctx, []uint64{101, 102})
	if a, _, err := f.backfillAnchors(ctx, st, plain); err != nil || a != nil || len(rpc.sampleAt) != 0 {
		t.Fatalf("out of range: %v %v %v", a, err, rpc.sampleAt)
	}
	// A sample whose model differs from the segment is skipped.
	rpc.mu.Lock()
	rpc.constraints = rpc.constraints[:1]
	rpc.mu.Unlock()
	header100, _ := rpc.HeadersByNumbers(ctx, []uint64{100})
	if a, _, err := f.backfillAnchors(ctx, st, header100); err != nil || a != nil {
		t.Fatalf("shape mismatch: %v %v", a, err)
	}
	// Legacy chains anchor the single backlog.
	lp := legacyParams()
	rpc.legacy = &lp
	lst := &pricer.State{MinBaseFee: big.NewInt(1), Legacy: &pricer.Legacy{SpeedLimit: lp.SpeedLimit, Inertia: lp.Inertia, Tolerance: lp.Tolerance}}
	a, _, err := f.backfillAnchors(ctx, lst, header100)
	if err != nil || a == nil {
		t.Fatalf("legacy anchors: %v", err)
	}
	if b, ok := a(100); !ok || len(b) != 1 || b[0] != 100 {
		t.Fatalf("legacy anchor: %v %v", b, ok)
	}
	// A failing archive call fails the step so it is retried.
	rpc.errs["PricingSampleAt"] = errRPC
	if _, _, err := f.backfillAnchors(ctx, lst, header100); !errors.Is(err, errRPC) {
		t.Fatalf("anchor error: %v", err)
	}
	scanned(f)
	if _, err := f.BackfillStep(ctx); !errors.Is(err, errRPC) {
		t.Fatalf("step with failing anchor: %v", err)
	}
	other := newFakeRPC(1000)
	other.fork(100, "other")
	f.archive = other
	if _, _, err := f.backfillAnchors(ctx, lst, header100); err == nil {
		t.Fatal("archive state from another fork must fail")
	}
}

// blockDigest renders the reconstructed pricing of a row, so two backfills
// that walked the same blocks in a different order can be compared.
func blockDigest(b *db.Block) string {
	if b == nil {
		return "<missing>"
	}
	return fmt.Sprintf("%d gas=%d fee=%s min=%v/%s predicted=%v/%s exp=%d bips=%v backlogs=%v anchored=%v",
		b.Number, b.GasUsed, b.BaseFee.BigInt(), b.MinBaseFee.Valid, b.MinBaseFee.Wei.BigInt(),
		b.PredictedBaseFee.Valid, b.PredictedBaseFee.Wei.BigInt(), b.ExponentBips, b.ConstraintBips, b.Backlogs, b.Anchored)
}

// TestBackfillWindowsDescend: on an archive network the backfill walks a
// segment in windows, newest first, each seeded from the state the archive
// reports below it. The reconstruction must be the one a single forward
// replay produces, block for block, including the predictions across every
// window boundary.
func TestBackfillWindowsDescend(t *testing.T) {
	ctx := context.Background()
	forward := dbtest.New()
	seedSets(t, forward)
	fwd := newTestFollower(t, newFakeRPC(1000), forward)
	if err := fwd.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runBackfill(t, fwd, 300)

	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedSets(t, store)
	// The archive answers with the state the forward replay ended each
	// block on, which is what a real archive node holds.
	archive := newFakeRPC(1000)
	archive.minFee = big.NewInt(pricer.InitialMinimumBaseFeeWei)
	archive.backlogsAt = func(n uint64) []uint64 {
		b, err := forward.BlockByNumber(ctx, 4663, n)
		if err != nil || b == nil {
			t.Errorf("no forward row at %d: %v", n, err)
			return nil
		}
		return b.Backlogs
	}
	archive.constraintsAt = func(n uint64) []nitro.Constraint {
		if n < 500 {
			return []nitro.Constraint{{Target: 60_000_000, Window: 15}, {Target: 20_000_000, Window: 86_400}}
		}
		return archive.constraints
	}
	f := newTestFollower(t, rpc, store)
	f.archive = archive
	f.cfg.BackfillWindow = 137
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	scanned(f)
	// The first run reconstructs the blocks just below the live data, not
	// the segment's oldest ones.
	for {
		st, err := f.BackfillStep(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if st != BackfillProgressed {
			t.Fatalf("first window: %v", st)
		}
		if b, _ := store.BlockByNumber(ctx, 4663, 863); b != nil {
			break
		}
	}
	if b, _ := store.BlockByNumber(ctx, 4663, 600); b != nil {
		t.Fatal("the first run must start a window below the live data, not at the segment floor")
	}
	runBackfill(t, f, 300)
	if fmt.Sprint(archive.sampleAt) != "[861 724 587 361 224]" {
		t.Fatalf("window seeds must descend, one below each window: %v", archive.sampleAt)
	}
	for n := uint64(100); n < 1000; n++ {
		got, _ := store.BlockByNumber(ctx, 4663, n)
		want, _ := forward.BlockByNumber(ctx, 4663, n)
		if blockDigest(got) != blockDigest(want) {
			t.Fatalf("block %d\n windowed: %s\n forward:  %s", n, blockDigest(got), blockDigest(want))
		}
	}
	if a, b := blockCountIn(store, db.Resolution1m), blockCountIn(forward, db.Resolution1m); a != b {
		t.Fatalf("folded blocks = %d, forward folded %d", a, b)
	}
	c, err := f.loadCursor(ctx)
	if err != nil || !c.Done || c.SkipFirst {
		t.Fatalf("cursor: %+v %v", c, err)
	}
}

// TestBackfillWindowSeedFallbacks: a window that cannot be seeded leaves
// the run where it always was, at the segment floor, and a window is never
// placed below the configured depth.
func TestBackfillWindowSeedFallbacks(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedSets(t, store)
	archive := newFakeRPC(1000)
	f := newTestFollower(t, rpc, store)
	f.archive = archive
	f.cfg.BackfillWindow = 137
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	scanned(f)
	// An archive that cannot serve the seed fails the step, so it is
	// retried rather than silently replaying the whole set forward.
	archive.errs["PricingSampleAt"] = errRPC
	if _, err := f.BackfillStep(ctx); !errors.Is(err, errRPC) {
		t.Fatalf("a failing seed must fail the step: %v", err)
	}
	if c, _ := f.loadCursor(ctx); c.Active {
		t.Fatalf("no run may be committed on a failed seed: %+v", c)
	}
	// A sampled model of another shape cannot seed the segment's replay
	// state, so the run starts at the floor instead.
	delete(archive.errs, "PricingSampleAt")
	archive.constraintsAt = func(uint64) []nitro.Constraint {
		return []nitro.Constraint{{Target: 60_000_000, Window: 15}}
	}
	if st, err := f.BackfillStep(ctx); err != nil || st != BackfillProgressed {
		t.Fatalf("shape mismatch: %v %v", st, err)
	}
	c, _ := f.loadCursor(ctx)
	if c.SegStart != 500 || c.SkipFirst {
		t.Fatalf("a skipped seed must leave the run at the segment floor: %+v", c)
	}

	// The window bottoms out at the configured depth, so an archive
	// network never reconstructs history nobody asked for.
	store2 := dbtest.New()
	seedSets(t, store2)
	f2 := newTestFollower(t, newFakeRPC(1000), store2)
	f2.archive = newFakeRPC(1000)
	f2.cfg.BackfillWindow = 800
	f2.cfg.BackfillDepth = 40 * time.Second // block 600, inside the newest set
	if err := f2.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	scanned(f2)
	if st, err := f2.BackfillStep(ctx); err != nil || st != BackfillProgressed {
		t.Fatalf("depth-clamped window: %v %v", st, err)
	}
	c2, _ := f2.loadCursor(ctx)
	if c2.SegStart != 600 || c2.LastAnchor != 598 {
		t.Fatalf("the window must stop at the depth: %+v", c2)
	}
	runBackfill(t, f2, 100)
	if b, _ := store2.BlockByNumber(ctx, 4663, 600); b == nil {
		t.Fatal("the depth block must be reconstructed")
	}
	if b, _ := store2.BlockByNumber(ctx, 4663, 599); b != nil {
		t.Fatal("nothing below the depth may be reconstructed")
	}
}

// TestBackfillLegacyWindowSeed: a legacy chain's windows are seeded too,
// against the parameters in force at the seed rather than the current ones.
func TestBackfillLegacyWindowSeed(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	lp := legacyParams()
	rpc.legacy = &lp
	store := dbtest.New()
	origin := `{"block":500,"minBaseFee":"30000000","archive":true,"legacy":{"speedLimit":3000000,"inertia":50,"tolerance":5,"backlog":900000}}`
	if err := store.SetState(ctx, 4663, db.StateOwnerScanOrigin, origin); err != nil {
		t.Fatal(err)
	}
	f := newTestFollower(t, rpc, store)
	f.cfg.BackfillDepth = 70 * time.Second
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	archive := newFakeRPC(1000)
	// The archive holds the origin's parameters, which is what the replay
	// state carries at the seed, and a backlog no pure replay would reach.
	archive.legacy = &nitro.LegacyParams{SpeedLimit: 3_000_000, Inertia: 50, Tolerance: 5}
	archive.backlogsAt = func(uint64) []uint64 { return []uint64{500_000_000} }
	f.archive = archive
	f.cfg.BackfillWindow = 200
	f.cfg.BackfillAnchorInterval = 100_000
	runBackfill(t, f, 200)
	if fmt.Sprint(archive.sampleAt) != "[798 598]" {
		t.Fatalf("legacy window seeds: %v", archive.sampleAt)
	}
	b800, _ := store.BlockByNumber(ctx, 4663, 800)
	if b800 == nil || b800.Backlogs[0] < 400_000_000 {
		t.Fatalf("the sampled backlog must seed the window: %+v", b800)
	}
	// The block below a window is priced by the run under it, exactly once.
	b799, _ := store.BlockByNumber(ctx, 4663, 799)
	if b799 == nil || !b799.PredictedBaseFee.Valid || !b800.PredictedBaseFee.Valid {
		t.Fatalf("a window boundary must not drop a prediction: %+v %+v", b799, b800)
	}
	if got, want := blockCountIn(store, db.Resolution1m), int64(1000-501+1); got != want {
		t.Fatalf("folded blocks = %d, want %d", got, want)
	}
}
