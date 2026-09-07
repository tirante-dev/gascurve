package nitro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/logger"
)

// defaultFailoverCooldown is used when PoolConfig.Cooldown is not set.
const defaultFailoverCooldown = time.Minute

// ErrNoEndpoint is returned when a pool has no usable endpoint left.
var ErrNoEndpoint = errors.New("rpc: no usable endpoint")

// PoolConfig describes one network's endpoints.
type PoolConfig struct {
	// ChainID is the chain every endpoint must report from eth_chainId.
	ChainID uint64
	// Endpoints lists the primary first, then the fallbacks.
	Endpoints []config.EndpointConfig
	// BatchSize is every endpoint's starting batch cap
	// (collector.header_batch_size).
	BatchSize int
	// Cooldown is how long the pool stays on a fallback before probing the
	// primary again (collector.failover_cooldown).
	Cooldown time.Duration
}

// PoolOption customizes a Pool.
type PoolOption func(*Pool)

// WithPoolLogger sets the logger; endpoints log with their index.
func WithPoolLogger(l *logger.Logger) PoolOption { return func(p *Pool) { p.log = l } }

// WithPoolClientOptions applies opts to every endpoint's Client.
func WithPoolClientOptions(opts ...Option) PoolOption {
	return func(p *Pool) { p.clientOpts = append(p.clientOpts, opts...) }
}

// withPoolClock replaces the clock and sleeper of the pool, its endpoints
// and their pacers, for tests.
func withPoolClock(now func() time.Time, sleep func(context.Context, time.Duration) error) PoolOption {
	return func(p *Pool) { p.now, p.sleep = now, sleep }
}

// EndpointStatus describes one endpoint for /status. URLs are never
// included: they can carry keys. Error is why the endpoint was disabled,
// empty while it is usable; it is built from the chain ids alone, so it
// carries no URL or credential either.
type EndpointStatus struct {
	Index    int
	WS       bool
	Archive  bool
	Disabled bool
	Error    string
	// WSCooling is set while the endpoint's WebSocket is cooled down after
	// a dial, subscribe or repeated disconnect failure; WSError says why,
	// sanitized the same way (never a URL or a credential). WebSocket
	// health is separate from HTTP verification: an endpoint whose
	// JSON-RPC answers can still have a socket that does not work.
	WSCooling bool
	WSError   string
}

// PoolStatus is the pool's routing state for /status.
type PoolStatus struct {
	Active    int
	Failovers uint64
	Endpoints []EndpointStatus
}

// Pool routes one network's JSON-RPC calls across its endpoints. Ordinary
// calls go to the active endpoint, the primary unless it failed: after a
// failed request (an EndpointError: transport error, HTTP 5xx, throttling
// past its back-off, or a chain id mismatch) the pool moves to the next
// usable endpoint for the cooldown, then probes the primary with one
// eth_chainId before returning to it. Capabilities are routed by WS and
// Archive independently of the active endpoint. Every endpoint is
// verified against the chain id before its first use; a mismatch disables
// it for good.
type Pool struct {
	chainID    uint64
	endpoints  []*Endpoint
	cooldown   time.Duration
	log        *logger.Logger
	now        func() time.Time
	sleep      func(context.Context, time.Duration) error
	clientOpts []Option
	// archive is the managed historical-state path, built once so callers
	// (and their failover state) share one binding; nil when no endpoint
	// serves historical state.
	archive *ArchivePool

	mu            sync.Mutex
	active        int
	failoverUntil time.Time
	probing       bool
	failovers     uint64
}

// NewPool builds the endpoints of cfg. Nothing is contacted until Verify
// or the first call.
func NewPool(cfg PoolConfig, opts ...PoolOption) *Pool {
	// The clock is set before the options so that production construction,
	// which passes none, still has one: failover and primary probing call
	// it on the first endpoint failure.
	p := &Pool{chainID: cfg.ChainID, cooldown: cfg.Cooldown, log: logger.Nop(), now: time.Now, sleep: sleepContext}
	for _, o := range opts {
		o(p)
	}
	if p.cooldown <= 0 {
		p.cooldown = defaultFailoverCooldown
	}
	for i, ec := range cfg.Endpoints {
		p.endpoints = append(p.endpoints, p.newEndpoint(i, ec, cfg.BatchSize))
	}
	for _, e := range p.endpoints {
		e.setPreSend(p.stillActive(e))
		if e.archive && p.archive == nil {
			p.archive = &ArchivePool{pool: p}
		}
	}
	return p
}

