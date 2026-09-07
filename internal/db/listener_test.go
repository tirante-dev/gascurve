package db

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/tirante-dev/gascurve/internal/logger"
)

type fakeNotificationListener struct {
	notifications chan *pq.Notification
	listenGate    chan struct{}
	closed        chan struct{}
	closeOnce     sync.Once
	callback      pq.EventCallbackType
}

func newFakeNotificationListener() *fakeNotificationListener {
	return &fakeNotificationListener{
		notifications: make(chan *pq.Notification, 4),
		listenGate:    make(chan struct{}),
		closed:        make(chan struct{}),
	}
}

func (f *fakeNotificationListener) Listen(string) error {
	select {
	case <-f.listenGate:
		return nil
	case <-f.closed:
		return errors.New("closed")
	}
}

func (f *fakeNotificationListener) NotificationChannel() <-chan *pq.Notification {
	return f.notifications
}

func (f *fakeNotificationListener) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

func TestPQListenerUnreachable(t *testing.T) {
	// An unreachable server never connects, so Listen blocks until the
	// context expires and NewListener returns an error after closing.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	l, err := NewListener(ctx, "postgres://postgres@127.0.0.1:1/x?sslmode=disable&connect_timeout=1", []string{ChannelLive, ChannelOwnerAction}, logger.Nop())
	if err == nil || l != nil {
		t.Fatalf("expected timeout error, got %v %v", l, err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	if _, err := NewListener(ctx2, "postgres://postgres@127.0.0.1:1/x?sslmode=disable&connect_timeout=1", []string{ChannelLive}, nil); err == nil {
		t.Fatal("expected error with nil logger too")
	}
}

// TestPQListenerRecoversClosedNotificationChannel proves that an unexpected
// close of lib/pq's top-level Notify channel does not close the stable API
// stream. Readiness drops while replacement subscriptions are blocked, then a
// reconnect marker makes the hub reconcile notifications lost in that gap.
func TestPQListenerRecoversClosedNotificationChannel(t *testing.T) {
	first := newFakeNotificationListener()
	second := newFakeNotificationListener()
	close(first.listenGate)
	createdSecond := make(chan struct{})
	var mu sync.Mutex
	created := 0
	factory := func(callback pq.EventCallbackType) notificationListener {
		mu.Lock()
		defer mu.Unlock()
		created++
		if created == 1 {
			first.callback = callback
			return first
		}
		second.callback = callback
		select {
		case <-createdSecond:
		default:
			close(createdSecond)
		}
		return second
	}
	l, err := newListener(context.Background(), []string{ChannelLive, ChannelOwnerAction}, nil, factory, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if status := l.Status(); !status.Ready || status.Reconnects != 0 || status.Error != "" {
		t.Fatalf("initial status: %+v", status)
	}

	first.notifications <- &pq.Notification{Channel: ChannelLive, Extra: "before"}
	select {
	case got := <-l.Notifications():
		if got.Channel != ChannelLive || got.Payload != "before" || got.Reconnected {
			t.Fatalf("initial notification: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("initial notification was not delivered")
	}
	close(first.notifications)
	select {
	case <-createdSecond:
	case <-time.After(time.Second):
		t.Fatal("replacement listener was not created")
	}
	if status := l.Status(); status.Ready || status.Error != "notification channel closed" {
		t.Fatalf("status during replacement: %+v", status)
	}
	select {
	case _, ok := <-l.Notifications():
		if !ok {
			t.Fatal("stable notification stream closed during recovery")
		}
	default:
	}

	close(second.listenGate)
	select {
	case got := <-l.Notifications():
		if !got.Reconnected || got.Channel != "" || got.Payload != "" {
			t.Fatalf("reconnect marker: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("reconnect marker was not delivered")
	}
	if status := l.Status(); !status.Ready || status.Reconnects != 1 || status.Error != "" {
		t.Fatalf("recovered status: %+v", status)
	}
	second.notifications <- &pq.Notification{Channel: ChannelOwnerAction, Extra: "after"}
	select {
	case got := <-l.Notifications():
		if got.Channel != ChannelOwnerAction || got.Payload != "after" || got.Reconnected {
			t.Fatalf("recovered notification: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("notification after recovery was not delivered")
	}
}

func TestPQListenerStatusTracksLibPQReconnect(t *testing.T) {
	raw := newFakeNotificationListener()
	close(raw.listenGate)
	factory := func(callback pq.EventCallbackType) notificationListener {
		raw.callback = callback
		return raw
	}
	l, err := newListener(context.Background(), []string{ChannelLive}, nil, factory, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	raw.callback(pq.ListenerEventDisconnected, errors.New("secret connection detail"))
	if status := l.Status(); status.Ready || status.Error != "connection lost" {
		t.Fatalf("disconnected status: %+v", status)
	}
	raw.callback(pq.ListenerEventConnectionAttemptFailed, errors.New("still down"))
	if status := l.Status(); status.Ready || status.Error != "connection attempt failed" {
		t.Fatalf("retry status: %+v", status)
	}
	raw.callback(pq.ListenerEventReconnected, nil)
	raw.notifications <- nil
	select {
	case got := <-l.Notifications():
		if !got.Reconnected {
			t.Fatalf("lib/pq reconnect marker: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("lib/pq reconnect marker was not delivered")
	}
	if status := l.Status(); !status.Ready || status.Reconnects != 1 || status.Error != "" {
		t.Fatalf("reconnected status: %+v", status)
	}
}
