package collector

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/db/dbtest"
	"github.com/tirante-dev/gascurve/internal/model"
	"github.com/tirante-dev/gascurve/internal/nitro"
)

// TestGenesisDepthScansWholeChain: backfill_depth genesis reaches the first
// block of a chain far too long for the default window, without a header
// search, without an origin and without a hole below one.
func TestGenesisDepthScansWholeChain(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(largeChainHead)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	f.cfg.BackfillDepth = config.Genesis
	if err := f.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rpc.logRanges) == 0 || rpc.logRanges[0][0] != 0 {
		t.Fatalf("genesis scan must start at the first block: %v", rpc.logRanges[:1])
	}
	if _, ok, _ := store.GetState(ctx, 4663, db.StateOwnerScanOrigin); ok {
		t.Fatal("a scan from genesis is not truncated, so it records no origin")
	}
	if holes := holesOf(t, store); len(holes) != 0 {
		t.Fatalf("nothing below genesis to record as missing: %+v", holes)
	}
	if calls := rpc.calledTimes("HeaderByNumber"); calls != 0 {
		t.Fatalf("genesis is not searched for: %d header reads", calls)
	}
}

// TestGenesisDepthBackfillsToTheFirstBlock: the backfill's floor under a
// genesis depth is block 1, and the walk finishes there rather than at a
// window measured back from the head.
func TestGenesisDepthBackfillsToTheFirstBlock(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedSets(t, store)
	f := newTestFollower(t, rpc, store)
	f.cfg.BackfillDepth = config.Genesis
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runBackfill(t, f, 200)
	c, err := f.loadCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c.DepthTarget != 1 || c.DepthStart != 1 || !c.Done {
		t.Fatalf("genesis floor: %+v", c)
	}
	if b, err := store.BlockByNumber(ctx, 4663, 100); err != nil || b == nil {
		t.Fatalf("the earliest constraint set's first block must be replayed: %+v %v", b, err)
	}
}

// TestBackfillDepthWidens: a deeper depth on a restarted collector lowers
// the recorded floor and puts a finished backfill back to work, and a
// shallower one is reported but leaves the history already built alone.
func TestBackfillDepthWidens(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedSets(t, store)
	f := newTestFollower(t, rpc, store)
	// 30 seconds of history is block 700, above the set at 500.
	f.cfg.BackfillDepth = config.Depth(30 * time.Second)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runBackfill(t, f, 60)
	c, err := f.loadCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Done || c.DepthStart != 700 || c.DepthTarget != 700 || c.End != 500 {
		t.Fatalf("shallow floor: %+v", c)
	}
	// A shallower depth on the next start changes nothing: the deeper floor
	// is history the operator has, and is not given back.
	shallow := newTestFollower(t, rpc, store)
	shallow.cfg.BackfillDepth = config.Depth(10 * time.Second)
	if err := shallow.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	scanned(shallow)
	if st, err := shallow.BackfillStep(ctx); err != nil || st != BackfillDone {
		t.Fatalf("a narrowed depth must not re-open the backfill: %v %v", st, err)
	}
	if c, err = shallow.loadCursor(ctx); err != nil || c.DepthTarget != 700 {
		t.Fatalf("narrowed target: %+v %v", c, err)
	}
	// A deeper one lowers the floor and resumes below what was built.
	deep := newTestFollower(t, rpc, store)
	deep.cfg.BackfillDepth = config.Depth(70 * time.Second)
	if err := deep.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runBackfill(t, deep, 200)
	if c, err = deep.loadCursor(ctx); err != nil {
		t.Fatal(err)
	}
	if !c.Done || c.DepthTarget != 300 || c.DepthStart != 300 || c.End != 100 {
		t.Fatalf("widened floor: %+v", c)
	}
	if b, err := store.BlockByNumber(ctx, 4663, 100); err != nil || b == nil {
		t.Fatalf("the widened range must be replayed: %+v %v", b, err)
	}
}

