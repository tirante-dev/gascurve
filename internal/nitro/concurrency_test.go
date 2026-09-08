package nitro

import (
	"context"
	"fmt"
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
		f.holdAll()
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
	})
	t.Run("metered", func(t *testing.T) {
		f := echoRPC(t)
		f.holdAll()
		c, _ := newTestClient(t, f, 1000)
		if got := c.sendConcurrency(); got != 1 {
			t.Fatalf("send concurrency %d, want 1 on a budgeted endpoint", got)
		}
		done := callAll(c, 6)
		if !f.awaitFlight(1) {
			t.Fatal("no request reached the server")
		}
		time.Sleep(50 * time.Millisecond)
		if in, peak := f.flight(); in != 1 || peak != 1 {
			t.Fatalf("in flight %d peak %d, want 1: a budgeted endpoint sends one request at a time", in, peak)
		}
		f.release()
		drain(t, done, 6)
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
	f.holdAll()
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
	f.holdAll()
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
	f.gate, f.gateMethod = make(chan struct{}), "boom_early"
	e := chunkedEndpoint(t, f, 2)
	reqs := echoRequests(20)
	reqs[2].Method = "boom_early"
	reqs[4].Method = "boom_late"
	done := make(chan error, 1)
	go func() {
		_, err := e.batch(context.Background(), reqs)
		done <- err
	}()
	// Every worker but the held one is finished: the late chunk has failed and been recorded, and
	// no further range is handed out.
	if !awaitSettled(f, 1) {
		t.Fatalf("in flight %d, want only the held chunk left", inFlightOf(f))
	}
	close(f.gate)
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

// awaitSettled waits until exactly n requests are in flight and stay there, so a test can tell that
// the other workers have finished rather than merely not started.
func awaitSettled(f *fakeRPC, n int) bool {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if in, _ := f.flight(); in == n {
			time.Sleep(50 * time.Millisecond)
			if in, _ := f.flight(); in == n {
				return true
			}
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// TestBatchShrinksUnderConcurrentChunks walks the adaptive cap down from several workers at once.
// The cut is monotonic and both sides of it take e.mu, so concurrent refusals commute; this is the
// case -race is here to prove.
func TestBatchShrinksUnderConcurrentChunks(t *testing.T) {
	f := echoRPC(t)
	f.sizeLimit = 3
	e := chunkedEndpoint(t, f, 8)
	reqs := echoRequests(40)
	results, err := e.batch(context.Background(), reqs)
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
	if got := e.BatchCap(); got > f.sizeLimit {
		t.Fatalf("batch cap %d, want it cut to at most the %d items the node fits", got, f.sizeLimit)
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
