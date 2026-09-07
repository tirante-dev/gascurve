package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/logger"
	"github.com/tirante-dev/gascurve/internal/metrics"
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

func TestMetricsEndpointServesRequestSeries(t *testing.T) {
	ts := newServer(t, seed(t))
	// One request per shape the labels must tell apart: a path parameter
	// that must not become a label of its own, an error status, and a path
	// no route claims.
	get(t, ts, "/api/v1/networks/robinhood/blocks?limit=2")
	get(t, ts, "/api/v1/networks/robinhood-testnet/blocks?limit=2")
	get(t, ts, "/api/v1/networks/nope/live")
	get(t, ts, "/api/v1/not-a-route")

	res, body := get(t, ts, metrics.Path)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	text := string(body)
	for _, want := range []string{
		`gascurve_api_requests_total{method="GET",route="/api/v1/networks/{network}/blocks",status="200"} 2`,
		`gascurve_api_requests_total{method="GET",route="/api/v1/networks/{network}/live",status="404"} 1`,
		`gascurve_api_request_duration_seconds_count{method="GET",route="/api/v1/networks/{network}/blocks"} 2`,
		// A path no route claims is labeled with the wildcard chi
		// matched, never with the path the caller sent.
		`gascurve_api_requests_total{method="GET",route="/api/v1/*",status="404"} 1`,
		"gascurve_api_ws_clients 0",
	} {
		hasSeries(t, text, want)
	}
	if strings.Contains(text, "not-a-route") {
		t.Fatalf("the path of a 404 became a label value:\n%s", text)
	}
	// A network name in the path must never become a label of its own.
	if strings.Contains(text, `route="/api/v1/networks/robinhood/blocks"`) {
		t.Fatalf("the request path became a route label:\n%s", text)
	}
	// The scrape does not count itself.
	if strings.Contains(text, `route="`+metrics.Path+`"`) {
		t.Fatalf("the metrics endpoint counted itself:\n%s", text)
	}
}

func TestMetricsRegistryCanBeSupplied(t *testing.T) {
	store := seed(t)
	reg := prometheus.NewRegistry()
	cfg := config.ServerConfig{RateLimitPerSecond: 1000, RateLimitBurst: 1000}
	s := New(store, cfg, nil, logger.Nop(), WithMetrics(metrics.NewAPI(reg), reg))
	// A nil pair leaves the server's own registry in place.
	s2 := New(store, cfg, nil, logger.Nop(), WithMetrics(nil, nil))
	for _, srv := range []*Server{s, s2} {
		serve := func(path string) *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, http.NoBody))
			return rec
		}
		if rec := serve("/api/v1/status"); rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		hasSeries(t, serve(metrics.Path).Body.String(),
			`gascurve_api_requests_total{method="GET",route="/api/v1/status",status="200"} 1`)
	}
}

