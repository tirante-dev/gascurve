// Package metrics holds the Prometheus registry both deployables expose and
// the instruments they fill in. Nothing here changes what the collector or
// the api does: every instrument is written from a point the code already
// reaches, and the /metrics handler reads the registry alone, so a scrape
// never waits on a follower's lock or on the database.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Namespace prefixes every series this repository exports.
const Namespace = "gascurve"

// Subsystems, one per deployable.
const (
	subsystemCollector = "collector"
	subsystemAPI       = "api"
)

// Path is where both deployables serve the exposition format.
const Path = "/metrics"

// NewRegistry builds a registry holding the Go runtime and process
// collectors, which are the same for both binaries.
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return reg
}

// Handler serves the exposition format for g. Scrape errors are reported to
// the client rather than logged: the collector has no request log.
func Handler(g prometheus.Gatherer) http.Handler {
	return promhttp.HandlerFor(g, promhttp.HandlerOpts{ErrorHandling: promhttp.HTTPErrorOnError})
}

// monotonic mirrors a cumulative value owned by another component (the RPC
// pool's own counters, say) onto a Prometheus counter, which may only ever
// be added to. It adds the growth since the last observation; a value that
// went backwards means the source started over and is counted from zero
// again rather than being ignored for good.
type monotonic struct {
	c    prometheus.Counter
	last uint64
}

func (m *monotonic) observe(v uint64) {
	if v < m.last {
		m.last = 0
	}
	if v > m.last {
		m.c.Add(float64(v - m.last))
		m.last = v
	}
}

// boolValue is 1 for true and 0 for false, the usual encoding for a state
// gauge.
func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
