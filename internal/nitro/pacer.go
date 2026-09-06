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

// MinCallsPerSecond is the smallest budget the pacer works with. Below one
// call every ten seconds a single token takes longer than any request
// timeout and the wait arithmetic stops being meaningful, so
// config.Validate rejects such a rate and NewPacer raises anything smaller
// to it.
const MinCallsPerSecond = 0.1

// maxPacerWait bounds one sleep, so a wait computed from a tiny rate can
// never overflow time.Duration and no caller is parked for ever: it wakes,
// re-checks the bucket and sleeps again.
const maxPacerWait = time.Minute

// Pacer is a token bucket that meters JSON-RPC calls per endpoint: one
// token per call, including every item inside a batch, refilled at the
// configured rate with a burst of twice the rate. Wait never spends tokens
// the requesting class does not hold, so the bucket never goes negative
// and over any window of w seconds at most burst + rate*w calls pass,
// whatever the mix of callers.
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
// the reserve as it refills while it waits its turn.
//
// Below one call per second a whole-token reserve is the whole burst and
// the bulk share is empty (MaxBatch is 0). The two lanes then time-share
// the bucket instead: they alternate whole tokens, so neither starves and
// neither borrows against future refills. A rate of zero means unlimited:
// Wait never sleeps and Available is effectively infinite. 429 back-off
// lives in the Client and applies either way.
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
	// Time-sharing bookkeeping: how many callers of each lane are waiting
	// and which lane took the last whole token, plus a channel closed
	// whenever that changes so the other lane wakes.
	bulkPending int
	fastPending int
	lastFast    bool
	turn        chan struct{}
}

// waiter is a queued caller: ready is closed when it is handed the
// turnstile.
type waiter struct {
	ready chan struct{}
}

