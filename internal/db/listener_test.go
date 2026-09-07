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
	closed        chan struct{}
	closeOnce     sync.Once
	callback      pq.EventCallbackType
}

func newFakeNotificationListener() *fakeNotificationListener {
	return &fakeNotificationListener{
		notifications: make(chan *pq.Notification, 4),
		closed:        make(chan struct{}),
	}
}

func (f *fakeNotificationListener) Listen(string) error {
	select {
	case <-f.closed:
		return errors.New("closed")
	default:
		return nil
	}
}

func (f *fakeNotificationListener) NotificationChannel() <-chan *pq.Notification {
	return f.notifications
}

func (f *fakeNotificationListener) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

func newFakeListener(t *testing.T, channels ...string) (*PQListener, *fakeNotificationListener) {
	t.Helper()
	raw := newFakeNotificationListener()
	l, err := newListener(context.Background(), channels, nil, func(callback pq.EventCallbackType) notificationListener {
		raw.callback = callback
		return raw
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, raw
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

func TestPQListenerDeliversNotifications(t *testing.T) {
	l, raw := newFakeListener(t, ChannelLive, ChannelOwnerAction)
	if status := l.Status(); !status.Ready || status.Reconnects != 0 || status.Error != "" {
		t.Fatalf("initial status: %+v", status)
	}
	raw.notifications <- &pq.Notification{Channel: ChannelLive, Extra: "payload"}
	select {
	case got := <-l.Notifications():
		if got.Channel != ChannelLive || got.Payload != "payload" || got.Reconnected {
			t.Fatalf("notification: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("notification was not delivered")
	}
}

// TestPQListenerStatusTracksLibPQReconnect covers the recovery that actually
// happens: lib/pq drops the connection, re-establishes it, re-issues every
// LISTEN, and marks the gap with a nil notification the hub reconciles from.
// Readiness follows it so a replica with no feed leaves the Service.
func TestPQListenerStatusTracksLibPQReconnect(t *testing.T) {
	l, raw := newFakeListener(t, ChannelLive)

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

// TestPQListenerClosedNotificationChannelEndsTheStream pins the contract
// Hub.Run relies on. lib/pq only closes its notification channel from Close,
// so this cannot happen while the process wants the feed; if it ever does,
// the stream ends rather than going quiet, which is what makes the hub exit
// instead of pinging clients it can never update again.
func TestPQListenerClosedNotificationChannelEndsTheStream(t *testing.T) {
	l, raw := newFakeListener(t, ChannelLive)
	close(raw.notifications)
	select {
	case _, ok := <-l.Notifications():
		if ok {
			t.Fatal("expected the stream to end")
		}
	case <-time.After(time.Second):
		t.Fatal("stream stayed open after the notification channel closed")
	}
	if status := l.Status(); status.Ready || status.Error != "notification channel closed" {
		t.Fatalf("status: %+v", status)
	}
}

func TestPQListenerCloseReportsAndIsIdempotent(t *testing.T) {
	l, _ := newFakeListener(t, ChannelLive)
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if status := l.Status(); status.Ready || status.Error != "listener closed" {
		t.Fatalf("closed status: %+v", status)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	select {
	case _, ok := <-l.Notifications():
		if ok {
			t.Fatal("expected the stream to end after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("stream stayed open after Close")
	}
}
