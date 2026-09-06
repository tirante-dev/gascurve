package collector

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/db/dbtest"
	"github.com/tirante-dev/gascurve/internal/model"
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

func runBackfill(t *testing.T, f *Follower, maxSteps int) (steps int) {
	t.Helper()
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
	f.minFeeChanges = []minFeeChange{{block: 300, fee: big.NewInt(30_000_000)}}
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// Nothing happens while the fast loop is catching up.
	f.catchingUp.Store(true)
	if st, err := f.BackfillStep(ctx); err != nil || st != BackfillIdle {
		t.Fatalf("catching up: %v %v", st, err)
	}
	f.catchingUp.Store(false)
	// Nor without spare budget.
	rpc.available = 0
	if st, err := f.BackfillStep(ctx); err != nil || st != BackfillIdle {
		t.Fatalf("no budget: %v %v", st, err)
	}
	rpc.available = 1000

	steps := runBackfill(t, f, 200)
	if steps < 90 {
		t.Fatalf("expected around 92 steps, got %d", steps)
	}
	c, err := f.loadCursor(ctx)
	if err != nil || !c.Done || c.DepthStart != 300 {
		t.Fatalf("cursor: %+v %v", c, err)
	}
	// Every block from the genesis set (100) to the first stored block is
	// folded exactly once: 891 backfilled plus the 10 live ones.
	var total int64
	for _, b := range store.BucketRows {
		if b.Resolution == db.Resolution1m {
			total += b.Blocks
		}
	}
	if total != 901 {
		t.Fatalf("1m bucket blocks = %d", total)
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

func TestBackfillWithoutSets(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	f.cfg.BackfillDepth = 30 * time.Second // block 700
	// No sample yet and no blocks: idle.
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
	var total int64
	for _, b := range store.BucketRows {
		if b.Resolution == db.Resolution1h {
			total += b.Blocks
			if b.ConstraintSetID.Valid {
				t.Fatal("no set id expected without sets")
			}
		}
	}
	if total != 301 { // 700..990 backfilled plus 991..1000 live
		t.Fatalf("hour bucket blocks = %d", total)
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
	store.FailOn["FoldBuckets"] = true
	if _, err := f.BackfillStep(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("fold: %v", err)
	}
	delete(store.FailOn, "FoldBuckets")
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
