package nitro

import (
	"context"
	"testing"
	"time"
)

func TestPacer(t *testing.T) {
	clock := newFakeClock()
	p := NewPacer(4).withClock(clock.Now, clock.Sleep)
	if p.Rate() != 4 || p.Available() != 8 {
		t.Fatalf("rate %v available %d", p.Rate(), p.Available())
	}
	ctx := context.Background()
	if err := p.Wait(ctx, 8); err != nil || len(clock.Sleeps()) != 0 {
		t.Fatalf("burst should not sleep: %v %v", err, clock.Sleeps())
	}
	if p.Available() != 0 {
		t.Fatalf("available after burst = %d", p.Available())
	}
	// Four more tokens cost one second at 4/s.
	if err := p.Wait(ctx, 4); err != nil {
		t.Fatal(err)
	}
	if s := clock.Sleeps(); len(s) != 1 || s[0] != time.Second {
		t.Fatalf("sleeps = %v", s)
	}
	// Time passing refills, capped at the burst.
	clock.Advance(time.Hour)
	if p.Available() != 8 {
		t.Fatalf("refill = %d", p.Available())
	}
	if err := p.Wait(ctx, 0); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := p.Wait(canceled, 100); err == nil {
		t.Fatal("expected context error")
	}
	if NewPacer(0.25).burst != 1 {
		t.Fatal("small rates keep a burst of one")
	}
}

func TestPacerUnlimited(t *testing.T) {
	clock := newFakeClock()
	p := NewPacer(0).withClock(clock.Now, clock.Sleep)
	if !p.Unlimited() || p.Rate() != 0 || p.Available() < MaxBatch*1000 || NewPacer(-1).Unlimited() != true {
		t.Fatalf("unlimited pacer: rate %v available %d", p.Rate(), p.Available())
	}
	ctx := context.Background()
	for range 100 {
		if err := p.Wait(ctx, MaxBatch); err != nil {
			t.Fatal(err)
		}
	}
	if len(clock.Sleeps()) != 0 || p.Available() < MaxBatch {
		t.Fatalf("unlimited pacer must never sleep: %v", clock.Sleeps())
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := p.Wait(canceled, 1); err == nil {
		t.Fatal("a canceled context is still honored")
	}
}

func TestSleepContext(t *testing.T) {
	if err := sleepContext(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if err := sleepContext(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepContext(ctx, time.Hour); err == nil {
		t.Fatal("expected cancellation")
	}
}
