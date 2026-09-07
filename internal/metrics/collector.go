package metrics

import (
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Label names. An endpoint is named by its position in the network's
// endpoint list and never by its URL: endpoint URLs carry credentials,
// which is why internal/nitro scrubs them from every error it returns.
const (
	labelNetwork  = "network"
	labelChainID  = "chain_id"
	labelEndpoint = "endpoint"
	labelClass    = "class"
	labelLoop     = "loop"
)

// The values the class label takes, matching nitro's pacer lanes.
const (
	classFast = "fast"
	classBulk = "bulk"
)

// networkLabels are on every collector series; endpointLabels add the
// endpoint's index.
var (
	networkLabels  = []string{labelNetwork, labelChainID}
	endpointLabels = []string{labelNetwork, labelChainID, labelEndpoint}
)

// Collector holds the collector's instruments. One Network is handed to
// each follower; the instruments live here, so a follower that is stuck or
// restarting still exports its last known values.
type Collector struct {
	heartbeat       prometheus.Gauge
	databaseOps     prometheus.Counter
	databaseErrors  prometheus.Counter
	databaseLatency prometheus.Gauge
	databaseLast    prometheus.Gauge

	headBlock       *prometheus.GaugeVec
	headLag         *prometheus.GaugeVec
	observedHead    *prometheus.GaugeVec
	headLagBlocks   *prometheus.GaugeVec
	lastSample      *prometheus.GaugeVec
	tickDuration    *prometheus.HistogramVec
	loopSuccess     *prometheus.GaugeVec
	loopError       *prometheus.GaugeVec
	loopDuration    *prometheus.HistogramVec
	rpcCalls        *prometheus.CounterVec
	rpcRequests     *prometheus.CounterVec
	rpcErrors       *prometheus.CounterVec
	rpcLatency      *prometheus.GaugeVec
	rateLimits      *prometheus.CounterVec
	activeEndpoint  *prometheus.GaugeVec
	failovers       *prometheus.CounterVec
	endpointLimits  *prometheus.CounterVec
	endpointOff     *prometheus.GaugeVec
	endpointCooling *prometheus.GaugeVec
	backfillCursor  *prometheus.GaugeVec
	backfillFloor   *prometheus.GaugeVec
	backfillLeft    *prometheus.GaugeVec
	backfillDone    *prometheus.GaugeVec
	holesPending    *prometheus.GaugeVec
	holesBlocks     *prometheus.GaugeVec
	holesUnfillable *prometheus.GaugeVec
	holesPendingBlk *prometheus.GaugeVec
	holesOldestAge  *prometheus.GaugeVec
	holesFilled     *prometheus.CounterVec
	gapsSkipped     *prometheus.CounterVec

	mu       sync.Mutex
	networks map[uint64]*Network
	dbMu     sync.Mutex
	dbOps    monotonic
	dbErrs   monotonic
}

// NewCollector registers the collector's instruments on reg.
func NewCollector(reg prometheus.Registerer) *Collector {
	c := &Collector{networks: map[uint64]*Network{}}
	gauge := func(name, help string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: Namespace, Subsystem: subsystemCollector, Name: name, Help: help}, networkLabels)
	}
	counter := func(name, help string) *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: Namespace, Subsystem: subsystemCollector, Name: name, Help: help}, networkLabels)
	}
	c.headBlock = gauge("head_block", "Number of the newest block the follower has committed.")
	c.headLag = gauge("head_lag_seconds", "Age of the committed head block, the figure /status reports as lagSeconds.")
	c.observedHead = gauge("observed_head_block", "Number of the newest chain head the fast loop observed.")
	c.headLagBlocks = gauge("head_lag_blocks", "Blocks between the newest observed chain head and the committed head.")
	c.lastSample = gauge("last_sample_timestamp_seconds", "Unix time of the last successful state sample.")
	c.tickDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: Namespace, Subsystem: subsystemCollector, Name: "tick_duration_seconds",
		Help:    "Wall time of one fast tick, whether it succeeded or failed.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60},
	}, networkLabels)
	loopLabels := append(append([]string{}, networkLabels...), labelLoop)
	c.loopSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: Namespace, Subsystem: subsystemCollector, Name: "loop_last_success_timestamp_seconds",
		Help: "Unix time of the last successful collector loop iteration.",
	}, loopLabels)
	c.loopError = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: Namespace, Subsystem: subsystemCollector, Name: "loop_last_error_timestamp_seconds",
		Help: "Unix time of the last failed collector loop iteration.",
	}, loopLabels)
	c.loopDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: Namespace, Subsystem: subsystemCollector, Name: "loop_duration_seconds",
		Help:    "Wall time of a collector loop iteration, whether it succeeded or failed.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 120},
	}, loopLabels)
	c.rpcCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: Namespace, Subsystem: subsystemCollector, Name: "rpc_calls_total",
		Help: "JSON-RPC calls sent, counting every item inside a batch, by pacer class.",
	}, append(append([]string{}, networkLabels...), labelClass))
	c.rpcRequests = counter("rpc_requests_total", "HTTP requests sent to JSON-RPC endpoints, including retries.")
	c.rpcErrors = counter("rpc_errors_total", "Failed JSON-RPC HTTP attempts and item-level JSON-RPC errors.")
	c.rpcLatency = gauge("rpc_latency_seconds", "Average JSON-RPC HTTP round-trip latency since process start, excluding pacer waits.")
	c.rateLimits = counter("rate_limit_events_total", "Times an endpoint of this network reported throttling.")
	c.activeEndpoint = gauge("active_endpoint", "Index of the endpoint ordinary calls currently go to.")
	c.failovers = counter("endpoint_failovers_total", "Times the pool moved this network to another endpoint.")
	c.endpointLimits = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: Namespace, Subsystem: subsystemCollector, Name: "endpoint_rate_limit_events_total",
		Help: "Times one endpoint reported throttling.",
	}, endpointLabels)
	c.endpointOff = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: Namespace, Subsystem: subsystemCollector, Name: "endpoint_disabled",
		Help: "1 while an endpoint is disabled (it failed verification or kept failing).",
	}, endpointLabels)
	c.endpointCooling = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: Namespace, Subsystem: subsystemCollector, Name: "endpoint_ws_cooling",
		Help: "1 while an endpoint's WebSocket is cooled down; its JSON-RPC keeps serving.",
	}, endpointLabels)
	c.backfillCursor = gauge("backfill_cursor_block", "Block the resumable backfill has replayed up to in its current segment.")
	c.backfillFloor = gauge("backfill_floor_block", "Oldest block the backfill will reach, derived from collector.backfill_depth.")
	c.backfillLeft = gauge("backfill_blocks_remaining", "Blocks between the cursor and the depth floor.")
	c.backfillDone = gauge("backfill_done", "1 once the backfill cursor reports the configured depth is rebuilt.")
	c.holesPending = gauge("holes_pending", "Ranges queued for the gap filler.")
	c.holesBlocks = gauge("holes_blocks", "Blocks that are not indexed, across queued and unfillable ranges.")
	c.holesUnfillable = gauge("holes_unfillable", "Ranges nothing can be replayed into.")
	c.holesPendingBlk = gauge("holes_pending_blocks", "Blocks still missing across ranges queued for the gap filler.")
	c.holesOldestAge = gauge("holes_oldest_age_seconds", "Age of the oldest range queued for the gap filler, or zero when none is queued.")
	c.holesFilled = counter("holes_filled_total", "Ranges the gap filler has completed.")
	c.gapsSkipped = counter("catch_up_gaps_skipped_total", "Catch-up gaps skipped because they exceeded the call budget.")
	c.heartbeat = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: Namespace, Subsystem: subsystemCollector, Name: "heartbeat_timestamp_seconds",
		Help: "Unix time of the latest collector monitor heartbeat.",
	})
	c.databaseOps = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: Namespace, Subsystem: subsystemCollector, Name: "database_operations_total",
		Help: "PostgreSQL driver operations.",
	})
	c.databaseErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: Namespace, Subsystem: subsystemCollector, Name: "database_errors_total",
		Help: "Failed PostgreSQL driver operations.",
	})
	c.databaseLatency = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: Namespace, Subsystem: subsystemCollector, Name: "database_latency_seconds",
		Help: "Average PostgreSQL driver operation latency since process start.",
	})
	c.databaseLast = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: Namespace, Subsystem: subsystemCollector, Name: "database_last_latency_seconds",
		Help: "Latency of the latest PostgreSQL driver operation.",
	})
	c.dbOps.c, c.dbErrs.c = c.databaseOps, c.databaseErrors
	reg.MustRegister(
		c.heartbeat, c.databaseOps, c.databaseErrors, c.databaseLatency, c.databaseLast,
		c.headBlock, c.headLag, c.observedHead, c.headLagBlocks, c.lastSample, c.tickDuration,
		c.loopSuccess, c.loopError, c.loopDuration, c.rpcCalls, c.rpcRequests, c.rpcErrors, c.rpcLatency, c.rateLimits,
		c.activeEndpoint, c.failovers, c.endpointLimits, c.endpointOff, c.endpointCooling,
		c.backfillCursor, c.backfillFloor, c.backfillLeft, c.backfillDone, c.holesPending,
		c.holesBlocks, c.holesUnfillable, c.holesPendingBlk, c.holesOldestAge, c.holesFilled, c.gapsSkipped,
	)
	return c
}

