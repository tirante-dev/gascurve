package nitro

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/tirante-dev/gascurve/internal/logger"
	"github.com/tirante-dev/gascurve/internal/version"
)

const (
	jsonrpcVersion = "2.0"
	// MaxBatch is the largest number of items sent in one HTTP batch request.
	MaxBatch = 100
	// RateLimitCode is the JSON-RPC error code some endpoints use for throttling.
	RateLimitCode = 429
	// RateLimitCodeExceeded is the code Infura and Alchemy answer with over their request limit.
	RateLimitCodeExceeded = -32005
	// RateLimitCodeQuickNode is the code QuickNode answers with at its per-second limit.
	RateLimitCodeQuickNode = -32007

	minBackoff = 2 * time.Second
	maxBackoff = 60 * time.Second
	callWindow = 10 * time.Second
)

// ErrRateLimited is returned when the endpoint kept throttling after all retry attempts.
var ErrRateLimited = errors.New("rpc: rate limited")

// ErrStaleEndpoint is returned when another caller failed over while this one queued for the send
// lock. Nothing was sent and the tokens were refunded, so the pool picks again and retries.
var ErrStaleEndpoint = errors.New("rpc: endpoint no longer active")

// rateLimitMessage matches the messages providers put on a throttling error whatever code they use.
var rateLimitMessage = regexp.MustCompile(`(?i)rate limit|request limit|too many requests|limit reached|limit exceeded`)

// IsRateLimit reports whether a JSON-RPC error is throttling rather than an answer: code 429,
// -32005 or -32007, or a message naming a rate or request limit. Handled like an HTTP 429.
func IsRateLimit(e *RPCError) bool {
	if e == nil {
		return false
	}
	switch e.Code {
	case RateLimitCode, RateLimitCodeExceeded, RateLimitCodeQuickNode:
		return true
	}
	return rateLimitMessage.MatchString(e.Message)
}

// EndpointError marks a failure of the endpoint itself rather than an answer from the node: a
// transport error, an HTTP 5xx, throttling past the back-off, or a chain id mismatch. A Pool fails
// over on these; JSON-RPC errors never carry it.
type EndpointError struct {
	Err error
}

func (e *EndpointError) Error() string { return e.Err.Error() }

func (e *EndpointError) Unwrap() error { return e.Err }

// IsEndpointError reports whether err is, or wraps, an EndpointError.
func IsEndpointError(err error) bool {
	var ee *EndpointError
	return errors.As(err, &ee)
}

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

// IsRevert reports whether err is an eth_call execution revert: code 3, an explicit revert message,
// or revert data carrying an Error(string) or Panic(uint256) payload. Generic server codes such as
// -32000 are not reverts: nodes use them for missing state and proxy failures too.
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

// isRevertData reports whether a JSON-RPC error data member is a hex string whose first four bytes
// are the Error or Panic selector.
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
	Calls           uint64
	Requests        uint64
	Errors          uint64
	TotalLatency    time.Duration
	RateLimitEvents uint64
	Last429At       time.Time
	Backoff         time.Duration
	// FastCalls and BulkCalls count every JSON-RPC call sent, by pacer class, one per batch item.
	// They only ever grow, so an observer can report them as counters.
	FastCalls uint64
	BulkCalls uint64
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
	url string
	// index names the endpoint in sanitized errors; scrub rewrites every error that could quote the
	// URL, which is a credential.
	index       int
	scrub       *scrubber
	httpClient  *http.Client
	pacer       *Pacer
	userAgent   string
	log         *logger.Logger
	sleep       func(context.Context, time.Duration) error
	now         func() time.Time
	maxAttempts int
	// observe, when set, sees every batch attempt: its item count and whether the endpoint answered
	// 429. An Endpoint adapts its batch cap from it.
	observe func(items int, limited bool)
	// chunk splits a request list of any length into HTTP batches. batchCapped by default; an
	// Endpoint replaces it with one that also honors the adaptive batch cap, so every typed call is
	// chunked the same way the header batches are.
	chunk batcher
	// preSend, when set, is the last check before the request leaves, under the send lock: it rejects
	// a call whose endpoint is no longer the pool's active one.
	preSend func(context.Context) error

	sendMu sync.Mutex // one in-flight HTTP request per network

	mu        sync.Mutex
	nextID    uint64
	callTimes []time.Time
	// Cumulative call counts per pacer class, kept for observation only: nothing routes on them.
	fastCalls       uint64
	bulkCalls       uint64
	calls           uint64
	requests        uint64
	errors          uint64
	totalLatency    time.Duration
	rateLimitEvents uint64
	last429         time.Time
	backoff         time.Duration
	// blockedUntil is the network-wide cooldown after a 429: no request is
	// sent before it, whichever caller holds the send lock.
	blockedUntil time.Time
}