func (p *Pool) newEndpoint(i int, ec config.EndpointConfig, batchSize int) *Endpoint {
	opts := append([]Option(nil), p.clientOpts...)
	opts = append(opts, WithPacer(NewPacer(ec.CallsPerSecond).withClock(p.now, p.sleep)), withClock(p.now, p.sleep))
	return newEndpoint(i, ec, batchSize, p.log, p.now, opts...)
}

// stillActive builds the check the endpoint makes under its send lock: an
// ordinary call that selected this endpoint before another caller failed
// over must not reach the wire. Capability calls (the archive path) select
// their endpoint themselves and are exempt.
func (p *Pool) stillActive(e *Endpoint) func(context.Context) error {
	return func(ctx context.Context) error {
		if capabilityCall(ctx) {
			return nil
		}
		p.mu.Lock()
		active := p.endpoints[p.active]
		p.mu.Unlock()
		if active == e {
			return nil
		}
		return &EndpointError{Err: fmt.Errorf("%w: endpoint %d, ordinary calls now go to %d", ErrStaleEndpoint, e.index, active.index)}
	}
}

// capabilityKey marks a context whose calls are routed by capability
// rather than by the active endpoint.
type capabilityKey struct{}

// withCapability marks ctx as a capability call (historical state), which
// picks its own endpoint and is not bound to the active one.
func withCapability(ctx context.Context) context.Context {
	return context.WithValue(ctx, capabilityKey{}, true)
}

func capabilityCall(ctx context.Context) bool {
	v, _ := ctx.Value(capabilityKey{}).(bool)
	return v
}

// Endpoints returns every endpoint, primary first.
func (p *Pool) Endpoints() []*Endpoint { return p.endpoints }

// ActiveEndpoint returns the index of the endpoint ordinary calls go to
// (0 is the primary).
func (p *Pool) ActiveEndpoint() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active
}

// Failovers counts how many times the pool moved to another endpoint.
func (p *Pool) Failovers() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failovers
}

// Status reports the routing state.
func (p *Pool) Status() PoolStatus {
	p.mu.Lock()
	st := PoolStatus{Active: p.active, Failovers: p.failovers, Endpoints: make([]EndpointStatus, len(p.endpoints))}
	p.mu.Unlock()
	for i, e := range p.endpoints {
		until, reason := e.wsCooling()
		st.Endpoints[i] = EndpointStatus{
			Index: i, WS: e.wsURL != "", Archive: e.archive, Disabled: e.Disabled(), Error: e.Reason(),
			WSCooling: !until.IsZero(), WSError: reason,
		}
	}
	return st
}

// WS returns the first verified endpoint with a WebSocket URL, or nil. An
// endpoint that could not be reached during Verify is not verified and is
// never bound: it would otherwise be followed for ever, and, if it later
// answered on another chain, its heads would be written under this chain
// id.
func (p *Pool) WS() *Endpoint { return p.capable(func(e *Endpoint) bool { return e.wsURL != "" }) }

// capable returns the first verified, usable endpoint matching want.
func (p *Pool) capable(want func(*Endpoint) bool) *Endpoint {
	for _, e := range p.endpoints {
		if want(e) && !e.Disabled() && e.Verified() {
			return e
		}
	}
	return nil
}

// HasWS reports whether any endpoint is configured with a ws_url, whatever
// its verification state: the follower builds its subscriber from that and
// lets WSURL choose an endpoint at every connection attempt.
func (p *Pool) HasWS() bool {
	for _, e := range p.endpoints {
		if e.wsURL != "" {
			return true
		}
	}
	return false
}

// WSLease binds a HeadSubscriber to one endpoint's WebSocket for one
// connection attempt: what to dial, and where to report what happened. A
// subscriber that reports back lets the pool track WebSocket health per
// endpoint, apart from the HTTP verification that only proves the
// endpoint's JSON-RPC answers, and rebind to another endpoint when a
// socket cannot be dialed, cannot be subscribed to, or will not stay up.
type WSLease struct {
	// URL is the endpoint to dial. It is a credential: never log it.
	URL string
	// Index names the endpoint in logs and errors.
	Index int

	pool  *Pool
	e     *Endpoint
	scrub *scrubber
}