// Network returns the instruments for one network, creating them on the
// first call. Followers are restarted in place, so the same chain id always
// gets the same series.
func (c *Collector) Network(name string, chainID uint64) *Network {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n, ok := c.networks[chainID]; ok {
		return n
	}
	id := strconv.FormatUint(chainID, 10)
	labels := prometheus.Labels{labelNetwork: name, labelChainID: id}
	n := &Network{
		parent: c, name: name, chainID: id,
		headBlock:       c.headBlock.With(labels),
		headLag:         c.headLag.With(labels),
		observedHead:    c.observedHead.With(labels),
		headLagBlocks:   c.headLagBlocks.With(labels),
		lastSample:      c.lastSample.With(labels),
		tickDuration:    c.tickDuration.With(labels),
		fastCalls:       monotonic{c: c.rpcCalls.WithLabelValues(name, id, classFast)},
		bulkCalls:       monotonic{c: c.rpcCalls.WithLabelValues(name, id, classBulk)},
		rpcRequests:     monotonic{c: c.rpcRequests.With(labels)},
		rpcErrors:       monotonic{c: c.rpcErrors.With(labels)},
		rpcLatency:      c.rpcLatency.With(labels),
		rateLimits:      monotonic{c: c.rateLimits.With(labels)},
		activeEndpoint:  c.activeEndpoint.With(labels),
		failovers:       monotonic{c: c.failovers.With(labels)},
		backfillCursor:  c.backfillCursor.With(labels),
		backfillFloor:   c.backfillFloor.With(labels),
		backfillLeft:    c.backfillLeft.With(labels),
		backfillDone:    c.backfillDone.With(labels),
		holesPending:    c.holesPending.With(labels),
		holesBlocks:     c.holesBlocks.With(labels),
		holesUnfillable: c.holesUnfillable.With(labels),
		holesPendingBlk: c.holesPendingBlk.With(labels),
		holesOldestAge:  c.holesOldestAge.With(labels),
		holesFilled:     c.holesFilled.With(labels),
		gapsSkipped:     c.gapsSkipped.With(labels),
		endpoints:       map[int]*endpoint{},
		loops:           map[string]loopInstruments{},
	}
	for _, loop := range []string{"fast", "slow", "history"} {
		n.loops[loop] = loopInstruments{
			success:  c.loopSuccess.WithLabelValues(name, id, loop),
			error:    c.loopError.WithLabelValues(name, id, loop),
			duration: c.loopDuration.WithLabelValues(name, id, loop),
		}
	}
	c.networks[chainID] = n
	return n
}

