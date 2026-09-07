package nitro

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/tirante-dev/gascurve/internal/logger"
)

const fakeSubID = "0xsub1"

// fakeWS is an httptest WebSocket server. script runs once per accepted
// connection with the 1-based accept count and returns when the server
// side is done with the socket.
type fakeWS struct {
	server  *httptest.Server
	mu      sync.Mutex
	accepts int
	script  func(n int, conn *websocket.Conn)
	done    chan struct{}
}

func newFakeWS(t *testing.T, script func(n int, conn *websocket.Conn)) *fakeWS {
	t.Helper()
	f := &fakeWS{script: script, done: make(chan struct{})}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "gascurve/dev" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		f.mu.Lock()
		f.accepts++
		n := f.accepts
		f.mu.Unlock()
		f.script(n, conn)
	}))
	t.Cleanup(func() {
		close(f.done)
		f.server.Close()
	})
	return f
}

func (f *fakeWS) url() string { return "ws" + strings.TrimPrefix(f.server.URL, "http") }

func (f *fakeWS) acceptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accepts
}

// ackSubscribe reads the eth_subscribe request and acknowledges it.
func ackSubscribe(conn *websocket.Conn) error {
	_, data, err := conn.Read(context.Background())
	if err != nil {
		return err
	}
	var req rpcRequest
	if err := json.Unmarshal(data, &req); err != nil || req.Method != methodSubscribe || len(req.Params) != 1 || req.Params[0] != subscriptionNewHeads {
		return fmt.Errorf("unexpected subscribe request %s", data)
	}
	return send(conn, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%q}`, req.ID, fakeSubID))
}

func send(conn *websocket.Conn, msg string) error {
	return conn.Write(context.Background(), websocket.MessageText, []byte(msg))
}

func headMsg(sub string, number uint64) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_subscription","params":{"subscription":%q,"result":{"number":%q,"hash":"0xh%d","timestamp":"0x64"}}}`, sub, blockTag(number), number)
}

// readUntilClosed serves pongs until the client goes away.
func readUntilClosed(conn *websocket.Conn) {
	for {
		if _, _, err := conn.Read(context.Background()); err != nil {
			return
		}
	}
}

// recordingSleeper records requested durations and sleeps briefly for real
// so the ping loop and back-off progress without wall-clock waits.
type recordingSleeper struct {
	mu     sync.Mutex
	sleeps []time.Duration
	after  func(n int)
}

func (r *recordingSleeper) sleep(ctx context.Context, d time.Duration) error {
	r.mu.Lock()
	r.sleeps = append(r.sleeps, d)
	n := len(r.sleeps)
	r.mu.Unlock()
	if r.after != nil {
		r.after(n)
	}
	return sleepContext(ctx, min(d, 2*time.Millisecond))
}

// backoffs returns the recorded sleeps that are not ping intervals.
func (r *recordingSleeper) backoffs(ping time.Duration) []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []time.Duration
	for _, d := range r.sleeps {
		if d != ping {
			out = append(out, d)
		}
	}
	return out
}

const (
	testMinBackoff = time.Millisecond
	testMaxBackoff = 4 * time.Millisecond
	testPing       = 40 * time.Millisecond
)

func newTestSubscriber(url string, sleeper *recordingSleeper) *HeadSubscriber {
	return NewHeadSubscriber(url, WithHeadLogger(logger.Nop()), WithHeadBackoff(testMinBackoff, testMaxBackoff), WithHeadPingInterval(testPing), withHeadSleep(sleeper.sleep), WithHeadHTTPClient(&http.Client{Timeout: 2 * time.Second}))
}

func waitHead(t *testing.T, got <-chan Head) Head {
	t.Helper()
	select {
	case h := <-got:
		return h
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a head")
		return Head{}
	}
}

