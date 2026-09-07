package collector

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/db/dbtest"
	"github.com/tirante-dev/gascurve/internal/metrics"
	"github.com/tirante-dev/gascurve/internal/model"
	"github.com/tirante-dev/gascurve/internal/nitro"
)

type monitorClock struct{ unix atomic.Int64 }

func newMonitorClock(at time.Time) *monitorClock {
	c := &monitorClock{}
	c.unix.Store(at.UnixNano())
	return c
}

func (c *monitorClock) Now() time.Time { return time.Unix(0, c.unix.Load()).UTC() }
func (c *monitorClock) Add(d time.Duration) {
	c.unix.Add(int64(d))
}

type fixedDatabaseStats struct{ value db.Stats }

func (s fixedDatabaseStats) Stats() db.Stats { return s.value }

func monitorRequest(t *testing.T, h http.Handler, path string) (status int, body string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, http.NoBody)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	b, err := io.ReadAll(w.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	return w.Code, string(b)
}

func TestMonitorHealthPersistenceAndMetrics(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	clock := newMonitorClock(now)
	store := dbtest.New()
	if err := store.UpsertNetwork(ctx, db.Network{ChainID: 4663, Name: "robinhood", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetState(ctx, 4663, db.StateHoles, `[{"from":100,"to":109,"at":"2026-09-07T11:59:00Z","next":105}]`); err != nil {
		t.Fatal(err)
	}
	rpc := newFakeRPC(110)
	rpc.stats = nitro.Stats{
		CallsLast10s: 7, Calls: 40, Requests: 8, Errors: 2,
		TotalLatency: 400 * time.Millisecond, RateLimitEvents: 3, Last429At: now.Add(-time.Minute),
	}
	reg := metrics.NewRegistry()
	collectorMetrics := metrics.NewCollector(reg)
	monitor := NewMonitor(store, fixedDatabaseStats{db.Stats{
		Operations: 20, Errors: 1, TotalLatency: 200 * time.Millisecond, LastLatency: 20 * time.Millisecond,
	}}, collectorMetrics, nil, WithMonitorClock(clock.Now), WithHeartbeatInterval(time.Second))
	NewFollower(Options{
		Network:   config.NetworkConfig{Name: "robinhood", ChainID: 4663, Enabled: true},
		Collector: config.CollectorConfig{TickInterval: time.Second, SlowInterval: time.Minute},
		RPC:       rpc, Store: store, Metrics: collectorMetrics, Monitor: monitor,
	})
	handler := monitor.Handler("test")
	if status, _ := monitorRequest(t, handler, "/startup"); status != http.StatusServiceUnavailable {
		t.Fatalf("startup before Run = %d", status)
	}
	if status, _ := monitorRequest(t, handler, "/health"); status != http.StatusOK {
		t.Fatalf("shallow health = %d", status)
	}
	if status, _ := monitorRequest(t, handler, "/ready"); status != http.StatusServiceUnavailable {
		t.Fatalf("ready before a fast success = %d", status)
	}

	monitor.observeHead(4663, 110, 108)
	monitor.observeLoop(4663, loopFast, now.Add(-25*time.Millisecond), nil)
	monitor.observeLoop(4663, loopSlow, now.Add(-50*time.Millisecond), nil)
	monitor.observeLoop(4663, loopHistory, now.Add(-75*time.Millisecond), nil)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		monitor.Run(runCtx)
		close(done)
	}()
	waitFor(t, "telemetry checkpoint", func() bool {
		_, ok, _ := store.GetState(ctx, 4663, db.StateTelemetry)
		return ok
	})
	cancel()
	<-done

	if status, body := monitorRequest(t, handler, "/startup"); status != http.StatusOK || !strings.Contains(body, `"version":"test"`) {
		t.Fatalf("startup = %d %s", status, body)
	}
	if status, body := monitorRequest(t, handler, "/ready"); status != http.StatusOK || !strings.Contains(body, `"ready"`) {
		t.Fatalf("ready = %d %s", status, body)
	}
	raw, ok, err := store.GetState(ctx, 4663, db.StateTelemetry)
	if err != nil || !ok {
		t.Fatalf("telemetry state: %t %v", ok, err)
	}
	var telemetry model.CollectorTelemetry
	if err := json.Unmarshal([]byte(raw), &telemetry); err != nil {
		t.Fatal(err)
	}
	if telemetry.HeartbeatAt == nil || telemetry.ObservedHead != 110 || telemetry.IndexedHead != 108 || telemetry.HeadLagBlocks != 2 {
		t.Fatalf("head telemetry: %+v", telemetry)
	}
	if telemetry.Loops.Fast.LastSuccessAt == nil || telemetry.Loops.Fast.LastDurationMS != 25 {
		t.Fatalf("loop telemetry: %+v", telemetry.Loops)
	}
	if telemetry.RPC.Calls != 40 || telemetry.RPC.Requests != 8 || telemetry.RPC.Errors != 2 || telemetry.RPC.AverageLatencyMS != 50 {
		t.Fatalf("rpc telemetry: %+v", telemetry.RPC)
	}
	if telemetry.Database.Operations != 20 || telemetry.Database.Errors != 1 || telemetry.Database.AverageLatencyMS != 10 || telemetry.Database.LastLatencyMS != 20 {
		t.Fatalf("database telemetry: %+v", telemetry.Database)
	}
	metricsBody := scrapeRegistry(t, reg)
	for _, want := range []string{
		`gascurve_collector_head_lag_blocks{chain_id="4663",network="robinhood"} 2`,
		`gascurve_collector_holes_pending_blocks{chain_id="4663",network="robinhood"} 5`,
		`gascurve_collector_rpc_errors_total{chain_id="4663",network="robinhood"} 2`,
		`gascurve_collector_rpc_latency_seconds{chain_id="4663",network="robinhood"} 0.05`,
		`gascurve_collector_database_errors_total 1`,
		`gascurve_collector_loop_last_success_timestamp_seconds{chain_id="4663",loop="history",network="robinhood"} 1.7887824e+09`,
	} {
		if !strings.Contains(metricsBody, want) {
			t.Errorf("metrics missing %q:\n%s", want, metricsBody)
		}
	}
	if strings.Count(metricsBody, "# TYPE gascurve_collector_rpc_errors_total counter") != 1 {
		t.Fatalf("metric declaration must occur once:\n%s", metricsBody)
	}

	clock.Add(time.Second)
	monitor.observeLoop(4663, loopFast, clock.Now(), context.DeadlineExceeded)
	if status, _ := monitorRequest(t, handler, "/ready"); status != http.StatusServiceUnavailable {
		t.Fatalf("a current fast-loop error must make readiness fail: %d", status)
	}
	monitor.observeLoop(4663, loopFast, clock.Now(), nil)
	clock.Add(time.Minute)
	if status, _ := monitorRequest(t, handler, "/ready"); status != http.StatusServiceUnavailable {
		t.Fatalf("a stale fast loop must make readiness fail: %d", status)
	}
	if status, _ := monitorRequest(t, handler, "/health"); status != http.StatusOK {
		t.Fatalf("an upstream outage must not fail liveness: %d", status)
	}
}
