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
	// (the way QuickNode rejects oversized batches).
	maxItems int
	// hold, when set, makes the next request wait until it is closed
	// before it is answered, so another caller can queue behind it.
	hold chan struct{}
}

type scriptStep struct {
	status int
	body   string
}

func newFakeRPC(t *testing.T) *fakeRPC {
	t.Helper()
	f := &fakeRPC{
		t:        t,
		handlers: map[string]handlerFunc{},
		calls:    map[string][]byte{},
		callErrs: map[string]*RPCError{},
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
	if f.maxItems > 0 && len(reqs) > f.maxItems {
		f.mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	f.items += len(reqs)
	f.mu.Unlock()
	responses := make([]map[string]any, 0, len(reqs))
	for _, req := range reqs {
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
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
