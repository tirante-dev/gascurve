package api

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
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

	// Owner actions are forwarded.
	action := model.OwnerActionNotification{ChainID: robinhood, Action: model.OwnerAction{Block: 5, Method: "setSpeedLimit", Args: json.RawMessage(`{"limit":1}`)}}
	payload, _ := json.Marshal(action)
	h.hub.Handle(ctx, db.Notification{Channel: db.ChannelOwnerAction, Payload: string(payload)})
	typ, data = readMsg(t, conn)
	var oa model.OwnerAction
	if typ != "owner_action" || json.Unmarshal(data, &oa) != nil || oa.Method != "setSpeedLimit" {
		t.Fatalf("owner action: %s %s", typ, data)
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
	// Unknown networks produce an error message but keep the socket.
	send(t, conn, map[string]string{"type": "subscribe", "network": "nope"})
	if typ, _ = readMsg(t, conn); typ != "error" {
		t.Fatalf("error message: %s", typ)
	}
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
	for _, tc := range []struct {
		query  string
		status int
	}{
		{"", http.StatusBadRequest},
		{"?network=nope", http.StatusNotFound},
	} {
		resp, err := http.Get(h.ts.URL + "/api/v1/ws" + tc.query)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Fatalf("%q: status %d", tc.query, resp.StatusCode)
		}
	}
	store.SetFailure("NetworkByRef", true)
	resp, err := http.Get(h.ts.URL + "/api/v1/ws?network=robinhood")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("store failure: %d", resp.StatusCode)
	}
	store.SetFailure("NetworkByRef", false)
	// A plain GET without the upgrade headers is rejected by the accept.
	resp, err = http.Get(h.ts.URL + "/api/v1/ws?network=robinhood")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("expected upgrade failure")
	}
	// Hello failures close the socket.
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
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("expected close")
	}
	store.SetFailure("LatestStateSample", false)
	// Subscribe hello failures are logged and ignored.
	conn = h.dial(t, "robinhood")
	if typ, _ := readMsg(t, conn); typ != "hello" {
		t.Fatal("hello")
	}
	store.SetFailure("RecentBlocks", true)
	store.SetFailure("BlocksAfter", true)
	send(t, conn, map[string]string{"type": "subscribe", "network": "robinhood-testnet"})
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
