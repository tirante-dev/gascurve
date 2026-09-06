package collector

import (
	"context"
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
	// Nothing happens while the fast loop is catching up.
	f.catchingUp.Store(true)
	if st, err := f.BackfillStep(ctx); err != nil || st != BackfillIdle {
		t.Fatalf("catching up: %v %v", st, err)
	}
	f.catchingUp.Store(false)
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
	if b299 == nil || b300 == nil || b299.MinBaseFee.Int64() != pricer.InitialMinimumBaseFeeWei || b300.MinBaseFee.Int64() != 30_000_000 {
		t.Fatalf("backfill rows and fees: %+v %+v", b299, b300)
	}
	if b299.PredictedBaseFee.Cmp(b300.PredictedBaseFee.BigInt()) <= 0 {
		t.Fatalf("the higher genesis floor must price higher: %s vs %s", b299.PredictedBaseFee, b300.PredictedBaseFee)
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
	c.Verified = false
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
	if _, err := store.InsertConstraintSet(ctx, db.ConstraintSet{ChainID: 4663, EffectiveBlock: 500, EffectiveAt: baseTime.Add(50 * time.Second), Source: model.SourceObserved,
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
	if !c.Done || c.DepthStart != 500 {
		t.Fatalf("depth clamped to the origin: %+v", c)
	}
	if b, _ := store.BlockByNumber(ctx, 4663, 499); b != nil {
		t.Fatal("nothing below the origin may be priced")
	}
	b500, _ := store.BlockByNumber(ctx, 4663, 500)
	if b500 == nil || b500.MinBaseFee.Int64() != 30_000_000 || b500.Backlogs[1] != 7+gasFor(500) {
		t.Fatalf("origin block replays from the sampled state: %+v", b500)
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

func TestBackfillWithoutSets(t *testing.T) {
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
	steps := runBackfill(t, f, 100)
	if steps < 29 {
		t.Fatalf("steps = %d", steps)
	}
	// Backfilled buckets carry no set (none is in force before the seed's
	// observed set at 1000); only the live bucket, whose last block is the
	// seed, is tagged with it.
	for _, b := range store.BucketRows {
		if b.Resolution == db.Resolution1h && b.ConstraintSetID.Valid != (b.LastBlock == 1000) {
			t.Fatalf("set id on backfilled bucket: %+v", b)
		}
	}
	if total := blockCountIn(store, db.Resolution1h); total != 301 { // 700..999 backfilled plus the live head
		t.Fatalf("hour bucket blocks = %d", total)
	}
	if c, _ := f.loadCursor(ctx); !c.Verified {
		t.Fatalf("segments chosen after the scan are verified: %+v", c)
	}
	// Legacy chains backfill the same way from the live parameters.
	rpcL := newFakeRPC(1000)
	lp := legacyParams()
	rpcL.legacy = &lp
	storeL := dbtest.New()
	fl := newTestFollower(t, rpcL, storeL)
	fl.cfg.BackfillDepth = 30 * time.Second
	if err := fl.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runBackfill(t, fl, 100)
	if storeL.BucketCount(4663, db.Resolution1m) == 0 {
		t.Fatal("legacy backfill wrote no buckets")
	}
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
	if st, err := f.BackfillStep(ctx); err != nil || st != BackfillIdle {
		t.Fatalf("no set, no sample: %v %v", st, err)
	}
	c.Active, c.SetID = true, 0
	if err := f.saveCursor(ctx, store, c); err != nil {
		t.Fatal(err)
	}
	if _, err := f.BackfillStep(ctx); err == nil {
		t.Fatal("state without sample should fail")
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

func TestFindBlockAt(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	f := newTestFollower(t, rpc, dbtest.New())
	n, err := f.findBlockAt(ctx, baseTime.Add(30*time.Second))
	if err != nil || n != 300 {
		t.Fatalf("findBlockAt = %d %v", n, err)
	}
	if n, _ := f.findBlockAt(ctx, baseTime.Add(-time.Hour)); n != 1 {
		t.Fatalf("before genesis = %d", n)
	}
	if n, _ := f.findBlockAt(ctx, baseTime.Add(time.Hour)); n != 1000 {
		t.Fatalf("after head = %d", n)
	}
	rpc.errs["BlockNumber"] = errRPC
	if _, err := f.findBlockAt(ctx, baseTime); !errors.Is(err, errRPC) {
		t.Fatalf("head error: %v", err)
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
	if b100.MinBaseFee.Int64() != pricer.InitialMinimumBaseFeeWei || b101.MinBaseFee.Int64() != 20_000_000 {
		t.Fatalf("anchor min fee: %s then %s", b100.MinBaseFee, b101.MinBaseFee)
	}
	// A recorded change after the anchor wins over the anchor.
	f.mu.Lock()
	f.minFeeChanges = []minFeeChange{{block: 150, fee: big.NewInt(7)}}
	f.mu.Unlock()
	fees := f.backfillFees(&backfillCursor{LastAnchor: 100, AnchorMinFee: "20000000"}, []nitro.Header{{Number: 149}, {Number: 150}, {Number: 200}, {Number: 201}}, map[uint64]*big.Int{200: big.NewInt(9)})
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
	anchor, fees, err := f.backfillAnchors(ctx, st, numbers)
	if err != nil {
		t.Fatal(err)
	}
	rows := f.replaySegment(st, c, headers, anchor, fees)
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
	anchor, fees, err := f.backfillAnchors(ctx, st, numbers)
	if err != nil || anchor == nil || fees[100].Int64() != 20_000_000 {
		t.Fatalf("anchors: %v %v", fees, err)
	}
	if b, ok := anchor(100); !ok || b[0] != 100 {
		t.Fatalf("anchor at 100: %v %v", b, ok)
	}
	if _, ok := anchor(101); ok {
		t.Fatal("101 is not an anchor block")
	}
	headers, _ := rpc.HeadersByNumbers(ctx, numbers)
	rows := f.replaySegment(st, &backfillCursor{}, headers, anchor, fees)
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
	if a, _, err := f.backfillAnchors(ctx, st, []uint64{101, 102}); err != nil || a != nil || len(rpc.sampleAt) != 0 {
		t.Fatalf("out of range: %v %v %v", a, err, rpc.sampleAt)
	}
	// A sample whose model differs from the segment is skipped.
	rpc.mu.Lock()
	rpc.constraints = rpc.constraints[:1]
	rpc.mu.Unlock()
	if a, _, err := f.backfillAnchors(ctx, st, []uint64{100}); err != nil || a != nil {
		t.Fatalf("shape mismatch: %v %v", a, err)
	}
	// Legacy chains anchor the single backlog.
	lp := legacyParams()
	rpc.legacy = &lp
	lst := &pricer.State{MinBaseFee: big.NewInt(1), Legacy: &pricer.Legacy{SpeedLimit: lp.SpeedLimit, Inertia: lp.Inertia, Tolerance: lp.Tolerance}}
	a, _, err := f.backfillAnchors(ctx, lst, []uint64{100})
	if err != nil || a == nil {
		t.Fatalf("legacy anchors: %v", err)
	}
	if b, ok := a(100); !ok || len(b) != 1 || b[0] != 100 {
		t.Fatalf("legacy anchor: %v %v", b, ok)
	}
	// A failing archive call fails the step so it is retried.
	rpc.errs["FastSampleAt"] = errRPC
	if _, _, err := f.backfillAnchors(ctx, lst, []uint64{100}); !errors.Is(err, errRPC) {
		t.Fatalf("anchor error: %v", err)
	}
	scanned(f)
	if _, err := f.BackfillStep(ctx); !errors.Is(err, errRPC) {
		t.Fatalf("step with failing anchor: %v", err)
	}
}
