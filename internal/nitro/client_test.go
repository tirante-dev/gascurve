package nitro

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tirante-dev/gascurve/internal/logger"
)

func echoHandler(params []json.RawMessage) any {
	return paramString(params[0])
}

func TestBatchOrderingAndUserAgent(t *testing.T) {
	f := newFakeRPC(t)
	f.handlers["echo"] = echoHandler
	f.reverse = true
	c, _ := newTestClient(t, f, 100)
	c = NewClient(f.server.URL, 100, WithPacer(c.pacer), WithLogger(logger.Nop()), WithHTTPClient(f.server.Client()))
	reqs := []Request{{Method: "echo", Params: []any{"a"}}, {Method: "echo", Params: []any{"b"}}, {Method: "nope"}}
	res, err := c.Batch(context.Background(), reqs)
	if err != nil {
		t.Fatal(err)
	}
	if string(res[0].Raw) != `"a"` || string(res[1].Raw) != `"b"` {
		t.Fatalf("results out of order: %s %s", res[0].Raw, res[1].Raw)
	}
	var rpcErr *RPCError
	if !errors.As(res[2].Err, &rpcErr) || rpcErr.Code != -32601 {
		t.Fatalf("expected method not found, got %v", res[2].Err)
	}
	if f.agents[0] != "gascurve/dev" {
		t.Fatalf("user agent = %q", f.agents[0])
	}
	if f.items != 3 {
		t.Fatalf("items = %d", f.items)
	}
	raw, err := c.Call(context.Background(), "echo", "x")
	if err != nil || string(raw) != `"x"` {
		t.Fatalf("Call: %s %v", raw, err)
	}
	if _, err := c.Call(context.Background(), "nope"); err == nil {
		t.Fatal("expected error")
	}
	if res, err := c.Batch(context.Background(), nil); res != nil || err != nil {
		t.Fatal("empty batch")
	}
	if _, err := c.Batch(context.Background(), make([]Request, 101)); err == nil {
		t.Fatal("oversized batch")
	}
	if c.Pacer() == nil {
		t.Fatal("pacer")
	}
}

