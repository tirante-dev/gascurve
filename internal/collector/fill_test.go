package collector

import (
	"context"
	"database/sql"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/db/dbtest"
	"github.com/tirante-dev/gascurve/internal/model"
	"github.com/tirante-dev/gascurve/internal/nitro"
	"github.com/tirante-dev/gascurve/internal/pricer"
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

func advanceRetry(f *Follower) {
	now := f.now().Add(10 * time.Minute)
	f.now = func() time.Time { return now }
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
	if len(holes) != 1 || holes[0].From != 1001 || holes[0].To != 1149 || holes[0].Lifecycle != rangePending || holes[0].Reason != reasonCatchUpLimit {
		t.Fatalf("a skipped gap is queued work: %+v", holes)
	}
	if holes[0].PredecessorAt != timestampString(tsFor(1000)) || holes[0].SuccessorAt != timestampString(tsFor(1150)) {
		t.Fatalf("the skipped interval must retain its time bounds: %+v", holes[0])
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
		return f.rewindHoles(ctx, s, 1010, false)
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
	for i := 1; i < maxRecoveryDeferrals-1; i++ {
		if st, err := f.FillStep(ctx); err != nil || st != FillIdle || len(rpc.headerCalls) != 0 {
			t.Fatalf("priority deferral %d: %v %v %v", i, st, err, rpc.headerCalls)
		}
	}
	if st, err := f.FillStep(ctx); err != nil || st != FillProgressed || len(rpc.headerCalls) != 1 {
		t.Fatalf("bounded recovery turn: %v %v %v", st, err, rpc.headerCalls)
	}
	rpc.headerCalls = nil
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
	rpc.available = 14
	rpc.headerCalls = nil
	if st, err := f.FillStep(ctx); err != nil || st != FillProgressed {
		t.Fatalf("receipt-weighted spare budget: %v %v", st, err)
	}
	if calls := rpc.headerCalls; len(calls) != 1 || len(calls[0]) != 7 {
		t.Fatalf("14 spare calls must fetch 7 blocks: %v", calls)
	}
}

// TestFillRetryLifecycle persists the failure count, error, attempt time and
// next retry across steps, then clears only the active failure when recovery
// advances. The historical retry count remains available to status readers.
func TestFillRetryLifecycle(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	skipGap(t, f, rpc)
	rpc.errs["HeadersByNumbers"] = errRPC
	if _, err := f.FillStep(ctx); !errors.Is(err, errRPC) {
		t.Fatalf("fill failure: %v", err)
	}
	holes := holesOf(t, store)
	if len(holes) != 1 || holes[0].Lifecycle != rangeRetrying || holes[0].RetryCount != 1 || holes[0].LastError == "" || holes[0].LastAttemptAt == "" || holes[0].NextRetryAt == "" {
		t.Fatalf("durable retry metadata: %+v", holes)
	}
	delete(rpc.errs, "HeadersByNumbers")
	advanceRetry(f)
	if st, err := f.FillStep(ctx); err != nil || st != FillProgressed {
		t.Fatalf("retry progress: %v %v", st, err)
	}
	holes = holesOf(t, store)
	if len(holes) != 1 || holes[0].Lifecycle != rangePending || holes[0].RetryCount != 1 || holes[0].LastError != "" || holes[0].NextRetryAt != "" || holes[0].CursorAt != timestampString(tsFor(1010)) {
		t.Fatalf("retry after progress: %+v", holes)
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
	advanceRetry(f)
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
	// An unreadable legacy checkpoint is a startup error and is retained for
	// repair instead of being treated as an empty range set.
	legacy := dbtest.New()
	_ = legacy.SetState(ctx, 4663, db.StateHoles, "{bad")
	if err := newTestFollower(t, rpc, legacy).ensureInit(ctx); err == nil {
		t.Fatal("unreadable legacy holes must stop initialization")
	}
	if raw, ok, _ := legacy.GetState(ctx, 4663, db.StateHoles); !ok || raw != "{bad" {
		t.Fatalf("unreadable legacy checkpoint must remain intact: %q %v", raw, ok)
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
		advanceRetry(f)
		store.FailOn[method] = true
		if _, err := f.FillStep(ctx); !errors.Is(err, dbtest.ErrInjected) {
			t.Fatalf("%s: %v", method, err)
		}
		store.FailOn[method] = false
	}
	// The progress is recorded inside the commit, reading the checkpoint
	// again so a hole another writer appended is not lost: a failure there
	// aborts the whole batch.
	store.SetFailure("ReplaceMissingRanges", true)
	advanceRetry(f)
	if _, err := f.FillStep(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("progress write: %v", err)
	}
	store.SetFailure("ReplaceMissingRanges", false)
	if holes := holesOf(t, store); len(holes) != 1 || holes[0].Next != 0 {
		t.Fatalf("an aborted batch leaves the cursor alone: %+v", holes)
	}
	// A follower that cannot even register its network never fetches.
	bad := dbtest.New()
	bad.FailOn["UpsertNetwork"] = true
	if _, err := newTestFollower(t, rpc, bad).FillStep(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("init: %v", err)
	}
	// A restarted follower that has not sampled yet still fills: the shape
	// comes from what was recorded before the range, never from the live
	// sample.
	f3 := newTestFollower(t, rpc, store)
	due := f.now().Add(10 * time.Minute)
	f3.now = func() time.Time { return due }
	rpc.headerCalls = nil
	if st, err := f3.FillStep(ctx); err != nil || st != FillProgressed {
		t.Fatalf("a restart fills from the recorded state: %v %v", st, err)
	}
	// With no recorded state at all before the range (no sample, no
	// constraint set) there is nothing to replay from and no call is made.
	store.SampleRows, store.SetRows = nil, nil
	if err := store.WithChainTx(ctx, 4663, func(s db.Store) error {
		return f.saveHoles(ctx, s, []hole{{From: 1001, To: 1149}})
	}); err != nil {
		t.Fatal(err)
	}
	f4 := newTestFollower(t, rpc, store)
	rpc.headerCalls = nil
	if st, err := f4.FillStep(ctx); err != nil || st != FillNone {
		t.Fatalf("nothing describes the pricer before the range: %v %v", st, err)
	}
	if len(rpc.headerCalls) != 0 {
		t.Fatalf("no call without a state to replay from: %v", rpc.headerCalls)
	}
	// A read failure on the state sample surfaces.
	store.FailOn["StateSampleAt"] = true
	if _, err := f4.FillStep(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("state sample lookup: %v", err)
	}
	store.FailOn["StateSampleAt"] = false
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
	advanceRetry(f)
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
	advanceRetry(f)
	if _, err := f.FillStep(ctx); err == nil {
		t.Fatal("an endpoint answering with nothing must be an error")
	}
	rpc.mu.Lock()
	rpc.noHeaders = false
	rpc.headerShift = 1
	rpc.mu.Unlock()
	advanceRetry(f)
	if _, err := f.FillStep(ctx); err == nil {
		t.Fatal("headers for other blocks than the ones asked for must be an error")
	}
	rpc.mu.Lock()
	rpc.headerShift = 0
	rpc.mu.Unlock()
	advanceRetry(f)
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

// TestFillBelowTheBoundaryResumes: a hole older than the hour of the first
// live block stores no block rows at all, so its continuation cannot look
// one up at the cursor. The hole carries the replay state itself and every
// batch continues from it, and the stored block after the range, which is
// below the boundary too, has its row rewritten without its additive
// bucket being rebuilt from the sparse rows around it.
func TestFillBelowTheBoundaryResumes(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	skipGap(t, f, rpc)
	f.mu.Lock()
	f.liveStart = &liveStart{Block: 1150, TS: baseTime.Add(2 * time.Hour).Unix()}
	f.mu.Unlock()
	before := blockCountIn(store, db.Resolution1m)
	if steps := fillAll(t, f, 40); steps != 15 {
		t.Fatalf("149 blocks in batches of 10 below the boundary: %d steps", steps)
	}
	if holesOf(t, store) != nil {
		t.Fatalf("the hole must be filled: %+v", holesOf(t, store))
	}
	if b, _ := store.BlockByNumber(ctx, 4663, 1075); b != nil {
		t.Fatal("blocks below the boundary belong to the additive folds, not to rows")
	}
	if got := blockCountIn(store, db.Resolution1m); got != before+149 {
		t.Fatalf("every block of the range counted once: %d -> %d", before, got)
	}
	if got := blockCountIn(store, db.Resolution1h); got != before+149 {
		t.Fatalf("hour buckets: %d -> %d", before, got)
	}
	// The stored block after the range still gets the replay's prediction,
	// so the error of the reconstruction is recorded where it can be seen.
	tail, _ := store.BlockByNumber(ctx, 4663, 1150)
	if tail == nil || tail.PredictedBaseFee.Cmp(tail.BaseFee.BigInt()) == 0 {
		t.Fatalf("the tail keeps its row and gains the replay error: %+v", tail)
	}
}

// TestFillFoldsOnceAcrossARewind: a rewind resets a hole's cursor, but the
// additive buckets it folded below the bucket boundary are not rebuilt
// from rows, so the refilled batches must not count those blocks a second
// time. The watermark is cleared only when the rewind deleted the buckets.
func TestFillFoldsOnceAcrossARewind(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	skipGap(t, f, rpc)
	f.mu.Lock()
	f.liveStart = &liveStart{Block: 1150, TS: baseTime.Add(2 * time.Hour).Unix()}
	f.mu.Unlock()
	before := blockCountIn(store, db.Resolution1m)
	for i := 0; i < 2; i++ {
		if st, err := f.FillStep(ctx); err != nil || st != FillProgressed {
			t.Fatalf("batch %d: %v %v", i, st, err)
		}
	}
	if got := blockCountIn(store, db.Resolution1m); got != before+20 {
		t.Fatalf("two batches folded: %d -> %d", before, got)
	}
	if holes := holesOf(t, store); len(holes) != 1 || holes[0].Next != 1021 || holes[0].Folded != 1021 {
		t.Fatalf("the fold watermark follows the cursor: %+v", holes)
	}
	// A rewind that reaches into what the filler wrote restarts the hole.
	if err := store.WithChainTx(ctx, 4663, func(s db.Store) error {
		return f.rewindHoles(ctx, s, 1005, false)
	}); err != nil {
		t.Fatal(err)
	}
	holes := holesOf(t, store)
	if len(holes) != 1 || holes[0].Next != 0 || holes[0].State != nil || holes[0].Folded != 1021 {
		t.Fatalf("the cursor restarts but the fold watermark survives: %+v", holes)
	}
	fillAll(t, f, 40)
	if got := blockCountIn(store, db.Resolution1m); got != before+149 {
		t.Fatalf("a refilled range must not be folded twice: %d -> %d", before, got)
	}
	// When the rewind deleted the additive buckets themselves, everything
	// the hole folded into them went with them and the watermark resets.
	if err := store.WithChainTx(ctx, 4663, func(s db.Store) error {
		if err := f.saveHoles(ctx, s, []hole{{From: 1001, To: 1149, Next: 1021, Folded: 1021}}); err != nil {
			return err
		}
		return f.rewindHoles(ctx, s, 1005, true)
	}); err != nil {
		t.Fatal(err)
	}
	if holes := holesOf(t, store); len(holes) != 1 || holes[0].Folded != 0 {
		t.Fatalf("deleted buckets clear the watermark: %+v", holes)
	}
}

// TestFillStrandedByRetention: the block a hole is anchored to can be
// pruned. A hole that has not started yet is reclassified rather than left
// queued for ever, is taken up again if the block comes back, and once it
// carries its own replay state it fills on even after the rows it was
// anchored to are gone.
func TestFillStrandedByRetention(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	skipGap(t, f, rpc)
	anchor := store.BlockRows[4663][1000]
	delete(store.BlockRows[4663], 1000)
	rpc.headerCalls = nil
	if st, err := f.FillStep(ctx); err != nil || st != FillNone {
		t.Fatalf("nothing to replay from: %v %v", st, err)
	}
	if holes := holesOf(t, store); len(holes) != 1 || holes[0].Reason != reasonNoState {
		t.Fatalf("a stranded hole must not stay queued for ever: %+v", holes)
	}
	if len(rpc.headerCalls) != 0 {
		t.Fatalf("a stranded hole costs no call: %v", rpc.headerCalls)
	}
	// The anchor reappears (the backfill reached it): the mark comes off.
	store.BlockRows[4663][1000] = anchor
	if st, err := f.FillStep(ctx); err != nil || st != FillProgressed {
		t.Fatalf("the hole becomes work again: %v %v", st, err)
	}
	holes := holesOf(t, store)
	if len(holes) != 1 || holes[0].Reason != "" || holes[0].State == nil || holes[0].State.Block != 1010 {
		t.Fatalf("the batch commits the replay state with the cursor: %+v", holes)
	}
	// Retention now takes the anchor and everything the filler wrote: the
	// carried state stands on its own and the next batch continues.
	for n := uint64(1000); n <= 1010; n++ {
		delete(store.BlockRows[4663], n)
	}
	rpc.headerCalls = nil
	if st, err := f.FillStep(ctx); err != nil || st != FillProgressed {
		t.Fatalf("a hole carrying its state survives retention: %v %v", st, err)
	}
	if calls := rpc.headerCalls; len(calls) != 1 || calls[0][0] != 1011 {
		t.Fatalf("the fill continues at the cursor: %v", calls)
	}
	// A carried state describing another block at that height than the
	// stored one came from a fork that is gone: the stored block wins.
	holes = holesOf(t, store)
	holes[0].State.Hash = "0xdead"
	store.BlockRows[4663][1020] = anchor
	row := store.BlockRows[4663][1020]
	row.Number, row.Hash, row.TS = 1020, rpc.hashFor(1020), time.Unix(int64(tsFor(1020)), 0).UTC()
	store.BlockRows[4663][1020] = row
	if err := store.WithChainTx(ctx, 4663, func(s db.Store) error { return f.saveHoles(ctx, s, holes) }); err != nil {
		t.Fatal(err)
	}
	if st, err := f.FillStep(ctx); err != nil || st != FillProgressed {
		t.Fatalf("the stored block is preferred: %v %v", st, err)
	}
	// Marking a range changes nothing when no entry starts there, and a
	// checkpoint that cannot be read fails the mark rather than dropping
	// the ranges it holds.
	if err := store.WithChainTx(ctx, 4663, func(s db.Store) error {
		return f.markHoles(ctx, s, map[uint64]string{999: reasonExpired})
	}); err != nil {
		t.Fatal(err)
	}
	if holes := holesOf(t, store); len(holes) != 1 || holes[0].Reason != "" {
		t.Fatalf("a mark for an unknown range must change nothing: %+v", holes)
	}
	store.FailOn["MissingRanges"] = true
	err := store.WithChainTx(ctx, 4663, func(s db.Store) error {
		return f.markHoles(ctx, s, map[uint64]string{1001: reasonNoState})
	})
	store.FailOn["MissingRanges"] = false
	if !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("unreadable checkpoint: %v", err)
	}
}

// TestHolesAreMergedAndDurable: ranges recorded twice, overlapping or
// touching become one entry, ranges of different classes stay apart, and
// no range is expired or forgotten as the durable set grows.
func TestHolesAreMergedAndDurable(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New()
	f := newTestFollower(t, newFakeRPC(1000), store)
	if err := f.ensureInit(ctx); err != nil {
		t.Fatal(err)
	}
	record := func(h hole) {
		t.Helper()
		if err := store.WithChainTx(ctx, 4663, func(s db.Store) error { return f.recordHole(ctx, s, h) }); err != nil {
			t.Fatal(err)
		}
	}
	record(hole{From: 100, To: 199})
	record(hole{From: 150, To: 249}) // a reorg re-records an overlapping range
	record(hole{From: 250, To: 299}) // adjacent
	holes := holesOf(t, store)
	if len(holes) != 1 || holes[0].From != 100 || holes[0].To != 299 {
		t.Fatalf("overlapping and adjacent ranges merge: %+v", holes)
	}
	// A range nothing can be replayed into never swallows queued work.
	record(hole{From: 300, To: 399, Reason: reasonNoState})
	holes = holesOf(t, store)
	if len(holes) != 2 || holes[1].From != 300 || holes[1].Reason != reasonNoState {
		t.Fatalf("classes stay apart: %+v", holes)
	}
	// A merge keeps the progress of the range it starts at and the highest
	// fold watermark, so nothing is folded into a bucket twice.
	if err := store.WithChainTx(ctx, 4663, func(s db.Store) error {
		return f.saveHoles(ctx, s, []hole{{From: 100, To: 199, Next: 150, Folded: 150}, {From: 180, To: 250, Folded: 170}})
	}); err != nil {
		t.Fatal(err)
	}
	holes = holesOf(t, store)
	if len(holes) != 1 || holes[0].To != 250 || holes[0].Next != 150 || holes[0].Folded != 170 {
		t.Fatalf("merged progress: %+v", holes)
	}
	// A merged entry keeps the bounds of its own start block. The later
	// range's predecessor names a block inside the merged interval, so it
	// only carries over when both ranges start at the same block.
	predAt := baseTime.Add(-time.Hour).Format(time.RFC3339)
	if err := store.WithChainTx(ctx, 4663, func(s db.Store) error {
		return f.saveHoles(ctx, s, []hole{{From: 100, To: 199}, {From: 180, To: 250, PredecessorAt: predAt}})
	}); err != nil {
		t.Fatal(err)
	}
	holes = holesOf(t, store)
	if len(holes) != 1 || holes[0].PredecessorAt != "" {
		t.Fatalf("a later range must not lend its predecessor bound: %+v", holes)
	}
	if err := store.WithChainTx(ctx, 4663, func(s db.Store) error {
		return f.saveHoles(ctx, s, []hole{{From: 100, To: 199}, {From: 100, To: 250, PredecessorAt: predAt}})
	}); err != nil {
		t.Fatal(err)
	}
	holes = holesOf(t, store)
	if len(holes) != 1 || holes[0].PredecessorAt != predAt {
		t.Fatalf("a shared start block shares the predecessor bound: %+v", holes)
	}
	// Retrying is a transient lifecycle within the fillable class. A repeated
	// skip that overlaps it still produces one row and keeps the retry state.
	retryAt := baseTime.Add(time.Minute).Format(time.RFC3339)
	if err := store.WithChainTx(ctx, 4663, func(s db.Store) error {
		return f.saveHoles(ctx, s, []hole{
			{From: 100, To: 199, Lifecycle: rangePending},
			{From: 150, To: 250, Lifecycle: rangeRetrying, RetryCount: 2, LastAttemptAt: retryAt, NextRetryAt: retryAt},
		})
	}); err != nil {
		t.Fatal(err)
	}
	holes = holesOf(t, store)
	if len(holes) != 1 || holes[0].To != 250 || holes[0].Lifecycle != rangeRetrying || holes[0].RetryCount != 2 || holes[0].NextRetryAt != retryAt {
		t.Fatalf("overlapping retry lifecycle: %+v", holes)
	}
	// More than the old queue cap stays recoverable in full.
	const oldPendingCap = 64
	many := make([]hole, 0, oldPendingCap+10)
	for i := 0; i < oldPendingCap+10; i++ {
		start := uint64(10_000 + i*10)
		many = append(many, hole{From: start, To: start + 4})
	}
	if err := store.WithChainTx(ctx, 4663, func(s db.Store) error { return f.saveHoles(ctx, s, many) }); err != nil {
		t.Fatal(err)
	}
	holes = holesOf(t, store)
	summary := model.SummarizeHoles(holes)
	if len(holes) != oldPendingCap+10 || summary.Pending != oldPendingCap+10 || summary.Unfillable != 0 {
		t.Fatalf("every queued range stays pending: %+v", summary)
	}
	if summary.Blocks != uint64(5*(oldPendingCap+10)) {
		t.Fatalf("every missing block stays counted: %+v", summary)
	}
	for i := 0; i < 10; i++ {
		if holes[i].Lifecycle != rangePending || holes[i].Reason == reasonExpired {
			t.Fatalf("old ranges must remain recoverable: %+v", holes[:12])
		}
	}
	// More than the old whole-checkpoint cap stays recorded too.
	const oldCheckpointCap = 256
	huge := make([]hole, 0, oldCheckpointCap+5)
	for i := 0; i < oldCheckpointCap+5; i++ {
		start := uint64(100_000 + i*10)
		huge = append(huge, hole{From: start, To: start + 4, Reason: reasonNoState})
	}
	if err := store.WithChainTx(ctx, 4663, func(s db.Store) error { return f.saveHoles(ctx, s, huge) }); err != nil {
		t.Fatal(err)
	}
	holes = holesOf(t, store)
	if len(holes) != oldCheckpointCap+5 || holes[0].From != 100_000 {
		t.Fatalf("durable rows must not be forgotten: %d entries from %d", len(holes), holes[0].From)
	}
}

// TestLegacyHolesAreImported moves every old checkpoint entry into durable
// rows. Ranges previously expired by the 64-entry policy become pending again,
// and the source checkpoint disappears only after the complete import.
func TestLegacyHolesAreImported(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New()
	legacy := `[{"from":10,"to":19,"at":"2026-09-06T06:00:00Z"},{"from":30,"to":39,"at":"2026-09-06T06:01:00Z","reason":"expired"},{"from":50,"to":59,"at":"2026-09-06T06:02:00Z","reason":"no state"}]`
	if err := store.SetState(ctx, 4663, db.StateHoles, legacy); err != nil {
		t.Fatal(err)
	}
	f := newTestFollower(t, newFakeRPC(1000), store)
	if err := f.ensureInit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.GetState(ctx, 4663, db.StateHoles); ok {
		t.Fatal("legacy checkpoint must be removed after import")
	}
	holes := holesOf(t, store)
	if len(holes) != 3 || holes[0].Lifecycle != rangePending || holes[1].Lifecycle != rangePending || holes[1].Reason != reasonCatchUpLimit || holes[2].Lifecycle != rangeBlocked || holes[2].Reason != reasonNoState {
		t.Fatalf("imported ranges: %+v", holes)
	}
}

// TestMalformedDurableReplayStateIsRetained verifies that a structurally
// unreadable JSONB replay state cannot make an incomplete range look absent.
func TestMalformedDurableReplayStateIsRetained(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New()
	row := db.MissingRange{
		ChainID: 4663, From: 1001, To: 1010, DetectedAt: baseTime, Lifecycle: rangePending,
		ReplayState: db.JSONB(`[]`),
	}
	if err := store.ReplaceMissingRanges(ctx, 4663, []db.MissingRange{row}); err != nil {
		t.Fatal(err)
	}
	f := newTestFollower(t, newFakeRPC(1000), store)
	if _, err := f.FillStep(ctx); err == nil {
		t.Fatal("malformed replay state must stop recovery")
	}
	rows, err := store.MissingRanges(ctx, 4663)
	if err != nil || len(rows) != 1 || string(rows[0].ReplayState) != `[]` {
		t.Fatalf("malformed row was not retained: %+v %v", rows, err)
	}
}

// TestFillLegacyUsesHistoricalParameters: a legacy chain whose speed limit
// changed after a gap must not have that gap replayed with the parameters
// in force now. The state before the range comes from the newest state
// sample taken at or before it, which carries what was really running, not
// from the live sample: the recorded changes can only be applied forward,
// never reversed.
func TestFillLegacyUsesHistoricalParameters(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	was := &nitro.LegacyParams{SpeedLimit: 7_000_000, Inertia: 102, Tolerance: 10, Backlog: 100_000_000}
	rpc.legacy = was
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// The owner lowers the speed limit inside the range that is about to
	// be skipped, so the live sample reports the new one.
	if _, err := store.InsertOwnerActions(ctx, []db.OwnerAction{{
		ChainID: 4663, BlockNumber: 1100, TxHash: "0xlimit", TS: time.Unix(int64(tsFor(1100)), 0).UTC(),
		Method: methodSetSpeedLimit, Args: db.JSONB(`{"limit":1000000}`),
	}}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	err := f.reloadSetsLocked(ctx)
	f.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	now := &nitro.LegacyParams{SpeedLimit: 1_000_000, Inertia: 102, Tolerance: 10, Backlog: 100_000_000}
	rpc.mu.Lock()
	rpc.legacy = now
	rpc.mu.Unlock()
	rpc.setHead(gapHead)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if holes := holesOf(t, store); len(holes) != 1 || holes[0].From != 1001 {
		t.Fatalf("the gap must be recorded: %+v", holes)
	}
	if st, err := f.FillStep(ctx); err != nil || st != FillProgressed {
		t.Fatalf("fill step: %v %v", st, err)
	}
	holes := holesOf(t, store)
	if len(holes) != 1 || holes[0].State == nil || holes[0].State.Legacy == nil {
		t.Fatalf("the hole must carry its legacy replay state: %+v", holes)
	}
	if got := holes[0].State.Legacy.SpeedLimit; got != was.SpeedLimit {
		t.Fatalf("the replay must start from the parameters in force before the gap, got speed limit %d", got)
	}
	// The blocks before the change are priced with the old parameters.
	prev, _ := store.BlockByNumber(ctx, 4663, 1000)
	replayed := func(l *nitro.LegacyParams) pricer.Result {
		st := &pricer.State{MinBaseFee: new(big.Int).Set(prev.MinBaseFee.Wei.BigInt()),
			Legacy: &pricer.Legacy{SpeedLimit: l.SpeedLimit, Inertia: l.Inertia, Tolerance: l.Tolerance, Backlog: prev.Backlogs[0]}}
		return pricer.Replay(st, uint64(prev.TS.Unix()),
			[]pricer.Block{{Number: 1001, Timestamp: tsFor(1001), GasUsed: gasFor(1001), BaseFee: feeFor(1001)}}, nil)[0]
	}
	wantOld, wantNew := replayed(was), replayed(now)
	if wantOld.Exponent == wantNew.Exponent {
		t.Fatal("the two parameter sets must price the block differently for this test to mean anything")
	}
	row, _ := store.BlockByNumber(ctx, 4663, 1001)
	if row == nil || row.ExponentBips != int64(wantOld.Exponent) || row.PredictedBaseFee.Cmp(wantOld.Predicted) != 0 {
		t.Fatalf("block 1001 priced with the parameters of its own time: %+v (want exponent %d)", row, wantOld.Exponent)
	}
}

// TestFillStateFromRecords: the records the gap filler rebuilds its replay
// state from, and the malformed ones it refuses rather than pricing
// history from a shape it cannot read.
func TestFillStateFromRecords(t *testing.T) {
	f := newTestFollower(t, newFakeRPC(1000), dbtest.New())
	for _, bad := range []*db.StateSample{
		nil,
		{Legacy: db.JSONB(`{bad`)},
		{Constraints: db.JSONB(`{bad`)},
		{Constraints: db.JSONB(`[]`)},
	} {
		if st := f.shapeAtLocked(10, bad); st != nil {
			t.Fatalf("a sample that describes no shape must be refused: %+v -> %+v", bad, st)
		}
	}
	st := f.shapeAtLocked(10, &db.StateSample{Legacy: db.JSONB(`{"speedLimit":7,"inertia":102,"tolerance":10,"backlog":5}`)})
	if st == nil || st.Legacy == nil || st.Legacy.SpeedLimit != 7 || st.Legacy.Inertia != 102 || st.Legacy.Tolerance != 10 {
		t.Fatalf("legacy shape from a sample: %+v", st)
	}
	st = f.shapeAtLocked(10, &db.StateSample{Constraints: db.JSONB(`[{"target":60,"window":15,"backlog":9}]`)})
	if st == nil || len(st.Constraints) != 1 || st.Constraints[0].Target != 60 || st.Constraints[0].Window != 15 || st.Constraints[0].Backlog != 0 {
		t.Fatalf("constraint shape from a sample, backlogs left to the caller: %+v", st)
	}
	for _, bad := range []*model.HoleState{{}, {MinBaseFee: "not a number"}} {
		if got := carriedFillState(bad); got != nil {
			t.Fatalf("an unreadable checkpoint must be refused: %+v -> %+v", bad, got)
		}
	}
	legacy := carriedFillState(&model.HoleState{MinBaseFee: "7", Legacy: &model.LegacyParams{SpeedLimit: 1, Inertia: 2, Tolerance: 3, Backlog: 4}})
	if legacy == nil || legacy.Legacy == nil || legacy.Legacy.Backlog != 4 || legacy.MinBaseFee.Int64() != 7 {
		t.Fatalf("carried legacy state: %+v", legacy)
	}
	cons := carriedFillState(&model.HoleState{Constraints: []model.Constraint{{Target: 1, Window: 2, Backlog: 3}}})
	if cons == nil || len(cons.Constraints) != 1 || cons.Constraints[0].Backlog != 3 {
		t.Fatalf("carried constraint state: %+v", cons)
	}
}

// TestHeaderOfKeepsGasUnitsConsistent: a header synthesized from a stored
// block carries no poster gas, because the row has no per-transaction
// compute boundaries to place an owner action at. Total gas on both sides
// keeps applyActionGas from subtracting a total-gas boundary out of a
// compute-gas block and saturating every backlog.
func TestHeaderOfKeepsGasUnitsConsistent(t *testing.T) {
	block := db.Block{
		Number: 1005, Hash: "0xaa", ParentHash: "0xa9", TS: time.Unix(1_700_000_000, 0).UTC(),
		GasUsed: 1_000_000, BaseFee: db.WeiFromUint64(20_000_000),
		L1Block: 42, TxCount: 3, PosterGas: sql.NullInt64{Int64: 400_000, Valid: true},
	}
	header := headerOf(block)
	if header.PosterGas != nil {
		t.Fatalf("synthesized headers must not carry poster gas: %d", *header.PosterGas)
	}
	if header.ComputeGas() != block.GasUsed {
		t.Fatalf("compute gas = %d, want the stored total %d", header.ComputeGas(), block.GasUsed)
	}
	if _, ok := header.ComputeGasBeforeTx(0); ok {
		t.Fatal("a stored block has no per-transaction compute boundaries")
	}
	st := &pricer.State{MinBaseFee: big.NewInt(1), Constraints: []pricer.Constraint{{Target: 60_000_000, Window: 15}}}
	applyActionGas(st, header.ComputeGas(), []actionBoundary{{txIndex: 2, gasBefore: 900_000}})
	if got := st.Backlogs()[0]; got != block.GasUsed {
		t.Fatalf("backlog = %d, want %d", got, block.GasUsed)
	}
}

// TestApplyActionGasClampsBoundaries: a boundary past the gas the block
// contributes is clamped instead of underflowing, so no backlog is
// saturated by a boundary resolved against another gas basis.
func TestApplyActionGasClampsBoundaries(t *testing.T) {
	st := &pricer.State{MinBaseFee: big.NewInt(1), Constraints: []pricer.Constraint{{Target: 60_000_000, Window: 15}}}
	applyActionGas(st, 600_000, []actionBoundary{{txIndex: 1, gasBefore: 900_000}, {txIndex: 2, gasBefore: 950_000}})
	if got := st.Backlogs()[0]; got != 600_000 {
		t.Fatalf("backlog = %d, want the block's gas 600000", got)
	}
}

// A row-backed hole whose window has already fallen below the prune frontier
// by the time it is filled. The store declines to rebuild that window, and
// treating the decline as success would report the gap filled with the
// bucket over it never updated. The recovered rows are folded in instead,
// so every block is counted exactly once.
func TestFillFoldsIntoAWindowTheStoreWillNotRebuild(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	skipGap(t, f, rpc)
	// The hole sits inside the minute bucket at 07:01 (blocks 600 through
	// 1199). A frontier at 07:01:30 puts that window, and the quarter-hour
	// and hour over it, below the frontier: all three are declined.
	inside := time.Unix(int64(tsFor(900)), 0).UTC()
	if err := store.SetState(ctx, 4663, db.StatePruneFrontier, inside.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	before := blockCountIn(store, db.Resolution1m)
	fillAll(t, f, 40)
	if holesOf(t, store) != nil {
		t.Fatalf("the gap must finish: %+v", holesOf(t, store))
	}
	if got := blockCountIn(store, db.Resolution1m); got != 151 {
		t.Fatalf("every block counted once: %d (was %d before the fill)", got, before)
	}
}

// After a rewind discarded a window below the frontier there is no bucket to add to. Folding the
// recovered rows would insert a bucket holding them alone, pruned prefix and canonical suffix both
// absent, and the hole's completion would present it as whole. Absent stays absent.
func TestFillLeavesADiscardedWindowAbsent(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	skipGap(t, f, rpc)
	inside := time.Unix(int64(tsFor(900)), 0).UTC()
	if err := store.SetState(ctx, 4663, db.StatePruneFrontier, inside.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	minute := inside.Truncate(time.Minute)
	if err := store.DiscardBucketsBelowFrontier(ctx, 4663, db.Resolution1m, []time.Time{minute}); err != nil {
		t.Fatal(err)
	}
	if before, _ := store.Buckets(ctx, 4663, db.Resolution1m, minute, minute.Add(time.Minute)); len(before) != 0 {
		t.Fatalf("the window was not discarded: %d", len(before))
	}
	fillAll(t, f, 40)
	if holesOf(t, store) != nil {
		t.Fatalf("the gap must finish: %+v", holesOf(t, store))
	}
	if after, _ := store.Buckets(ctx, 4663, db.Resolution1m, minute, minute.Add(time.Minute)); len(after) != 0 {
		t.Fatalf("a fold inserted a partial bucket into a discarded window: %+v", after[0])
	}
}
