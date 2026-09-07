package metrics

import (
	"net/http"
	"testing"
	"time"
)

func TestAPIRecordsRequests(t *testing.T) {
	reg := NewRegistry()
	a := NewAPI(reg)
	a.ObserveRequest("/api/v1/networks/{network}/blocks", http.MethodGet, http.StatusOK, 12*time.Millisecond)
	a.ObserveRequest("/api/v1/networks/{network}/blocks", http.MethodGet, http.StatusOK, 8*time.Millisecond)
	a.ObserveRequest("/api/v1/status", http.MethodGet, http.StatusInternalServerError, time.Second)
	body := scrape(t, reg)
	for _, want := range []string{
		`gascurve_api_requests_total{method="GET",route="/api/v1/networks/{network}/blocks",status="200"} 2`,
		`gascurve_api_requests_total{method="GET",route="/api/v1/status",status="500"} 1`,
		`gascurve_api_request_duration_seconds_count{method="GET",route="/api/v1/networks/{network}/blocks"} 2`,
	} {
		hasSeries(t, body, want)
	}
}

func TestAPIRecordsWebSocketActivity(t *testing.T) {
	reg := NewRegistry()
	a := NewAPI(reg)
	a.WSConnected()
	a.WSConnected()
	a.WSDisconnected()
	a.WSFrameSent()
	a.WSFrameSent()
	a.WSFrameSent()
	a.WSClientDropped()
	body := scrape(t, reg)
	for _, want := range []string{
		"gascurve_api_ws_clients 1",
		"gascurve_api_ws_frames_sent_total 3",
		"gascurve_api_ws_clients_dropped_total 1",
	} {
		hasSeries(t, body, want)
	}
}

func TestAPIObservesListenerAtScrapeTime(t *testing.T) {
	reg := NewRegistry()
	a := NewAPI(reg)
	ready, reconnects := false, uint64(0)
	a.ObserveListener(func() (bool, uint64) { return ready, reconnects })
	body := scrape(t, reg)
	for _, want := range []string{
		"gascurve_api_listener_ready 0",
		"gascurve_api_listener_reconnects_total 0",
	} {
		hasSeries(t, body, want)
	}
	ready, reconnects = true, 3
	body = scrape(t, reg)
	for _, want := range []string{
		"gascurve_api_listener_ready 1",
		"gascurve_api_listener_reconnects_total 3",
	} {
		hasSeries(t, body, want)
	}
}
