package db

import (
	"context"
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

// notificationListener is the part of *pq.Listener this package uses,
// narrowed so the status transitions can be driven from a test without a
// Postgres to disconnect.
type notificationListener interface {
	Listen(string) error
	NotificationChannel() <-chan *pq.Notification
	Close() error
}

type listenerFactory func(pq.EventCallbackType) notificationListener

// PQListener is a Listener over lib/pq's LISTEN support. lib/pq owns the
// reconnection; PQListener adds only the health lib/pq reports through its
// event callback, so readiness can take a replica whose notification feed is
// down out of the Service while its query pool keeps working.
//
// Nothing here replaces a failed lib/pq listener, because a failed one cannot
// be observed: lib/pq closes the notification channel in exactly one place,
// listenerMain, after listenerConnLoop returns, and both of that loop's
// returns are guarded by l.closed(), which only Close sets. A closed channel
// therefore means this process closed it. If one ever closes anyway, Hub.Run
// treats it as fatal and the process restarts, which recovers more simply
// than swapping the listener underneath a live hub.
type PQListener struct {
	l    notificationListener
	out  chan Notification
	done chan struct{}
	once sync.Once
	log  *logger.Logger

	mu         sync.Mutex
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
	return newListener(ctx, channels, log, func(callback pq.EventCallbackType) notificationListener {
		return pq.NewListener(url, listenerReconnectMinDelay, listenerReconnectMaxDelay, callback)
	})
}

func newListener(ctx context.Context, channels []string, log *logger.Logger, factory listenerFactory) (*PQListener, error) {
	if log == nil {
		log = logger.Nop()
	}
	pl := &PQListener{out: make(chan Notification, 256), done: make(chan struct{}), log: log}
	pl.l = factory(pl.listenerEvent)
	errc := make(chan error, 1)
	go func() {
		for _, channel := range channels {
			if err := pl.l.Listen(channel); err != nil {
				errc <- fmt.Errorf("listen %s: %w", channel, err)
				return
			}
		}
		errc <- nil
	}()
	select {
	case err := <-errc:
		if err != nil {
			_ = pl.l.Close()
			return nil, err
		}
	case <-ctx.Done():
		_ = pl.l.Close()
		return nil, fmt.Errorf("listener: %w", ctx.Err())
	}
	pl.markSubscribed()
	go pl.pump()
	return pl, nil
}

func (pl *PQListener) pump() {
	defer close(pl.out)
	for {
		select {
		case <-pl.done:
			return
		case n, ok := <-pl.l.NotificationChannel():
			if !ok {
				pl.markUnavailable("notification channel closed")
				return
			}
			out := Notification{Reconnected: true}
			if n != nil {
				out = Notification{Channel: n.Channel, Payload: n.Extra}
			}
			select {
			case pl.out <- out:
			case <-pl.done:
				return
			}
		}
	}
}

func (pl *PQListener) listenerEvent(event pq.ListenerEventType, err error) {
	if err != nil {
		pl.log.Warn("postgres listener event", "event", int(event), "err", err.Error())
	}
	switch event {
	case pq.ListenerEventDisconnected:
		pl.markUnavailable("connection lost")
	case pq.ListenerEventConnectionAttemptFailed:
		pl.markUnavailable("connection attempt failed")
	case pq.ListenerEventConnected, pq.ListenerEventReconnected:
		pl.markConnected()
	}
}

func (pl *PQListener) markSubscribed() {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if pl.closed {
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

func (pl *PQListener) markConnected() {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if pl.closed {
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

func (pl *PQListener) markUnavailable(reason string) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if pl.closed {
		return
	}
	pl.eventKnown = true
	pl.connected = false
	pl.status.Ready = false
	pl.status.Error = reason
}

// Notifications returns the delivery channel.
func (pl *PQListener) Notifications() <-chan Notification { return pl.out }

// Status returns a consistent snapshot of listener health.
func (pl *PQListener) Status() ListenerStatus {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	return pl.status
}

// Close stops delivery and closes the connection.
func (pl *PQListener) Close() error {
	var err error
	pl.once.Do(func() {
		pl.mu.Lock()
		pl.closed = true
		pl.status.Ready = false
		pl.status.Error = "listener closed"
		pl.mu.Unlock()
		close(pl.done)
		err = pl.l.Close()
	})
	return err
}
