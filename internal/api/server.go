// Package api serves the REST endpoints and the WebSocket described in
// docs/ARCHITECTURE.md sections 6 and 7. It reads Postgres only.
package api

import (
	"container/list"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"golang.org/x/time/rate"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/logger"
	"github.com/tirante-dev/gascurve/internal/model"
)

const (
	requestTimeout   = 30 * time.Second
	defaultRateLimit = 20.0
	defaultRateBurst = 60
	// maxRateEntries is the hard cap on tracked client addresses; beyond
	// it the least recently seen address is evicted.
	maxRateEntries = 10_000
	// rateEntryTTL is how long an idle address keeps its bucket.
	rateEntryTTL = 10 * time.Minute
	// rateSweepEvery bounds how often idle entries are expired.
	rateSweepEvery = time.Minute
)

// Server holds the handlers' dependencies.
type Server struct {
	store   db.Store
	cfg     config.ServerConfig
	log     *logger.Logger
	hub     *Hub
	version string
	now     func() time.Time
	router  chi.Router
}

// Option customizes a Server.
type Option func(*Server)

// WithClock overrides the clock (tests).
func WithClock(now func() time.Time) Option { return func(s *Server) { s.now = now } }

// WithVersion sets the version reported by /status.
func WithVersion(v string) Option { return func(s *Server) { s.version = v } }

// New builds the server and its router. hub may be nil when the WebSocket
// is not wanted.
func New(store db.Store, cfg config.ServerConfig, hub *Hub, log *logger.Logger, opts ...Option) *Server {
	if log == nil {
		log = logger.Nop()
	}
	s := &Server{store: store, cfg: cfg, log: log, hub: hub, now: time.Now, version: "dev"}
	for _, o := range opts {
		o(s)
	}
	if s.hub != nil {
		s.hub.live = s.buildLive
		s.hub.network = s.networkModel
		s.hub.setLimits(cfg.WSMaxPerIP, cfg.WSMaxTotal)
	}
	s.router = s.routes()
	return s
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler { return s.router }

func (s *Server) routes() chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	trusted, err := s.cfg.TrustedProxyNets()
	if err != nil {
		s.log.Warn("ignoring server.trusted_proxies", "err", err.Error())
		trusted = nil
	}
	r.Use(realIP(trusted))
	r.Use(requestLogger(s.log))
	r.Use(middleware.Recoverer)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   s.cfg.CORSOrigins,
		AllowedMethods:   []string{http.MethodGet, http.MethodOptions},
		AllowedHeaders:   []string{"Accept", "Content-Type", "X-Request-ID"},
		ExposedHeaders:   []string{"X-Request-ID"},
		AllowCredentials: false,
		MaxAge:           600,
	}))
	limit, burst := s.cfg.RateLimitPerSecond, s.cfg.RateLimitBurst
	if limit <= 0 {
		limit = defaultRateLimit
	}
	if burst <= 0 {
		burst = defaultRateBurst
	}
	r.Use(newRateLimiter(limit, burst).middleware)
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "route not found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	})

	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(requestTimeout))
		r.Get("/health", s.handleHealth)
		r.Get("/ready", s.handleReady)
		r.Route("/api/v1", func(r chi.Router) {
			r.Get("/health", s.handleHealth)
			r.Get("/ready", s.handleReady)
			r.Get("/status", s.handleStatus)
			r.Get("/networks", s.handleNetworks)
			r.Route("/networks/{network}", func(r chi.Router) {
				r.Get("/", s.handleNetwork)
				r.Get("/live", s.handleLive)
				r.Get("/blocks", s.handleBlocks)
				r.Get("/series", s.handleSeries)
				r.Get("/constraints", s.handleConstraints)
				r.Get("/owner-actions", s.handleOwnerActions)
				r.Get("/batches", s.handleBatches)
				r.Get("/l1", s.handleL1)
			})
		})
	})
	if s.hub != nil {
		r.Get("/api/v1/ws", s.hub.ServeWS)
	}
	return r
}

// writeJSON writes v with the given status and Cache-Control.
func writeJSON(w http.ResponseWriter, status int, cacheControl string, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if cacheControl != "" {
		w.Header().Set("Cache-Control", cacheControl)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes the error envelope.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, "no-store", model.ErrorBody{Error: model.ErrorDetail{Code: code, Message: msg}})
}

