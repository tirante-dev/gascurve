package nitro

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/logger"
)

// theKey is the credential every keyed URL in these tests carries. No
// error, log line or status value may ever contain it.
const theKey = "s3cret-api-key-0123456789"

// assertNoKey fails when v quotes the key, or any of the URLs built from
// it, instead of naming the endpoint.
func assertNoKey(t *testing.T, what, v string) {
	t.Helper()
	if strings.Contains(v, theKey) {
		t.Fatalf("%s leaks the endpoint key: %s", what, v)
	}
}

// TestClientErrorsCarryNoURL: a transport failure to a keyed endpoint is
// reported by net/http as a *url.Error whose message is the whole URL.
// Every error leaving the client names the endpoint by index instead.
func TestClientErrorsCarryNoURL(t *testing.T) {
	ctx := context.Background()
	// 127.0.0.1 port 1 is closed, so the dial fails without a network.
	keyed := "http://user:" + theKey + "@127.0.0.1:1/v2/" + theKey + "?apikey=" + theKey
	e := newEndpoint(3, config.EndpointConfig{RPCURL: keyed}, 10, logger.Nop(), time.Now,
		WithHTTPClient(&http.Client{Timeout: 2 * time.Second}), WithMaxAttempts(1))
	_, err := e.BlockNumber(ctx)
	if err == nil {
		t.Fatal("a dial to a closed port must fail")
	}
	assertNoKey(t, "dial error", err.Error())
	if !strings.Contains(err.Error(), "endpoint 3") {
		t.Fatalf("a dial error must name the endpoint: %s", err)
	}
	if !IsEndpointError(err) {
		t.Fatalf("a dial failure is an endpoint failure: %v", err)
	}
	// Nothing that would print the URL again is left in the chain.
	var ue *url.Error
	if errors.As(err, &ue) {
		t.Fatalf("a *url.Error must not survive sanitizing: %s", ue)
	}
}

// TestProviderBodyCarriesNoURL: a provider is free to quote the request
// URL back, in a response body or in a JSON-RPC error message. Neither
// reaches an error a caller could log.
func TestProviderBodyCarriesNoURL(t *testing.T) {
	ctx := context.Background()
	f := newFakeRPC(t)
	keyed := config.EndpointConfig{RPCURL: f.server.URL + "/v2/" + theKey}
	e := newEndpoint(1, keyed, 10, logger.Nop(), time.Now, WithHTTPClient(f.server.Client()), WithMaxAttempts(1))
	// An HTTP error whose body echoes the request URL.
	f.mu.Lock()
	f.script = append(f.script, scriptStep{status: http.StatusBadRequest, body: "no route for /v2/" + theKey})
	f.mu.Unlock()
	_, err := e.BlockNumber(ctx)
	if err == nil {
		t.Fatal("http 400 must be an error")
	}
	assertNoKey(t, "response body", err.Error())

	// A JSON-RPC error message that echoes it.
	f.handlers["eth_blockNumber"] = func([]json.RawMessage) any {
		return &RPCError{Code: -32000, Message: "bad request to /v2/" + theKey, Data: []byte(`"` + theKey + `"`)}
	}
	_, err = e.BlockNumber(ctx)
	if err == nil {
		t.Fatal("a JSON-RPC error must be an error")
	}
	assertNoKey(t, "rpc error message", err.Error())
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("the JSON-RPC error must survive sanitizing: %v", err)
	}
	assertNoKey(t, "rpc error data", string(rpcErr.Data))
}

// TestHTTPAuthFailureFailsOver: an endpoint answering 401 or 403 is
// refusing this caller, which the next endpoint may well not do, so it
// drives failover the way a 5xx does. A 400 stays the caller's error.
func TestHTTPAuthFailureFailsOver(t *testing.T) {
	ctx := context.Background()
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		a, b := chainFake(t, testChainIDHex), chainFake(t, testChainIDHex)
		clock := newFakeClock()
		p := newTestPool(t, clock, 30*time.Second, endpointOf(a, 100), endpointOf(b, 100))
		if err := p.Verify(ctx); err != nil {
			t.Fatal(err)
		}
		a.fail(status)
		echoVia(t, p, "over")
		if p.ActiveEndpoint() != 1 {
			t.Fatalf("http %d must fail over, active %d", status, p.ActiveEndpoint())
		}
	}
	// A 400 says something about the request: it is not the endpoint's
	// fault and repeating it elsewhere would only repeat the answer.
	a, b := chainFake(t, testChainIDHex), chainFake(t, testChainIDHex)
	clock := newFakeClock()
	p := newTestPool(t, clock, 30*time.Second, endpointOf(a, 100), endpointOf(b, 100))
	if err := p.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	a.fail(http.StatusBadRequest)
	if _, err := p.Call(ctx, "echo", "x"); err == nil || IsEndpointError(err) {
		t.Fatalf("http 400 must not fail over: %v", err)
	}
	if p.ActiveEndpoint() != 0 {
		t.Fatalf("active moved on a 400: %d", p.ActiveEndpoint())
	}
}

