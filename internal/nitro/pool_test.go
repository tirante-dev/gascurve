package nitro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/logger"
)

const (
	testChainID    = 4663
	testChainIDHex = "0x1237"
)

// chainFake is a fakeRPC that answers eth_chainId with id and echoes.
func chainFake(t *testing.T, id string) *fakeRPC {
	t.Helper()
	f := newFakeRPC(t)
	f.handlers["eth_chainId"] = func([]json.RawMessage) any { return id }
	f.handlers["echo"] = echoHandler
	return f
}

func (f *fakeRPC) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func (f *fakeRPC) fail(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script = append(f.script, scriptStep{status: status, body: "down"})
}

func endpointOf(f *fakeRPC, cps float64) config.EndpointConfig {
	return config.EndpointConfig{RPCURL: f.server.URL, CallsPerSecond: cps}
}

func newTestPool(t *testing.T, clock *fakeClock, cooldown time.Duration, eps ...config.EndpointConfig) *Pool {
	t.Helper()
	return NewPool(PoolConfig{ChainID: testChainID, Endpoints: eps, BatchSize: 100, Cooldown: cooldown},
		WithPoolClientOptions(WithHTTPClient(&http.Client{}), WithMaxAttempts(2)), withPoolClock(clock.Now, clock.Sleep))
}

func echoVia(t *testing.T, p *Pool, want string) {
	t.Helper()
	raw, err := p.Call(context.Background(), "echo", want)
	if err != nil || string(raw) != fmt.Sprintf("%q", want) {
		t.Fatalf("echo: %s %v", raw, err)
	}
}

// TestPoolFailoverAndRecovery: a failed request moves ordinary calls to the
// fallback for the cooldown, after which the primary is probed with one
// eth_chainId and taken back only when it answers; failures on the
// fallback wrap around, and answers from the node never fail over.
func TestPoolFailoverAndRecovery(t *testing.T) {
	ctx := context.Background()
	a, b := chainFake(t, testChainIDHex), chainFake(t, testChainIDHex)
	clock := newFakeClock()
	p := newTestPool(t, clock, 30*time.Second, endpointOf(a, 100), endpointOf(b, 100))
	if err := p.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	if a.requestCount() != 1 || b.requestCount() != 1 || !p.Endpoints()[0].Verified() || !p.Endpoints()[1].Verified() {
		t.Fatalf("verify: %d %d", a.requestCount(), b.requestCount())
	}
	echoVia(t, p, "one")
	if a.requestCount() != 2 || b.requestCount() != 1 || p.ActiveEndpoint() != 0 {
		t.Fatalf("ordinary call must go to the primary: %d %d", a.requestCount(), b.requestCount())
	}
	// The primary answers 500: the call succeeds on the fallback.
	a.fail(http.StatusInternalServerError)
	echoVia(t, p, "two")
	if a.requestCount() != 3 || b.requestCount() != 2 || p.ActiveEndpoint() != 1 || p.Failovers() != 1 {
		t.Fatalf("failover: a=%d b=%d active=%d failovers=%d", a.requestCount(), b.requestCount(), p.ActiveEndpoint(), p.Failovers())
	}
	// Inside the cooldown the primary is left alone.
	clock.Advance(29 * time.Second)
	echoVia(t, p, "three")
	if a.requestCount() != 3 || b.requestCount() != 3 {
		t.Fatalf("cooldown: a=%d b=%d", a.requestCount(), b.requestCount())
	}
	// After the cooldown the primary is probed; a failed probe extends the
	// stay on the fallback by another cooldown.
	clock.Advance(time.Second)
	a.fail(http.StatusBadGateway)
	echoVia(t, p, "four")
	if a.requestCount() != 4 || b.requestCount() != 4 || p.ActiveEndpoint() != 1 {
		t.Fatalf("failed probe: a=%d b=%d active=%d", a.requestCount(), b.requestCount(), p.ActiveEndpoint())
	}
	clock.Advance(29 * time.Second)
	echoVia(t, p, "five")
	if a.requestCount() != 4 || b.requestCount() != 5 {
		t.Fatalf("extended cooldown: a=%d b=%d", a.requestCount(), b.requestCount())
	}
	// A successful probe (eth_chainId on the primary) returns to it.
	clock.Advance(time.Second)
	echoVia(t, p, "six")
	if a.requestCount() != 6 || b.requestCount() != 5 || p.ActiveEndpoint() != 0 || p.Failovers() != 1 {
		t.Fatalf("recovery: a=%d b=%d active=%d failovers=%d", a.requestCount(), b.requestCount(), p.ActiveEndpoint(), p.Failovers())
	}
	// A transport error on the fallback wraps around to the primary.
	a.fail(http.StatusInternalServerError)
	echoVia(t, p, "seven")
	if p.ActiveEndpoint() != 1 || p.Failovers() != 2 {
		t.Fatalf("second failover: active=%d failovers=%d", p.ActiveEndpoint(), p.Failovers())
	}
	b.fail(http.StatusServiceUnavailable)
	echoVia(t, p, "eight")
	if p.ActiveEndpoint() != 0 || p.Failovers() != 3 {
		t.Fatalf("wrap around: active=%d failovers=%d", p.ActiveEndpoint(), p.Failovers())
	}
	// Both down: the error is returned once every endpoint was tried.
	a.fail(http.StatusInternalServerError)
	b.fail(http.StatusInternalServerError)
	if _, err := p.Call(ctx, "echo", "nine"); !IsEndpointError(err) || p.Failovers() != 4 || p.ActiveEndpoint() != 1 {
		t.Fatalf("both down: %v active=%d failovers=%d", err, p.ActiveEndpoint(), p.Failovers())
	}
	// The node's own errors are answers, not endpoint failures.
	var rpcErr *RPCError
	if _, err := p.Call(ctx, "nope"); !errors.As(err, &rpcErr) || IsEndpointError(err) || p.Failovers() != 4 {
		t.Fatalf("rpc error must not fail over: %v", err)
	}
	// Throttling past the back-off is an endpoint failure (attempts: 2).
	clock.Advance(time.Minute)
	echoVia(t, p, "ten") // probe and return to the primary
	a.fail(http.StatusTooManyRequests)
	a.fail(http.StatusTooManyRequests)
	echoVia(t, p, "eleven")
	if p.ActiveEndpoint() != 1 || p.Failovers() != 5 {
		t.Fatalf("429 past back-off: active=%d failovers=%d", p.ActiveEndpoint(), p.Failovers())
	}
	st := p.Stats()
	if st.RateLimitEvents != 2 || st.Last429At.IsZero() || st.CallsLast10s == 0 || st.Backoff != minBackoff {
		t.Fatalf("aggregated stats: %+v", st)
	}
	if p.Available() <= 0 {
		t.Fatal("available")
	}
	// A canceled context is returned as is, without failing over.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := p.Call(canceled, "echo", "x"); !errors.Is(err, context.Canceled) || p.Failovers() != 5 {
		t.Fatalf("canceled: %v failovers=%d", err, p.Failovers())
	}
	if len(p.Status().Endpoints) != 2 || p.Status().Active != 1 || p.Status().Failovers != 5 {
		t.Fatalf("status: %+v", p.Status())
	}
}

