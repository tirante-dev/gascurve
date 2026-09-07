package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/db/dbtest"
)

// seedBlocksWithoutPosterGas stores [from, to] the way a collector that did
// not read receipts stored them: every column but poster gas.
func seedBlocksWithoutPosterGas(t *testing.T, rpc *fakeRPC, store *dbtest.MemStore, from, to uint64) []db.Block {
	t.Helper()
	rows := make([]db.Block, 0, to-from+1)
	for n := from; n <= to; n++ {
		rows = append(rows, db.Block{
			ChainID: 4663, Number: n, Hash: rpc.hashFor(n), ParentHash: rpc.hashFor(n - 1),
			TS: time.Unix(int64(tsFor(n)), 0).UTC(), GasUsed: gasFor(n),
			BaseFee: db.NewWei(feeFor(n)), TxCount: 1, PricingVersion: db.PricingFull,
			PredictedBaseFee: db.NewWei(feeFor(n)),
			MinBaseFee:       db.NullWei{Wei: db.NewWei(feeFor(n)), Valid: true},
		})
	}
	if err := store.UpsertBlocks(context.Background(), rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

// repairLiveStart is where the tests put the first live block: seeded rows at
// or above it are row-backed and in scope for the repair, ones below it belong
// to the backfill.
const repairLiveStart = uint64(900)

// repairFollower is a follower whose live start sits at repairLiveStart.
func repairFollower(t *testing.T, rpc *fakeRPC, store *dbtest.MemStore) *Follower {
	t.Helper()
	f := newTestFollower(t, rpc, store)
	ls := &liveStart{Block: repairLiveStart, TS: int64(tsFor(repairLiveStart))}
	if err := f.saveLiveStart(context.Background(), store, ls); err != nil {
		t.Fatal(err)
	}
	// An explicit frontier well before the seeded rows. Without a record the
	// repair stands in the oldest stored row, which in these stores is the
	// start of history rather than a deletion boundary, and every test would
	// then be measuring that stand-in instead of what it means to.
	if err := store.SetState(context.Background(), 4663, db.StatePruneFrontier, baseTime.Add(-24*time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	return f
}

func posterGasOf(t *testing.T, store *dbtest.MemStore, number uint64) sql.NullInt64 {
	t.Helper()
	b, err := store.BlockByNumber(context.Background(), 4663, number)
	if err != nil || b == nil {
		t.Fatalf("block %d: %v", number, err)
	}
	return b.PosterGas
}

func repairCursor(t *testing.T, f *Follower) *posterGasCursor {
	t.Helper()
	c, err := f.loadPosterGasCursor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRepairStepWritesPosterGasAndRebuildsBuckets(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	rpc.posterGas = func(n uint64) uint64 { return 1_000 + n }
	store := dbtest.New()
	seedBlocksWithoutPosterGas(t, rpc, store, 900, 909)
	f := repairFollower(t, rpc, store)

	status, err := f.RepairStep(ctx)
	if err != nil || status != RepairProgressed {
		t.Fatalf("repair: %v %v", status, err)
	}
	for n := uint64(900); n <= 909; n++ {
		got := posterGasOf(t, store, n)
		if !got.Valid || got.Int64 != int64(1_000+n) {
			t.Fatalf("block %d poster gas %+v, want %d", n, got, 1_000+n)
		}
	}
	if c := repairCursor(t, f); c.Next != 910 || c.Done {
		t.Fatalf("cursor %+v, want next 910 and not done", c)
	}
	// The point of the pass: the bucket over the repaired rows now carries
	// poster gas, which is what makes a compute-gas rate and a fee split.
	buckets, err := store.Buckets(ctx, 4663, db.Resolution1m, time.Unix(int64(tsFor(900)), 0).UTC().Truncate(time.Minute), time.Unix(int64(tsFor(1000)), 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) == 0 {
		t.Fatal("no buckets rebuilt")
	}
	for _, b := range buckets {
		if !b.PosterGas.Valid {
			t.Fatalf("bucket %s still has no poster gas", b.BucketStart)
		}
	}
}

func TestRepairStepFinishesAndStaysFinished(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedBlocksWithoutPosterGas(t, rpc, store, 900, 902)
	f := repairFollower(t, rpc, store)

	if status, err := f.RepairStep(ctx); err != nil || status != RepairProgressed {
		t.Fatalf("first step: %v %v", status, err)
	}
	// Nothing left, so the pass records that it is done.
	if status, err := f.RepairStep(ctx); err != nil || status != RepairNone {
		t.Fatalf("second step: %v %v", status, err)
	}
	if c := repairCursor(t, f); !c.Done {
		t.Fatalf("cursor %+v, want done", c)
	}
	before := len(rpc.posterGasCalls)
	// A finished pass answers from the cursor without reading anything.
	if status, err := f.RepairStep(ctx); err != nil || status != RepairNone {
		t.Fatalf("third step: %v %v", status, err)
	}
	if len(rpc.posterGasCalls) != before {
		t.Fatalf("a finished repair made %d more calls", len(rpc.posterGasCalls)-before)
	}
}

// The repair is scoped by the bucket, not by the live block. Rows in the
// boundary hour that sit below live_start are still row-backed, so their
// buckets are rebuilt from rows and they have to be repaired with the rest.
func TestRepairStepRepairsTheBoundaryHourBelowLiveStart(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedBlocksWithoutPosterGas(t, rpc, store, 890, 905)
	f := repairFollower(t, rpc, store)

	for range 3 {
		if status, err := f.RepairStep(ctx); err != nil || status == RepairIdle {
			t.Fatalf("repair: %v %v", status, err)
		}
	}
	for _, n := range []uint64{890, 899, 900} {
		if got := posterGasOf(t, store, n); !got.Valid {
			t.Fatalf("block %d in the boundary hour was not repaired", n)
		}
	}
}

// A bucket that starts before the boundary belongs to the backfill, which
// owns it additively; rebuilding it from rows would be wrong.
func TestRepairStepSkipsBucketsTheBackfillOwns(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedBlocksWithoutPosterGas(t, rpc, store, 890, 905)
	f := newTestFollower(t, rpc, store)
	// Live start an hour on, so every seeded row sits in an earlier bucket.
	ls := &liveStart{Block: 5_000, TS: int64(tsFor(repairLiveStart)) + 3_600}
	if err := f.saveLiveStart(ctx, store, ls); err != nil {
		t.Fatal(err)
	}

	if status, err := f.RepairStep(ctx); err != nil || status != RepairProgressed {
		t.Fatalf("repair: %v %v", status, err)
	}
	if len(rpc.posterGasCalls) != 0 {
		t.Fatalf("read receipts for buckets the backfill owns: %v", rpc.posterGasCalls)
	}
	if got := posterGasOf(t, store, 905); got.Valid {
		t.Fatalf("a backfill-owned row was repaired: %+v", got)
	}
}

// A short block_retention must not put the cutoff past the present and leave
// the pass reporting itself finished having repaired nothing: what has to be
// whole is the bucket's window, so the check is on the bucket's own start.
func TestRepairStepStillWorksOnAShortRetention(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedBlocksWithoutPosterGas(t, rpc, store, 900, 905)
	f := repairFollower(t, rpc, store)
	f.cfg.BlockRetention = time.Hour

	if status, err := f.RepairStep(ctx); err != nil || status != RepairProgressed {
		t.Fatalf("repair: %v %v", status, err)
	}
	if got := posterGasOf(t, store, 900); !got.Valid {
		t.Fatal("an hour of retention repaired nothing, though its bucket is whole")
	}
}

// A block is retried before the pass moves past it, so an endpoint that is
// unreachable, rate limited or behind cannot walk the cursor through a range
// one block per step and leave all of it unrepairable.
func TestRepairStepRetriesOneBlockBeforeGivingUpOnIt(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedBlocksWithoutPosterGas(t, rpc, store, 900, 901)
	f := repairFollower(t, rpc, store)
	f.mu.Lock()
	f.repairNarrow = 1
	f.mu.Unlock()
	rpc.errs["PosterGasByNumbers"] = errors.New("endpoint unreachable")

	// Every attempt but the last leaves the cursor where it was.
	for attempt := 1; attempt < maxRepairAttempts; attempt++ {
		status, err := f.RepairStep(ctx)
		if status != RepairIdle || err == nil {
			t.Fatalf("attempt %d: %v %v", attempt, status, err)
		}
		if c := repairCursor(t, f); c.Next != 0 {
			t.Fatalf("attempt %d moved the cursor to %d", attempt, c.Next)
		}
	}
	if status, err := f.RepairStep(ctx); err != nil || status != RepairProgressed {
		t.Fatalf("final attempt: %v %v", status, err)
	}
	if c := repairCursor(t, f); c.Next != 901 {
		t.Fatalf("cursor %+v, want it past the block it gave up on", c)
	}
	// A block it gives up on does not carry its count into the next one.
	f.mu.Lock()
	attempts := f.repairAttempts
	f.mu.Unlock()
	if attempts != maxRepairAttempts {
		t.Fatalf("attempts %d, want %d", attempts, maxRepairAttempts)
	}
}

func TestRepairStepWaitsWithoutLiveStart(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedBlocksWithoutPosterGas(t, rpc, store, 900, 905)
	f := newTestFollower(t, rpc, store)
	f.mu.Lock()
	f.liveStart = nil
	f.mu.Unlock()

	// ensureInit adopts the oldest stored block as the live start, so the
	// only way to have none is a store with no blocks at all.
	empty := dbtest.New()
	g := newTestFollower(t, rpc, empty)
	if status, err := g.RepairStep(ctx); err != nil || status != RepairNone {
		t.Fatalf("repair without a live start: %v %v", status, err)
	}
	if len(rpc.posterGasCalls) != 0 {
		t.Fatalf("read receipts with no row-backed history: %v", rpc.posterGasCalls)
	}
}

func TestRepairStepYieldsToACatchingUpFastLoop(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedBlocksWithoutPosterGas(t, rpc, store, 900, 905)
	f := repairFollower(t, rpc, store)
	f.catchingUp.Store(true)

	if status, err := f.RepairStep(ctx); err != nil || status != RepairIdle {
		t.Fatalf("repair: %v %v", status, err)
	}
	if len(rpc.posterGasCalls) != 0 {
		t.Fatalf("read receipts while the fast loop was catching up: %v", rpc.posterGasCalls)
	}
	if c := repairCursor(t, f); c.Next != 0 {
		t.Fatalf("cursor moved to %d while yielding", c.Next)
	}
}

func TestRepairStepPassesOverBlocksRetentionIsAboutToDrop(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedBlocksWithoutPosterGas(t, rpc, store, 900, 905)
	f := repairFollower(t, rpc, store)
	// Retention so short that every seeded block is already past the margin
	// a rebuild needs, so reading their receipts would buy nothing. The
	// backfill has to be finished for retention to be what prune goes by;
	// while it runs prune holds these rows and they are still repairable.
	f.cfg.BlockRetention = time.Second
	if err := f.saveCursor(ctx, store, &backfillCursor{Done: true}); err != nil {
		t.Fatal(err)
	}

	status, err := f.RepairStep(ctx)
	if err != nil || status != RepairProgressed {
		t.Fatalf("repair: %v %v", status, err)
	}
	if len(rpc.posterGasCalls) != 0 {
		t.Fatalf("read receipts for blocks whose buckets cannot be rebuilt: %v", rpc.posterGasCalls)
	}
	if c := repairCursor(t, f); c.Next != 906 {
		t.Fatalf("cursor %+v, want it stepped past the batch", c)
	}
}

func TestRepairStepNarrowsThenStepsOverABlockThatWillNotVerify(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedBlocksWithoutPosterGas(t, rpc, store, 900, 907)
	f := repairFollower(t, rpc, store)
	rpc.errs["PosterGasByNumbers"] = errors.New("receipts unavailable")

	// Every read fails, so the batch halves each step until one block is
	// left, which is then retried maxRepairAttempts times and stepped over.
	var sizes []int
	for range 16 {
		status, err := f.RepairStep(ctx)
		if status == RepairProgressed && err == nil {
			break
		}
		if status != RepairIdle || err == nil {
			t.Fatalf("step: %v %v", status, err)
		}
		f.mu.Lock()
		sizes = append(sizes, f.repairNarrow)
		f.mu.Unlock()
	}
	if len(sizes) < 2 || sizes[0] <= sizes[len(sizes)-1] {
		t.Fatalf("batch did not narrow: %v", sizes)
	}
	if c := repairCursor(t, f); c.Next != 901 {
		t.Fatalf("cursor %+v, want it stepped past the one block that failed", c)
	}
	if got := posterGasOf(t, store, 900); got.Valid {
		t.Fatalf("a block that would not verify was written anyway: %+v", got)
	}
}

func TestRepairStepWidensAfterASuccessfulRead(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedBlocksWithoutPosterGas(t, rpc, store, 900, 905)
	f := repairFollower(t, rpc, store)
	f.mu.Lock()
	f.repairNarrow = 2
	f.mu.Unlock()

	if status, err := f.RepairStep(ctx); err != nil || status != RepairProgressed {
		t.Fatalf("repair: %v %v", status, err)
	}
	if len(rpc.posterGasCalls) != 1 || len(rpc.posterGasCalls[0]) != 2 {
		t.Fatalf("narrowed batch not honored: %v", rpc.posterGasCalls)
	}
	f.mu.Lock()
	narrow := f.repairNarrow
	f.mu.Unlock()
	if narrow != 0 {
		t.Fatalf("repairNarrow %d after a successful read, want 0", narrow)
	}
}

func TestRepairStepRestartsOnAnUnreadableCursor(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedBlocksWithoutPosterGas(t, rpc, store, 900, 902)
	f := repairFollower(t, rpc, store)
	if err := store.SetState(ctx, 4663, db.StatePosterGasRepair, "not json"); err != nil {
		t.Fatal(err)
	}

	// A checkpoint nobody can read repeats work, which every write here is
	// safe against, rather than stopping the pass.
	if status, err := f.RepairStep(ctx); err != nil || status != RepairProgressed {
		t.Fatalf("repair: %v %v", status, err)
	}
	if got := posterGasOf(t, store, 900); !got.Valid {
		t.Fatal("block 900 was not repaired after an unreadable cursor")
	}
}

func TestRepairStepLeavesRowsThatAlreadyHavePosterGas(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	rpc.posterGas = func(uint64) uint64 { return 500 }
	store := dbtest.New()
	rows := seedBlocksWithoutPosterGas(t, rpc, store, 900, 903)
	rows[1].PosterGas = sql.NullInt64{Int64: 42, Valid: true}
	if err := store.UpsertBlocks(ctx, rows[1:2]); err != nil {
		t.Fatal(err)
	}
	f := repairFollower(t, rpc, store)

	if status, err := f.RepairStep(ctx); err != nil || status != RepairProgressed {
		t.Fatalf("repair: %v %v", status, err)
	}
	if got := posterGasOf(t, store, 901); got.Int64 != 42 {
		t.Fatalf("an already recorded value was overwritten: %+v", got)
	}
	for _, batch := range rpc.posterGasCalls {
		for _, n := range batch {
			if n == 901 {
				t.Fatal("read receipts for a block that already had poster gas")
			}
		}
	}
}

func TestRepairBatchFollowsTheBudgetAndTheNarrowing(t *testing.T) {
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := repairFollower(t, rpc, store)
	f.cfg.HeaderBatchSize = 10

	if n := f.repairBatchFor(0); n != 10 {
		t.Fatalf("full batch %d, want the configured 10", n)
	}
	if n := f.repairBatchFor(3); n != 3 {
		t.Fatalf("narrowed batch %d, want 3", n)
	}
	if n := f.repairBatchFor(20); n != 10 {
		t.Fatalf("a narrowing above the configured size gave %d", n)
	}
	// A spare budget under the smallest batch raises the size back up, but
	// never past the narrowing: a batch that fails whole because one target
	// in it will not read has to reach the single-block path that steps
	// that target over, or the repair retries the same blocks for good.
	rpc.mu.Lock()
	rpc.available = 0
	rpc.mu.Unlock()
	if n := f.repairBatchFor(0); n != minBackfillBatch {
		t.Fatalf("no budget gave %d, want the smallest batch %d", n, minBackfillBatch)
	}
	for _, narrowed := range []int{1, 2} {
		if n := f.repairBatchFor(narrowed); n != narrowed {
			t.Fatalf("narrowed to %d with no budget gave %d", narrowed, n)
		}
	}
}

// The retention horizon does not fall on every resolution at once. A row
// whose hour has gone short can still have whole minute and quarter-hour
// buckets, and stepping the cursor past it would leave those blank for good.
func TestRepairStepRebuildsTheResolutionsThatAreStillWhole(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(30_000)
	store := dbtest.New()
	// Blocks at 07:47:30, deep enough into the hour that the three
	// resolutions truncate to three different bucket starts.
	seedBlocksWithoutPosterGas(t, rpc, store, 28_500, 28_505)
	ts := time.Unix(int64(tsFor(28_500)), 0).UTC()
	now := ts.Add(90 * time.Second)
	f := newTestFollower(t, rpc, store, func(o *Options) { o.Now = func() time.Time { return now } })
	if err := f.saveLiveStart(ctx, store, &liveStart{Block: 28_500, TS: int64(tsFor(28_500))}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetState(ctx, 4663, db.StatePruneFrontier, baseTime.Add(-24*time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	// Puts the horizon on the quarter-hour bucket's start: that one and the
	// minute bucket are whole, the hour started well before it.
	horizon := ts.Truncate(15 * time.Minute)
	f.cfg.BlockRetention = now.Sub(horizon) + repairPruneSlack
	// Retention is what prune goes by only once the backfill is finished.
	if err := f.saveCursor(ctx, store, &backfillCursor{Done: true}); err != nil {
		t.Fatal(err)
	}
	if got := wholeResolutions(ts, horizon); len(got) != 2 {
		t.Fatalf("resolutions still whole: %v, want the minute and quarter-hour ones", got)
	}

	if status, err := f.RepairStep(ctx); err != nil || status != RepairProgressed {
		t.Fatalf("repair: %v %v", status, err)
	}
	if got := posterGasOf(t, store, 28_500); !got.Valid {
		t.Fatal("a row whose finer buckets are whole was skipped")
	}
	for _, res := range []string{db.Resolution1m, db.Resolution15m} {
		buckets, err := store.Buckets(ctx, 4663, res, ts.Add(-time.Hour), ts.Add(time.Hour))
		if err != nil || len(buckets) == 0 {
			t.Fatalf("%s buckets: %d %v", res, len(buckets), err)
		}
		for _, b := range buckets {
			if !b.PosterGas.Valid {
				t.Fatalf("%s bucket %s was not repaired", res, b.BucketStart)
			}
		}
	}
	// The hour is not rebuilt, because its window has lost rows to retention.
	hours, err := store.Buckets(ctx, 4663, db.Resolution1h, ts.Add(-time.Hour), ts.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range hours {
		if b.PosterGas.Valid {
			t.Fatalf("an hour whose window is short was rebuilt anyway: %s", b.BucketStart)
		}
	}
}

// prune keeps every row from the boundary hour up while the backfill is
// unfinished, whatever retention says. Reading the nominal retention window
// instead of prune's own cutoff would step the cursor past buckets whose
// rows are all still there, and they would never be repaired.
func TestRepairStepFollowsPrunesCutoffWhileTheBackfillRuns(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedBlocksWithoutPosterGas(t, rpc, store, 900, 905)
	f := repairFollower(t, rpc, store)
	// Older than retention, so the nominal window would call every row
	// stale, but the backfill has not finished and prune is holding them.
	f.cfg.BlockRetention = time.Minute
	if err := f.saveCursor(ctx, store, &backfillCursor{Done: false, Active: true}); err != nil {
		t.Fatal(err)
	}

	if status, err := f.RepairStep(ctx); err != nil || status != RepairProgressed {
		t.Fatalf("repair: %v %v", status, err)
	}
	if got := posterGasOf(t, store, 900); !got.Valid {
		t.Fatal("a row prune is still holding was passed over as stale")
	}

	// Once the backfill is done prune drops to the retention window and
	// those rows really are going, so the pass stops spending calls on them.
	rpc.posterGasCalls = nil
	store2 := dbtest.New()
	seedBlocksWithoutPosterGas(t, rpc, store2, 900, 905)
	g := repairFollower(t, rpc, store2)
	g.cfg.BlockRetention = time.Minute
	if err := g.saveCursor(ctx, store2, &backfillCursor{Done: true}); err != nil {
		t.Fatal(err)
	}
	if status, err := g.RepairStep(ctx); err != nil || status != RepairProgressed {
		t.Fatalf("repair after the backfill: %v %v", status, err)
	}
	if len(rpc.posterGasCalls) != 0 {
		t.Fatalf("read receipts for rows retention is dropping: %v", rpc.posterGasCalls)
	}
}

// A block the pass gives up on is behind the cursor, which only moves
// forward, so it needs a sweep of its own to be seen again. Without one a
// block that failed while an endpoint was briefly unavailable would never be
// read, and a restart would resume past it rather than retry it.
func TestRepairSweepsTheBlocksItPassedOver(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedBlocksWithoutPosterGas(t, rpc, store, 900, 902)
	f := repairFollower(t, rpc, store)
	f.mu.Lock()
	f.repairNarrow = 1
	f.mu.Unlock()
	rpc.errs["PosterGasByNumbers"] = errors.New("endpoint unreachable")

	// Give up on block 900 after its attempts, and record it.
	for range maxRepairAttempts {
		if _, err := f.RepairStep(ctx); err != nil && !strings.Contains(err.Error(), "unreachable") {
			t.Fatal(err)
		}
	}
	c := repairCursor(t, f)
	if len(c.Skipped) != 1 || c.Skipped[0] != 900 || c.Done {
		t.Fatalf("cursor %+v, want block 900 recorded and not done", c)
	}

	// The endpoint comes back. Reaching the end of the range starts a sweep
	// of what was passed over rather than calling the pass finished.
	delete(rpc.errs, "PosterGasByNumbers")
	for range 8 {
		status, err := f.RepairStep(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if status == RepairNone {
			break
		}
	}
	if got := posterGasOf(t, store, 900); !got.Valid {
		t.Fatal("a block the pass swept again was still not repaired")
	}
	if c := repairCursor(t, f); !c.Done || len(c.Skipped) != 0 {
		t.Fatalf("cursor %+v, want done with nothing left over", c)
	}
}

// A block that fails every sweep has to end the pass rather than circle in it.
func TestRepairStopsSweepingAfterMaxSweeps(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedBlocksWithoutPosterGas(t, rpc, store, 900, 901)
	f := repairFollower(t, rpc, store)
	f.mu.Lock()
	f.repairNarrow = 1
	f.mu.Unlock()
	rpc.errs["PosterGasByNumbers"] = errors.New("receipts will never verify")

	for range 200 {
		status, err := f.RepairStep(ctx)
		if status == RepairNone && err == nil {
			break
		}
	}
	c := repairCursor(t, f)
	if !c.Done {
		t.Fatalf("cursor %+v, want the pass to have ended", c)
	}
	// Sweeps counts the re-sweeps, so the pass made maxRepairSweeps in all.
	if c.Sweeps+1 != maxRepairSweeps {
		t.Fatalf("made %d passes, want %d", c.Sweeps+1, maxRepairSweeps)
	}
}

// Raising block_retention moves prune's cutoff back over rows the shorter
// setting already deleted. Rebuilding the bucket straddling that frontier
// would sum only its surviving rows and replace a correct aggregate with a
// short one, so the recorded frontier outranks the current cutoff.
func TestRepairStepWillNotRebuildBelowThePruneFrontier(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedBlocksWithoutPosterGas(t, rpc, store, 900, 905)
	f := repairFollower(t, rpc, store)
	if err := f.saveCursor(ctx, store, &backfillCursor{Done: true}); err != nil {
		t.Fatal(err)
	}
	// A prune already ran with a short retention and deleted everything
	// below the seeded rows' own hour.
	frontier := time.Unix(int64(tsFor(905)), 0).UTC().Add(time.Minute)
	if err := store.SetState(ctx, 4663, db.StatePruneFrontier, frontier.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	// Retention is now generous, so the current cutoff sits well before it.
	f.cfg.BlockRetention = 48 * time.Hour

	status, err := f.RepairStep(ctx)
	if err != nil || status != RepairProgressed {
		t.Fatalf("repair: %v %v", status, err)
	}
	if len(rpc.posterGasCalls) != 0 {
		t.Fatalf("read receipts for buckets below the prune frontier: %v", rpc.posterGasCalls)
	}
	// Without the frontier the generous retention would have called these
	// rows repairable, so the guard is what stopped it. Same setup, same
	// clock, its own store because the run above stepped its cursor past
	// the rows it passed over.
	control := dbtest.New()
	seedBlocksWithoutPosterGas(t, rpc, control, 900, 905)
	g := repairFollower(t, rpc, control)
	if err := g.saveCursor(ctx, control, &backfillCursor{Done: true}); err != nil {
		t.Fatal(err)
	}
	g.cfg.BlockRetention = 48 * time.Hour
	if status, err := g.RepairStep(ctx); err != nil || status != RepairProgressed {
		t.Fatalf("repair without a frontier: %v %v", status, err)
	}
	if len(rpc.posterGasCalls) == 0 {
		t.Fatal("nothing was read even with no frontier recorded, so the test proves nothing")
	}
}

func TestPruneRecordsAFrontierThatOnlyMovesForward(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedBlocksWithoutPosterGas(t, rpc, store, 900, 905)
	f := repairFollower(t, rpc, store)
	if err := f.saveCursor(ctx, store, &backfillCursor{Done: true}); err != nil {
		t.Fatal(err)
	}
	f.cfg.BlockRetention = time.Minute
	if err := f.prune(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := f.pruneFrontier(ctx)
	if err != nil || first.IsZero() {
		t.Fatalf("frontier after a prune: %v %v", first, err)
	}

	// Retention is raised, so this prune's cutoff is earlier. The record
	// stays where it was: those rows are gone either way.
	f.cfg.BlockRetention = 48 * time.Hour
	if err := f.prune(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := f.pruneFrontier(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Equal(first) {
		t.Fatalf("frontier moved back from %v to %v", first, second)
	}
}

// A database pruned by a collector older than the frontier checkpoint has a
// deletion boundary it never recorded. Reading an absent key as "nothing was
// deleted" would let the first rollout that also raises block_retention
// rebuild straight across it.
func TestPruneFrontierStandsInTheOldestRowWhenNothingWasRecorded(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedBlocksWithoutPosterGas(t, rpc, store, 900, 905)
	f := newTestFollower(t, rpc, store)

	got, err := f.pruneFrontier(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Unix(int64(tsFor(900)), 0).UTC()
	if !got.Equal(want) {
		t.Fatalf("frontier %v, want the oldest stored row at %v", got, want)
	}
	// A store with nothing in it constrains nothing.
	empty := newTestFollower(t, rpc, dbtest.New())
	if got, err := empty.pruneFrontier(ctx); err != nil || !got.IsZero() {
		t.Fatalf("frontier on an empty store: %v %v", got, err)
	}
}

// The cutoff is handed to PruneBlocks whole, so recording it to the second
// would understate what was deleted by up to a second.
func TestPruneFrontierKeepsSubSecondPrecision(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New()
	f := newTestFollower(t, newFakeRPC(1000), store)
	cutoff := baseTime.Add(1500 * time.Millisecond)
	if err := f.recordPruneFrontier(ctx, store, cutoff); err != nil {
		t.Fatal(err)
	}
	got, err := f.pruneFrontier(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(cutoff.UTC()) {
		t.Fatalf("frontier %v, want the cutoff it was given, %v", got, cutoff.UTC())
	}
}

func TestWholeResolutions(t *testing.T) {
	// 10:47:30 truncates to three different starts: 10:47, 10:45 and 10:00.
	ts := time.Date(2026, 9, 7, 10, 47, 30, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		horizon time.Time
		want    int
	}{
		{"every window whole", ts.Add(-2 * time.Hour), 3},
		{"the hour has gone short", time.Date(2026, 9, 7, 10, 30, 0, 0, time.UTC), 2},
		{"only the minute is left", time.Date(2026, 9, 7, 10, 46, 0, 0, time.UTC), 1},
		{"nothing is rebuildable", ts.Add(time.Hour), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := wholeResolutions(ts, tc.horizon); len(got) != tc.want {
				t.Fatalf("resolutions %v, want %d of them", got, tc.want)
			}
		})
	}
}

func TestPosterGasCursorRoundTrips(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New()
	f := repairFollower(t, newFakeRPC(1000), store)
	want := &posterGasCursor{Next: 4242, Done: true}
	if err := f.savePosterGasCursor(ctx, store, want); err != nil {
		t.Fatal(err)
	}
	got := repairCursor(t, f)
	if got.Next != want.Next || got.Done != want.Done {
		t.Fatalf("cursor %+v, want %+v", got, want)
	}
	raw, ok, err := store.GetState(ctx, 4663, db.StatePosterGasRepair)
	if err != nil || !ok {
		t.Fatalf("state: %v %v", ok, err)
	}
	var decoded posterGasCursor
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("stored cursor is not json: %v", err)
	}
}
