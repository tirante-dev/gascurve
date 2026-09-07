package collector

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/db/dbtest"
	"github.com/tirante-dev/gascurve/internal/model"
	"github.com/tirante-dev/gascurve/internal/nitro"
)

// gapHead is the head every skipped-gap test jumps to: 150 blocks past
// the first one, well over the 100 a paced tick may catch up.
const gapHead = 1150

// skipGap drives a paced follower over a gap wider than its catch-up
// budget, so the head is seeded alone and the skipped range is recorded.
func skipGap(t *testing.T, f *Follower, rpc *fakeRPC) {
	t.Helper()
	ctx := context.Background()
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	rpc.setHead(gapHead)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	rpc.headerCalls = nil
}

// fillAll runs fill steps until nothing is fillable any more.
func fillAll(t *testing.T, f *Follower, maxSteps int) (steps int) {
	t.Helper()
	for steps < maxSteps {
		status, err := f.FillStep(context.Background())
		if err != nil {
			t.Fatalf("fill step %d: %v", steps, err)
		}
		if status == FillNone {
			return steps
		}
		if status == FillIdle {
			t.Fatalf("unexpected idle at fill step %d", steps)
		}
		steps++
	}
	t.Fatalf("the gap did not fill in %d steps", maxSteps)
	return steps
}

// TestFillSkippedGap: a paced network skips a gap, then the filler fetches
// the range in header batches, replays it forward from the stored block
// before it, writes the blocks and their buckets, records the replay error
// against the sampled head that follows the gap, and removes the hole.
func TestFillSkippedGap(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	skipGap(t, f, rpc)

	holes := holesOf(t, store)
	if len(holes) != 1 || holes[0].From != 1001 || holes[0].To != 1149 || holes[0].Reason != "" {
		t.Fatalf("a skipped gap is queued work: %+v", holes)
	}
	if steps := fillAll(t, f, 40); steps != 15 {
		t.Fatalf("149 blocks in batches of 10: %d steps", steps)
	}
	if calls := rpc.headerCalls; len(calls) != 15 || calls[0][0] != 1001 || len(calls[0]) != 10 || len(calls[14]) != 9 {
		t.Fatalf("header batches: %v", calls)
	}
	if rpc.classOf("HeadersByNumbers") != nitro.Bulk {
		t.Fatal("gap headers must go through the bulk lane, leaving the fast reserve alone")
	}
	if holesOf(t, store) != nil {
		t.Fatalf("the filled hole must be gone: %+v", holesOf(t, store))
	}
	if _, ok, _ := store.GetState(ctx, 4663, db.StateHoles); ok {
		t.Fatal("the last hole takes the checkpoint with it")
	}
	// Every block of the gap is stored with the whole pricing breakdown
	// and folded into the buckets exactly once.
	blocks, _ := store.RecentBlocks(ctx, 4663, 1000)
	if len(blocks) != 151 {
		t.Fatalf("stored blocks after the fill: %d", len(blocks))
	}
	for _, want := range []uint64{1001, 1075, 1149} {
		b, _ := store.BlockByNumber(ctx, 4663, want)
		if b == nil || !b.Known() || b.PricingVersion != db.PricingFull || len(b.ConstraintBips) != 2 {
			t.Fatalf("filled block %d: %+v", want, b)
		}
	}
	b1000, _ := store.BlockByNumber(ctx, 4663, 1000)
	b1001, _ := store.BlockByNumber(ctx, 4663, 1001)
	if b1001.Backlogs[0] != b1000.Backlogs[0]+gasFor(1001) || b1001.Anchored {
		t.Fatalf("the fill replays forward from the stored block before the gap: %+v", b1001)
	}
	if blockCountIn(store, db.Resolution1m) != 151 || blockCountIn(store, db.Resolution1h) != 151 {
		t.Fatalf("bucket block counts: 1m %d 1h %d", blockCountIn(store, db.Resolution1m), blockCountIn(store, db.Resolution1h))
	}
	// The sampled head after the gap keeps its real backlogs but now
	// carries the prediction of the replayed range, so the error of the
	// reconstruction is recorded where it can be seen.
	head, _ := store.BlockByNumber(ctx, 4663, 1150)
	if !head.Anchored || head.Backlogs[1] != 11_194_391_810_886 {
		t.Fatalf("the sampled head keeps its sampled backlogs: %+v", head)
	}
	if head.PredictedBaseFee.Cmp(head.BaseFee.BigInt()) == 0 || db.ReplayErrorBips(*head) == 0 {
		t.Fatalf("the replay error must be recorded against the sampled head: %+v", head)
	}
	// A further step has nothing to do and costs no call.
	before := len(rpc.headerCalls)
	if st, err := f.FillStep(ctx); err != nil || st != FillNone {
		t.Fatalf("nothing left to fill: %v %v", st, err)
	}
	if len(rpc.headerCalls) != before {
		t.Fatal("an empty checkpoint must cost no header call")
	}
}