// Connected reports a live subscription on the leased endpoint. A lease
// that names no endpoint (one a test built by hand) is inert.
func (l *WSLease) Connected() {
	if l == nil || l.e == nil {
		return
	}
	l.e.noteWSConnected()
}

// Failed reports that the leased endpoint's WebSocket could not be
// dialed, could not be subscribed to, or did not stay up. subscribed says
// whether the subscription had been acknowledged before it broke.
func (l *WSLease) Failed(subscribed bool, err error) {
	if l == nil || l.e == nil || err == nil {
		return
	}
	reason := l.scrub.text(err.Error())
	if l.e.noteWSFailure(subscribed, l.pool.cooldown, reason) {
		l.pool.log.Warn("endpoint websocket cooled down, resolving another one",
			"endpoint", l.e.index, "cooldown", l.pool.cooldown.String(), "err", reason)
	}
}

// WSEndpoint resolves the WebSocket endpoint to dial next and leases it:
// the first usable one with a ws_url whose socket is not cooling down,
// verified now when it has not been verified yet. A HeadSubscriber calls
// it before every connection attempt, so a subscriber rebinds to the next
// endpoint as soon as the one it followed is disabled, fails verification
// or has a socket that does not work, and comes back to the primary once
// it recovers. When every WebSocket endpoint is cooling down the one that
// recovers first is leased anyway: a network with a single WebSocket
// endpoint must keep trying on the subscriber's own back-off rather than
// lose heads for a whole cooldown.
func (p *Pool) WSEndpoint(ctx context.Context) (*WSLease, error) {
	var errs []error
	var cooling *Endpoint
	var coolingUntil time.Time
	for _, e := range p.endpoints {
		if e.wsURL == "" || e.Disabled() {
			continue
		}
		if until, _ := e.wsCooling(); !until.IsZero() {
			if cooling == nil || until.Before(coolingUntil) {
				cooling, coolingUntil = e, until
			}
			continue
		}
		if err := p.verify(withCapability(ctx), e); err != nil {
			errs = append(errs, err)
			continue
		}
		return p.lease(e), nil
	}
	if cooling != nil {
		err := p.verify(withCapability(ctx), cooling)
		if err == nil {
			return p.lease(cooling), nil
		}
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return nil, ErrNoEndpoint
	}
	return nil, fmt.Errorf("%w: %w", ErrNoEndpoint, errors.Join(errs...))
}

func (p *Pool) lease(e *Endpoint) *WSLease {
	return &WSLease{URL: e.wsURL, Index: e.index, pool: p, e: e, scrub: e.scrub}
}

// WSURL resolves the WebSocket endpoint to dial next and returns its URL
// alone. WSEndpoint is the fuller form: it leases the endpoint, so the
// subscriber can report whether its socket worked.
func (p *Pool) WSURL(ctx context.Context) (string, error) {
	l, err := p.WSEndpoint(ctx)
	if err != nil {
		return "", err
	}
	return l.URL, nil
}

// ArchivePool routes historical state calls across the endpoints that
// serve them. It is independent of the active endpoint (a capability is
// routed by capability, not by order): it verifies the endpoint it picks,
// fails over to the next archive endpoint on an endpoint error, and stays
// there, so a dead archive endpoint is left behind instead of retried for
// ever.
type ArchivePool struct {
	pool *Pool
	mu   sync.Mutex
	at   int
}

// Archive returns the managed archive path, or nil when no endpoint is
// configured to serve historical state. The same path is returned every
// time, so its binding and failover state are shared by every caller.
func (p *Pool) Archive() *ArchivePool { return p.archive }

// Endpoint returns the archive endpoint calls currently go to, or nil when
// none is usable.
func (a *ArchivePool) Endpoint() *Endpoint {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.nextLocked()
}

// nextLocked returns the first usable archive endpoint at or after the
// current one.
func (a *ArchivePool) nextLocked() *Endpoint {
	for i := a.at; i < len(a.pool.endpoints); i++ {
		if e := a.pool.endpoints[i]; e.archive && !e.Disabled() {
			a.at = i
			return e
		}
	}
	return nil
}

