package nitro

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tirante-dev/gascurve/internal/logger"
)

// echoRPC answers "echo" with its first parameter, so a test can tell one request from another.
func echoRPC(t *testing.T) *fakeRPC {
	f := newFakeRPC(t)
	f.handlers["echo"] = echoHandler
	return f
}

func echoRequests(n int) []Request {
	reqs := make([]Request, n)
	for i := range reqs {
		reqs[i] = Request{Method: "echo", Params: []any{fmt.Sprintf("item-%d", i)}}
	}
	return reqs
}

// chunkedEndpoint is an unmetered endpoint whose batch cap forces several chunks per call.
func chunkedEndpoint(t *testing.T, f *fakeRPC, batchSize int) *Endpoint {
	t.Helper()
	clock := newFakeClock()
	return newEndpoint(0, endpointOf(f, 0), batchSize, logger.Nop(), clock.Now,
		WithHTTPClient(f.server.Client()), withClock(clock.Now, clock.Sleep))
}

// TestSendGateBoundsInFlightRequests pins the send gate: a budgeted endpoint keeps one request in
// flight, which is the etiquette rule the budget exists for, and an unmetered one keeps
// maxUnmeteredSends.
func TestSendGateBoundsInFlightRequests(t *testing.T) {
	t.Run("unmetered", func(t *testing.T) {
		f := echoRPC(t)
		f.holdAll(t)
		c, _ := newTestClient(t, f, 0)
		if got := c.sendConcurrency(); got != maxUnmeteredSends {
			t.Fatalf("send concurrency %d, want %d", got, maxUnmeteredSends)
		}
		done := callAll(c, maxUnmeteredSends+2)
		if !f.awaitFlight(maxUnmeteredSends) {
			t.Fatalf("only %d requests reached the server at once, want %d", inFlightOf(f), maxUnmeteredSends)
		}
		time.Sleep(20 * time.Millisecond)
		if in, peak := f.flight(); in != maxUnmeteredSends || peak != maxUnmeteredSends {
			t.Fatalf("in flight %d peak %d, want %d", in, peak, maxUnmeteredSends)
		}
		f.release()
		drain(t, done, maxUnmeteredSends+2)
		if _, peak := f.flight(); peak != maxUnmeteredSends {
			t.Fatalf("peak in flight %d over the whole run, want %d", peak, maxUnmeteredSends)
		}
	})
	t.Run("metered", func(t *testing.T) {
		f := echoRPC(t)
		f.holdAll(t)
		c, _ := newTestClient(t, f, 1000)
		if got := c.sendConcurrency(); got != 1 {
			t.Fatalf("send concurrency %d, want 1 on a budgeted endpoint", got)
		}
		const callers = 6
		available := c.pacer.Available()
		done := callAll(c, callers)
		if !f.awaitFlight(1) {
			t.Fatal("no request reached the server")
		}
		// Every caller has paid the pacer, so all of them are contending for the gate rather than
		// merely not started yet: what holds the other five back is the single slot.
		if !awaitAvailable(c, available-callers) {
			t.Fatalf("pacer availability %d, want all %d callers to have paid", c.pacer.Available(), callers)
		}
		if in, peak := f.flight(); in != 1 || peak != 1 {
			t.Fatalf("in flight %d peak %d, want 1: a budgeted endpoint sends one request at a time", in, peak)
		}
		f.release()
		drain(t, done, callers)
		if _, peak := f.flight(); peak != 1 {
			t.Fatalf("peak in flight %d, want 1", peak)
		}
	})
}

func callAll(c *Client, n int) chan error {
	done := make(chan error, n)
	for range n {
		go func() {
			_, err := c.Call(context.Background(), "echo", "x")
			done <- err
		}()
	}
	return done
}

func drain(t *testing.T, done chan error, n int) {
	t.Helper()
	for range n {
		if err := <-done; err != nil {
			t.Fatalf("call: %v", err)
		}
	}
}

func inFlightOf(f *fakeRPC) int {
	in, _ := f.flight()
	return in
}

