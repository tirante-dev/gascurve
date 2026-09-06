package nitro

import (
	"context"
	"math"
	"sync"
	"time"
)

// Class is the priority of a call at the pacer. Bulk is the default: the
// catch-up header batches, log chunks, batch-report scans and the
// backfill. Fast is the head-triggered state sample, which is latency
// critical: it has first claim on the fast reserve, a small share of the
// budget kept aside for it, and shares the rest first come, first served
// with Bulk.
type Class uint8

const (
	// Bulk is the default class: everything that is not the fast tick.
	Bulk Class = iota
	// Fast is the fast tick's state sample and its single small calls.
	Fast
)

// String names the class for logs.
func (c Class) String() string {
	if c == Fast {
		return "fast"
	}
	return "bulk"
}

type classKey struct{}

// WithClass marks every call made with ctx as class.
func WithClass(ctx context.Context, class Class) context.Context {
	return context.WithValue(ctx, classKey{}, class)
}

// ClassOf returns the class ctx carries, Bulk when none.
func ClassOf(ctx context.Context) Class {
	if c, ok := ctx.Value(classKey{}).(Class); ok {
		return c
	}
	return Bulk
}

// tokenEpsilon absorbs floating point error in token arithmetic: a
// caller a billionth of a token short is not made to sleep for it.
const tokenEpsilon = 1e-9

// Pacer is a token bucket that meters JSON-RPC calls per endpoint: one
// token per call, including every item inside a batch, refilled at the
// configured rate with a burst of twice the rate. Wait never spends tokens
// the bucket does not hold, so over any window of w seconds at most
// burst + rate*w calls pass, whatever the mix of callers.
//
// Two classes of caller share the bucket. Part of it is the fast reserve:
// its own small bucket of max(1, rate/4) tokens, refilled at that many
// tokens per second (never more than the rate itself), which only Fast
// callers draw from and which they draw from at once, without queueing,
// whenever it holds a whole token. Bulk callers only ever see the bucket
// above the reserve, so a Fast call under bulk load is served as soon as
// the reserve holds a token, and a Bulk batch is capped at the burst
// minus the reserve (MaxBatch). What a Fast caller wants beyond the
// reserve it queues for first come, first served with the Bulk callers:
// callers wait their turn at a turnstile and take tokens in arrival
// order, so a large Bulk batch is never starved by a stream of small
// calls, and a fast tick that wants more than the reserve slows the bulk
// work down instead of stopping it. A queued Fast caller keeps drawing
// the reserve as it refills while it waits its turn. A rate of zero means
// unlimited: Wait never sleeps and Available is effectively infinite. 429
// back-off lives in the Client and applies either way.
type Pacer struct {
	mu        sync.Mutex
	unlimited bool
	rate      float64
	burst     float64
	// reserve is the fast reserve's size in tokens and reserveRate its
	// refill rate; fastTokens is its level, never above tokens.
	reserve     float64
	reserveRate float64
	tokens      float64
	fastTokens  float64
	last        time.Time
	now         func() time.Time
	sleep       func(context.Context, time.Duration) error
	// The turnstile. held is true while a caller waits on tokens; the
	// others queue behind it in arrival order.
	held  bool
	queue []*waiter
}

// waiter is a queued caller: ready is closed when it is handed the
// turnstile.
type waiter struct {
	ready chan struct{}
}

// NewPacer creates a bucket allowing callsPerSecond sustained calls, or an
// unlimited pacer when callsPerSecond is zero or negative.
func NewPacer(callsPerSecond float64) *Pacer {
	p := &Pacer{now: time.Now, sleep: sleepContext}
	if callsPerSecond <= 0 {
		p.unlimited = true
		p.last = p.now()
		return p
	}
	burst := 2 * callsPerSecond
	if burst < 1 {
		burst = 1
	}
	p.rate, p.burst, p.tokens = callsPerSecond, burst, burst
	p.reserve = max(1, callsPerSecond/4)
	p.reserveRate = min(p.reserve, callsPerSecond)
	p.fastTokens = p.reserve
	p.last = p.now()
	return p
}

// Unlimited reports whether the pacer never waits.
func (p *Pacer) Unlimited() bool { return p.unlimited }

// withClock replaces the clock and sleeper, for tests.
func (p *Pacer) withClock(now func() time.Time, sleep func(context.Context, time.Duration) error) *Pacer {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.now = now
	p.sleep = sleep
	p.last = now()
	return p
}

// Rate returns the sustained calls per second, 0 when unlimited.
func (p *Pacer) Rate() float64 { return p.rate }