// TestPoolVerification: a mismatching endpoint is disabled and skipped by
// routing, an unreachable one stays unverified until it answers, and a
// pool without a usable endpoint refuses to run.
func TestPoolVerification(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	good, wrong, flaky := chainFake(t, testChainIDHex), chainFake(t, "0x1"), chainFake(t, testChainIDHex)
	flaky.fail(http.StatusInternalServerError)
	p := newTestPool(t, clock, 30*time.Second,
		endpointOf(good, 100),
		config.EndpointConfig{RPCURL: wrong.server.URL, WSURL: "wss://wrong", Archive: true, CallsPerSecond: 100},
		config.EndpointConfig{RPCURL: flaky.server.URL, Archive: true, CallsPerSecond: 100},
	)
	if err := p.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	st := p.Status()
	if st.Active != 0 || st.Failovers != 0 || !st.Endpoints[1].Disabled || st.Endpoints[2].Disabled || !st.Endpoints[1].WS || !st.Endpoints[1].Archive || st.Endpoints[2].WS {
		t.Fatalf("status after verify: %+v", st)
	}
	if p.Endpoints()[2].Verified() || p.Endpoints()[2].Disabled() {
		t.Fatal("an unreachable endpoint stays unverified, not disabled")
	}
	// Capabilities skip the disabled endpoint, and only a verified one is
	// ever bound: the WebSocket endpoint here reports the wrong chain.
	if p.WS() != nil || !p.HasWS() {
		t.Fatalf("capabilities: ws=%v hasWS=%v", p.WS(), p.HasWS())
	}
	if _, err := p.WSURL(ctx); !errors.Is(err, ErrNoEndpoint) {
		t.Fatalf("a disabled WebSocket endpoint is never dialed: %v", err)
	}
	if ar := p.Archive(); ar == nil || ar.Endpoint() == nil || ar.Endpoint().Index() != 2 || !ar.Endpoint().Archive() {
		t.Fatalf("archive: %v", p.Archive())
	}
	// A failure on the primary skips the disabled endpoint and verifies the
	// flaky one before using it; when that fails too the primary is retried.
	good.fail(http.StatusInternalServerError)
	flaky.fail(http.StatusInternalServerError)
	echoVia(t, p, "one")
	if p.ActiveEndpoint() != 0 || p.Failovers() != 2 || wrong.requestCount() != 1 {
		t.Fatalf("skip disabled: active=%d failovers=%d wrong=%d", p.ActiveEndpoint(), p.Failovers(), wrong.requestCount())
	}
	// Now the flaky endpoint answers: it is verified on first use.
	good.fail(http.StatusInternalServerError)
	echoVia(t, p, "two")
	if p.ActiveEndpoint() != 2 || !p.Endpoints()[2].Verified() {
		t.Fatalf("lazy verification: active=%d verified=%v", p.ActiveEndpoint(), p.Endpoints()[2].Verified())
	}
	// A primary that starts reporting another chain at the probe is
	// disabled and never probed again.
	clock.Advance(time.Minute)
	good.handlers["eth_chainId"] = func([]json.RawMessage) any { return "0x1" }
	echoVia(t, p, "three")
	if p.ActiveEndpoint() != 2 || !p.Endpoints()[0].Disabled() {
		t.Fatalf("probe mismatch: active=%d disabled=%v", p.ActiveEndpoint(), p.Endpoints()[0].Disabled())
	}
	clock.Advance(time.Minute)
	before := good.requestCount()
	echoVia(t, p, "four")
	if good.requestCount() != before {
		t.Fatal("a disabled primary must not be probed")
	}
	// The last usable endpoint failing leaves nothing to fail over to.
	flaky.fail(http.StatusInternalServerError)
	if _, err := p.Call(ctx, "echo", "five"); !IsEndpointError(err) || p.Failovers() != 3 {
		t.Fatalf("no alternative: %v failovers=%d", err, p.Failovers())
	}

	// A mismatching primary with a good fallback starts on the fallback.
	wrong2, good2 := chainFake(t, "0x2"), chainFake(t, testChainIDHex)
	p2 := newTestPool(t, clock, time.Minute, endpointOf(wrong2, 100), endpointOf(good2, 100))
	if err := p2.Verify(ctx); err != nil || p2.ActiveEndpoint() != 1 || p2.Failovers() != 1 {
		t.Fatalf("primary mismatch: %v active=%d failovers=%d", err, p2.ActiveEndpoint(), p2.Failovers())
	}
	echoVia(t, p2, "x")
	if wrong2.requestCount() != 1 {
		t.Fatal("a disabled primary must not receive calls")
	}
	// Verify is idempotent and the fallback stays disabled-free.
	if err := p2.Verify(ctx); err != nil || p2.Failovers() != 1 {
		t.Fatalf("second verify: %v", err)
	}

	// Nothing usable: mismatch plus unreachable.
	dead := chainFake(t, testChainIDHex)
	dead.server.Close()
	p3 := newTestPool(t, clock, time.Minute, endpointOf(wrong2, 100), endpointOf(dead, 100))
	if err := p3.Verify(ctx); !errors.Is(err, ErrNoEndpoint) {
		t.Fatalf("expected ErrNoEndpoint, got %v", err)
	}
	if _, err := p3.Call(ctx, "echo"); !IsEndpointError(err) {
		t.Fatalf("unreachable fallback: %v", err)
	}
	if err := p3.Verify(ctx); !errors.Is(err, ErrNoEndpoint) {
		t.Fatalf("expected ErrNoEndpoint, got %v", err)
	}
	// Only disabled endpoints: every call is ErrNoEndpoint.
	p4 := newTestPool(t, clock, time.Minute, endpointOf(wrong2, 100))
	if err := p4.Verify(ctx); !errors.Is(err, ErrNoEndpoint) {
		t.Fatalf("expected ErrNoEndpoint, got %v", err)
	}
	if _, err := p4.Call(ctx, "echo"); !errors.Is(err, ErrNoEndpoint) || p4.Available() != 0 {
		t.Fatalf("disabled only: %v", err)
	}
	// An empty pool.
	empty := NewPool(PoolConfig{ChainID: testChainID})
	if err := empty.Verify(ctx); !errors.Is(err, ErrNoEndpoint) {
		t.Fatalf("empty verify: %v", err)
	}
	if _, err := empty.BlockNumber(ctx); !errors.Is(err, ErrNoEndpoint) || empty.Available() != 0 || empty.WS() != nil || empty.Archive() != nil || empty.HasWS() {
		t.Fatalf("empty pool: %v", err)
	}
	if _, err := empty.WSURL(ctx); !errors.Is(err, ErrNoEndpoint) {
		t.Fatalf("empty pool ws url: %v", err)
	}
	if pol := empty.Policy(); pol.Unlimited || pol.Rate != 0 {
		t.Fatalf("an empty pool reports the paced policy: %+v", pol)
	}
	if empty.cooldown != defaultFailoverCooldown {
		t.Fatal("default cooldown")
	}
}