// TestFastCallDoesNotQueueBehindBulk is the reason a Fast call takes no slot on an unmetered
// endpoint. Every slot is occupied by a bulk request that will not return, so a Fast call that took
// one would never reach the wire: making the head sample wait for bulk work to drain is worse than
// the single send lock this replaced.
func TestFastCallDoesNotQueueBehindBulk(t *testing.T) {
	f := echoRPC(t)
	f.holdAll(t)
	c, _ := newTestClient(t, f, 0)
	bulk := callAll(c, maxUnmeteredSends)
	if !f.awaitFlight(maxUnmeteredSends) {
		t.Fatalf("bulk filled only %d of %d send slots", inFlightOf(f), maxUnmeteredSends)
	}
	fast := make(chan error, 1)
	go func() {
		_, err := c.Call(WithClass(context.Background(), Fast), "echo", "fast")
		fast <- err
	}()
	if !f.awaitFlight(maxUnmeteredSends + 1) {
		t.Fatalf("the fast call queued behind %d in flight bulk requests", maxUnmeteredSends)
	}
	f.release()
	if err := <-fast; err != nil {
		t.Fatalf("fast call: %v", err)
	}
	drain(t, bulk, maxUnmeteredSends)
	if got := c.Stats().FastCalls; got != 1 {
		t.Fatalf("fast calls %d, want 1", got)
	}
}

// TestBatchFansOutChunksInOrder pins both halves of the fan out: several chunks of one call are in
// flight at once, and the results still land at their own indexes. The fake reverses each response
// array, so nothing here can be passing on arrival order.
func TestBatchFansOutChunksInOrder(t *testing.T) {
	f := echoRPC(t)
	f.reverse = true
	f.holdAll(t)
	e := chunkedEndpoint(t, f, 2)
	reqs := echoRequests(12)
	type outcome struct {
		results []Result
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		results, err := e.batch(context.Background(), reqs)
		done <- outcome{results, err}
	}()
	if !f.awaitFlight(maxUnmeteredSends) {
		t.Fatalf("only %d chunks were in flight at once, want %d", inFlightOf(f), maxUnmeteredSends)
	}
	f.release()
	got := <-done
	if got.err != nil {
		t.Fatalf("batch: %v", got.err)
	}
	if len(got.results) != len(reqs) {
		t.Fatalf("got %d results for %d requests", len(got.results), len(reqs))
	}
	for i, r := range got.results {
		if r.Err != nil {
			t.Fatalf("result %d: %v", i, r.Err)
		}
		if want := fmt.Sprintf("%q", fmt.Sprintf("item-%d", i)); string(r.Raw) != want {
			t.Fatalf("result %d is %s, want %s", i, r.Raw, want)
		}
	}
	if got := f.requestCount(); got != len(reqs)/2 {
		t.Fatalf("%d requests, want %d chunks of two", got, len(reqs)/2)
	}
}

// TestBatchParallelChunkFailureReportsLowestChunk keeps the error a fanned out call returns
// independent of which worker lost first. The later chunk is made to fail while the earlier one is
// still held, so a first-one-wins rule would report the wrong chunk and the answer would depend on
// scheduling.
func TestBatchParallelChunkFailureReportsLowestChunk(t *testing.T) {
	f := echoRPC(t)
	f.failMethods = map[string]bool{"boom_early": true, "boom_late": true}
	f.holdMethod(t, "boom_early")
	e := chunkedEndpoint(t, f, 2)
	reqs := echoRequests(20)
	reqs[2].Method = "boom_early"
	reqs[4].Method = "boom_late"
	done := make(chan error, 1)
	go func() {
		_, err := e.batch(context.Background(), reqs)
		done <- err
	}()
	// Every worker but the held one is finished: the whole first wave has been sent, the late chunk
	// has failed and been recorded, and no further range is handed out.
	if !awaitSettled(f, 1, maxUnmeteredSends) {
		t.Fatalf("in flight %d after %d requests, want only the held chunk left", inFlightOf(f), f.requestCount())
	}
	f.releaseMethod()
	err := <-done
	if err == nil {
		t.Fatal("want the failing chunk's error")
	}
	if !IsEndpointError(err) {
		t.Fatalf("an http 500 must reach the pool as an endpoint error: %v", err)
	}
	if !strings.Contains(err.Error(), "boom_early") || strings.Contains(err.Error(), "boom_late") {
		t.Fatalf("error %q, want the lowest failing chunk's", err)
	}
}