// Reserve returns the size of the fast reserve in tokens, max(1, rate/4),
// 0 when unlimited. The reserve refills at that many tokens per second
// (at the rate itself when the rate is lower), so it is one second's worth
// of fast calls.
func (p *Pacer) Reserve() float64 { return p.reserve }

// MaxBatch returns the most items one Bulk request may carry without
// spending tokens the bucket cannot hold for it: the burst minus the fast
// reserve on a budgeted endpoint, the protocol cap on an unlimited one. A
// Fast request may carry the reserve more than that.
func (p *Pacer) MaxBatch() int {
	if p.unlimited {
		return MaxBatch
	}
	return max(min(int(p.sharedCap()), MaxBatch), 1)
}

// sharedCap is the most tokens the bucket holds above a full reserve.
func (p *Pacer) sharedCap() float64 { return p.burst - p.reserve }

// refillLocked credits the time since the last refill to the bucket and
// to the reserve. The reserve never rises above the bucket: an overdrawn
// bucket pays itself back before the reserve accrues again.
func (p *Pacer) refillLocked() {
	t := p.now()
	elapsed := t.Sub(p.last).Seconds()
	if elapsed > 0 {
		accrue := elapsed
		if p.tokens < 0 {
			accrue = max(elapsed+p.tokens/p.rate, 0)
		}
		p.tokens = min(p.tokens+elapsed*p.rate, p.burst)
		p.fastTokens = min(p.fastTokens+accrue*p.reserveRate, p.reserve)
	}
	p.fastTokens = max(min(p.fastTokens, p.tokens), 0)
	p.last = t
}

// sharedLocked is what the bucket holds above the reserve's current level:
// the tokens open to any caller.
func (p *Pacer) sharedLocked() float64 { return p.tokens - p.fastTokens }

// Available returns the number of whole tokens a Bulk caller can take
// without waiting, that is the bucket above the fast reserve's current
// level (math.MaxInt when unlimited). A reserve the fast tick has drawn
// down makes that many more tokens available to bulk work until it
// refills.
func (p *Pacer) Available() int {
	if p.unlimited {
		return math.MaxInt
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refillLocked()
	return max(int(math.Floor(p.sharedLocked()+tokenEpsilon)), 0)
}

// drawReserveLocked takes whole reserve tokens for a Fast caller, as many
// as it still needs, and returns how many it still needs after that.
func (p *Pacer) drawReserveLocked(remaining int) int {
	k := min(remaining, int(math.Floor(p.fastTokens+tokenEpsilon)))
	if k <= 0 {
		return remaining
	}
	p.tokens -= float64(k)
	p.fastTokens = max(p.fastTokens-float64(k), 0)
	return remaining - k
}

// Wait takes n tokens for the class ctx carries (Bulk by default). A Fast
// caller first draws what the reserve holds, at once; for the rest, and
// for every Bulk token, it waits its turn at the turnstile and sleeps
// until the bucket holds the tokens above the reserve. Nothing is ever
// borrowed against future refills, which is what keeps a windowed budget.
// n above what the class may hold at once can never be held (callers cap
// batches at MaxBatch): such a request waits for a full bucket and
// overdraws by the excess alone.
func (p *Pacer) Wait(ctx context.Context, n int) error {
	if n <= 0 {
		return nil
	}
	if err := ctx.Err(); err != nil || p.unlimited {
		return err
	}
	class := ClassOf(ctx)
	remaining := n
	if class == Fast {
		p.mu.Lock()
		p.refillLocked()
		remaining = p.drawReserveLocked(remaining)
		p.mu.Unlock()
		if remaining == 0 {
			return nil
		}
	}
	remaining, err := p.acquire(ctx, class, remaining)
	if err != nil || remaining == 0 {
		return err
	}
	for {
		p.mu.Lock()
		p.refillLocked()
		if class == Fast {
			remaining = p.drawReserveLocked(remaining)
		}
		need := min(float64(remaining), p.sharedCap())
		if remaining == 0 || p.sharedLocked()+tokenEpsilon >= need {
			p.tokens -= float64(remaining)
			p.releaseLocked()
			p.mu.Unlock()
			return nil
		}
		d := p.sharedWaitLocked(need)
		if class == Fast {
			d = min(d, p.reserveWaitLocked())
		}
		p.mu.Unlock()
		if err := p.sleep(ctx, d); err != nil {
			p.mu.Lock()
			p.releaseLocked()
			p.mu.Unlock()
			return err
		}
	}
}

// sharedWaitLocked returns how long until the bucket holds need tokens
// above the reserve, with nobody spending meanwhile: an overdrawn bucket
// pays itself back first, then the reserve refills alongside (the shared
// part grows at the rate minus the reserve rate), then the full rate goes
// to the shared part. The sleep is rounded up so it always covers the
// shortfall; a Fast draw meanwhile keeps the reserve from filling, which
// only makes the sleeper wake early and sleep again.
func (p *Pacer) sharedWaitLocked(need float64) time.Duration {
	tokens, fast := p.tokens, p.fastTokens
	var t float64
	if tokens < 0 {
		t = -tokens / p.rate
		tokens, fast = 0, 0
	}
	shared := tokens - fast
	if shared >= need {
		return seconds(t)
	}
	if fast < p.reserve {
		growth := p.rate - p.reserveRate
		fill := (p.reserve - fast) / p.reserveRate
		if growth > 0 && shared+growth*fill >= need {
			return seconds(t + (need-shared)/growth)
		}
		t += fill
		shared += growth * fill
	}
	return seconds(t + (need-shared)/p.rate)
}

// reserveWaitLocked returns how long until the reserve holds a whole
// token, with nobody spending meanwhile; the caller has just drawn every
// whole token it held.
func (p *Pacer) reserveWaitLocked() time.Duration {
	tokens, fast := p.tokens, p.fastTokens
	var t float64
	if tokens < 0 {
		t = -tokens / p.rate
		fast = 0
	}
	return seconds(t + (1-fast)/p.reserveRate)
}

// seconds converts a wait in seconds to a duration, rounded up so that
// truncation can never leave the bucket a rounding error short.
func seconds(s float64) time.Duration {
	return time.Duration(math.Ceil(s * float64(time.Second)))
}

// acquire takes the turnstile, queueing in arrival order when another
// caller holds it. A queued Fast caller keeps drawing the reserve as it
// refills and leaves the queue once that has covered it. It returns the
// tokens the caller still needs; zero means it is served and holds
// nothing.
func (p *Pacer) acquire(ctx context.Context, class Class, remaining int) (int, error) {
	p.mu.Lock()
	if !p.held {
		p.held = true
		p.mu.Unlock()
		return remaining, nil
	}
	w := &waiter{ready: make(chan struct{})}
	p.queue = append(p.queue, w)
	p.mu.Unlock()
	if class == Fast {
		return p.parkFast(ctx, w, remaining)
	}
	return remaining, p.park(ctx, w)
}

// park waits until w is handed the turnstile. On cancellation w leaves
// the queue, or passes the turnstile on when it was handed it meanwhile.
func (p *Pacer) park(ctx context.Context, w *waiter) error {
	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
		p.leave(w)
		return ctx.Err()
	}
}

