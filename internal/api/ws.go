package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/time/rate"

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
	// defaultWSPerIP and defaultWSTotal cap concurrent sockets.
	defaultWSPerIP = 8
	defaultWSTotal = 2000
	// msgRate and msgBurst bound client messages per connection.
	msgRate  = 10
	msgBurst = 20
	// wsDBTimeout bounds the database work done for one hello.
	wsDBTimeout = 5 * time.Second
	// networkCacheTTL is how long a network lookup (found or not) is reused.
	networkCacheTTL = 5 * time.Second
	// seenActions is how many delivered owner actions are remembered per
	// network for de-duplication after a LISTEN reconnect.
	seenActions = 256
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
	now          func() time.Time
	maxPerIP     int
	maxTotal     int

	mu       sync.Mutex
	networks map[uint64]*netState
	perIP    map[string]int
	total    int
	cache    map[string]cachedNetwork
}

type cachedNetwork struct {
	n       *db.Network
	expires time.Time
}

type netState struct {
	snapshot json.RawMessage
	blocks   []model.BlockPoint
	last     uint64
	clients  map[*client]struct{}
	// refreshMu serializes block refreshes for the network.
	refreshMu sync.Mutex
	// ownerSince and seen track delivered owner actions so a reconnect can
	// reconcile from the table without repeating any.
	ownerInit  bool
	ownerSince uint64
	seen       map[actionKey]struct{}
	seenOrder  []actionKey
}

type actionKey struct {
	block    uint64
	txHash   string
	logIndex uint64
}

// HubOption customizes a Hub.
type HubOption func(*Hub)

// WithPingInterval sets the ping cadence (tests use a short one).
func WithPingInterval(d time.Duration) HubOption { return func(h *Hub) { h.pingInterval = d } }

// WithOrigins sets the allowed WebSocket origin patterns.
func WithOrigins(origins []string) HubOption {
	return func(h *Hub) { h.origins = originPatterns(origins) }
}

// WithConnectionLimits caps concurrent sockets per client address and in
// total.
func WithConnectionLimits(perIP, total int) HubOption {
	return func(h *Hub) { h.setLimits(perIP, total) }
}

// NewHub creates a hub.
func NewHub(store db.Store, log *logger.Logger, opts ...HubOption) *Hub {
	if log == nil {
		log = logger.Nop()
	}
	h := &Hub{
		store: store, log: log, pingInterval: defaultPingGap, networks: map[uint64]*netState{},
		perIP: map[string]int{}, cache: map[string]cachedNetwork{}, now: time.Now,
		maxPerIP: defaultWSPerIP, maxTotal: defaultWSTotal,
	}
	for _, o := range opts {
		o(h)
	}
	return h
}

func (h *Hub) setLimits(perIP, total int) {
	if perIP > 0 {
		h.maxPerIP = perIP
	}
	if total > 0 {
		h.maxTotal = total
	}
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
	switch {
	case n.Reconnected:
		h.reconcile(ctx)
	case n.Channel == db.ChannelLive:
		h.handleLive(ctx, n.Payload)
	case n.Channel == db.ChannelOwnerAction:
		h.handleOwnerAction(n.Payload)
	}
}

func (h *Hub) state(chainID uint64) *netState {
	st, ok := h.networks[chainID]
	if !ok {
		st = &netState{clients: map[*client]struct{}{}, seen: map[actionKey]struct{}{}}
		h.networks[chainID] = st
	}
	return st
}

// lookupNetwork resolves a reference through a short-lived cache so a
// client cannot turn subscribe messages into database queries. Unknown
// references are cached too.
func (h *Hub) lookupNetwork(ctx context.Context, ref string) (*db.Network, error) {
	now := h.now()
	h.mu.Lock()
	if c, ok := h.cache[ref]; ok && now.Before(c.expires) {
		h.mu.Unlock()
		return c.n, nil
	}
	h.mu.Unlock()
	n, err := h.store.NetworkByRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.cache[ref] = cachedNetwork{n: n, expires: now.Add(networkCacheTTL)}
	h.mu.Unlock()
	return n, nil
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
	h.mu.Lock()
	st := h.state(head.ChainID)
	st.snapshot = json.RawMessage(payload)
	for c := range st.clients {
		c.deliver(outbound{raw: tick})
		if len(newBlocks) > 0 {
			c.deliver(outbound{blocks: newBlocks})
		}
	}
	h.mu.Unlock()
}