// Option customizes a Client.
type Option func(*Client)

func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.httpClient = h } }

func WithPacer(p *Pacer) Option { return func(c *Client) { c.pacer = p } }

func WithLogger(l *logger.Logger) Option { return func(c *Client) { c.log = l } }

func WithMaxAttempts(n int) Option { return func(c *Client) { c.maxAttempts = n } }

// withBatchObserver reports every batch attempt's item count and whether it was throttled.
func withBatchObserver(fn func(items int, limited bool)) Option {
	return func(c *Client) { c.observe = fn }
}

// withBatcher replaces how a request list is split into HTTP batches.
func withBatcher(fn batcher) Option {
	return func(c *Client) { c.chunk = fn }
}

// withEndpointIndex names the endpoint in sanitized errors.
func withEndpointIndex(i int) Option { return func(c *Client) { c.index = i } }

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
	if c.chunk == nil {
		c.chunk = c.batchCapped
	}
	// After the options: the index names the endpoint in every message the URL is taken out of.
	c.scrub = newScrubber(c.index, c.url)
	return c
}

func (c *Client) Pacer() *Pacer { return c.pacer }

func (c *Client) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.trimCallsLocked(c.now())
	return Stats{
		CallsLast10s: len(c.callTimes), Calls: c.calls, Requests: c.requests, Errors: c.errors,
		TotalLatency: c.totalLatency, RateLimitEvents: c.rateLimitEvents, Last429At: c.last429, Backoff: c.backoff,
		FastCalls: c.fastCalls, BulkCalls: c.bulkCalls,
	}
}

func (c *Client) trimCallsLocked(now time.Time) {
	cut := now.Add(-callWindow)
	i := 0
	for i < len(c.callTimes) && c.callTimes[i].Before(cut) {
		i++
	}
	c.callTimes = c.callTimes[i:]
}

func (c *Client) recordCalls(class Class, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.trimCallsLocked(now)
	for i := 0; i < n; i++ {
		c.callTimes = append(c.callTimes, now)
	}
	if class == Fast {
		c.fastCalls += uint64(n)
	} else {
		c.bulkCalls += uint64(n)
	}
	c.calls += uint64(n)
}

func (c *Client) recordRequest(latency time.Duration, failed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests++
	c.totalLatency += max(latency, 0)
	if failed {
		c.errors++
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

// Batch sends up to MaxBatch calls in one HTTP request and returns one Result per request, in
// order. Throttling retries the whole batch with exponential back-off. Every attempt pays for its
// calls at the pacer, in the lane the context's Class selects, and honors the network-wide
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
			c.recordResultErrors(results)
			return results, nil
		}
		if attempt >= c.maxAttempts {
			return nil, throttled(attempt)
		}
	}
}

func (c *Client) recordResultErrors(results []Result) {
	var failures uint64
	for _, result := range results {
		if result.Err != nil {
			failures++
		}
	}
	if failures == 0 {
		return
	}
	c.mu.Lock()
	c.errors += failures
	c.mu.Unlock()
}

// throttled is the error for a request that stayed rate limited after attempts tries.
func throttled(attempts int) error {
	return endpointErrorf("%w after %d attempts", ErrRateLimited, attempts)
}

// attempt sends reqs once: it pays the pacer, waits out any cooldown, posts the batch and reports
// whether the endpoint throttled it or answered, updating the back-off either way.
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

