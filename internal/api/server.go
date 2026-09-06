// Package api serves the REST endpoints and the WebSocket described in
// docs/ARCHITECTURE.md sections 6 and 7. It reads Postgres only.
package api

import (
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
	maxRateEntries   = 10_000
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
	}
	s.router = s.routes()
	return s
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler { return s.router }

func (s *Server) routes() chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(realIP)
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

// realIP replaces RemoteAddr with the client address a reverse proxy
// forwarded, but only when the direct peer is a loopback or private address
// (that is, the proxy itself). Requests arriving straight from the internet
// cannot spoof their address through the headers.
func realIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
				if fwd := forwardedFor(r); fwd != "" {
					r.RemoteAddr = net.JoinHostPort(fwd, "0")
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// forwardedFor returns the first X-Forwarded-For entry or X-Real-IP.
func forwardedFor(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		if ip := net.ParseIP(strings.TrimSpace(first)); ip != nil {
			return ip.String()
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

// rateLimiter keeps a token bucket per client IP.
type rateLimiter struct {
	mu      sync.Mutex
	limit   rate.Limit
	burst   int
	clients map[string]*rateEntry
	now     func() time.Time
}

type rateEntry struct {
	limiter *rate.Limiter
	seen    time.Time
}

func newRateLimiter(perSecond float64, burst int) *rateLimiter {
	return &rateLimiter{limit: rate.Limit(perSecond), burst: burst, clients: map[string]*rateEntry{}, now: time.Now}
}

func (rl *rateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := rl.now()
	e, ok := rl.clients[ip]
	if !ok {
		if len(rl.clients) >= maxRateEntries {
			for k, v := range rl.clients {
				if now.Sub(v.seen) > 10*time.Minute {
					delete(rl.clients, k)
				}
			}
		}
		e = &rateEntry{limiter: rate.NewLimiter(rl.limit, rl.burst)}
		rl.clients[ip] = e
	}
	e.seen = now
	return e.limiter.AllowN(now, 1)
}

func (rl *rateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := r.RemoteAddr
		if host, _, ok := strings.Cut(ip, ":"); ok && !strings.Contains(ip, "]") {
			ip = host
		} else if i := strings.LastIndex(ip, "]:"); i > 0 {
			ip = ip[:i+1]
		}
		if !rl.allow(ip) {
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