// parkFast is park for a Fast caller: while it waits its turn it wakes
// whenever the reserve holds a whole token and draws it, so bulk work
// ahead of it never delays the reserve's share. Once the reserve has
// covered it, it leaves the queue and reports zero remaining.
func (p *Pacer) parkFast(ctx context.Context, w *waiter, remaining int) (int, error) {
	for {
		p.mu.Lock()
		p.refillLocked()
		remaining = p.drawReserveLocked(remaining)
		if remaining == 0 {
			p.leaveLocked(w)
			p.mu.Unlock()
			return 0, nil
		}
		d := p.reserveWaitLocked()
		p.mu.Unlock()
		refilled, stop := p.after(ctx, d)
		select {
		case <-w.ready:
			stop()
			return remaining, nil
		case <-ctx.Done():
			stop()
			p.leave(w)
			return remaining, ctx.Err()
		case <-refilled:
			stop()
		}
	}
}

// after runs the sleeper for d in the background: done is closed when it
// ends and stop ends it early.
func (p *Pacer) after(ctx context.Context, d time.Duration) (done <-chan struct{}, stop func()) {
	sctx, cancel := context.WithCancel(ctx)
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		_ = p.sleep(sctx, d)
	}()
	return ended, cancel
}

// leave takes w out of the queue, or passes the turnstile on when w was
// handed it meanwhile.
func (p *Pacer) leave(w *waiter) {
	p.mu.Lock()
	p.leaveLocked(w)
	p.mu.Unlock()
}

func (p *Pacer) leaveLocked(w *waiter) {
	if !p.dequeueLocked(w) {
		p.releaseLocked()
	}
}

// dequeueLocked removes w from the queue and reports whether it was
// still queued.
func (p *Pacer) dequeueLocked(w *waiter) bool {
	for i, x := range p.queue {
		if x == w {
			p.queue = append(p.queue[:i], p.queue[i+1:]...)
			return true
		}
	}
	return false
}

// releaseLocked hands the turnstile to the first queued caller, or opens
// it when nobody waits.
func (p *Pacer) releaseLocked() {
	if len(p.queue) == 0 {
		p.held = false
		return
	}
	w := p.queue[0]
	p.queue = p.queue[1:]
	close(w.ready)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
