package nitro

import (
	"context"
	"math"
	"sync"
	"time"
)

// Pacer is a token bucket that meters JSON-RPC calls per endpoint: one
// token per call, including every item inside a batch, refilled at the
// configured rate with a burst of twice the rate. Wait never spends tokens
// the bucket does not hold, so over any window of w seconds at most
// burst + rate*w calls pass, whatever the mix of callers; batches are
// therefore capped at the burst (MaxBatch). Callers wait in arrival order,
// so a large batch is never starved by a stream of small calls. A rate of
// zero means unlimited: Wait never sleeps and Available is effectively
// infinite. 429 back-off lives in the Client and applies either way.
type Pacer struct {
	mu        sync.Mutex
	unlimited bool
	rate      float64
	burst     float64
	tokens    float64
	last      time.Time
	now       func() time.Time
	sleep     func(context.Context, time.Duration) error
	// turn is a FIFO turnstile: the caller holding it is the only one
	// waiting on tokens, the others queue behind it in arrival order.
	turn chan struct{}
}

// NewPacer creates a bucket allowing callsPerSecond sustained calls, or an
// unlimited pacer when callsPerSecond is zero or negative.
func NewPacer(callsPerSecond float64) *Pacer {
	p := &Pacer{now: time.Now, sleep: sleepContext, turn: make(chan struct{}, 1)}
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

// MaxBatch returns the most items one request may carry without spending
// tokens the bucket cannot hold: the burst on a budgeted endpoint, the
// protocol cap on an unlimited one.
func (p *Pacer) MaxBatch() int {
	if p.unlimited {
		return MaxBatch
	}
	return max(min(int(p.burst), MaxBatch), 1)
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

// Available returns the number of whole tokens that can be taken without
// waiting (math.MaxInt when unlimited).
func (p *Pacer) Available() int {
	if p.unlimited {
		return math.MaxInt
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refillLocked()
	if p.tokens < 0 {
		return 0
	}
	return int(p.tokens)
}

// Wait takes n tokens, sleeping until the bucket holds them; it never
// borrows against future refills, which is what keeps a windowed budget.
// n above the burst can never be held at once (callers cap batches at
// MaxBatch): such a request waits for a full bucket and overdraws by the
// excess alone.
func (p *Pacer) Wait(ctx context.Context, n int) error {
	if n <= 0 {
		return nil
	}
	if err := ctx.Err(); err != nil || p.unlimited {
		return err
	}
	select {
	case p.turn <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-p.turn }()
	need := min(float64(n), p.burst)
	for {
		p.mu.Lock()
		p.refillLocked()
		if p.tokens >= need {
			p.tokens -= float64(n)
			p.mu.Unlock()
			return nil
		}
		short := need - p.tokens
		p.mu.Unlock()
		if err := p.sleep(ctx, time.Duration(short/p.rate*float64(time.Second))); err != nil {
			return err
		}
	}
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
