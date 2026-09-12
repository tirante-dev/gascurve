package collector

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/db/dbtest"
	"github.com/tirante-dev/gascurve/internal/nitro"
	"github.com/tirante-dev/gascurve/internal/pricer"
)

func constraintState() *pricer.State {
	return &pricer.State{MinBaseFee: big.NewInt(1), Constraints: []pricer.Constraint{{Target: 1, Window: 1}}}
}

func legacyState() *pricer.State {
	return &pricer.State{MinBaseFee: big.NewInt(1), Legacy: &pricer.Legacy{SpeedLimit: 1, Inertia: 1}}
}

// versioned builds headers from block 10 upwards, one per version.
func versioned(versions ...uint64) []nitro.Header {
	out := make([]nitro.Header, len(versions))
	for i, v := range versions {
		out[i] = nitro.Header{Number: 10 + uint64(i), ArbOSVersion: v}
	}
	return out
}

func TestUnsupportedModel(t *testing.T) {
	old := pricer.FirstConstraintVersion - 1
	tests := []struct {
		name             string
		state            *pricer.State
		headers          []nitro.Header
		wantFrom, wantTo uint64
		want             bool
	}{
		{"all on the constraint model", constraintState(), versioned(51, 61), 0, 0, false},
		{"a legacy replay is the model those blocks ran", legacyState(), versioned(old, old), 0, 0, false},
		{"no state at all", nil, versioned(old), 0, 0, false},
		{"unrecorded versions are not evidence", constraintState(), versioned(0, 0), 0, 0, false},
		{"a run below the constraint model", constraintState(), versioned(old, old, 51), 10, 11, true},
		{"one block below it", constraintState(), versioned(51, old, 61), 11, 11, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			from, to, got := unsupportedModel(tt.state, tt.headers)
			if got != tt.want || from != tt.wantFrom || to != tt.wantTo {
				t.Errorf("unsupportedModel = (%d, %d, %v), want (%d, %d, %v)", from, to, got, tt.wantFrom, tt.wantTo, tt.want)
			}
		})
	}
}