// NewPacer creates a bucket allowing callsPerSecond sustained calls, or an
// unlimited pacer when callsPerSecond is zero or negative. A positive rate
// below MinCallsPerSecond is raised to it.
func NewPacer(callsPerSecond float64) *Pacer {
	p := &Pacer{now: time.Now, sleep: sleepContext}
	if callsPerSecond <= 0 {
		p.unlimited = true
		p.last = p.now()
		return p
	}
	callsPerSecond = max(callsPerSecond, MinCallsPerSecond)
	p.rate = callsPerSecond
	p.burst = max(2*callsPerSecond, 1)
	p.tokens = p.burst
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

// sharedCap is the most tokens the bucket holds above a full reserve.
func (p *Pacer) sharedCap() float64 { return p.burst - p.reserve }

// timeShared reports whether a whole-token reserve leaves the bulk lane no
// capacity of its own, which is every rate below one call per second. The
// two lanes then alternate whole tokens over the whole bucket instead of
// the bulk lane running a permanent debt.
func (p *Pacer) timeShared() bool { return !p.unlimited && p.sharedCap() < 1 }

// MaxBatch returns the most items one Bulk request may carry without
// spending tokens the bucket cannot hold for it: the burst minus the fast
// reserve on a budgeted endpoint, the protocol cap on an unlimited one. It
// is 0 when the bulk share holds nothing at all (a time-shared bucket);
// callers then send one item at a time and wait their turn at the pacer.
func (p *Pacer) MaxBatch() int { return p.MaxBatchFor(Bulk) }

// MaxBatchFor is MaxBatch for a class: a Fast request may carry the fast
// reserve more than a Bulk one.
func (p *Pacer) MaxBatchFor(class Class) int {
	if p.unlimited {
		return MaxBatch
	}
	held := p.sharedCap()
	if class == Fast {
		held += p.reserve
	}
	return min(max(int(held+tokenEpsilon), 0), MaxBatch)
}

// chunkSize bounds one HTTP batch: what the pacer can hold for the class,
// never more than what is left to send, and at least one item, since a
// bucket with no share for the class still passes one call at a time.
func chunkSize(capacity, remaining int) int {
	return min(max(capacity, 1), remaining)
}

// refillLocked credits the time since the last refill to the bucket and to
// the reserve. The reserve never rises above the bucket: tokens the bulk
// lane spent are gone from both.
func (p *Pacer) refillLocked() {
	t := p.now()
	if elapsed := t.Sub(p.last).Seconds(); elapsed > 0 {
		p.tokens = min(p.tokens+elapsed*p.rate, p.burst)
		p.fastTokens = min(p.fastTokens+elapsed*p.reserveRate, p.reserve)
	}
	p.fastTokens = min(p.fastTokens, p.tokens)
	p.last = t
}

// sharedLocked is what the bucket holds above the reserve's current level:
// the tokens open to any caller.
func (p *Pacer) sharedLocked() float64 { return max(p.tokens-p.fastTokens, 0) }

// openLocked is what the bucket holds for a class right now: the tokens
// above the reserve, or, on a time-shared bucket where the bulk lane has
// no share of its own, the whole bucket for a Bulk caller whose turn it
// is.
func (p *Pacer) openLocked(class Class) float64 {
	if class == Bulk && p.timeShared() {
		if !p.bulkTurnLocked() {
			return 0
		}
		return p.tokens
	}
	return p.sharedLocked()
}

// bulkTurnLocked reports whether the bulk lane may take from a time-shared
// bucket: it yields to a waiting Fast caller that was not the last served.
func (p *Pacer) bulkTurnLocked() bool { return p.fastPending == 0 || p.lastFast }

// fastTurnLocked reports whether the fast lane may draw from a time-shared
// bucket: it yields to a waiting Bulk caller that was not the last served.
func (p *Pacer) fastTurnLocked() bool { return p.bulkPending == 0 || !p.lastFast }

// Available returns the number of whole tokens a Bulk caller can take
// without waiting (math.MaxInt when unlimited): the bucket above the fast
// reserve's current level, or the whole bucket on a time-shared one. A
// reserve the fast tick has drawn down makes that many more tokens
// available to bulk work until it refills.
func (p *Pacer) Available() int {
	if p.unlimited {
		return math.MaxInt
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refillLocked()
	if p.timeShared() {
		return max(int(math.Floor(p.tokens+tokenEpsilon)), 0)
	}
	return max(int(math.Floor(p.sharedLocked()+tokenEpsilon)), 0)
}

// Refund gives n tokens back to a bucket that had already handed them out
// for a request that never reached the wire (a cooldown or a failed-over
// endpoint invalidated the reservation). The bucket never rises above its
// burst, so a refund can only ever return what was taken.
func (p *Pacer) Refund(n int) {
	if p.unlimited || n <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refillLocked()
	p.tokens = min(p.tokens+float64(n), p.burst)
	p.fastTokens = min(p.fastTokens, p.tokens)
	p.notifyTurnLocked()
}

// drawReserveLocked takes whole reserve tokens for a Fast caller, as many
// as it still needs, and returns how many it still needs after that. On a
// time-shared bucket it yields when it is the bulk lane's turn.
func (p *Pacer) drawReserveLocked(remaining int) int {
	if p.timeShared() && !p.fastTurnLocked() {
		return remaining
	}
	k := min(remaining, int(math.Floor(p.fastTokens+tokenEpsilon)))
	if k <= 0 {
		return remaining
	}
	p.tokens -= float64(k)
	p.fastTokens = max(p.fastTokens-float64(k), 0)
	p.servedLocked(true)
	return remaining - k
}

// takeLocked spends as many whole tokens as the class holds right now, at
// most want, and reports how many it took. Nothing is ever borrowed
// against future refills, which is what keeps the windowed budget: a
// caller that wants more than the class holds takes what there is and
// waits for the rest.
func (p *Pacer) takeLocked(class Class, want int) int {
	k := min(want, int(math.Floor(p.openLocked(class)+tokenEpsilon)))
	if k <= 0 {
		return 0
	}
	p.tokens -= float64(k)
	p.fastTokens = min(p.fastTokens, p.tokens)
	if class == Bulk {
		p.servedLocked(false)
	}
	return k
}

// servedLocked records which lane took the last whole token and wakes the
// other one, which may have been waiting for its turn.
func (p *Pacer) servedLocked(fast bool) {
	p.lastFast = fast
	p.notifyTurnLocked()
}

// turnSignalLocked returns a channel closed the next time a lane takes a
// token or stops waiting.
func (p *Pacer) turnSignalLocked() <-chan struct{} {
	if p.turn == nil {
		p.turn = make(chan struct{})
	}
	return p.turn
}

func (p *Pacer) notifyTurnLocked() {
	if p.turn != nil {
		close(p.turn)
		p.turn = nil
	}
}

// enter records a waiting caller of a lane, for the time-sharing turns.
func (p *Pacer) enter(class Class) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if class == Fast {
		p.fastPending++
		return
	}
	p.bulkPending++
}

// leave undoes enter and wakes the other lane, which may have been
// yielding to this one.
func (p *Pacer) leave(class Class) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if class == Fast {
		p.fastPending--
	} else {
		p.bulkPending--
	}
	p.notifyTurnLocked()
}

// Wait takes n tokens for the class ctx carries (Bulk by default). A Fast
// caller first draws what the reserve holds, at once; for the rest, and
// for every Bulk token, it waits its turn at the turnstile and takes
// whole tokens as the bucket holds them, sleeping for the ones it does
// not. Nothing is ever borrowed against future refills, so n above what
// the class may hold at once simply takes several refill periods rather
// than overdrawing.
func (p *Pacer) Wait(ctx context.Context, n int) error {
	if n <= 0 {
		return nil
	}
	if err := ctx.Err(); err != nil || p.unlimited {
		return err
	}
	class := ClassOf(ctx)
	p.enter(class)
	defer p.leave(class)
	if class == Fast && p.timeShared() {
		// The bulk lane may hold the turnstile while it waits for a token
		// of its own, so the fast lane never queues behind it here.
		return p.waitTurns(ctx, n)
	}
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
	return p.spend(ctx, class, remaining)
}

