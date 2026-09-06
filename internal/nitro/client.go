package nitro

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/tirante-dev/gascurve/internal/logger"
	"github.com/tirante-dev/gascurve/internal/version"
)

const (
	// jsonrpcVersion is the protocol version sent in every request.
	jsonrpcVersion = "2.0"
	// MaxBatch is the largest number of items sent in one HTTP batch request.
	MaxBatch = 100
	// RateLimitCode is the JSON-RPC error code some endpoints use for throttling.
	RateLimitCode = 429

	minBackoff = 2 * time.Second
	maxBackoff = 60 * time.Second
	callWindow = 10 * time.Second
)

// ErrRateLimited is returned when the endpoint kept throttling after all
// retry attempts.
var ErrRateLimited = errors.New("rpc: rate limited")

// EndpointError marks a failure of the endpoint itself rather than an
// answer from the node: a transport error, an HTTP 5xx, throttling that
// outlasted the back-off, or a chain id mismatch. A Pool fails over on
// these; JSON-RPC errors (reverts, unknown methods, missing blocks) never
// carry it.
type EndpointError struct {
	Err error
}

func (e *EndpointError) Error() string { return e.Err.Error() }

// Unwrap exposes the underlying error to errors.Is and errors.As.
func (e *EndpointError) Unwrap() error { return e.Err }

// IsEndpointError reports whether err is, or wraps, an EndpointError.
func IsEndpointError(err error) bool {
	var ee *EndpointError
	return errors.As(err, &ee)
}

// endpointErrorf wraps a formatted error as an EndpointError.
func endpointErrorf(format string, args ...any) error {
	return &EndpointError{Err: fmt.Errorf(format, args...)}
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// IsRevert reports whether err is an eth_call execution revert: the
// standard revert code 3, an explicit revert message, or revert data
// carrying an Error(string) or Panic(uint256) payload. Generic server codes
// such as -32000 are not reverts by themselves: nodes use them for missing
// state, unavailable headers and proxy failures.
func IsRevert(err error) bool {
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		return false
	}
	return rpcErr.Code == 3 || containsFold(rpcErr.Message, "revert") || isRevertData(rpcErr.Data)
}

// Revert payload selectors: Error(string) and Panic(uint256).
var (
	revertErrorSelector = Selector("Error(string)")
	revertPanicSelector = Selector("Panic(uint256)")
)

// isRevertData reports whether a JSON-RPC error data member is valid revert
// data: a hex string whose first four bytes are the Error or Panic selector.
func isRevertData(data json.RawMessage) bool {
	if len(data) == 0 {
		return false
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		var obj struct {
			Data string `json:"data"`
		}
		if err := json.Unmarshal(data, &obj); err != nil || obj.Data == "" {
			return false
		}
		s = obj.Data
	}
	b, err := DecodeHex(s)
	if err != nil || len(b) < 4 {
		return false
	}
	var sel [4]byte
	copy(sel[:], b[:4])
	return sel == revertErrorSelector || sel == revertPanicSelector
}

// Request is one JSON-RPC call.
type Request struct {
	Method string
	Params []any
}

// Result is one JSON-RPC batch item result.
type Result struct {
	Raw json.RawMessage
	Err error
}