// awaitSettled waits until exactly n requests are in flight and the served count has stopped moving,
// after at least sent have been served. A handler returning proves only that its answer left the
// server; a count that stays put with ranges still unclaimed proves the client recorded the failure,
// since the workers that finished would otherwise have taken the next range and sent it.
func awaitSettled(f *fakeRPC, n, sent int) bool {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		served := f.requestCount()
		if in, _ := f.flight(); in == n && served >= sent {
			time.Sleep(50 * time.Millisecond)
			if in, _ := f.flight(); in == n && f.requestCount() == served {
				return true
			}
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// TestBatchShrinksUnderConcurrentChunks walks the adaptive cap down from several workers at once.
// The cut is monotonic and both sides of it take e.mu, so concurrent refusals commute; this is the
// case -race is here to prove. The barrier is what makes it that case: without it a loopback server
// can answer every refusal in turn, and the test would pass without the cuts ever overlapping.
func TestBatchShrinksUnderConcurrentChunks(t *testing.T) {
	f := echoRPC(t)
	f.sizeLimit = 3
	f.holdAll(t)
	e := chunkedEndpoint(t, f, 8)
	reqs := echoRequests(40)
	type outcome struct {
		results []Result
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		results, err := e.batch(context.Background(), reqs)
		done <- outcome{results, err}
	}()
	if !f.awaitFlight(maxUnmeteredSends) {
		t.Fatalf("only %d chunks were in flight, want %d refused together", inFlightOf(f), maxUnmeteredSends)
	}
	f.release()
	got := <-done
	results, err := got.results, got.err
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	for i, r := range results {
		if r.Err != nil {
			t.Fatalf("result %d: %v", i, r.Err)
		}
		if want := fmt.Sprintf("%q", fmt.Sprintf("item-%d", i)); string(r.Raw) != want {
			t.Fatalf("result %d is %s, want %s", i, r.Raw, want)
		}
	}
	// Four eight-item chunks refused together each compute min(cap, 4), then a wave at four is still
	// refused and computes min(4, 2). Overlapping cuts must land on the same place a serial pair
	// would, not on whichever answered last.
	if got := e.BatchCap(); got != 2 {
		t.Fatalf("batch cap %d, want 2 after two waves of refusals under the %d item limit", got, f.sizeLimit)
	}
	if _, peak := f.flight(); peak < maxUnmeteredSends {
		t.Fatalf("peak in flight %d, want the refusals to have overlapped %d deep", peak, maxUnmeteredSends)
	}
}

// TestConcurrent429IsOneThrottleWave keeps a single throttle from costing several exponential steps.
// Every request already on the wire when the cooldown opens answers 429 too, and charging each of
// them a step would reach the ceiling on the first throttle and delay failover by minutes.
func TestConcurrent429IsOneThrottleWave(t *testing.T) {
	f := echoRPC(t)
	for range maxUnmeteredSends {
		f.script = append(f.script, scriptStep{status: http.StatusTooManyRequests})
	}
	f.holdAll(t)
	clock := newFakeClock()
	c := NewClient(f.server.URL, 0, WithPacer(NewPacer(0).withClock(clock.Now, clock.Sleep)),
		withClock(clock.Now, clock.Sleep), WithHTTPClient(f.server.Client()), WithMaxAttempts(1))
	done := callAll(c, maxUnmeteredSends)
	if !f.awaitFlight(maxUnmeteredSends) {
		t.Fatalf("only %d requests were in flight, want %d", inFlightOf(f), maxUnmeteredSends)
	}
	f.release()
	for range maxUnmeteredSends {
		if err := <-done; !errors.Is(err, ErrRateLimited) {
			t.Fatalf("call: %v, want a rate limit", err)
		}
	}
	st := c.Stats()
	if st.RateLimitEvents != uint64(maxUnmeteredSends) {
		t.Fatalf("rate limit events %d, want every 429 recorded", st.RateLimitEvents)
	}
	if st.Backoff != 2*minBackoff {
		t.Fatalf("back-off %s, want one step from %s: the wave must cost one doubling", st.Backoff, minBackoff)
	}
}

// TestConcurrent429HalvesTheBatchCapOnce is the same rule for the adaptive cap: one wave teaches the
// endpoint one thing, so the cap comes down one step rather than to its floor.
func TestConcurrent429HalvesTheBatchCapOnce(t *testing.T) {
	f := echoRPC(t)
	for range maxUnmeteredSends {
		f.script = append(f.script, scriptStep{status: http.StatusTooManyRequests})
	}
	f.holdAll(t)
	clock := newFakeClock()
	e := newEndpoint(0, endpointOf(f, 0), MaxBatch, logger.Nop(), clock.Now,
		WithHTTPClient(f.server.Client()), withClock(clock.Now, clock.Sleep), WithMaxAttempts(1))
	done := make(chan error, 1)
	go func() {
		_, err := e.batch(context.Background(), echoRequests(MaxBatch*maxUnmeteredSends))
		done <- err
	}()
	if !f.awaitFlight(maxUnmeteredSends) {
		t.Fatalf("only %d chunks were in flight, want %d", inFlightOf(f), maxUnmeteredSends)
	}
	f.release()
	if err := <-done; !errors.Is(err, ErrRateLimited) {
		t.Fatalf("batch: %v, want a rate limit", err)
	}
	if got := e.BatchCap(); got != MaxBatch/2 {
		t.Fatalf("batch cap %d, want %d: one wave halves it once, not down to the floor", got, MaxBatch/2)
	}
}

// TestThrottleWaveIsDecidedBySendTime pins the wave on when a request left rather than on when its
// answer came back. Answers of one wave can straggle across the growing cooldown boundary, and
// classifying them by arrival would let each straggler take another exponential step.
func TestThrottleWaveIsDecidedBySendTime(t *testing.T) {
	f := echoRPC(t)
	clock := newFakeClock()
	c := NewClient(f.server.URL, 0, WithPacer(NewPacer(0).withClock(clock.Now, clock.Sleep)),
		withClock(clock.Now, clock.Sleep), WithHTTPClient(f.server.Client()))
	sent := clock.Now()
	if !c.noteRateLimit(sent) {
		t.Fatal("the first 429 of a wave opens the cooldown")
	}
	if got := c.Stats().Backoff; got != 2*minBackoff {
		t.Fatalf("back-off %s, want one step from %s", got, minBackoff)
	}
	// A straggler of the same wave, answering past the boundary the first one set.
	clock.Advance(minBackoff + time.Second)
	if c.noteRateLimit(sent) {
		t.Fatal("a 429 answering a request sent before the cooldown must not open another one")
	}
	if got := c.Stats().Backoff; got != 2*minBackoff {
		t.Fatalf("back-off %s, want the straggler to cost nothing", got)
	}
	// A request sent after the cooldown had to be waited out is a new wave.
	if !c.noteRateLimit(clock.Now()) {
		t.Fatal("a 429 answering a request sent past the cooldown opens a new one")
	}
	if got := c.Stats().Backoff; got != 4*minBackoff {
		t.Fatalf("back-off %s, want a second step", got)
	}
	if got := c.Stats().RateLimitEvents; got != 3 {
		t.Fatalf("rate limit events %d, want every 429 recorded whether or not it cost a step", got)
	}
}

// TestSuppressedThrottleStillDelaysCapRecovery keeps a wave's later 429s from being forgotten
// entirely: they must not halve the cap again, but the cap they left must still hold for the
// recovery interval measured from the last of them.
func TestSuppressedThrottleStillDelaysCapRecovery(t *testing.T) {
	f := echoRPC(t)
	clock := newFakeClock()
	e := newEndpoint(0, endpointOf(f, 0), MaxBatch, logger.Nop(), clock.Now,
		WithHTTPClient(f.server.Client()), withClock(clock.Now, clock.Sleep))
	e.observe(MaxBatch, true, true)
	shrunk := e.BatchCap()
	if shrunk != MaxBatch/2 {
		t.Fatalf("batch cap %d, want %d after the throttle that opened the cooldown", shrunk, MaxBatch/2)
	}
	half := capRecoveryInterval / 2
	clock.Advance(half)
	e.observe(MaxBatch, true, false)
	if got := e.BatchCap(); got != shrunk {
		t.Fatalf("batch cap %d, want %d: a straggler of the wave must not halve it again", got, shrunk)
	}
	clock.Advance(capRecoveryInterval - half)
	e.observe(1, false, false)
	if got := e.BatchCap(); got != shrunk {
		t.Fatalf("batch cap %d recovered %s after the last 429, want it held for %s", got, capRecoveryInterval-half, capRecoveryInterval)
	}
	clock.Advance(half)
	e.observe(1, false, false)
	if got := e.BatchCap(); got != shrunk*2 {
		t.Fatalf("batch cap %d, want %d once the interval has run from the last 429", got, shrunk*2)
	}
}

func throttleRequests(method string, n int) []Request {
	reqs := make([]Request, n)
	for i := range reqs {
		reqs[i] = Request{Method: method, Params: []any{i}}
	}
	return reqs
}

// TestStragglerThrottleIsSuppressedThroughTheCallSite drives a straggling 429 through send rather
// than calling noteRateLimit and observe by hand, so the timestamp send passes and the observer call
// it makes are covered too. The late request is held inside the handler, so it was already on the
// wire when the early one opened the cooldown, and the clock is moved past that cooldown before its
// answer is released: by arrival it looks like a new wave, by send time it is the same one.
func TestStragglerThrottleIsSuppressedThroughTheCallSite(t *testing.T) {
	f := echoRPC(t)
	f.limitMethods = map[string]bool{"throttle_early": true, "throttle_late": true}
	f.holdMethod(t, "throttle_late")
	clock := newFakeClock()
	e := newEndpoint(0, endpointOf(f, 0), MaxBatch, logger.Nop(), clock.Now,
		WithHTTPClient(f.server.Client()), withClock(clock.Now, clock.Sleep), WithMaxAttempts(1))
	late := make(chan error, 1)
	go func() {
		_, err := e.batch(context.Background(), throttleRequests("throttle_late", MaxBatch))
		late <- err
	}()
	if !f.awaitFlight(1) {
		t.Fatal("the late request never reached the server")
	}
	if _, err := e.batch(context.Background(), throttleRequests("throttle_early", MaxBatch)); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("early batch: %v, want a rate limit", err)
	}
	backoff, shrunk := e.Stats().Backoff, e.BatchCap()
	if backoff != 2*minBackoff || shrunk != MaxBatch/2 {
		t.Fatalf("back-off %s cap %d, want %s and %d from the 429 that opened the cooldown", backoff, shrunk, 2*minBackoff, MaxBatch/2)
	}
	half := capRecoveryInterval / 2
	clock.Advance(half)
	f.releaseMethod()
	if err := <-late; !errors.Is(err, ErrRateLimited) {
		t.Fatalf("late batch: %v, want a rate limit", err)
	}
	if got := e.Stats().Backoff; got != backoff {
		t.Fatalf("back-off %s, want %s: the straggler was sent before the cooldown and costs no step", got, backoff)
	}
	if got := e.BatchCap(); got != shrunk {
		t.Fatalf("batch cap %d, want %d: the straggler must not halve it again", got, shrunk)
	}
	// One recovery interval after the first 429, but only half of one after the straggler, which the
	// endpoint only knows about if the call site reported it.
	clock.Advance(half)
	e.observe(1, false, false)
	if got := e.BatchCap(); got != shrunk {
		t.Fatalf("batch cap %d recovered %s after the last 429, want it held for %s", got, half, capRecoveryInterval)
	}
}

// TestSendSpendsNothingOnACanceledContext covers the race a select cannot resolve: with a free slot
// and a done context both ready, either case may win, so the context is checked again afterwards.
// A Fast call skips the gate entirely and needs the same check.
func TestSendSpendsNothingOnACanceledContext(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cps   float64
		class Class
	}{
		{"metered bulk", 4, Bulk},
		{"unmetered fast", 0, Fast},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := echoRPC(t)
			c, _ := newTestClient(t, f, tc.cps)
			ctx, cancel := context.WithCancel(WithClass(context.Background(), tc.class))
			cancel()
			available := c.pacer.Available()
			if err := c.pacer.Wait(context.Background(), 1); err != nil {
				t.Fatalf("pay the pacer: %v", err)
			}
			if _, _, _, _, err := c.send(ctx, []byte("[]"), 1); !errors.Is(err, context.Canceled) {
				t.Fatalf("send: %v, want the context error", err)
			}
			if got := c.pacer.Available(); got != available {
				t.Fatalf("pacer availability %d, want the %d tokens of an unsent request refunded", got, available)
			}
			// An unlimited pacer reports no availability to compare, so the call accounting is what
			// says the request was abandoned before it was charged for.
			if got := c.Stats().Calls; got != 0 {
				t.Fatalf("%d calls counted for a request that never left", got)
			}
			if got := f.requestCount(); got != 0 {
				t.Fatalf("%d requests reached the server on a canceled context", got)
			}
		})
	}
}

