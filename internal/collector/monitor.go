package collector

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/logger"
	"github.com/tirante-dev/gascurve/internal/metrics"
	"github.com/tirante-dev/gascurve/internal/model"
	"github.com/tirante-dev/gascurve/internal/nitro"
)

const (
	loopFast    = "fast"
	loopSlow    = "slow"
	loopHistory = "history"

	defaultHeartbeatInterval = 10 * time.Second
	minimumFastFreshness     = 30 * time.Second
	minimumHistoryFreshness  = 2 * time.Minute
	monitorStatusKey         = "status"
	// readinessErrorStreak is how many consecutive fast-loop failures
	// remove readiness. A public RPC answers the occasional call with 429
	// and the loop recovers on its next tick, so a single failure says
	// nothing about whether the collector is keeping up. A sustained
	// outage still removes readiness here, and would anyway once the loop
	// passes its freshness window.
	readinessErrorStreak = 3
)

type databaseStats interface {
	Stats() db.Stats
}

type loopRuntime struct {
	lastSuccess time.Time
	lastError   time.Time
	err         string
	errorStreak int64
	duration    time.Duration
	staleAfter  time.Duration
}

type networkRuntime struct {
	name         string
	rpc          RPC
	metrics      *metrics.Network
	observedHead uint64
	indexedHead  uint64
	holes        model.HolesStatus
	loops        map[string]*loopRuntime
}

// Monitor records collector progress in memory, persists a heartbeat and
// telemetry checkpoint for the API, and serves process health.
type Monitor struct {
	store    db.Store
	database databaseStats
	metrics  *metrics.Collector
	log      *logger.Logger
	now      func() time.Time
	interval time.Duration
	started  atomic.Bool

	mu        sync.Mutex
	heartbeat time.Time
	networks  map[uint64]*networkRuntime
}

// MonitorOption customizes a Monitor.
type MonitorOption func(*Monitor)

// WithMonitorClock overrides the monitor clock for tests.
func WithMonitorClock(now func() time.Time) MonitorOption {
	return func(m *Monitor) { m.now = now }
}

// WithHeartbeatInterval overrides the durable heartbeat cadence for tests.
func WithHeartbeatInterval(d time.Duration) MonitorOption {
	return func(m *Monitor) {
		if d > 0 {
			m.interval = d
		}
	}
}

// NewMonitor builds the process-wide collector monitor. database may be nil
// for a store without local query accounting, such as a unit-test fake.
func NewMonitor(store db.Store, database databaseStats, instruments *metrics.Collector, log *logger.Logger, opts ...MonitorOption) *Monitor {
	if log == nil {
		log = logger.Nop()
	}
	m := &Monitor{
		store: store, database: database, metrics: instruments, log: log, now: time.Now,
		interval: defaultHeartbeatInterval, networks: map[uint64]*networkRuntime{},
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

func (m *Monitor) register(n config.NetworkConfig, cfg config.CollectorConfig, rpc RPC, instruments *metrics.Network) {
	fastStale := max(3*n.EffectiveTickInterval(cfg), minimumFastFreshness)
	slowStale := max(3*cfg.SlowInterval, minimumFastFreshness)
	historyStale := max(3*cfg.SlowInterval, minimumHistoryFreshness)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.networks[n.ChainID] = &networkRuntime{
		name: n.Name, rpc: rpc, metrics: instruments,
		loops: map[string]*loopRuntime{
			loopFast: {staleAfter: fastStale}, loopSlow: {staleAfter: slowStale}, loopHistory: {staleAfter: historyStale},
		},
	}
}

func (m *Monitor) observeLoop(chainID uint64, loop string, start time.Time, err error) {
	at := m.now()
	m.mu.Lock()
	n := m.networks[chainID]
	if n == nil || n.loops[loop] == nil {
		m.mu.Unlock()
		return
	}
	l := n.loops[loop]
	l.duration = max(at.Sub(start), 0)
	if err != nil {
		l.lastError, l.err = at, err.Error()
		l.errorStreak++
	} else {
		l.lastSuccess, l.errorStreak = at, 0
	}
	instruments, duration := n.metrics, l.duration
	m.mu.Unlock()
	if instruments != nil {
		instruments.ObserveLoop(loop, at, duration, err != nil)
	}
}

func (m *Monitor) observeHead(chainID, observed, indexed uint64) {
	m.mu.Lock()
	n := m.networks[chainID]
	if n != nil {
		n.observedHead = max(n.observedHead, observed)
		n.indexedHead = indexed
		observed, indexed = n.observedHead, n.indexedHead
	}
	m.mu.Unlock()
	if n != nil && n.metrics != nil {
		n.metrics.ObserveProgress(observed, indexed)
	}
}

// Run refreshes and persists collector telemetry until ctx ends. A database
// failure cannot stop this loop or process liveness; readiness reports it.
func (m *Monitor) Run(ctx context.Context) {
	m.started.Store(true)
	m.persist(ctx)
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.persist(ctx)
		}
	}
}

