//go:build integration

package collector

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/metrics"
)

const (
	agedSpacing  = 30
	agedPerHour  = 3600 / agedSpacing
	agedFrom     = 2 * agedPerHour
	agedTo       = 4*agedPerHour - 1
	agedRetained = 3 * time.Hour
)

var (
	agedStart = baseTime.Add(2 * time.Hour)
	agedEnd   = baseTime.Add(4 * time.Hour)
)

type agedFixture struct {
	t       *testing.T
	pg      *db.Postgres
	rpc     *fakeRPC
	archive *fakeRPC
	f       *Follower
	reg     *prometheus.Registry
	head    uint64
}

func openAgedPostgres(t *testing.T) *db.Postgres {
	t.Helper()
	url := os.Getenv("TEST_DB_URL")
	if url == "" {
		t.Skip("TEST_DB_URL not set")
	}
	d, err := db.Open(url, 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	p := db.NewPostgres(d)
	if err := db.ResetSchema(context.Background(), p.DB()); err != nil {
		t.Fatal(err)
	}
	return p
}

func sparseChain(head uint64) *fakeRPC {
	rpc := newFakeRPC(head)
	rpc.spacing = agedSpacing
	return rpc
}

// newAgedFixture records a two hour outage, hour aligned at both ends (blocks agedFrom to agedTo),
// then lets the chain run on under a real retention until the outage lies two hours below the head.
// Nothing is filled and nothing pruned yet.
func newAgedFixture(t *testing.T) *agedFixture {
	t.Helper()
	ctx := context.Background()
	pg := openAgedPostgres(t)
	rpc := sparseChain(agedFrom - 1)
	reg := metrics.NewRegistry()
	f := newTestFollower(t, rpc, pg, func(o *Options) {
		o.Collector = config.CollectorConfig{
			TickInterval: time.Second, SlowInterval: time.Minute, HeaderBatchSize: 10,
			BlockRetention: agedRetained, SampleRetention: 168 * time.Hour, BackfillDepth: config.Depth(700 * time.Second),
		}
		o.Metrics = metrics.NewCollector(reg)
	})
	fx := &agedFixture{t: t, pg: pg, rpc: rpc, f: f, reg: reg}
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	fx.head = agedTo + 1
	rpc.setHead(fx.head)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	holes := fx.holes()
	if len(holes) != 1 || holes[0].From != agedFrom || holes[0].To != agedTo || holes[0].SuccessorAt != agedEnd.Format(time.RFC3339) {
		t.Fatalf("the outage must be one hole with hour aligned bounds: %+v", holes)
	}
	if err := f.saveCursor(ctx, pg, &backfillCursor{Done: true}); err != nil {
		t.Fatal(err)
	}
	if err := pg.SetState(ctx, 4663, db.StatePosterGasRepair, `{"done":true}`); err != nil {
		t.Fatal(err)
	}
	fx.runTo(agedTo + 1 + 2*agedPerHour + 40)
	fx.prune()
	fx.archive = sparseChain(fx.head)
	f.archive = fx.archive
	return fx
}

// runTo follows the chain to head in steps a paced tick can take without skipping a range.
func (fx *agedFixture) runTo(head uint64) {
	fx.t.Helper()
	if fx.head == 0 {
		fx.head = agedFrom - 1
	}
	for fx.head < head {
		fx.head = min(fx.head+50, head)
		fx.rpc.setHead(fx.head)
		if fx.archive != nil {
			fx.archive.setHead(fx.head)
		}
		if err := fx.f.Tick(context.Background()); err != nil {
			fx.t.Fatal(err)
		}
	}
}

func (fx *agedFixture) prune() {
	fx.t.Helper()
	if err := fx.f.prune(context.Background()); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *agedFixture) holes() []hole {
	fx.t.Helper()
	rows, err := fx.pg.MissingRanges(context.Background(), 4663)
	if err != nil {
		fx.t.Fatal(err)
	}
	out := make([]hole, len(rows))
	for i, row := range rows {
		if out[i], err = holeFromRow(row); err != nil {
			fx.t.Fatal(err)
		}
	}
	return out
}

func (fx *agedFixture) frontier() time.Time {
	fx.t.Helper()
	raw, ok, err := fx.pg.GetState(context.Background(), 4663, db.StatePruneFrontier)
	if err != nil || !ok {
		fx.t.Fatalf("no prune frontier: %v", err)
	}
	ts, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		fx.t.Fatal(err)
	}
	return ts
}