// realIP replaces RemoteAddr with the client address the configured
// reverse proxies forwarded. Forwarding headers are believed only when the
// direct peer is a trusted proxy; the X-Forwarded-For chain is then walked
// from the right, past every trusted hop, to the first address a trusted
// proxy did not vouch for. A private peer address proves nothing by
// itself, and with no trusted proxies configured the peer is the client.
func realIP(trusted []*net.IPNet) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(trusted) > 0 {
				if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
					if peer := net.ParseIP(host); peer != nil && inNets(peer, trusted) {
						if client := forwardedClient(r, trusted); client != "" {
							r.RemoteAddr = net.JoinHostPort(client, "0")
						}
					}
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func inNets(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// forwardedClient walks X-Forwarded-For right to left and returns the first
// hop that is not a trusted proxy (or the leftmost hop when every hop is
// trusted), falling back to X-Real-IP. Empty when nothing usable is there.
func forwardedClient(r *http.Request, trusted []*net.IPNet) string {
	var hops []net.IP
	for _, xff := range r.Header.Values("X-Forwarded-For") {
		for _, part := range strings.Split(xff, ",") {
			if ip := net.ParseIP(strings.TrimSpace(part)); ip != nil {
				hops = append(hops, ip)
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		if i == 0 || !inNets(hops[i], trusted) {
			return hops[i].String()
		}
	}
	if ip := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); ip != nil {
		return ip.String()
	}
	return ""
}

// requestLogger logs one line per request.
func requestLogger(log *logger.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			if id := middleware.GetReqID(r.Context()); id != "" {
				w.Header().Set("X-Request-Id", id)
			}
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			log.Info("request",
				"method", r.Method, "path", r.URL.Path, "status", ww.Status(), "bytes", ww.BytesWritten(),
				"durationMs", time.Since(start).Milliseconds(), "ip", r.RemoteAddr, "requestId", middleware.GetReqID(r.Context()))
		})
	}
}

// rateLimiter keeps a token bucket per client IP in a bounded LRU: idle
// entries expire after rateEntryTTL (swept at most every rateSweepEvery,
// on any request, not only when a new address shows up) and the least
// recently seen address is evicted when the hard cap is reached, so
// memory and per-request work stay bounded whatever the addresses do.
type rateLimiter struct {
	mu        sync.Mutex
	limit     rate.Limit
	burst     int
	maxEntry  int
	ttl       time.Duration
	clients   map[string]*list.Element
	order     *list.List // most recently seen at the front
	lastSweep time.Time
	now       func() time.Time
}

type rateEntry struct {
	ip      string
	limiter *rate.Limiter
	seen    time.Time
}

func newRateLimiter(perSecond float64, burst int) *rateLimiter {
	return &rateLimiter{limit: rate.Limit(perSecond), burst: burst, maxEntry: maxRateEntries, ttl: rateEntryTTL, clients: map[string]*list.Element{}, order: list.New(), now: time.Now}
}

func (rl *rateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := rl.now()
	if now.Sub(rl.lastSweep) >= rateSweepEvery {
		rl.lastSweep = now
		rl.sweepLocked(now)
	}
	el, ok := rl.clients[ip]
	if !ok {
		for rl.order.Len() >= rl.maxEntry {
			rl.removeLocked(rl.order.Back())
		}
		el = rl.order.PushFront(&rateEntry{ip: ip, limiter: rate.NewLimiter(rl.limit, rl.burst)})
		rl.clients[ip] = el
	} else {
		rl.order.MoveToFront(el)
	}
	e := entryOf(el)
	e.seen = now
	return e.limiter.AllowN(now, 1)
}

func entryOf(el *list.Element) *rateEntry {
	e, _ := el.Value.(*rateEntry)
	return e
}

// sweepLocked drops entries idle for longer than the TTL, walking from the
// least recently seen end and stopping at the first live one.
func (rl *rateLimiter) sweepLocked(now time.Time) {
	for el := rl.order.Back(); el != nil; {
		e := entryOf(el)
		if now.Sub(e.seen) <= rl.ttl {
			return
		}
		prev := el.Prev()
		rl.removeLocked(el)
		el = prev
	}
}

func (rl *rateLimiter) removeLocked(el *list.Element) {
	delete(rl.clients, entryOf(el).ip)
	rl.order.Remove(el)
}

// size returns the number of tracked addresses.
func (rl *rateLimiter) size() int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.order.Len()
}

// clientIP strips the port from a remote address.
func clientIP(addr string) string {
	if host, _, ok := strings.Cut(addr, ":"); ok && !strings.Contains(addr, "]") {
		return host
	} else if i := strings.LastIndex(addr, "]:"); i > 0 {
		return addr[:i+1]
	}
	return addr
}

func (rl *rateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow(clientIP(r.RemoteAddr)) {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originPatterns derives WebSocket origin host patterns from CORS origins.
func originPatterns(origins []string) []string {
	var out []string
	for _, o := range origins {
		if o == "*" {
			return []string{"*"}
		}
		u, err := url.Parse(o)
		if err != nil || u.Host == "" {
			out = append(out, o)
			continue
		}
		out = append(out, u.Host)
	}
	return out
}