func TestHeadSubscriberHeadsAndReconnect(t *testing.T) {
	ws := newFakeWS(t, func(n int, conn *websocket.Conn) {
		if err := ackSubscribe(conn); err != nil {
			return
		}
		switch n {
		case 1:
			for _, num := range []uint64{100, 101, 102} {
				_ = send(conn, headMsg(fakeSubID, num))
			}
			// The server drops the socket: the client must reconnect.
			_ = conn.Close(websocket.StatusGoingAway, "restarting")
		default:
			_ = send(conn, headMsg(fakeSubID, 200))
			_ = send(conn, headMsg(fakeSubID, 201))
			readUntilClosed(conn)
		}
	})
	sleeper := &recordingSleeper{}
	s := newTestSubscriber(ws.url(), sleeper)
	if s.Connected() {
		t.Fatal("not connected before Run")
	}
	got := make(chan Head, 16)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		s.Run(ctx, func(h Head) { got <- h })
		close(finished)
	}()
	nums := make([]uint64, 0, 5)
	for range 5 {
		h := waitHead(t, got)
		nums = append(nums, h.Number)
		if h.Hash != fmt.Sprintf("0xh%d", h.Number) || h.Timestamp != 100 {
			t.Fatalf("head fields: %+v", h)
		}
	}
	if fmt.Sprint(nums) != "[100 101 102 200 201]" {
		t.Fatalf("heads = %v", nums)
	}
	if !s.Connected() {
		t.Fatal("must report connected while subscribed")
	}
	if ws.acceptCount() != 2 {
		t.Fatalf("accepts = %d", ws.acceptCount())
	}
	// The reconnect waited the minimum back-off because the first
	// connection had subscribed successfully.
	if b := sleeper.backoffs(testPing); len(b) != 1 || b[0] != testMinBackoff {
		t.Fatalf("backoffs = %v", b)
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if s.Connected() {
		t.Fatal("must report disconnected after Run returns")
	}
}

func TestHeadSubscriberDialBackoff(t *testing.T) {
	// Nothing listens: every dial fails and the back-off doubles up to the cap.
	dead := httptest.NewServer(http.NotFoundHandler())
	url := "ws" + strings.TrimPrefix(dead.URL, "http")
	dead.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sleeper := &recordingSleeper{after: func(n int) {
		if n == 5 {
			cancel()
		}
	}}
	s := newTestSubscriber(url, sleeper)
	s.Run(ctx, func(Head) { t.Error("no head expected") })
	if b := sleeper.backoffs(testPing); fmt.Sprint(b) != "[1ms 2ms 4ms 4ms 4ms]" {
		t.Fatalf("backoffs = %v", b)
	}
	// A server that refuses the upgrade is a dial error too.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer plain.Close()
	if ok, err := NewHeadSubscriber(plain.URL, WithHeadHTTPClient(plain.Client())).runOnce(context.Background(), nil); ok || err == nil {
		t.Fatalf("expected dial error, got %v %v", ok, err)
	}
}