// TestPoolCapabilities: newHeads and archive are routed to the first
// endpoint that has them, whatever the active endpoint is.
func TestPoolCapabilities(t *testing.T) {
	clock := newFakeClock()
	a, b, c := chainFake(t, testChainIDHex), chainFake(t, testChainIDHex), chainFake(t, testChainIDHex)
	p := newTestPool(t, clock, time.Minute,
		endpointOf(a, 4),
		config.EndpointConfig{RPCURL: b.server.URL, WSURL: "wss://b/ws"},
		config.EndpointConfig{RPCURL: c.server.URL, WSURL: "wss://c/ws", Archive: true},
	)
	if err := p.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ws := p.WS(); ws == nil || ws.Index() != 1 || ws.WSURL() != "wss://b/ws" || !p.HasWS() {
		t.Fatalf("ws endpoint: %+v", ws)
	}
	if url, err := p.WSURL(context.Background()); err != nil || url != "wss://b/ws" {
		t.Fatalf("ws url: %q %v", url, err)
	}
	if ar := p.Archive(); ar == nil || ar.Endpoint() == nil || ar.Endpoint().Index() != 2 || !ar.Endpoint().Archive() {
		t.Fatalf("archive endpoint: %+v", p.Archive())
	}
	// The policy follows the active endpoint, not the primary's budget.
	if pol := p.Policy(); pol.Unlimited || pol.Rate != 4 {
		t.Fatalf("primary policy: %+v", pol)
	}
	p.mu.Lock()
	p.active = 1
	p.mu.Unlock()
	if pol := p.Policy(); !pol.Unlimited {
		t.Fatalf("active fallback policy: %+v", pol)
	}
	p.mu.Lock()
	p.active = 0
	p.mu.Unlock()
	if p.ActiveEndpoint() != 0 || p.Endpoints()[0].Client.Pacer().Rate() != 4 || !p.Endpoints()[1].Client.Pacer().Unlimited() {
		t.Fatal("every endpoint keeps its own pacer")
	}
	st := p.Status()
	if st.Endpoints[0].WS || !st.Endpoints[1].WS || st.Endpoints[1].Archive || !st.Endpoints[2].Archive || st.Endpoints[2].Index != 2 {
		t.Fatalf("status capabilities: %+v", st)
	}
	// A failure reported for an endpoint that is no longer active (another
	// caller already moved on) is simply retried on the new one.
	p.mu.Lock()
	p.active = 1
	p.mu.Unlock()
	if !p.failOver(p.Endpoints()[0], errors.New("stale")) || p.Failovers() != 0 || p.ActiveEndpoint() != 1 {
		t.Fatal("a stale failure must not fail over again")
	}
}