// fill runs the filler to the end, the chain advancing a few blocks and a prune running between
// every pair of batches, as they do in production.
func (fx *agedFixture) fill() {
	fx.t.Helper()
	for i := 0; i < 400; i++ {
		status, err := fx.f.FillStep(context.Background())
		if err != nil {
			fx.t.Fatalf("fill step %d: %v", i, err)
		}
		if status == FillNone {
			return
		}
		fx.runTo(fx.head + 10)
		fx.prune()
	}
	fx.t.Fatal("the fill did not finish")
}

// settle moves the chain on and prunes so no row of the outage survives and the frontier lies past
// it, which is where a repair run days later finds the database.
func (fx *agedFixture) settle() {
	fx.t.Helper()
	fx.runTo(fx.head + 2*agedPerHour)
	fx.prune()
	if rows, err := fx.pg.BlocksBetween(context.Background(), 4663, agedStart, agedEnd); err != nil || len(rows) != 0 {
		fx.t.Fatalf("rows of the outage survive the prune: %d %v", len(rows), err)
	}
	if fx.frontier().Before(agedEnd.Add(time.Hour)) {
		fx.t.Fatalf("the frontier %s is not past the outage", fx.frontier())
	}
}

func (fx *agedFixture) buckets(res string, from, to time.Time) []db.Bucket {
	fx.t.Helper()
	out, err := fx.pg.Buckets(context.Background(), 4663, res, from, to)
	if err != nil {
		fx.t.Fatal(err)
	}
	return out
}

type bucketKey struct {
	res   string
	start time.Time
}

type bucketFacts struct {
	Blocks    int64
	GasUsed   uint64
	Fees      string
	Sum       string
	LastBlock uint64
}

func (fx *agedFixture) snapshot(from, to time.Time) map[bucketKey]bucketFacts {
	fx.t.Helper()
	out := map[bucketKey]bucketFacts{}
	for _, res := range db.ResolutionOrder {
		for _, b := range fx.buckets(res, from, to) {
			key := bucketKey{res, b.BucketStart.UTC()}
			if _, dup := out[key]; dup {
				fx.t.Fatalf("two buckets for %s %s", res, key.start)
			}
			out[key] = bucketFacts{Blocks: b.Blocks, GasUsed: b.GasUsed, Fees: b.FeesWei.String(), Sum: b.BaseFeeSum.Wei.String(), LastBlock: b.LastBlock}
		}
	}
	return out
}

func sameSnapshot(t *testing.T, what string, got, want map[bucketKey]bucketFacts) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d buckets, want %d", what, len(got), len(want))
	}
	for k, w := range want {
		if g, ok := got[k]; !ok || g != w {
			t.Fatalf("%s: %s %s is %+v, want %+v", what, k.res, k.start.Format("15:04"), g, w)
		}
	}
}

func (fx *agedFixture) deleteBuckets(from, to time.Time) {
	fx.t.Helper()
	if _, err := fx.pg.DB().ExecContext(context.Background(),
		`DELETE FROM buckets WHERE chain_id = 4663 AND bucket_start >= $1 AND bucket_start < $2`, from, to); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *agedFixture) requeue(from, to uint64, successor time.Time) {
	fx.t.Helper()
	row := db.MissingRange{
		ChainID: 4663, From: from, To: to, DetectedAt: fx.f.now().UTC(), CreatedAt: fx.f.now().UTC(),
		Lifecycle: rangePending, Reason: reasonCatchUpLimit, SuccessorAt: sql.NullTime{Time: successor, Valid: !successor.IsZero()},
	}
	if err := fx.pg.WithChainTx(context.Background(), 4663, func(s db.Store) error {
		return s.ReplaceMissingRanges(context.Background(), 4663, []db.MissingRange{row})
	}); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *agedFixture) holesFilled() string {
	return `gascurve_collector_holes_filled_total{chain_id="4663",network="robinhood"}`
}

func windowsIn(res string, from, to time.Time) []time.Time {
	var out []time.Time
	for s := from; s.Before(to); s = s.Add(db.Resolutions[res]) {
		out = append(out, s)
	}
	return out
}

func blocksIn(from, to time.Time) int64 {
	return int64(to.Sub(from) / (agedSpacing * time.Second))
}

// requireWholeWindows asserts exactly one bucket per window of [from, to), each holding every block
// of its window, and returns them.
func (fx *agedFixture) requireWholeWindows(from, to time.Time) {
	fx.t.Helper()
	for _, res := range db.ResolutionOrder {
		width := db.Resolutions[res]
		got := fx.buckets(res, from, to)
		windows := windowsIn(res, from, to)
		if len(got) != len(windows) {
			fx.t.Fatalf("%s: %d buckets for %d windows", res, len(got), len(windows))
		}
		for i, b := range got {
			if !b.BucketStart.Equal(windows[i]) || b.Blocks != blocksIn(windows[i], windows[i].Add(width)) {
				fx.t.Fatalf("%s %s: %d blocks, want %d", res, windows[i].Format("15:04"), b.Blocks, blocksIn(windows[i], windows[i].Add(width)))
			}
		}
	}
}