// failed moves past an endpoint that could not serve historical state.
func (a *ArchivePool) failed(e *Endpoint) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.at == e.index {
		a.at = e.index + 1
	}
}

// FastSampleAt reads the pricer state at a block from an archive endpoint,
// verifying it first and moving to the next one on an endpoint error.
func (a *ArchivePool) FastSampleAt(ctx context.Context, number uint64) (*Sample, error) {
	ctx = withCapability(ctx)
	var errs []error
	for range a.pool.endpoints {
		e := a.Endpoint()
		if e == nil {
			break
		}
		err := a.pool.verify(ctx, e)
		if err == nil {
			var s *Sample
			if s, err = e.FastSampleAt(ctx, number); err == nil {
				return s, nil
			}
		}
		if ctx.Err() != nil || !IsEndpointError(err) {
			return nil, err
		}
		a.pool.log.Warn("archive endpoint failed, moving to the next one", "endpoint", e.index, "err", err.Error())
		errs = append(errs, err)
		a.failed(e)
	}
	if len(errs) == 0 {
		return nil, ErrNoEndpoint
	}
	return nil, fmt.Errorf("%w: %w", ErrNoEndpoint, errors.Join(errs...))
}

// Verify checks every endpoint's eth_chainId against the configured chain
// id. A mismatching endpoint is disabled for good and reported; one that
// cannot be reached stays unverified and is checked again before its first
// use. Verify fails when no endpoint could be verified.
func (p *Pool) Verify(ctx context.Context) error {
	var errs []error
	usable := 0
	for _, e := range p.endpoints {
		if e.Disabled() {
			continue
		}
		if err := p.check(ctx, e); err != nil {
			errs = append(errs, err)
			continue
		}
		usable++
	}
	if usable == 0 {
		if len(errs) == 0 {
			return ErrNoEndpoint
		}
		return fmt.Errorf("%w: %w", ErrNoEndpoint, errors.Join(errs...))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if from := p.endpoints[p.active]; from.Disabled() {
		p.failOverLocked(from, errors.New(from.Reason()))
	}
	return nil
}

// check asks the endpoint for its chain id: a match marks it verified, a
// mismatch disables it. Both failures are EndpointErrors.
func (p *Pool) check(ctx context.Context, e *Endpoint) error {
	// Verification addresses one endpoint by name, so it is exempt from the
	// active-endpoint check the ordinary path makes under the send lock.
	id, err := e.ChainID(withCapability(ctx))
	if err != nil {
		p.log.Warn("endpoint unverified, eth_chainId failed", "endpoint", e.index, "err", err.Error())
		return endpointErrorf("endpoint %d: eth_chainId: %w", e.index, err)
	}
	if id != p.chainID {
		reason := fmt.Sprintf("reports chain id %d, configured %d", id, p.chainID)
		e.disable(reason)
		p.log.Error("endpoint disabled, chain id mismatch", "endpoint", e.index, "rpcChainId", id, "configured", p.chainID)
		return endpointErrorf("endpoint %d: %s", e.index, reason)
	}
	if !e.Verified() {
		e.setVerified()
		p.log.Info("endpoint verified", "endpoint", e.index, "ws", e.wsURL != "", "archive", e.archive, "batchCap", e.BatchCap())
	}
	return nil
}

// verify checks an endpoint that has not been verified yet.
func (p *Pool) verify(ctx context.Context, e *Endpoint) error {
	if e.Verified() {
		return nil
	}
	return p.check(ctx, e)
}

// currentLocked returns the active endpoint, moving on when it has been
// disabled. Nil means nothing is usable.
func (p *Pool) currentLocked() *Endpoint {
	if len(p.endpoints) == 0 {
		return nil
	}
	e := p.endpoints[p.active]
	if !e.Disabled() {
		return e
	}
	if !p.advanceLocked() {
		return nil
	}
	return p.endpoints[p.active]
}

// advanceLocked moves active to the next usable endpoint after it, in
// cyclic order. It reports false when there is none.
func (p *Pool) advanceLocked() bool {
	for step := 1; step < len(p.endpoints); step++ {
		i := (p.active + step) % len(p.endpoints)
		if !p.endpoints[i].Disabled() {
			p.active = i
			return true
		}
	}
	return false
}

// failOverLocked moves away from the endpoint that just failed and starts
// the cooldown. It reports false when no other endpoint is usable.
func (p *Pool) failOverLocked(from *Endpoint, cause error) bool {
	if !p.advanceLocked() {
		return false
	}
	p.failoverUntil = p.now().Add(p.cooldown)
	p.failovers++
	p.log.Warn("endpoint failed, failing over", "from", from.index, "to", p.active, "cooldown", p.cooldown.String(), "err", cause.Error())
	return true
}

// failOver is failOverLocked for a request that failed on from. When
// another caller already moved on it simply reports that a retry is worth
// it.
func (p *Pool) failOver(from *Endpoint, cause error) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.endpoints[p.active] != from {
		return true
	}
	return p.failOverLocked(from, cause)
}

