package nitro

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
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
	// Four calls per second: a 10-item batch drains the burst of 8 and
	// waits for two more tokens on every attempt.
	p := NewPacer(4).withClock(clock.Now, clock.Sleep)
	c := NewClient(f.server.URL, 4, WithPacer(p), withClock(clock.Now, clock.Sleep), WithHTTPClient(f.server.Client()))
	reqs := make([]Request, 10)
	for i := range reqs {
		reqs[i] = Request{Method: "echo", Params: []any{"a"}}
	}
	if _, err := c.Batch(context.Background(), reqs); err != nil {
		t.Fatal(err)
	}
	// Attempt 1 pays 10 tokens against a burst of 8 (500 ms). The retry
	// pays 10 tokens again: the bucket refilled 2 during the first sleep,
	// so it waits 2.5 s, which already covers the 2 s cooldown.
	sleeps := clock.Sleeps()
	if len(sleeps) != 2 || sleeps[0] != 500*time.Millisecond || sleeps[1] != 2500*time.Millisecond {
		t.Fatalf("sleeps = %v", sleeps)
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
