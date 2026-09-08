package nitro

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeRPC is an httptest JSON-RPC server driven by per-method handlers and
// per-selector eth_call return data.
type fakeRPC struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	handlers map[string]handlerFunc
	calls    map[string][]byte // selector hex -> return data hex
	callErrs map[string]*RPCError
	requests int
	items    int
	agents   []string
	callTags []string // block tag of every eth_call, in order
	reverse  bool
	// script of HTTP statuses/bodies to return before normal handling.
	script []scriptStep
	// maxItems, when positive, answers any batch with more items with 429
	// (the way QuickNode rejects oversized batches), or, when limitErr is
	// set, with HTTP 200 and limitErr on the first item alone (the way
	// QuickNode reports its per-second request limit).
	maxItems int
	limitErr *RPCError
	// hold, when set, makes the next request wait until it is closed
	// before it is answered, so another caller can queue behind it.
	hold chan struct{}
	// sizeLimit, when not negative, is how many items of a batch fit in one response: everything from
	// that index on is answered with -32003, the way a geth node stops filling a batch once the answer
	// outgrows its limit. Zero refuses even a single item.
	sizeLimit int
	// barrier, when set, holds every request until it is closed, so several can be observed in
	// flight at once. inFlight counts the requests being served and peak is its high-water mark.
	barrier  chan struct{}
	inFlight int
	peak     int
	// failMethods answers any batch carrying one of these methods with HTTP 500 quoting it, so a
	// test can fail one chunk of a fanned out request and not the others. gate, when set, holds any
	// batch carrying gateMethod until it is closed, which orders two failures that would otherwise
	// race.
	failMethods map[string]bool
	gate        chan struct{}
	gateMethod  string
}

type scriptStep struct {
	status int
	body   string
}

