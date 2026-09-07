package nitro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/tirante-dev/gascurve/internal/logger"
)

// rangeLimitedLogs installs an eth_getLogs handler that refuses any range
// wider than limit blocks with refusal, and otherwise returns one OwnerActs
// log for every block in the range that appears in at. It returns a
// counter of refusals.
func rangeLimitedLogs(f *fakeRPC, limit uint64, refusal *RPCError, at ...uint64) *int {
	refused := 0
	f.handlers["eth_getLogs"] = func(params []json.RawMessage) any {
		var filter map[string]string
		_ = json.Unmarshal(params[0], &filter)
		from, _ := strconv.ParseUint(strings.TrimPrefix(filter["fromBlock"], "0x"), 16, 64)
		to, _ := strconv.ParseUint(strings.TrimPrefix(filter["toBlock"], "0x"), 16, 64)
		if to-from+1 > limit {
			refused++
			return refusal
		}
		out := []any{}
		for _, n := range at {
			if n >= from && n <= to {
				out = append(out, json.RawMessage(fmt.Sprintf(
					`{"address":%q,"topics":[%q],"data":"0x00","blockNumber":"0x%x","transactionHash":"0xtx","logIndex":"0x0","blockTimestamp":"0x5"}`,
					ArbOwnerAddress, OwnerActsTopic, n)))
			}
		}
		return out
	}
	return &refused
}

func rangeEndpoint(t *testing.T, f *fakeRPC) (*Endpoint, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	e := newEndpoint(0, endpointOf(f, 0), 100, logger.Nop(), clock.Now, WithHTTPClient(f.server.Client()), withClock(clock.Now, clock.Sleep))
	return e, clock
}

func blockNumbers(logs []Log) []uint64 {
	out := make([]uint64, len(logs))
	for i, l := range logs {
		out[i] = l.BlockNumber
	}
	return out
}

// TestEndpointLogRangeLearned: an endpoint that refuses wide ranges (here
// with QuickNode's wording, though the wording plays no part) is asked for
// narrower and narrower pieces until it answers, the logs come back in
// block order, the accepted width is kept for the next call, and every
// refusal is followed by a growing pause.
func TestEndpointLogRangeLearned(t *testing.T) {
	f := newFakeRPC(t)
	refused := rangeLimitedLogs(f, 10_000, &RPCError{Code: -32614, Message: "eth_getLogs is limited to a 10,000 range"}, 16, 25_000, 60_000, 99_999)
	e, clock := rangeEndpoint(t, f)
	ctx := context.Background()
	if e.LogRange() != MaxLogRange {
		t.Fatalf("starting range %d", e.LogRange())
	}
	logs, err := e.OwnerActsLogs(ctx, 0, 99_999)
	if err != nil {
		t.Fatal(err)
	}
	if got := blockNumbers(logs); fmt.Sprint(got) != "[16 25000 60000 99999]" {
		t.Fatalf("logs %v", got)
	}
	// 100,000 -> 50,000 -> 25,000 -> 12,500 -> 6,250: four refusals, then sixteen pieces.
	if *refused != 4 || f.requests != 20 || e.LogRange() != 6_250 {
		t.Fatalf("refused %d requests %d range %d", *refused, f.requests, e.LogRange())
	}
	if sleeps := clock.Sleeps(); fmt.Sprint(sleeps) != "[1s 2s 4s 8s]" {
		t.Fatalf("back-off between refusals: %v", sleeps)
	}
	// Remembered: the next wide call is split up front and never refused.
	before := f.requests
	if _, err := e.OwnerActsLogs(ctx, 100_000, 199_999); err != nil {
		t.Fatal(err)
	}
	if *refused != 4 || f.requests-before != 16 {
		t.Fatalf("second call: refused %d, %d requests", *refused, f.requests-before)
	}
	// A range within the width is one call, and an empty range none.
	before = f.requests
	if logs, err := e.OwnerActsLogs(ctx, 0, 5_000); err != nil || len(logs) != 1 || f.requests-before != 1 {
		t.Fatalf("narrow call: %v %v %d requests", logs, err, f.requests-before)
	}
	if logs, err := e.OwnerActsLogs(ctx, 5, 4); err != nil || logs != nil {
		t.Fatalf("empty range: %v %v", logs, err)
	}
}

