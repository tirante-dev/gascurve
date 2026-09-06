package nitro

import (
	"context"
	"sync"
	"time"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/logger"
)

const (
	// minBatchCap is the floor the adaptive batch cap halves down to.
	minBatchCap = 10
	// capRecoveryInterval is how long an endpoint must answer without a
	// 429 before its batch cap grows one step (doubles) again.
	capRecoveryInterval = time.Minute
)

// Endpoint is one JSON-RPC endpoint of a network: a Client with its own
// token bucket, its capabilities (WebSocket URL, archive state) and an
// adaptive batch cap. The cap starts at the configured header batch size,
// halves (floor 10) whenever the endpoint answers a batch with 429 and
// recovers one step per successful minute. Chain id verification can
// disable an endpoint for good.
type Endpoint struct {
	*Client
	index   int
	wsURL   string
	archive bool
	log     *logger.Logger
	now     func() time.Time

	mu           sync.Mutex
	batchCap     int
	maxBatchCap  int
	minBatchCap  int
	lastLimit    time.Time
	lastRecovery time.Time
	verified     bool
	disabled     bool
	reason       string
}

// newEndpoint builds an endpoint at index for cfg with a starting batch cap
// of batchSize. opts customize the client (the observer is always set).
func newEndpoint(index int, cfg config.EndpointConfig, batchSize int, log *logger.Logger, now func() time.Time, opts ...Option) *Endpoint {
	if batchSize <= 0 || batchSize > MaxBatch {
		batchSize = MaxBatch
	}
	e := &Endpoint{
		index:       index,
		wsURL:       cfg.WSURL,
		archive:     cfg.Archive,
		log:         log.With("endpoint", index),
		now:         now,
		batchCap:    batchSize,
		maxBatchCap: batchSize,
		minBatchCap: min(minBatchCap, batchSize),
	}
	opts = append(append([]Option{WithLogger(e.log)}, opts...), withBatchObserver(e.observe))
	e.Client = NewClient(cfg.RPCURL, cfg.CallsPerSecond, opts...)
	return e
}

// Index is the endpoint's position: 0 is the primary.
func (e *Endpoint) Index() int { return e.index }

// WSURL returns the newHeads endpoint, "" when the endpoint has none.
func (e *Endpoint) WSURL() string { return e.wsURL }

// Archive reports whether the endpoint serves historical state.
func (e *Endpoint) Archive() bool { return e.archive }

// BatchCap returns the current adaptive batch cap.
func (e *Endpoint) BatchCap() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.batchCap
}

// Disabled reports whether verification disabled the endpoint.
func (e *Endpoint) Disabled() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.disabled
}

// Verified reports whether eth_chainId has confirmed the endpoint's chain.
func (e *Endpoint) Verified() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.verified
}

// Reason returns why the endpoint was disabled, "" while it is usable.
func (e *Endpoint) Reason() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reason
}

func (e *Endpoint) setVerified() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.verified = true
}

func (e *Endpoint) disable(reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.disabled, e.reason = true, reason
}

// observe adapts the batch cap from a batch attempt's outcome.
func (e *Endpoint) observe(items int, limited bool) {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	if limited {
		e.lastLimit = now
		if items > 1 && e.batchCap > e.minBatchCap {
			e.batchCap = max(e.batchCap/2, e.minBatchCap)
			e.log.Warn("batch answered with 429, batch cap halved", "batchCap", e.batchCap, "items", items)
		}
		return
	}
	if e.batchCap >= e.maxBatchCap {
		return
	}
	since := e.lastLimit
	if e.lastRecovery.After(since) {
		since = e.lastRecovery
	}
	if now.Sub(since) < capRecoveryInterval {
		return
	}
	e.batchCap = min(e.batchCap*2, e.maxBatchCap)
	e.lastRecovery = now
	e.log.Info("batch cap recovered one step", "batchCap", e.batchCap)
}

// batch sends reqs in chunks of at most the current cap, itself bounded by
// what the endpoint's token bucket can hold at once (a budgeted endpoint
// never sends a batch it has not paid for in full). A throttled chunk is
// retried after the back-off at whatever the cap has become, so an
// oversized batch shrinks instead of being resent as is; the attempt
// budget is the client's.
func (e *Endpoint) batch(ctx context.Context, reqs []Request) ([]Result, error) {
	out := make([]Result, 0, len(reqs))
	attempts := 0
	for start := 0; start < len(reqs); {
		size := min(e.BatchCap(), e.pacer.MaxBatch())
		chunk := reqs[start:min(start+size, len(reqs))]
		results, limited, err := e.attempt(ctx, chunk)
		if err != nil {
			return nil, err
		}
		if limited {
			attempts++
			if attempts >= e.maxAttempts {
				return nil, throttled(attempts)
			}
			continue
		}
		out = append(out, results...)
		start += len(chunk)
	}
	return out, nil
}

// HeadersByNumbers fetches headers in batches of at most the adaptive cap.
func (e *Endpoint) HeadersByNumbers(ctx context.Context, numbers []uint64) ([]Header, error) {
	blocks, err := blocksByNumbers(ctx, numbers, false, e.batch)
	if err != nil {
		return nil, err
	}
	out := make([]Header, len(blocks))
	for i := range blocks {
		out[i] = blocks[i].Header
	}
	return out, nil
}

// BlocksWithTxs fetches full blocks in batches of at most the adaptive cap.
func (e *Endpoint) BlocksWithTxs(ctx context.Context, numbers []uint64) ([]Block, error) {
	return blocksByNumbers(ctx, numbers, true, e.batch)
}