// TestFillNewestHoleFirst: two gaps, the newer one fills first, because
// the recent charts are the ones being looked at.
func TestFillNewestHoleFirst(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	skipGap(t, f, rpc)
	rpc.setHead(1400)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	rpc.headerCalls = nil
	if holes := holesOf(t, store); len(holes) != 2 || holes[1].From != 1151 {
		t.Fatalf("two gaps: %+v", holes)
	}
	if st, err := f.FillStep(ctx); err != nil || st != FillProgressed {
		t.Fatalf("fill step: %v %v", st, err)
	}
	if calls := rpc.headerCalls; len(calls) != 1 || calls[0][0] != 1151 {
		t.Fatalf("the newest gap goes first: %v", calls)
	}
	// The older gap is only reached once the newer one is complete.
	fillAll(t, f, 60)
	if holesOf(t, store) != nil {
		t.Fatalf("both gaps fill: %+v", holesOf(t, store))
	}
	for _, want := range []uint64{1001, 1149, 1151, 1399} {
		if b, _ := store.BlockByNumber(ctx, 4663, want); b == nil {
			t.Fatalf("block %d must be indexed", want)
		}
	}
}

// TestFillLeavesUnfillableHoles: a range with no stored state before it
// can never be replayed. It is left alone, costs no call and is reported
// as unfillable, and it is taken up as soon as the state appears.
func TestFillLeavesUnfillableHoles(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.WithChainTx(ctx, 4663, func(s db.Store) error {
		return f.recordHole(ctx, s, hole{From: 0, To: 899, Reason: reasonNoState})
	}); err != nil {
		t.Fatal(err)
	}
	// A range recorded without state when it was skipped: nothing is
	// stored at 949, so nothing can be replayed forward from there yet.
	if err := store.WithChainTx(ctx, 4663, func(s db.Store) error {
		return f.recordHole(ctx, s, hole{From: 950, To: 999, Reason: reasonNoState})
	}); err != nil {
		t.Fatal(err)
	}
	if st, err := f.FillStep(ctx); err != nil || st != FillNone {
		t.Fatalf("nothing fillable: %v %v", st, err)
	}
	if len(rpc.headerCalls) != 0 {
		t.Fatalf("an unfillable hole must cost no call: %v", rpc.headerCalls)
	}
	summary := model.SummarizeHoles(holesOf(t, store))
	if summary.Pending != 0 || summary.Unfillable != 2 || summary.Blocks != 900+50 {
		t.Fatalf("holes status: %+v", summary)
	}
	// The state appears (the backfill reached block 949): the hole above
	// it becomes work, the one recorded without state stays unfillable.
	f.cfg.BackfillDepth = 96 * time.Second
	seedSets(t, store)
	f.mu.Lock()
	_ = f.reloadSetsLocked(ctx)
	f.mu.Unlock()
	runBackfill(t, f, 200)
	if b, _ := store.BlockByNumber(ctx, 4663, 949); b == nil || !b.Known() {
		t.Fatalf("the backfill must store block 949 with its state: %+v", b)
	}
	fillAll(t, f, 20)
	holes := holesOf(t, store)
	if summary := model.SummarizeHoles(holes); summary.Pending != 0 || summary.Unfillable != len(holes) {
		t.Fatalf("only holes without state are left: %+v %+v", summary, holes)
	}
	for _, h := range holes {
		if h.From == 950 {
			t.Fatalf("the range whose state appeared must be filled: %+v", holes)
		}
	}
	if b, _ := store.BlockByNumber(ctx, 4663, 975); b == nil || !b.Known() {
		t.Fatal("the newly fillable range must be indexed")
	}
}