// An outage that fell below the prune frontier before the fill reached it, with a real retention, a
// prune between replay batches and the anchor block long gone from the database, comes back whole.
func TestIntegrationAgedHoleIsRebuiltWithPruneRunningBetweenBatches(t *testing.T) {
	fx := newAgedFixture(t)
	before := fx.snapshot(baseTime, agedStart)
	afterFrom, afterTo := agedEnd, agedEnd.Add(time.Hour)
	after := fx.snapshot(afterFrom, afterTo)
	if fx.frontier().Before(agedStart) {
		t.Fatalf("the frontier %s must already be inside the outage", fx.frontier())
	}
	if prev, err := fx.pg.BlockByNumber(context.Background(), 4663, agedFrom-1); err != nil || prev != nil {
		t.Fatalf("the block before the hole must be pruned: %v %v", prev, err)
	}
	if f := fx.frontier(); !f.After(agedStart) || !f.Before(agedEnd) {
		t.Fatalf("the frontier %s must start inside the outage", f)
	}
	fx.fill()
	if f := fx.frontier(); f.Before(agedEnd) {
		t.Fatalf("the frontier %s must have passed the outage while it was being filled", f)
	}
	if h := fx.holes(); len(h) != 0 {
		t.Fatalf("missing_ranges must be empty: %+v", h)
	}
	fx.requireWholeWindows(agedStart, agedEnd)
	sameSnapshot(t, "before the outage", fx.snapshot(baseTime, agedStart), before)
	sameSnapshot(t, "after the outage", fx.snapshot(afterFrom, afterTo), after)
	if len(fx.archive.sampleAt) == 0 || fx.archive.sampleAt[0] != agedFrom-1 {
		t.Fatalf("the replay is anchored on the archive at the block before the hole: %v", fx.archive.sampleAt)
	}
	first := fx.buckets(db.Resolution1m, agedStart, agedStart.Add(time.Minute))
	last := fx.buckets(db.Resolution1m, agedEnd.Add(-time.Minute), agedEnd)
	if len(first) != 1 || first[0].LastBlock != agedFrom+1 || len(last) != 1 || last[0].LastBlock != agedTo {
		t.Fatalf("the first and last minute of the outage hold exactly its edge blocks: %+v %+v", first, last)
	}
	hasSeries(t, scrapeRegistry(t, fx.reg), fx.holesFilled()+" 1")
}

// The production repair: the buckets of the two hours are deleted and the block range is queued
// again with an hour aligned successor. Every window is rebuilt once and nothing else moves.
func TestIntegrationRequeuedAgedHoleDoesNotDoubleCount(t *testing.T) {
	fx := newAgedFixture(t)
	fx.fill()
	fx.settle()
	want := fx.snapshot(agedStart, agedEnd)
	before := fx.snapshot(baseTime, agedStart)
	after := fx.snapshot(agedEnd, agedEnd.Add(time.Hour))
	fx.requireWholeWindows(agedStart, agedEnd)

	fx.deleteBuckets(agedStart, agedEnd)
	if len(fx.snapshot(agedStart, agedEnd)) != 0 {
		t.Fatal("the repair must leave the outage without buckets")
	}
	fx.requeue(agedFrom, agedTo, agedEnd)
	fx.fill()
	fx.settle()
	if h := fx.holes(); len(h) != 0 {
		t.Fatalf("missing_ranges must be empty: %+v", h)
	}
	sameSnapshot(t, "rebuilt outage", fx.snapshot(agedStart, agedEnd), want)
	sameSnapshot(t, "before the outage", fx.snapshot(baseTime, agedStart), before)
	sameSnapshot(t, "after the outage", fx.snapshot(agedEnd, agedEnd.Add(time.Hour)), after)
	fx.requireWholeWindows(agedStart, agedEnd)
}

