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
// included: they can carry keys.
type EndpointStatus struct {
	Index    int
	WS       bool
	Archive  bool
	Disabled bool
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

	mu            sync.Mutex
	active        int
	failoverUntil time.Time
	probing       bool
	failovers     uint64
}

// NewPool builds the endpoints of cfg. Nothing is contacted until Verify
// or the first call.
func NewPool(cfg PoolConfig, opts ...PoolOption) *Pool {
	p := &Pool{chainID: cfg.ChainID, cooldown: cfg.Cooldown, log: logger.Nop()}
	for _, o := range opts {
		o(p)
	}
	if p.cooldown <= 0 {
		p.cooldown = defaultFailoverCooldown
	}
	for i, ec := range cfg.Endpoints {
		p.endpoints = append(p.endpoints, p.newEndpoint(i, ec, cfg.BatchSize))
	}
	return p
}

func (p *Pool) newEndpoint(i int, ec config.EndpointConfig, batchSize int) *Endpoint {
	now := p.now
	if now == nil {
		now = time.Now
	}
	opts := append([]Option(nil), p.clientOpts...)
	if p.now != nil {
		opts = append(opts, WithPacer(NewPacer(ec.CallsPerSecond).withClock(p.now, p.sleep)), withClock(p.now, p.sleep))
	}
	return newEndpoint(i, ec, batchSize, p.log, now, opts...)
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
		st.Endpoints[i] = EndpointStatus{Index: i, WS: e.wsURL != "", Archive: e.archive, Disabled: e.Disabled()}
	}
	return st
}

// WS returns the first usable endpoint with a WebSocket URL, or nil.
func (p *Pool) WS() *Endpoint {
	for _, e := range p.endpoints {
		if e.wsURL != "" && !e.Disabled() {
			return e
		}
	}
	return nil
}

// Archive returns the first usable endpoint serving historical state, or
// nil.
func (p *Pool) Archive() *Endpoint {
	for _, e := range p.endpoints {
		if e.archive && !e.Disabled() {
			return e
		}
	}
	return nil
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
	id, err := e.ChainID(ctx)
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
	for attempt := 1; ; attempt++ {
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
		if attempt >= len(p.endpoints) || !p.failOver(e, err) {
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
