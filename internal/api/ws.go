package api

import (
	"context"
	"encoding/base64"
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
// table (with their hashes, so a reorg that replaces blocks at or below
// the ring's tip is noticed), so clients get `blocks` messages without
// querying per client.
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
	// snapshotAt is the sampledAt of the cached snapshot, for reconciling.
	snapshotAt time.Time
	blocks     []ringBlock
	last       uint64
	clients    map[*client]struct{}
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

// ringBlock is a ring entry: the point clients get and the stored hash the
// hub compares against the table to notice a reorg.
type ringBlock struct {
	point model.BlockPoint
	hash  string
}

// delta is what one refresh found: a reorg (the canonical replacements
// above the ancestor, which clients apply before anything else) or new
// blocks appended to the ring.
type delta struct {
	reorg  *model.Reorg
	blocks []model.BlockPoint
}

func points(ring []ringBlock) []model.BlockPoint {
	out := make([]model.BlockPoint, len(ring))
	for i, b := range ring {
		out[i] = b.point
	}
	return out
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
		ChainID   uint64 `json:"chainId"`
		SampledAt string `json:"sampledAt"`
	}
	if err := json.Unmarshal([]byte(payload), &head); err != nil || head.ChainID == 0 {
		h.log.Warn("bad live notification", "payload", truncate(payload))
		return
	}
	d, err := h.refreshBlocks(ctx, head.ChainID)
	if err != nil {
		h.log.Warn("refresh blocks", "err", err.Error())
	}
	at, _ := time.Parse(time.RFC3339, head.SampledAt)
	h.mu.Lock()
	st := h.state(head.ChainID)
	st.snapshot, st.snapshotAt = json.RawMessage(payload), at
	h.fanOut(st, d, json.RawMessage(payload))
	h.mu.Unlock()
}

// fanOut delivers a refresh's outcome and, when given, a tick to every
// client of a network, in the order clients must apply them: the reorg
// first, then the tick, then the new blocks. The caller holds h.mu.
func (h *Hub) fanOut(st *netState, d delta, tick json.RawMessage) {
	var reorg, tickMsg []byte
	if d.reorg != nil {
		reorg, _ = json.Marshal(wsMessage{Type: msgReorg, Data: mustJSON(d.reorg)})
	}
	if tick != nil {
		tickMsg, _ = json.Marshal(wsMessage{Type: msgTick, Data: tick})
	}
	for c := range st.clients {
		if reorg != nil {
			// The client's hello may have carried orphaned blocks: the
			// watermark drops to the ancestor so their replacements pass.
			c.watermark = min(c.watermark, d.reorg.Ancestor)
			c.deliver(outbound{raw: reorg})
		}
		if tickMsg != nil {
			c.deliver(outbound{raw: tickMsg})
		}
		if len(d.blocks) > 0 {
			c.deliver(outbound{blocks: d.blocks})
		}
	}
}

