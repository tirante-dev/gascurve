package collector

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/db/dbtest"
	"github.com/tirante-dev/gascurve/internal/logger"
	"github.com/tirante-dev/gascurve/internal/nitro"
)

// rewound simulates the fast loop committing a rewind while another loop
// is fetching: it bumps the chain's generation the way rewindToAncestor
// does inside its transaction.
func rewound(t *testing.T, f *Follower, store *dbtest.MemStore) func() {
	t.Helper()
	return func() {
		ctx := context.Background()
		if err := store.WithChainTx(ctx, f.chainID, func(s db.Store) error { return f.bumpGeneration(ctx, s) }); err != nil {
			t.Error(err)
		}
	}
}

func generationOf(t *testing.T, store *dbtest.MemStore) uint64 {
	t.Helper()
	raw, ok, err := store.GetState(context.Background(), 4663, db.StateGeneration)
	if err != nil || !ok {
		return 0
	}
	gen, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return gen
}

// TestGenerationDiscardsStaleWork: every writer that makes network calls
// outside the chain lock captures the chain's rewind counter first and
// compares it inside its transaction. Work fetched from a fork the fast
// loop has since rewound is discarded, and, crucially, its cursors never
// restore a checkpoint the rewind lowered.
func TestGenerationDiscardsStaleWork(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	rpc.logs = fixtureLogs(t)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// A rewind lands while the owner log read is in flight: the actions
	// and the log cursor belong to the old fork and are dropped.
	rpc.hooks["OwnerActsLogs"] = rewound(t, f, store)
	err := f.scanOwnerActions(ctx)
	if !errors.Is(err, errStaleGeneration) {
		t.Fatalf("stale owner scan: %v", err)
	}
	if v, ok, _ := store.GetState(ctx, 4663, db.StateOwnerLogCursor); ok {
		t.Fatalf("the log cursor must not move: %q", v)
	}
	if n, _ := store.OwnerActions(ctx, 4663, time.Time{}, time.Time{}, 0); len(n) != 0 {
		t.Fatalf("old-fork actions must not be stored: %d", len(n))
	}
	if generationOf(t, store) != 1 {
		t.Fatalf("generation = %d", generationOf(t, store))
	}
	// The retry, with the current generation, commits.
	delete(rpc.hooks, "OwnerActsLogs")
	if err := f.scanOwnerActions(ctx); err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := store.GetState(ctx, 4663, db.StateOwnerLogCursor); !ok || v != "1000" {
		t.Fatalf("log cursor after the retry: %q", v)
	}
	if v, ok, _ := store.GetState(ctx, 4663, db.StateOwnerScanThrough); !ok || v != "1000" {
		t.Fatalf("readiness checkpoint after the retry: %q", v)
	}

	// The readiness checkpoint is written under the same check: a rewind
	// that lowered it is not undone by a pass that started on the old fork.
	_ = store.SetState(ctx, 4663, db.StateOwnerScanThrough, "10")
	f.mu.Lock()
	f.ownerScanThrough = 10
	f.mu.Unlock()
	rpc.hooks["OwnerActsLogs"] = rewound(t, f, store)
	delete(store.StateRows, "4663/"+db.StateOwnerLogCursor)
	if err := f.scanOwnerActions(ctx); !errors.Is(err, errStaleGeneration) {
		t.Fatalf("stale readiness write: %v", err)
	}
	if v, _, _ := store.GetState(ctx, 4663, db.StateOwnerScanThrough); v != "10" {
		t.Fatalf("the rewound checkpoint must stay lowered: %q", v)
	}
	delete(rpc.hooks, "OwnerActsLogs")

	// The batch report scan is the same: blocks read from the old fork are
	// not stored and the scan cursor stays put.
	full := rpc.header(1000)
	rpc.mu.Lock()
	rpc.fullBlocks[1000] = nitro.Block{Header: full}
	rpc.mu.Unlock()
	_ = store.SetState(ctx, 4663, db.StateBatchScanCursor, "999")
	rpc.hooks["BlocksWithTxs"] = rewound(t, f, store)
	rpc.mu.Lock()
	rows := store.BlockRows[4663]
	for n, b := range rows {
		b.TxCount = 2
		rows[n] = b
	}
	rpc.mu.Unlock()
	if err := f.scanBatchReports(ctx); !errors.Is(err, errStaleGeneration) {
		t.Fatalf("stale batch scan: %v", err)
	}
	if v, _, _ := store.GetState(ctx, 4663, db.StateBatchScanCursor); v != "999" {
		t.Fatalf("the batch cursor must not move: %q", v)
	}
	delete(rpc.hooks, "BlocksWithTxs")

	// A backfill step whose headers came from the old fork writes nothing
	// and reports idle rather than an error, so the loop simply retries.
	seedSets(t, store)
	f.mu.Lock()
	_ = f.reloadSetsLocked(ctx)
	f.mu.Unlock()
	scanned(f)
	f.cfg.BackfillDepth = config.Depth(30 * time.Second)
	if _, err := f.BackfillStep(ctx); err != nil { // start the segment
		t.Fatal(err)
	}
	before, _ := f.loadCursor(ctx)
	rpc.hooks["HeadersByNumbers"] = rewound(t, f, store)
	st, err := f.BackfillStep(ctx)
	if err != nil || st != BackfillIdle {
		t.Fatalf("stale backfill step: %v %v", st, err)
	}
	after, _ := f.loadCursor(ctx)
	if after.Next != before.Next {
		t.Fatalf("the backfill cursor must not advance on discarded work: %d -> %d", before.Next, after.Next)
	}
	delete(rpc.hooks, "HeadersByNumbers")
	if _, err := f.BackfillStep(ctx); err != nil {
		t.Fatal(err)
	}
	if c, _ := f.loadCursor(ctx); c.Next <= before.Next {
		t.Fatalf("the retry must progress: %+v", c)
	}
	// A corrupt counter is an error, not a silent zero.
	_ = store.SetState(ctx, 4663, db.StateGeneration, "x")
	if _, err := f.generation(ctx, store); err == nil {
		t.Fatal("corrupt generation")
	}
}