// pick returns the endpoint the next ordinary call goes to. Once the
// cooldown on a fallback has passed the primary is probed first (by one
// caller at a time) and taken back when it answers with the right chain.
func (p *Pool) pick(ctx context.Context) (*Endpoint, error) {
	p.mu.Lock()
	e := p.currentLocked()
	if e == nil {
		p.mu.Unlock()
		return nil, ErrNoEndpoint
	}
	primary := p.endpoints[0]
	probe := e != primary && !p.probing && !primary.Disabled() && !p.now().Before(p.failoverUntil)
	if probe {
		p.probing = true
	}
	p.mu.Unlock()
	if !probe {
		return e, nil
	}
	err := p.check(ctx, primary)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.probing = false
	if err != nil {
		p.failoverUntil = p.now().Add(p.cooldown)
		p.log.Warn("primary endpoint still failing, staying on fallback", "active", p.active, "retryIn", p.cooldown.String(), "err", err.Error())
	} else {
		p.log.Info("primary endpoint recovered, returning to it", "from", p.active)
		p.active = 0
	}
	e = p.currentLocked()
	if e == nil {
		return nil, ErrNoEndpoint
	}
	return e, nil
}

// do runs fn on the active endpoint (verifying it first when needed) and,
// on an EndpointError, fails over and retries on the next one, at most once
// per endpoint. Context cancellation and node answers are returned as is.
func (p *Pool) do(ctx context.Context, fn func(*Endpoint) error) error {
	tried, stale := 0, 0
	for {
		e, err := p.pick(ctx)
		if err != nil {
			return err
		}
		if err = p.verify(ctx, e); err == nil {
			err = fn(e)
		}
		if err == nil || ctx.Err() != nil || !IsEndpointError(err) {
			return err
		}
		if errors.Is(err, ErrStaleEndpoint) {
			// Another caller failed over while this one queued for the send
			// lock: nothing was sent, so pick again rather than fail over.
			// The bound keeps a pathological hand-off from looping.
			stale++
			if stale > len(p.endpoints) {
				return err
			}
			continue
		}
		tried++
		if tried >= len(p.endpoints) || !p.failOver(e, err) {
			return err
		}
	}
}

// call runs a typed request through do.
func call[T any](ctx context.Context, p *Pool, fn func(*Endpoint) (T, error)) (T, error) {
	var out T
	err := p.do(ctx, func(e *Endpoint) error {
		v, err := fn(e)
		out = v
		return err
	})
	return out, err
}

// Call performs a single JSON-RPC call on the active endpoint.
func (p *Pool) Call(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	return call(ctx, p, func(e *Endpoint) (json.RawMessage, error) { return e.Call(ctx, method, params...) })
}

// BlockNumber returns the latest block number.
func (p *Pool) BlockNumber(ctx context.Context) (uint64, error) {
	return call(ctx, p, func(e *Endpoint) (uint64, error) { return e.BlockNumber(ctx) })
}

// ChainID returns the chain id the active endpoint reports.
func (p *Pool) ChainID(ctx context.Context) (uint64, error) {
	return call(ctx, p, func(e *Endpoint) (uint64, error) { return e.ChainID(ctx) })
}

// HeaderByNumber fetches one header.
func (p *Pool) HeaderByNumber(ctx context.Context, number uint64) (*Header, error) {
	return call(ctx, p, func(e *Endpoint) (*Header, error) { return e.HeaderByNumber(ctx, number) })
}