// refreshBlocks reconciles the ring with the blocks table and returns what
// changed: the blocks newer than the ring's tip, oldest first, or, when
// the table no longer holds the tip the ring knows (its hash changed or
// its row is gone), a reorg: the ring is truncated to the highest entry
// the table still agrees with and the canonical blocks above it are
// appended and reported as replacements. Refreshes are serialized per
// network and their results filtered against the ring, so concurrent
// callers never append the same block twice or move the tip backwards.
func (h *Hub) refreshBlocks(ctx context.Context, chainID uint64) (delta, error) {
	h.mu.Lock()
	st := h.state(chainID)
	h.mu.Unlock()
	st.refreshMu.Lock()
	defer st.refreshMu.Unlock()

	h.mu.Lock()
	last, empty := st.last, len(st.blocks) == 0
	var tipHash string
	if !empty {
		tipHash = st.blocks[len(st.blocks)-1].hash
	}
	h.mu.Unlock()

	var rows []db.Block
	var err error
	if empty {
		rows, err = h.store.RecentBlocks(ctx, chainID, helloBlocks)
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
	} else {
		// From the tip itself, so its row proves the ring is still on-chain.
		rows, err = h.store.BlocksAfter(ctx, chainID, last-1, ringSize)
	}
	if err != nil {
		return delta{}, err
	}
	if !empty && (len(rows) == 0 || rows[0].Number != last || rows[0].Hash != tipHash) {
		return h.reorgRing(ctx, st, chainID)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	added := make([]model.BlockPoint, 0, len(rows))
	for _, b := range rows {
		if len(st.blocks) > 0 && b.Number <= st.last {
			continue
		}
		added = append(added, blockPoint(b))
		st.blocks = append(st.blocks, ringBlock{point: blockPoint(b), hash: b.Hash})
	}
	st.trim()
	return delta{blocks: added}, nil
}

// reorgRing rebuilds the ring after the table diverged from it: the
// ancestor is the highest ring entry whose row still exists with the same
// hash (rows written before hashes were stored count as matching only
// against an equally hashless ring entry), everything above it is dropped
// and replaced by the canonical rows.
func (h *Hub) reorgRing(ctx context.Context, st *netState, chainID uint64) (delta, error) {
	rows, err := h.store.RecentBlocks(ctx, chainID, ringSize)
	if err != nil {
		return delta{}, err
	}
	byNumber := make(map[uint64]db.Block, len(rows))
	for _, r := range rows {
		byNumber[r.Number] = r
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	keep := 0
	for i := len(st.blocks) - 1; i >= 0; i-- {
		if r, ok := byNumber[st.blocks[i].point.Number]; ok && r.Hash == st.blocks[i].hash {
			keep = i + 1
			break
		}
	}
	var ancestor uint64
	if keep > 0 {
		ancestor = st.blocks[keep-1].point.Number
	}
	st.blocks = st.blocks[:keep]
	replaced := []model.BlockPoint{}
	for i := len(rows) - 1; i >= 0; i-- { // ascending
		r := rows[i]
		if r.Number <= ancestor {
			continue
		}
		replaced = append(replaced, blockPoint(r))
		st.blocks = append(st.blocks, ringBlock{point: blockPoint(r), hash: r.Hash})
	}
	st.trim()
	h.log.Warn("blocks replaced below the ring tip, resending the canonical chain", "chainId", chainID, "ancestor", ancestor, "blocks", len(replaced))
	return delta{reorg: &model.Reorg{ChainID: chainID, Ancestor: ancestor, Blocks: replaced}}, nil
}

// trim caps the ring and records its tip. The caller holds h.mu.
func (st *netState) trim() {
	if len(st.blocks) > ringSize {
		st.blocks = st.blocks[len(st.blocks)-ringSize:]
	}
	st.last = 0
	if len(st.blocks) > 0 {
		st.last = st.blocks[len(st.blocks)-1].point.Number
	}
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
// notification sent meanwhile was lost, so for every network with clients
// the blocks are refreshed from the table, the live snapshot is rebuilt
// from the latest sample and sent as a tick when it is newer than the
// cached one, and owner actions since the cursor are delivered once.
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
		d, err := h.refreshBlocks(ctx, id)
		if err != nil {
			h.log.Warn("reconcile blocks", "err", err.Error())
		}
		tick := h.newerSnapshot(ctx, id)
		h.mu.Lock()
		st := h.state(id)
		since := st.ownerSince
		if tick != nil {
			st.snapshot = tick.raw
			st.snapshotAt = tick.at
		}
		h.fanOut(st, d, tick.payload())
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

// snapshotUpdate is a live snapshot rebuilt from the table.
type snapshotUpdate struct {
	raw json.RawMessage
	at  time.Time
}

// payload is the tick to send, nil for no update.
func (u *snapshotUpdate) payload() json.RawMessage {
	if u == nil {
		return nil
	}
	return u.raw
}

// newerSnapshot rebuilds a network's live snapshot from the latest sample
// and returns it when it is newer than the cached one (or nothing is
// cached), so a tick lost during a LISTEN outage is made up.
func (h *Hub) newerSnapshot(ctx context.Context, chainID uint64) *snapshotUpdate {
	snap, err := h.live(ctx, chainID)
	if err != nil {
		if !errors.Is(err, errNoData) {
			h.log.Warn("reconcile snapshot", "err", err.Error())
		}
		return nil
	}
	at, err := time.Parse(time.RFC3339, snap.SampledAt)
	if err != nil {
		return nil
	}
	h.mu.Lock()
	st := h.state(chainID)
	stale := st.snapshot == nil || at.After(st.snapshotAt)
	h.mu.Unlock()
	if !stale {
		return nil
	}
	return &snapshotUpdate{raw: mustJSON(snap), at: at}
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
// it. The client's watermark is the hello's last block: a blocks message
// never repeats a block the hello carried, whatever refresh produced it.
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
	blocks := points(st.blocks)
	if len(blocks) > helloBlocks {
		blocks = blocks[len(blocks)-helloBlocks:]
	}
	c.watermark = 0
	if len(blocks) > 0 {
		c.watermark = blocks[len(blocks)-1].Number
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
	msgReorg       = "reorg"
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
	// watermark is the last block the client's hello carried (guarded by
	// the hub lock): blocks at or below it are never sent again.
	watermark uint64
	once      sync.Once
	closed    chan struct{}
}

// deliver queues a fan-out item for the client, dropping blocks its hello
// already carried. The caller holds the hub lock.
func (c *client) deliver(o outbound) {
	if o.raw != nil {
		c.enqueue(o.raw)
		return
	}
	blocks := o.blocks
	for len(blocks) > 0 && blocks[0].Number <= c.watermark {
		blocks = blocks[1:]
	}
	if len(blocks) == 0 {
		return
	}
	c.watermark = blocks[len(blocks)-1].Number
	msg, _ := json.Marshal(wsMessage{Type: msgBlocks, Data: mustJSON(blocks)})
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
// Every refusal before the upgrade, including a malformed handshake, is
// the JSON error envelope.
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
	if msg := handshakeError(r); msg != "" {
		writeError(w, http.StatusBadRequest, "bad_request", msg)
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

// handshakeError validates the WebSocket handshake headers the upgrade
// library would otherwise refuse with a plaintext response: the Connection
// token, the protocol version and the client key. It returns the reason,
// or "" when the handshake is well formed.
func handshakeError(r *http.Request) string {
	if !r.ProtoAtLeast(1, 1) {
		return "websocket handshake requires HTTP/1.1"
	}
	if !headerHasToken(r.Header, "Connection", "upgrade") {
		return "Connection header must include Upgrade"
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		return "unsupported Sec-WebSocket-Version, only 13 is supported"
	}
	keys := r.Header.Values("Sec-WebSocket-Key")
	if len(keys) != 1 {
		return "exactly one Sec-WebSocket-Key header is required"
	}
	if raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keys[0])); err != nil || len(raw) != 16 {
		return "Sec-WebSocket-Key must be a base64 encoded 16 byte value"
	}
	return ""
}

// headerHasToken reports whether a comma-separated header carries a token,
// case-insensitively.
func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
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