// send performs one HTTP round trip under the per-endpoint send lock. The tokens were taken before
// the caller queued for that lock, so the reservation is revalidated once it holds it: a cooldown
// another caller started meanwhile refunds the tokens, waits it out and pays again, which is what
// stops a queue of waiters bursting through the moment the lock opens. preSend then rejects a call
// whose endpoint is no longer active. A cooldown this call starts is published before the lock is
// released, so a waiter observes it instead of sending into the throttle.
func (c *Client) send(ctx context.Context, payload []byte, calls int) (responses []rpcResponse, limited bool, sentAt time.Time, err error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	for {
		wait := c.cooldown()
		if wait <= 0 {
			break
		}
		c.pacer.Refund(calls)
		if err := c.sleep(ctx, wait); err != nil {
			return nil, false, time.Time{}, err
		}
		if err := c.pacer.Wait(ctx, calls); err != nil {
			return nil, false, time.Time{}, err
		}
	}
	if c.preSend != nil {
		if err := c.preSend(ctx); err != nil {
			c.pacer.Refund(calls)
			return nil, false, time.Time{}, err
		}
	}
	c.recordCalls(ClassOf(ctx), calls)
	sentAt = c.now()
	responses, limited, err = c.post(ctx, payload)
	c.recordRequest(c.now().Sub(sentAt), limited || err != nil)
	if limited {
		c.noteRateLimit()
	}
	return responses, limited, sentAt, err
}

// isEndpointStatus reports whether an HTTP status is the endpoint failing rather than the node
// answering. A 401 or 403 means this endpoint is misconfigured or its key is rejected, which the
// next one may not be, so it drives failover like a 5xx. Every other non-2xx status says something
// about the request, so retrying it elsewhere would only repeat it.
func isEndpointStatus(code int) bool {
	return code/100 == 5 || code == http.StatusUnauthorized || code == http.StatusForbidden
}

// post performs the HTTP request itself. Every error it returns is sanitized: transport failures
// come back as *url.Error carrying the whole URL, and a provider is free to quote the request URL
// in its response body too.
func (c *Client) post(ctx context.Context, payload []byte) (responses []rpcResponse, limited bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return nil, false, c.scrub.errorf("rpc: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, false, &EndpointError{Err: c.scrub.wrap(err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, true, nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, false, &EndpointError{Err: c.scrub.errorf("rpc: read response: %w", err)}
	}
	body := c.scrub.text(truncate(string(data), 200))
	if isEndpointStatus(resp.StatusCode) {
		return nil, false, endpointErrorf("rpc: %s: http %d: %s", c.scrub.name, resp.StatusCode, body)
	}
	if resp.StatusCode/100 != 2 {
		return nil, false, fmt.Errorf("rpc: %s: http %d: %s", c.scrub.name, resp.StatusCode, body)
	}
	if err := json.Unmarshal(data, &responses); err != nil {
		// Some endpoints answer a batch with a single error object.
		var single rpcResponse
		if err2 := json.Unmarshal(data, &single); err2 != nil {
			return nil, false, c.scrub.errorf("rpc: decode response: %w", err)
		}
		single.Error = c.scrub.rpcError(single.Error)
		if IsRateLimit(single.Error) {
			return nil, true, nil
		}
		if single.Error != nil {
			return nil, false, fmt.Errorf("rpc: %w", single.Error)
		}
		responses = []rpcResponse{single}
	}
	for i := range responses {
		responses[i].Error = c.scrub.rpcError(responses[i].Error)
		if IsRateLimit(responses[i].Error) {
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

// noteRateLimit records a 429, starts the network-wide cooldown and doubles the back-off.
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

// resetBackoff returns the back-off to its minimum after a request sent past the cooldown
// succeeded. One sent before a cooldown another caller started meanwhile proves nothing.
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

// Available returns the calls that can be made right now without waiting: zero during a 429
// cooldown, otherwise the pacer's spare tokens.
func (c *Client) Available() int {
	if c.cooldown() > 0 {
		return 0
	}
	return c.pacer.Available()
}