// TestPoolProductionConstructor: the constructor production uses, with no
// clock option at all, survives both a startup failover (a primary
// reporting the wrong chain) and a runtime one (a primary answering 500).
// The clock the failover path calls must be set before the options are
// applied, not by them.
func TestPoolProductionConstructor(t *testing.T) {
	ctx := context.Background()
	wrong, good := chainFake(t, "0x1"), chainFake(t, testChainIDHex)
	p := NewPool(PoolConfig{ChainID: testChainID, BatchSize: 50, Endpoints: []config.EndpointConfig{
		endpointOf(wrong, 100), endpointOf(good, 100),
	}}, WithPoolLogger(logger.Nop()))
	if p.now == nil || p.sleep == nil || p.cooldown != defaultFailoverCooldown {
		t.Fatal("the production constructor must install a clock before the options")
	}
	// Startup failover: the mismatching primary is disabled and the pool
	// moves to the fallback, which calls the clock.
	if err := p.Verify(ctx); err != nil || p.ActiveEndpoint() != 1 || p.Failovers() != 1 {
		t.Fatalf("startup failover: %v active=%d failovers=%d", err, p.ActiveEndpoint(), p.Failovers())
	}
	if st := p.Status(); !st.Endpoints[0].Disabled || st.Endpoints[0].Error == "" || st.Endpoints[1].Error != "" {
		t.Fatalf("status carries the sanitized reason: %+v", st.Endpoints)
	}
	echoVia(t, p, "one")

	// Runtime failover on a pool whose primary is fine at start and fails
	// later, again with no clock option.
	a, b := chainFake(t, testChainIDHex), chainFake(t, testChainIDHex)
	q := NewPool(PoolConfig{ChainID: testChainID, BatchSize: 50, Endpoints: []config.EndpointConfig{
		endpointOf(a, 100), endpointOf(b, 100),
	}}, WithPoolLogger(logger.Nop()))
	if err := q.Verify(ctx); err != nil || q.ActiveEndpoint() != 0 {
		t.Fatalf("verify: %v", err)
	}
	a.fail(http.StatusInternalServerError)
	echoVia(t, q, "two")
	if q.ActiveEndpoint() != 1 || q.Failovers() != 1 {
		t.Fatalf("runtime failover: active=%d failovers=%d", q.ActiveEndpoint(), q.Failovers())
	}
	if q.Endpoints()[0].BatchCap() != 50 || q.Endpoints()[0].now == nil {
		t.Fatalf("endpoint clock and cap: %+v", q.Status())
	}
}

// TestPoolArchiveFailover: the archive path verifies the endpoint it
// picks, moves to the next archive endpoint when it fails and stays
// there, and never binds an endpoint that reports another chain.
func TestPoolArchiveFailover(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	plain, wrong, first, second := chainFake(t, testChainIDHex), chainFake(t, "0x1"), newFakeRPC(t), newFakeRPC(t)
	for _, f := range []*fakeRPC{first, second} {
		setupChain(f)
		f.handlers["eth_chainId"] = func([]json.RawMessage) any { return testChainIDHex }
	}
	p := newTestPool(t, clock, time.Minute,
		endpointOf(plain, 1000),
		config.EndpointConfig{RPCURL: wrong.server.URL, Archive: true, CallsPerSecond: 1000},
		config.EndpointConfig{RPCURL: first.server.URL, Archive: true, CallsPerSecond: 1000},
		config.EndpointConfig{RPCURL: second.server.URL, Archive: true, CallsPerSecond: 1000},
	)
	if err := p.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	ar := p.Archive()
	if ar == nil || ar.Endpoint().Index() != 2 {
		t.Fatalf("the archive path skips the endpoint on another chain: %v", ar.Endpoint())
	}
	if s, err := ar.FastSampleAt(ctx, 200); err != nil || s.Header.Number != 200 {
		t.Fatalf("archive sample: %+v %v", s, err)
	}
	if l1, err := ar.L1SampleAt(ctx, 200); err != nil || l1.PerBatchGasCharge != 210_000 || l1.ParentGasFloorPerToken != 10 {
		t.Fatalf("archive L1 sample: %+v %v", l1, err)
	}
	// Ordinary calls stay on the primary while the archive path uses its
	// own endpoint: a capability is routed by capability.
	if p.ActiveEndpoint() != 0 {
		t.Fatalf("archive calls must not move the active endpoint: %d", p.ActiveEndpoint())
	}
	// The archive endpoint goes down: the path moves to the next one and
	// rebinds there.
	first.server.Close()
	if l1, err := ar.L1SampleAt(ctx, 201); err != nil || l1.ArbOSVersion != 61 {
		t.Fatalf("archive L1 failover: %+v %v", l1, err)
	}
	if s, err := ar.FastSampleAt(ctx, 201); err != nil || s.Header.Number != 201 {
		t.Fatalf("archive sample after failover: %+v %v", s, err)
	}
	if ar.Endpoint().Index() != 3 || p.Failovers() != 0 {
		t.Fatalf("rebinding: endpoint=%d failovers=%d", ar.Endpoint().Index(), p.Failovers())
	}
	// A node answer (a block that does not exist) is not an endpoint
	// failure and does not move the binding.
	if _, err := ar.FastSampleAt(ctx, 9_000); err == nil || IsEndpointError(err) || ar.Endpoint().Index() != 3 {
		t.Fatalf("a node answer must not fail over: %v", err)
	}
	// With every archive endpoint gone the error names them all.
	second.server.Close()
	if _, err := ar.FastSampleAt(ctx, 202); !errors.Is(err, ErrNoEndpoint) {
		t.Fatalf("exhausted archive endpoints: %v", err)
	}
	if ar.Endpoint() != nil {
		t.Fatalf("no archive endpoint left: %v", ar.Endpoint())
	}
	if _, err := ar.FastSampleAt(ctx, 203); !errors.Is(err, ErrNoEndpoint) {
		t.Fatalf("no archive endpoint: %v", err)
	}
	// A pool with no archive endpoint at all offers no archive path.
	if newTestPool(t, clock, time.Minute, endpointOf(plain, 1000)).Archive() != nil {
		t.Fatal("no archive endpoint configured")
	}
}