// ObserveHeartbeat records that the collector monitor is running.
func (c *Collector) ObserveHeartbeat(at time.Time) {
	c.heartbeat.Set(float64(at.Unix()))
}

// DatabaseState is cumulative PostgreSQL operation accounting.
type DatabaseState struct {
	Operations  uint64
	Errors      uint64
	AverageTime time.Duration
	LastTime    time.Duration
}

// ObserveDatabase mirrors the collector's process-wide database accounting.
func (c *Collector) ObserveDatabase(d DatabaseState) {
	c.dbMu.Lock()
	defer c.dbMu.Unlock()
	c.dbOps.observe(d.Operations)
	c.dbErrs.observe(d.Errors)
	c.databaseLatency.Set(d.AverageTime.Seconds())
	c.databaseLast.Set(d.LastTime.Seconds())
}

// endpoint holds one endpoint's series.
type endpoint struct {
	limits  monotonic
	off     prometheus.Gauge
	cooling prometheus.Gauge
}

type loopInstruments struct {
	success  prometheus.Gauge
	error    prometheus.Gauge
	duration prometheus.Observer
}

// Network is one follower's view of the collector instruments. Every method
// is safe for concurrent use and none of them can block on the follower.
type Network struct {
	parent  *Collector
	name    string
	chainID string

	headBlock       prometheus.Gauge
	headLag         prometheus.Gauge
	observedHead    prometheus.Gauge
	headLagBlocks   prometheus.Gauge
	lastSample      prometheus.Gauge
	tickDuration    prometheus.Observer
	activeEndpoint  prometheus.Gauge
	backfillCursor  prometheus.Gauge
	backfillFloor   prometheus.Gauge
	backfillLeft    prometheus.Gauge
	backfillDone    prometheus.Gauge
	holesPending    prometheus.Gauge
	holesBlocks     prometheus.Gauge
	holesUnfillable prometheus.Gauge
	holesPendingBlk prometheus.Gauge
	holesOldestAge  prometheus.Gauge
	holesFilled     prometheus.Counter
	gapsSkipped     prometheus.Counter

	mu          sync.Mutex
	fastCalls   monotonic
	bulkCalls   monotonic
	rpcRequests monotonic
	rpcErrors   monotonic
	rpcLatency  prometheus.Gauge
	rateLimits  monotonic
	failovers   monotonic
	endpoints   map[int]*endpoint
	loops       map[string]loopInstruments
}

