package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// API holds the api's instruments.
type API struct {
	requests  *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	wsClients prometheus.Gauge
	wsFrames  prometheus.Counter
	wsDropped prometheus.Counter
}

// NewAPI registers the api's instruments on reg.
func NewAPI(reg prometheus.Registerer) *API {
	a := &API{
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

// otherMethod stands in for any method outside knownMethods.
const otherMethod = "other"

// knownMethods bounds the method label. An HTTP method is an arbitrary
// token, not a closed set: net/http accepts any token and the router
// answers 405, so a caller sending a fresh made-up method per request
// would otherwise mint two series each time and grow the registry without
// end. Only the methods that exist are labeled; the rest share one value.
var knownMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true,
	http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true,
	http.MethodConnect: true, http.MethodOptions: true, http.MethodTrace: true,
}

// ObserveRequest records one served request. route must be the router's
// own pattern rather than the request path, or every block number in a URL
// becomes a series of its own; method is bounded here.
func (a *API) ObserveRequest(route, method string, status int, d time.Duration) {
	if !knownMethods[method] {
		method = otherMethod
	}
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
