package api

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/db/dbtest"
	"github.com/tirante-dev/gascurve/internal/model"
	"github.com/tirante-dev/gascurve/internal/pricer"
)

func arbos(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

// unmeasured is an ArbOS version above every one the pricer records as measured, so the tests keep
// asking about an unverified version as the list grows.
var unmeasured = func() int64 {
	measured := pricer.VerifiedVersions()
	return int64(measured[len(measured)-1]) + 1
}()

func fidelityBlock(number uint64, at time.Time, version sql.NullInt64) db.Block {
	return db.Block{
		ChainID: robinhood, Number: number, TS: at, GasUsed: 10, PosterGas: sql.NullInt64{Valid: true},
		BaseFee: db.WeiFromUint64(100), PredictedBaseFee: db.NullWeiFromUint64(100), MinBaseFee: db.NullWeiFromUint64(50),
		Backlogs: db.Uint64Array{1}, PricingVersion: db.PricingFull, ArbOSVersion: version,
	}
}

// A block point carries the version its header recorded, so a client reading per-block history can see
// where a boundary falls without inferring it from the constraint set.
func TestBlockPointCarriesTheArbOSVersion(t *testing.T) {
	points := blockPoints([]db.Block{
		fidelityBlock(1, now, arbos(61)),
		fidelityBlock(2, now, sql.NullInt64{}),
	}, nil)
	if len(points) != 2 {
		t.Fatalf("points: %+v", points)
	}
	if points[0].ArbOSVersionMin == nil || *points[0].ArbOSVersionMin != 61 || points[0].ReplayFidelity != model.FidelityVerified {
		t.Errorf("a block on a measured version is verified: %+v", points[0])
	}
	if points[1].ArbOSVersionMin != nil || points[1].ReplayFidelity != model.FidelityUnknown {
		t.Errorf("a block with no recorded version says so: %+v", points[1])
	}
}

// The bucket path is what the history ranges serve, and it is where a boundary shows: a bucket whose
// blocks straddle an upgrade reports the span rather than a single version.
func TestBucketPointReportsFidelity(t *testing.T) {
	tests := []struct {
		name     string
		lo, hi   sql.NullInt64
		want     string
		wantSpan bool
	}{
		{"one measured version", arbos(61), arbos(61), model.FidelityVerified, false},
		{"across an upgrade", arbos(51), arbos(61), model.FidelityBoundary, true},
		{"a version nobody measured", arbos(unmeasured), arbos(unmeasured), model.FidelityUnverified, false},
		{"nothing recorded", sql.NullInt64{}, sql.NullInt64{}, model.FidelityUnknown, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := db.Bucket{
				ChainID: robinhood, Resolution: db.Resolution1m, BucketStart: now.Add(-time.Hour).Truncate(time.Minute), Blocks: 2,
				BaseFeeMin: db.WeiFromUint64(1), BaseFeeAvg: db.WeiFromUint64(1), BaseFeeMax: db.WeiFromUint64(1), FeesWei: db.WeiFromUint64(1),
				PricingVersion: db.PricingFull, ArbOSVersionMin: tt.lo, ArbOSVersionMax: tt.hi,
			}
			p, ok := bucketPoint(b, time.Minute, now, time.Time{})
			if !ok {
				t.Fatal("the bucket must render")
			}
			if p.ReplayFidelity != tt.want {
				t.Errorf("replayFidelity = %q, want %q", p.ReplayFidelity, tt.want)
			}
			if (p.ArbOSVersionMin != nil) != tt.lo.Valid {
				t.Errorf("arbosVersionMin = %v, want valid = %v", p.ArbOSVersionMin, tt.lo.Valid)
			}
			if tt.wantSpan && (p.ArbOSVersionMin == nil || p.ArbOSVersionMax == nil || *p.ArbOSVersionMin == *p.ArbOSVersionMax) {
				t.Errorf("a boundary bucket must report both ends: %+v", p)
			}
		})
	}
}

// The stepped-down 1h path aggregates blocks itself rather than reading a stored bucket, so it has its
// own answer to give and one block without a version has to sink the whole step.
func TestSteppedPointsAggregateTheArbOSRange(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New()
	if err := store.UpsertNetwork(ctx, db.Network{ChainID: robinhood, Name: "robinhood", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// Enough blocks to force the step down. The upgrades land inside a step rather than on its edge, so
	// a step really does straddle one, and the last stretch recorded no version at all.
	var blocks []db.Block
	for i := uint64(0); i < 2500; i++ {
		version := arbos(unmeasured)
		switch {
		case i >= 2000:
			version = sql.NullInt64{}
		case i >= 1025:
			version = arbos(61)
		case i >= 525:
			version = arbos(51)
		}
		blocks = append(blocks, fidelityBlock(i, now.Add(-time.Duration(2500-i)*100*time.Millisecond).Truncate(time.Second), version))
	}
	if err := store.UpsertBlocks(ctx, blocks); err != nil {
		t.Fatal(err)
	}
	ts := newServer(t, store)
	resp, body := get(t, ts, "/api/v1/networks/robinhood/series?range=1h")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var s model.Series
	decode(t, body, &s)
	if s.Resolution != "5s" {
		t.Fatalf("resolution %s", s.Resolution)
	}
	seen := map[string]int{}
	for _, p := range s.Points {
		seen[p.ReplayFidelity]++
	}
	for _, want := range []string{model.FidelityVerified, model.FidelityBoundary, model.FidelityUnverified, model.FidelityUnknown} {
		if seen[want] == 0 {
			t.Fatalf("no %s step in %v", want, seen)
		}
	}
}
