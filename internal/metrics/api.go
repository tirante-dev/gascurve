package metrics

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// API holds the api's instruments.
type API struct {
	reg       prometheus.Registerer
	requests  *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	wsClients prometheus.Gauge
	wsFrames  prometheus.Counter
	wsDropped prometheus.Counter
}

// NewAPI registers the api's instruments on reg.
func NewAPI(reg prometheus.Registerer) *API {
	a := &API{
		reg: reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: subsystemAPI, Name: "requests_total",
			Help: "HTTP requests served, by route pattern, method and status.",
		}, []string{"route", "method", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: subsystemAPI, Name: "request_duration_seconds",
			Help:    "Time to serve one HTTP request, by route pattern and method.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"route", "method"}),
		wsClients: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: Namespace, Subsystem: subsystemAPI, Name: "ws_clients",
			Help: "WebSocket clients currently subscribed.",
		}),
		wsFrames: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: subsystemAPI, Name: "ws_frames_sent_total",
			Help: "WebSocket frames written to clients, pings included.",
		}),
		wsDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: subsystemAPI, Name: "ws_clients_dropped_total",
			Help: "WebSocket clients closed because their outbound queue was full.",
		}),
	}
	reg.MustRegister(a.requests, a.duration, a.wsClients, a.wsFrames, a.wsDropped)
	return a
}

// ObserveListener publishes the PostgreSQL notification listener behind
// status, read at scrape time so nothing has to push it. Without it a wedged
// LISTEN feed is visible only to /status and to whoever notices that /ready
// answers 503, which is also what a failed database ping looks like.
func (a *API) ObserveListener(status func() (ready bool, reconnects uint64)) {
	a.reg.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: Namespace, Subsystem: subsystemAPI, Name: "listener_ready",
			Help: "1 while the PostgreSQL notification listener is connected and subscribed.",
		}, func() float64 {
			ready, _ := status()
			if !ready {
				return 0
			}
			return 1
		}),
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: subsystemAPI, Name: "listener_reconnects_total",
			Help: "Notification listener recoveries since the process started.",
		}, func() float64 {
			_, reconnects := status()
			return float64(reconnects)
		}),
	)
}

// ObserveRequest records one served request. route must be the router's
// own pattern rather than the request path, or every block number in a URL
// becomes a series of its own.
func (a *API) ObserveRequest(route, method string, status int, d time.Duration) {
	a.requests.WithLabelValues(route, method, strconv.Itoa(status)).Inc()
	a.duration.WithLabelValues(route, method).Observe(d.Seconds())
}

// WSConnected and WSDisconnected track subscribed WebSocket clients.
func (a *API) WSConnected()    { a.wsClients.Inc() }
func (a *API) WSDisconnected() { a.wsClients.Dec() }

// WSFrameSent counts one frame written to a client.
func (a *API) WSFrameSent() { a.wsFrames.Inc() }

// WSClientDropped counts a client closed for a full outbound queue.
func (a *API) WSClientDropped() { a.wsDropped.Inc() }
