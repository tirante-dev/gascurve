package nitro

import (
	"context"
	"errors"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fastCtx(ctx context.Context) context.Context { return WithClass(ctx, Fast) }

// third is the sleep for one token above the reserve while the reserve
// refills on a 4/s pacer: three tokens per second, rounded up.
var third = seconds(1.0 / 3)

func TestPacer(t *testing.T) {
	clock := newFakeClock()
	p := NewPacer(4).withClock(clock.Now, clock.Sleep)
	// Burst 8, one token of it the fast reserve: a bulk caller sees 7 and
	// may not batch more than that.
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
	// Four more fast tokens cost one second at 4/s: the reserve's token
	// arrives with three shared ones.
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
	// A rate below one call per second holds nothing above its whole-token
	// reserve, so a bulk batch may carry nothing at all and the lanes
	// time-share the bucket one item at a time instead.
	if small := NewPacer(0.25); small.burst != 1 || small.Reserve() != 1 || small.reserveRate != 0.25 ||
		small.MaxBatch() != 0 || small.MaxBatchFor(Fast) != 1 || !small.timeShared() || NewPacer(1000).MaxBatch() != MaxBatch {
		t.Fatalf("small rates keep a burst of one, an empty bulk share and a reserve refilling at the rate: maxBatch %d fast %d", small.MaxBatch(), small.MaxBatchFor(Fast))
	}
	// A rate below MinCallsPerSecond is raised to it rather than producing
	// waits the duration cannot hold.
	if tiny := NewPacer(1e-12); tiny.Rate() != MinCallsPerSecond || seconds(math.Inf(1)) != maxPacerWait || seconds(math.NaN()) != 0 {
		t.Fatalf("tiny rate %v, saturated waits %v %v", tiny.Rate(), seconds(math.Inf(1)), seconds(math.NaN()))
	}
	// A time-shared bucket: the fast lane takes the token it holds, the
	// bulk lane takes the next one, and neither ever goes into debt.
	slow := NewPacer(0.25).withClock(clock.Now, clock.Sleep)
	before := len(clock.Sleeps())
	if err := slow.Wait(fastCtx(ctx), 1); err != nil || slow.tokens != 0 || len(clock.Sleeps()) != before {
		t.Fatalf("slow fast call: %v tokens %v", err, slow.tokens)
	}
	clock.Advance(4 * time.Second)
	if err := slow.Wait(ctx, 1); err != nil || slow.tokens != 0 || len(clock.Sleeps()) != before {
		t.Fatalf("slow bulk call: %v tokens %v sleeps %v", err, slow.tokens, clock.Sleeps()[before:])
	}
	if err := slow.Wait(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if s := clock.Sleeps()[before:]; len(s) != 1 || s[0] != 4*time.Second || slow.tokens != 0 {
		t.Fatalf("the next bulk token costs a full refill period: sleeps %v tokens %v", s, slow.tokens)
	}
	// A bulk caller never draws the bucket below the reserve: the whole
	// bulk share goes without sleeping and leaves the reserve for a fast
	// call, which the reserve serves at once; the next bulk token then
	// arrives at the rate minus the reserve rate while the reserve
	// refills alongside.
	before = len(clock.Sleeps())
	if err := p.Wait(ctx, 7); err != nil || len(clock.Sleeps()) != before || p.Available() != 0 || p.tokens != 1 || p.fastTokens != 1 {
		t.Fatalf("bulk share: %v sleeps %v available %d tokens %v reserve %v", err, clock.Sleeps()[before:], p.Available(), p.tokens, p.fastTokens)
	}
	if err := p.Wait(fastCtx(ctx), 1); err != nil || len(clock.Sleeps()) != before || p.tokens != 0 || p.fastTokens != 0 {
		t.Fatalf("the reserve serves a fast call at once: %v sleeps %v tokens %v reserve %v", err, clock.Sleeps()[before:], p.tokens, p.fastTokens)
	}
	if err := p.Wait(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if s := clock.Sleeps()[before:]; len(s) != 1 || s[0] != third {
		t.Fatalf("a bulk token waits for itself beside the refilling reserve: %v", s)
	}
	// Tokens are never borrowed: a fast request for the whole burst
	// waits for a full bucket, taking the reserve's token as it arrives.
	clock.Advance(time.Hour)
	if err := p.Wait(fastCtx(ctx), 5); err != nil || p.tokens != 3 || p.fastTokens != 0 {
		t.Fatalf("fast 5: %v tokens %v reserve %v", err, p.tokens, p.fastTokens)
	}
	before = len(clock.Sleeps())
	if err := p.Wait(fastCtx(ctx), 8); err != nil {
		t.Fatal(err)
	}
	if s := clock.Sleeps()[before:]; len(s) != 2 || s[0] != time.Second || s[1] != third {
		t.Fatalf("full-burst wait = %v, want the reserve's token after one second and the last shared token a third later", s)
	}
	if !near(p.tokens, 1.0/3) || !near(p.fastTokens, 1.0/3) {
		t.Fatalf("after the full burst: tokens %v reserve %v", p.tokens, p.fastTokens)
	}
	// A request above what the class can hold at once takes what the bucket
	// holds and waits for the rest: the bucket never goes negative.
	clock.Advance(time.Hour)
	before = len(clock.Sleeps())
	if err := p.Wait(fastCtx(ctx), 10); err != nil || p.tokens < 0 || !near(p.tokens, 2.0/3) {
		t.Fatalf("fast above the burst: %v tokens %v", err, p.tokens)
	}
	if s := clock.Sleeps()[before:]; len(s) != 1 || s[0] != seconds(2.0/3) {
		t.Fatalf("the two tokens above the burst cost half a second at the rate: %v", s)
	}
	// A bulk request above its share does the same.
	clock.Advance(time.Hour)
	before = len(clock.Sleeps())
	if err := p.Wait(ctx, 9); err != nil || p.tokens < 0 || p.tokens != 1 {
		t.Fatalf("bulk above its share: %v tokens %v", err, p.tokens)
	}
	if s := clock.Sleeps()[before:]; len(s) != 1 || s[0] != 500*time.Millisecond {
		t.Fatalf("the two tokens above the bulk share cost half a second: %v", s)
	}
	// Refunding a reservation that never reached the wire puts the tokens
	// back, never above the burst.
	p.Refund(2)
	if p.tokens != 3 {
		t.Fatalf("refund: tokens %v", p.tokens)
	}
	clock.Advance(time.Hour)
	p.Refund(5)
	if p.tokens != p.burst {
		t.Fatalf("a refund never exceeds the burst: %v", p.tokens)
	}
	unlimited := NewPacer(0)
	unlimited.Refund(3)
	p.Refund(0)
	if unlimited.tokens != 0 {
		t.Fatalf("an unlimited pacer holds no tokens: %v", unlimited.tokens)
	}
}

// TestPacerTimeSharedLanes: below one call per second the bulk share is empty, so the two lanes
// alternate whole tokens instead of the bulk lane running a debt. The clock is manual and fires only
// once both lanes are waiting, which is how real time has them at half a call per second; an instant
// fake sleep let one goroutine loop through several tokens first, making the ratio a matter of
// scheduling rather than of the turn rule under test.
func TestPacerTimeSharedLanes(t *testing.T) {
	clock := newManualClock()
	p := NewPacer(0.5).withClock(clock.Now, clock.Sleep)
	if !p.timeShared() || p.MaxBatch() != 0 {
		t.Fatalf("a half call per second leaves no bulk share: timeShared %v maxBatch %d", p.timeShared(), p.MaxBatch())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var fast, bulk atomic.Int64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for p.Wait(fastCtx(ctx), 1) == nil {
			fast.Add(1)
		}
	}()
	go func() {
		defer wg.Done()
		for p.Wait(ctx, 1) == nil {
			bulk.Add(1)
		}
	}()
	// The bucket starts with one token, which the first caller takes without
	// sleeping; every later token is one firing of the clock.
	const rounds = 40
	for i := 1; i <= rounds; i++ {
		waitFor(t, "the last token to be taken", func() bool { return fast.Load()+bulk.Load() == int64(i) })
		waitFor(t, "both lanes waiting, one of them asleep", func() bool {
			f, b := lanesWaiting(p)
			return f == 1 && b == 1 && len(clock.sleeps()) > 0
		})
		clock.fire(t)
	}
	waitFor(t, "the final token to be taken", func() bool { return fast.Load()+bulk.Load() == rounds+1 })
	cancel()
	wg.Wait()
	f, b := fast.Load(), bulk.Load()
	if tokens, _ := p.levels(); tokens < 0 {
		t.Fatalf("time sharing must never overdraw: tokens %v", tokens)
	}
	// With both lanes always waiting, every token goes to the lane that did
	// not take the last one: strict alternation.
	if f-b > 1 || b-f > 1 {
		t.Fatalf("lanes did not alternate: fast %d bulk %d", f, b)
	}
	// One lane alone still gets every token: nothing waits for a turn that
	// will not come.
	fake := newFakeClock()
	q := NewPacer(0.5).withClock(fake.Now, fake.Sleep)
	for range 3 {
		if err := q.Wait(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
	}
	if tokens, _ := q.levels(); tokens < 0 {
		t.Fatalf("bulk alone overdrew: %v", tokens)
	}
	// So does the fast lane on its own.
	r := NewPacer(0.5).withClock(fake.Now, fake.Sleep)
	for range 3 {
		if err := r.Wait(fastCtx(context.Background()), 1); err != nil {
			t.Fatal(err)
		}
	}
	if tokens, _ := r.levels(); tokens < 0 {
		t.Fatalf("fast alone overdrew: %v", tokens)
	}
}

// TestPacerFastBeyondReserveSharesFCFS: a fast tick that wants more than
// the reserve (five calls per tick, back to back, on a 4/s pacer) is
// served the reserve first and shares the rest with bulk work first come,
// first served, so a bulk caller still gets its batches through at
// roughly the rate minus the reserve, where strict priority would have
// starved it. The window budget holds throughout.
func TestPacerFastBeyondReserveSharesFCFS(t *testing.T) {
	clock := newFakeClock()
	p := NewPacer(4).withClock(clock.Now, clock.Sleep)
	ctx := context.Background()
	start := clock.Now()
	var fast, bulk int
	for clock.Now().Sub(start) < 30*time.Second {
		for _, n := range []int{1, 4} { // the tick: eth_blockNumber, then the state batch
			if err := p.Wait(fastCtx(ctx), n); err != nil {
				t.Fatal(err)
			}
			fast += n
		}
		if err := p.Wait(ctx, p.MaxBatch()); err != nil {
			t.Fatal(err)
		}
		bulk += p.MaxBatch()
	}
	elapsed := clock.Now().Sub(start).Seconds()
	if float64(fast+bulk) > 8+4*elapsed {
		t.Fatalf("%d calls in %.2fs exceed the budget", fast+bulk, elapsed)
	}
	bulkRate, fastRate := float64(bulk)/elapsed, float64(fast)/elapsed
	// Every round is one tick (five tokens, one of them the reserve's)
	// and one seven-token batch: about three seconds, so bulk work runs
	// at about 2.3/s against a rate minus reserve of 3/s, and the tick
	// gets more than the reserve alone would give it.
	if bulkRate < 2.2 || bulkRate > 3 || fastRate < 1.5 {
		t.Fatalf("bulk %.2f/s fast %.2f/s over %.2fs: want bulk near the rate minus the reserve and fast above the reserve", bulkRate, fastRate, elapsed)
	}
	t.Logf("bulk %.2f/s, fast %.2f/s over %.2fs", bulkRate, fastRate, elapsed)
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

func (p *Pacer) queued() (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.queue), p.held
}

// TestPacerQueueOrder: a fast caller the reserve covers never queues,
// even while another caller holds the turnstile, and bulk callers take
// tokens in arrival order, so a large batch queued behind the head of the
// line goes out before a small call that arrived after it, instead of
// being starved by it.
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
	// The reserve (250/s) has covered the fast caller before the
	// turnstile opens.
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	served := append([]int(nil), order...)
	mu.Unlock()
	if len(served) != 1 || served[0] != 2 {
		t.Fatalf("served while held = %v, want the fast caller from the reserve", served)
	}
	release()
	wg.Wait()
	if len(order) != 3 || order[0] != 2 || order[1] != 0 || order[2] != 1 {
		t.Fatalf("order = %v, want the fast caller first, then the bulk callers as they arrived", order)
	}
	// Queued callers of either class honor cancellation and leave the
	// queue; a fast caller wanting more than the reserve queues for the
	// rest.
	release = p.hold()
	qctx, qcancel := context.WithCancel(ctx)
	errs := make(chan error, 2)
	go func() { errs <- p.Wait(qctx, 1) }()
	go func() { errs <- p.Wait(WithClass(qctx, Fast), 2000) }()
	waitFor(t, "two queued callers", func() bool { n, _ := p.queued(); return n == 2 })
	qcancel()
	for range 2 {
		if err := <-errs; !errors.Is(err, context.Canceled) {
			t.Fatalf("a queued caller honors cancellation: %v", err)
		}
	}
	if n, _ := p.queued(); n != 0 {
		t.Fatalf("%d callers left in the queue", n)
	}
	release()
	// A caller canceled just as it was handed the turnstile passes it on.
	p.hold()
	if err := p.park(qctx, &waiter{ready: make(chan struct{})}); !errors.Is(err, context.Canceled) || p.held {
		t.Fatalf("park after a handoff: %v held %v", err, p.held)
	}
}

// near compares token levels, allowing for the nanosecond every sleep
// is rounded up by.
func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// levels returns the bucket and reserve levels under the lock.
func (p *Pacer) levels() (tokens, reserve float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tokens, p.fastTokens
}

// waitForTimeout is generous on purpose: the condition is normally true
// within microseconds, and the wait only runs out when the code under test
// is genuinely stuck. A tight budget instead made the lane test fail on a
// loaded machine, where the whole package runs beside every other one.
const waitForTimeout = 60 * time.Second

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitForTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// manualClock is a fake clock whose sleeps block until the test fires
// them, so concurrent callers can be driven one wake-up at a time.
type manualClock struct {
	mu      sync.Mutex
	now     time.Time
	pending []*manualSleep
}

type manualSleep struct {
	d     time.Duration
	until time.Time
	fired chan struct{}
}

func newManualClock() *manualClock {
	return &manualClock{now: time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	s := &manualSleep{d: d, until: c.now.Add(d), fired: make(chan struct{})}
	c.pending = append(c.pending, s)
	c.mu.Unlock()
	select {
	case <-s.fired:
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		c.remove(s)
		c.mu.Unlock()
		return ctx.Err()
	}
}

func (c *manualClock) remove(s *manualSleep) {
	for i, x := range c.pending {
		if x == s {
			c.pending = append(c.pending[:i], c.pending[i+1:]...)
			return
		}
	}
}

// sleeps returns the pending sleep durations, shortest first.
func (c *manualClock) sleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Duration, 0, len(c.pending))
	for _, s := range c.pending {
		out = append(out, s.d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// expect waits until exactly these sleeps are pending, shortest first.
func (c *manualClock) expect(t *testing.T, want ...time.Duration) {
	t.Helper()
	var got []time.Duration
	deadline := time.Now().Add(waitForTimeout)
	for {
		got = c.sleeps()
		if len(got) == len(want) {
			same := true
			for i := range got {
				// Within the nanosecond a sleep is rounded up by.
				same = same && got[i]-want[i] <= time.Nanosecond && want[i]-got[i] <= time.Nanosecond
			}
			if same {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending sleeps = %v, want %v", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// fire advances the clock to the earliest pending sleep and wakes every
// sleeper due by then.
func (c *manualClock) fire(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) == 0 {
		t.Fatal("nothing to fire")
	}
	earliest := c.pending[0].until
	for _, s := range c.pending[1:] {
		if s.until.Before(earliest) {
			earliest = s.until
		}
	}
	c.now = earliest
	kept := c.pending[:0]
	for _, s := range c.pending {
		if s.until.After(earliest) {
			kept = append(kept, s)
			continue
		}
		close(s.fired)
	}
	c.pending = kept
}

// TestPacerQueuedFastDrawsReserve: a fast caller queued behind a sleeping
// bulk caller keeps drawing the reserve as it refills, then takes its turn
// for the rest first come, first served; the bulk caller's own sleep is
// never cut short by it. Every wake-up is driven by hand on a 4/s pacer
// (reserve one token per second).
func TestPacerQueuedFastDrawsReserve(t *testing.T) {
	clock := newManualClock()
	p := NewPacer(4).withClock(clock.Now, clock.Sleep)
	ctx := context.Background()
	if err := p.Wait(fastCtx(ctx), 8); err != nil { // drain bucket and reserve
		t.Fatal(err)
	}
	bulkDone := make(chan error, 1)
	go func() { bulkDone <- p.Wait(ctx, 5) }()
	// Five shared tokens: three arrive in the first second beside the
	// refilling reserve, two more at the full rate after it.
	clock.expect(t, 1500*time.Millisecond)
	fastDone := make(chan error, 1)
	go func() { fastDone <- p.Wait(fastCtx(ctx), 3) }()
	// The fast caller queues and waits for the reserve's next token.
	clock.expect(t, time.Second, 1500*time.Millisecond)
	clock.fire(t) // t = 1 s: the reserve token goes to the queued fast caller
	clock.expect(t, time.Second, 1500*time.Millisecond)
	if tokens, reserve := p.levels(); tokens != 3 || reserve != 0 {
		t.Fatalf("after the reserve draw: tokens %v reserve %v", tokens, reserve)
	}
	clock.fire(t) // t = 1.5 s: the bulk caller is half a token short, the draw kept the reserve refilling
	clock.expect(t, seconds(0.5/3), time.Second)
	clock.fire(t) // t = 1.67 s: the bulk caller is served and hands the turnstile on
	if err := <-bulkDone; err != nil {
		t.Fatal(err)
	}
	// The fast caller now holds the turnstile with two tokens to go and
	// sleeps for whichever comes first: the reserve's next token.
	clock.expect(t, third)
	select {
	case err := <-fastDone:
		t.Fatalf("the fast caller must still be waiting: %v", err)
	default:
	}
	clock.fire(t) // t = 2 s: one from the reserve, one shared
	if err := <-fastDone; err != nil {
		t.Fatal(err)
	}
	if tokens, reserve := p.levels(); !near(tokens, 0) || !near(reserve, 0) {
		t.Fatalf("eight tokens in two seconds: tokens %v reserve %v", tokens, reserve)
	}
	if n, held := p.queued(); n != 0 || held {
		t.Fatalf("queue %d held %v", n, held)
	}
	// A fast caller canceled while it waits on the reserve leaves the
	// queue; a bulk caller canceled while it sleeps releases the
	// turnstile.
	bctx, bcancel := context.WithCancel(ctx)
	go func() { bulkDone <- p.Wait(bctx, 7) }()
	clock.expect(t, 2*time.Second)
	fctx, fcancel := context.WithCancel(ctx)
	go func() { fastDone <- p.Wait(fastCtx(fctx), 2) }()
	clock.expect(t, time.Second, 2*time.Second)
	fcancel()
	if err := <-fastDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled queued fast caller: %v", err)
	}
	clock.expect(t, 2*time.Second)
	if n, _ := p.queued(); n != 0 {
		t.Fatalf("%d callers left in the queue", n)
	}
	bcancel()
	if err := <-bulkDone; !errors.Is(err, context.Canceled) || p.held {
		t.Fatalf("canceled bulk sleep: %v held %v", err, p.held)
	}
	// A fast caller holding the turnstile and canceled while it sleeps
	// releases it too.
	fctx, fcancel = context.WithCancel(ctx)
	go func() { fastDone <- p.Wait(fastCtx(fctx), 3) }()
	clock.expect(t, time.Second)
	fcancel()
	if err := <-fastDone; !errors.Is(err, context.Canceled) || p.held {
		t.Fatalf("canceled fast sleep: %v held %v", err, p.held)
	}
	// A caller that leaves the queue just as it was handed the turnstile
	// passes it on.
	p.hold()
	p.abandon(&waiter{ready: make(chan struct{})})
	if p.held {
		t.Fatal("abandoning after a handoff must open the turnstile")
	}
}

// TestPacerFastLatency: on a 4/s endpoint a bulk caller paying for a
// 100-item reservation, in the chunks an endpoint sends, never delays a
// fast call: the first is served from the reserve at once, the next one
// within the reserve's refill, where first come first served alone would
// have parked it behind a two second sleep.
func TestPacerFastLatency(t *testing.T) {
	p := NewPacer(4)
	ctx := context.Background()
	if err := p.Wait(ctx, 7); err != nil { // drain the bulk share, the reserve stays
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
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("fast call waited %v for the reserve it should have found", elapsed)
	}
	start = time.Now()
	if err := p.Wait(fastCtx(ctx), 1); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("second fast call waited %v, want the reserve's refill", elapsed)
	}
	cancel()
	if err := <-bulkDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("bulk caller: %v", err)
	}
}

// TestPacerBulkProgressUnderFastLoad: with a fast caller asking for
// tokens back to back, far above the reserve, bulk batches still go
// through on a real clock, and the two together never exceed the budget.
func TestPacerBulkProgressUnderFastLoad(t *testing.T) {
	const rate = 20
	p := NewPacer(rate)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := p.Wait(fastCtx(ctx), 2*rate); err != nil { // drain bucket and reserve
		t.Fatal(err)
	}
	start := time.Now()
	var fast, bulk atomic.Int64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for p.Wait(fastCtx(ctx), 1) == nil {
			fast.Add(1)
		}
	}()
	go func() {
		defer wg.Done()
		for p.Wait(ctx, 5) == nil {
			bulk.Add(5)
		}
	}()
	time.Sleep(time.Second)
	cancel()
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	f, b := fast.Load(), bulk.Load()
	if float64(f+b) > 2*rate+rate*elapsed+1 {
		t.Fatalf("%d fast + %d bulk in %.2fs exceed the budget", f, b, elapsed)
	}
	// The reserve alone gives the fast caller five a second; a bulk batch
	// of five needs a third of a second of shared tokens.
	if b < 5 || f < 4 {
		t.Fatalf("fast %d bulk %d in %.2fs: bulk work must progress under sustained fast demand", f, b, elapsed)
	}
	t.Logf("fast %d, bulk %d in %.2fs", f, b, elapsed)
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

// lanesWaiting reports how many Fast and Bulk callers are inside Wait.
func lanesWaiting(p *Pacer) (fast, bulk int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fastPending, p.bulkPending
}