// TestGenerationBumpedByRewind: a reorg rewind bumps the counter inside
// its own transaction, which is what makes every writer that captured the
// old value abort.
func TestGenerationBumpedByRewind(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	rpc.setHead(1002)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if generationOf(t, store) != 0 {
		t.Fatalf("no rewind, no bump: %d", generationOf(t, store))
	}
	rpc.fork(1001, "z")
	rpc.setHead(1004)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if generationOf(t, store) != 1 {
		t.Fatalf("a rewind bumps the counter: %d", generationOf(t, store))
	}
}

// TestSlowSampleGeneration: a slow sample published while a tick's
// transaction is running is not lost. The tick clears only the generation
// it actually persisted, so the newer sample is stored by the next tick
// instead of appearing on the WebSocket and never reaching state_samples.
func TestSlowSampleGeneration(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.sampleSlow(ctx); err != nil {
		t.Fatal(err)
	}
	// The tick captures generation 1 and, while its transaction is running,
	// the slow loop publishes generation 2.
	store.Hooks["InsertStateSample"] = func() {
		if err := f.sampleSlow(ctx); err != nil {
			t.Error(err)
		}
	}
	rpc.setHead(1002)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	delete(store.Hooks, "InsertStateSample")
	f.mu.Lock()
	gen, saved := f.slowGen, f.slowSaved
	f.mu.Unlock()
	if gen != 2 || saved != 1 {
		t.Fatalf("the tick may only clear what it wrote: generation %d saved %d", gen, saved)
	}
	// The next tick stores the newer sample rather than dropping it.
	rpc.setHead(1003)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	saved = f.slowSaved
	f.mu.Unlock()
	if saved != 2 {
		t.Fatalf("the newer slow sample must be persisted: saved %d", saved)
	}
	withL1, err := store.LatestStateSample(ctx, 4663, true)
	if err != nil || withL1 == nil || withL1.BlockNumber != 1003 {
		t.Fatalf("the latest stored L1 sample: %+v %v", withL1, err)
	}
	// With nothing new pending, a later tick writes no slow data again.
	rpc.setHead(1004)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	latest, _ := store.LatestStateSample(ctx, 4663, false)
	if latest.BlockNumber != 1004 || latest.L1 != nil {
		t.Fatalf("a tick with nothing pending carries no L1 data: %+v", latest)
	}
}

// TestPolicyFollowsActiveEndpoint: gap skipping and the batch-report
// prefilter follow the endpoint ordinary calls actually go to, not the
// primary's configuration, and one operation takes a single snapshot of it.
func TestPolicyFollowsActiveEndpoint(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	pool := &fakePool{fakeRPC: rpc, pol: nitro.Policy{Rate: 4}}
	f := NewFollower(Options{
		Network:   config.NetworkConfig{Name: "robinhood", ChainID: 4663, CallsPerSecond: 4, Enabled: true},
		Collector: testConfig(), RPC: pool, Store: store, Log: logger.Nop(),
		Now:   func() time.Time { return baseTime.Add(1000 * time.Second / 10) },
		Sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
	})
	if pol := f.policy(); pol.unlimited || pol.rate != 4 {
		t.Fatalf("paced active endpoint: %+v", pol)
	}
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// A gap far beyond the paced budget is skipped while the primary is
	// active.
	rpc.setHead(1200)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if holes := holesOf(t, store); len(holes) != 1 {
		t.Fatalf("a paced endpoint skips the gap: %+v", holes)
	}
	// The pool fails over to an unlimited endpoint: the same gap is now
	// fetched in full, because the policy is the active endpoint's.
	pool.pol = nitro.Policy{Unlimited: true}
	if pol := f.policy(); !pol.unlimited {
		t.Fatalf("unlimited active endpoint: %+v", pol)
	}
	rpc.setHead(1400)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if holes := holesOf(t, store); len(holes) != 1 {
		t.Fatalf("an unlimited endpoint fetches the gap: %+v", holes)
	}
	if b, _ := store.BlockByNumber(ctx, 4663, 1300); b == nil {
		t.Fatal("every block of the gap must be replayed on an unlimited endpoint")
	}
	// Without a pool the network's own configuration is the policy.
	plain := newTestFollower(t, newFakeRPC(1), store)
	if pol := plain.policy(); pol.unlimited || pol.rate != 4 {
		t.Fatalf("plain client policy: %+v", pol)
	}
	plain.net.CallsPerSecond = 0
	if pol := plain.policy(); !pol.unlimited {
		t.Fatalf("plain unlimited policy: %+v", pol)
	}
}

