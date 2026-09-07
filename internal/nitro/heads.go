package nitro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/tirante-dev/gascurve/internal/logger"
	"github.com/tirante-dev/gascurve/internal/version"
)

const (
	headsMinBackoff      = time.Second
	headsMaxBackoff      = 30 * time.Second
	headsPingInterval    = 15 * time.Second
	headsHandshakeLimit  = 15 * time.Second
	headsReadLimit       = 1 << 20
	headsSubscribeID     = 1
	methodSubscribe      = "eth_subscribe"
	methodSubscription   = "eth_subscription"
	subscriptionNewHeads = "newHeads"
)

// Head is a newHeads notification: the header fields the follower needs to
// trigger a tick pinned to that block.
type Head struct {
	Number    uint64
	Hash      string
	Timestamp uint64
}

// HeadSubscriber keeps an eth_subscribe("newHeads") subscription open over WebSocket. It reconnects
// with exponential back-off and reports whether it is subscribed so the follower can fall back to
// polling. The URL is never logged: it can carry a key.
type HeadSubscriber struct {
	url func(context.Context) (string, error)
	// lease, when set, resolves the endpoint and takes the report of what happened with its socket, so
	// a WebSocket that does not work moves the subscription to another endpoint.
	lease        func(context.Context) (*WSLease, error)
	userAgent    string
	log          *logger.Logger
	httpClient   *http.Client
	minBackoff   time.Duration
	maxBackoff   time.Duration
	pingInterval time.Duration
	sleep        func(context.Context, time.Duration) error

	connected atomic.Bool
}

// HeadOption customizes a HeadSubscriber.
type HeadOption func(*HeadSubscriber)

func WithHeadLogger(l *logger.Logger) HeadOption { return func(s *HeadSubscriber) { s.log = l } }

// WithHeadURL replaces the fixed URL with a resolver called before every connection attempt, so a
// subscriber rebinds when the endpoint it followed is disabled. An error keeps it retrying.
func WithHeadURL(fn func(context.Context) (string, error)) HeadOption {
	return func(s *HeadSubscriber) { s.url = fn }
}

// WithHeadLease replaces the URL resolver with one that leases an endpoint: the subscriber then reports
// every dial, subscribe and disconnect failure back, so an endpoint whose WebSocket does not work is
// cooled down. This is the form a Pool provides.
func WithHeadLease(fn func(context.Context) (*WSLease, error)) HeadOption {
	return func(s *HeadSubscriber) { s.lease = fn }
}

// WithHeadHTTPClient sets the HTTP client used for the WebSocket handshake; its Timeout bounds it.
func WithHeadHTTPClient(h *http.Client) HeadOption {
	return func(s *HeadSubscriber) { s.httpClient = h }
}

// WithHeadBackoff sets the reconnect back-off range.
func WithHeadBackoff(minWait, maxWait time.Duration) HeadOption {
	return func(s *HeadSubscriber) { s.minBackoff, s.maxBackoff = minWait, maxWait }
}

// WithHeadPingInterval sets how often the socket is pinged; a missed pong closes it, so a dead
// connection is noticed even on an idle chain.
func WithHeadPingInterval(d time.Duration) HeadOption {
	return func(s *HeadSubscriber) { s.pingInterval = d }
}

// withHeadSleep replaces the back-off sleeper, for tests.
func withHeadSleep(sleep func(context.Context, time.Duration) error) HeadOption {
	return func(s *HeadSubscriber) { s.sleep = sleep }
}