// TestPoolWSRebinds: the WebSocket resolver skips a disabled endpoint and
// one that cannot be verified, and returns the next verified endpoint's
// URL, so a head subscriber rebinds instead of following a dead or
// unverified endpoint for ever.
func TestPoolWSRebinds(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	wrong, flaky, good := chainFake(t, "0x1"), chainFake(t, testChainIDHex), chainFake(t, testChainIDHex)
	// Down for the verification at start and for the first resolution.
	flaky.fail(http.StatusInternalServerError)
	flaky.fail(http.StatusInternalServerError)
	p := newTestPool(t, clock, time.Minute,
		config.EndpointConfig{RPCURL: wrong.server.URL, WSURL: "wss://wrong/ws", CallsPerSecond: 100},
		config.EndpointConfig{RPCURL: flaky.server.URL, WSURL: "wss://flaky/ws", CallsPerSecond: 100},
		config.EndpointConfig{RPCURL: good.server.URL, WSURL: "wss://good/ws", CallsPerSecond: 100},
	)
	if err := p.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	// The first endpoint is on another chain (disabled), the second could
	// not be reached during Verify and fails its check now, so the third
	// one is dialed.
	url, err := p.WSURL(ctx)
	if err != nil || url != "wss://good/ws" {
		t.Fatalf("ws url: %q %v", url, err)
	}
	if ws := p.WS(); ws == nil || ws.Index() != 2 {
		t.Fatalf("only a verified endpoint is bound: %v", ws)
	}
	// Once the flaky endpoint answers it is verified and taken back, since
	// capability routing is by order among the usable endpoints.
	if url, err := p.WSURL(ctx); err != nil || url != "wss://flaky/ws" {
		t.Fatalf("recovered endpoint: %q %v", url, err)
	}
}

// TestPoolStaleEndpointRefused: a call that selected an endpoint before
// another caller failed over is refused under the send gate, its pacer
// reservation is refunded, and the pool retries it on the endpoint that is
// active now instead of hitting the failed one during its cooldown.
func TestPoolStaleEndpointRefused(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	a, b := chainFake(t, testChainIDHex), chainFake(t, testChainIDHex)
	p := newTestPool(t, clock, time.Minute, endpointOf(a, 100), endpointOf(b, 100))
	if err := p.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	before := a.requestCount()
	// Another caller fails the primary over while this one is queued.
	e := p.Endpoints()[0]
	if !p.failOver(e, errors.New("500")) || p.ActiveEndpoint() != 1 {
		t.Fatalf("failover: active=%d", p.ActiveEndpoint())
	}
	tokens := e.Pacer().Available()
	if err := e.preSend(ctx); !errors.Is(err, ErrStaleEndpoint) || !IsEndpointError(err) {
		t.Fatalf("the send gate must refuse a stale endpoint: %v", err)
	}
	// The pool routes around it: the call lands on the active endpoint and
	// the primary is not touched.
	echoVia(t, p, "one")
	if a.requestCount() != before || p.Failovers() != 1 {
		t.Fatalf("a stale endpoint is retried, not failed over: a=%d failovers=%d", a.requestCount(), p.Failovers())
	}
	if e.Pacer().Available() != tokens {
		t.Fatalf("the refused reservation must be refunded: %d, was %d", e.Pacer().Available(), tokens)
	}
	// A capability call names its endpoint and is exempt.
	if err := e.preSend(withCapability(ctx)); err != nil {
		t.Fatalf("capability call: %v", err)
	}
}

// TestEndpointBatchCap: a 429 answered to a batch halves the endpoint's
// cap (floor 10) and the batch is resent at the new size; a successful
// minute grows it back one step at a time.
func TestEndpointBatchCap(t *testing.T) {
	ctx := context.Background()
	f := newFakeRPC(t)
	setupChain(f)
	f.handlers["eth_chainId"] = func([]json.RawMessage) any { return testChainIDHex }
	f.maxItems = 50
	clock := newFakeClock()
	p := NewPool(PoolConfig{ChainID: testChainID, Endpoints: []config.EndpointConfig{endpointOf(f, 1000)}, BatchSize: 100},
		WithPoolClientOptions(WithHTTPClient(f.server.Client())), withPoolClock(clock.Now, clock.Sleep))
	e := p.Endpoints()[0]
	if e.BatchCap() != 100 {
		t.Fatalf("cap = %d", e.BatchCap())
	}
	nums := make([]uint64, 0, 100)
	for n := uint64(100); n < 200; n++ {
		nums = append(nums, n)
	}
	hs, err := p.HeadersByNumbers(ctx, nums)
	if err != nil || len(hs) != 100 || hs[99].Number != 199 {
		t.Fatalf("headers: %d %v", len(hs), err)
	}
	// After eth_chainId: 100 items rejected, then four batches of 50 after
	// the 2 s back-off (one header and one receipt call per block).
	if e.BatchCap() != 50 || f.requestCount() != 6 || fmt.Sprint(clock.Sleeps()) != "[2s]" {
		t.Fatalf("after 429: cap=%d requests=%d sleeps=%v", e.BatchCap(), f.requestCount(), clock.Sleeps())
	}
	// No recovery before a successful minute has passed.
	if _, err := p.HeadersByNumbers(ctx, nums[:50]); err != nil || e.BatchCap() != 50 {
		t.Fatalf("early recovery: cap=%d %v", e.BatchCap(), err)
	}
	clock.Advance(capRecoveryInterval)
	f.maxItems = 100
	if _, err := p.HeadersByNumbers(ctx, nums[:10]); err != nil || e.BatchCap() != 100 {
		t.Fatalf("recovery: cap=%d %v", e.BatchCap(), err)
	}
	// The cap never drops below 10; once it is there the endpoint gives up
	// after the client's attempts and reports an endpoint failure.
	f.maxItems = 1
	if _, err := p.HeadersByNumbers(ctx, nums[:20]); !errors.Is(err, ErrRateLimited) || !IsEndpointError(err) {
		t.Fatalf("expected rate limit failure, got %v", err)
	}
	if e.BatchCap() != 10 || p.Stats().RateLimitEvents != 6 {
		t.Fatalf("floor: cap=%d stats=%+v", e.BatchCap(), p.Stats())
	}
	// Single calls are unaffected by the cap and a 429 on one does not
	// halve it further.
	if h, err := p.HeaderByNumber(ctx, 150); err != nil || h.Number != 150 || e.BatchCap() != 10 {
		t.Fatalf("single call: %+v %v cap=%d", h, err, e.BatchCap())
	}
	f.maxItems = 0
	f.fail(http.StatusTooManyRequests)
	if _, err := p.BlockNumber(ctx); err != nil || e.BatchCap() != 10 {
		t.Fatalf("single 429: %v cap=%d", err, e.BatchCap())
	}
	// Recovery is one doubling per quiet minute: 10, 20, 40, 80, 100.
	for _, want := range []int{20, 40, 80, 100, 100} {
		clock.Advance(capRecoveryInterval)
		if _, err := p.BlockNumber(ctx); err != nil || e.BatchCap() != want {
			t.Fatalf("recovery step: cap=%d want %d %v", e.BatchCap(), want, err)
		}
	}
	// Full blocks use the cap too, and transport errors surface.
	if blocks, err := p.BlocksWithTxs(ctx, []uint64{320}); err != nil || len(blocks) != 1 || len(blocks[0].Txs) != 2 {
		t.Fatalf("blocks with txs: %v %v", blocks, err)
	}
	f.server.Close()
	if _, err := p.HeadersByNumbers(ctx, nums[:5]); !IsEndpointError(err) {
		t.Fatalf("transport error: %v", err)
	}
	// A batch size outside the limits falls back to MaxBatch, and the
	// floor never exceeds a small configured size.
	if newEndpoint(0, config.EndpointConfig{}, 0, p.log, clock.Now).BatchCap() != MaxBatch || newEndpoint(0, config.EndpointConfig{}, 5, p.log, clock.Now).minBatchCap != 5 {
		t.Fatal("batch size defaults")
	}
}