// TestBackfillFloorFollowsTheScanOrigin: the floor is clamped to the owner
// scan origin without overwriting the target, so an origin lowered later
// releases the rest of the configured depth instead of it being lost.
func TestBackfillFloorFollowsTheScanOrigin(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedSets(t, store)
	f := newTestFollower(t, rpc, store)
	f.cfg.BackfillDepth = config.Depth(70 * time.Second)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.scanOrigin = &scanOrigin{Block: 499, Archive: true, MinBaseFee: "1"}
	f.mu.Unlock()
	runBackfill(t, f, 60)
	c, err := f.loadCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c.DepthTarget != 300 || c.DepthStart != 500 {
		t.Fatalf("clamped floor: %+v", c)
	}
	f.mu.Lock()
	f.scanOrigin = &scanOrigin{Block: 99, Archive: true, MinBaseFee: "1"}
	f.mu.Unlock()
	runBackfill(t, f, 60)
	if c, err = f.loadCursor(ctx); err != nil {
		t.Fatal(err)
	}
	if c.DepthStart != 300 || !c.Done {
		t.Fatalf("released floor: %+v", c)
	}
}

// TestExtendScanOrigin: a depth reaching below a truncated scan's origin
// reads the actions between the two, re-samples the state at the new
// cutoff and leaves the owner log cursor, which tracks the top of the
// scanned range, exactly where it was.
func TestExtendScanOrigin(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(largeChainHead)
	archive := newFakeRPC(largeChainHead)
	archive.minFee = big.NewInt(30_000_000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	f.archive = archive
	depthPastCutoff(f)
	if err := f.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	if f.scanOrigin == nil || f.scanOrigin.Block != scanCutoff {
		t.Fatalf("first origin: %+v", f.scanOrigin)
	}
	cursor, _, _ := store.GetState(ctx, 4663, db.StateOwnerLogCursor)

	deeper := newTestFollower(t, rpc, store)
	deeper.archive = archive
	deeper.cfg.BackfillDepth = config.Genesis
	rpc.logRanges = nil
	if err := deeper.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	// The scan now reaches the first block, so nothing truncates it: on a
	// constraints chain the origin is dropped rather than moved to block 1,
	// which would put an observed set above the genesis set and clamp the
	// floor past block 1 for nothing.
	if deeper.scanOrigin != nil {
		t.Fatalf("an untruncated scan holds no origin: %+v", deeper.scanOrigin)
	}
	if _, ok, _ := store.GetState(ctx, 4663, db.StateOwnerScanOrigin); ok {
		t.Fatal("the recorded origin must be cleared too")
	}
	if len(rpc.logRanges) == 0 || rpc.logRanges[0][0] != 0 {
		t.Fatalf("the range below the old origin must be read: %v", rpc.logRanges[:1])
	}
	if now, _, _ := store.GetState(ctx, 4663, db.StateOwnerLogCursor); now != cursor {
		t.Fatalf("the log cursor tracks the top of the scan: %q, was %q", now, cursor)
	}
	if len(archive.sampleAt) != 2 || archive.sampleAt[1] != firstOriginBlock {
		t.Fatalf("the state at the new cutoff must be sampled: %v", archive.sampleAt)
	}
	sets, _ := store.ConstraintSets(ctx, 4663)
	for _, cs := range sets {
		if cs.EffectiveBlock == firstOriginBlock+1 && cs.Source == model.SourceObserved {
			t.Fatalf("no observed set may be left at the first block: %+v", cs)
		}
	}
	// Checked once per process: a second pass costs no further sample.
	if err := deeper.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(archive.sampleAt) != 2 {
		t.Fatalf("the extension runs once: %v", archive.sampleAt)
	}
}

// TestExtendScanOriginWithoutArchive: without an endpoint serving
// historical state there is nothing to re-sample at a lower cutoff, so the
// walk is refused rather than spent and the origin stays where it is.
func TestExtendScanOriginWithoutArchive(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(largeChainHead)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	depthPastCutoff(f)
	if err := f.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	deeper := newTestFollower(t, rpc, store)
	deeper.cfg.BackfillDepth = config.Genesis
	rpc.logRanges = nil
	if err := deeper.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	if deeper.scanOrigin == nil || deeper.scanOrigin.Block != scanCutoff {
		t.Fatalf("origin without archive: %+v", deeper.scanOrigin)
	}
	for _, r := range rpc.logRanges {
		if r[0] < scanCutoff {
			t.Fatalf("nothing below the origin may be read: %v", rpc.logRanges)
		}
	}
}

// TestExtendScanOriginErrors: a failing log read or a failing re-sample
// leaves the recorded origin alone, so the next pass tries the whole
// extension again rather than half of it standing as fact.
func TestExtendScanOriginErrors(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(largeChainHead)
	archive := newFakeRPC(largeChainHead)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	f.archive = archive
	depthPastCutoff(f)
	if err := f.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		fail func()
		undo func()
	}{
		{"logs", func() { rpc.errs["OwnerActsLogs"] = errRPC }, func() { delete(rpc.errs, "OwnerActsLogs") }},
		{"sample", func() { archive.errs["PricingSampleAt"] = errRPC }, func() { delete(archive.errs, "PricingSampleAt") }},
	} {
		deeper := newTestFollower(t, rpc, store)
		deeper.archive = archive
		deeper.cfg.BackfillDepth = config.Genesis
		tc.fail()
		if err := deeper.SlowTick(ctx); !errors.Is(err, errRPC) {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if deeper.scanOrigin == nil || deeper.scanOrigin.Block != scanCutoff {
			t.Fatalf("%s: origin moved on a failure: %+v", tc.name, deeper.scanOrigin)
		}
		tc.undo()
	}
}

// TestBackfillAcrossAPricingModelChange: a replay reaching back past a
// chain's move from the legacy single-backlog pricer to constraint sets
// splits at the change, so the legacy stretch is priced with the
// parameters sampled at the origin and not with today's model.
func TestBackfillAcrossAPricingModelChange(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	// The chain runs on the legacy pricer until block 500, where an owner
	// action installs the first constraint set, which has two constraints
	// where the legacy model has one backlog.
	if _, err := store.InsertConstraintSet(ctx, db.ConstraintSet{
		ChainID: 4663, EffectiveBlock: 500, EffectiveAt: baseTime.Add(50 * time.Second), Source: model.SourceOwnerAction,
		Constraints: entriesJSON([]model.ConstraintSetEntry{
			{Target: 60_000_000, Window: 15, StartingBacklog: 11},
			{Target: 20_000_000, Window: 86_400, StartingBacklog: 1_000},
		}),
	}); err != nil {
		t.Fatal(err)
	}
	f := newTestFollower(t, rpc, store)
	f.cfg.BackfillDepth = config.Depth(70 * time.Second)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.scanOrigin = &scanOrigin{
		Block: 299, Archive: true, MinBaseFee: "10000000",
		Legacy: &model.LegacyParams{SpeedLimit: 7_000_000, Inertia: 102, Tolerance: 10, Backlog: 4_200},
	}
	f.mu.Unlock()
	runBackfill(t, f, 80)
	legacy, err := store.BlockByNumber(ctx, 4663, 400)
	if err != nil || legacy == nil {
		t.Fatalf("legacy block: %+v %v", legacy, err)
	}
	if len(legacy.Backlogs) != 1 || len(legacy.ConstraintBips) != 1 {
		t.Fatalf("block 400 is before the first set, so the legacy model prices it: %v %v", legacy.Backlogs, legacy.ConstraintBips)
	}
	if legacy.MinBaseFee.Wei.Int64() != 10_000_000 {
		t.Fatalf("the legacy stretch keeps the fee sampled at the origin: %v", legacy.MinBaseFee)
	}
	modern, err := store.BlockByNumber(ctx, 4663, 600)
	if err != nil || modern == nil {
		t.Fatalf("constraints block: %+v %v", modern, err)
	}
	if len(modern.Backlogs) != 2 || len(modern.ConstraintBips) != 2 {
		t.Fatalf("block 600 is priced by the constraint set: %v %v", modern.Backlogs, modern.ConstraintBips)
	}
	// The buckets carry the set only where one was in force, so a reader
	// cannot attribute the legacy stretch to today's constraints.
	var legacySet, modernSet bool
	for _, b := range store.BucketRows {
		switch {
		case b.Resolution != db.Resolution1m:
		case b.LastBlock <= 499:
			legacySet = legacySet || b.ConstraintSetID.Valid
		case b.LastBlock >= 600:
			modernSet = modernSet || b.ConstraintSetID.Valid
		}
	}
	if legacySet {
		t.Fatal("a legacy bucket belongs to no constraint set")
	}
	if !modernSet {
		t.Fatal("a constraints bucket records the set it replayed")
	}
}

// TestBackfillDepthMigratesAnOldCursor: a cursor written before the target
// existed carries only the floor it settled on. That floor is the history
// the operator has, so it seeds the target; reading the configuration into
// it instead would raise the floor of every unfinished backfill on upgrade,
// because the window has slid forward since it was first resolved.
func TestBackfillDepthMigratesAnOldCursor(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedSets(t, store)
	f := newTestFollower(t, rpc, store)
	f.cfg.BackfillDepth = config.Depth(70 * time.Second)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// The floor a longer window settled on before the target was recorded,
	// with the walk still in progress.
	if err := f.saveCursor(ctx, store, &backfillCursor{DepthStart: 120, Top: 1000, End: 600}); err != nil {
		t.Fatal(err)
	}
	scanned(f)
	if _, err := f.BackfillStep(ctx); err != nil {
		t.Fatal(err)
	}
	c, err := f.loadCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c.DepthTarget != 120 || c.DepthStart != 120 {
		t.Fatalf("today's shallower window must not raise the floor: %+v", c)
	}
}

// TestBackfillDepthSurvivesARewind: a rewind landing while the floor is
// being resolved discards the write, and the next step resolves it again.
// Marking the depth resolved before the cursor commits would drop the
// widening on the floor until the process restarted.
func TestBackfillDepthSurvivesARewind(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedSets(t, store)
	f := newTestFollower(t, rpc, store)
	f.cfg.BackfillDepth = config.Depth(70 * time.Second)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	scanned(f)
	rpc.hooks["HeaderByNumber"] = rewound(t, f, store)
	if _, err := f.BackfillStep(ctx); err != nil {
		t.Fatalf("a rewind is retried as idle, not an error: %v", err)
	}
	if c, err := f.loadCursor(ctx); err != nil || c.DepthTarget != 0 {
		t.Fatalf("the discarded floor must not be stored: %+v %v", c, err)
	}
	delete(rpc.hooks, "HeaderByNumber")
	if _, err := f.BackfillStep(ctx); err != nil {
		t.Fatal(err)
	}
	c, err := f.loadCursor(ctx)
	if err != nil || c.DepthTarget != 300 || c.DepthStart != 300 {
		t.Fatalf("the next step must resolve the floor again: %+v %v", c, err)
	}
}

// TestGenesisDepthOnALegacyChain: a legacy chain has no constraint set to
// replay from, so a depth reaching the first block still samples the state
// there through an archive endpoint and records it as the origin. Without
// one the range is reported missing rather than reconstructed from a
// guessed model.
func TestGenesisDepthOnALegacyChain(t *testing.T) {
	ctx := context.Background()
	legacy := func(head uint64) *fakeRPC {
		r := newFakeRPC(head)
		r.legacy = &nitro.LegacyParams{SpeedLimit: 7_000_000, Inertia: 102, Tolerance: 10}
		return r
	}
	rpc, archive := legacy(1000), legacy(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	f.archive = archive
	f.cfg.BackfillDepth = config.Genesis
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	if f.scanOrigin == nil || f.scanOrigin.Block != firstOriginBlock || f.scanOrigin.Legacy == nil {
		t.Fatalf("a legacy chain needs a sampled origin to replay from: %+v", f.scanOrigin)
	}
	runBackfill(t, f, 200)
	c, err := f.loadCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Block 1's own state is what the sample describes, so the replay
	// starts at block 2.
	if c.DepthTarget != 1 || c.DepthStart != firstOriginBlock+1 {
		t.Fatalf("legacy genesis floor: %+v", c)
	}
	b, err := store.BlockByNumber(ctx, 4663, 2)
	if err != nil || b == nil || len(b.Backlogs) != 1 {
		t.Fatalf("the whole chain must be replayed on the legacy model: %+v %v", b, err)
	}
	if holes := holesOf(t, store); len(holes) != 0 {
		t.Fatalf("nothing is missing once the origin is sampled: %+v", holes)
	}
	// Without an archive endpoint there is nothing to sample, so the range
	// below the earliest known state is reported rather than invented.
	bare := newTestFollower(t, legacy(1000), dbtest.New())
	bare.cfg.BackfillDepth = config.Genesis
	if err := bare.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := bare.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	if bare.scanOrigin != nil {
		t.Fatalf("no origin without archive state: %+v", bare.scanOrigin)
	}
}

// TestGenesisDepthOnAConstraintsChain: at the first block the scan is not
// truncated, so a chain with a genesis constraint set to replay from keeps
// no origin. Recording one would put an observed set above that genesis
// set and clamp the floor past the first block for nothing.
func TestGenesisDepthOnAConstraintsChain(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	archive := newFakeRPC(1000)
	store := dbtest.New()
	seedSets(t, store)
	f := newTestFollower(t, rpc, store)
	f.archive = archive
	f.cfg.BackfillDepth = config.Genesis
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	if f.scanOrigin != nil {
		t.Fatalf("a constraints chain scanned from the first block holds no origin: %+v", f.scanOrigin)
	}
	sets, _ := store.ConstraintSets(ctx, 4663)
	if len(sets) != 2 {
		t.Fatalf("no observed set may be added at the first block: %+v", sets)
	}
	runBackfill(t, f, 200)
	c, err := f.loadCursor(ctx)
	if err != nil || c.DepthStart != 1 {
		t.Fatalf("the floor stays at the first block: %+v %v", c, err)
	}
}

// TestBackfillDepthMigrationIsDurable: a finished cursor written before the
// target existed changes nothing else, so nothing else would carry the
// migrated target to the database, and every step would resolve it again
// with a fresh header search for the life of the process.
func TestBackfillDepthMigrationIsDurable(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedSets(t, store)
	f := newTestFollower(t, rpc, store)
	f.cfg.BackfillDepth = config.Depth(70 * time.Second)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.saveCursor(ctx, store, &backfillCursor{Done: true, DepthStart: 300, Top: 1000, End: 300}); err != nil {
		t.Fatal(err)
	}
	scanned(f)
	if st, err := f.BackfillStep(ctx); err != nil || st != BackfillDone {
		t.Fatalf("a finished cursor stays finished: %v %v", st, err)
	}
	if c, err := f.loadCursor(ctx); err != nil || c.DepthTarget != 300 {
		t.Fatalf("the migrated target must be stored: %+v %v", c, err)
	}
	rpc.calls["HeaderByNumber"] = 0
	for range 3 {
		if _, err := f.BackfillStep(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if calls := rpc.calledTimes("HeaderByNumber"); calls != 0 {
		t.Fatalf("a resolved floor is not searched for again: %d header reads", calls)
	}
}

// TestGenesisOriginForAScanThatAlreadyReachedIt: a legacy chain whose scan
// had already reached the first block under an earlier collector recorded
// no origin, and its owner log cursor keeps a fresh scan from ever running
// again. Without a state sampled there its whole history is reported
// missing, so the sample is taken on the next start. A constraints chain
// has its genesis set to replay from and costs no call at all.
func TestGenesisOriginForAScanThatAlreadyReachedIt(t *testing.T) {
	ctx := context.Background()
	archive := newFakeRPC(1000)
	archive.legacy = &nitro.LegacyParams{SpeedLimit: 7_000_000, Inertia: 102, Tolerance: 10}
	rpc := newFakeRPC(1000)
	rpc.legacy = archive.legacy
	store := dbtest.New()
	// A completed scan from the first block, as an earlier collector left
	// it: a log cursor, no origin.
	if err := store.SetState(ctx, 4663, db.StateOwnerLogCursor, "999"); err != nil {
		t.Fatal(err)
	}
	f := newTestFollower(t, rpc, store)
	f.archive = archive
	f.cfg.BackfillDepth = config.Genesis
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	if f.scanOrigin == nil || f.scanOrigin.Block != firstOriginBlock || f.scanOrigin.Legacy == nil {
		t.Fatalf("a legacy chain needs the state at the first block sampled: %+v", f.scanOrigin)
	}
	// Recorded once: the block comparison skips it from here on.
	if err := f.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(archive.sampleAt) != 1 {
		t.Fatalf("the origin is sampled once: %v", archive.sampleAt)
	}
	// On a constraints chain the same sample says the state is not legacy,
	// so no origin is recorded and the genesis set is what the replay
	// starts from. The count of recorded sets is no substitute for the
	// sample: a chain that was legacy and later upgraded has sets and still
	// needs the state at the first block.
	modern, modernArchive := newFakeRPC(1000), newFakeRPC(1000)
	store2 := dbtest.New()
	seedSets(t, store2)
	if err := store2.SetState(ctx, 4663, db.StateOwnerLogCursor, "999"); err != nil {
		t.Fatal(err)
	}
	g := newTestFollower(t, modern, store2)
	g.archive = modernArchive
	g.cfg.BackfillDepth = config.Genesis
	if err := g.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := g.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	if g.scanOrigin != nil || len(modernArchive.sampleAt) != 1 {
		t.Fatalf("a constraints chain records no origin: %+v %v", g.scanOrigin, modernArchive.sampleAt)
	}
	if _, ok, _ := store2.GetState(ctx, 4663, db.StateOwnerScanOrigin); ok {
		t.Fatal("nothing may be recorded for a chain that replays from its sets")
	}
}

// TestBackfillDepthWidensAcrossWindows: on an archive network the descent
// bottoms out at the floor, so a widened floor has to extend the windows
// below what was already reconstructed rather than leaving them where the
// first start put them.
func TestBackfillDepthWidensAcrossWindows(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	archive := newFakeRPC(1000)
	store := dbtest.New()
	seedSets(t, store)
	f := newTestFollower(t, rpc, store)
	f.archive = archive
	f.cfg.BackfillWindow = 100
	f.cfg.BackfillDepth = config.Depth(30 * time.Second) // block 700
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runBackfill(t, f, 200)
	if b, err := store.BlockByNumber(ctx, 4663, 600); err != nil || b != nil {
		t.Fatalf("nothing below the first floor may be replayed: %+v %v", b, err)
	}
	deep := newTestFollower(t, rpc, store)
	deep.archive = archive
	deep.cfg.BackfillWindow = 100
	deep.cfg.BackfillDepth = config.Depth(70 * time.Second) // block 300
	if err := deep.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runBackfill(t, deep, 400)
	c, err := deep.loadCursor(ctx)
	if err != nil || !c.Done || c.DepthTarget != 300 {
		t.Fatalf("widened cursor: %+v %v", c, err)
	}
	// Every window between the old floor and the new one is filled, with no
	// gap left where the descent restarted.
	for _, n := range []uint64{310, 450, 600, 699} {
		b, err := store.BlockByNumber(ctx, 4663, n)
		if err != nil || b == nil {
			t.Fatalf("block %d must be reconstructed: %+v %v", n, b, err)
		}
	}
}