// refreshBlocks pulls blocks newer than the ring head and returns them,
// oldest first. Refreshes are serialized per network and their results
// filtered against the ring head, so concurrent callers never append the
// same block twice or move the head backwards.
func (h *Hub) refreshBlocks(ctx context.Context, chainID uint64) ([]model.BlockPoint, error) {
	h.mu.Lock()
	st := h.state(chainID)
	h.mu.Unlock()
	st.refreshMu.Lock()
	defer st.refreshMu.Unlock()

	h.mu.Lock()
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
	h.mu.Lock()
	defer h.mu.Unlock()
	points := make([]model.BlockPoint, 0, len(rows))
	for _, b := range rows {
		if len(st.blocks) > 0 && b.Number <= st.last {
			continue
		}
		points = append(points, blockPoint(b))
	}
	st.blocks = append(st.blocks, points...)
	if len(st.blocks) > ringSize {
		st.blocks = st.blocks[len(st.blocks)-ringSize:]
	}
	if len(st.blocks) > 0 {
		st.last = max(st.last, st.blocks[len(st.blocks)-1].Number)
	}
	return points, nil
}

func (h *Hub) handleOwnerAction(payload string) {
	var n model.OwnerActionNotification
	if err := json.Unmarshal([]byte(payload), &n); err != nil || n.ChainID == 0 {
		h.log.Warn("bad owner action notification", "payload", truncate(payload))
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.state(n.ChainID)
	if !st.markSeen(actionKey{block: n.Action.Block, txHash: n.Action.TxHash, logIndex: n.LogIndex}) {
		return
	}
	msg, _ := json.Marshal(wsMessage{Type: msgOwnerAction, Data: mustJSON(n.Action)})
	for c := range st.clients {
		c.deliver(outbound{raw: msg})
	}
}

// markSeen records a delivered action and reports whether it was new.
func (st *netState) markSeen(k actionKey) bool {
	if _, ok := st.seen[k]; ok {
		return false
	}
	st.seen[k] = struct{}{}
	st.seenOrder = append(st.seenOrder, k)
	if len(st.seenOrder) > seenActions {
		delete(st.seen, st.seenOrder[0])
		st.seenOrder = st.seenOrder[1:]
	}
	st.ownerSince = max(st.ownerSince, k.block)
	return true
}

// initOwnerCursor starts a network's owner-action cursor at the newest
// stored action, so a later reconcile only looks at what came after.
func (h *Hub) initOwnerCursor(ctx context.Context, chainID uint64) {
	h.mu.Lock()
	st := h.state(chainID)
	done := st.ownerInit
	h.mu.Unlock()
	if done {
		return
	}
	latest, err := h.store.OwnerActions(ctx, chainID, time.Time{}, time.Time{}, 1)
	if err != nil {
		h.log.Warn("owner action cursor", "err", err.Error())
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if st.ownerInit {
		return
	}
	st.ownerInit = true
	for _, a := range latest {
		st.markSeen(actionKey{block: a.BlockNumber, txHash: a.TxHash, logIndex: uint64(a.LogIndex)})
	}
}

// reconcile runs after the LISTEN connection was re-established: every
// notification sent meanwhile was lost, so blocks are refreshed from the
// table for every network with clients and owner actions since the cursor
// are delivered once.
func (h *Hub) reconcile(ctx context.Context) {
	h.mu.Lock()
	ids := make([]uint64, 0, len(h.networks))
	for id, st := range h.networks {
		if len(st.clients) > 0 {
			ids = append(ids, id)
		}
	}
	h.mu.Unlock()
	for _, id := range ids {
		newBlocks, err := h.refreshBlocks(ctx, id)
		if err != nil {
			h.log.Warn("reconcile blocks", "err", err.Error())
		}
		h.mu.Lock()
		st := h.state(id)
		since := st.ownerSince
		if len(newBlocks) > 0 {
			for c := range st.clients {
				c.deliver(outbound{blocks: newBlocks})
			}
		}
		h.mu.Unlock()
		actions, err := h.store.OwnerActionsSince(ctx, id, since)
		if err != nil {
			h.log.Warn("reconcile owner actions", "err", err.Error())
			continue
		}
		h.mu.Lock()
		for _, a := range actions {
			if !st.markSeen(actionKey{block: a.BlockNumber, txHash: a.TxHash, logIndex: uint64(a.LogIndex)}) {
				continue
			}
			msg, _ := json.Marshal(wsMessage{Type: msgOwnerAction, Data: mustJSON(ownerActionModel(a))})
			for c := range st.clients {
				c.deliver(outbound{raw: msg})
			}
		}
		h.mu.Unlock()
	}
}

// prepared is everything a hello needs that comes from the database,
// gathered before the client is registered so registration itself cannot
// fail.
type prepared struct {
	net      model.Network
	fallback *model.LiveSnapshot
}

// prepare does the database work for a hello: the network model, the ring
// (filled from the table when the hub has not seen a tick yet), the owner
// cursor and a snapshot fallback. Short deadlines keep a subscribe storm
// from holding connections.
func (h *Hub) prepare(ctx context.Context, n db.Network) (*prepared, error) {
	ctx, cancel := context.WithTimeout(ctx, wsDBTimeout)
	defer cancel()
	net, err := h.network(ctx, n)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	st := h.state(n.ChainID)
	empty := len(st.blocks) == 0
	noSnap := st.snapshot == nil
	h.mu.Unlock()
	if empty {
		if _, err := h.refreshBlocks(ctx, n.ChainID); err != nil {
			return nil, err
		}
	}
	h.initOwnerCursor(ctx, n.ChainID)
	p := &prepared{net: net}
	if noSnap {
		snap, err := h.live(ctx, n.ChainID)
		if err != nil && !errors.Is(err, errNoData) {
			return nil, err
		}
		p.fallback = snap
	}
	return p, nil
}

// subscribe moves the client to a network and queues its hello, all under
// the hub lock: the hello is built from the ring exactly as it is at
// registration, and fan-out that arrives from then on is buffered behind
// it, so nothing between hello and the first tick can be missed or
// duplicated.
func (h *Hub) subscribe(c *client, chainID uint64, p *prepared) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c.chainID != 0 {
		delete(h.state(c.chainID).clients, c)
	}
	c.chainID = chainID
	st := h.state(chainID)
	st.clients[c] = struct{}{}
	snapshot := st.snapshot
	blocks := append([]model.BlockPoint(nil), st.blocks...)
	if len(blocks) > helloBlocks {
		blocks = blocks[len(blocks)-helloBlocks:]
	}
	if blocks == nil {
		blocks = []model.BlockPoint{}
	}
	if snapshot == nil && p.fallback != nil {
		snapshot = mustJSON(p.fallback)
	}
	if snapshot == nil {
		snapshot = json.RawMessage("null")
	}
	data := map[string]any{networkParam: p.net, "snapshot": snapshot, "recentBlocks": blocks}
	hello, _ := json.Marshal(wsMessage{Type: msgHello, Data: mustJSON(data)})
	c.enqueue(hello)
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

// admit reserves a connection slot for an address, or reports which cap
// refused it.
func (h *Hub) admit(ip string) (ok bool, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.total >= h.maxTotal {
		return false, "too many connections"
	}
	if h.perIP[ip] >= h.maxPerIP {
		return false, "too many connections from this address"
	}
	h.total++
	h.perIP[ip]++
	return true, ""
}

func (h *Hub) release(ip string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.total--
	if h.perIP[ip] <= 1 {
		delete(h.perIP, ip)
	} else {
		h.perIP[ip]--
	}
}

// Connections returns the open socket count.
func (h *Hub) Connections() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.total
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

func errorMessage(code, msg string) []byte {
	return mustJSON(wsError{Type: msgError, Error: model.ErrorDetail{Code: code, Message: msg}})
}

// outbound is one fan-out item: a raw message, or blocks that are encoded
// when delivered (so a hello's block range can filter them).
type outbound struct {
	raw    []byte
	blocks []model.BlockPoint
}

type client struct {
	hub     *Hub
	conn    *websocket.Conn
	send    chan []byte
	limiter *rate.Limiter
	chainID uint64
	once    sync.Once
	closed  chan struct{}
}

// deliver queues a fan-out item for the client.
func (c *client) deliver(o outbound) {
	if o.raw != nil {
		c.enqueue(o.raw)
		return
	}
	msg, _ := json.Marshal(wsMessage{Type: msgBlocks, Data: mustJSON(o.blocks)})
	c.enqueue(msg)
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
// Every refusal before the upgrade is the JSON error envelope.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get(networkParam)
	if ref == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "network query parameter is required")
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		writeError(w, http.StatusBadRequest, "bad_request", "websocket upgrade required")
		return
	}
	if !h.originAllowed(r) {
		writeError(w, http.StatusForbidden, "forbidden", "origin not allowed")
		return
	}
	n, err := h.lookupNetwork(r.Context(), ref)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if n == nil {
		writeError(w, http.StatusNotFound, "not_found", "unknown network "+ref)
		return
	}
	ip := clientIP(r.RemoteAddr)
	ok, reason := h.admit(ip)
	if !ok {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusTooManyRequests, "rate_limited", reason)
		return
	}
	defer h.release(ip)
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
	c := &client{hub: h, conn: conn, send: make(chan []byte, clientQueue), closed: make(chan struct{}), limiter: rate.NewLimiter(msgRate, msgBurst)}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	p, err := h.prepare(ctx, *n)
	if err != nil {
		h.log.Warn("websocket hello", "err", err.Error())
		_ = c.write(ctx, errorMessage("internal", "could not prepare the subscription"))
		_ = conn.Close(websocket.StatusInternalError, "hello failed")
		return
	}
	h.subscribe(c, n.ChainID, p)
	defer h.unsubscribe(c)

	pongs := make(chan struct{}, 1)
	go c.readLoop(ctx, pongs)
	c.writeLoop(ctx, pongs)
	_ = conn.Close(websocket.StatusNormalClosure, "bye")
}

