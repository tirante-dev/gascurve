package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
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

func readMsg(t *testing.T, conn *websocket.Conn) (typ string, data json.RawMessage) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
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
	snap := model.LiveSnapshot{ChainID: chainID, SampledAt: now.Format(time.RFC3339), BaseFee: "42", Constraints: []model.Constraint{}}
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
	// The hub's snapshot is the same shape as /live, spot included.
	if hello.Snapshot.EthUsd == nil || hello.Snapshot.EthUsd.Price != "4523.40" {
		t.Fatalf("hello snapshot spot: %+v", hello.Snapshot.EthUsd)
	}
	if h.hub.ClientCount(robinhood) != 1 {
		t.Fatal("client not subscribed")
	}

	// A new block lands and the collector ticks: tick then blocks.
	ctx := context.Background()
	if err := store.UpsertBlocks(ctx, []db.Block{{ChainID: robinhood, Number: 1031, TS: now, GasUsed: 1, BaseFee: db.WeiFromUint64(1), PredictedBaseFee: db.NullWeiFromUint64(1), Backlogs: nil}}); err != nil {
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
		// Malformed handshakes get the JSON envelope, never the upgrade
		// library's plaintext.
		{"no connection token", "?network=robinhood", http.Header{"Upgrade": []string{"websocket"}, "Sec-WebSocket-Version": []string{"13"}, "Sec-WebSocket-Key": []string{"dGhlIHNhbXBsZSBub25jZQ=="}}, http.StatusBadRequest, "bad_request"},
		{"bad version", "?network=robinhood", http.Header{"Upgrade": []string{"websocket"}, "Connection": []string{"keep-alive, Upgrade"}, "Sec-WebSocket-Version": []string{"12"}, "Sec-WebSocket-Key": []string{"dGhlIHNhbXBsZSBub25jZQ=="}}, http.StatusBadRequest, "bad_request"},
		{"bad key", "?network=robinhood", http.Header{"Upgrade": []string{"websocket"}, "Connection": []string{"Upgrade"}, "Sec-WebSocket-Version": []string{"13"}, "Sec-WebSocket-Key": []string{"nope"}}, http.StatusBadRequest, "bad_request"},
		{"missing key", "?network=robinhood", http.Header{"Upgrade": []string{"websocket"}, "Connection": []string{"Upgrade"}, "Sec-WebSocket-Version": []string{"13"}}, http.StatusBadRequest, "bad_request"},
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
	if err := empty.UpsertNetwork(context.Background(), db.Network{ChainID: 9, Name: "empty", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	h2 := newWSHarness(t, empty, time.Hour)
	conn2 := h2.dial(t, "empty")
	typ, data := readMsg(t, conn2)
	if typ != "hello" || !strings.Contains(string(data), `"snapshot":null`) || !strings.Contains(string(data), `"recentBlocks":[]`) {
		t.Fatalf("empty hello: %s %s", typ, data)
	}
	// A slow consumer is dropped instead of blocking the hub, and nothing
	// more is queued for it: a closing client accepts no further messages,
	// so the write loop's flush is bounded by what is already there.
	// The hub is the client's owner: it is what counts the drop, so even a
	// hand-built client gets one.
	slow := &client{hub: NewHub(nil, nil), send: make(chan []byte, 1), closed: make(chan struct{})}
	slow.enqueue([]byte("a"))
	slow.enqueue([]byte("b"))
	select {
	case <-slow.closed:
	default:
		t.Fatal("slow consumer not closed")
	}
	if !slow.dropped.Load() {
		t.Fatal("an overflowed queue must mark the client dropped")
	}
	<-slow.send
	slow.enqueue([]byte("c"))
	select {
	case msg := <-slow.send:
		t.Fatalf("a closing client must accept nothing more: %s", msg)
	default:
	}
	slow.close() // idempotent
	// Handshake validation covers HTTP/1.0 too.
	old := httptest.NewRequest(http.MethodGet, "/ws", http.NoBody)
	old.Proto, old.ProtoMajor, old.ProtoMinor = "HTTP/1.0", 1, 0
	if handshakeError(old) == "" {
		t.Fatal("HTTP/1.0 handshake")
	}
}

// TestClientWatermark: a client never receives a block its hello (or an
// earlier blocks message) already carried, whatever refresh produced it.
func TestClientWatermark(t *testing.T) {
	c := &client{send: make(chan []byte, 4), closed: make(chan struct{}), watermark: 5}
	c.deliver(outbound{blocks: []model.BlockPoint{{Number: 4}, {Number: 5}}})
	if len(c.send) != 0 {
		t.Fatal("blocks at or below the watermark must be dropped")
	}
	c.deliver(outbound{blocks: []model.BlockPoint{{Number: 5}, {Number: 6}, {Number: 7}}})
	if len(c.send) != 1 || c.watermark != 7 {
		t.Fatalf("queued %d, watermark %d", len(c.send), c.watermark)
	}
	var m struct {
		Type string             `json:"type"`
		Data []model.BlockPoint `json:"data"`
	}
	if err := json.Unmarshal(<-c.send, &m); err != nil || m.Type != "blocks" || len(m.Data) != 2 || m.Data[0].Number != 6 {
		t.Fatalf("filtered message: %+v %v", m, err)
	}
	c.deliver(outbound{blocks: []model.BlockPoint{{Number: 7}}})
	if len(c.send) != 0 {
		t.Fatal("a repeated block must be dropped")
	}
}

// TestHubReorg: when the collector replaces blocks at or below the ring's
// tip, the hub truncates its ring to the common ancestor, resends the
// canonical blocks in a reorg message before the tick, and later blocks
// messages continue above the new tip without repeating any block.
func TestHubReorg(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New()
	if err := store.UpsertNetwork(ctx, db.Network{ChainID: robinhood, Name: "robinhood", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	mk := func(n uint64, tag string) db.Block {
		return db.Block{ChainID: robinhood, Number: n, Hash: "0x" + tag + strconv.FormatUint(n, 10), TS: now.Add(time.Duration(n) * time.Second), BaseFee: db.WeiFromUint64(n), PredictedBaseFee: db.NullWeiFromUint64(n)}
	}
	var blocks []db.Block
	for n := uint64(100); n <= 110; n++ {
		blocks = append(blocks, mk(n, "a"))
	}
	if err := store.UpsertBlocks(ctx, blocks); err != nil {
		t.Fatal(err)
	}
	h := newWSHarness(t, store, time.Hour)
	conn := h.dial(t, "robinhood")
	typ, data := readMsg(t, conn)
	var hello struct {
		RecentBlocks []model.BlockPoint `json:"recentBlocks"`
	}
	if typ != "hello" || json.Unmarshal(data, &hello) != nil || len(hello.RecentBlocks) != 11 || hello.RecentBlocks[10].Number != 110 {
		t.Fatalf("hello: %s %s", typ, data)
	}
	// Blocks 108..110 are replaced and 111 lands on the new fork.
	if _, err := store.DeleteBlocksAfter(ctx, robinhood, 107); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBlocks(ctx, []db.Block{mk(108, "b"), mk(109, "b"), mk(110, "b"), mk(111, "b")}); err != nil {
		t.Fatal(err)
	}
	h.hub.Handle(ctx, liveNotification(robinhood))
	typ, data = readMsg(t, conn)
	var reorg model.Reorg
	if typ != "reorg" || json.Unmarshal(data, &reorg) != nil || reorg.ChainID != robinhood || reorg.Ancestor != 107 || len(reorg.Blocks) != 4 || reorg.Blocks[0].Number != 108 || reorg.Blocks[3].Number != 111 {
		t.Fatalf("reorg: %s %s", typ, data)
	}
	if typ, _ = readMsg(t, conn); typ != "tick" {
		t.Fatalf("tick after reorg: %s", typ)
	}
	h.hub.mu.Lock()
	st := h.hub.networks[robinhood]
	tip, last := st.blocks[len(st.blocks)-1], st.last
	h.hub.mu.Unlock()
	if last != 111 || tip.hash != "0xb111" || len(st.blocks) != 12 {
		t.Fatalf("ring after reorg: last %d tip %+v len %d", last, tip, len(st.blocks))
	}
	// The next block is a plain blocks message, nothing repeated.
	if err := store.UpsertBlocks(ctx, []db.Block{mk(112, "b")}); err != nil {
		t.Fatal(err)
	}
	h.hub.Handle(ctx, liveNotification(robinhood))
	if typ, _ = readMsg(t, conn); typ != "tick" {
		t.Fatalf("tick: %s", typ)
	}
	typ, data = readMsg(t, conn)
	var more []model.BlockPoint
	if typ != "blocks" || json.Unmarshal(data, &more) != nil || len(more) != 1 || more[0].Number != 112 {
		t.Fatalf("blocks after reorg: %s %s", typ, data)
	}
	// A reorg below every ring entry replaces the whole ring (ancestor 0),
	// and a tip row that vanished is a reorg too.
	if _, err := store.DeleteBlocksAfter(ctx, robinhood, 99); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBlocks(ctx, []db.Block{mk(100, "c"), mk(101, "c")}); err != nil {
		t.Fatal(err)
	}
	h.hub.Handle(ctx, liveNotification(robinhood))
	typ, data = readMsg(t, conn)
	if typ != "reorg" || json.Unmarshal(data, &reorg) != nil || reorg.Ancestor != 0 || len(reorg.Blocks) != 2 || reorg.Blocks[1].Number != 101 {
		t.Fatalf("deep reorg: %s %s", typ, data)
	}
	readMsg(t, conn) // tick
	h.hub.mu.Lock()
	w := 0
	for c := range st.clients {
		w = int(c.watermark)
	}
	h.hub.mu.Unlock()
	if w != 0 {
		t.Fatalf("the client's watermark must drop to the ancestor: %d", w)
	}
	if _, err := store.DeleteBlocksAfter(ctx, robinhood, 100); err != nil {
		t.Fatal(err)
	}
	h.hub.Handle(ctx, liveNotification(robinhood))
	typ, data = readMsg(t, conn)
	if typ != "reorg" || json.Unmarshal(data, &reorg) != nil || reorg.Ancestor != 100 || len(reorg.Blocks) != 0 || !strings.Contains(string(data), `"blocks":[]`) {
		t.Fatalf("vanished tip: %s %s", typ, data)
	}
	// A store failure during the reorg rebuild surfaces and leaves the
	// ring alone.
	if _, err := store.DeleteBlocksAfter(ctx, robinhood, 99); err != nil {
		t.Fatal(err)
	}
	store.SetFailure("RecentBlocks", true)
	if _, err := h.hub.refreshBlocks(ctx, robinhood); err == nil {
		t.Fatal("expected a store error")
	}
	store.SetFailure("RecentBlocks", false)
	h.hub.mu.Lock()
	last = st.last
	h.hub.mu.Unlock()
	if last != 100 {
		t.Fatalf("ring must survive a failed rebuild: %d", last)
	}
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
	// Third socket from the same address: a well-formed handshake refused
	// with the JSON envelope.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/ws?network=robinhood", http.NoBody)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
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

// TestWebSocketConnectionCapsUseRealIP verifies that the HTTP real-IP
// middleware also provides the identity used by the WebSocket per-IP cap.
// Trusted ingress clients get separate counts, while an untrusted peer
// cannot choose a count by forging X-Forwarded-For.
func TestWebSocketConnectionCapsUseRealIP(t *testing.T) {
	for _, tc := range []struct {
		name          string
		trusted       []string
		secondAllowed bool
	}{
		{name: "trusted ingress", trusted: []string{"127.0.0.1", "::1"}, secondAllowed: true},
		{name: "untrusted forwarding header", secondAllowed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := seed(t)
			hub := NewHub(store, logger.Nop(), WithPingInterval(time.Hour), WithOrigins(nil), WithConnectionLimits(1, 4))
			cfg := config.ServerConfig{
				RateLimitPerSecond: 1000,
				RateLimitBurst:     1000,
				TrustedProxies:     tc.trusted,
				WSMaxPerIP:         1,
				WSMaxTotal:         4,
			}
			s := New(store, cfg, hub, logger.Nop(), WithClock(func() time.Time { return now }))
			ts := httptest.NewServer(s.Handler())
			defer ts.Close()

			dial := func(ip string) (*websocket.Conn, *http.Response, error) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/ws?network=robinhood"
				return websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"X-Forwarded-For": []string{ip}}})
			}

			first, resp, err := dial("198.51.100.1")
			if err != nil {
				t.Fatalf("first dial: %v", err)
			}
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
			defer first.CloseNow()
			readMsg(t, first)

			second, resp, err := dial("198.51.100.2")
			if tc.secondAllowed {
				if err != nil {
					t.Fatalf("second client dial: %v", err)
				}
				if resp != nil && resp.Body != nil {
					resp.Body.Close()
				}
				defer second.CloseNow()
				readMsg(t, second)

				third, refused, err := dial("198.51.100.2")
				if third != nil {
					third.CloseNow()
				}
				if err == nil || refused == nil || refused.StatusCode != http.StatusTooManyRequests {
					t.Fatalf("same client cap: err %v response %+v", err, refused)
				}
				if refused.Body != nil {
					refused.Body.Close()
				}
				return
			}

			if second != nil {
				second.CloseNow()
			}
			if err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("untrusted forwarded address bypassed cap: err %v response %+v", err, resp)
			}
			if resp.Body != nil {
				resp.Body.Close()
			}
		})
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
	if err := store.UpsertBlocks(ctx, []db.Block{{ChainID: robinhood, Number: 1031, TS: now, BaseFee: db.WeiFromUint64(1), PredictedBaseFee: db.NullWeiFromUint64(1)}}); err != nil {
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
				_ = store.UpsertBlocks(ctx, []db.Block{{ChainID: robinhood, Number: 1040 + uint64(i), TS: now, BaseFee: db.WeiFromUint64(1), PredictedBaseFee: db.NullWeiFromUint64(1)}})
			}
			_, _ = hub.refreshBlocks(ctx, robinhood)
		}(i)
	}
	wg.Wait()
	hub.mu.Lock()
	ring := points(hub.networks[robinhood].blocks)
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
	if err := store.UpsertBlocks(ctx, []db.Block{{ChainID: robinhood, Number: 1031, TS: now, BaseFee: db.WeiFromUint64(1), PredictedBaseFee: db.NullWeiFromUint64(1)}}); err != nil {
		t.Fatal(err)
	}
	// One of the two was already delivered by NOTIFY before the outage.
	h.hub.Handle(ctx, ownerNotification(robinhood, 174151, 0, "0xcc", "setSpeedLimit"))
	if typ, _ := readMsg(t, conn); typ != "owner_action" {
		t.Fatalf("notify: %s", typ)
	}
	// The hub has never seen a tick for this network: the reconcile
	// rebuilds the snapshot from the latest sample and sends it as a tick.
	h.hub.Handle(ctx, db.Notification{Reconnected: true})
	got := map[string]int{}
	for range 3 {
		typ, data := readMsg(t, conn)
		got[typ]++
		if typ == "owner_action" && !strings.Contains(string(data), "setL2GasPricingInertia") {
			t.Fatalf("the already delivered action must not repeat: %s", data)
		}
		if typ == "tick" && !strings.Contains(string(data), `"number":1030`) {
			t.Fatalf("the reconciled tick must be the latest sample: %s", data)
		}
	}
	if got["blocks"] != 1 || got["owner_action"] != 1 || got["tick"] != 1 {
		t.Fatalf("reconcile delivered %v", got)
	}
	// A second reconnect delivers nothing new: the snapshot is not newer.
	h.hub.Handle(ctx, db.Notification{Reconnected: true})
	h.hub.Handle(ctx, liveNotification(robinhood))
	if typ, _ := readMsg(t, conn); typ != "tick" {
		t.Fatalf("second reconcile must be silent: %s", typ)
	}
	// A sample committed while the listener was down is made up for.
	if err := store.InsertStateSample(ctx, db.StateSample{ChainID: robinhood, SampledAt: now.Add(time.Second), BlockNumber: 1031, BaseFee: db.WeiFromUint64(7), MinBaseFee: db.WeiFromUint64(1), Constraints: db.JSONB(`[]`), Prices: db.JSONB(`{}`)}); err != nil {
		t.Fatal(err)
	}
	h.hub.Handle(ctx, db.Notification{Reconnected: true})
	typ, data := readMsg(t, conn)
	if typ != "tick" || !strings.Contains(string(data), `"baseFee":"7"`) {
		t.Fatalf("missed tick must be reconciled: %s %s", typ, data)
	}
	h.hub.Handle(ctx, db.Notification{Reconnected: true})
	h.hub.Handle(ctx, liveNotification(robinhood))
	if typ, data := readMsg(t, conn); typ != "tick" || !strings.Contains(string(data), `"baseFee":"42"`) {
		t.Fatalf("the reconciled snapshot must not repeat: %s %s", typ, data)
	}
	// The synthetic notification above dated the cache before the sample,
	// so the sample is offered again; an unparsable sampledAt is ignored.
	if u := h.hub.newerSnapshot(ctx, robinhood); u == nil || !strings.Contains(string(u.raw), `"baseFee":"7"`) {
		t.Fatalf("update expected: %+v", u)
	}
	h.hub.live = func(context.Context, uint64) (*model.LiveSnapshot, error) {
		return &model.LiveSnapshot{SampledAt: "bad"}, nil
	}
	if u := h.hub.newerSnapshot(ctx, robinhood); u != nil || u.payload() != nil {
		t.Fatal("unparsable snapshot time")
	}
	h.hub.live = func(context.Context, uint64) (*model.LiveSnapshot, error) { return nil, errors.New("boom") }
	if u := h.hub.newerSnapshot(ctx, robinhood); u != nil {
		t.Fatal("snapshot failure is logged, not delivered")
	}
	h.hub.live = func(context.Context, uint64) (*model.LiveSnapshot, error) { return nil, errNoData }
	if u := h.hub.newerSnapshot(ctx, robinhood); u != nil {
		t.Fatal("no data is not an update")
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
	ch     chan db.Notification
	status db.ListenerStatus
}