func TestRateLimitBackoff(t *testing.T) {
	f := newFakeRPC(t)
	f.handlers["echo"] = echoHandler
	f.script = []scriptStep{
		{status: http.StatusTooManyRequests, body: "slow down"},
		{status: http.StatusOK, body: `{"jsonrpc":"2.0","id":1,"error":{"code":429,"message":"rate limited"}}`},
		{status: http.StatusOK, body: `[{"jsonrpc":"2.0","id":1,"error":{"code":429,"message":"rate limited"}}]`},
	}
	c, clock := newTestClient(t, f, 100)
	raw, err := c.Call(context.Background(), "echo", "ok")
	if err != nil || string(raw) != `"ok"` {
		t.Fatalf("Call after backoff: %s %v", raw, err)
	}
	sleeps := clock.Sleeps()
	if len(sleeps) != 3 || sleeps[0] != 2*time.Second || sleeps[1] != 4*time.Second || sleeps[2] != 8*time.Second {
		t.Fatalf("backoff sleeps = %v", sleeps)
	}
	st := c.Stats()
	if st.RateLimitEvents != 3 || st.Last429At.IsZero() || st.Backoff != minBackoff || st.CallsLast10s != 2 {
		t.Fatalf("stats = %+v", st)
	}
	// Exhausting attempts returns ErrRateLimited; the back-off is capped.
	f.script = nil
	for range 10 {
		f.script = append(f.script, scriptStep{status: http.StatusTooManyRequests})
	}
	c2 := NewClient(f.server.URL, 100, WithPacer(NewPacer(100).withClock(clock.Now, clock.Sleep)), withClock(clock.Now, clock.Sleep), WithMaxAttempts(7), WithHTTPClient(f.server.Client()))
	_, err = c2.Call(context.Background(), "echo", "x")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
	s2 := clock.Sleeps()[3:]
	if s2[len(s2)-1] != maxBackoff || len(s2) != 6 {
		t.Fatalf("capped sleeps = %v", s2)
	}
	// Canceled context aborts the retry sleep.
	f.script = []scriptStep{{status: http.StatusTooManyRequests}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Call(ctx, "echo", "x"); err == nil {
		t.Fatal("expected context error")
	}
	clock.Advance(time.Minute)
	if c.Stats().CallsLast10s != 0 {
		t.Fatal("call window should have expired")
	}
}

// TestCooldownSharedAcrossCallers: a 429 seen by one caller blocks every
// caller on the network for the back-off, every retry pays the pacer again,
// and the back-off only resets after a success sent past the cooldown.
func TestCooldownSharedAcrossCallers(t *testing.T) {
	f := newFakeRPC(t)
	f.handlers["echo"] = echoHandler
	f.script = []scriptStep{{status: http.StatusTooManyRequests}}
	clock := newFakeClock()
	// Four calls per second: a 10-item batch is three more than a bulk
	// caller can ever hold, so it takes the bulk share and waits for the
	// rest instead of overdrawing.
	p := NewPacer(4).withClock(clock.Now, clock.Sleep)
	c := NewClient(f.server.URL, 4, WithPacer(p), withClock(clock.Now, clock.Sleep), WithHTTPClient(f.server.Client()))
	reqs := make([]Request, 10)
	for i := range reqs {
		reqs[i] = Request{Method: "echo", Params: []any{"a"}}
	}
	if _, err := c.Batch(context.Background(), reqs); err != nil {
		t.Fatal(err)
	}
	// Attempt 1 takes the seven tokens of the bulk share at once and waits
	// 750 ms for the last three. The retry pays the whole ten again, in
	// the same two steps, which already covers the 2 s cooldown the 429
	// started; the bucket never goes negative.
	sleeps := clock.Sleeps()
	if len(sleeps) != 3 || sleeps[0] != 750*time.Millisecond || sleeps[1] != 1750*time.Millisecond || sleeps[2] != 750*time.Millisecond {
		t.Fatalf("sleeps = %v", sleeps)
	}
	if tokens, _ := p.levels(); tokens < 0 {
		t.Fatalf("a batch above the bulk share must not overdraw: %v", tokens)
	}
	if c.Stats().Backoff != minBackoff {
		t.Fatal("back-off should reset after a success past the cooldown")
	}
	// A fresh 429 starts a cooldown that other callers observe through
	// Available() and through their own send.
	f.script = []scriptStep{{status: http.StatusTooManyRequests}}
	c2 := NewClient(f.server.URL, 0, WithPacer(NewPacer(0).withClock(clock.Now, clock.Sleep)), withClock(clock.Now, clock.Sleep), WithHTTPClient(f.server.Client()), WithMaxAttempts(1))
	if _, err := c2.Call(context.Background(), "echo", "x"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected rate limit, got %v", err)
	}
	if c2.Available() != 0 || c2.cooldown() != 2*time.Second {
		t.Fatalf("cooldown should hide availability: %d %v", c2.Available(), c2.cooldown())
	}
	before := len(clock.Sleeps())
	if _, err := c2.Call(context.Background(), "echo", "y"); err != nil {
		t.Fatal(err)
	}
	if s := clock.Sleeps()[before:]; len(s) != 1 || s[0] != 2*time.Second {
		t.Fatalf("second caller must wait out the cooldown: %v", s)
	}
	if c2.Available() <= 0 {
		t.Fatal("availability returns after the cooldown")
	}
	// A success whose request left before another caller's cooldown began
	// does not reset the back-off.
	c2.mu.Lock()
	c2.backoff = 8 * time.Second
	c2.blockedUntil = clock.Now().Add(time.Minute)
	c2.mu.Unlock()
	c2.resetBackoff(clock.Now())
	if c2.Stats().Backoff != 8*time.Second {
		t.Fatal("back-off must not reset for a request sent before the cooldown")
	}
	// The cooldown sleep honors cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, err := c2.send(ctx, []byte("[]"), 1); err == nil {
		t.Fatal("expected context error")
	}
	// The cooldown is published before the send lock is released: a caller
	// that queued behind the throttled request sleeps through the cooldown
	// instead of sending into the throttle.
	f.script = []scriptStep{{status: http.StatusTooManyRequests}}
	gate := make(chan struct{})
	f.mu.Lock()
	f.hold = gate
	f.mu.Unlock()
	c3 := NewClient(f.server.URL, 0, WithPacer(NewPacer(0).withClock(clock.Now, clock.Sleep)), withClock(clock.Now, clock.Sleep), WithHTTPClient(f.server.Client()), WithMaxAttempts(1))
	first := make(chan error, 1)
	go func() {
		_, err := c3.Call(context.Background(), "echo", "throttled")
		first <- err
	}()
	// Wait until the first request is held by the server, then queue the
	// second behind the send lock and release the first.
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		held := f.hold == nil
		f.mu.Unlock()
		if held || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	second := make(chan error, 1)
	go func() {
		_, err := c3.Call(context.Background(), "echo", "queued")
		second <- err
	}()
	time.Sleep(20 * time.Millisecond)
	before = len(clock.Sleeps())
	close(gate)
	if err := <-first; !errors.Is(err, ErrRateLimited) {
		t.Fatalf("first caller: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("second caller: %v", err)
	}
	if s := clock.Sleeps()[before:]; len(s) != 1 || s[0] != 2*time.Second || c3.Stats().RateLimitEvents != 1 {
		t.Fatalf("the queued caller must observe the cooldown: sleeps %v stats %+v", s, c3.Stats())
	}
}

func TestChainID(t *testing.T) {
	f := newFakeRPC(t)
	f.handlers["eth_chainId"] = func([]json.RawMessage) any { return "0x1237" }
	c, _ := newTestClient(t, f, 100)
	if id, err := c.ChainID(context.Background()); err != nil || id != 4663 {
		t.Fatalf("ChainID = %d %v", id, err)
	}
	f.handlers["eth_chainId"] = func([]json.RawMessage) any { return 7 }
	if _, err := c.ChainID(context.Background()); err == nil {
		t.Fatal("decode error expected")
	}
	delete(f.handlers, "eth_chainId")
	if _, err := c.ChainID(context.Background()); err == nil {
		t.Fatal("rpc error expected")
	}
}

func TestTransportErrors(t *testing.T) {
	f := newFakeRPC(t)
	f.handlers["echo"] = echoHandler
	c, _ := newTestClient(t, f, 100)
	f.script = []scriptStep{{status: http.StatusInternalServerError, body: "boom"}}
	if _, err := c.Call(context.Background(), "echo", "x"); err == nil {
		t.Fatal("expected http error")
	}
	f.script = []scriptStep{{status: http.StatusOK, body: "not json"}}
	if _, err := c.Call(context.Background(), "echo", "x"); err == nil {
		t.Fatal("expected decode error")
	}
	f.script = []scriptStep{{status: http.StatusOK, body: `{"jsonrpc":"2.0","id":1,"error":{"code":-32600,"message":"invalid"}}`}}
	if _, err := c.Call(context.Background(), "echo", "x"); err == nil {
		t.Fatal("expected single error")
	}
	f.script = []scriptStep{{status: http.StatusOK, body: `{"jsonrpc":"2.0","id":999,"result":"z"}`}}
	if _, err := c.Call(context.Background(), "echo", "x"); err == nil {
		t.Fatal("expected missing id error")
	}
	f.script = []scriptStep{{status: http.StatusOK, body: `[]`}}
	if _, err := c.Call(context.Background(), "echo", "x"); err == nil {
		t.Fatal("expected missing response")
	}
	f.server.Close()
	if _, err := c.Call(context.Background(), "echo", "x"); err == nil {
		t.Fatal("expected connection error")
	}
	bad := NewClient("http://[::1]:namedport", 1)
	if _, err := bad.Call(context.Background(), "echo"); err == nil {
		t.Fatal("expected request build error")
	}
}

func TestIsRevert(t *testing.T) {
	if !IsRevert(&RPCError{Code: 3, Message: "execution reverted"}) || !IsRevert(&RPCError{Code: 3, Message: "x"}) {
		t.Fatal("revert code")
	}
	if !IsRevert(&RPCError{Code: 1, Message: "VM Revert"}) || !IsRevert(&RPCError{Code: -32000, Message: "execution reverted"}) {
		t.Fatal("revert message")
	}
	// Generic server codes are not reverts by number alone: nodes use them
	// for missing state and proxy failures too.
	if IsRevert(&RPCError{Code: -32000, Message: "missing trie node"}) || IsRevert(&RPCError{Code: -32015, Message: "header not found"}) {
		t.Fatal("server codes misclassified as reverts")
	}
	if IsRevert(&RPCError{Code: 1, Message: "other"}) || IsRevert(errors.New("plain")) {
		t.Fatal("non revert")
	}
	// Valid revert data (Error(string) or Panic(uint256)) is a revert
	// whatever the code says; garbage data is not.
	errSel := Selector("Error(string)")
	panicSel := Selector("Panic(uint256)")
	if !IsRevert(&RPCError{Code: -32000, Message: "x", Data: json.RawMessage(`"` + EncodeHex(append(errSel[:], 0, 0)) + `"`)}) {
		t.Fatal("Error(string) data")
	}
	if !IsRevert(&RPCError{Code: -32000, Message: "x", Data: json.RawMessage(`{"data":"` + EncodeHex(panicSel[:]) + `"}`)}) {
		t.Fatal("Panic(uint256) data in an object")
	}
	for _, d := range []string{`"0x"`, `"0x01020304"`, `"zz"`, `12`, `{"other":1}`, `{"data":""}`} {
		if IsRevert(&RPCError{Code: -32000, Message: "x", Data: json.RawMessage(d)}) {
			t.Fatalf("data %s is not revert data", d)
		}
	}
	if (&RPCError{Code: 1, Message: "m"}).Error() != "rpc error 1: m" {
		t.Fatal("Error()")
	}
	if truncate("abc", 2) != "ab..." || truncate("a", 5) != "a" {
		t.Fatal("truncate")
	}
}

func TestIsRateLimit(t *testing.T) {
	limited := []*RPCError{
		{Code: RateLimitCode, Message: "x"},
		{Code: RateLimitCodeExceeded, Message: "limit exceeded"},
		{Code: RateLimitCodeQuickNode, Message: "50/second request limit reached"},
		{Code: -32000, Message: "Rate Limit Exceeded"},
		{Code: -32000, Message: "Too Many Requests"},
		{Code: -32000, Message: "daily request limit reached"},
		{Code: 1, Message: "project limit exceeded"},
		{Code: 1, Message: "monthly quota LIMIT REACHED"},
	}
	for _, e := range limited {
		if !IsRateLimit(e) {
			t.Fatalf("%v should be a rate limit", e)
		}
	}
	answers := []*RPCError{
		nil,
		{Code: -32000, Message: "execution reverted"},
		{Code: -32601, Message: "method not found"},
		{Code: -32000, Message: "missing trie node"},
		{Code: 3, Message: "limited liability"},
	}
	for _, e := range answers {
		if IsRateLimit(e) {
			t.Fatalf("%v is an answer, not a rate limit", e)
		}
	}
}

// TestJSONRPCRateLimit: a JSON-RPC error that reports throttling (by code
// or message) is handled exactly like an HTTP 429: it counts as a rate
// limit event, starts the back-off, retries the whole batch even when only
// one item carried it, and past the attempt budget becomes the endpoint
// error that drives failover.
func TestJSONRPCRateLimit(t *testing.T) {
	ctx := context.Background()
	f := newFakeRPC(t)
	f.handlers["echo"] = echoHandler
	c, clock := newTestClient(t, f, 100)
	// A single call answered with QuickNode's per-second limit.
	f.script = []scriptStep{{status: http.StatusOK, body: `{"jsonrpc":"2.0","id":1,"error":{"code":-32007,"message":"50/second request limit reached"}}`}}
	raw, err := c.Call(ctx, "echo", "ok")
	if err != nil || string(raw) != `"ok"` {
		t.Fatalf("Call after a JSON-RPC limit: %s %v", raw, err)
	}
	if s := clock.Sleeps(); len(s) != 1 || s[0] != minBackoff {
		t.Fatalf("back-off sleeps = %v", s)
	}
	if st := c.Stats(); st.RateLimitEvents != 1 || st.Last429At.IsZero() || st.Backoff != minBackoff {
		t.Fatalf("stats = %+v", st)
	}
	// A batch where one item alone carries the limit is retried as a whole
	// after the back-off, so every item is answered by one attempt.
	var once atomic.Bool
	f.handlers["echo"] = func(params []json.RawMessage) any {
		if paramString(params[0]) == "b" && once.CompareAndSwap(false, true) {
			return &RPCError{Code: RateLimitCodeExceeded, Message: "limit exceeded"}
		}
		return echoHandler(params)
	}
	f.mu.Lock()
	f.items = 0
	f.mu.Unlock()
	res, err := c.Batch(ctx, []Request{{Method: "echo", Params: []any{"a"}}, {Method: "echo", Params: []any{"b"}}, {Method: "echo", Params: []any{"c"}}})
	if err != nil || len(res) != 3 || res[0].Err != nil || res[1].Err != nil || res[2].Err != nil || string(res[1].Raw) != `"b"` {
		t.Fatalf("batch after a partial limit: %+v %v", res, err)
	}
	if s := clock.Sleeps(); len(s) != 2 || s[1] != minBackoff || f.items != 6 || c.Stats().RateLimitEvents != 2 {
		t.Fatalf("the whole batch is resent once: sleeps %v items %d stats %+v", s, f.items, c.Stats())
	}
	// A limit that persists past the attempt budget, reported by message
	// alone, is the same endpoint error an HTTP 429 leaves behind.
	f.handlers["echo"] = func([]json.RawMessage) any {
		return &RPCError{Code: -32000, Message: "Too Many Requests"}
	}
	_, err = c.Call(ctx, "echo", "x")
	if !errors.Is(err, ErrRateLimited) || !IsEndpointError(err) {
		t.Fatalf("expected a rate limit endpoint error, got %v", err)
	}
	if st := c.Stats(); st.RateLimitEvents != 2+uint64(c.maxAttempts) {
		t.Fatalf("every throttled attempt counts: %+v", st)
	}
}

// TestReservationRefundedOnCooldown: tokens are taken before a caller
// queues for the send lock, so a 429 another caller saw meanwhile
// invalidates the reservation. It is refunded, the cooldown is waited out
// and the tokens are paid for again under the lock, instead of a queue of
// waiters bursting through the moment the lock opens with tokens they
// reserved while the endpoint was throttling.
func TestReservationRefundedOnCooldown(t *testing.T) {
	f := newFakeRPC(t)
	f.handlers["echo"] = echoHandler
	clock := newFakeClock()
	p := NewPacer(4).withClock(clock.Now, clock.Sleep)
	c := NewClient(f.server.URL, 4, WithPacer(p), withClock(clock.Now, clock.Sleep), WithHTTPClient(f.server.Client()))
	// Another caller's 429 starts a two second cooldown while this one is
	// paying for its seven token batch.
	c.mu.Lock()
	c.blockedUntil = clock.Now().Add(2 * time.Second)
	c.mu.Unlock()
	reqs := make([]Request, 7)
	for i := range reqs {
		reqs[i] = Request{Method: "echo", Params: []any{"a"}}
	}
	if _, err := c.Batch(context.Background(), reqs); err != nil {
		t.Fatal(err)
	}
	if s := clock.Sleeps(); len(s) != 1 || s[0] != 2*time.Second {
		t.Fatalf("the cooldown must be waited out under the lock: %v", s)
	}
	// The batch was paid for twice and refunded once, so the bucket holds
	// what one seven token batch leaves, not a full one.
	if tokens, reserve := p.levels(); tokens != 1 || reserve != 1 {
		t.Fatalf("the invalidated reservation must be refunded and paid again: tokens %v reserve %v", tokens, reserve)
	}
	// A canceled cooldown wait gives the tokens back and fails.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.mu.Lock()
	c.blockedUntil = clock.Now().Add(time.Minute)
	c.mu.Unlock()
	if _, err := c.Batch(ctx, reqs[:1]); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cooldown: %v", err)
	}
	// A request refused under the send lock (its endpoint is no longer the
	// active one) is refunded too and never reaches the wire.
	c.mu.Lock()
	c.blockedUntil = time.Time{}
	c.mu.Unlock()
	clock.Advance(time.Hour)
	c.preSend = func(context.Context) error { return &EndpointError{Err: ErrStaleEndpoint} }
	before, requests := p.Available(), f.requestCount()
	if _, err := c.Batch(context.Background(), reqs[:3]); !errors.Is(err, ErrStaleEndpoint) {
		t.Fatalf("stale endpoint: %v", err)
	}
	if p.Available() != before || f.requestCount() != requests {
		t.Fatalf("a refused request must be refunded and unsent: available %d (was %d) requests %d (was %d)",
			p.Available(), before, f.requestCount(), requests)
	}
}
