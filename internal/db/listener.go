package db

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lib/pq"

	"github.com/tirante-dev/gascurve/internal/logger"
)

const (
	listenerReconnectMinDelay = time.Second
	listenerReconnectMaxDelay = 30 * time.Second
)

// Notification is one LISTEN event. Reconnected is set (with an empty
// channel) when the connection was re-established: notifications sent
// meanwhile were lost and the consumer has to reconcile from the tables.
type Notification struct {
	Channel     string
	Payload     string
	Reconnected bool
}

// ListenerStatus is the current health of the LISTEN connection. Reconnects
// counts successful recoveries after the initial subscription. Error is empty
// while the listener is ready.
type ListenerStatus struct {
	Ready      bool
	Reconnects uint64
	Error      string
}

// Listener delivers NOTIFY payloads. The API hub consumes this interface so
// it can be tested without Postgres.
type Listener interface {
	Notifications() <-chan Notification
	Close() error
}

// ListenerStatusReporter exposes listener health without making it a
// requirement for test or alternate notification sources.
type ListenerStatusReporter interface {
	Status() ListenerStatus
}

type notificationListener interface {
	Listen(string) error
	NotificationChannel() <-chan *pq.Notification
	Close() error
}

type listenerFactory func(pq.EventCallbackType) notificationListener

// PQListener supervises lib/pq's LISTEN support. lib/pq reconnects ordinary
// connection failures itself. If its top-level notification channel closes
// unexpectedly, PQListener replaces the whole listener and keeps the stable
// Notifications channel alive.
type PQListener struct {
	channels []string
	factory  listenerFactory
	out      chan Notification
	done     chan struct{}
	once     sync.Once
	log      *logger.Logger
	minDelay time.Duration
	maxDelay time.Duration

	mu         sync.Mutex
	current    notificationListener
	generation uint64
	closed     bool
	status     ListenerStatus
	everReady  bool
	eventKnown bool
	connected  bool
	subscribed bool
}

// NewListener subscribes to channels on url. It blocks until the initial
// subscriptions are established or ctx is done.
func NewListener(ctx context.Context, url string, channels []string, log *logger.Logger) (*PQListener, error) {
	factory := func(callback pq.EventCallbackType) notificationListener {
		return pq.NewListener(url, listenerReconnectMinDelay, listenerReconnectMaxDelay, callback)
	}
	return newListener(ctx, channels, log, factory, listenerReconnectMinDelay, listenerReconnectMaxDelay)
}

func newListener(
	ctx context.Context,
	channels []string,
	log *logger.Logger,
	factory listenerFactory,
	minDelay time.Duration,
	maxDelay time.Duration,
) (*PQListener, error) {
	if log == nil {
		log = logger.Nop()
	}
	pl := &PQListener{
		channels: append([]string(nil), channels...), factory: factory,
		out: make(chan Notification, 256), done: make(chan struct{}), log: log,
		minDelay: minDelay, maxDelay: maxDelay,
	}
	l, generation, err := pl.connect(ctx)
	if err != nil {
		return nil, err
	}
	go pl.run(context.WithoutCancel(ctx), l, generation)
	return pl, nil
}

func (pl *PQListener) connect(ctx context.Context) (notificationListener, uint64, error) {
	pl.mu.Lock()
	if pl.closed {
		pl.mu.Unlock()
		return nil, 0, errors.New("listener closed")
	}
	pl.generation++
	generation := pl.generation
	pl.eventKnown = false
	pl.connected = false
	pl.subscribed = false
	pl.mu.Unlock()

	l := pl.factory(func(event pq.ListenerEventType, err error) {
		pl.listenerEvent(generation, event, err)
	})
	pl.mu.Lock()
	if pl.closed || pl.generation != generation {
		pl.mu.Unlock()
		_ = l.Close()
		return nil, 0, errors.New("listener closed")
	}
	pl.current = l
	pl.mu.Unlock()

	errC := make(chan error, 1)
	go func() {
		for _, channel := range pl.channels {
			if err := l.Listen(channel); err != nil {
				errC <- fmt.Errorf("listen %s: %w", channel, err)
				return
			}
		}
		errC <- nil
	}()
	select {
	case err := <-errC:
		if err != nil {
			pl.markUnavailable(generation, "subscription failed")
			pl.retire(generation, l)
			return nil, 0, err
		}
		pl.markSubscribed(generation)
		return l, generation, nil
	case <-ctx.Done():
		pl.retire(generation, l)
		return nil, 0, fmt.Errorf("listener: %w", ctx.Err())
	case <-pl.done:
		pl.retire(generation, l)
		return nil, 0, errors.New("listener closed")
	}
}