// TestEndpointLogRangeRecovers: after a quiet minute the range doubles
// again, one step per minute, and a refusal at the wider width halves it
// back, so the endpoint's limit is tracked rather than assumed.
func TestEndpointLogRangeRecovers(t *testing.T) {
	f := newFakeRPC(t)
	refused := rangeLimitedLogs(f, 10_000, &RPCError{Code: -32000, Message: "no"})
	e, clock := rangeEndpoint(t, f)
	ctx := context.Background()
	if _, err := e.OwnerActsLogs(ctx, 0, 99_999); err != nil || e.LogRange() != 6_250 {
		t.Fatalf("learn: %v range %d", err, e.LogRange())
	}
	// Within the minute nothing changes.
	if _, err := e.OwnerActsLogs(ctx, 0, 999); err != nil || e.LogRange() != 6_250 {
		t.Fatalf("too soon: %v range %d", err, e.LogRange())
	}
	clock.Advance(capRecoveryInterval)
	if _, err := e.OwnerActsLogs(ctx, 0, 999); err != nil || e.LogRange() != 12_500 {
		t.Fatalf("one step: %v range %d", err, e.LogRange())
	}
	// Only one step per quiet interval, even across several calls.
	if _, err := e.OwnerActsLogs(ctx, 0, 999); err != nil || e.LogRange() != 12_500 {
		t.Fatalf("second step too soon: %v range %d", err, e.LogRange())
	}
	// The probe at the wider width is refused and the range halves back.
	before := *refused
	if _, err := e.OwnerActsLogs(ctx, 0, 12_499); err != nil || e.LogRange() != 6_250 || *refused != before+1 {
		t.Fatalf("probe: %v range %d refused %d", err, e.LogRange(), *refused-before)
	}
	// Recovery never exceeds MaxLogRange.
	g := newFakeRPC(t)
	rangeLimitedLogs(g, 1<<40, nil)
	e2, clock2 := rangeEndpoint(t, g)
	e2.observeLogRange(MaxLogRange, true)
	for range 20 {
		clock2.Advance(capRecoveryInterval)
		e2.observeLogRange(1, false)
	}
	if e2.LogRange() != MaxLogRange {
		t.Fatalf("range %d", e2.LogRange())
	}
}

// TestEndpointLogRangeErrors: a refusal narrows only when narrower is on
// offer; a rate limit the client could not wait out counts as a refusal;
// transport and context failures go straight back to the caller.
func TestEndpointLogRangeErrors(t *testing.T) {
	ctx := context.Background()
	// A single block refused surfaces the endpoint's error.
	f := newFakeRPC(t)
	refused := rangeLimitedLogs(f, 0, &RPCError{Code: -32614, Message: "nothing for you"})
	e, _ := rangeEndpoint(t, f)
	var rpcErr *RPCError
	if _, err := e.OwnerActsLogs(ctx, 7, 7); !errors.As(err, &rpcErr) || rpcErr.Code != -32614 || *refused != 1 {
		t.Fatalf("single block refusal must surface: %v", err)
	}
	// So does a refusal at the floor width.
	g := newFakeRPC(t)
	rangeLimitedLogs(g, 0, &RPCError{Code: -32000, Message: "no"})
	e2, _ := rangeEndpoint(t, g)
	if _, err := e2.OwnerActsLogs(ctx, 0, 99_999); !errors.As(err, &rpcErr) || e2.LogRange() != minLogRange {
		t.Fatalf("floor: %v range %d", err, e2.LogRange())
	}
	// A rate limit the client gave up on narrows the range like any refusal.
	h := newFakeRPC(t)
	rangeLimitedLogs(h, 10_000, &RPCError{Code: RateLimitCodeExceeded, Message: "query returned more than 10000 results"}, 42)
	e3, _ := rangeEndpoint(t, h)
	if logs, err := e3.OwnerActsLogs(ctx, 0, 19_999); err != nil || len(logs) != 1 || e3.LogRange() != 10_000 {
		t.Fatalf("rate limited refusal: %v %v range %d", logs, err, e3.LogRange())
	}
	// A canceled context stops the retries with the context's error.
	k := newFakeRPC(t)
	rangeLimitedLogs(k, 10, &RPCError{Code: -32614, Message: "no"})
	e4, _ := rangeEndpoint(t, k)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := e4.OwnerActsLogs(cctx, 0, 99); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context: %v", err)
	}
	// A server that does not answer is not a refusal: the error goes back
	// unchanged and the range is not touched.
	m := newFakeRPC(t)
	e5, _ := rangeEndpoint(t, m)
	m.server.Close()
	if _, err := e5.OwnerActsLogs(ctx, 0, 99_999); err == nil || errors.As(err, &rpcErr) || e5.LogRange() != MaxLogRange {
		t.Fatalf("transport failure: %v range %d", err, e5.LogRange())
	}
	if logsRefused(errors.New("dial tcp")) || !logsRefused(fmt.Errorf("wrapped: %w", ErrRateLimited)) || !logsRefused(&RPCError{Code: -1, Message: "x"}) {
		t.Fatal("logsRefused classification")
	}
}
