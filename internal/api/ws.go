package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/logger"
	"github.com/tirante-dev/gascurve/internal/model"
)

const (
	ringSize        = 1000
	helloBlocks     = 120
	clientQueue     = 64
	defaultPingGap  = 30 * time.Second
	missedPingLimit = 2
	wsReadLimit     = 4 << 10
)

// Hub fans NOTIFY payloads out to WebSocket clients. It keeps, per network,
// the latest snapshot and a ring of recent blocks read from the blocks
// table, so clients get `blocks` messages without querying per client.
type Hub struct {
	store        db.Store
	log          *logger.Logger
	pingInterval time.Duration
	origins      []string
	live         func(context.Context, uint64) (*model.LiveSnapshot, error)
	network      func(context.Context, db.Network) (model.Network, error)

	mu       sync.Mutex
	networks map[uint64]*netState
}

type netState struct {
	snapshot json.RawMessage
	blocks   []model.BlockPoint
	last     uint64
	clients  map[*client]struct{}
}

// HubOption customizes a Hub.
type HubOption func(*Hub)

// WithPingInterval sets the ping cadence (tests use a short one).
func WithPingInterval(d time.Duration) HubOption { return func(h *Hub) { h.pingInterval = d } }

// WithOrigins sets the allowed WebSocket origin patterns.
func WithOrigins(origins []string) HubOption {
	return func(h *Hub) { h.origins = originPatterns(origins) }
}

// NewHub creates a hub.
func NewHub(store db.Store, log *logger.Logger, opts ...HubOption) *Hub {
	if log == nil {
		log = logger.Nop()
	}
	h := &Hub{store: store, log: log, pingInterval: defaultPingGap, networks: map[uint64]*netState{}}
	for _, o := range opts {
		o(h)
	}
	return h
}

// Run consumes notifications until ctx ends or the listener closes.
func (h *Hub) Run(ctx context.Context, l db.Listener) {
	for {
		select {
		case <-ctx.Done():
			return
		case n, ok := <-l.Notifications():
			if !ok {
				return
			}
			h.Handle(ctx, n)
		}
	}
}

// Handle dispatches one notification.
func (h *Hub) Handle(ctx context.Context, n db.Notification) {
	switch n.Channel {
	case db.ChannelLive:
		h.handleLive(ctx, n.Payload)
	case db.ChannelOwnerAction:
		h.handleOwnerAction(n.Payload)
	}
}

func (h *Hub) state(chainID uint64) *netState {
	st, ok := h.networks[chainID]
	if !ok {
		st = &netState{clients: map[*client]struct{}{}}
		h.networks[chainID] = st
	}
	return st
}

func (h *Hub) handleLive(ctx context.Context, payload string) {
	var head struct {
		ChainID uint64 `json:"chainId"`
	}
	if err := json.Unmarshal([]byte(payload), &head); err != nil || head.ChainID == 0 {
		h.log.Warn("bad live notification", "payload", truncate(payload))
		return
	}
	newBlocks, err := h.refreshBlocks(ctx, head.ChainID)
	if err != nil {
		h.log.Warn("refresh blocks", "err", err.Error())
	}
	tick, _ := json.Marshal(wsMessage{Type: msgTick, Data: json.RawMessage(payload)})
	var blocksMsg []byte
	if len(newBlocks) > 0 {
		blocksMsg, _ = json.Marshal(wsMessage{Type: msgBlocks, Data: mustJSON(newBlocks)})
	}
	h.mu.Lock()
	st := h.state(head.ChainID)
	st.snapshot = json.RawMessage(payload)
	for c := range st.clients {
		c.enqueue(tick)
		if blocksMsg != nil {
			c.enqueue(blocksMsg)
		}
	}
	h.mu.Unlock()
}

// refreshBlocks pulls blocks newer than the ring head and returns them,
// oldest first.
func (h *Hub) refreshBlocks(ctx context.Context, chainID uint64) ([]model.BlockPoint, error) {
	h.mu.Lock()
	st := h.state(chainID)
	last := st.last
	empty := len(st.blocks) == 0
	h.mu.Unlock()

	var rows []db.Block
	var err error
	if empty {
		rows, err = h.store.RecentBlocks(ctx, chainID, helloBlocks)
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
	} else {
		rows, err = h.store.BlocksAfter(ctx, chainID, last, ringSize)
	}
	if err != nil {
		return nil, err
	}
	points := make([]model.BlockPoint, 0, len(rows))
	for _, b := range rows {
		points = append(points, blockPoint(b))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	st = h.state(chainID)
	st.blocks = append(st.blocks, points...)
	if len(st.blocks) > ringSize {
		st.blocks = st.blocks[len(st.blocks)-ringSize:]
	}
	if len(st.blocks) > 0 {
		st.last = st.blocks[len(st.blocks)-1].Number
	}
	return points, nil
}

func (h *Hub) handleOwnerAction(payload string) {
	var n model.OwnerActionNotification
	if err := json.Unmarshal([]byte(payload), &n); err != nil || n.ChainID == 0 {
		h.log.Warn("bad owner action notification", "payload", truncate(payload))
		return
	}
	msg, _ := json.Marshal(wsMessage{Type: msgOwnerAction, Data: mustJSON(n.Action)})
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.state(n.ChainID).clients {
		c.enqueue(msg)
	}
}

// hello builds the hello payload for a network from hub state, falling
// back to the database when the hub has not seen a tick yet.
func (h *Hub) hello(ctx context.Context, n db.Network) ([]byte, error) {
	net, err := h.network(ctx, n)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	st := h.state(n.ChainID)
	snapshot := st.snapshot
	blocks := append([]model.BlockPoint(nil), st.blocks...)
	h.mu.Unlock()
	if snapshot == nil {
		snap, err := h.live(ctx, n.ChainID)
		if err != nil && !errors.Is(err, errNoData) {
			return nil, err
		}
		if snap != nil {
			snapshot = mustJSON(snap)
		}
	}
	if len(blocks) == 0 {
		if _, err := h.refreshBlocks(ctx, n.ChainID); err != nil {
			return nil, err
		}
		h.mu.Lock()
		blocks = append([]model.BlockPoint(nil), h.state(n.ChainID).blocks...)
		h.mu.Unlock()
	}
	if len(blocks) > helloBlocks {
		blocks = blocks[len(blocks)-helloBlocks:]
	}
	if blocks == nil {
		blocks = []model.BlockPoint{}
	}
	if snapshot == nil {
		snapshot = json.RawMessage("null")
	}
	data := map[string]any{networkParam: net, "snapshot": snapshot, "recentBlocks": blocks}
	return json.Marshal(wsMessage{Type: msgHello, Data: mustJSON(data)})
}

func (h *Hub) subscribe(c *client, chainID uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c.chainID != 0 {
		delete(h.state(c.chainID).clients, c)
	}
	c.chainID = chainID
	h.state(chainID).clients[c] = struct{}{}
}

func (h *Hub) unsubscribe(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c.chainID != 0 {
		delete(h.state(c.chainID).clients, c)
	}
}

// ClientCount returns the number of clients subscribed to a network.
func (h *Hub) ClientCount(chainID uint64) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.state(chainID).clients)
}

