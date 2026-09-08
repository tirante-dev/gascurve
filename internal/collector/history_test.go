package collector

import (
	"context"
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/db/dbtest"
	"github.com/tirante-dev/gascurve/internal/model"

	"github.com/tirante-dev/gascurve/internal/config"
)

// historyStore is a chain whose history has been reconstructed once: a
// live start in the second hour, a backfilled bucket in the hour before
// it, a row-backed bucket in its own hour, a finished backfill cursor and
// the owner-scan checkpoints a completed scan leaves behind.
func historyStore(t *testing.T) *dbtest.MemStore {
	t.Helper()
	ctx := context.Background()
	store := dbtest.New()
	liveTS := baseTime.Add(time.Hour)
	raw, err := json.Marshal(liveStart{Block: 1000, TS: liveTS.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := json.Marshal(backfillCursor{Done: true, DepthStart: 100, Top: 1000, End: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		db.StateLiveStart:        string(raw),
		db.StateBackfillCursor:   string(cursor),
		db.StateOwnerScanOrigin:  `{"block":99,"archive":false}`,
		db.StateOwnerLogCursor:   "1000",
		db.StateOwnerScanThrough: "1000",
		db.StateGeneration:       "3",
	} {
		if err := store.SetState(ctx, 4663, key, value); err != nil {
			t.Fatal(err)
		}
	}
	// One bucket the backfill owns (the hour before the live start) and
	// one the live rows own (the hour of the live start).
	starts := []time.Time{baseTime, liveTS.Truncate(time.Hour)}
	buckets := make([]db.Bucket, 0, len(starts))
	for _, start := range starts {
		buckets = append(buckets, db.Bucket{
			ChainID: 4663, Resolution: db.Resolution1h, BucketStart: start, Blocks: 10,
			BaseFeeMin: db.NewWei(big.NewInt(1)), BaseFeeAvg: db.NewWei(big.NewInt(1)), BaseFeeMax: db.NewWei(big.NewInt(1)),
			FeesWei: db.NewWei(big.NewInt(0)), LastBlock: 999,
		})
	}
	if err := store.FoldBuckets(ctx, buckets); err != nil {
		t.Fatal(err)
	}
	return store
}

// epochFollower is a follower over store configured at a history epoch.
func epochFollower(t *testing.T, store *dbtest.MemStore, epoch int) *Follower {
	t.Helper()
	f := newTestFollower(t, newFakeRPC(1000), store)
	f.net.HistoryEpoch = epoch
	return f
}

func stateOf(t *testing.T, store *dbtest.MemStore, key string) (string, bool) {
	t.Helper()
	v, ok, err := store.GetState(context.Background(), 4663, key)
	if err != nil {
		t.Fatal(err)
	}
	return v, ok
}

func bucketStarts(t *testing.T, store *dbtest.MemStore) []time.Time {
	t.Helper()
	rows, err := store.Buckets(context.Background(), 4663, db.Resolution1h, baseTime.Add(-time.Hour), baseTime.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]time.Time, 0, len(rows))
	for _, b := range rows {
		out = append(out, b.BucketStart)
	}
	return out
}

// TestHistoryEpochRebuildsOnce: raising history_epoch drops what the
// backfill owns and the checkpoints that decide where it starts, then
// records the epoch, so the same configuration on the next start (a
// restarted pod, which is the case a flag would get wrong) rebuilds
// nothing.
func TestHistoryEpochRebuildsOnce(t *testing.T) {
	ctx := context.Background()
	store := historyStore(t)
	liveHour := baseTime.Add(time.Hour)

	if err := epochFollower(t, store, 1).ensureInit(ctx); err != nil {
		t.Fatal(err)
	}
	starts := bucketStarts(t, store)
	if len(starts) != 1 || !starts[0].Equal(liveHour) {
		t.Fatalf("only the backfill's buckets go, the row-backed hour stays: %v", starts)
	}
	c, err := (&Follower{store: store, chainID: 4663}).loadCursor(ctx)
	if err != nil || c.Done || c.Top != 0 {
		t.Fatalf("the backfill starts over: %+v %v", c, err)
	}
	for _, key := range []string{db.StateOwnerScanOrigin, db.StateOwnerLogCursor, db.StateOwnerScanThrough} {
		if _, ok := stateOf(t, store, key); ok {
			t.Fatalf("%s must be cleared so the origin is established again", key)
		}
	}
	if v, _ := stateOf(t, store, db.StateHistoryEpoch); v != "1" {
		t.Fatalf("the rebuilt epoch is recorded: %q", v)
	}
	// Work fetched before the rebuild must not commit onto the history
	// that replaced it.
	if v, _ := stateOf(t, store, db.StateGeneration); v != "4" {
		t.Fatalf("generation after the rebuild: %q", v)
	}
	// The live rows themselves are observations, not reconstruction.
	if _, ok := stateOf(t, store, db.StateLiveStart); !ok {
		t.Fatal("the live start survives a rebuild")
	}

	// A restart with the same epoch: the checkpoints a fresh scan wrote
	// are left alone and nothing is rebuilt again.
	if err := store.SetState(ctx, 4663, db.StateOwnerLogCursor, "2000"); err != nil {
		t.Fatal(err)
	}
	if err := epochFollower(t, store, 1).ensureInit(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _ := stateOf(t, store, db.StateOwnerLogCursor); v != "2000" {
		t.Fatalf("a restart at the same epoch must rebuild nothing: cursor %q", v)
	}
	if v, _ := stateOf(t, store, db.StateGeneration); v != "4" {
		t.Fatalf("a restart at the same epoch must not bump the generation: %q", v)
	}

	// Raising it again rebuilds again.
	if err := epochFollower(t, store, 2).ensureInit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := stateOf(t, store, db.StateOwnerLogCursor); ok {
		t.Fatal("a raised epoch rebuilds again")
	}
	if v, _ := stateOf(t, store, db.StateHistoryEpoch); v != "2" {
		t.Fatalf("epoch after the second rebuild: %q", v)
	}
}

// TestHistoryEpochUnsetChangesNothing: the default never rebuilds and
// leaves no checkpoint behind, so a database that has always run without
// the setting is untouched by it.
func TestHistoryEpochUnsetChangesNothing(t *testing.T) {
	ctx := context.Background()
	store := historyStore(t)
	if err := epochFollower(t, store, 0).ensureInit(ctx); err != nil {
		t.Fatal(err)
	}
	if len(bucketStarts(t, store)) != 2 {
		t.Fatalf("buckets: %v", bucketStarts(t, store))
	}
	if _, ok := stateOf(t, store, db.StateHistoryEpoch); ok {
		t.Fatal("no epoch is recorded when none is configured")
	}
	if v, _ := stateOf(t, store, db.StateOwnerLogCursor); v != "1000" {
		t.Fatalf("owner cursor: %q", v)
	}
}

// TestHistoryEpochLoweredIsIgnored: a value below the stored one is a
// rollback of the configuration, not a request to rebuild.
func TestHistoryEpochLoweredIsIgnored(t *testing.T) {
	ctx := context.Background()
	store := historyStore(t)
	if err := store.SetState(ctx, 4663, db.StateHistoryEpoch, "5"); err != nil {
		t.Fatal(err)
	}
	if err := epochFollower(t, store, 2).ensureInit(ctx); err != nil {
		t.Fatal(err)
	}
	if len(bucketStarts(t, store)) != 2 {
		t.Fatalf("nothing is rebuilt: %v", bucketStarts(t, store))
	}
	if v, _ := stateOf(t, store, db.StateHistoryEpoch); v != "5" {
		t.Fatalf("the stored epoch stands: %q", v)
	}
}

// TestHistoryEpochUnreadableCheckpoint: a value that is not a number is
// treated as never rebuilt rather than failing the follower's start.
func TestHistoryEpochUnreadableCheckpoint(t *testing.T) {
	ctx := context.Background()
	store := historyStore(t)
	if err := store.SetState(ctx, 4663, db.StateHistoryEpoch, "not a number"); err != nil {
		t.Fatal(err)
	}
	if err := epochFollower(t, store, 1).ensureInit(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _ := stateOf(t, store, db.StateHistoryEpoch); v != "1" {
		t.Fatalf("the epoch is rewritten with a readable value: %q", v)
	}
}

// TestHistoryEpochResetsHoles: a blocked range becomes pending so the
// re-established origin can make it fillable without losing its record, a
// range that folded into deleted buckets restarts, and a row-backed range
// keeps the progress its filler made.
func TestHistoryEpochResetsHoles(t *testing.T) {
	ctx := context.Background()
	store := historyStore(t)
	holes := []hole{
		{From: 10, To: 90, Reason: reasonNoState},
		{From: 200, To: 400, Next: 300, Folded: 300, State: &model.HoleState{Block: 299}},
		{From: 1100, To: 1200, Next: 1150, State: &model.HoleState{Block: 1149}},
	}
	raw, err := json.Marshal(holes)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetState(ctx, 4663, db.StateHoles, string(raw)); err != nil {
		t.Fatal(err)
	}
	if err := epochFollower(t, store, 1).ensureInit(ctx); err != nil {
		t.Fatal(err)
	}
	got := holesOf(t, store)
	if len(got) != 3 {
		t.Fatalf("every missing range must stay recorded: %+v", got)
	}
	if got[0].From != 10 || got[0].Lifecycle != rangePending || got[0].Reason != "" {
		t.Fatalf("the blocked range is requeued without being erased: %+v", got[0])
	}
	if got[1].From != 200 || got[1].Next != 0 || got[1].Folded != 0 || got[1].State != nil || got[1].CursorAt != "" {
		t.Fatalf("a range folded into deleted buckets restarts: %+v", got[1])
	}
	if got[2].From != 1100 || got[2].Next != 1150 || got[2].State == nil {
		t.Fatalf("a row-backed range keeps its progress: %+v", got[2])
	}
}

// TestHistoryEpochWithoutBuckets: a database with no reconstructed history
// yet records the epoch without touching anything, so the setting can be
// deployed with a fresh database and still costs one rebuild later.
func TestHistoryEpochWithoutBuckets(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New()
	f := epochFollower(t, store, 2)
	if err := f.ensureInit(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _ := stateOf(t, store, db.StateHistoryEpoch); v != "2" {
		t.Fatalf("epoch: %q", v)
	}
	if len(bucketStarts(t, store)) != 0 {
		t.Fatal("nothing to delete on a fresh database")
	}
}

// TestHistoryEpochFailureRetries: a failing write aborts the start, so the
// follower never runs on a half-rebuilt history. The epoch is recorded
// last and only on success, so the next start runs the whole rebuild
// again rather than skipping it as done.
func TestHistoryEpochFailureRetries(t *testing.T) {
	ctx := context.Background()
	store := historyStore(t)
	store.SetFailure("DeleteBucketsBefore", true)
	err := epochFollower(t, store, 1).ensureInit(ctx)
	if err == nil {
		t.Fatal("a failed rebuild must fail the start")
	}
	store.SetFailure("DeleteBucketsBefore", false)
	if _, ok := stateOf(t, store, db.StateHistoryEpoch); ok {
		t.Fatal("a failed rebuild records no epoch, so the next start retries it")
	}
	// The rebuild failed before it reached them; the real store rolls the
	// whole transaction back either way.
	if v, _ := stateOf(t, store, db.StateOwnerLogCursor); v != "1000" {
		t.Fatalf("checkpoints after a failed rebuild: %q", v)
	}
	// The retry succeeds and records the epoch.
	if err := epochFollower(t, store, 1).ensureInit(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _ := stateOf(t, store, db.StateHistoryEpoch); v != "1" {
		t.Fatalf("epoch after the retry: %q", v)
	}
}

// TestHistoryEpochAnotherWriterWon: two collectors starting on the same
// chain. The second re-reads the epoch inside the chain transaction, finds
// the rebuild already done and leaves the first one's work alone rather
// than deleting the buckets it has begun to write.
func TestHistoryEpochAnotherWriterWon(t *testing.T) {
	ctx := context.Background()
	store := historyStore(t)
	f := epochFollower(t, store, 0)
	if err := f.ensureInit(ctx); err != nil {
		t.Fatal(err)
	}
	f.net.HistoryEpoch = 1
	// The store's hook runs before every GetState. The first is this
	// follower's own read, the second is the one inside its transaction:
	// the other writer commits between them.
	reads := 0
	store.Hooks["GetState"] = func() {
		reads++
		if reads != 2 {
			return
		}
		if err := store.SetState(ctx, 4663, db.StateHistoryEpoch, "1"); err != nil {
			t.Error(err)
		}
	}
	f.mu.Lock()
	err := f.applyHistoryEpochLocked(ctx)
	f.mu.Unlock()
	delete(store.Hooks, "GetState")
	if err != nil {
		t.Fatal(err)
	}
	if len(bucketStarts(t, store)) != 2 {
		t.Fatalf("the loser rebuilds nothing: %v", bucketStarts(t, store))
	}
	if v, _ := stateOf(t, store, db.StateGeneration); v != "3" {
		t.Fatalf("the loser bumps no generation: %q", v)
	}
	if v, _ := stateOf(t, store, db.StateOwnerLogCursor); v != "1000" {
		t.Fatalf("the loser clears no checkpoint: %q", v)
	}
}

// TestHistoryEpochCheckpointFailure: a checkpoint that cannot be cleared
// fails the rebuild rather than leaving the origin claiming a state the
// rebuild is replacing.
func TestHistoryEpochCheckpointFailure(t *testing.T) {
	store := historyStore(t)
	store.SetFailure("DeleteState", true)
	if err := epochFollower(t, store, 1).ensureInit(context.Background()); err == nil {
		t.Fatal("a checkpoint that cannot be cleared must fail the rebuild")
	}
	store.SetFailure("DeleteState", false)
	if _, ok := stateOf(t, store, db.StateHistoryEpoch); ok {
		t.Fatal("a failed rebuild records no epoch")
	}
}

// TestHistoryEpochRebuildsTheBackfill: the whole cycle on a real backfill.
// The history is reconstructed, a raised epoch resets it, and the backfill
// runs again from the start and lands on exactly the same buckets: what
// the rebuild deleted is re-folded and what it kept is rebuilt from the
// rows it kept, so no block is counted twice and none is lost.
func TestHistoryEpochRebuildsTheBackfill(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedSets(t, store)
	f := newTestFollower(t, rpc, store)
	f.cfg.BackfillDepth = config.Depth(70 * time.Second)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runBackfill(t, f, 200)
	before := blockCountIn(store, db.Resolution1m)
	if before != 901 {
		t.Fatalf("blocks folded by the first backfill: %d", before)
	}
	if err := store.SetState(ctx, 4663, db.StateOwnerLogCursor, "1000"); err != nil {
		t.Fatal(err)
	}

	f2 := epochFollower(t, store, 1)
	f2.cfg.BackfillDepth = config.Depth(70 * time.Second)
	if err := f2.ensureInit(ctx); err != nil {
		t.Fatal(err)
	}
	c, err := f2.loadCursor(ctx)
	if err != nil || c.Done {
		t.Fatalf("the backfill starts over: %+v %v", c, err)
	}
	if _, ok := stateOf(t, store, db.StateOwnerLogCursor); ok {
		t.Fatal("the owner scan runs again so the origin is re-established")
	}

	runBackfill(t, f2, 200)
	if after := blockCountIn(store, db.Resolution1m); after != before {
		t.Fatalf("the rebuilt history must match the first one: %d then %d", before, after)
	}
	c, err = f2.loadCursor(ctx)
	if err != nil || !c.Done || c.DepthStart != 300 {
		t.Fatalf("cursor after the rebuild: %+v %v", c, err)
	}
}

// TestHistoryEpochRebuildsPastADamagedCheckpoint: a rebuild is how an
// operator recovers a chain whose owner-scan checkpoint cannot be read, so
// it has to run before that checkpoint is parsed. Otherwise the start
// fails on the very state the rebuild would replace.
func TestHistoryEpochRebuildsPastADamagedCheckpoint(t *testing.T) {
	ctx := context.Background()
	store := historyStore(t)
	if err := store.SetState(ctx, 4663, db.StateOwnerScanOrigin, "{not json"); err != nil {
		t.Fatal(err)
	}
	// Without a rebuild the follower cannot start at all.
	if err := epochFollower(t, store, 0).ensureInit(ctx); err == nil {
		t.Fatal("an unreadable owner scan origin must fail the start")
	}
	if err := epochFollower(t, store, 1).ensureInit(ctx); err != nil {
		t.Fatalf("the rebuild must clear the damaged checkpoint: %v", err)
	}
	if _, ok := stateOf(t, store, db.StateOwnerScanOrigin); ok {
		t.Fatal("the damaged checkpoint is gone")
	}
	if v, _ := stateOf(t, store, db.StateHistoryEpoch); v != "1" {
		t.Fatalf("epoch: %q", v)
	}
}
