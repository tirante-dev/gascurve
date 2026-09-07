package nitro

import (
	"context"
	"errors"
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
	// logRange is the eth_getLogs block range the endpoint is asked for
	// once a refusal has taught it one (0 before, meaning MaxLogRange);
	// lastLogRefusal and lastLogRecovery pace its recovery.
	logRange        uint64
	lastLogRefusal  time.Time
	lastLogRecovery time.Time
	// WebSocket health, tracked apart from HTTP verification: an endpoint
	// whose JSON-RPC answers can still have a socket that cannot be
	// dialed, cannot be subscribed to, or will not stay up. wsUntil is
	// when its socket may be tried again, wsReason says why it was cooled
	// down, wsDrops counts subscriptions lost soon after connecting and
	// wsUpAt is when the last one connected.
	wsUntil  time.Time
	wsReason string
	wsDrops  int
	wsUpAt   time.Time
}

const (
	// wsDropLimit is how many subscriptions may be lost in quick
	// succession before the endpoint's WebSocket is cooled down. A dial or
	// subscribe failure cools it down at once: nothing about it worked.
	wsDropLimit = 3
	// wsHealthyFor is how long a subscription must hold before the drops
	// before it stop counting against the endpoint.
	wsHealthyFor = time.Minute
)

// noteWSConnected records a live subscription on the endpoint.
func (e *Endpoint) noteWSConnected() {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.wsUpAt = now
	e.wsUntil, e.wsReason = time.Time{}, ""
}

// noteWSFailure records a WebSocket failure and reports whether it cooled
// the endpoint's socket down for cooldown. subscribed says whether the
// subscription had been acknowledged before it broke: a dial or subscribe
// failure cools the endpoint down at once, while a subscription that was
// live is only held against it when it keeps dropping soon after
// connecting, so an ordinary reconnect does not move a follower off a
// working endpoint.
func (e *Endpoint) noteWSFailure(subscribed bool, cooldown time.Duration, reason string) bool {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	if subscribed {
		if !e.wsUpAt.IsZero() && now.Sub(e.wsUpAt) >= wsHealthyFor {
			e.wsDrops = 0
		}
		e.wsDrops++
		if e.wsDrops < wsDropLimit {
			return false
		}
	}
	e.wsDrops = 0
	e.wsUntil, e.wsReason = now.Add(cooldown), reason
	return true
}

// wsCooling returns when the endpoint's WebSocket may be tried again and
// why it was cooled down. A zero until means it is usable now.
func (e *Endpoint) wsCooling() (until time.Time, reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.now().Before(e.wsUntil) {
		return e.wsUntil, e.wsReason
	}
	return time.Time{}, ""
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
	opts = append(append([]Option{WithLogger(e.log)}, opts...), withBatchObserver(e.observe), withBatcher(e.batch), withEndpointIndex(index))
	e.Client = NewClient(cfg.RPCURL, cfg.CallsPerSecond, opts...)
	// The endpoint's WebSocket URL is a credential too, and the subscriber
	// reports its failures through this endpoint.
	e.scrub = newScrubber(index, cfg.RPCURL, cfg.WSURL)
	return e
}