// TestFillResumesAfterRestart: the hole entry carries the progress cursor,
// so a restarted collector continues where it stopped rather than
// refetching what it already wrote.
func TestFillResumesAfterRestart(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	skipGap(t, f, rpc)
	if st, err := f.FillStep(ctx); err != nil || st != FillProgressed {
		t.Fatalf("first batch: %v %v", st, err)
	}
	if holes := holesOf(t, store); len(holes) != 1 || holes[0].Next != 1011 {
		t.Fatalf("the cursor must be committed with the batch: %+v", holes)
	}
	f2 := newTestFollower(t, rpc, store)
	if err := f2.Tick(ctx); err != nil { // a restarted follower samples first
		t.Fatal(err)
	}
	rpc.headerCalls = nil
	if st, err := f2.FillStep(ctx); err != nil || st != FillProgressed {
		t.Fatalf("resumed batch: %v %v", st, err)
	}
	if calls := rpc.headerCalls; len(calls) != 1 || calls[0][0] != 1011 {
		t.Fatalf("the restart must resume from the cursor: %v", calls)
	}
	fillAll(t, f2, 40)
	if holesOf(t, store) != nil {
		t.Fatalf("the resumed gap must finish: %+v", holesOf(t, store))
	}
	if blockCountIn(store, db.Resolution1m) != 151 {
		t.Fatalf("every block counted once: %d", blockCountIn(store, db.Resolution1m))
	}
}

// TestFillStaleGeneration: a rewind that lands while the fill is fetching
// discards the batch. Nothing is written and the cursor does not move, so
// the retry fetches the same range from the canonical chain.
func TestFillStaleGeneration(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	skipGap(t, f, rpc)
	if st, err := f.FillStep(ctx); err != nil || st != FillProgressed {
		t.Fatalf("first batch: %v %v", st, err)
	}
	before := holesOf(t, store)
	rpc.hooks["HeadersByNumbers"] = rewound(t, f, store)
	st, err := f.FillStep(ctx)
	if err != nil || st != FillIdle {
		t.Fatalf("stale fill step: %v %v", st, err)
	}
	after := holesOf(t, store)
	if len(after) != 1 || after[0].Next != before[0].Next {
		t.Fatalf("the cursor must not advance on discarded work: %+v -> %+v", before, after)
	}
	if b, _ := store.BlockByNumber(ctx, 4663, 1011); b != nil {
		t.Fatal("discarded work must not be written")
	}
	delete(rpc.hooks, "HeadersByNumbers")
	if st, err := f.FillStep(ctx); err != nil || st != FillProgressed {
		t.Fatalf("the retry must progress: %v %v", st, err)
	}
	if holes := holesOf(t, store); holes[0].Next != 1021 {
		t.Fatalf("cursor after the retry: %+v", holes)
	}
}