// TestPoolTypedCalls runs every typed call through a pool against the
// fake chain.
func TestPoolTypedCalls(t *testing.T) {
	ctx := context.Background()
	f := newFakeRPC(t)
	setupChain(f)
	f.handlers["eth_chainId"] = func([]json.RawMessage) any { return testChainIDHex }
	clock := newFakeClock()
	p := NewPool(PoolConfig{ChainID: testChainID, Endpoints: []config.EndpointConfig{endpointOf(f, 1000)}, BatchSize: 100},
		WithPoolClientOptions(WithHTTPClient(f.server.Client())), withPoolClock(clock.Now, clock.Sleep))
	if id, err := p.ChainID(ctx); err != nil || id != testChainID {
		t.Fatalf("ChainID: %d %v", id, err)
	}
	if n, err := p.BlockNumber(ctx); err != nil || n != 320 {
		t.Fatalf("BlockNumber: %d %v", n, err)
	}
	if h, err := p.HeaderByNumber(ctx, 100); err != nil || h.Number != 100 {
		t.Fatalf("HeaderByNumber: %+v %v", h, err)
	}
	if s, err := p.FastSample(ctx); err != nil || s.Header.Number != 320 || len(s.Constraints) != 2 {
		t.Fatalf("FastSample: %+v %v", s, err)
	}
	if s, err := p.FastSampleAt(ctx, 200); err != nil || s.Header.Number != 200 {
		t.Fatalf("FastSampleAt: %+v %v", s, err)
	}
	if logs, err := p.OwnerActsLogs(ctx, 0, 100); err != nil || len(logs) != 1 {
		t.Fatalf("OwnerActsLogs: %v %v", logs, err)
	}
	if receipts, err := p.TransactionReceipts(ctx, []string{"0x01"}); err != nil || len(receipts) != 1 || receipts[0].GasUsed != 3 {
		t.Fatalf("TransactionReceipts: %v %v", receipts, err)
	}
	if bal, err := p.Balance(ctx, "0x1"); err != nil || bal.Int64() != 100 {
		t.Fatalf("Balance: %s %v", bal, err)
	}
	if l1, err := p.L1Sample(ctx); err != nil || l1.RewardRate != 10 {
		t.Fatalf("L1Sample: %+v %v", l1, err)
	}
	if l1, err := p.L1SampleAt(ctx, 100); err != nil || l1.ArbOSVersion != 61 {
		t.Fatalf("L1SampleAt: %+v %v", l1, err)
	}
	if acc, err := p.FeeAccounts(ctx); err != nil || acc.Network.Balance.Int64() != 100 {
		t.Fatalf("FeeAccounts: %+v %v", acc, err)
	}
	if v, err := p.ArbOSVersion(ctx); err != nil || v != 61 {
		t.Fatalf("ArbOSVersion: %d %v", v, err)
	}
	if _, err := p.HeadersByNumbers(ctx, []uint64{100, 5}); err == nil || IsEndpointError(err) {
		t.Fatalf("a missing block is an answer, not a failure: %v", err)
	}
	if st := p.Status(); st.Active != 0 || st.Failovers != 0 || len(st.Endpoints) != 1 {
		t.Fatalf("status: %+v", st)
	}
}

// TestPoolWindowBudget: on a budgeted endpoint the loops' whole call mix
// (samples, header batches, logs, full blocks, L1 getters) never puts more
// than burst + rate*10 calls into any ten second window, and no request
// carries more items than the bucket can hold, however the callers batch.
func TestPoolWindowBudget(t *testing.T) {
	ctx := context.Background()
	f := newFakeRPC(t)
	setupChain(f)
	f.handlers["eth_chainId"] = func([]json.RawMessage) any { return testChainIDHex }
	f.maxItems = 8 // a request beyond the burst is answered with 429
	clock := newFakeClock()
	p := NewPool(PoolConfig{ChainID: testChainID, Endpoints: []config.EndpointConfig{endpointOf(f, 4)}, BatchSize: 100},
		WithPoolClientOptions(WithHTTPClient(f.server.Client())), withPoolClock(clock.Now, clock.Sleep))
	nums := make([]uint64, 0, 100)
	for n := uint64(100); n < 200; n++ {
		nums = append(nums, n)
	}
	steps := []func() error{
		func() error { _, err := p.FastSample(ctx); return err },
		func() error { _, err := p.HeadersByNumbers(ctx, nums); return err },
		func() error { _, err := p.OwnerActsLogs(ctx, 0, 100_000); return err },
		func() error { _, err := p.L1Sample(ctx); return err },
		func() error { _, err := p.BlocksWithTxs(ctx, nums[:20]); return err },
		func() error { _, err := p.FeeAccounts(ctx); return err },
		func() error { _, err := p.FastSampleAt(ctx, 150); return err },
		func() error { _, err := p.HeadersByNumbers(ctx, nums[:30]); return err },
	}
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if st := p.Stats(); st.CallsLast10s > 8+40 || st.RateLimitEvents != 0 {
			t.Fatalf("step %d: %d calls in the last 10 s (rate limit events %d)", i, st.CallsLast10s, st.RateLimitEvents)
		}
		clock.Advance(300 * time.Millisecond)
	}
	if p.Endpoints()[0].BatchCap() != 100 || f.requestCount() < 13 {
		t.Fatalf("batches must be chunked to the burst without touching the cap: cap %d requests %d", p.Endpoints()[0].BatchCap(), f.requestCount())
	}
}

