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
// critical: it is served before any queued Bulk call and always finds the
// fast reserve in the bucket.
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

// Pacer is a token bucket that meters JSON-RPC calls per endpoint: one
// token per call, including every item inside a batch, refilled at the
// configured rate with a burst of twice the rate. Wait never spends tokens
// the bucket does not hold, so over any window of w seconds at most
// burst + rate*w calls pass, whatever the mix of callers.
//
// Two classes of caller share the bucket. A share of it, the fast reserve
// (max(1, rate/4) tokens), is kept for Fast callers: a Bulk wait only
// draws the bucket down to the reserve, so a Bulk batch is capped at
// burst minus reserve (MaxBatch) and a Fast call always finds at least the
// reserve without waiting behind bulk work. Callers queue at a turnstile,
// Fast ahead of Bulk and in arrival order within a class, so a large Bulk
// batch is never starved by a stream of small Bulk calls, and a Bulk
// caller sleeping for tokens steps aside the moment a Fast caller arrives.
// The priority is strict, so Fast callers that together want more than
// the whole rate starve bulk work: the fast tick's calls per second must
// fit inside the endpoint's budget. A rate of zero means unlimited: Wait
// never sleeps and Available is effectively infinite. 429 back-off lives
// in the Client and applies either way.
type Pacer struct {
	mu        sync.Mutex
	unlimited bool
	rate      float64
	burst     float64
	reserve   float64
	tokens    float64
	last      time.Time
	now       func() time.Time
	sleep     func(context.Context, time.Duration) error
	// The turnstile. held is true while a caller waits on tokens; the
	// others queue behind it by class. wake, when set, interrupts the
	// sleep of a Bulk holder so a Fast arrival is served first.
	held bool
	fast []*waiter
	bulk []*waiter
	wake chan struct{}
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

// Reserve returns the tokens kept for Fast callers, 0 when unlimited.
func (p *Pacer) Reserve() float64 { return p.reserve }

// MaxBatch returns the most items one Bulk request may carry without
// spending tokens the bucket cannot hold for it: the burst minus the fast
// reserve on a budgeted endpoint, the protocol cap on an unlimited one.
func (p *Pacer) MaxBatch() int {
	if p.unlimited {
		return MaxBatch
	}
	return max(min(int(p.burst-p.reserve), MaxBatch), 1)
}

func (p *Pacer) refillLocked() {
	t := p.now()
	elapsed := t.Sub(p.last).Seconds()
	if elapsed > 0 {
		p.tokens += elapsed * p.rate
		if p.tokens > p.burst {
			p.tokens = p.burst
		}
	}
	p.last = t
}

// Available returns the number of whole tokens a Bulk caller can take
// without waiting, that is the bucket above the fast reserve
// (math.MaxInt when unlimited).
func (p *Pacer) Available() int {
	if p.unlimited {
		return math.MaxInt
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refillLocked()
	if p.tokens < p.reserve {
		return 0
	}
	return int(p.tokens - p.reserve)
}

// floor returns how many tokens a caller of class must leave in the
// bucket.
func (p *Pacer) floor(class Class) float64 {
	if class == Fast {
		return 0
	}
	return p.reserve
}

// Wait takes n tokens for the class ctx carries (Bulk by default),
// sleeping until the bucket holds them above the class's floor; it never
// borrows against future refills, which is what keeps a windowed budget.
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
	if err := p.acquire(ctx, class); err != nil {
		return err
	}
	floor := p.floor(class)
	need := min(float64(n), p.burst-floor)
	for {
		p.mu.Lock()
		p.refillLocked()
		if p.tokens-floor >= need {
			p.tokens -= float64(n)
			p.releaseLocked()
			p.mu.Unlock()
			return nil
		}
		short := need + floor - p.tokens
		var wake chan struct{}
		if class == Bulk {
			if len(p.fast) > 0 {
				// A Fast caller queued before this turn: step aside now.
				w := p.stepAsideLocked()
				p.mu.Unlock()
				if err := p.park(ctx, w); err != nil {
					return err
				}
				continue
			}
			wake = make(chan struct{})
			p.wake = wake
		}
		p.mu.Unlock()
		// Rounded up so the sleep always covers the shortfall: truncation
		// could leave the bucket a rounding error short forever.
		err := p.pause(ctx, time.Duration(math.Ceil(short/p.rate*float64(time.Second))), wake)
		p.mu.Lock()
		p.wake = nil
		if err != nil {
			p.releaseLocked()
			p.mu.Unlock()
			return err
		}
		p.mu.Unlock()
	}
}

// acquire takes the turnstile, queueing by class when another caller
// holds it. A Fast arrival interrupts the sleep of a Bulk holder.
func (p *Pacer) acquire(ctx context.Context, class Class) error {
	p.mu.Lock()
	if !p.held {
		p.held = true
		p.mu.Unlock()
		return nil
	}
	w := &waiter{ready: make(chan struct{})}
	if class == Fast {
		p.fast = append(p.fast, w)
		if p.wake != nil {
			close(p.wake)
			p.wake = nil
		}
	} else {
		p.bulk = append(p.bulk, w)
	}
	p.mu.Unlock()
	return p.park(ctx, w)
}

// park waits until w is handed the turnstile. On cancellation w leaves
// the queue, or passes the turnstile on when it was handed it meanwhile.
func (p *Pacer) park(ctx context.Context, w *waiter) error {
	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
		p.mu.Lock()
		if !p.dequeue(w) {
			p.releaseLocked()
		}
		p.mu.Unlock()
		return ctx.Err()
	}
}

// dequeue removes w from whichever queue holds it and reports whether it
// was still queued.
func (p *Pacer) dequeue(w *waiter) bool {
	for _, q := range []*[]*waiter{&p.fast, &p.bulk} {
		for i, x := range *q {
			if x == w {
				*q = append((*q)[:i], (*q)[i+1:]...)
				return true
			}
		}
	}
	return false
}

// releaseLocked hands the turnstile to the first queued caller, Fast
// before Bulk, or opens it when nobody waits.
func (p *Pacer) releaseLocked() {
	var w *waiter
	switch {
	case len(p.fast) > 0:
		w, p.fast = p.fast[0], p.fast[1:]
	case len(p.bulk) > 0:
		w, p.bulk = p.bulk[0], p.bulk[1:]
	default:
		p.held = false
		return
	}
	close(w.ready)
}

// stepAsideLocked lets a Bulk holder yield to the queued Fast callers: it
// hands the turnstile over and returns the waiter parked at the head of
// the Bulk queue that brings it back.
func (p *Pacer) stepAsideLocked() *waiter {
	w := &waiter{ready: make(chan struct{})}
	p.bulk = append([]*waiter{w}, p.bulk...)
	p.releaseLocked()
	return w
}

// pause sleeps for d, or until wake is closed (a Fast arrival), or until
// ctx ends. Only a cancellation of ctx is an error.
func (p *Pacer) pause(ctx context.Context, d time.Duration, wake <-chan struct{}) error {
	if wake == nil {
		return p.sleep(ctx, d)
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-wake:
			cancel()
		case <-done:
		}
	}()
	if err := p.sleep(sctx, d); err != nil {
		return ctx.Err()
	}
	return nil
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