func (f *fakeListener) Notifications() <-chan db.Notification { return f.ch }
func (f *fakeListener) Status() db.ListenerStatus             { return f.status }
func (f *fakeListener) Close() error                          { return nil }

func TestHubRun(t *testing.T) {
	store := seed(t)
	hub := NewHub(store, nil)
	hub.live = func(context.Context, uint64) (*model.LiveSnapshot, error) { return nil, errors.New("x") }
	l := &fakeListener{ch: make(chan db.Notification, 2)}
	l.ch <- liveNotification(robinhood)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		done <- hub.Run(ctx, l)
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
	if err := <-done; err != nil {
		t.Fatalf("canceled run: %v", err)
	}
	// Closing the listener channel without cancellation is a fatal error, so
	// the API process cannot remain ready while the feed is dead.
	l2 := &fakeListener{ch: make(chan db.Notification)}
	close(l2.ch)
	if err := hub.Run(context.Background(), l2); err == nil || err.Error() != "notification listener closed" {
		t.Fatalf("closed listener: %v", err)
	}
	// The ring is capped.
	ctx2 := context.Background()
	var blocks []db.Block
	for i := uint64(0); i < ringSize+50; i++ {
		blocks = append(blocks, db.Block{ChainID: robinhood, Number: 2000 + i, TS: now, BaseFee: db.WeiFromUint64(1), PredictedBaseFee: db.NullWeiFromUint64(1)})
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

// TestHubHelloBroadcastsItsRefresh: a hello that finds the ring empty
// fills it through the one path that also delivers what the refresh found,
// so a client already connected to the network still receives every block,
// where a preparation that consumed the delta and dropped it would have
// swallowed them.
func TestHubHelloBroadcastsItsRefresh(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New()
	if err := store.UpsertNetwork(ctx, db.Network{ChainID: robinhood, Name: "robinhood", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	h := newWSHarness(t, store, time.Hour)
	// Client A connects while the table is empty, so its hello carries no
	// blocks and the hub's ring stays empty.
	first := h.dial(t, "robinhood")
	if typ, _ := readMsg(t, first); typ != "hello" {
		t.Fatalf("hello: %s", typ)
	}
	blocks := make([]db.Block, 0, 3)
	for n := uint64(200); n < 203; n++ {
		blocks = append(blocks, db.Block{ChainID: robinhood, Number: n, Hash: "0x" + strconv.FormatUint(n, 10),
			TS: now.Add(time.Duration(n) * time.Second), BaseFee: db.WeiFromUint64(n), PredictedBaseFee: db.NullWeiFromUint64(n)})
	}
	if err := store.UpsertBlocks(ctx, blocks); err != nil {
		t.Fatal(err)
	}
	// Client B's hello fills the ring. The blocks it found are broadcast,
	// so A sees them even though no notification has arrived yet.
	second := h.dial(t, "robinhood")
	if typ, _ := readMsg(t, second); typ != "hello" {
		t.Fatalf("second hello: %s", typ)
	}
	typ, data := readMsg(t, first)
	var got []model.BlockPoint
	if typ != "blocks" || json.Unmarshal(data, &got) != nil || len(got) != 3 || got[2].Number != 202 {
		t.Fatalf("the refresh a hello made must be broadcast: %s %s", typ, data)
	}
}

// TestHubRefreshPagesToTheTip: one tick can advance the chain by more than
// a single page, so the refresh keeps reading until the table's tip
// instead of leaving the ring behind until another notification arrives.
func TestHubRefreshPagesToTheTip(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New()
	if err := store.UpsertNetwork(ctx, db.Network{ChainID: robinhood, Name: "robinhood", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	h := newWSHarness(t, store, time.Hour)
	mk := func(n uint64) db.Block {
		return db.Block{ChainID: robinhood, Number: n, Hash: "0x" + strconv.FormatUint(n, 10),
			TS: now.Add(time.Duration(n) * time.Second), BaseFee: db.WeiFromUint64(n), PredictedBaseFee: db.NullWeiFromUint64(n)}
	}
	if err := store.UpsertBlocks(ctx, []db.Block{mk(1)}); err != nil {
		t.Fatal(err)
	}
	conn := h.dial(t, "robinhood")
	if typ, _ := readMsg(t, conn); typ != "hello" {
		t.Fatalf("hello: %s", typ)
	}
	// More than one page of blocks lands between two notifications.
	rows := make([]db.Block, 0, ringSize+50)
	for n := uint64(2); n <= ringSize+51; n++ {
		rows = append(rows, mk(n))
	}
	if err := store.UpsertBlocks(ctx, rows); err != nil {
		t.Fatal(err)
	}
	h.hub.Handle(ctx, liveNotification(robinhood))
	h.hub.mu.Lock()
	last := h.hub.networks[robinhood].last
	h.hub.mu.Unlock()
	if last != ringSize+51 {
		t.Fatalf("the ring must reach the table's tip in one refresh: last %d", last)
	}
}

// TestHubReorgRewindsOwnerCursor: a reorg lowers the owner-action cursor
// to the ancestor and forgets the actions above it, so a replacement
// action on the canonical chain is delivered even when its notification
// was lost during a LISTEN reconnect.
func TestHubReorgRewindsOwnerCursor(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New()
	if err := store.UpsertNetwork(ctx, db.Network{ChainID: robinhood, Name: "robinhood", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	mk := func(n uint64, tag string) db.Block {
		return db.Block{ChainID: robinhood, Number: n, Hash: "0x" + tag + strconv.FormatUint(n, 10),
			TS: now.Add(time.Duration(n) * time.Second), BaseFee: db.WeiFromUint64(n), PredictedBaseFee: db.NullWeiFromUint64(n)}
	}
	var blocks []db.Block
	for n := uint64(50); n <= 55; n++ {
		blocks = append(blocks, mk(n, "a"))
	}
	if err := store.UpsertBlocks(ctx, blocks); err != nil {
		t.Fatal(err)
	}
	h := newWSHarness(t, store, time.Hour)
	conn := h.dial(t, "robinhood")
	if typ, _ := readMsg(t, conn); typ != "hello" {
		t.Fatalf("hello: %s", typ)
	}
	// An owner action at block 54 is delivered, which advances the cursor.
	h.hub.Handle(ctx, ownerNotification(robinhood, 54, 0, "0xold", "setMinimumL2BaseFee"))
	if typ, _ := readMsg(t, conn); typ != "owner_action" {
		t.Fatalf("owner action: %s", typ)
	}
	h.hub.mu.Lock()
	since := h.hub.networks[robinhood].ownerSince
	h.hub.mu.Unlock()
	if since != 54 {
		t.Fatalf("cursor after delivery: %d", since)
	}
	// A reorg replaces 53 upwards, and the replacement action lands at 53
	// with a different transaction; its notification is lost.
	if _, err := store.DeleteBlocksAfter(ctx, robinhood, 52); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBlocks(ctx, []db.Block{mk(53, "b"), mk(54, "b"), mk(55, "b")}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertOwnerActions(ctx, []db.OwnerAction{{ChainID: robinhood, BlockNumber: 53, TxHash: "0xnew", LogIndex: 0,
		TS: now, Method: "setMinimumL2BaseFee", Args: db.JSONB(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	h.hub.Handle(ctx, liveNotification(robinhood))
	if typ, _ := readMsg(t, conn); typ != "reorg" {
		t.Fatalf("reorg: %s", typ)
	}
	if typ, _ := readMsg(t, conn); typ != "tick" {
		t.Fatalf("tick after reorg: %s", typ)
	}
	h.hub.mu.Lock()
	since = h.hub.networks[robinhood].ownerSince
	_, stillSeen := h.hub.networks[robinhood].seen[actionKey{block: 54, txHash: "0xold"}]
	h.hub.mu.Unlock()
	if since != 52 || stillSeen {
		t.Fatalf("the owner cursor must follow the chain down: since %d seen %v", since, stillSeen)
	}
	// Reconciliation after the LISTEN reconnect now finds the replacement.
	h.hub.Handle(ctx, db.Notification{Reconnected: true})
	typ, data := readMsg(t, conn)
	var action model.OwnerAction
	if typ != "owner_action" || json.Unmarshal(data, &action) != nil || action.TxHash != "0xnew" || action.Block != 53 {
		t.Fatalf("the replacement action must be delivered: %s %s", typ, data)
	}
}

// TestWebSocketHelloReAgesEthUsd: the hub keeps the last tick it published
// and serves it to the next client's hello. That snapshot's ETH/USD quote
// must be re-aged against the serving clock, so a quote that expired since
// the tick was published, or one stamped materially later than the serving
// clock, reaches the client as null rather than as live.
func TestWebSocketHelloReAgesEthUsd(t *testing.T) {
	helloQuote := func(t *testing.T, at time.Time) *model.EthUsd {
		t.Helper()
		store := seed(t)
		h := newWSHarness(t, store, time.Hour)
		snap := model.LiveSnapshot{
			ChainID: robinhood, SampledAt: now.Format(time.RFC3339), BaseFee: "42", Constraints: []model.Constraint{},
			EthUsd: &model.EthUsd{Price: "4523.40", At: at.Format(time.RFC3339), Source: "coinbase"},
		}
		payload, err := json.Marshal(snap)
		if err != nil {
			t.Fatal(err)
		}
		h.hub.Handle(context.Background(), db.Notification{Channel: db.ChannelLive, Payload: string(payload)})
		typ, data := readMsg(t, h.dial(t, "robinhood"))
		if typ != "hello" {
			t.Fatalf("first message %s", typ)
		}
		var hello struct {
			Snapshot model.LiveSnapshot `json:"snapshot"`
		}
		if err := json.Unmarshal(data, &hello); err != nil {
			t.Fatal(err)
		}
		return hello.Snapshot.EthUsd
	}
	if q := helloQuote(t, now.Add(-time.Minute)); q == nil || q.Price != "4523.40" {
		t.Fatalf("a fresh quote is served: %+v", q)
	}
	if q := helloQuote(t, now.Add(-2*config.DefaultEthUsdMaxAge)); q != nil {
		t.Fatalf("a quote that aged out must be null: %+v", q)
	}
	if q := helloQuote(t, now.Add(ethUsdFutureSkew+time.Minute)); q != nil {
		t.Fatalf("a quote from the serving clock's future must be null: %+v", q)
	}
}

// TestWebSocketSlowClientDropped: a peer that never reads fills its
// outbound queue. The hub must stop queueing for it, close the socket and
// give up its connection slot rather than keep it registered while the
// write loop drains stale messages into a socket nobody reads.
func TestWebSocketSlowClientDropped(t *testing.T) {
	store := seed(t)
	hub := NewHub(store, logger.Nop(), WithPingInterval(time.Hour), WithOrigins([]string{"http://localhost:3000"}), withClientQueue(2))
	cfg := config.ServerConfig{CORSOrigins: []string{"http://localhost:3000"}, RateLimitPerSecond: 1000, RateLimitBurst: 1000}
	srv := New(store, cfg, hub, logger.Nop(), WithClock(func() time.Time { return now }))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/ws?network=robinhood"
	conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{"http://localhost:3000"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	// The peer never reads a single frame from here on.
	defer func() { _ = conn.CloseNow() }()
	deadline := time.Now().Add(5 * time.Second)
	for hub.ClientCount(robinhood) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	// Ticks big enough to fill the peer's socket buffers, so the write
	// loop blocks and the queue overflows.
	filler := strings.Repeat("x", 64<<10)
	payload := `{"chainId":` + strconv.FormatUint(robinhood, 10) + `,"sampledAt":"` + now.Format(time.RFC3339) + `","filler":"` + filler + `"}`
	for range 400 {
		if hub.Connections() == 0 {
			break
		}
		hub.Handle(context.Background(), db.Notification{Channel: db.ChannelLive, Payload: payload})
	}
	deadline = time.Now().Add(3 * time.Second)
	for hub.Connections() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if hub.Connections() != 0 || hub.ClientCount(robinhood) != 0 {
		t.Fatalf("a peer that never reads must be dropped: connections %d clients %d", hub.Connections(), hub.ClientCount(robinhood))
	}
}

// The socket follows the same rule as the REST routes: a disabled network is not one clients may
// follow, whether they name it in the handshake or switch to it later.
func TestWebSocketRejectsDisabledNetwork(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New()
	for _, n := range []db.Network{
		{ChainID: robinhood, Name: "robinhood", Enabled: true},
		{ChainID: testnet, Name: "robinhood-testnet", Enabled: false},
	} {
		if err := store.UpsertNetwork(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	h := newWSHarness(t, store, time.Hour)
	url := "ws" + strings.TrimPrefix(h.ts.URL, "http") + "/api/v1/ws?network=robinhood-testnet"
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(dialCtx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{"http://localhost:3000"}}})
	if err == nil {
		_ = conn.CloseNow()
		t.Fatal("handshake accepted a disabled network")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("handshake status: %+v", resp)
	}
	_ = resp.Body.Close()

	conn2 := h.dial(t, "robinhood")
	if typ, _ := readMsg(t, conn2); typ != "hello" {
		t.Fatalf("hello: %s", typ)
	}
	send(t, conn2, map[string]string{"type": "subscribe", "network": "robinhood-testnet"})
	if typ, data := readMsg(t, conn2); typ != "error" {
		t.Fatalf("subscribe to a disabled network: %s %s", typ, data)
	}
}