func (pl *PQListener) run(ctx context.Context, l notificationListener, generation uint64) {
	defer close(pl.out)
	delay := pl.minDelay
	for {
		if pl.pump(l) {
			return
		}
		pl.markUnavailable(generation, "notification channel closed")
		pl.retire(generation, l)

		for {
			if !pl.wait(delay) {
				return
			}
			var err error
			l, generation, err = pl.connect(ctx)
			if err == nil {
				pl.log.Info("postgres listener recovered")
				if !pl.deliver(Notification{Reconnected: true}) {
					return
				}
				delay = pl.minDelay
				break
			}
			if pl.isClosed() {
				return
			}
			pl.log.Warn("postgres listener replacement failed", "err", err.Error())
			delay *= 2
			if delay > pl.maxDelay {
				delay = pl.maxDelay
			}
		}
	}
}

// pump forwards one underlying listener. It reports true only when the
// PQListener itself is closing.
func (pl *PQListener) pump(l notificationListener) bool {
	for {
		select {
		case <-pl.done:
			return true
		case n, ok := <-l.NotificationChannel():
			if !ok {
				return false
			}
			out := Notification{Reconnected: true}
			if n != nil {
				out = Notification{Channel: n.Channel, Payload: n.Extra}
			}
			if !pl.deliver(out) {
				return true
			}
		}
	}
}

func (pl *PQListener) deliver(n Notification) bool {
	select {
	case pl.out <- n:
		return true
	case <-pl.done:
		return false
	}
}

func (pl *PQListener) wait(delay time.Duration) bool {
	if delay <= 0 {
		return !pl.isClosed()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-pl.done:
		return false
	}
}

func (pl *PQListener) listenerEvent(generation uint64, event pq.ListenerEventType, err error) {
	if err != nil {
		pl.log.Warn("postgres listener event", "event", int(event), "err", err.Error())
	}
	switch event {
	case pq.ListenerEventDisconnected:
		pl.markUnavailable(generation, "connection lost")
	case pq.ListenerEventConnectionAttemptFailed:
		pl.markUnavailable(generation, "connection attempt failed")
	case pq.ListenerEventConnected, pq.ListenerEventReconnected:
		pl.markConnected(generation)
	}
}

func (pl *PQListener) markSubscribed(generation uint64) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if pl.closed || pl.generation != generation {
		return
	}
	pl.subscribed = true
	// A successful Listen proves the connection when no event has reported
	// otherwise. If a disconnect raced the final subscription, its event wins
	// and the later Connected or Reconnected event restores readiness.
	if pl.eventKnown && !pl.connected {
		return
	}
	pl.markReadyLocked()
}

func (pl *PQListener) markConnected(generation uint64) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if pl.closed || pl.generation != generation {
		return
	}
	pl.eventKnown = true
	pl.connected = true
	if pl.subscribed {
		pl.markReadyLocked()
	}
}

func (pl *PQListener) markReadyLocked() {
	if !pl.status.Ready && pl.everReady {
		pl.status.Reconnects++
	}
	pl.status.Ready = true
	pl.status.Error = ""
	pl.everReady = true
}

func (pl *PQListener) markUnavailable(generation uint64, reason string) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if pl.closed || pl.generation != generation {
		return
	}
	pl.eventKnown = true
	pl.connected = false
	pl.status.Ready = false
	pl.status.Error = reason
}

func (pl *PQListener) retire(generation uint64, l notificationListener) {
	pl.mu.Lock()
	if pl.generation == generation {
		pl.generation++
		if pl.current == l {
			pl.current = nil
		}
	}
	pl.mu.Unlock()
	_ = l.Close()
}

func (pl *PQListener) isClosed() bool {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	return pl.closed
}

// Notifications returns the stable delivery channel. It closes only when
// Close is called, not when an underlying lib/pq listener fails.
func (pl *PQListener) Notifications() <-chan Notification { return pl.out }

// Status returns a consistent snapshot of listener health.
func (pl *PQListener) Status() ListenerStatus {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	return pl.status
}

// Close stops delivery and closes the current connection.
func (pl *PQListener) Close() error {
	pl.once.Do(func() {
		pl.mu.Lock()
		pl.closed = true
		pl.status.Ready = false
		pl.status.Error = "listener closed"
		pl.generation++
		l := pl.current
		pl.current = nil
		close(pl.done)
		pl.mu.Unlock()
		if l != nil {
			_ = l.Close()
		}
	})
	return nil
}
