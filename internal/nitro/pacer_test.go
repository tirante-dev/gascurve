package nitro

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func fastCtx(ctx context.Context) context.Context { return WithClass(ctx, Fast) }

func TestPacer(t *testing.T) {
	clock := newFakeClock()
	p := NewPacer(4).withClock(clock.Now, clock.Sleep)
	// Burst 8, one token of it reserved for the fast lane: a bulk caller
	// sees 7 and may not batch more than that.
	if p.Rate() != 4 || p.Reserve() != 1 || p.Available() != 7 || p.MaxBatch() != 7 {
		t.Fatalf("rate %v reserve %v available %d maxBatch %d", p.Rate(), p.Reserve(), p.Available(), p.MaxBatch())
	}
	ctx := context.Background()
	if err := p.Wait(fastCtx(ctx), 8); err != nil || len(clock.Sleeps()) != 0 {
		t.Fatalf("a fast caller may take the whole burst without sleeping: %v %v", err, clock.Sleeps())
	}
	if p.Available() != 0 {
		t.Fatalf("available after burst = %d", p.Available())
	}
	// Four more fast tokens cost one second at 4/s.
	if err := p.Wait(fastCtx(ctx), 4); err != nil {
		t.Fatal(err)
	}
	if s := clock.Sleeps(); len(s) != 1 || s[0] != time.Second {
		t.Fatalf("sleeps = %v", s)
	}
	// Time passing refills, capped at the burst.
	clock.Advance(time.Hour)
	if p.Available() != 7 {
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
	if small := NewPacer(0.25); small.burst != 1 || small.Reserve() != 1 || small.MaxBatch() != 1 || NewPacer(1000).MaxBatch() != MaxBatch {
		t.Fatal("small rates keep a burst of one; a batch never exceeds the burst minus the reserve")
	}
	// A bulk caller never draws the bucket below the reserve: the whole
	// bulk share goes without sleeping and leaves the reserve for a fast
	// call; the next bulk token then waits for the reserve to refill too.
	before := len(clock.Sleeps())
	if err := p.Wait(ctx, 7); err != nil || len(clock.Sleeps()) != before || p.Available() != 0 || p.tokens != 1 {
		t.Fatalf("bulk share: %v sleeps %v available %d tokens %v", err, clock.Sleeps()[before:], p.Available(), p.tokens)
	}
	if err := p.Wait(fastCtx(ctx), 1); err != nil || len(clock.Sleeps()) != before || p.tokens != 0 {
		t.Fatalf("the reserve serves a fast call at once: %v sleeps %v tokens %v", err, clock.Sleeps()[before:], p.tokens)
	}
	if err := p.Wait(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if s := clock.Sleeps()[before:]; len(s) != 1 || s[0] != 500*time.Millisecond {
		t.Fatalf("a bulk token waits for itself and the reserve: %v", s)
	}
	// Tokens are never borrowed: a request for the whole burst waits until
	// the bucket is full again, and one above the burst waits for a full
	// bucket and overdraws by the excess alone.
	clock.Advance(time.Hour)
	if err := p.Wait(fastCtx(ctx), 5); err != nil {
		t.Fatal(err)
	}
	before = len(clock.Sleeps())
	if err := p.Wait(fastCtx(ctx), 8); err != nil {
		t.Fatal(err)
	}
	if s := clock.Sleeps()[before:]; len(s) != 1 || s[0] != 1250*time.Millisecond {
		t.Fatalf("full-burst wait = %v", s)
	}
	clock.Advance(time.Hour)
	if err := p.Wait(fastCtx(ctx), 10); err != nil || p.Available() != 0 {
		t.Fatalf("overdraw: %v available %d", err, p.Available())
	}
	clock.Advance(750 * time.Millisecond)
	if p.Available() != 0 || p.tokens != 1 {
		t.Fatalf("the excess and then the reserve are paid back first: %d (%v tokens)", p.Available(), p.tokens)
	}
	clock.Advance(250 * time.Millisecond)
	if p.Available() != 1 {
		t.Fatalf("available above the reserve: %d", p.Available())
	}
	// A bulk request above its share waits for a full bucket and overdraws.
	clock.Advance(time.Hour)
	before = len(clock.Sleeps())
	if err := p.Wait(ctx, 9); err != nil || len(clock.Sleeps()) != before || p.tokens != -1 {
		t.Fatalf("bulk overdraw: %v sleeps %v tokens %v", err, clock.Sleeps()[before:], p.tokens)
	}
}

func TestClass(t *testing.T) {
	ctx := context.Background()
	if ClassOf(ctx) != Bulk || ClassOf(WithClass(ctx, Fast)) != Fast || ClassOf(WithClass(WithClass(ctx, Fast), Bulk)) != Bulk {
		t.Fatal("class from context")
	}
	if Bulk.String() != "bulk" || Fast.String() != "fast" {
		t.Fatal("class names")
	}
}

// hold takes the turnstile by hand so callers queue behind it; the
// returned function opens it again.
func (p *Pacer) hold() func() {
	p.mu.Lock()
	p.held = true
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		p.releaseLocked()
		p.mu.Unlock()
	}
}

// TestPacerQueueOrder: a fast caller is served before every queued bulk
// caller, and bulk callers take tokens in arrival order, so a large batch
// queued behind the head of the line goes out before a small call that
// arrived after it, instead of being starved by it.
func TestPacerQueueOrder(t *testing.T) {
	p := NewPacer(1000)
	ctx := context.Background()
	if err := p.Wait(fastCtx(ctx), 2000); err != nil { // drain the bucket
		t.Fatal(err)
	}
	release := p.hold()
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	callers := []struct {
		n   int
		ctx context.Context
	}{{100, ctx}, {1, ctx}, {1, fastCtx(ctx)}}
	for i, c := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.Wait(c.ctx, c.n); err != nil {
				t.Error(err)
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
		}()
		time.Sleep(20 * time.Millisecond) // the big bulk one is first in line
	}
	release()
	wg.Wait()
	if len(order) != 3 || order[0] != 2 || order[1] != 0 || order[2] != 1 {
		t.Fatalf("order = %v, want the fast caller first, then the bulk callers as they arrived", order)
	}
	// Queued callers of either class honor cancellation and leave the
	// queue.
	release = p.hold()
	qctx, qcancel := context.WithCancel(ctx)
	errs := make(chan error, 2)
	go func() { errs <- p.Wait(qctx, 1) }()
	go func() { errs <- p.Wait(WithClass(qctx, Fast), 1) }()
	time.Sleep(20 * time.Millisecond)
	qcancel()
	for range 2 {
		if err := <-errs; !errors.Is(err, context.Canceled) {
			t.Fatalf("a queued caller honors cancellation: %v", err)
		}
	}
	p.mu.Lock()
	queued := len(p.fast) + len(p.bulk)
	p.mu.Unlock()
	if queued != 0 {
		t.Fatalf("%d callers left in the queue", queued)
	}
	release()
	// A caller canceled just as it was handed the turnstile passes it on.
	p.hold()
	if err := p.park(qctx, &waiter{ready: make(chan struct{})}); !errors.Is(err, context.Canceled) || p.held {
		t.Fatalf("park after a handoff: %v held %v", err, p.held)
	}
}

