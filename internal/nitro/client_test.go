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
	if !IsRevert(&RPCError{Code: 3, Message: "execution reverted"}) || !IsRevert(&RPCError{Code: -32000, Message: "x"}) {
		t.Fatal("revert codes")
	}
	if !IsRevert(&RPCError{Code: 1, Message: "VM Revert"}) {
		t.Fatal("revert message")
	}
	if IsRevert(&RPCError{Code: 1, Message: "other"}) || IsRevert(errors.New("plain")) {
		t.Fatal("non revert")
	}
	if (&RPCError{Code: 1, Message: "m"}).Error() != "rpc error 1: m" {
		t.Fatal("Error()")
	}
	if truncate("abc", 2) != "ab..." || truncate("a", 5) != "a" {
		t.Fatal("truncate")
	}
}