// TestSendSlotHonorsCancellation stops a caller waiting for a send slot from outliving its deadline,
// and gives the pacer back the tokens it paid before queueing.
func TestSendSlotHonorsCancellation(t *testing.T) {
	f := echoRPC(t)
	f.holdAll(t)
	c, _ := newTestClient(t, f, 4)
	first := make(chan error, 1)
	go func() {
		_, err := c.Call(context.Background(), "echo", "held")
		first <- err
	}()
	if !f.awaitFlight(1) {
		t.Fatal("the first request never reached the server")
	}
	available := c.pacer.Available()
	ctx, cancel := context.WithCancel(context.Background())
	queued := make(chan error, 1)
	go func() {
		_, err := c.Call(ctx, "echo", "queued")
		queued <- err
	}()
	// The queued caller has paid the pacer and is waiting on the single slot the budgeted endpoint
	// has, which the held request owns.
	if !awaitAvailable(c, available-1) {
		t.Fatalf("pacer availability %d, want the queued call to have paid %d", c.pacer.Available(), available-1)
	}
	cancel()
	if err := <-queued; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued call: %v, want the context error", err)
	}
	if got := c.pacer.Available(); got != available {
		t.Fatalf("pacer availability %d, want the %d tokens of an unsent request refunded", got, available)
	}
	f.release()
	if err := <-first; err != nil {
		t.Fatalf("first call: %v", err)
	}
	if got := f.requestCount(); got != 1 {
		t.Fatalf("%d requests reached the server, want only the held one", got)
	}
}