// originAllowed checks the Origin header against the configured patterns
// before the upgrade, so a refusal is a JSON error rather than the
// library's plain text. Browsers always send Origin; non-browser clients
// without one are allowed, as the library does.
func (h *Hub) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || len(h.origins) == 0 {
		return true
	}
	host := origin
	if i := strings.Index(origin, "://"); i >= 0 {
		host = origin[i+3:]
	}
	host = strings.TrimSuffix(host, "/")
	for _, pattern := range h.origins {
		if pattern == "*" || strings.EqualFold(pattern, host) {
			return true
		}
	}
	return false
}

// readLoop handles pong and subscribe messages. Messages beyond the
// per-connection budget are answered with one error and the socket closes.
func (c *client) readLoop(ctx context.Context, pongs chan<- struct{}) {
	defer c.close()
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			return
		}
		if !c.limiter.Allow() {
			c.enqueue(errorMessage("rate_limited", "too many messages"))
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
			c.handleSubscribe(ctx, msg.Network)
		}
	}
}

// handleSubscribe switches the client to another network. Subscribing to
// the network it already follows is a no-op.
func (c *client) handleSubscribe(ctx context.Context, ref string) {
	n, err := c.hub.lookupNetwork(ctx, ref)
	if err != nil {
		c.enqueue(errorMessage("internal", "could not resolve network "+ref))
		return
	}
	if n == nil {
		c.enqueue(errorMessage("not_found", "unknown network "+ref))
		return
	}
	if n.ChainID == c.chainID {
		return
	}
	p, err := c.hub.prepare(ctx, *n)
	if err != nil {
		c.hub.log.Warn("websocket hello", "err", err.Error())
		c.enqueue(errorMessage("internal", "could not switch to network "+ref))
		return
	}
	c.hub.subscribe(c, n.ChainID, p)
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
			// Flush what was queued (an error message, say) before closing.
			for {
				select {
				case msg := <-c.send:
					if err := c.write(ctx, msg); err != nil {
						return
					}
				default:
					return
				}
			}
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