// Stats is a point in time view of the client's request accounting.
type Stats struct {
	CallsLast10s    int
	RateLimitEvents uint64
	Last429At       time.Time
	Backoff         time.Duration
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      uint64 `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcResponse struct {
	ID     uint64          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *RPCError       `json:"error"`
}

// Client is a paced JSON-RPC client for one network.
type Client struct {
	url         string
	httpClient  *http.Client
	pacer       *Pacer
	userAgent   string
	log         *logger.Logger
	sleep       func(context.Context, time.Duration) error
	now         func() time.Time
	maxAttempts int
	// observe, when set, sees every batch attempt: how many items it
	// carried and whether the endpoint answered 429. An Endpoint adapts
	// its batch cap from it.
	observe func(items int, limited bool)

	sendMu sync.Mutex // one in-flight HTTP request per network

	mu              sync.Mutex
	nextID          uint64
	callTimes       []time.Time
	rateLimitEvents uint64
	last429         time.Time
	backoff         time.Duration
	// blockedUntil is the network-wide cooldown after a 429: no request is
	// sent before it, whichever caller holds the send lock.
	blockedUntil time.Time
}

// Option customizes a Client.
type Option func(*Client)

// WithHTTPClient sets the HTTP client.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.httpClient = h } }

// WithPacer sets the token bucket.
func WithPacer(p *Pacer) Option { return func(c *Client) { c.pacer = p } }

// WithLogger sets the logger.
func WithLogger(l *logger.Logger) Option { return func(c *Client) { c.log = l } }

// WithMaxAttempts sets how many times a throttled request is retried.
func WithMaxAttempts(n int) Option { return func(c *Client) { c.maxAttempts = n } }

// withBatchObserver reports every batch attempt's item count and whether
// it was throttled.
func withBatchObserver(fn func(items int, limited bool)) Option {
	return func(c *Client) { c.observe = fn }
}

// withClock replaces the clock and sleeper, for tests.
func withClock(now func() time.Time, sleep func(context.Context, time.Duration) error) Option {
	return func(c *Client) {
		c.now = now
		c.sleep = sleep
	}
}

// NewClient creates a client for url with a callsPerSecond budget.
func NewClient(url string, callsPerSecond float64, opts ...Option) *Client {
	c := &Client{
		url:         url,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
		userAgent:   version.UserAgent(),
		log:         logger.Nop(),
		sleep:       sleepContext,
		now:         time.Now,
		maxAttempts: 5,
		backoff:     minBackoff,
	}
	for _, o := range opts {
		o(c)
	}
	if c.pacer == nil {
		c.pacer = NewPacer(callsPerSecond)
	}
	if c.pacer.now == nil {
		c.pacer.now = c.now
	}
	return c
}

// Pacer returns the client's token bucket.
func (c *Client) Pacer() *Pacer { return c.pacer }

// Stats returns request accounting.
func (c *Client) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.trimCallsLocked(c.now())
	return Stats{CallsLast10s: len(c.callTimes), RateLimitEvents: c.rateLimitEvents, Last429At: c.last429, Backoff: c.backoff}
}

func (c *Client) trimCallsLocked(now time.Time) {
	cut := now.Add(-callWindow)
	i := 0
	for i < len(c.callTimes) && c.callTimes[i].Before(cut) {
		i++
	}
	c.callTimes = c.callTimes[i:]
}

func (c *Client) recordCalls(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.trimCallsLocked(now)
	for i := 0; i < n; i++ {
		c.callTimes = append(c.callTimes, now)
	}
}

func (c *Client) ids(n int) []uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]uint64, n)
	for i := range out {
		c.nextID++
		out[i] = c.nextID
	}
	return out
}

// Call performs a single JSON-RPC call.
func (c *Client) Call(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	if params == nil {
		params = []any{}
	}
	results, err := c.Batch(ctx, []Request{{Method: method, Params: params}})
	if err != nil {
		return nil, err
	}
	return results[0].Raw, results[0].Err
}

// Batch sends up to MaxBatch calls in one HTTP request and returns one
// Result per request, in order. Throttling (HTTP 429 or a JSON-RPC 429 on
// any item) retries the whole batch with exponential back-off. Every
// attempt pays for its calls at the pacer and honors the network-wide
// cooldown, so a retry can never exceed the budget or race other callers.
func (c *Client) Batch(ctx context.Context, reqs []Request) ([]Result, error) {
	if len(reqs) == 0 {
		return nil, nil
	}
	if len(reqs) > MaxBatch {
		return nil, fmt.Errorf("rpc: batch of %d exceeds %d items", len(reqs), MaxBatch)
	}
	for attempt := 1; ; attempt++ {
		results, limited, err := c.attempt(ctx, reqs)
		if err != nil {
			return nil, err
		}
		if !limited {
			return results, nil
		}
		if attempt >= c.maxAttempts {
			return nil, throttled(attempt)
		}
	}
}

// throttled is the error for a request that stayed rate limited after
// attempts tries.
func throttled(attempts int) error {
	return endpointErrorf("%w after %d attempts", ErrRateLimited, attempts)
}

// attempt sends reqs once: it pays the pacer, waits out any cooldown, posts
// the batch and reports whether the endpoint throttled it (after recording
// the 429 and starting the back-off) or answered (after resetting the
// back-off). The results are in request order.
func (c *Client) attempt(ctx context.Context, reqs []Request) (results []Result, limited bool, err error) {
	ids := c.ids(len(reqs))
	body := make([]rpcRequest, len(reqs))
	for i, r := range reqs {
		params := r.Params
		if params == nil {
			params = []any{}
		}
		body[i] = rpcRequest{JSONRPC: jsonrpcVersion, ID: ids[i], Method: r.Method, Params: params}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, false, fmt.Errorf("rpc: encode batch: %w", err)
	}
	if err := c.pacer.Wait(ctx, len(reqs)); err != nil {
		return nil, false, err
	}
	responses, limited, sentAt, err := c.send(ctx, payload, len(reqs))
	if err != nil {
		return nil, false, err
	}
	if c.observe != nil {
		c.observe(len(reqs), limited)
	}
	if limited {
		return nil, true, nil
	}
	c.resetBackoff(sentAt)
	return matchResults(ids, responses), false, nil
}

// cooldown returns how long the network-wide 429 cooldown still has to run.
func (c *Client) cooldown() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.blockedUntil.Sub(c.now())
}

// send performs one HTTP round trip under the per-endpoint send lock,
// sleeping through any active cooldown first so every caller respects a
// 429 seen by any other. limited is true on HTTP 429 or when any item
// carries a JSON-RPC 429 error; the cooldown it starts is published before
// the lock is released, so a caller waiting for the lock observes it
// instead of sending into the throttle. sentAt is when the request left.
func (c *Client) send(ctx context.Context, payload []byte, calls int) (responses []rpcResponse, limited bool, sentAt time.Time, err error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if wait := c.cooldown(); wait > 0 {
		if err := c.sleep(ctx, wait); err != nil {
			return nil, false, time.Time{}, err
		}
	}
	c.recordCalls(calls)
	sentAt = c.now()
	responses, limited, err = c.post(ctx, payload)
	if limited {
		c.noteRateLimit()
	}
	return responses, limited, sentAt, err
}

// post performs the HTTP request itself.
func (c *Client) post(ctx context.Context, payload []byte) (responses []rpcResponse, limited bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return nil, false, fmt.Errorf("rpc: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, false, endpointErrorf("rpc: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, true, nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, false, endpointErrorf("rpc: read response: %w", err)
	}
	if resp.StatusCode/100 == 5 {
		return nil, false, endpointErrorf("rpc: http %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if resp.StatusCode/100 != 2 {
		return nil, false, fmt.Errorf("rpc: http %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if err := json.Unmarshal(data, &responses); err != nil {
		// Some endpoints answer a batch with a single error object.
		var single rpcResponse
		if err2 := json.Unmarshal(data, &single); err2 != nil {
			return nil, false, fmt.Errorf("rpc: decode response: %w", err)
		}
		if single.Error != nil && single.Error.Code == RateLimitCode {
			return nil, true, nil
		}
		if single.Error != nil {
			return nil, false, fmt.Errorf("rpc: %w", single.Error)
		}
		responses = []rpcResponse{single}
	}
	for _, r := range responses {
		if r.Error != nil && r.Error.Code == RateLimitCode {
			return nil, true, nil
		}
	}
	return responses, false, nil
}

func matchResults(ids []uint64, responses []rpcResponse) []Result {
	byID := make(map[uint64]rpcResponse, len(responses))
	for _, r := range responses {
		byID[r.ID] = r
	}
	out := make([]Result, len(ids))
	for i, id := range ids {
		r, ok := byID[id]
		switch {
		case !ok:
			out[i].Err = fmt.Errorf("rpc: missing response for id %d", id)
		case r.Error != nil:
			out[i].Err = r.Error
		default:
			out[i].Raw = r.Result
		}
	}
	return out
}

// noteRateLimit records a 429, starts the network-wide cooldown for the
// current back-off and doubles it for the next one.
func (c *Client) noteRateLimit() {
	c.mu.Lock()
	now := c.now()
	c.trimCallsLocked(now)
	c.rateLimitEvents++
	c.last429 = now
	wait := c.backoff
	c.blockedUntil = now.Add(wait)
	c.backoff *= 2
	if c.backoff > maxBackoff {
		c.backoff = maxBackoff
	}
	recent := len(c.callTimes)
	c.mu.Unlock()
	c.log.Warn("rpc rate limited", "backoff", wait.String(), "callsLast10s", recent)
}

// resetBackoff returns the back-off to its minimum after a request that was
// sent past the cooldown succeeded. A success sent before a cooldown that
// another caller started meanwhile proves nothing and keeps the back-off.
func (c *Client) resetBackoff(sentAt time.Time) {
	c.mu.Lock()
	if !sentAt.Before(c.blockedUntil) {
		c.backoff = minBackoff
	}
	c.mu.Unlock()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func containsFold(s, sub string) bool {
	return bytes.Contains(bytes.ToLower([]byte(s)), bytes.ToLower([]byte(sub)))
}

// Available returns how many calls can be made right now without waiting:
// zero during a 429 cooldown, otherwise the pacer's spare tokens.
func (c *Client) Available() int {
	if c.cooldown() > 0 {
		return 0
	}
	return c.pacer.Available()
}