func (m *Monitor) persist(ctx context.Context) {
	at := m.now().UTC()
	m.mu.Lock()
	m.heartbeat = at
	ids := make([]uint64, 0, len(m.networks))
	for id := range m.networks {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	if m.metrics != nil {
		m.metrics.ObserveHeartbeat(at)
		if m.database != nil {
			m.metrics.ObserveDatabase(databaseMetrics(m.database.Stats()))
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		m.refreshHoles(ctx, id, at)
		telemetry := m.telemetry(id)
		body, err := json.Marshal(telemetry)
		if err == nil {
			err = m.store.SetState(ctx, id, db.StateTelemetry, string(body))
		}
		if err != nil && ctx.Err() == nil {
			m.log.Warn("persist collector telemetry", "chainId", id, "err", err.Error())
		}
	}
}

func (m *Monitor) refreshHoles(ctx context.Context, chainID uint64, now time.Time) {
	rows, err := m.store.MissingRanges(ctx, chainID)
	if err != nil {
		return
	}
	holes := make([]model.Hole, 0, len(rows))
	for _, row := range rows {
		holes = append(holes, model.Hole{
			From: row.From, To: row.To, At: row.DetectedAt.UTC().Format(time.RFC3339),
			Lifecycle: row.Lifecycle, Next: row.Cursor, Reason: row.Reason,
		})
	}
	// The legacy checkpoint still holds every range until the fill loop
	// imports it, so freshness stays reported across a rolling upgrade.
	raw, ok, err := m.store.GetState(ctx, chainID, db.StateHoles)
	if err != nil {
		return
	}
	if ok {
		var legacy []model.Hole
		if json.Unmarshal([]byte(raw), &legacy) != nil {
			return
		}
		holes = append(holes, legacy...)
	}
	m.mu.Lock()
	n := m.networks[chainID]
	var summary model.HolesStatus
	if n != nil {
		summary = model.SummarizeHolesAt(holes, now)
		n.holes = summary
	}
	m.mu.Unlock()
	if n != nil && n.metrics != nil {
		age := time.Duration(0)
		if summary.OldestPendingAgeSeconds != nil {
			age = time.Duration(*summary.OldestPendingAgeSeconds) * time.Second
		}
		n.metrics.ObserveHoleFreshness(summary.PendingBlocks, age)
	}
}

func (m *Monitor) telemetry(chainID uint64) model.CollectorTelemetry {
	m.mu.Lock()
	n := m.networks[chainID]
	heartbeat := m.heartbeat
	var out model.CollectorTelemetry
	var rpc RPC
	var instruments *metrics.Network
	if n != nil {
		out.HeartbeatStaleAfterSeconds = int64(3 * m.interval / time.Second)
		out.ObservedHead, out.IndexedHead = n.observedHead, n.indexedHead
		if n.observedHead > n.indexedHead {
			out.HeadLagBlocks = n.observedHead - n.indexedHead
		}
		out.Loops.Fast = loopModel(n.loops[loopFast])
		out.Loops.Slow = loopModel(n.loops[loopSlow])
		out.Loops.History = loopModel(n.loops[loopHistory])
		rpc, instruments = n.rpc, n.metrics
	}
	m.mu.Unlock()
	if rpc != nil {
		st := rpc.Stats()
		out.RPC = rpcModel(st)
		if instruments != nil {
			var status *nitro.PoolStatus
			if pool, ok := rpc.(EndpointPool); ok {
				value := pool.Status()
				status = &value
			}
			instruments.ObservePool(poolMetrics(st, status))
		}
	}
	if !heartbeat.IsZero() {
		at := heartbeat.Format(time.RFC3339Nano)
		out.HeartbeatAt = &at
	}
	if m.database != nil {
		out.Database = databaseModel(m.database.Stats())
	}
	return out
}

func loopModel(l *loopRuntime) model.LoopStatus {
	if l == nil {
		return model.LoopStatus{}
	}
	out := model.LoopStatus{
		LastDurationMS: l.duration.Milliseconds(),
		StaleAfterSecs: int64(l.staleAfter / time.Second),
		ErrorStreak:    l.errorStreak,
	}
	if !l.lastSuccess.IsZero() {
		at := l.lastSuccess.UTC().Format(time.RFC3339Nano)
		out.LastSuccessAt = &at
	}
	if !l.lastError.IsZero() {
		at, msg := l.lastError.UTC().Format(time.RFC3339Nano), l.err
		out.LastErrorAt, out.LastError = &at, &msg
	}
	return out
}

func rpcModel(st nitro.Stats) model.RPCMetrics {
	out := model.RPCMetrics{
		Calls: st.Calls, Requests: st.Requests, Errors: st.Errors,
		CallsLast10Seconds: st.CallsLast10s, RateLimitEvents: st.RateLimitEvents,
	}
	if st.Requests > 0 {
		out.AverageLatencyMS = float64(st.TotalLatency) / float64(time.Millisecond) / float64(st.Requests)
	}
	if !st.Last429At.IsZero() {
		at := st.Last429At.UTC().Format(time.RFC3339)
		out.Last429At = &at
	}
	return out
}

func databaseModel(st db.Stats) model.DatabaseMetrics {
	out := model.DatabaseMetrics{Operations: st.Operations, Errors: st.Errors, LastLatencyMS: float64(st.LastLatency) / float64(time.Millisecond)}
	if st.Operations > 0 {
		out.AverageLatencyMS = float64(st.TotalLatency) / float64(time.Millisecond) / float64(st.Operations)
	}
	return out
}

func databaseMetrics(st db.Stats) metrics.DatabaseState {
	out := metrics.DatabaseState{Operations: st.Operations, Errors: st.Errors, LastTime: st.LastLatency}
	if st.Operations > 0 {
		out.AverageTime = st.TotalLatency / time.Duration(st.Operations)
	}
	return out
}

// Handler serves collector startup, shallow liveness and operational
// readiness. The process observability server mounts it alongside /metrics.
func (m *Monitor) Handler(version string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /startup", func(w http.ResponseWriter, _ *http.Request) {
		if !m.started.Load() {
			monitorJSON(w, http.StatusServiceUnavailable, map[string]string{monitorStatusKey: "starting"})
			return
		}
		monitorJSON(w, http.StatusOK, map[string]string{monitorStatusKey: "started", "version": version})
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		monitorJSON(w, http.StatusOK, map[string]string{monitorStatusKey: "ok", "version": version})
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := m.store.Ping(ctx); err != nil {
			monitorJSON(w, http.StatusServiceUnavailable, map[string]string{monitorStatusKey: "not_ready", "reason": "database unavailable"})
			return
		}
		if reason := m.notReadyReason(); reason != "" {
			monitorJSON(w, http.StatusServiceUnavailable, map[string]string{monitorStatusKey: "not_ready", "reason": reason})
			return
		}
		monitorJSON(w, http.StatusOK, map[string]string{monitorStatusKey: "ready"})
	})
	return mux
}

func (m *Monitor) notReadyReason() string {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.started.Load() {
		return "collector starting"
	}
	for _, n := range m.networks {
		fast := n.loops[loopFast]
		if fast.lastSuccess.IsZero() {
			return "waiting for first successful fast loop for " + n.name
		}
		if fast.errorStreak >= readinessErrorStreak {
			return "fast loop failing for " + n.name
		}
		if now.Sub(fast.lastSuccess) > fast.staleAfter {
			return "fast loop stale for " + n.name
		}
	}
	return ""
}

func monitorJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