// Deletion bounds narrower than the queued blocks: the first hour of the range still has its
// buckets, which already count its blocks. They are left as they are, the range is recorded as
// incomplete history instead of filled, and no block is counted twice.
func TestIntegrationRequeuedHoleOverSurvivingBucketsIsRefused(t *testing.T) {
	fx := newAgedFixture(t)
	fx.fill()
	fx.settle()
	want := fx.snapshot(agedStart, agedEnd)
	filled := fx.holesFilledCount()

	mid := agedStart.Add(time.Hour)
	fx.deleteBuckets(mid, agedEnd)
	fx.requeue(agedFrom, agedTo, agedEnd)
	fx.fill()
	fx.settle()
	h := fx.holes()
	if len(h) != 1 || h[0].Reason != reasonIncompleteHistory || h[0].Lifecycle != rangeBlocked || h[0].State == nil || h[0].State.Unbuilt == 0 {
		t.Fatalf("a range over surviving buckets is incomplete history: %+v", h)
	}
	if got := fx.holesFilledCount(); got != filled {
		t.Fatalf("holes_filled_total moved from %d to %d", filled, got)
	}
	sameSnapshot(t, "outage", fx.snapshot(agedStart, agedEnd), want)
	fx.requireWholeWindows(agedStart, agedEnd)
}

// Block bounds wider than the deletion bounds on the trailing side: the range runs half an hour into
// live history whose buckets were not deleted. They already count those blocks, so they are left
// alone and the range is recorded as incomplete history.
func TestIntegrationRequeuedHoleRunningIntoLiveBucketsIsRefused(t *testing.T) {
	fx := newAgedFixture(t)
	fx.fill()
	fx.settle()
	want := fx.snapshot(agedStart, agedEnd.Add(time.Hour))
	filled := fx.holesFilledCount()

	fx.deleteBuckets(agedStart, agedEnd)
	to := uint64(agedTo + agedPerHour/2)
	fx.requeue(agedFrom, to, agedEnd.Add(30*time.Minute))
	fx.fill()
	fx.settle()
	h := fx.holes()
	if len(h) != 1 || h[0].Reason != reasonIncompleteHistory || h[0].State == nil || h[0].State.Unbuilt == 0 {
		t.Fatalf("a range running into live buckets is incomplete history: %+v", h)
	}
	if got := fx.holesFilledCount(); got != filled {
		t.Fatalf("holes_filled_total moved from %d to %d", filled, got)
	}
	sameSnapshot(t, "outage and the live hour after it", fx.snapshot(agedStart, agedEnd.Add(time.Hour)), want)
	fx.requireWholeWindows(agedStart, agedEnd)
}

func (fx *agedFixture) holesFilledCount() int {
	fx.t.Helper()
	for line := range strings.SplitSeq(scrapeRegistry(fx.t, fx.reg), "\n") {
		if rest, ok := strings.CutPrefix(line, fx.holesFilled()+" "); ok {
			n, err := strconv.Atoi(strings.TrimSpace(rest))
			if err != nil {
				fx.t.Fatal(err)
			}
			return n
		}
	}
	fx.t.Fatal("no holes_filled_total series")
	return 0
}

// A range queued without a successor time cannot prove any window lies wholly inside it, so nothing
// is built and the range is recorded as incomplete history instead of filled.
func TestIntegrationRequeuedHoleWithoutASuccessorBuildsNothing(t *testing.T) {
	fx := newAgedFixture(t)
	fx.fill()
	fx.settle()
	fx.deleteBuckets(agedStart, agedEnd)
	fx.requeue(agedFrom, agedTo, time.Time{})
	fx.fill()
	h := fx.holes()
	if len(h) != 1 || h[0].Reason != reasonIncompleteHistory {
		t.Fatalf("incomplete history expected: %+v", h)
	}
	if got := fx.snapshot(agedStart, agedEnd); len(got) != 0 {
		t.Fatalf("no window may be written without proof it is whole: %d buckets", len(got))
	}
}

// A range queued wholly inside one surviving window, its predecessor and successor both in that
// window's hour. The hour bucket already counts the range's blocks and cannot be told otherwise from
// its own fields, so it is left alone and the range is recorded as incomplete history.
func TestIntegrationRequeuedHoleInsideOneSurvivingWindowIsRefused(t *testing.T) {
	fx := newAgedFixture(t)
	fx.fill()
	fx.settle()
	want := fx.snapshot(agedStart, agedEnd.Add(time.Hour))
	filled := fx.holesFilledCount()

	from, to := uint64(agedFrom+40), uint64(agedFrom+59)
	fx.requeue(from, to, baseTime.Add(time.Duration(to+1)*agedSpacing*time.Second))
	fx.fill()
	fx.settle()
	h := fx.holes()
	if len(h) != 1 || h[0].Reason != reasonIncompleteHistory || h[0].State == nil || h[0].State.Unbuilt == 0 {
		t.Fatalf("a range inside one surviving window is incomplete history: %+v", h)
	}
	if got := fx.holesFilledCount(); got != filled {
		t.Fatalf("holes_filled_total moved from %d to %d", filled, got)
	}
	sameSnapshot(t, "buckets around the range", fx.snapshot(agedStart, agedEnd.Add(time.Hour)), want)
}
