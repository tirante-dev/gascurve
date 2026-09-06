package nitro

import (
	"context"
	"math"
	"sync"
	"time"
)

// Pacer is a token bucket that meters JSON-RPC calls per network: one token
// per call, including every item inside a batch, refilled at the configured
// rate with a burst of twice the rate. A rate of zero means unlimited: Wait
// never sleeps and Available is effectively infinite. Batch size caps and
// 429 back-off live in the Client and apply either way.
type Pacer struct {
	mu        sync.Mutex
	unlimited bool
	rate      float64
	burst     float64
	tokens    float64
	last      time.Time
	now       func() time.Time
	sleep     func(context.Context, time.Duration) error
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

// Wait takes n tokens, sleeping until the bucket can afford them. n larger
// than the burst is allowed and simply waits longer.
func (p *Pacer) Wait(ctx context.Context, n int) error {
	if n <= 0 {
		return nil
	}
	if p.unlimited {
		return ctx.Err()
	}
	p.mu.Lock()
	p.refillLocked()
	p.tokens -= float64(n)
	deficit := -p.tokens
	p.mu.Unlock()
	if deficit <= 0 {
		return nil
	}
	return p.sleep(ctx, time.Duration(deficit/p.rate*float64(time.Second)))
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