// TestEndpointBatchCapJSONRPC: a batch answered with a JSON-RPC rate limit
// error on one of its items halves the endpoint's cap and is resent at
// the new size, and one that keeps failing fails the endpoint over.
func TestEndpointBatchCapJSONRPC(t *testing.T) {
	ctx := context.Background()
	f := newFakeRPC(t)
	setupChain(f)
	f.handlers["eth_chainId"] = func([]json.RawMessage) any { return testChainIDHex }
	f.maxItems = 50
	f.limitErr = &RPCError{Code: RateLimitCodeQuickNode, Message: "50/second request limit reached"}
	clock := newFakeClock()
	p := NewPool(PoolConfig{ChainID: testChainID, Endpoints: []config.EndpointConfig{endpointOf(f, 1000)}, BatchSize: 100},
		WithPoolClientOptions(WithHTTPClient(f.server.Client())), withPoolClock(clock.Now, clock.Sleep))
	e := p.Endpoints()[0]
	nums := make([]uint64, 0, 100)
	for n := uint64(100); n < 200; n++ {
		nums = append(nums, n)
	}
	hs, err := p.HeadersByNumbers(ctx, nums)
	if err != nil || len(hs) != 100 || hs[99].Number != 199 {
		t.Fatalf("headers: %d %v", len(hs), err)
	}
	// After eth_chainId: 200 header and receipt items rejected in two
	// batches, then four batches of 50 after the 2 s back-off.
	if e.BatchCap() != 50 || f.requestCount() != 6 || fmt.Sprint(clock.Sleeps()) != "[2s]" || p.Stats().RateLimitEvents != 1 {
		t.Fatalf("after a JSON-RPC limit: cap=%d requests=%d sleeps=%v stats=%+v", e.BatchCap(), f.requestCount(), clock.Sleeps(), p.Stats())
	}
	// A persistent limit at the floor is an endpoint failure.
	f.maxItems = 1
	if _, err := p.HeadersByNumbers(ctx, nums[:20]); !errors.Is(err, ErrRateLimited) || !IsEndpointError(err) {
		t.Fatalf("expected rate limit failure, got %v", err)
	}
	if e.BatchCap() != 10 {
		t.Fatalf("floor: cap=%d", e.BatchCap())
	}

	// With a fallback, a primary that keeps answering the limit is failed
	// over like one answering HTTP 429.
	a, b := chainFake(t, testChainIDHex), chainFake(t, testChainIDHex)
	limited := scriptStep{status: http.StatusOK, body: `[{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"limit exceeded"}}]`}
	// Two attempts per request: the verification at start and the lazy one
	// before the first call both stay throttled.
	a.script = []scriptStep{limited, limited, limited, limited}
	p2 := newTestPool(t, clock, time.Minute, endpointOf(a, 100), endpointOf(b, 100))
	if err := p2.Verify(ctx); err != nil || p2.ActiveEndpoint() != 0 || p2.Endpoints()[0].Verified() || p2.Endpoints()[0].Disabled() {
		t.Fatalf("a throttled primary stays unverified: %v active=%d", err, p2.ActiveEndpoint())
	}
	echoVia(t, p2, "one")
	if p2.ActiveEndpoint() != 1 || p2.Failovers() != 1 || b.requestCount() != 2 || p2.Stats().RateLimitEvents != 4 {
		t.Fatalf("failover on a JSON-RPC limit: active=%d failovers=%d b=%d stats=%+v", p2.ActiveEndpoint(), p2.Failovers(), b.requestCount(), p2.Stats())
	}
}

// TestPoolFastLane: the class a context carries reaches the endpoint's
// pacer through the pool, so a fast call is served while a bulk header
// batch is still paying for its reservation, within one token interval
// on a 4/s endpoint.
func TestPoolFastLane(t *testing.T) {
	ctx := context.Background()
	f := newFakeRPC(t)
	setupChain(f)
	f.handlers["eth_chainId"] = func([]json.RawMessage) any { return testChainIDHex }
	f.handlers["echo"] = echoHandler
	p := NewPool(PoolConfig{ChainID: testChainID, Endpoints: []config.EndpointConfig{endpointOf(f, 4)}, BatchSize: 100},
		WithPoolClientOptions(WithHTTPClient(f.server.Client())))
	if err := p.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	nums := make([]uint64, 0, 100)
	for n := uint64(100); n < 200; n++ {
		nums = append(nums, n)
	}
	bctx, cancel := context.WithCancel(ctx)
	defer cancel()
	bulkDone := make(chan error, 1)
	go func() {
		_, err := p.HeadersByNumbers(bctx, nums)
		bulkDone <- err
	}()
	time.Sleep(100 * time.Millisecond) // the first chunk went, the next one sleeps for tokens
	start := time.Now()
	raw, err := p.Call(WithClass(ctx, Fast), "echo", "head")
	if err != nil || string(raw) != `"head"` {
		t.Fatalf("fast call: %s %v", raw, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("fast call waited %v behind the bulk batch", elapsed)
	}
	cancel()
	if err := <-bulkDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("bulk batch: %v", err)
	}
	if e := p.Endpoints()[0]; e.Pacer().MaxBatch() != 7 || e.Pacer().Reserve() != 1 {
		t.Fatalf("bulk batches leave the reserve: maxBatch %d reserve %v", e.Pacer().MaxBatch(), e.Pacer().Reserve())
	}
}