func newFakeRPC(t *testing.T) *fakeRPC {
	t.Helper()
	f := &fakeRPC{
		t:         t,
		handlers:  map[string]handlerFunc{},
		calls:     map[string][]byte{},
		callErrs:  map[string]*RPCError{},
		sizeLimit: -1,
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	f.handlers["eth_call"] = f.ethCall
	return f
}

// handlerFunc answers one JSON-RPC call; returning a *RPCError makes it
// the error member of the response.
type handlerFunc func(params []json.RawMessage) any

func (f *fakeRPC) ethCall(params []json.RawMessage) any {
	var arg struct {
		To   string `json:"to"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal(params[0], &arg); err != nil {
		return &RPCError{Code: -32602, Message: err.Error()}
	}
	if len(params) > 1 {
		f.callTags = append(f.callTags, paramString(params[1]))
	}
	sel := arg.Data[:10]
	if e, ok := f.callErrs[sel]; ok {
		return e
	}
	data, ok := f.calls[sel]
	if !ok {
		return &RPCError{Code: -32000, Message: "execution reverted"}
	}
	return EncodeHex(data)
}

func carries(reqs []rpcRequest, method string) bool {
	for _, r := range reqs {
		if r.Method == method {
			return true
		}
	}
	return false
}

// holdAll makes every request wait until release is called, so a test can pin how many the client
// keeps in flight at once. The cleanup releases whatever is still held, so a failed assertion
// reports itself instead of leaving the held callers to time the package out.
func (f *fakeRPC) holdAll(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	f.barrier = make(chan struct{})
	f.mu.Unlock()
	t.Cleanup(f.release)
}

// holdMethod holds any batch carrying method until releaseMethod is called. The cleanup releases it
// so a failed assertion reports itself rather than leaving the handler blocked and the package to
// time out on the server's close.
func (f *fakeRPC) holdMethod(t *testing.T, method string) {
	t.Helper()
	f.mu.Lock()
	f.gate, f.gateMethod = make(chan struct{}), method
	f.mu.Unlock()
	t.Cleanup(f.releaseMethod)
}

func (f *fakeRPC) releaseMethod() {
	f.mu.Lock()
	g := f.gate
	f.gate = nil
	f.mu.Unlock()
	if g != nil {
		close(g)
	}
}

func (f *fakeRPC) release() {
	f.mu.Lock()
	b := f.barrier
	f.barrier = nil
	f.mu.Unlock()
	if b != nil {
		close(b)
	}
}

// awaitHeld waits until a request set aside by hold has arrived.
func (f *fakeRPC) awaitHeld() bool {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		f.mu.Lock()
		taken := f.hold == nil
		f.mu.Unlock()
		if taken {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// flight returns the requests being served right now and the high-water mark since the start.
func (f *fakeRPC) flight() (inFlight, peak int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inFlight, f.peak
}

// awaitFlight waits until n requests are being served at once, reporting false if they never are.
func (f *fakeRPC) awaitFlight(n int) bool {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if in, _ := f.flight(); in >= n {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

func (f *fakeRPC) setCall(sig string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[SelectorHex(sig)] = data
}

func (f *fakeRPC) setCallErr(sig string, e *RPCError) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callErrs[SelectorHex(sig)] = e
}

func (f *fakeRPC) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	if f.hold != nil {
		hold := f.hold
		f.hold = nil
		f.mu.Unlock()
		<-hold
		f.mu.Lock()
	}
	f.requests++
	f.inFlight++
	f.peak = max(f.peak, f.inFlight)
	barrier := f.barrier
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()
	if barrier != nil {
		<-barrier
	}
	f.mu.Lock()
	f.agents = append(f.agents, r.Header.Get("User-Agent"))
	if len(f.script) > 0 {
		step := f.script[0]
		f.script = f.script[1:]
		f.mu.Unlock()
		w.WriteHeader(step.status)
		_, _ = io.WriteString(w, step.body)
		return
	}
	f.mu.Unlock()

	var reqs []rpcRequest
	single := false
	if err := json.Unmarshal(body, &reqs); err != nil {
		var one rpcRequest
		if err := json.Unmarshal(body, &one); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		reqs = []rpcRequest{one}
		single = true
	}
	f.mu.Lock()
	gate, gateMethod := f.gate, f.gateMethod
	f.mu.Unlock()
	if gate != nil && carries(reqs, gateMethod) {
		<-gate
	}
	f.mu.Lock()
	for _, req := range reqs {
		if f.failMethods[req.Method] {
			f.mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, req.Method)
			return
		}
	}
	var limitErr *RPCError
	if f.maxItems > 0 && len(reqs) > f.maxItems {
		if f.limitErr == nil {
			f.mu.Unlock()
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		limitErr = f.limitErr
	}
	f.items += len(reqs)
	sizeLimit := f.sizeLimit
	f.mu.Unlock()
	responses := make([]map[string]any, 0, len(reqs))
	for i, req := range reqs {
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if i == 0 && limitErr != nil {
			resp["error"] = limitErr
			responses = append(responses, resp)
			continue
		}
		if sizeLimit >= 0 && len(reqs) > sizeLimit && i >= sizeLimit {
			resp["error"] = &RPCError{Code: ResponseTooLargeCode, Message: "response too large"}
			responses = append(responses, resp)
			continue
		}
		f.mu.Lock()
		h, ok := f.handlers[req.Method]
		f.mu.Unlock()
		if !ok {
			resp["error"] = &RPCError{Code: -32601, Message: "method not found: " + req.Method}
		} else {
			params := make([]json.RawMessage, len(req.Params))
			for i, p := range req.Params {
				params[i], _ = json.Marshal(p)
			}
			out := h(params)
			if rpcErr, isErr := out.(*RPCError); isErr {
				resp["error"] = rpcErr
			} else {
				resp["result"] = out
			}
		}
		responses = append(responses, resp)
	}
	if f.reverse {
		for i, j := 0, len(responses)-1; i < j; i, j = i+1, j-1 {
			responses[i], responses[j] = responses[j], responses[i]
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if single {
		_ = json.NewEncoder(w).Encode(responses[0])
		return
	}
	_ = json.NewEncoder(w).Encode(responses)
}

// fakeClock is a manual clock whose sleeps advance time instantly.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return nil
}

func (c *fakeClock) Sleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

func newTestClient(t *testing.T, f *fakeRPC, cps float64) (*Client, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	p := NewPacer(cps).withClock(clock.Now, clock.Sleep)
	c := NewClient(f.server.URL, cps, WithPacer(p), withClock(clock.Now, clock.Sleep), WithHTTPClient(f.server.Client()))
	return c, clock
}

func blockJSON(number, ts, gasUsed uint64, baseFee string, txs []any, l1 uint64) map[string]any {
	m := map[string]any{
		"number":        blockTag(number),
		"hash":          "0xabc",
		"parentHash":    "0xparent",
		"timestamp":     blockTag(ts),
		"gasUsed":       blockTag(gasUsed),
		"gasLimit":      "0x1e84800",
		"baseFeePerGas": baseFee,
		"transactions":  txs,
	}
	if l1 > 0 {
		m["l1BlockNumber"] = blockTag(l1)
	}
	return m
}

func paramString(p json.RawMessage) string {
	var s string
	_ = json.Unmarshal(p, &s)
	return s
}