// ObserveHead records a committed head: its number, the age of the block it
// names and when the sample that carried it was taken.
func (n *Network) ObserveHead(block uint64, blockAt, sampledAt, now time.Time) {
	n.headBlock.Set(float64(block))
	n.headLag.Set(max(now.Sub(blockAt).Seconds(), 0))
	n.lastSample.Set(float64(sampledAt.Unix()))
}

// ObserveTick records how long one fast tick took.
func (n *Network) ObserveTick(d time.Duration) { n.tickDuration.Observe(d.Seconds()) }

// ObserveProgress records the newest observed head and its distance from
// the newest committed head.
func (n *Network) ObserveProgress(observed, indexed uint64) {
	n.observedHead.Set(float64(observed))
	lag := uint64(0)
	if observed > indexed {
		lag = observed - indexed
	}
	n.headLagBlocks.Set(float64(lag))
}

// ObserveLoop records one loop outcome. Error text stays out of labels to
// avoid an unbounded series count; the durable status checkpoint carries it.
func (n *Network) ObserveLoop(loop string, at time.Time, d time.Duration, failed bool) {
	instruments, ok := n.loops[loop]
	if !ok {
		return
	}
	instruments.duration.Observe(d.Seconds())
	if failed {
		instruments.error.Set(float64(at.Unix()))
		return
	}
	instruments.success.Set(float64(at.Unix()))
}

// GapSkipped counts a catch-up gap the follower skipped rather than fetched.
func (n *Network) GapSkipped() { n.gapsSkipped.Inc() }