func awaitAvailable(c *Client, want int) bool {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if c.pacer.Available() <= want {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// TestPooledTransportClearsTheWorstCase pins the connection ceiling against what the send gate lets
// through: a full set of bulk requests plus a Fast sample that takes no slot and can itself fan out
// to as many chunks. A tighter cap would put the head sample back in a queue, inside the transport.
func TestPooledTransportClearsTheWorstCase(t *testing.T) {
	tr, ok := pooledTransport().(*http.Transport)
	if !ok {
		t.Skip("http.DefaultTransport is not an *http.Transport")
	}
	if tr.MaxConnsPerHost < 2*maxUnmeteredSends {
		t.Fatalf("MaxConnsPerHost %d, want at least %d bulk plus fanned out fast requests", tr.MaxConnsPerHost, 2*maxUnmeteredSends)
	}
	if tr.MaxIdleConnsPerHost < 2*maxUnmeteredSends {
		t.Fatalf("MaxIdleConnsPerHost %d, want the %d a full wave opens kept rather than re-dialed", tr.MaxIdleConnsPerHost, 2*maxUnmeteredSends)
	}
}

// TestTrimCallsDropsStaleOutOfOrder covers the trim as a filter rather than a prefix skip: a stale
// entry behind a fresh one is dropped, so CallsLast10s does not depend on the slice being sorted.
func TestTrimCallsDropsStaleOutOfOrder(t *testing.T) {
	f := echoRPC(t)
	c, _ := newTestClient(t, f, 0)
	now := time.Now()
	c.mu.Lock()
	c.callTimes = []time.Time{
		now.Add(-time.Second),
		now.Add(-2 * callWindow),
		now.Add(-2 * time.Second),
		now.Add(-3 * callWindow),
	}
	c.trimCallsLocked(now)
	kept := append([]time.Time(nil), c.callTimes...)
	c.mu.Unlock()
	if len(kept) != 2 {
		t.Fatalf("kept %d call times, want the 2 inside the window", len(kept))
	}
	for _, at := range kept {
		if now.Sub(at) > callWindow {
			t.Fatalf("kept a call time %s old, past the %s window", now.Sub(at), callWindow)
		}
	}
}