func TestSpansArbOSUpgrade(t *testing.T) {
	tests := []struct {
		name    string
		headers []nitro.Header
		want    uint64
		wantOK  bool
	}{
		{"one version throughout", versioned(61, 61, 61), 0, false},
		{"an upgrade in the middle", versioned(51, 51, 61), 12, true},
		{"unrecorded versions are skipped", versioned(51, 0, 0, 51), 0, false},
		{"an upgrade across an unrecorded block", versioned(51, 0, 61), 12, true},
		{"nothing recorded at all", versioned(0, 0), 0, false},
		{"no headers", nil, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			at, ok := spansArbOSUpgrade(tt.headers)
			if ok != tt.wantOK || at != tt.want {
				t.Errorf("spansArbOSUpgrade = (%d, %v), want (%d, %v)", at, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestKnownArbOSVersionZeroIsNotMissing(t *testing.T) {
	headers := []nitro.Header{
		{Number: 10, ArbOSVersion: 0, ArbOSVersionKnown: true},
		{Number: 11, ArbOSVersion: 51},
	}
	if from, to, found := unsupportedModel(constraintState(), headers); !found || from != 10 || to != 10 {
		t.Fatalf("known version zero must predate the constraint model: %d..%d %v", from, to, found)
	}
	if at, found := spansArbOSUpgrade(headers); !found || at != 11 {
		t.Fatalf("known version zero must participate in upgrades: %d %v", at, found)
	}
	if stored := arbOSVersionOf(headers[0]); !stored.Valid || stored.Int64 != 0 {
		t.Fatalf("known version zero must be stored: %+v", stored)
	}
	if live := liveArbOSVersion(headers[0]); live == nil || *live != 0 {
		t.Fatalf("known version zero must be published: %v", live)
	}
}

// A catch-up that encounters an unsupported prefix seeds the sampled head, but every skipped block is
// covered by the blocked range. A supported suffix must not disappear between the narrow refusal and
// the new seed.
func TestCatchUpUnsupportedPrefixRecordsEverySkippedBlock(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	rpc.arbos = pricer.FirstConstraintVersion - 1
	rpc.arbosFrom, rpc.arbosTo = 1002, 61
	rpc.setHead(1003)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	holes := holesOf(t, store)
	if len(holes) != 1 || holes[0].From != 1001 || holes[0].To != 1002 || holes[0].Reason != reasonUnsupportedModel {
		t.Fatalf("every skipped block must be accounted for: %+v", holes)
	}
	if b, _ := store.BlockByNumber(ctx, 4663, 1002); b != nil {
		t.Fatalf("a skipped supported suffix may not look indexed: %+v", b)
	}
	if b, _ := store.BlockByNumber(ctx, 4663, 1003); b == nil {
		t.Fatal("the sampled head must still seed the new replay")
	}
}

// A gap whose blocks predate the multi-constraint pricer is recorded as unfillable rather than priced
// with a constraint set the chain did not have. It stays visible as blocked work, so an operator sees
// the range rather than a plausible number over it.
func TestFillRefusesBlocksOlderThanTheConstraintModel(t *testing.T) {
	rpc := newFakeRPC(1000)
	rpc.arbos = pricer.FirstConstraintVersion - 1
	rpc.arbosFrom, rpc.arbosTo = gapHead, 61
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	skipGap(t, f, rpc)

	rpc.hooks["HeadersByNumbers"] = rewound(t, f, store)
	status, err := f.FillStep(context.Background())
	if err != nil {
		t.Fatalf("fill step: %v", err)
	}
	if status != FillIdle {
		t.Fatalf("a rewind during the refusal must discard its mark: %v", status)
	}
	if holes := holesOf(t, store); len(holes) != 1 || holes[0].Reason != reasonCatchUpLimit || holes[0].Lifecycle != rangePending {
		t.Fatalf("a stale refusal must leave the queued range unchanged: %+v", holes)
	}
	delete(rpc.hooks, "HeadersByNumbers")
	status, err = f.FillStep(context.Background())
	if err != nil {
		t.Fatalf("fill retry: %v", err)
	}
	if status != FillNone {
		t.Fatalf("a range on a model the pricer does not implement is not fillable work: %v", status)
	}
	holes := holesOf(t, store)
	if len(holes) != 1 || holes[0].Reason != reasonUnsupportedModel || holes[0].Lifecycle != rangeBlocked {
		t.Fatalf("the range must be blocked as an unsupported model: %+v", holes)
	}
	if b, _ := store.BlockByNumber(context.Background(), 4663, 1001); b != nil {
		t.Fatalf("no block of the refused range may be priced: %+v", b)
	}
	before := len(rpc.headerCalls)
	if status, err := f.FillStep(context.Background()); err != nil || status != FillNone {
		t.Fatalf("blocked range: %v %v", status, err)
	}
	if len(rpc.headerCalls) != before {
		t.Fatalf("an unsupported range must not be fetched again: %v", rpc.headerCalls[before:])
	}
}

// The same range fills normally once its blocks are on a version that has the constraint model, which
// is what keeps the refusal from swallowing ordinary history.
func TestFillPricesBlocksOnTheConstraintModel(t *testing.T) {
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	skipGap(t, f, rpc)
	fillAll(t, f, 40)
	if holes := holesOf(t, store); holes != nil {
		t.Fatalf("the range must fill: %+v", holes)
	}
	b, _ := store.BlockByNumber(context.Background(), 4663, 1001)
	if b == nil || !b.ArbOSVersion.Valid || b.ArbOSVersion.Int64 != 61 {
		t.Fatalf("a filled block carries the version its header recorded: %+v", b)
	}
}

// A range that crosses an upgrade is filled: the model is one the pricer implements on both sides. What
// changes is the bucket, which records the span so the api can say the replay crossed a boundary. With
// the same upgrade above the range, every bucket names one version instead.
func TestFillAcrossAnArbOSUpgradeRecordsTheSpan(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name      string
		upgradeAt uint64
		wantSpan  bool
	}{
		{"the upgrade lands inside the gap", 1100, true},
		{"the upgrade lands above the gap", gapHead + 1, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rpc := newFakeRPC(1000)
			rpc.arbos, rpc.arbosFrom, rpc.arbosTo = 51, tt.upgradeAt, 61
			store := dbtest.New()
			f := newTestFollower(t, rpc, store)
			skipGap(t, f, rpc)
			fillAll(t, f, 40)

			below, _ := store.BlockByNumber(ctx, 4663, 1050)
			if below == nil || !below.ArbOSVersion.Valid || below.ArbOSVersion.Int64 != 51 {
				t.Fatalf("a block below the upgrade carries the old version: %+v", below)
			}
			spanning, named := 0, 0
			for _, b := range store.BucketRows {
				switch {
				case b.SpansArbOSUpgrade():
					spanning++
				case b.ArbOSVersionMin.Valid:
					named++
				}
			}
			if (spanning > 0) != tt.wantSpan {
				t.Fatalf("spanning buckets = %d, want any = %v", spanning, tt.wantSpan)
			}
			if !tt.wantSpan && named == 0 {
				t.Fatal("every bucket must name the one version its blocks ran")
			}
		})
	}
}

// The backfill walks into older blocks, so it is where a chain that ran the legacy pricer before an
// upgrade is met. It stops at the model boundary and records the rest as one unfillable range instead
// of pricing pre-ArbOS-50 blocks with the constraint set recorded above them.
func TestBackfillStopsBelowTheConstraintModel(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	rpc.arbos, rpc.arbosFrom, rpc.arbosTo = pricer.FirstConstraintVersion-1, 400, 61
	store := dbtest.New()
	seedSets(t, store)
	f := newTestFollower(t, rpc, store)
	f.cfg.BackfillDepth = config.Depth(70 * time.Second)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runBackfill(t, f, 100)

	holes := holesOf(t, store)
	found := false
	for _, h := range holes {
		if h.Reason == reasonUnsupportedModel && h.Lifecycle == rangeBlocked {
			found = true
		}
	}
	if !found {
		t.Fatalf("the stretch below the constraint model must be recorded as unsupported: %+v", holes)
	}
	// Nothing below the upgrade may be priced, and the refusal must not swallow blocks above it.
	if b, _ := store.BlockByNumber(ctx, 4663, 350); b != nil {
		t.Fatalf("a pre-model block was priced anyway: %+v", b)
	}
}

// The same walk over a chain that ran the constraint model throughout reconstructs normally, so the
// stop is a model check and not a refusal to backfill deep history.
func TestBackfillPricesAConstraintModelChainThroughout(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	rpc.arbos, rpc.arbosFrom, rpc.arbosTo = 51, 400, 61
	store := dbtest.New()
	seedSets(t, store)
	f := newTestFollower(t, rpc, store)
	f.cfg.BackfillDepth = config.Depth(70 * time.Second)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runBackfill(t, f, 100)
	for _, h := range holesOf(t, store) {
		if h.Reason == reasonUnsupportedModel {
			t.Fatalf("a chain on the constraint model throughout must not be refused: %+v", h)
		}
	}
}
