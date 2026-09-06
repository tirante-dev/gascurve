package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/db/dbtest"
	"github.com/tirante-dev/gascurve/internal/logger"
	"github.com/tirante-dev/gascurve/internal/model"
)

func bigInt(v int64) *big.Int { return big.NewInt(v) }

type wsHarness struct {
	store *dbtest.MemStore
	hub   *Hub
	ts    *httptest.Server
}

func newWSHarness(t *testing.T, store *dbtest.MemStore, ping time.Duration) *wsHarness {
	t.Helper()
	hub := NewHub(store, logger.Nop(), WithPingInterval(ping), WithOrigins([]string{"http://localhost:3000"}))
	cfg := config.ServerConfig{CORSOrigins: []string{"http://localhost:3000"}, RateLimitPerSecond: 1000, RateLimitBurst: 1000}
	s := New(store, cfg, hub, logger.Nop(), WithClock(func() time.Time { return now }))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &wsHarness{store: store, hub: hub, ts: ts}
}

func (h *wsHarness) dial(t *testing.T, network string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(h.ts.URL, "http") + "/api/v1/ws?network=" + network
	conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{"http://localhost:3000"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func readMsg(t *testing.T, conn *websocket.Conn) (string, json.RawMessage) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return m.Type, m.Data
}

func send(t *testing.T, conn *websocket.Conn, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := conn.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func liveNotification(chainID uint64) db.Notification {
	snap := model.LiveSnapshot{ChainID: chainID, BaseFee: "42", Constraints: []model.Constraint{}}
	b, _ := json.Marshal(snap)
	return db.Notification{Channel: db.ChannelLive, Payload: string(b)}
}

func ownerNotification(chainID, block, logIndex uint64, tx, method string) db.Notification {
	n := model.OwnerActionNotification{ChainID: chainID, LogIndex: logIndex, Action: model.OwnerAction{Block: block, TxHash: tx, Method: method, Args: json.RawMessage(`{}`)}}
	b, _ := json.Marshal(n)
	return db.Notification{Channel: db.ChannelOwnerAction, Payload: string(b)}
}

func TestWebSocketFlow(t *testing.T) {
	store := seed(t)
	h := newWSHarness(t, store, time.Hour)
	conn := h.dial(t, "robinhood")

	typ, data := readMsg(t, conn)
	var hello struct {
		Network      model.Network      `json:"network"`
		Snapshot     model.LiveSnapshot `json:"snapshot"`
		RecentBlocks []model.BlockPoint `json:"recentBlocks"`
	}
	if typ != "hello" {
		t.Fatalf("first message %s", typ)
	}
	if err := json.Unmarshal(data, &hello); err != nil {
		t.Fatal(err)
	}
	if hello.Network.ChainID != robinhood || hello.Snapshot.Block.Number != 1030 || len(hello.RecentBlocks) != 30 || hello.RecentBlocks[0].Number != 1001 {
		t.Fatalf("hello: %+v", hello)
	}
	if h.hub.ClientCount(robinhood) != 1 {
		t.Fatal("client not subscribed")
	}

	// A new block lands and the collector ticks: tick then blocks.
	ctx := context.Background()
	if err := store.UpsertBlocks(ctx, []db.Block{{ChainID: robinhood, Number: 1031, TS: now, GasUsed: 1, BaseFee: db.WeiFromUint64(1), PredictedBaseFee: db.WeiFromUint64(1), Backlogs: nil}}); err != nil {
		t.Fatal(err)
	}
	h.hub.Handle(ctx, liveNotification(robinhood))
	typ, data = readMsg(t, conn)
	var tick model.LiveSnapshot
	if typ != "tick" || json.Unmarshal(data, &tick) != nil || tick.BaseFee != "42" {
		t.Fatalf("tick: %s %s", typ, data)
	}
	typ, data = readMsg(t, conn)
	var blocks []model.BlockPoint
	if typ != "blocks" || json.Unmarshal(data, &blocks) != nil || len(blocks) != 1 || blocks[0].Number != 1031 || blocks[0].Backlogs == nil {
		t.Fatalf("blocks: %s %s", typ, data)
	}
	// A tick without new blocks only sends the tick.
	h.hub.Handle(ctx, liveNotification(robinhood))
	if typ, _ = readMsg(t, conn); typ != "tick" {
		t.Fatalf("second tick: %s", typ)
	}
	// Ticks for other networks are not delivered.
	h.hub.Handle(ctx, liveNotification(testnet))

	// Owner actions are forwarded once, whatever the notification count.
	action := model.OwnerActionNotification{ChainID: robinhood, LogIndex: 3, Action: model.OwnerAction{Block: 2000, TxHash: "0xnew", Method: "setSpeedLimit", Args: json.RawMessage(`{"limit":1}`)}}
	payload, _ := json.Marshal(action)
	h.hub.Handle(ctx, db.Notification{Channel: db.ChannelOwnerAction, Payload: string(payload)})
	h.hub.Handle(ctx, db.Notification{Channel: db.ChannelOwnerAction, Payload: string(payload)})
	typ, data = readMsg(t, conn)
	var oa model.OwnerAction
	if typ != "owner_action" || json.Unmarshal(data, &oa) != nil || oa.Method != "setSpeedLimit" {
		t.Fatalf("owner action: %s %s", typ, data)
	}
	// Subscribing to the network already followed is a no-op.
	send(t, conn, map[string]string{"type": "subscribe", "network": "4663"})
	h.hub.Handle(ctx, liveNotification(robinhood))
	if typ, _ = readMsg(t, conn); typ != "tick" {
		t.Fatalf("same-network subscribe must not send a hello: %s", typ)
	}

	// Switching networks yields a fresh hello and moves the subscription.
	send(t, conn, map[string]string{"type": "subscribe", "network": "robinhood-testnet"})
	typ, data = readMsg(t, conn)
	// The hub already holds a testnet tick, so hello carries that snapshot.
	if typ != "hello" || json.Unmarshal(data, &hello) != nil || hello.Network.ChainID != testnet || hello.Network.Model != model.ModelLegacy || hello.Snapshot.ChainID != testnet || hello.Snapshot.BaseFee != "42" {
		t.Fatalf("hello after subscribe: %s %s", typ, data)
	}
	if h.hub.ClientCount(robinhood) != 0 || h.hub.ClientCount(testnet) != 1 {
		t.Fatal("subscription not moved")
	}
	h.hub.Handle(ctx, liveNotification(testnet))
	if typ, _ = readMsg(t, conn); typ != "tick" {
		t.Fatalf("testnet tick: %s", typ)
	}
	// Unknown networks produce an error message but keep the socket, and
	// the lookup is cached: no store query for a repeated reference.
	send(t, conn, map[string]string{"type": "subscribe", "network": "nope"})
	if typ, _ = readMsg(t, conn); typ != "error" {
		t.Fatalf("error message: %s", typ)
	}
	store.SetFailure("NetworkByRef", true)
	send(t, conn, map[string]string{"type": "subscribe", "network": "nope"})
	typ, raw := readMsg(t, conn)
	if typ != "error" {
		t.Fatalf("cached unknown network: %s %s", typ, raw)
	}
	// An uncached reference with a failing store is an internal error, not not_found.
	send(t, conn, map[string]string{"type": "subscribe", "network": "other"})
	if typ, raw = readMsg(t, conn); typ != "error" {
		t.Fatalf("store failure on subscribe: %s %s", typ, raw)
	}
	store.SetFailure("NetworkByRef", false)
	// Garbage and pongs are ignored.
	if err := conn.Write(ctx, websocket.MessageText, []byte("not json")); err != nil {
		t.Fatal(err)
	}
	send(t, conn, map[string]string{"type": "pong"})
	h.hub.Handle(ctx, liveNotification(testnet))
	if typ, _ = readMsg(t, conn); typ != "tick" {
		t.Fatalf("tick after garbage: %s", typ)
	}
	// Bad notifications are ignored.
	h.hub.Handle(ctx, db.Notification{Channel: db.ChannelLive, Payload: "{bad"})
	h.hub.Handle(ctx, db.Notification{Channel: db.ChannelOwnerAction, Payload: "{bad"})
	h.hub.Handle(ctx, db.Notification{Channel: "other", Payload: "{}"})
	_ = conn.Close(websocket.StatusNormalClosure, "done")
	deadline := time.Now().Add(2 * time.Second)
	for h.hub.ClientCount(testnet) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if h.hub.ClientCount(testnet) != 0 {
		t.Fatal("client not removed on close")
	}
}

func TestWebSocketPingPong(t *testing.T) {
	store := seed(t)
	h := newWSHarness(t, store, 30*time.Millisecond)
	conn := h.dial(t, "robinhood")
	if typ, _ := readMsg(t, conn); typ != "hello" {
		t.Fatalf("hello expected, got %s", typ)
	}
	// Answer the first pings, then go quiet: the server closes after two
	// missed pongs.
	for range 3 {
		if typ, _ := readMsg(t, conn); typ != "ping" {
			t.Fatalf("ping expected, got %s", typ)
		}
		send(t, conn, map[string]string{"type": "pong"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, _, err := conn.Read(ctx)
		if err == nil {
			continue
		}
		if websocket.CloseStatus(err) != websocket.StatusGoingAway {
			t.Fatalf("expected going away close, got %v", err)
		}
		return
	}
}

func TestWebSocketErrors(t *testing.T) {
	store := seed(t)
	h := newWSHarness(t, store, time.Hour)
	upgrade := http.Header{"Upgrade": []string{"websocket"}, "Connection": []string{"Upgrade"}}
	for _, tc := range []struct {
		name    string
		query   string
		headers http.Header
		status  int
		code    string
	}{
		{"missing network", "", upgrade, http.StatusBadRequest, "bad_request"},
		{"unknown network", "?network=nope", upgrade, http.StatusNotFound, "not_found"},
		{"not an upgrade", "?network=robinhood", nil, http.StatusBadRequest, "bad_request"},
		{"bad origin", "?network=robinhood", http.Header{"Upgrade": []string{"websocket"}, "Origin": []string{"http://evil.example"}}, http.StatusForbidden, "forbidden"},
	} {
		req, _ := http.NewRequest(http.MethodGet, h.ts.URL+"/api/v1/ws"+tc.query, http.NoBody)
		req.Header = tc.headers
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var e model.ErrorBody
		decode(t, mustRead(t, resp), &e)
		if resp.StatusCode != tc.status || e.Error.Code != tc.code {
			t.Fatalf("%s: status %d code %q", tc.name, resp.StatusCode, e.Error.Code)
		}
	}
	store.SetFailure("NetworkByRef", true)
	req, _ := http.NewRequest(http.MethodGet, h.ts.URL+"/api/v1/ws?network=uncached", http.NoBody)
	req.Header = upgrade
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var e model.ErrorBody
	decode(t, mustRead(t, resp), &e)
	if resp.StatusCode != http.StatusInternalServerError || e.Error.Code != "internal" {
		t.Fatalf("store failure: %d %+v", resp.StatusCode, e)
	}
	store.SetFailure("NetworkByRef", false)
	// Hello failures send a protocol error and close the socket.
	store.SetFailure("LatestStateSample", true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(h.ts.URL, "http") + "/api/v1/ws?network=robinhood"
	conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{"http://localhost:3000"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	_, data, err := conn.Read(ctx)
	if err != nil || !strings.Contains(string(data), `"type":"error"`) || !strings.Contains(string(data), `"internal"`) {
		t.Fatalf("expected an error message before the close: %s %v", data, err)
	}
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("expected close")
	}
	deadline := time.Now().Add(2 * time.Second)
	for h.hub.Connections() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if h.hub.Connections() != 0 {
		t.Fatalf("connection slot leaked: %d", h.hub.Connections())
	}
	store.SetFailure("LatestStateSample", false)
	// A failed switch reports an internal error and keeps the old
	// subscription.
	conn = h.dial(t, "robinhood")
	if typ, _ := readMsg(t, conn); typ != "hello" {
		t.Fatal("hello")
	}
	store.SetFailure("RecentBlocks", true)
	store.SetFailure("BlocksAfter", true)
	send(t, conn, map[string]string{"type": "subscribe", "network": "robinhood-testnet"})
	if typ, _ := readMsg(t, conn); typ != "error" {
		t.Fatalf("switch failure must be reported: %s", typ)
	}
	h.hub.Handle(ctx, liveNotification(robinhood))
	if typ, _ := readMsg(t, conn); typ != "tick" {
		t.Fatalf("still subscribed to robinhood: %s", typ)
	}
	store.SetFailure("RecentBlocks", false)
	store.SetFailure("BlocksAfter", false)
	// Hub without a snapshot and no data for the network says null.
	empty := dbtest.New()
	if err := empty.UpsertNetwork(context.Background(), db.Network{ChainID: 9, Name: "empty"}); err != nil {
		t.Fatal(err)
	}
	h2 := newWSHarness(t, empty, time.Hour)
	conn2 := h2.dial(t, "empty")
	typ, data := readMsg(t, conn2)
	if typ != "hello" || !strings.Contains(string(data), `"snapshot":null`) || !strings.Contains(string(data), `"recentBlocks":[]`) {
		t.Fatalf("empty hello: %s %s", typ, data)
	}
	// A slow consumer is closed instead of blocking the hub.
	slow := &client{send: make(chan []byte, 1), closed: make(chan struct{})}
	slow.enqueue([]byte("a"))
	slow.enqueue([]byte("b"))
	select {
	case <-slow.closed:
	default:
		t.Fatal("slow consumer not closed")
	}
	slow.close() // idempotent
}

func mustRead(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestWebSocketConnectionCaps: sockets are capped per address and in
// total, slots are released on close, and a client that floods messages
// gets one error and is disconnected.
func TestWebSocketConnectionCaps(t *testing.T) {
	store := seed(t)
	hub := NewHub(store, logger.Nop(), WithPingInterval(time.Hour), WithOrigins(nil), WithConnectionLimits(2, 3))
	cfg := config.ServerConfig{RateLimitPerSecond: 1000, RateLimitBurst: 1000, WSMaxPerIP: 2, WSMaxTotal: 3}
	s := New(store, cfg, hub, logger.Nop(), WithClock(func() time.Time { return now }))
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	h := &wsHarness{store: store, hub: hub, ts: ts}
	c1 := h.dial(t, "robinhood")
	c2 := h.dial(t, "robinhood")
	readMsg(t, c1)
	readMsg(t, c2)
	if hub.Connections() != 2 {
		t.Fatalf("connections = %d", hub.Connections())
	}
	// Third socket from the same address: refused with the JSON envelope.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/ws?network=robinhood", http.NoBody)
	req.Header.Set("Upgrade", "websocket")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var e model.ErrorBody
	decode(t, mustRead(t, resp), &e)
	if resp.StatusCode != http.StatusTooManyRequests || e.Error.Code != "rate_limited" || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("per-ip cap: %d %+v", resp.StatusCode, e)
	}
	// Another address is admitted until the total cap.
	if ok, _ := hub.admit("other"); !ok {
		t.Fatal("other address should be admitted")
	}
	if ok, reason := hub.admit("third"); ok || !strings.Contains(reason, "too many connections") {
		t.Fatalf("total cap: %v %q", ok, reason)
	}
	hub.release("other")
	// Closing a socket frees its slot.
	_ = c2.Close(websocket.StatusNormalClosure, "bye")
	deadline := time.Now().Add(2 * time.Second)
	for hub.Connections() != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if hub.Connections() != 1 {
		t.Fatalf("slot not released: %d", hub.Connections())
	}
	// A message flood: the burst is answered, then one error and a close.
	for range msgBurst + 5 {
		send(t, c1, map[string]string{"type": "pong"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sawError := false
	for {
		_, data, err := c1.Read(ctx)
		if err != nil {
			break
		}
		if strings.Contains(string(data), `"rate_limited"`) {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("flooding client must get a rate_limited error before the close")
	}
	hub.setLimits(0, 0)
	if hub.maxPerIP != 2 || hub.maxTotal != 3 {
		t.Fatal("zero limits keep the previous values")
	}
}

// TestWebSocketHelloAtomic: a tick that lands while a hello is being
// prepared is delivered after the hello, once, and never lost; concurrent
// refreshes never duplicate blocks or move the ring head backwards.
func TestWebSocketHelloAtomic(t *testing.T) {
	store := seed(t)
	hub := NewHub(store, logger.Nop(), WithPingInterval(time.Hour), WithOrigins(nil))
	s := New(store, config.ServerConfig{RateLimitPerSecond: 1000, RateLimitBurst: 1000}, hub, logger.Nop(), WithClock(func() time.Time { return now }))
	ctx := context.Background()
	// Slow the network model so a tick can interleave with the hello.
	gate := make(chan struct{})
	hub.network = func(ctx context.Context, n db.Network) (model.Network, error) {
		<-gate
		return s.networkModel(ctx, n)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	h := &wsHarness{store: store, hub: hub, ts: ts}
	conn := h.dial(t, "robinhood")
	// While the hello waits, a new block and a tick arrive.
	time.Sleep(20 * time.Millisecond)
	if err := store.UpsertBlocks(ctx, []db.Block{{ChainID: robinhood, Number: 1031, TS: now, BaseFee: db.WeiFromUint64(1), PredictedBaseFee: db.WeiFromUint64(1)}}); err != nil {
		t.Fatal(err)
	}
	go hub.Handle(ctx, liveNotification(robinhood))
	time.Sleep(20 * time.Millisecond)
	close(gate)
	typ, data := readMsg(t, conn)
	if typ != "hello" {
		t.Fatalf("first message %s", typ)
	}
	var hello struct {
		RecentBlocks []model.BlockPoint `json:"recentBlocks"`
	}
	if err := json.Unmarshal(data, &hello); err != nil {
		t.Fatal(err)
	}
	helloLast := hello.RecentBlocks[len(hello.RecentBlocks)-1].Number
	// Whatever the interleaving, the client sees every block exactly once:
	// either in the hello or in a following blocks message.
	seen := map[uint64]int{}
	for _, b := range hello.RecentBlocks {
		seen[b.Number]++
	}
	hub.Handle(ctx, liveNotification(robinhood))
	deadline := time.Now().Add(2 * time.Second)
	for seen[1031] == 0 && time.Now().Before(deadline) {
		typ, data := readMsg(t, conn)
		if typ != "blocks" {
			continue
		}
		var blocks []model.BlockPoint
		if err := json.Unmarshal(data, &blocks); err != nil {
			t.Fatal(err)
		}
		for _, b := range blocks {
			if b.Number <= helloLast {
				t.Fatalf("block %d delivered twice (hello ended at %d)", b.Number, helloLast)
			}
			seen[b.Number]++
		}
	}
	if seen[1031] != 1 {
		t.Fatalf("block 1031 seen %d times", seen[1031])
	}
	// Concurrent refreshes: the ring head only moves forward and no block
	// is appended twice.
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_ = store.UpsertBlocks(ctx, []db.Block{{ChainID: robinhood, Number: 1040 + uint64(i), TS: now, BaseFee: db.WeiFromUint64(1), PredictedBaseFee: db.WeiFromUint64(1)}})
			}
			_, _ = hub.refreshBlocks(ctx, robinhood)
		}(i)
	}
	wg.Wait()
	hub.mu.Lock()
	ring := append([]model.BlockPoint(nil), hub.networks[robinhood].blocks...)
	last := hub.networks[robinhood].last
	hub.mu.Unlock()
	for i := 1; i < len(ring); i++ {
		if ring[i].Number <= ring[i-1].Number {
			t.Fatalf("ring not strictly ascending: %d after %d", ring[i].Number, ring[i-1].Number)
		}
	}
	if last != ring[len(ring)-1].Number {
		t.Fatalf("last %d vs ring end %d", last, ring[len(ring)-1].Number)
	}
}

// TestHubReconcile: after the LISTEN connection reconnects, blocks stored
// meanwhile are fanned out and owner actions since the per-network cursor
// are delivered once, de-duplicated by (block, tx hash, log index).
func TestHubReconcile(t *testing.T) {
	store := seed(t)
	h := newWSHarness(t, store, time.Hour)
	conn := h.dial(t, "robinhood")
	readMsg(t, conn)
	ctx := context.Background()
	// The cursor starts at the newest stored action (block 174150), so
	// history is not replayed; a new action and a new block land while
	// the listener is down.
	if _, err := store.InsertOwnerActions(ctx, []db.OwnerAction{
		{ChainID: robinhood, BlockNumber: 174151, TxHash: "0xcc", LogIndex: 0, TS: now, Method: "setSpeedLimit", Args: db.JSONB(`{"limit":2}`)},
		{ChainID: robinhood, BlockNumber: 174151, TxHash: "0xcc", LogIndex: 1, TS: now, Method: "setL2GasPricingInertia", Args: db.JSONB(`{"sec":3}`)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBlocks(ctx, []db.Block{{ChainID: robinhood, Number: 1031, TS: now, BaseFee: db.WeiFromUint64(1), PredictedBaseFee: db.WeiFromUint64(1)}}); err != nil {
		t.Fatal(err)
	}
	// One of the two was already delivered by NOTIFY before the outage.
	h.hub.Handle(ctx, ownerNotification(robinhood, 174151, 0, "0xcc", "setSpeedLimit"))
	if typ, _ := readMsg(t, conn); typ != "owner_action" {
		t.Fatalf("notify: %s", typ)
	}
	h.hub.Handle(ctx, db.Notification{Reconnected: true})
	got := map[string]int{}
	for range 2 {
		typ, data := readMsg(t, conn)
		got[typ]++
		if typ == "owner_action" && !strings.Contains(string(data), "setL2GasPricingInertia") {
			t.Fatalf("the already delivered action must not repeat: %s", data)
		}
	}
	if got["blocks"] != 1 || got["owner_action"] != 1 {
		t.Fatalf("reconcile delivered %v", got)
	}
	// A second reconnect delivers nothing new.
	h.hub.Handle(ctx, db.Notification{Reconnected: true})
	h.hub.Handle(ctx, liveNotification(robinhood))
	if typ, _ := readMsg(t, conn); typ != "tick" {
		t.Fatalf("second reconcile must be silent: %s", typ)
	}
	// Store failures during a reconcile are logged, not fatal; networks
	// without clients are skipped.
	store.SetFailure("OwnerActionsSince", true)
	store.SetFailure("BlocksAfter", true)
	h.hub.Handle(ctx, db.Notification{Reconnected: true})
	store.SetFailure("OwnerActionsSince", false)
	store.SetFailure("BlocksAfter", false)
	h.hub.Handle(ctx, liveNotification(robinhood))
	if typ, _ := readMsg(t, conn); typ != "tick" {
		t.Fatalf("after failed reconcile: %s", typ)
	}
	// The seen set is bounded.
	st := h.hub.state(robinhood)
	h.hub.mu.Lock()
	for i := range seenActions + 10 {
		st.markSeen(actionKey{block: uint64(i), txHash: "x"})
	}
	n := len(st.seen)
	h.hub.mu.Unlock()
	if n != seenActions {
		t.Fatalf("seen set = %d", n)
	}
	// Cursor initialization tolerates a store failure.
	empty := dbtest.New()
	hub2 := NewHub(empty, nil)
	empty.SetFailure("OwnerActions", true)
	hub2.initOwnerCursor(ctx, 1)
	hub2.mu.Lock()
	init := hub2.state(1).ownerInit
	hub2.mu.Unlock()
	if init {
		t.Fatal("cursor must not be marked initialized after a failure")
	}
}

type fakeListener struct {
	ch chan db.Notification
}

func (f *fakeListener) Notifications() <-chan db.Notification { return f.ch }
func (f *fakeListener) Close() error                          { return nil }

func TestHubRun(t *testing.T) {
	store := seed(t)
	hub := NewHub(store, nil)
	hub.live = func(context.Context, uint64) (*model.LiveSnapshot, error) { return nil, errors.New("x") }
	l := &fakeListener{ch: make(chan db.Notification, 2)}
	l.ch <- liveNotification(robinhood)
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		hub.Run(ctx, l)
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		hub.mu.Lock()
		got := hub.networks[robinhood] != nil && hub.networks[robinhood].snapshot != nil
		hub.mu.Unlock()
		if got {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	hub.mu.Lock()
	if hub.networks[robinhood] == nil || hub.networks[robinhood].last != 1030 {
		t.Fatalf("hub state: %+v", hub.networks[robinhood])
	}
	hub.mu.Unlock()
	cancel()
	<-done
	// Closing the listener channel also ends Run.
	l2 := &fakeListener{ch: make(chan db.Notification)}
	close(l2.ch)
	hub.Run(context.Background(), l2)
	// The ring is capped.
	ctx2 := context.Background()
	var blocks []db.Block
	for i := uint64(0); i < ringSize+50; i++ {
		blocks = append(blocks, db.Block{ChainID: robinhood, Number: 2000 + i, TS: now, BaseFee: db.WeiFromUint64(1), PredictedBaseFee: db.WeiFromUint64(1)})
	}
	if err := store.UpsertBlocks(ctx2, blocks); err != nil {
		t.Fatal(err)
	}
	hub.Handle(ctx2, liveNotification(robinhood))
	hub.Handle(ctx2, liveNotification(robinhood))
	hub.mu.Lock()
	n := len(hub.networks[robinhood].blocks)
	hub.mu.Unlock()
	if n != ringSize {
		t.Fatalf("ring size = %d", n)
	}
}
