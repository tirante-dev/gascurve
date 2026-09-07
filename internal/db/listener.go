package db

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/lib/pq"

	"github.com/tirante-dev/gascurve/internal/logger"
)

// Notification is one LISTEN event. Reconnected is set (with an empty
// channel) when the connection was re-established: notifications sent
// meanwhile were lost and the consumer has to reconcile from the tables.
type Notification struct {
	Channel     string
	Payload     string
	Reconnected bool
}

// Listener delivers NOTIFY payloads. The API hub consumes this interface so
// it can be tested without Postgres.
type Listener interface {
	Notifications() <-chan Notification
	Close() error
}

// PQListener is a Listener over lib/pq's LISTEN support with automatic
// reconnection.
type PQListener struct {
	l    *pq.Listener
	out  chan Notification
	done chan struct{}
	once sync.Once
	log  *logger.Logger
}

// NewListener subscribes to channels on url. It blocks until the
// subscriptions are established or ctx is done.
func NewListener(ctx context.Context, url string, channels []string, log *logger.Logger) (*PQListener, error) {
	if log == nil {
		log = logger.Nop()
	}
	pl := &PQListener{out: make(chan Notification, 256), done: make(chan struct{}), log: log}
	pl.l = pq.NewListener(url, time.Second, 30*time.Second, func(ev pq.ListenerEventType, err error) {
		if err != nil {
			log.Warn("postgres listener event", "event", int(ev), "err", err.Error())
		}
	})
	errc := make(chan error, 1)
	go func() {
		for _, ch := range channels {
			if err := pl.l.Listen(ch); err != nil {
				errc <- fmt.Errorf("listen %s: %w", ch, err)
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
	go pl.pump()
	return pl, nil
}

func (pl *PQListener) pump() {
	defer close(pl.out)
	for {
		select {
		case <-pl.done:
			return
		case n, ok := <-pl.l.Notify:
			if !ok {
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

// Notifications returns the delivery channel.
func (pl *PQListener) Notifications() <-chan Notification { return pl.out }

// Close stops delivery and closes the connection.
func (pl *PQListener) Close() error {
	var err error
	pl.once.Do(func() {
		close(pl.done)
		err = pl.l.Close()
	})
	return err
}