// setPreSend installs the check the pool makes under the send lock, so a
// call that selected this endpoint before another caller failed over is
// rejected before it reaches the wire.
func (e *Endpoint) setPreSend(fn func(context.Context) error) { e.preSend = fn }

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
// what the endpoint's token bucket can hold at once for the calling class
// (a budgeted endpoint never sends a batch it has not paid for in full).
// Every typed call goes through it, not only the header batches, so an
// eight-call L1 sample on a four calls per second budget is split rather
// than eating the fast reserve. A throttled chunk is retried after the
// back-off at whatever the cap has become, so an oversized batch shrinks
// instead of being resent as is; the attempt budget is the client's.
func (e *Endpoint) batch(ctx context.Context, reqs []Request) ([]Result, error) {
	out := make([]Result, 0, len(reqs))
	attempts := 0
	class := ClassOf(ctx)
	for start := 0; start < len(reqs); {
		size := chunkSize(min(e.BatchCap(), e.pacer.MaxBatchFor(class)), len(reqs)-start)
		chunk := reqs[start : start+size]
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

// MaxLogRange is the widest block range one eth_getLogs asks for before an
// endpoint has said otherwise, and the ceiling the learned range grows back
// to.
const MaxLogRange = 100_000

// minLogRange is the floor the learned range halves down to. One block is
// the narrowest question there is: an endpoint that refuses it is not
// serving logs at all, which is an endpoint failure rather than a range
// the caller asked too much for.
const minLogRange = 1

// logRefusalBackoff is the pause before re-asking a refused getLogs range
// in narrower pieces. It is paid once per piece, not once per halving: the
// halvings that follow a refusal are a smaller ask rather than the same
// request again, and waiting for each of them would stall a scan for
// minutes. It doubles per consecutive refused piece, up to
// maxLogRefusalBackoff, so an endpoint that keeps refusing is asked less
// often as well as for less.
const (
	logRefusalBackoff    = time.Second
	maxLogRefusalBackoff = 30 * time.Second
)

// LogRange returns the widest eth_getLogs block range the endpoint is
// currently asked for: MaxLogRange until a refusal, then what it has been
// seen to accept, growing back one step per quiet minute.
func (e *Endpoint) LogRange() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.logRangeLocked()
}

func (e *Endpoint) logRangeLocked() uint64 {
	if e.logRange == 0 {
		return MaxLogRange
	}
	return e.logRange
}

// observeLogRange adapts the getLogs range from one call's outcome, the way
// observe adapts the batch cap: a refused range halves the width (floor
// minLogRange) and stamps the refusal; an accepted call after a quiet
// capRecoveryInterval doubles it again (ceiling MaxLogRange), so the
// endpoint's real limit is found without being told, and probed again now
// and then in case it moved. The next call reads the new width with
// LogRange.
func (e *Endpoint) observeLogRange(width uint64, refused bool) {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	if refused {
		e.lastLogRefusal = now
		cur := min(e.logRangeLocked(), width)
		e.logRange = max(cur/2, minLogRange)
		e.log.Warn("eth_getLogs range refused, halving", "refused", width, "range", e.logRange)
		return
	}
	if e.logRange == 0 || e.logRange >= MaxLogRange {
		return
	}
	since := e.lastLogRefusal
	if e.lastLogRecovery.After(since) {
		since = e.lastLogRecovery
	}
	if now.Sub(since) < capRecoveryInterval {
		return
	}
	e.lastLogRecovery = now
	e.logRange = min(e.logRange*2, MaxLogRange)
	e.log.Info("eth_getLogs range recovered one step", "range", e.logRange)
}

// logsRefused reports whether err is the endpoint declining the request it
// was given rather than failing to answer: a JSON-RPC error of any wording
// (providers cap getLogs by block range, by result count or by response
// size and each says so differently), or a rate limit that outlasted the
// client's own back-off. A transport or context failure is not a refusal
// and goes back to the caller, whose pool decides about failover.
func logsRefused(err error) bool {
	var rpcErr *RPCError
	return errors.As(err, &rpcErr) || errors.Is(err, ErrRateLimited)
}

// OwnerActsLogs fetches OwnerActs events over [from, to] in pieces of at
// most the endpoint's getLogs range. A refused piece wider than one block
// narrows the range and is re-asked in smaller pieces; the accepted width
// is kept for later calls and probed upward again after a quiet minute.
// Logs come back in block order, as one call would return them.
//
// A refusal of a single block is not a range the caller asked too much
// for: this endpoint cannot answer the question at all, so it comes back
// as an EndpointError and the pool tries the next endpoint. The provider's
// own error stays underneath it, so the last endpoint's refusal still
// surfaces as the JSON-RPC error it was.
//
// The back-off is paid at most once per piece and honors the context, so
// narrowing a range from a hundred thousand blocks to one costs seventeen
// calls rather than seventeen waits.
func (e *Endpoint) OwnerActsLogs(ctx context.Context, from, to uint64) ([]Log, error) {
	fetch := func(a, b uint64) ([]Log, error) { return e.Client.OwnerActsLogs(ctx, a, b) }
	var out []Log
	backoff := logRefusalBackoff
	waited := false
	for start := from; start <= to; {
		width := e.LogRange()
		end := start + width - 1
		if end > to || end < start {
			end = to
		}
		logs, err := fetch(start, end)
		if err == nil {
			out = append(out, logs...)
			e.observeLogRange(end-start+1, false)
			if end == to {
				break
			}
			start = end + 1
			if !waited {
				// A piece taken at the first ask clears the back-off; one
				// that had to be narrowed keeps the longer pause for the
				// next piece.
				backoff = logRefusalBackoff
			}
			waited = false
			continue
		}
		if ctx.Err() != nil || !logsRefused(err) {
			return nil, err
		}
		asked := end - start + 1
		if asked <= 1 {
			return nil, &EndpointError{Err: e.scrub.errorf("endpoint %d: eth_getLogs refused a single block %d: %w", e.index, start, err)}
		}
		e.observeLogRange(asked, true)
		if waited {
			continue
		}
		if err := e.sleep(ctx, backoff); err != nil {
			return nil, err
		}
		waited = true
		backoff = min(backoff*2, maxLogRefusalBackoff)
	}
	return out, nil
}