// TestRewindMovesNetworkRowAndClearsState: a rewind moves the network row
// down to the surviving ancestor in the same transaction and drops every
// in-memory field that described the orphaned head, so a failure to fetch
// or persist the replacement leaves nothing pointing at a block that is
// off-chain.
func TestRewindMovesNetworkRowAndClearsState(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	rpc.setHead(1005)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := store.NetworkRows[4663]; n.HeadBlock.Int64 != 1005 {
		t.Fatalf("head before the rewind: %+v", n.HeadBlock)
	}
	// The replacement fetch fails right after the rewind commits: the first
	// header read (the one that reveals the fork) succeeds and arms the
	// failure for the read that follows the rewind.
	rpc.fork(1002, "r")
	rpc.setHead(1007)
	rpc.hooks["HeadersByNumbers"] = func() {
		rpc.mu.Lock()
		rpc.errs["HeadersByNumbers"] = errRPC
		rpc.mu.Unlock()
	}
	if err := f.Tick(ctx); !errors.Is(err, errRPC) {
		t.Fatalf("expected the replacement fetch to fail, got %v", err)
	}
	n := store.NetworkRows[4663]
	if n.HeadBlock.Int64 != 1001 {
		t.Fatalf("the network row must follow the rewind down to the ancestor: %+v", n.HeadBlock)
	}
	if !n.HeadAt.Valid || n.HeadAt.Time.Unix() != int64(tsFor(1001)) {
		t.Fatalf("head_at must be the ancestor's timestamp: %+v", n.HeadAt)
	}
	f.mu.Lock()
	sample, snap, state, head := f.lastSample, f.snapshot, f.state, f.head
	f.mu.Unlock()
	if sample != nil || snap != nil || state != nil {
		t.Fatalf("nothing may describe the orphaned head: sample %v snapshot %v state %v", sample, snap, state)
	}
	if head != 1001 {
		t.Fatalf("in-memory head after the rewind: %d", head)
	}
	// Recovery: the replacement blocks are fetched and the row moves back up.
	delete(rpc.hooks, "HeadersByNumbers")
	delete(rpc.errs, "HeadersByNumbers")
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := store.NetworkRows[4663]; n.HeadBlock.Int64 != 1007 || n.LastError.Valid {
		t.Fatalf("network row after recovery: %+v", n)
	}
	for num := uint64(1002); num <= 1007; num++ {
		if b, _ := store.BlockByNumber(ctx, 4663, num); b == nil || b.Hash != rpc.hashFor(num) {
			t.Fatalf("block %d not canonical: %+v", num, b)
		}
	}
}

// TestHeadBehindDistinguishesLagFromRollback: a reported head below the
// stored one is only a rollback when the node reports another hash at that
// height. A lagging endpoint reports the same hashes and is skipped;
// otherwise the follower rewinds instead of ignoring the head for ever.
func TestHeadBehindDistinguishesLagFromRollback(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1010)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	rpc.setHead(1015)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// A lagging endpoint: same chain, fewer blocks. Nothing is rewound.
	rpc.setHead(1012)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if f.Head() != 1015 || generationOf(t, store) != 0 {
		t.Fatalf("a lagging endpoint must not rewind: head %d generation %d", f.Head(), generationOf(t, store))
	}
	if b, _ := store.BlockByNumber(ctx, 4663, 1015); b == nil {
		t.Fatal("blocks above a lagging head must survive")
	}
	// A real rollback: the node reports a different hash at the lower head.
	rpc.fork(1012, "k")
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if generationOf(t, store) != 1 {
		t.Fatalf("a confirmed rollback must rewind: generation %d", generationOf(t, store))
	}
	if f.Head() != 1011 {
		t.Fatalf("head after the rollback rewind: %d", f.Head())
	}
	if b, _ := store.BlockByNumber(ctx, 4663, 1015); b != nil {
		t.Fatalf("orphaned blocks must be gone: %+v", b)
	}
	// The next tick replays the canonical chain from the ancestor.
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if f.Head() != 1012 {
		t.Fatalf("head after the replacement tick: %d", f.Head())
	}
	// A head below the oldest stored block is a lag, not a rollback: there
	// is no row to compare it against.
	rpc.setHead(1)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if f.Head() != 1012 {
		t.Fatalf("no row at the reported head: %d", f.Head())
	}
	// A lookup failure surfaces instead of being read as a rollback.
	store.FailOn["BlockByNumber"] = true
	if err := f.Tick(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("lookup failure: %v", err)
	}
}