// TestPacerFastPreemptsSleepingBulk: a bulk caller sleeping for tokens
// steps aside the moment a fast caller arrives, serves it, and resumes at
// the head of the bulk queue.
func TestPacerFastPreemptsSleepingBulk(t *testing.T) {
	clock := newFakeClock()
	slept := make(chan time.Duration, 8)
	unblock := make(chan struct{})
	// A sleeper that blocks until the test lets it go (advancing the clock
	// then) or the pacer interrupts it.
	sleep := func(ctx context.Context, d time.Duration) error {
		slept <- d
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-unblock:
			clock.Advance(d)
			return nil
		}
	}
	p := NewPacer(4).withClock(clock.Now, sleep)
	ctx := context.Background()
	if err := p.Wait(fastCtx(ctx), 8); err != nil { // drain the bucket
		t.Fatal(err)
	}
	bulkDone := make(chan error, 1)
	go func() { bulkDone <- p.Wait(ctx, 7) }()
	// The bulk caller needs its seven plus the reserve: two seconds.
	if d := <-slept; d != 2*time.Second {
		t.Fatalf("bulk sleep = %v", d)
	}
	fastDone := make(chan error, 1)
	go func() { fastDone <- p.Wait(fastCtx(ctx), 1) }()
	// The fast arrival interrupts the bulk sleep; the fast caller then
	// waits for its own token only.
	if d := <-slept; d != 250*time.Millisecond {
		t.Fatalf("fast sleep = %v", d)
	}
	unblock <- struct{}{}
	if err := <-fastDone; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-bulkDone:
		t.Fatalf("the bulk caller must still be waiting: %v", err)
	default:
	}
	// The bulk caller resumes and sleeps for the full bucket again.
	if d := <-slept; d != 2*time.Second {
		t.Fatalf("resumed bulk sleep = %v", d)
	}
	unblock <- struct{}{}
	if err := <-bulkDone; err != nil || p.tokens != 1 {
		t.Fatalf("bulk: %v tokens %v", err, p.tokens)
	}
	// A bulk caller whose context ends while it sleeps releases the
	// turnstile.
	bctx, cancel := context.WithCancel(ctx)
	go func() { bulkDone <- p.Wait(bctx, 7) }()
	<-slept
	cancel()
	if err := <-bulkDone; !errors.Is(err, context.Canceled) || p.held {
		t.Fatalf("canceled bulk sleep: %v held %v", err, p.held)
	}
	// A bulk caller that yields to a fast caller and is canceled while it
	// waits to come back leaves the queue.
	bctx, cancel = context.WithCancel(ctx)
	go func() { bulkDone <- p.Wait(bctx, 7) }()
	<-slept
	go func() { fastDone <- p.Wait(fastCtx(ctx), 2) }()
	<-slept // the fast caller sleeps for its second token, the bulk one is parked
	cancel()
	if err := <-bulkDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled parked bulk: %v", err)
	}
	unblock <- struct{}{}
	if err := <-fastDone; err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	queued, held := len(p.fast)+len(p.bulk), p.held
	p.mu.Unlock()
	if queued != 0 || held {
		t.Fatalf("queue %d held %v", queued, held)
	}
}

