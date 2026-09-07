package collector

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tirante-dev/gascurve/internal/db/dbtest"
	"github.com/tirante-dev/gascurve/internal/logger"
	"github.com/tirante-dev/gascurve/internal/metrics"
	"github.com/tirante-dev/gascurve/internal/nitro"
)

// hasSeries fails unless the exposition text carries the sample exactly.
func hasSeries(t *testing.T, body, sample string) {
	t.Helper()
	for line := range strings.SplitSeq(body, "\n") {
		if strings.TrimSpace(line) == sample {
			return
		}
	}
	t.Fatalf("missing series %q in:\n%s", sample, body)
}

// TestMetricsServerReportsFollowerState runs the collector's own metrics
// server against a follower that has ticked, scanned and filled, and reads
// it the way Prometheus would.
func TestMetricsServerReportsFollowerState(t *testing.T) {
	ctx := context.Background()
	reg := metrics.NewRegistry()
	m := metrics.NewCollector(reg)
	srv, err := metrics.NewServer(ctx, "127.0.0.1:0", reg, logger.Nop())
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- srv.Run(sctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("metrics server: %v", err)
		}
	}()

	pool := &fakePool{fakeRPC: newFakeRPC(1000), status: nitro.PoolStatus{
		Active: 1, Failovers: 2,
		Endpoints: []nitro.EndpointStatus{
			{Index: 0, Disabled: true, Error: "reports chain id 1, configured 4663", RateLimitEvents: 5},
			{Index: 1, WS: true, WSCooling: true, WSError: "dial failed"},
		},
	}}
	pool.stats = nitro.Stats{RateLimitEvents: 5, FastCalls: 11, BulkCalls: 23}
	store := dbtest.New()
	f := newTestFollower(t, pool.fakeRPC, store, func(o *Options) {
		o.RPC = pool
		o.Metrics = m
	})
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.persistStats(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.FillStep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.BackfillStep(ctx); err != nil {
		t.Fatal(err)
	}

	body := scrapeServer(t, srv.ListenAddr())
	const labels = `{chain_id="4663",network="robinhood"}`
	for _, want := range []string{
		// The seeded head and the age of the block it names.
		"gascurve_collector_head_block" + labels + " 1000",
		"gascurve_collector_head_lag_seconds" + labels + " 0",
		"gascurve_collector_tick_duration_seconds_count" + labels + " 1",
		// The pool's routing state, endpoints named by index only.
		"gascurve_collector_active_endpoint" + labels + " 1",
		"gascurve_collector_endpoint_failovers_total" + labels + " 2",
		"gascurve_collector_rate_limit_events_total" + labels + " 5",
		`gascurve_collector_endpoint_disabled{chain_id="4663",endpoint="0",network="robinhood"} 1`,
		`gascurve_collector_endpoint_ws_cooling{chain_id="4663",endpoint="1",network="robinhood"} 1`,
		`gascurve_collector_endpoint_rate_limit_events_total{chain_id="4663",endpoint="0",network="robinhood"} 5`,
		`gascurve_collector_rpc_calls_total{chain_id="4663",class="fast",network="robinhood"} 11`,
		`gascurve_collector_rpc_calls_total{chain_id="4663",class="bulk",network="robinhood"} 23`,
		// Nothing was skipped, so the history gauges read zero.
		"gascurve_collector_holes_pending" + labels + " 0",
		"gascurve_collector_holes_blocks" + labels + " 0",
		"gascurve_collector_backfill_done" + labels + " 0",
	} {
		hasSeries(t, body, want)
	}
	// An endpoint URL must never reach a label, and the fake pool's
	// endpoints carry a sanitized reason that must not either.
	if strings.Contains(body, "http") || strings.Contains(body, "dial failed") {
		t.Fatalf("an endpoint URL or error reached the exposition text:\n%s", body)
	}
}

// TestMetricsRecordSkippedGapsAndFilledHoles drives the two counters the
// history loops own, on a network whose budget cannot follow the chain.
func TestMetricsRecordSkippedGapsAndFilledHoles(t *testing.T) {
	ctx := context.Background()
	reg := metrics.NewRegistry()
	m := metrics.NewCollector(reg)
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store, func(o *Options) { o.Metrics = m })
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// A gap wider than max_catch_up_batches header batches is skipped and
	// recorded as a hole, then filled a batch at a time.
	rpc.setHead(1000 + uint64(f.cfg.HeaderBatchSize*f.cfg.MaxCatchUpBatches) + 50)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	hasSeries(t, scrapeRegistry(t, reg), `gascurve_collector_catch_up_gaps_skipped_total{chain_id="4663",network="robinhood"} 1`)
	body := scrapeRegistry(t, reg)
	hasSeries(t, body, `gascurve_collector_holes_filled_total{chain_id="4663",network="robinhood"} 0`)

	for i := 0; i < 200; i++ {
		status, err := f.FillStep(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if status == FillNone {
			break
		}
	}
	body = scrapeRegistry(t, reg)
	hasSeries(t, body, `gascurve_collector_holes_filled_total{chain_id="4663",network="robinhood"} 1`)
	hasSeries(t, body, `gascurve_collector_holes_pending{chain_id="4663",network="robinhood"} 0`)
}

func TestBackfillStateFromCursor(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    backfillCursor
		want metrics.BackfillState
	}{
		{"fresh", backfillCursor{}, metrics.BackfillState{}},
		{"done", backfillCursor{Done: true, DepthStart: 100, Next: 900}, metrics.BackfillState{Cursor: 900, Floor: 100, Done: true}},
		{
			"between segments",
			backfillCursor{DepthStart: 100, SegStart: 500, End: 500},
			metrics.BackfillState{Cursor: 500, Floor: 100, Remaining: 400},
		},
		{
			// Inside a segment the blocks left are the rest of it plus
			// every segment still to come below it.
			"active", backfillCursor{Active: true, DepthStart: 100, SegStart: 500, Next: 700, End: 900},
			metrics.BackfillState{Cursor: 700, Floor: 100, Remaining: 600},
		},
		{
			// A depth floor above the cursor (the owner scan origin moved
			// up) must not underflow.
			"floor above the segment", backfillCursor{Active: true, DepthStart: 900, SegStart: 500, Next: 700, End: 900},
			metrics.BackfillState{Cursor: 700, Floor: 900, Remaining: 200},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := backfillState(&tc.c); got != tc.want {
				t.Fatalf("backfillState = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestPoolMetricsWithoutAPool(t *testing.T) {
	// A plain client reports the aggregate counters and no routing state.
	got := poolMetrics(nitro.Stats{RateLimitEvents: 2, FastCalls: 3, BulkCalls: 4}, nil)
	want := metrics.PoolState{RateLimitEvents: 2, FastCalls: 3, BulkCalls: 4}
	if got.Active != want.Active || got.Failovers != want.Failovers || len(got.Endpoints) != 0 ||
		got.RateLimitEvents != want.RateLimitEvents || got.FastCalls != want.FastCalls || got.BulkCalls != want.BulkCalls {
		t.Fatalf("poolMetrics = %+v, want %+v", got, want)
	}
}

// scrapeServer reads the metrics endpoint of a running server.
func scrapeServer(t *testing.T, addr string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+metrics.Path, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scrape status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// scrapeRegistry renders a registry the way a scrape would see it, without
// a server in between.
func scrapeRegistry(t *testing.T, g prometheus.Gatherer) string {
	t.Helper()
	rec := httptest.NewRecorder()
	metrics.Handler(g).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, metrics.Path, http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status %d", rec.Code)
	}
	return rec.Body.String()
}
