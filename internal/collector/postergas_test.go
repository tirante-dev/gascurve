package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

func TestRepairStepSkipsBlocksBelowLiveStart(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedBlocksWithoutPosterGas(t, rpc, store, 890, 905)
	// Blocks under live start belong to the backfill, which owns their
	// buckets additively; rebuilding those from rows would be wrong.
	f := repairFollower(t, rpc, store)

	if status, err := f.RepairStep(ctx); err != nil || status != RepairProgressed {
		t.Fatalf("repair: %v %v", status, err)
	}
	if got := posterGasOf(t, store, 899); got.Valid {
		t.Fatalf("block 899 under live start was repaired: %+v", got)
	}
	if got := posterGasOf(t, store, 900); !got.Valid {
		t.Fatal("block 900 at live start was not repaired")
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
	// a rebuild needs, so reading their receipts would buy nothing.
	f.cfg.BlockRetention = time.Second

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

	// Every read fails, so the batch halves on each attempt until one block
	// is left, which is then left unrepaired rather than retried for good.
	var sizes []int
	for range 6 {
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