func TestHeadSubscriberSubscribeErrorsAndNoise(t *testing.T) {
	ws := newFakeWS(t, func(n int, conn *websocket.Conn) {
		_, data, err := conn.Read(context.Background())
		if err != nil {
			return
		}
		var req rpcRequest
		_ = json.Unmarshal(data, &req)
		switch n {
		case 1:
			// The node rejects the subscription.
			_ = send(conn, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32601,"message":"notifications not supported"}}`, req.ID))
			readUntilClosed(conn)
		case 2:
			// Malformed subscription id.
			_ = send(conn, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":7}`, req.ID))
			readUntilClosed(conn)
		case 3:
			// The socket closes before any ack arrives.
			_ = conn.Close(websocket.StatusGoingAway, "bye")
		default:
			// Noise before the ack is ignored, then every kind of junk after it.
			_ = send(conn, headMsg(fakeSubID, 1))
			_ = send(conn, `not json`)
			_ = send(conn, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%q}`, req.ID, fakeSubID))
			_ = send(conn, headMsg("0xother", 2))
			_ = send(conn, `{"jsonrpc":"2.0","id":9,"result":"x"}`)
			_ = send(conn, `{broken`)
			_ = send(conn, `{"jsonrpc":"2.0","method":"eth_subscription","params":{"subscription":"0xsub1","result":{"number":"zz"}}}`)
			_ = send(conn, `{"jsonrpc":"2.0","method":"eth_subscription","params":{"subscription":"0xsub1","result":{"number":"0x1","timestamp":"zz"}}}`)
			_ = send(conn, headMsg(fakeSubID, 300))
			readUntilClosed(conn)
		}
	})
	sleeper := &recordingSleeper{}
	s := newTestSubscriber(ws.url(), sleeper)
	got := make(chan Head, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx, func(h Head) { got <- h })
	if h := waitHead(t, got); h.Number != 300 {
		t.Fatalf("head = %+v", h)
	}
	if ws.acceptCount() != 4 {
		t.Fatalf("accepts = %d", ws.acceptCount())
	}
	// Three failed attempts without a subscription keep doubling.
	if b := sleeper.backoffs(testPing); fmt.Sprint(b) != "[1ms 2ms 4ms]" {
		t.Fatalf("backoffs = %v", b)
	}
	select {
	case h := <-got:
		t.Fatalf("unexpected extra head %+v", h)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestHeadSubscriberPingWatchdog(t *testing.T) {
	stall := make(chan struct{})
	ws := newFakeWS(t, func(n int, conn *websocket.Conn) {
		if err := ackSubscribe(conn); err != nil {
			return
		}
		if n == 1 {
			// Subscribed but never reads again: pongs stop, the client
			// must notice and reconnect.
			<-stall
			return
		}
		_ = send(conn, headMsg(fakeSubID, 400))
		readUntilClosed(conn)
	})
	defer close(stall)
	sleeper := &recordingSleeper{}
	s := newTestSubscriber(ws.url(), sleeper)
	got := make(chan Head, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx, func(h Head) { got <- h })
	if h := waitHead(t, got); h.Number != 400 {
		t.Fatalf("head = %+v", h)
	}
	if ws.acceptCount() != 2 {
		t.Fatalf("accepts = %d", ws.acceptCount())
	}
}

func TestParseHeadNotification(t *testing.T) {
	h, ok, err := parseHeadNotification([]byte(headMsg("0xs", 42)), "0xs")
	if err != nil || !ok || h.Number != 42 || h.Hash != "0xh42" || h.Timestamp != 100 {
		t.Fatalf("parse: %+v %v %v", h, ok, err)
	}
	if _, ok, err := parseHeadNotification([]byte(`{"jsonrpc":"2.0","id":1,"result":"0xs"}`), "0xs"); ok || err != nil {
		t.Fatal("responses are not heads")
	}
	if _, ok, err := parseHeadNotification([]byte(`{"jsonrpc":"2.0","method":"eth_subscription","params":{"subscription":"0xs","result":{"number":"0x5"}}}`), "0xs"); !ok || err != nil {
		t.Fatal("timestamp is optional")
	}
	if _, _, err := parseHeadNotification([]byte(`[`), "0xs"); err == nil {
		t.Fatal("invalid json")
	}
	if NewHeadSubscriber("ws://x").minBackoff != headsMinBackoff || NewHeadSubscriber("ws://x").pingInterval != headsPingInterval {
		t.Fatal("defaults")
	}
}

// TestHeadSubscriberRebinds: the subscriber asks its resolver for an
// endpoint before every connection attempt, so it moves to another one the
// moment the endpoint it followed stops being usable, and it keeps
// retrying with its back-off while none is.
func TestHeadSubscriberRebinds(t *testing.T) {
	first := newFakeWS(t, func(_ int, conn *websocket.Conn) {
		if err := ackSubscribe(conn); err != nil {
			return
		}
		_ = send(conn, headMsg(fakeSubID, 100))
		_ = conn.Close(websocket.StatusGoingAway, "endpoint disabled")
	})
	second := newFakeWS(t, func(_ int, conn *websocket.Conn) {
		if err := ackSubscribe(conn); err != nil {
			return
		}
		_ = send(conn, headMsg(fakeSubID, 200))
		readUntilClosed(conn)
	})
	var mu sync.Mutex
	state := 0 // 0: first endpoint, 1: nothing usable, 2: second endpoint
	resolve := func(context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		switch state {
		case 0:
			state = 1
			return first.url(), nil
		case 1:
			state = 2
			return "", ErrNoEndpoint
		default:
			return second.url(), nil
		}
	}
	sleeper := &recordingSleeper{}
	s := NewHeadSubscriber("", WithHeadLogger(logger.Nop()), WithHeadURL(resolve),
		WithHeadBackoff(testMinBackoff, testMaxBackoff), WithHeadPingInterval(testPing),
		withHeadSleep(sleeper.sleep), WithHeadHTTPClient(&http.Client{Timeout: 2 * time.Second}))
	got := make(chan Head, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	go func() {
		s.Run(ctx, func(h Head) { got <- h })
		close(finished)
	}()
	if h := waitHead(t, got); h.Number != 100 {
		t.Fatalf("first endpoint: %+v", h)
	}
	// The endpoint is disabled: the resolver reports nothing usable, which
	// is retried rather than fatal, and the next attempt binds the other
	// endpoint.
	if h := waitHead(t, got); h.Number != 200 {
		t.Fatalf("rebound endpoint: %+v", h)
	}
	if first.acceptCount() != 1 || second.acceptCount() != 1 {
		t.Fatalf("accepts: first %d second %d", first.acceptCount(), second.acceptCount())
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