// TestPoolWSHealthRebinds: an endpoint whose JSON-RPC answers can still
// have a WebSocket that cannot be dialed or subscribed to. HTTP
// verification says nothing about that, so WebSocket health is tracked per
// endpoint: a reported failure cools the endpoint's socket down, the next
// resolution leases the following verified WS-capable endpoint, and
// /status says which socket is cooling and why.
func TestPoolWSHealthRebinds(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	a, b := chainFake(t, testChainIDHex), chainFake(t, testChainIDHex)
	p := newTestPool(t, clock, time.Minute,
		config.EndpointConfig{RPCURL: a.server.URL, WSURL: "wss://a/ws?key=" + theKey, CallsPerSecond: 100},
		config.EndpointConfig{RPCURL: b.server.URL, WSURL: "wss://b/ws", CallsPerSecond: 100},
	)
	if err := p.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	lease, err := p.WSEndpoint(ctx)
	if err != nil || lease.Index != 0 || lease.URL != "wss://a/ws?key="+theKey {
		t.Fatalf("first lease: %+v %v", lease, err)
	}
	// A dial failure quoting the URL cools the endpoint down at once, and
	// the reason kept for /status names the endpoint, not the key.
	lease.Failed(false, errors.New("dial wss://a/ws?key="+theKey+": connection refused"))
	st := p.Status()
	if !st.Endpoints[0].WSCooling || st.Endpoints[1].WSCooling {
		t.Fatalf("websocket cooldown not reported: %+v", st.Endpoints)
	}
	if strings.Contains(st.Endpoints[0].WSError, theKey) || !strings.Contains(st.Endpoints[0].WSError, "endpoint 0") {
		t.Fatalf("the reported reason must not carry the key: %q", st.Endpoints[0].WSError)
	}
	if st.Endpoints[0].Disabled || p.ActiveEndpoint() != 0 {
		t.Fatal("a broken websocket must not disable the endpoint's JSON-RPC")
	}
	// The subscriber now resolves the next verified WS-capable endpoint.
	if lease, err = p.WSEndpoint(ctx); err != nil || lease.Index != 1 || lease.URL != "wss://b/ws" {
		t.Fatalf("rebound lease: %+v %v", lease, err)
	}
	// A subscription that connects and drops once is not held against an
	// endpoint: only repeated drops soon after connecting cool it down.
	for range wsDropLimit - 1 {
		lease.Connected()
		lease.Failed(true, errors.New("read: unexpected EOF"))
	}
	if until, _ := p.Endpoints()[1].wsCooling(); !until.IsZero() {
		t.Fatal("an occasional reconnect must not cool an endpoint down")
	}
	lease.Connected()
	lease.Failed(true, errors.New("read: unexpected EOF"))
	if until, _ := p.Endpoints()[1].wsCooling(); until.IsZero() {
		t.Fatal("repeated drops must cool the endpoint down")
	}
	// Every socket is cooling: the one that recovers first is leased
	// anyway, so a single-endpoint network keeps trying.
	if lease, err = p.WSEndpoint(ctx); err != nil || lease.Index != 0 {
		t.Fatalf("all cooling: %+v %v", lease, err)
	}
	// Once the cooldown passes the primary is resolved normally again.
	clock.Advance(2 * time.Minute)
	if lease, err = p.WSEndpoint(ctx); err != nil || lease.Index != 0 {
		t.Fatalf("after the cooldown: %+v %v", lease, err)
	}
	// A subscription that held for a while resets the drop count.
	lease.Connected()
	clock.Advance(wsHealthyFor)
	lease.Failed(true, errors.New("read: unexpected EOF"))
	if until, _ := p.Endpoints()[0].wsCooling(); !until.IsZero() {
		t.Fatal("a long lived subscription must not count against the endpoint")
	}
	// A nil lease (a subscriber built with a plain URL) reports nothing.
	var none *WSLease
	none.Connected()
	none.Failed(true, errors.New("x"))
}

// TestPoolStatusCarriesObservableCounters: the routing state reports each
// endpoint's own throttling count and the pool's aggregate call counts per
// pacer class, and never a URL.
func TestPoolStatusCarriesObservableCounters(t *testing.T) {
	ctx := context.Background()
	a, b := chainFake(t, testChainIDHex), chainFake(t, testChainIDHex)
	clock := newFakeClock()
	p := newTestPool(t, clock, 30*time.Second, endpointOf(a, 100), endpointOf(b, 100))
	if err := p.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	a.fail(http.StatusTooManyRequests)
	echoVia(t, p, "one")
	if _, err := p.Call(WithClass(ctx, Fast), "echo", "two"); err != nil {
		t.Fatal(err)
	}
	st := p.Status()
	if st.Endpoints[0].RateLimitEvents != 1 || st.Endpoints[1].RateLimitEvents != 0 {
		t.Fatalf("per-endpoint rate limit events = %d and %d", st.Endpoints[0].RateLimitEvents, st.Endpoints[1].RateLimitEvents)
	}
	stats := p.Stats()
	if stats.FastCalls != 1 || stats.BulkCalls == 0 {
		t.Fatalf("pool class counters = fast %d bulk %d", stats.FastCalls, stats.BulkCalls)
	}
	for _, e := range st.Endpoints {
		if strings.Contains(e.Error, "http") || strings.Contains(e.WSError, "http") {
			t.Fatalf("an endpoint URL reached the status: %+v", e)
		}
	}
}