// TestPacerFastLatency: on a 4/s endpoint a bulk caller paying for a
// 100-item reservation, in the chunks an endpoint sends, never delays a
// fast call beyond one token interval, where first come first served
// would have parked it behind a two second sleep.
func TestPacerFastLatency(t *testing.T) {
	p := NewPacer(4)
	ctx := context.Background()
	if err := p.Wait(fastCtx(ctx), 8); err != nil { // drain the bucket
		t.Fatal(err)
	}
	bctx, cancel := context.WithCancel(ctx)
	defer cancel()
	bulkDone := make(chan error, 1)
	go func() {
		for paid := 0; paid < 100; {
			n := min(p.MaxBatch(), 100-paid)
			if err := p.Wait(bctx, n); err != nil {
				bulkDone <- err
				return
			}
			paid += n
		}
		bulkDone <- nil
	}()
	time.Sleep(50 * time.Millisecond) // the bulk caller holds the turnstile, asleep
	start := time.Now()
	if err := p.Wait(fastCtx(ctx), 1); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("fast call waited %v behind a bulk reservation", elapsed)
	}
	cancel()
	if err := <-bulkDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("bulk caller: %v", err)
	}
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
		c := ctx
		if i%4 == 1 {
			c = fastCtx(ctx)
		}
		if err := p.Wait(c, size); err != nil {
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
	if !p.Unlimited() || p.Rate() != 0 || p.Reserve() != 0 || p.Available() < MaxBatch*1000 || NewPacer(-1).Unlimited() != true {
		t.Fatalf("unlimited pacer: rate %v available %d", p.Rate(), p.Available())
	}
	ctx := context.Background()
	for i := range 100 {
		c := ctx
		if i%2 == 0 {
			c = fastCtx(ctx)
		}
		if err := p.Wait(c, MaxBatch); err != nil {
			t.Fatal(err)
		}
	}
	if len(clock.Sleeps()) != 0 || p.Available() < MaxBatch || p.MaxBatch() != MaxBatch {
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