// TestFillRestartedByReorg: a rewind that reaches into a hole being filled
// takes the blocks the filler wrote with it, so the hole's cursor goes
// back to the start of the range.
func TestFillRestartedByReorg(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	skipGap(t, f, rpc)
	if st, err := f.FillStep(ctx); err != nil || st != FillProgressed {
		t.Fatalf("first batch: %v %v", st, err)
	}
	if holes := holesOf(t, store); holes[0].Next != 1011 {
		t.Fatalf("cursor: %+v", holes)
	}
	// The chain reorganizes inside what the filler wrote: the node reports
	// a rolled back head whose stored hash differs, and the rewind lands
	// below the hole's cursor.
	rpc.fork(1005, "z")
	rpc.setHead(1005)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	holes := holesOf(t, store)
	if len(holes) != 1 || holes[0].From != 1001 || holes[0].Next != 0 {
		t.Fatalf("the reorg must restart the hole: %+v", holes)
	}
	if b, _ := store.BlockByNumber(ctx, 4663, 1005); b != nil {
		t.Fatal("the rewind must remove what the filler wrote above the ancestor")
	}
	if b, _ := store.BlockByNumber(ctx, 4663, 1004); b == nil {
		t.Fatal("blocks at or below the ancestor survive")
	}
	// A rewind that does not reach what the filler wrote leaves the cursor
	// alone: those blocks are still on the canonical chain.
	if err := store.WithChainTx(ctx, 4663, func(s db.Store) error {
		return f.saveHoles(ctx, s, []hole{{From: 1001, To: 1149, Next: 1011}})
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.WithChainTx(ctx, 4663, func(s db.Store) error {
		return f.rewindHoles(ctx, s, 1010)
	}); err != nil {
		t.Fatal(err)
	}
	if holes := holesOf(t, store); holes[0].Next != 1011 {
		t.Fatalf("an ancestor at the end of the filled part leaves it alone: %+v", holes)
	}
}

// TestFillYieldsWhileCatchingUp: the fast loop's catch-up owns the budget,
// so the filler stands down until it is done.
func TestFillYieldsWhileCatchingUp(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	skipGap(t, f, rpc)
	f.catchingUp.Store(true)
	if st, err := f.FillStep(ctx); err != nil || st != FillIdle {
		t.Fatalf("catching up: %v %v", st, err)
	}
	if len(rpc.headerCalls) != 0 {
		t.Fatalf("no header may be fetched while the fast loop catches up: %v", rpc.headerCalls)
	}
	f.catchingUp.Store(false)
	// The same while the last tick found the stored head more than a
	// header batch behind: the filler's batches would queue ahead of the
	// catch-up's and turn the lag into another skipped gap.
	f.behind.Store(uint64(f.cfg.HeaderBatchSize) + 1)
	if st, err := f.FillStep(ctx); err != nil || st != FillIdle || len(rpc.headerCalls) != 0 {
		t.Fatalf("behind the chain: %v %v %v", st, err, rpc.headerCalls)
	}
	f.behind.Store(uint64(f.cfg.HeaderBatchSize))
	if st, err := f.FillStep(ctx); err != nil || st != FillProgressed {
		t.Fatalf("after the catch-up: %v %v", st, err)
	}
	// Without spare budget the smallest batch still goes, queued for its
	// turn at the pacer, exactly like the backfill.
	rpc.available = 0
	rpc.headerCalls = nil
	if st, err := f.FillStep(ctx); err != nil || st != FillProgressed {
		t.Fatalf("no spare budget: %v %v", st, err)
	}
	if calls := rpc.headerCalls; len(calls) != 1 || len(calls[0]) != minBackfillBatch {
		t.Fatalf("the smallest batch goes without spare budget: %v", calls)
	}
}

// TestFillChainMustLink: headers that do not build on the stored block
// before the gap, or on the stored block after it, are retried rather than
// written.
func TestFillChainMustLink(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	skipGap(t, f, rpc)
	// The node now serves a different chain from block 1000 on, so the
	// first header of the range does not build on the stored block 1000.
	rpc.fork(1000, "z")
	if _, err := f.FillStep(ctx); err == nil {
		t.Fatal("a header that does not build on the stored block must be an error")
	}
	if b, _ := store.BlockByNumber(ctx, 4663, 1001); b != nil {
		t.Fatal("nothing may be written when the chain does not link")
	}
	if holes := holesOf(t, store); holes[0].Next != 0 {
		t.Fatalf("the cursor must not move: %+v", holes)
	}
	// A range that links to the stored block before it but not to the
	// stored head after it is refused just the same.
	rpc.mu.Lock()
	rpc.forks = nil
	rpc.mu.Unlock()
	rows := store.BlockRows[4663]
	head := rows[1150]
	head.ParentHash = "0xdead"
	rows[1150] = head
	for i := 0; i < 14; i++ {
		if _, err := f.FillStep(ctx); err != nil {
			t.Fatalf("batch %d: %v", i, err)
		}
	}
	if _, err := f.FillStep(ctx); err == nil {
		t.Fatal("the last batch must link to the stored head after the gap")
	}
	if b, _ := store.BlockByNumber(ctx, 4663, 1149); b != nil {
		t.Fatal("the batch that does not link must not be written")
	}
	if holes := holesOf(t, store); holes[0].Next != 1141 {
		t.Fatalf("the cursor stops before the batch that does not link: %+v", holes)
	}
}

// TestFillErrors: every failure on the way surfaces instead of being
// written around.
func TestFillErrors(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	skipGap(t, f, rpc)

	store.FailOn["GetState"] = true
	if _, err := f.FillStep(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("generation read: %v", err)
	}
	store.FailOn["GetState"] = false
	// An unreadable checkpoint is not fatal: it is a record of what is
	// missing, so the step reports nothing to do.
	_ = store.SetState(ctx, 4663, db.StateHoles, "{bad")
	if st, err := f.FillStep(ctx); err != nil || st != FillNone {
		t.Fatalf("unreadable holes: %v %v", st, err)
	}
	if err := store.WithChainTx(ctx, 4663, func(s db.Store) error {
		return f.saveHoles(ctx, s, []hole{{From: 1001, To: 1149}})
	}); err != nil {
		t.Fatal(err)
	}
	store.FailOn["BlockByNumber"] = true
	if _, err := f.FillStep(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("state lookup: %v", err)
	}
	store.FailOn["BlockByNumber"] = false
	rpc.errs["HeadersByNumbers"] = errRPC
	if _, err := f.FillStep(ctx); !errors.Is(err, errRPC) {
		t.Fatalf("header fetch: %v", err)
	}
	delete(rpc.errs, "HeadersByNumbers")
	for _, method := range []string{"UpsertBlocks", "RebuildBuckets", "FoldBuckets"} {
		store.FailOn[method] = true
		if _, err := f.FillStep(ctx); !errors.Is(err, dbtest.ErrInjected) {
			t.Fatalf("%s: %v", method, err)
		}
		store.FailOn[method] = false
	}
	// The progress is recorded inside the commit, reading the checkpoint
	// again so a hole another writer appended is not lost: a failure there
	// aborts the whole batch.
	reads := 0
	store.Hooks["GetState"] = func() {
		reads++
		if reads == 4 {
			store.SetFailure("GetState", true)
		}
	}
	if _, err := f.FillStep(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("progress write: %v", err)
	}
	delete(store.Hooks, "GetState")
	store.SetFailure("GetState", false)
	if holes := holesOf(t, store); len(holes) != 1 || holes[0].Next != 0 {
		t.Fatalf("an aborted batch leaves the cursor alone: %+v", holes)
	}
	// A follower that cannot even register its network never fetches.
	bad := dbtest.New()
	bad.FailOn["UpsertNetwork"] = true
	if _, err := newTestFollower(t, rpc, bad).FillStep(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("init: %v", err)
	}
	// Before the first sample of the process no pricer shape is known, so
	// the filler waits rather than guessing one.
	f3 := newTestFollower(t, rpc, store)
	rpc.headerCalls = nil
	if st, err := f3.FillStep(ctx); err != nil || st != FillNone {
		t.Fatalf("before the first sample: %v %v", st, err)
	}
	if len(rpc.headerCalls) != 0 {
		t.Fatalf("no call before the first sample: %v", rpc.headerCalls)
	}
}

// TestFillWithoutStoredTail: a gap whose following block is no longer
// stored (a rewind cut the chain there) still fills. There is simply no
// sampled state to record the replay error against.
func TestFillWithoutStoredTail(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	skipGap(t, f, rpc)
	for i := 0; i < 14; i++ {
		if _, err := f.FillStep(ctx); err != nil {
			t.Fatalf("batch %d: %v", i, err)
		}
	}
	// The last batch looks the block after the range up: a failure there
	// aborts the batch rather than filling without it.
	reads := 0
	store.Hooks["BlockByNumber"] = func() {
		reads++
		if reads == 2 {
			store.SetFailure("BlockByNumber", true)
		}
	}
	if _, err := f.FillStep(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("the block after the range: %v", err)
	}
	delete(store.Hooks, "BlockByNumber")
	store.SetFailure("BlockByNumber", false)
	delete(store.BlockRows[4663], 1150)
	fillAll(t, f, 40)
	if holesOf(t, store) != nil {
		t.Fatalf("the gap must fill without its following block: %+v", holesOf(t, store))
	}
	if b, _ := store.BlockByNumber(ctx, 4663, 1149); b == nil {
		t.Fatal("the last block of the range must be written")
	}
	if b, _ := store.BlockByNumber(ctx, 4663, 1150); b != nil {
		t.Fatal("the filler must not invent the block after the range")
	}
}

// TestFillRefusesMismatchedState: a stored block whose backlogs do not
// match the pricer shape in force is no state to replay from. A stored
// head after the range carrying another shape is not one to anchor to
// either, but that only leaves its replay error unrecorded: the range
// itself is still indexed.
func TestFillRefusesMismatchedState(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	skipGap(t, f, rpc)
	rows := store.BlockRows[4663]
	prev := rows[1000]
	prev.Backlogs = db.Uint64Array{1}
	rows[1000] = prev
	if st, err := f.FillStep(ctx); err != nil || st != FillNone {
		t.Fatalf("a block that does not match the shape is no state: %v %v", st, err)
	}
	if len(rpc.headerCalls) != 0 {
		t.Fatalf("nothing may be fetched for it: %v", rpc.headerCalls)
	}
	prev.Backlogs = db.Uint64Array{3_111_506, 11_194_391_810_886}
	rows[1000] = prev
	head := rows[1150]
	head.Backlogs = db.Uint64Array{1}
	rows[1150] = head
	fillAll(t, f, 40)
	if holesOf(t, store) != nil {
		t.Fatalf("the range must still be indexed: %+v", holesOf(t, store))
	}
	if b, _ := store.BlockByNumber(ctx, 4663, 1149); b == nil {
		t.Fatal("the last block of the range must be written")
	}
	after, _ := store.BlockByNumber(ctx, 4663, 1150)
	if after.PredictedBaseFee.Cmp(after.BaseFee.BigInt()) != 0 {
		t.Fatalf("a head with other backlogs must not be anchored to: %+v", after)
	}
}

// TestFillBelowTheBucketBoundary: a hole older than the hour of the first
// live block folds its buckets additively, exactly as the backfill does,
// so it cannot wipe what the backfill folded there. The stored head after
// the range is a live row and is written back either way.
func TestFillBelowTheBucketBoundary(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	skipGap(t, f, rpc)
	f.mu.Lock()
	f.liveStart = &liveStart{Block: 1150, TS: baseTime.Add(2 * time.Hour).Unix()}
	f.mu.Unlock()
	before := blockCountIn(store, db.Resolution1m)
	if st, err := f.FillStep(ctx); err != nil || st != FillProgressed {
		t.Fatalf("fill step: %v %v", st, err)
	}
	if b, _ := store.BlockByNumber(ctx, 4663, 1001); b != nil {
		t.Fatal("blocks below the boundary belong to the additive folds, not to rows")
	}
	if got := blockCountIn(store, db.Resolution1m); got != before+10 {
		t.Fatalf("the batch must fold into the buckets: %d -> %d", before, got)
	}
}

// TestFillBrokenLinksInsideTheBatch: headers that do not link to each
// other are a reorg during the fetch, retried rather than replayed, and an
// endpoint that answers with nothing is an error, not an empty range.
func TestFillBrokenLinksInsideTheBatch(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	skipGap(t, f, rpc)
	rpc.mu.Lock()
	rpc.parentOverride = map[uint64]string{1005: "0xbad"}
	rpc.mu.Unlock()
	if _, err := f.FillStep(ctx); err == nil {
		t.Fatal("headers that do not link must be retried")
	}
	if b, _ := store.BlockByNumber(ctx, 4663, 1001); b != nil {
		t.Fatal("nothing may be written from a range that does not link")
	}
	rpc.mu.Lock()
	rpc.parentOverride = nil
	rpc.noHeaders = true
	rpc.mu.Unlock()
	if _, err := f.FillStep(ctx); err == nil {
		t.Fatal("an endpoint answering with nothing must be an error")
	}
	rpc.mu.Lock()
	rpc.noHeaders = false
	rpc.headerShift = 1
	rpc.mu.Unlock()
	if _, err := f.FillStep(ctx); err == nil {
		t.Fatal("headers for other blocks than the ones asked for must be an error")
	}
	rpc.mu.Lock()
	rpc.headerShift = 0
	rpc.mu.Unlock()
	if st, err := f.FillStep(ctx); err != nil || st != FillProgressed {
		t.Fatalf("the retry must progress: %v %v", st, err)
	}
}

// TestFillFinishesTheHoleItStarted: a hole already being filled is
// completed before a newer one is begun, or a network that keeps skipping
// would start every new hole and finish none.
func TestFillFinishesTheHoleItStarted(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	skipGap(t, f, rpc)
	if st, err := f.FillStep(ctx); err != nil || st != FillProgressed {
		t.Fatalf("first step: %v %v", st, err)
	}
	first := holesOf(t, store)
	if len(first) != 1 || first[0].Next == 0 {
		t.Fatalf("the first hole must be in progress: %+v", first)
	}
	rpc.setHead(1400)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if holes := holesOf(t, store); len(holes) != 2 || holes[1].From != 1151 {
		t.Fatalf("a second, newer gap: %+v", holes)
	}
	rpc.headerCalls = nil
	if st, err := f.FillStep(ctx); err != nil || st != FillProgressed {
		t.Fatalf("next step: %v %v", st, err)
	}
	if calls := rpc.headerCalls; len(calls) != 1 || calls[0][0] != first[0].Start() {
		t.Fatalf("the started hole continues at its cursor %d: %v", first[0].Start(), calls)
	}
	fillAll(t, f, 60)
	if holesOf(t, store) != nil {
		t.Fatalf("both gaps fill: %+v", holesOf(t, store))
	}
}

// TestTickTracksHowFarBehind: the tick records the stored head's lag and
// clears it once it has caught up or skipped, so history work resumes.
func TestTickTracksHowFarBehind(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if f.behind.Load() != 0 || f.historyMustWait() {
		t.Fatalf("caught up after a tick: behind %d", f.behind.Load())
	}
	rpc.setHead(gapHead)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// The gap was skipped and the head seeded: caught up again.
	if f.behind.Load() != 0 {
		t.Fatalf("behind after a skip: %d", f.behind.Load())
	}
	// A failing tick leaves the lag recorded.
	rpc.setHead(gapHead + 30)
	rpc.errs["HeadersByNumbers"] = errRPC
	if err := f.Tick(ctx); err == nil {
		t.Fatal("expected the catch-up to fail")
	}
	if f.behind.Load() != 30 || !f.historyMustWait() {
		t.Fatalf("behind after a failed catch-up: %d", f.behind.Load())
	}
	delete(rpc.errs, "HeadersByNumbers")
}