// HeadersByNumbers fetches headers in batches of the endpoint's cap.
func (p *Pool) HeadersByNumbers(ctx context.Context, numbers []uint64) ([]Header, error) {
	return call(ctx, p, func(e *Endpoint) ([]Header, error) { return e.HeadersByNumbers(ctx, numbers) })
}

// BlocksWithTxs fetches full blocks in batches of the endpoint's cap.
func (p *Pool) BlocksWithTxs(ctx context.Context, numbers []uint64) ([]Block, error) {
	return call(ctx, p, func(e *Endpoint) ([]Block, error) { return e.BlocksWithTxs(ctx, numbers) })
}

// OwnerActsLogs fetches OwnerActs events over [from, to].
func (p *Pool) OwnerActsLogs(ctx context.Context, from, to uint64) ([]Log, error) {
	return call(ctx, p, func(e *Endpoint) ([]Log, error) { return e.OwnerActsLogs(ctx, from, to) })
}

// Balance returns an account balance at the latest block.
func (p *Pool) Balance(ctx context.Context, address string) (*big.Int, error) {
	return call(ctx, p, func(e *Endpoint) (*big.Int, error) { return e.Balance(ctx, address) })
}

// FastSample performs the fast tick at the latest block.
func (p *Pool) FastSample(ctx context.Context) (*Sample, error) {
	return call(ctx, p, func(e *Endpoint) (*Sample, error) { return e.FastSample(ctx) })
}

// FastSampleAt performs the fast tick pinned to one block.
func (p *Pool) FastSampleAt(ctx context.Context, number uint64) (*Sample, error) {
	return call(ctx, p, func(e *Endpoint) (*Sample, error) { return e.FastSampleAt(ctx, number) })
}

// L1Sample reads the L1 pricer getters.
func (p *Pool) L1Sample(ctx context.Context) (*L1Sample, error) {
	return call(ctx, p, func(e *Endpoint) (*L1Sample, error) { return e.L1Sample(ctx) })
}

// FeeAccounts reads the fee accounts and their balances.
func (p *Pool) FeeAccounts(ctx context.Context) (*FeeAccounts, error) {
	return call(ctx, p, func(e *Endpoint) (*FeeAccounts, error) { return e.FeeAccounts(ctx) })
}

// ArbOSVersion returns the ArbOS version.
func (p *Pool) ArbOSVersion(ctx context.Context) (uint64, error) {
	return call(ctx, p, func(e *Endpoint) (uint64, error) { return e.ArbOSVersion(ctx) })
}

// Stats aggregates request accounting over every endpoint; the back-off is
// the active endpoint's.
func (p *Pool) Stats() Stats {
	active := p.ActiveEndpoint()
	var out Stats
	for i, e := range p.endpoints {
		s := e.Stats()
		out.CallsLast10s += s.CallsLast10s
		out.RateLimitEvents += s.RateLimitEvents
		if s.Last429At.After(out.Last429At) {
			out.Last429At = s.Last429At
		}
		if i == active {
			out.Backoff = s.Backoff
		}
	}
	return out
}

// Policy is the call policy of the endpoint ordinary calls go to right
// now: its budget in calls per second and whether it has none. Gap
// skipping and the batch-report prefilter follow the active endpoint, not
// the primary's configuration, so a paced primary with an unlimited
// fallback stops skipping gaps while the fallback serves the network.
type Policy struct {
	Unlimited bool
	Rate      float64
}

// Policy returns the active endpoint's policy; a pool with nothing usable
// reports the paced policy, which is the safe one.
func (p *Pool) Policy() Policy {
	p.mu.Lock()
	e := p.currentLocked()
	p.mu.Unlock()
	if e == nil {
		return Policy{}
	}
	return Policy{Unlimited: e.pacer.Unlimited(), Rate: e.pacer.Rate()}
}

// Available returns how many calls the active endpoint can make right now
// without waiting, 0 when nothing is usable.
func (p *Pool) Available() int {
	p.mu.Lock()
	e := p.currentLocked()
	p.mu.Unlock()
	if e == nil {
		return 0
	}
	return e.Available()
}
