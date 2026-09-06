package nitro

import (
	"context"
	"sync"
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
	if NewPacer(0.25).burst != 1 || NewPacer(0.25).MaxBatch() != 1 || p.MaxBatch() != 8 || NewPacer(1000).MaxBatch() != MaxBatch {
		t.Fatal("small rates keep a burst of one; a batch never exceeds the burst")
	}
	// Tokens are never borrowed: a request for the whole burst waits until
	// the bucket is full again, and one above the burst waits for a full
	// bucket and overdraws by the excess alone.
	clock.Advance(time.Hour)
	if err := p.Wait(ctx, 5); err != nil {
		t.Fatal(err)
	}
	before := len(clock.Sleeps())
	if err := p.Wait(ctx, 8); err != nil {
		t.Fatal(err)
	}
	if s := clock.Sleeps()[before:]; len(s) != 1 || s[0] != 1250*time.Millisecond {
		t.Fatalf("full-burst wait = %v", s)
	}
	clock.Advance(time.Hour)
	if err := p.Wait(ctx, 10); err != nil || p.Available() != 0 {
		t.Fatalf("overdraw: %v available %d", err, p.Available())
	}
	clock.Advance(500 * time.Millisecond)
	if p.Available() != 0 {
		t.Fatalf("the excess is paid back first: %d", p.Available())
	}
}

// TestPacerFIFO: callers take tokens in arrival order, so a large batch
// queued behind the head of the line goes out before a small call that
// arrived after it, instead of being starved by it.
func TestPacerFIFO(t *testing.T) {
	p := NewPacer(1000)
	ctx := context.Background()
	if err := p.Wait(ctx, 2000); err != nil { // drain the bucket
		t.Fatal(err)
	}
	p.turn <- struct{}{} // hold the turnstile so the callers queue behind it
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	for i, n := range []int{100, 1} {
		wg.Add(1)
		go func(i, n int) {
			defer wg.Done()
			if err := p.Wait(ctx, n); err != nil {
				t.Error(err)
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
		}(i, n)
		time.Sleep(20 * time.Millisecond) // the big one is first in line
	}
	<-p.turn
	wg.Wait()
	if len(order) != 2 || order[0] != 0 {
		t.Fatalf("order = %v, the first caller must go first", order)
	}
	canceled, cancel := context.WithCancel(ctx)
	p.turn <- struct{}{}
	cancel()
	if err := p.Wait(canceled, 1); err == nil {
		t.Fatal("a queued caller honors cancellation")
	}
	<-p.turn
}

// TestPacerWindowedBudget: whatever the callers do, no window of ten
// seconds ever carries more than burst + rate*10 calls.
func TestPacerWindowedBudget(t *testing.T) {
	clock := newFakeClock()
	p := NewPacer(4).withClock(clock.Now, clock.Sleep)
	ctx := context.Background()
	sizes := []int{8, 1, 4, 8, 8, 100, 4, 4, 20, 1, 8, 8, 8, 1, 4, 4}
	sent := make([]struct {
		at time.Time
		n  int
	}, 0, len(sizes))
	for i, n := range sizes {
		size := min(n, p.MaxBatch())
		if err := p.Wait(ctx, size); err != nil {
			t.Fatal(err)
		}
		sent = append(sent, struct {
			at time.Time
			n  int
		}{clock.Now(), size})
		if i%3 == 0 {
			clock.Advance(700 * time.Millisecond)
		}
		total := 0
		for _, s := range sent {
			if !s.at.Before(clock.Now().Add(-10 * time.Second)) {
				total += s.n
			}
		}
		if total > 8+40 {
			t.Fatalf("request %d: %d calls in the last 10 s exceed the budget", i, total)
		}
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
