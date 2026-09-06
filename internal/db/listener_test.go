package db

import (
	"context"
	"testing"
	"time"

	"github.com/tirante-dev/gascurve/internal/logger"
)

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