// TestMetricsBoundsTheMethodLabel: an HTTP method is an arbitrary token,
// so a caller sending a fresh one per request must not be able to mint a
// series each time.
func TestMetricsBoundsTheMethodLabel(t *testing.T) {
	ts := newServer(t, seed(t))
	for _, method := range []string{"X000001", "X000002", "WHATEVER"} {
		req, err := http.NewRequest(method, ts.URL+"/api/v1/status", http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	_, body := get(t, ts, metrics.Path)
	text := string(body)
	for _, method := range []string{"X000001", "X000002", "WHATEVER"} {
		if strings.Contains(text, method) {
			t.Fatalf("the method %q became a label value:\n%s", method, text)
		}
	}
	// All three collapse onto one series, whatever the router answered.
	if !strings.Contains(text, `method="other"`) {
		t.Fatalf("unknown methods must collapse onto one label value:\n%s", text)
	}
}

func TestRoutePatternFallsBackWithoutARouter(t *testing.T) {
	// A request that never reached chi (a middleware ahead of the router,
	// or a handler mounted on its own) has no pattern to report.
	if got := routePattern(httptest.NewRequest(http.MethodGet, "/whatever", http.NoBody)); got != unmatchedRoute {
		t.Fatalf("routePattern = %q, want %q", got, unmatchedRoute)
	}
}

func TestMetricsEndpointServesWebSocketSeries(t *testing.T) {
	h := newWSHarness(t, seed(t), time.Hour)
	conn := h.dial(t, "robinhood")
	if typ, _ := readMsg(t, conn); typ != "hello" {
		t.Fatalf("first message = %q", typ)
	}
	// The client is counted before the write loop starts, so reading the
	// hello proves that gauge. The frame counter is incremented after
	// conn.Write returns, which the client's read orders nothing against, so
	// it has to be waited for rather than asserted outright.
	hasSeries(t, scrapeHarness(t, h), "gascurve_api_ws_clients 1")
	waitFor(t, func() bool {
		return strings.Contains(scrapeHarness(t, h), "gascurve_api_ws_frames_sent_total 1")
	}, "the hello frame must be counted")

	_ = conn.Close(websocket.StatusNormalClosure, "bye")
	waitFor(t, func() bool {
		return strings.Contains(scrapeHarness(t, h), "gascurve_api_ws_clients 0")
	}, "the client count must drop when the socket closes")
}

func TestMetricsCountsDroppedWebSocketClients(t *testing.T) {
	h := newWSHarness(t, seed(t), time.Hour)
	slow := &client{hub: h.hub, send: make(chan []byte, 1), closed: make(chan struct{})}
	slow.enqueue([]byte("a"))
	// The second message overflows the queue: the peer is not reading, so
	// the client is dropped rather than left to hold a connection slot.
	slow.enqueue([]byte("b"))
	hasSeries(t, scrapeHarness(t, h), "gascurve_api_ws_clients_dropped_total 1")
}

// TestMetricsCountsRefusedWebSocketHandshakes: a handshake the server
// refused is an ordinary answer and must reach the error rate, while one
// that upgraded is not a request at all.
func TestMetricsCountsRefusedWebSocketHandshakes(t *testing.T) {
	h := newWSHarness(t, seed(t), time.Hour)
	// Refused before the upgrade: no network parameter is a 400.
	resp, err := http.Get(h.ts.URL + "/api/v1/ws")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// And one that upgrades cleanly is not counted.
	conn := h.dial(t, "robinhood")
	if typ, _ := readMsg(t, conn); typ != "hello" {
		t.Fatalf("first message = %q", typ)
	}
	body := scrapeHarness(t, h)
	hasSeries(t, body, `gascurve_api_requests_total{method="GET",route="/api/v1/ws",status="400"} 1`)
	if strings.Contains(body, `route="/api/v1/ws",status="101"`) || strings.Contains(body, `route="/api/v1/ws",status="200"`) {
		t.Fatalf("an upgraded socket was counted as a request:\n%s", body)
	}
}

// TestMetricsEndpointIsNotRateLimited: where a proxy makes ordinary
// traffic and Prometheus share one address, a throttled scrape reads as a
// dead api, so the endpoint is exempt from the per-IP budget.
func TestMetricsEndpointIsNotRateLimited(t *testing.T) {
	store := seed(t)
	// One request per second, burst one: the second ordinary request is
	// refused, and every scrape still succeeds.
	cfg := config.ServerConfig{RateLimitPerSecond: 1, RateLimitBurst: 1}
	s := New(store, cfg, nil, logger.Nop())
	serve := func(path string) int {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, http.NoBody))
		return rec.Code
	}
	if got := serve("/api/v1/status"); got != http.StatusOK {
		t.Fatalf("first request = %d", got)
	}
	if got := serve("/api/v1/status"); got != http.StatusTooManyRequests {
		t.Fatalf("the budget must still apply to ordinary traffic, got %d", got)
	}
	for i := range 5 {
		if got := serve(metrics.Path); got != http.StatusOK {
			t.Fatalf("scrape %d = %d, want 200", i, got)
		}
	}
}

// TestMetricsCountsAConcurrentDropOnce: two goroutines can find the queue
// full at the same moment, and one connection going away is one drop.
func TestMetricsCountsAConcurrentDropOnce(t *testing.T) {
	h := newWSHarness(t, seed(t), time.Hour)
	slow := &client{hub: h.hub, send: make(chan []byte, 1), closed: make(chan struct{})}
	slow.enqueue([]byte("a")) // fills the queue
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slow.enqueue([]byte("b"))
		}()
	}
	wg.Wait()
	hasSeries(t, scrapeHarness(t, h), "gascurve_api_ws_clients_dropped_total 1")
}

// scrapeHarness reads the harness server's metrics endpoint.
func scrapeHarness(t *testing.T, h *wsHarness) string {
	t.Helper()
	resp, err := http.Get(h.ts.URL + metrics.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// waitFor polls cond until it holds or the test gives up.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}