// TestPoolFailsOverAGetLogsFloorRefusal: an endpoint that will not serve a
// single block of logs cannot answer the question at all, so the pool
// moves to one that can.
func TestPoolFailsOverAGetLogsFloorRefusal(t *testing.T) {
	ctx := context.Background()
	a, b := chainFake(t, testChainIDHex), chainFake(t, testChainIDHex)
	rangeLimitedLogs(a, 0, &RPCError{Code: -32000, Message: "no logs for you"})
	rangeLimitedLogs(b, 1<<40, nil, 5)
	clock := newFakeClock()
	p := newTestPool(t, clock, 30*time.Second, endpointOf(a, 100), endpointOf(b, 100))
	if err := p.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	logs, err := p.OwnerActsLogs(ctx, 5, 5)
	if err != nil || len(logs) != 1 || logs[0].BlockNumber != 5 {
		t.Fatalf("the pool must try the next endpoint: %v %v", logs, err)
	}
	if p.ActiveEndpoint() != 1 {
		t.Fatalf("active endpoint %d", p.ActiveEndpoint())
	}
}

// TestHeadSubscriberErrorsCarryNoURL: the WebSocket dialer reports a
// transport failure as a *url.Error holding the whole ws:// URL, which is
// as much a credential as the RPC URL. The subscriber names the endpoint
// instead, and tells the pool that the endpoint's socket failed.
func TestHeadSubscriberErrorsCarryNoURL(t *testing.T) {
	ctx := context.Background()
	f := chainFake(t, testChainIDHex)
	clock := newFakeClock()
	// 127.0.0.1 port 1 is closed, so the dial fails without a network.
	p := newTestPool(t, clock, time.Minute,
		config.EndpointConfig{RPCURL: f.server.URL, WSURL: "ws://user:" + theKey + "@127.0.0.1:1/ws?apikey=" + theKey, CallsPerSecond: 100})
	if err := p.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	s := NewHeadSubscriber("", WithHeadLogger(logger.Nop()), WithHeadLease(p.WSEndpoint),
		WithHeadHTTPClient(&http.Client{Timeout: 2 * time.Second}))
	subscribed, err := s.runOnce(ctx, nil)
	if err == nil || subscribed {
		t.Fatalf("a dial to a closed port must fail: %v", err)
	}
	assertNoKey(t, "websocket dial error", err.Error())
	if !strings.Contains(err.Error(), "endpoint 0") {
		t.Fatalf("a websocket dial error must name the endpoint: %s", err)
	}
	// The failure reached the pool, and what it kept for /status carries
	// no key either.
	st := p.Status()
	if !st.Endpoints[0].WSCooling {
		t.Fatal("a dial failure must cool the endpoint's websocket down")
	}
	assertNoKey(t, "endpoint status", st.Endpoints[0].WSError)
}

// TestScrubber: the scrubber replaces every part of a URL that can carry a
// secret, is safe on a nil receiver (a subscriber built without one), and
// leaves an error it has nothing to say about exactly as it is.
func TestScrubber(t *testing.T) {
	s := newScrubber(2, "https://user:"+theKey+"@rpc.example/v2/"+theKey+"?apikey="+theKey, "wss://rpc.example/ws/"+theKey)
	for _, in := range []string{
		"https://user:" + theKey + "@rpc.example/v2/" + theKey + "?apikey=" + theKey,
		"no route for /v2/" + theKey,
		"apikey=" + theKey,
		"wss://rpc.example/ws/" + theKey,
	} {
		if out := s.text(in); strings.Contains(out, theKey) {
			t.Fatalf("not scrubbed: %q -> %q", in, out)
		} else if !strings.Contains(out, "endpoint 2") {
			t.Fatalf("the endpoint must be named: %q -> %q", in, out)
		}
	}
	// A URL that cannot be parsed still has its whole text replaced.
	bad := newScrubber(0, "://"+theKey)
	if out := bad.text("dial ://" + theKey); strings.Contains(out, theKey) {
		t.Fatalf("unparsable URL not scrubbed: %q", out)
	}
	// Nothing to say: the error comes back unchanged, and the identity of
	// the value is kept so errors.Is still matches it.
	plain := errors.New("dial tcp: connection refused")
	if got := s.wrap(plain); !errors.Is(got, plain) || got.Error() != plain.Error() {
		t.Fatalf("an error with no secret in it must be returned as is: %v", got)
	}
	// A *url.Error with no cause still loses its URL.
	empty := s.wrap(&url.Error{Op: "Post", URL: "https://rpc.example/v2/" + theKey})
	if empty == nil || strings.Contains(empty.Error(), theKey) {
		t.Fatalf("empty url error: %v", empty)
	}
	// A nil scrubber and a nil error are no-ops.
	var none *scrubber
	if none.text("x") != "x" || none.rpcError(nil) != nil {
		t.Fatal("a nil scrubber must be a no-op")
	}
	if got := none.wrap(plain); !errors.Is(got, plain) || got.Error() != plain.Error() {
		t.Fatalf("a nil scrubber must return the error unchanged: %v", got)
	}
	if s.wrap(nil) != nil || s.rpcError(nil) != nil {
		t.Fatal("nil in, nil out")
	}
}