// spend takes tokens while holding the turnstile until the caller has all
// it needs, sleeping (or waiting for its lane's turn) in between.
func (p *Pacer) spend(ctx context.Context, class Class, remaining int) error {
	for {
		p.mu.Lock()
		p.refillLocked()
		if class == Fast {
			remaining = p.drawReserveLocked(remaining)
		}
		remaining -= p.takeLocked(class, remaining)
		if remaining == 0 {
			p.releaseLocked()
			p.mu.Unlock()
			return nil
		}
		turn, d := p.waitForLocked(class, remaining)
		p.mu.Unlock()
		var err error
		if turn != nil {
			select {
			case <-turn:
			case <-ctx.Done():
				err = ctx.Err()
			}
		} else {
			err = p.sleep(ctx, d)
		}
		if err != nil {
			p.mu.Lock()
			p.releaseLocked()
			p.mu.Unlock()
			return err
		}
	}
}

// waitTurns is Wait for a Fast caller on a time-shared bucket: it draws
// whole tokens from the reserve as they refill and as the bulk lane takes
// its turns, and never holds the turnstile.
func (p *Pacer) waitTurns(ctx context.Context, remaining int) error {
	for {
		p.mu.Lock()
		p.refillLocked()
		remaining = p.drawReserveLocked(remaining)
		if remaining == 0 {
			p.mu.Unlock()
			return nil
		}
		yielding := !p.fastTurnLocked()
		turn := p.turnSignalLocked()
		d := p.reserveWaitLocked()
		p.mu.Unlock()
		if yielding {
			select {
			case <-turn:
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		if err := p.sleep(ctx, d); err != nil {
			return err
		}
	}
}

// waitForLocked returns how a caller waits for the tokens it still needs:
// either for its lane's turn on a time-shared bucket (a channel), or for
// the bucket to refill (a duration).
func (p *Pacer) waitForLocked(class Class, remaining int) (<-chan struct{}, time.Duration) {
	if class == Bulk && p.timeShared() && !p.bulkTurnLocked() {
		return p.turnSignalLocked(), 0
	}
	d := p.openWaitLocked(class, remaining)
	if class == Fast {
		d = min(d, p.reserveWaitLocked())
	}
	return nil, d
}

// classCapLocked is the most tokens a class can ever hold at once beyond
// what the reserve already gave it.
func (p *Pacer) classCapLocked(class Class) float64 {
	if class == Bulk && p.timeShared() {
		return p.burst
	}
	return p.sharedCap()
}

// openWaitLocked returns how long until want tokens (never more than the
// class can hold at once) are open to the class, with nobody spending
// meanwhile: the reserve refills alongside, so the shared part grows at
// the rate minus the reserve rate until the reserve is full and at the
// full rate after that. The sleep is rounded up so it always covers the
// shortfall; a Fast draw meanwhile keeps the reserve from filling, which
// only makes the sleeper wake early and sleep again.
func (p *Pacer) openWaitLocked(class Class, want int) time.Duration {
	need := min(float64(want), p.classCapLocked(class))
	shared := p.openLocked(class)
	if shared >= need {
		return 0
	}
	if class == Bulk && p.timeShared() {
		// The whole bucket is open to a bulk caller on its turn.
		return seconds((need - shared) / p.rate)
	}
	var t float64
	if fast := p.fastTokens; fast < p.reserve {
		growth := p.rate - p.reserveRate
		fill := (p.reserve - fast) / p.reserveRate
		if growth > 0 && shared+growth*fill >= need {
			return seconds((need - shared) / growth)
		}
		t += fill
		shared += growth * fill
	}
	return seconds(t + (need-shared)/p.rate)
}

// reserveWaitLocked returns how long until the reserve holds a whole
// token, with nobody spending meanwhile.
func (p *Pacer) reserveWaitLocked() time.Duration {
	return seconds((1 - p.fastTokens) / p.reserveRate)
}

// seconds converts a wait in seconds to a duration, rounded up so that
// truncation can never leave the bucket a rounding error short, and
// saturated at maxPacerWait so a tiny rate cannot overflow the duration or
// park a caller indefinitely.
func seconds(s float64) time.Duration {
	if !(s > 0) {
		return 0
	}
	if s >= maxPacerWait.Seconds() {
		return maxPacerWait
	}
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
		p.abandon(w)
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
			p.abandonLocked(w)
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
			p.abandon(w)
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

// abandon takes w out of the queue, or passes the turnstile on when w was
// handed it meanwhile.
func (p *Pacer) abandon(w *waiter) {
	p.mu.Lock()
	p.abandonLocked(w)
	p.mu.Unlock()
}

func (p *Pacer) abandonLocked(w *waiter) {
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
