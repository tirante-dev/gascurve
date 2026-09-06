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

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// IsRevert reports whether err is an eth_call execution revert.
func IsRevert(err error) bool {
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		return false
	}
	return rpcErr.Code == 3 || rpcErr.Code == -32000 || rpcErr.Code == -32015 || containsFold(rpcErr.Message, "revert")
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

	sendMu sync.Mutex // one in-flight HTTP request per network

	mu              sync.Mutex
	nextID          uint64
	callTimes       []time.Time
	rateLimitEvents uint64
	last429         time.Time
	backoff         time.Duration
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
// any item) retries the whole batch with exponential back-off.
func (c *Client) Batch(ctx context.Context, reqs []Request) ([]Result, error) {
	if len(reqs) == 0 {
		return nil, nil
	}
	if len(reqs) > MaxBatch {
		return nil, fmt.Errorf("rpc: batch of %d exceeds %d items", len(reqs), MaxBatch)
	}
	if err := c.pacer.Wait(ctx, len(reqs)); err != nil {
		return nil, err
	}
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
		return nil, fmt.Errorf("rpc: encode batch: %w", err)
	}

	for attempt := 1; ; attempt++ {
		c.recordCalls(len(reqs))
		responses, limited, err := c.send(ctx, payload)
		if err != nil {
			return nil, err
		}
		if !limited {
			c.resetBackoff()
			return matchResults(ids, responses), nil
		}
		wait := c.noteRateLimit()
		if attempt >= c.maxAttempts {
			return nil, fmt.Errorf("%w after %d attempts", ErrRateLimited, attempt)
		}
		if err := c.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// send performs one HTTP round trip. limited is true on HTTP 429 or when
// any item carries a JSON-RPC 429 error.
func (c *Client) send(ctx context.Context, payload []byte) (responses []rpcResponse, limited bool, err error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return nil, false, fmt.Errorf("rpc: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("rpc: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, true, nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, false, fmt.Errorf("rpc: read response: %w", err)
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

func (c *Client) noteRateLimit() time.Duration {
	c.mu.Lock()
	now := c.now()
	c.trimCallsLocked(now)
	c.rateLimitEvents++
	c.last429 = now
	wait := c.backoff
	c.backoff *= 2
	if c.backoff > maxBackoff {
		c.backoff = maxBackoff
	}
	recent := len(c.callTimes)
	c.mu.Unlock()
	c.log.Warn("rpc rate limited", "url", c.url, "backoff", wait.String(), "callsLast10s", recent)
	return wait
}

func (c *Client) resetBackoff() {
	c.mu.Lock()
	c.backoff = minBackoff
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

// Available returns how many calls can be made right now without waiting.
func (c *Client) Available() int { return c.pacer.Available() }
