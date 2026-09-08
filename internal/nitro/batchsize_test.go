package nitro

import (
	"context"
	"strings"
	"testing"
)

func TestIsResponseTooLarge(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *RPCError
		want bool
	}{
		{"nil", nil, false},
		{"too large", &RPCError{Code: ResponseTooLargeCode, Message: "response too large"}, true},
		{"other code", &RPCError{Code: -32000, Message: "response too large"}, false},
		{"rate limit", &RPCError{Code: RateLimitCodeQuickNode, Message: "limit"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsResponseTooLarge(tc.err); got != tc.want {
				t.Fatalf("IsResponseTooLarge = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEndpointBatchShrinksOnResponseTooLarge covers the wedge a node's batch response limit used to
// cause: the refusal lands on whichever items did not fit, so re-asking at the same width repeats it
// for ever. The cap has to come down off the chunk that failed.
func TestEndpointBatchShrinksOnResponseTooLarge(t *testing.T) {
	f := newFakeRPC(t)
	setupChain(f)
	f.sizeLimit = 8
	e, _ := rangeEndpoint(t, f)

	numbers := []uint64{100, 101, 102, 103, 104, 105}
	headers, err := e.HeadersByNumbers(context.Background(), numbers)
	if err != nil {
		t.Fatalf("HeadersByNumbers: %v", err)
	}
	if len(headers) != len(numbers) {
		t.Fatalf("got %d headers, want %d", len(headers), len(numbers))
	}
	for i, h := range headers {
		if h.Number != numbers[i] {
			t.Fatalf("header %d is block %d, want %d", i, h.Number, numbers[i])
		}
	}
	if got := e.BatchCap(); got != 6 {
		t.Fatalf("batch cap %d, want 6 after one halving of the twelve item chunk", got)
	}
}

// TestEndpointBatchResponseTooLargeAtOneItem pins the termination condition: a single item the node
// still refuses is its answer, not a width to keep halving, so the error reaches the caller.
func TestEndpointBatchResponseTooLargeAtOneItem(t *testing.T) {
	f := newFakeRPC(t)
	setupChain(f)
	f.sizeLimit = 0
	e, _ := rangeEndpoint(t, f)

	_, err := e.HeadersByNumbers(context.Background(), []uint64{100})
	if err == nil {
		t.Fatal("want an error once a one item batch is still refused")
	}
	if !strings.Contains(err.Error(), "response too large") {
		t.Fatalf("error %q does not name the refusal", err)
	}
	if got := e.BatchCap(); got != sizeBatchFloor {
		t.Fatalf("batch cap %d, want the size floor %d", got, sizeBatchFloor)
	}
}

// TestEndpointSizeShrinkHoldsBeforeRecovering keeps a dense range from oscillating: the cap a size
// refusal set has to survive capRecoveryInterval like a throttled one, or the next good batch would
// double it straight back into the refusal.
func TestEndpointSizeShrinkHoldsBeforeRecovering(t *testing.T) {
	f := newFakeRPC(t)
	setupChain(f)
	f.sizeLimit = 8
	e, clock := rangeEndpoint(t, f)

	if _, err := e.HeadersByNumbers(context.Background(), []uint64{100, 101, 102, 103, 104, 105}); err != nil {
		t.Fatalf("HeadersByNumbers: %v", err)
	}
	shrunk := e.BatchCap()
	e.observe(1, false)
	if got := e.BatchCap(); got != shrunk {
		t.Fatalf("batch cap %d grew before the recovery interval, want %d", got, shrunk)
	}
	clock.Advance(capRecoveryInterval)
	e.observe(1, false)
	if got := e.BatchCap(); got != shrunk*2 {
		t.Fatalf("batch cap %d, want %d one step after the recovery interval", got, shrunk*2)
	}
}