// NewHeadSubscriber creates a subscriber for a ws:// or wss:// endpoint. WithHeadURL replaces the
// fixed endpoint with a resolver.
func NewHeadSubscriber(url string, opts ...HeadOption) *HeadSubscriber {
	s := &HeadSubscriber{
		url:          func(context.Context) (string, error) { return url, nil },
		userAgent:    version.UserAgent(),
		log:          logger.Nop(),
		httpClient:   &http.Client{Timeout: headsHandshakeLimit},
		minBackoff:   headsMinBackoff,
		maxBackoff:   headsMaxBackoff,
		pingInterval: headsPingInterval,
		sleep:        sleepContext,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Connected reports whether a newHeads subscription is live right now.
func (s *HeadSubscriber) Connected() bool { return s.connected.Load() }

// Run keeps the subscription alive until ctx ends, calling fn for every head from the goroutine that
// called Run. Connection loss is retried with exponential back-off, which resets after each successful
// subscription.
func (s *HeadSubscriber) Run(ctx context.Context, fn func(Head)) {
	backoff := s.minBackoff
	for ctx.Err() == nil {
		subscribed, err := s.runOnce(ctx, fn)
		if ctx.Err() != nil {
			return
		}
		if subscribed {
			backoff = s.minBackoff
			s.log.Warn("newHeads subscription lost, polling until it reconnects", "err", err.Error(), "retryIn", backoff.String())
		} else {
			s.log.Warn("newHeads subscription failed, polling until it connects", "err", err.Error(), "retryIn", backoff.String())
		}
		if err := s.sleep(ctx, backoff); err != nil {
			return
		}
		backoff = min(backoff*2, s.maxBackoff)
	}
}

// target resolves the endpoint of one connection attempt: its URL, the scrubber that keeps that URL
// out of every error, and the lease that reports the outcome (nil for a plain URL).
type target struct {
	url   string
	scrub *scrubber
	lease *WSLease
}

func (s *HeadSubscriber) target(ctx context.Context) (target, error) {
	if s.lease != nil {
		l, err := s.lease(ctx)
		if err != nil {
			return target{}, err
		}
		return target{url: l.URL, scrub: l.scrub, lease: l}, nil
	}
	u, err := s.url(ctx)
	if err != nil {
		return target{}, err
	}
	return target{url: u, scrub: newScrubber(0, u)}, nil
}

// runOnce dials, subscribes and delivers heads until the connection ends; subscribed reports whether
// the subscription was acknowledged. Every error is sanitized: the dialer reports a transport failure
// as a *url.Error carrying the whole URL, and a provider can quote the request back in an error.
func (s *HeadSubscriber) runOnce(ctx context.Context, fn func(Head)) (subscribed bool, err error) {
	t, err := s.target(ctx)
	if err != nil {
		return false, fmt.Errorf("resolve endpoint: %w", err)
	}
	defer func() {
		// A failure the endpoint is responsible for: shutdown is not.
		if err != nil && ctx.Err() == nil {
			t.lease.Failed(subscribed, err)
		}
	}()
	conn, resp, err := websocket.Dial(ctx, t.url, &websocket.DialOptions{
		HTTPClient: s.httpClient,
		HTTPHeader: http.Header{"User-Agent": {s.userAgent}},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return false, fmt.Errorf("dial: %w", t.scrub.wrap(err))
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(headsReadLimit)

	subID, err := s.subscribe(ctx, conn)
	if err != nil {
		return false, t.scrub.wrap(err)
	}
	t.lease.Connected()
	s.connected.Store(true)
	defer s.connected.Store(false)
	s.log.Info("newHeads subscription connected", "endpoint", t.endpoint(), "subscription", subID)

	pingCtx, stopPing := context.WithCancel(ctx)
	defer stopPing()
	go s.pingLoop(pingCtx, conn)

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return true, fmt.Errorf("read: %w", t.scrub.wrap(err))
		}
		head, ok, err := parseHeadNotification(data, subID)
		if err != nil {
			s.log.Warn("undecodable newHeads notification", "err", err.Error())
			continue
		}
		if ok {
			fn(head)
		}
	}
}

// endpoint names the leased endpoint for a log line, -1 for a plain URL.
func (t target) endpoint() int {
	if t.lease == nil {
		return -1
	}
	return t.lease.Index
}

// subscribe sends eth_subscribe and waits for its acknowledgement within the handshake limit.
// Notifications before the ack are ignored: nothing can be attributed to a subscription id yet.
func (s *HeadSubscriber) subscribe(ctx context.Context, conn *websocket.Conn) (string, error) {
	hctx, cancel := context.WithTimeout(ctx, s.httpClient.Timeout)
	defer cancel()
	payload, err := json.Marshal(rpcRequest{JSONRPC: jsonrpcVersion, ID: headsSubscribeID, Method: methodSubscribe, Params: []any{subscriptionNewHeads}})
	if err != nil {
		return "", fmt.Errorf("encode subscribe: %w", err)
	}
	if err := conn.Write(hctx, websocket.MessageText, payload); err != nil {
		return "", fmt.Errorf("subscribe: %w", err)
	}
	for {
		_, data, err := conn.Read(hctx)
		if err != nil {
			return "", fmt.Errorf("subscribe ack: %w", err)
		}
		var ack rpcResponse
		if err := json.Unmarshal(data, &ack); err != nil || ack.ID != headsSubscribeID {
			continue
		}
		if ack.Error != nil {
			return "", fmt.Errorf("subscribe: %w", ack.Error)
		}
		var id string
		if err := json.Unmarshal(ack.Result, &id); err != nil || id == "" {
			return "", errors.New("subscribe: malformed subscription id")
		}
		return id, nil
	}
}

// pingLoop pings the peer every pingInterval and closes the connection when a pong does not arrive,
// which makes the reader fail and Run reconnect.
func (s *HeadSubscriber) pingLoop(ctx context.Context, conn *websocket.Conn) {
	for {
		if err := s.sleep(ctx, s.pingInterval); err != nil {
			return
		}
		pctx, cancel := context.WithTimeout(ctx, s.pingInterval)
		err := conn.Ping(pctx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("newHeads socket unresponsive, closing it", "err", err.Error())
				_ = conn.CloseNow()
			}
			return
		}
	}
}

// headNotification is the eth_subscription envelope.
type headNotification struct {
	Method string `json:"method"`
	Params struct {
		Subscription string `json:"subscription"`
		Result       struct {
			Number    string `json:"number"`
			Hash      string `json:"hash"`
			Timestamp string `json:"timestamp"`
		} `json:"result"`
	} `json:"params"`
}

// parseHeadNotification decodes one message. ok is false for messages that are not newHeads
// notifications for subID.
func parseHeadNotification(data []byte, subID string) (Head, bool, error) {
	var n headNotification
	if err := json.Unmarshal(data, &n); err != nil {
		return Head{}, false, fmt.Errorf("decode notification: %w", err)
	}
	if n.Method != methodSubscription || n.Params.Subscription != subID {
		return Head{}, false, nil
	}
	h := Head{Hash: n.Params.Result.Hash}
	var err error
	if h.Number, err = HexUint64(n.Params.Result.Number); err != nil {
		return Head{}, false, fmt.Errorf("head number: %w", err)
	}
	if n.Params.Result.Timestamp != "" {
		if h.Timestamp, err = HexUint64(n.Params.Result.Timestamp); err != nil {
			return Head{}, false, fmt.Errorf("head timestamp: %w", err)
		}
	}
	return h, true, nil
}
