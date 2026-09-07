package metrics

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tirante-dev/gascurve/internal/logger"
)

// get fetches path from the server and returns its status and body.
func get(t *testing.T, addr, path string) (status int, body string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(raw)
}

func TestServerServesMetricsAndNothingElse(t *testing.T) {
	reg := NewRegistry()
	NewCollector(reg).Network("robinhood", 4663).ObserveHead(1000, sampledAt, sampledAt, sampledAt)
	srv, err := NewServer(context.Background(), "127.0.0.1:0", reg, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	status, body := get(t, srv.ListenAddr(), Path)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	hasSeries(t, body, `gascurve_collector_head_block{chain_id="4663",network="robinhood"} 1000`)
	if !strings.Contains(body, "go_goroutines") {
		t.Fatal("the runtime collectors are missing")
	}
	if status, _ := get(t, srv.ListenAddr(), "/"); status != http.StatusNotFound {
		t.Fatalf("root status = %d, want 404", status)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestServerRefusesAPortItCannotBind(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	if _, err := NewServer(context.Background(), taken.Addr().String(), NewRegistry(), logger.Nop()); err == nil {
		t.Fatal("binding a port already in use must fail at startup")
	}
}

func TestServerMountsHealthRoutesBesideMetrics(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	srv, err := NewServer(context.Background(), "127.0.0.1:0", NewRegistry(), nil, mux)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	if status, _ := get(t, srv.ListenAddr(), "/health"); status != http.StatusNoContent {
		t.Fatalf("health status = %d", status)
	}
	if status, _ := get(t, srv.ListenAddr(), Path); status != http.StatusOK {
		t.Fatalf("metrics status = %d", status)
	}
	if status, _ := get(t, srv.ListenAddr(), "/missing"); status != http.StatusNotFound {
		t.Fatalf("missing status = %d", status)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestAddrCoversEveryInterface(t *testing.T) {
	if got := Addr(9090); got != ":9090" {
		t.Fatalf("Addr(9090) = %q", got)
	}
}