// HoleFilled counts a queued range the gap filler completed.
func (n *Network) HoleFilled() { n.holesFilled.Inc() }

// HolesState summarizes the ranges that are not indexed, exactly as
// /status reports them.
type HolesState struct {
	Pending    int
	Blocks     uint64
	Unfillable int
}

// ObserveHoles records the gap filler's queue.
func (n *Network) ObserveHoles(h HolesState) {
	n.holesPending.Set(float64(h.Pending))
	n.holesBlocks.Set(float64(h.Blocks))
	n.holesUnfillable.Set(float64(h.Unfillable))
}

// ObserveHoleFreshness records queued work only. Unfillable ranges remain in
// the compatibility holes metrics but have no pending age or block count.
func (n *Network) ObserveHoleFreshness(blocks uint64, oldestAge time.Duration) {
	n.holesPendingBlk.Set(float64(blocks))
	n.holesOldestAge.Set(max(oldestAge.Seconds(), 0))
}

// BackfillState is where the resumable backfill has got to.
type BackfillState struct {
	// Cursor is the block the current segment has replayed up to.
	Cursor uint64
	// Floor is the oldest block the configured depth reaches.
	Floor uint64
	// Remaining is how many blocks lie between them, over every segment
	// still to come.
	Remaining uint64
	Done      bool
}

// ObserveBackfill records the backfill cursor.
func (n *Network) ObserveBackfill(b BackfillState) {
	n.backfillCursor.Set(float64(b.Cursor))
	n.backfillFloor.Set(float64(b.Floor))
	n.backfillLeft.Set(float64(b.Remaining))
	n.backfillDone.Set(boolValue(b.Done))
}

// EndpointState is one endpoint's observable state. It is identified by its
// index in the network's endpoint list: a URL is a credential and never
// becomes a label value.
type EndpointState struct {
	Index           int
	Disabled        bool
	WSCooling       bool
	RateLimitEvents uint64
}

// PoolState is the RPC pool's routing state together with the cumulative
// counters its endpoints keep.
type PoolState struct {
	Active          int
	Failovers       uint64
	RateLimitEvents uint64
	FastCalls       uint64
	BulkCalls       uint64
	Requests        uint64
	Errors          uint64
	TotalLatency    time.Duration
	Endpoints       []EndpointState
}

// ObservePool records the routing state and the pool's cumulative counters.
// The counters are mirrored as growth since the previous call, so a pool
// that starts over is counted from zero rather than ignored.
func (n *Network) ObservePool(p PoolState) {
	n.activeEndpoint.Set(float64(p.Active))
	n.mu.Lock()
	defer n.mu.Unlock()
	n.failovers.observe(p.Failovers)
	n.rateLimits.observe(p.RateLimitEvents)
	n.fastCalls.observe(p.FastCalls)
	n.bulkCalls.observe(p.BulkCalls)
	n.rpcRequests.observe(p.Requests)
	n.rpcErrors.observe(p.Errors)
	if p.Requests > 0 {
		n.rpcLatency.Set(p.TotalLatency.Seconds() / float64(p.Requests))
	}
	for _, e := range p.Endpoints {
		ep := n.endpointLocked(e.Index)
		ep.limits.observe(e.RateLimitEvents)
		ep.off.Set(boolValue(e.Disabled))
		ep.cooling.Set(boolValue(e.WSCooling))
	}
}

// endpointLocked returns one endpoint's series, creating them on first use.
func (n *Network) endpointLocked(index int) *endpoint {
	if ep, ok := n.endpoints[index]; ok {
		return ep
	}
	idx := strconv.Itoa(index)
	ep := &endpoint{
		limits:  monotonic{c: n.parent.endpointLimits.WithLabelValues(n.name, n.chainID, idx)},
		off:     n.parent.endpointOff.WithLabelValues(n.name, n.chainID, idx),
		cooling: n.parent.endpointCooling.WithLabelValues(n.name, n.chainID, idx),
	}
	n.endpoints[index] = ep
	return ep
}