// Message types and field names of the WebSocket protocol.
const (
	msgHello       = "hello"
	msgTick        = "tick"
	msgBlocks      = "blocks"
	msgOwnerAction = "owner_action"
	msgPing        = "ping"
	msgPong        = "pong"
	msgSubscribe   = "subscribe"
	msgError       = "error"
	networkParam   = "network"
)

type wsMessage struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

type clientMessage struct {
	Type    string `json:"type"`
	Network string `json:"network"`
}

// wsError is sent when a client request cannot be honored.
type wsError struct {
	Type  string            `json:"type"`
	Error model.ErrorDetail `json:"error"`
}

type client struct {
	hub     *Hub
	conn    *websocket.Conn
	send    chan []byte
	chainID uint64
	once    sync.Once
	closed  chan struct{}
}

func (c *client) enqueue(msg []byte) {
	select {
	case c.send <- msg:
	default:
		// Slow consumer: drop the connection rather than block the hub.
		c.close()
	}
}

func (c *client) close() {
	c.once.Do(func() { close(c.closed) })
}

// ServeWS upgrades the connection and runs the client until it goes away.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get(networkParam)
	if ref == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "network query parameter is required")
		return
	}
	n, err := h.store.NetworkByRef(r.Context(), ref)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if n == nil {
		writeError(w, http.StatusNotFound, "not_found", "unknown network "+ref)
		return
	}
	opts := &websocket.AcceptOptions{OriginPatterns: h.origins}
	if len(h.origins) == 0 {
		opts.InsecureSkipVerify = true
	}
	conn, err := websocket.Accept(w, r, opts)
	if err != nil {
		h.log.Warn("websocket accept", "err", err.Error())
		return
	}
	conn.SetReadLimit(wsReadLimit)
	c := &client{hub: h, conn: conn, send: make(chan []byte, clientQueue), closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	hello, err := h.hello(ctx, *n)
	if err != nil {
		h.log.Warn("websocket hello", "err", err.Error())
		_ = conn.Close(websocket.StatusInternalError, "hello failed")
		return
	}
	h.subscribe(c, n.ChainID)
	defer h.unsubscribe(c)
	c.enqueue(hello)

	pongs := make(chan struct{}, 1)
	go c.readLoop(ctx, pongs)
	c.writeLoop(ctx, pongs)
	_ = conn.Close(websocket.StatusNormalClosure, "bye")
}

// readLoop handles pong and subscribe messages.
func (c *client) readLoop(ctx context.Context, pongs chan<- struct{}) {
	defer c.close()
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			return
		}
		var msg clientMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case msgPong:
			select {
			case pongs <- struct{}{}:
			default:
			}
		case msgSubscribe:
			n, err := c.hub.store.NetworkByRef(ctx, msg.Network)
			if err != nil || n == nil {
				c.enqueue(mustJSON(wsError{Type: msgError, Error: model.ErrorDetail{Code: "not_found", Message: "unknown network " + msg.Network}}))
				continue
			}
			hello, err := c.hub.hello(ctx, *n)
			if err != nil {
				c.hub.log.Warn("websocket hello", "err", err.Error())
				continue
			}
			c.hub.subscribe(c, n.ChainID)
			c.enqueue(hello)
		}
	}
}

// writeLoop sends queued messages and pings, closing after two missed
// pongs.
func (c *client) writeLoop(ctx context.Context, pongs <-chan struct{}) {
	ticker := time.NewTicker(c.hub.pingInterval)
	defer ticker.Stop()
	missed := 0
	ping, _ := json.Marshal(wsMessage{Type: msgPing})
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			return
		case <-pongs:
			missed = 0
		case msg := <-c.send:
			if err := c.write(ctx, msg); err != nil {
				return
			}
		case <-ticker.C:
			if missed >= missedPingLimit {
				_ = c.conn.Close(websocket.StatusGoingAway, "missed pongs")
				return
			}
			missed++
			if err := c.write(ctx, ping); err != nil {
				return
			}
		}
	}
}

func (c *client) write(ctx context.Context, msg []byte) error {
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.conn.Write(wctx, websocket.MessageText, msg)
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

func truncate(s string) string {
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}
