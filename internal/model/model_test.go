package model

import (
	"encoding/json"
	"testing"
)

func TestLiveSnapshotJSONShape(t *testing.T) {
	s := LiveSnapshot{ChainID: 4663, Constraints: []Constraint{}, BaseFee: "1"}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"chainId", "sampledAt", "block", "baseFee", "minBaseFee", "multiplierBips", "exponentBips", "model", "constraints", "prices", "gasPerSecond", "replayErrorBips"} {
		if _, ok := m[key]; !ok {
			t.Errorf("missing key %s", key)
		}
	}
	for _, key := range []string{"legacy", "l1", "accounts"} {
		if _, ok := m[key]; ok {
			t.Errorf("optional key %s should be omitted", key)
		}
	}
	if m["chainId"] != float64(4663) || m["baseFee"] != "1" {
		t.Fatalf("values: %v", m)
	}
	if _, ok := m["constraints"].([]any); !ok {
		t.Fatalf("constraints should be an array: %v", m["constraints"])
	}
}

func TestNullableTimestamps(t *testing.T) {
	b, err := json.Marshal(Network{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if v, ok := m["headAt"]; !ok || v != nil {
		t.Fatalf("headAt should be present and null: %v", m)
	}
	if v, ok := m["lagSeconds"]; !ok || v != nil {
		t.Fatalf("lagSeconds should be present and null: %v", m)
	}
	b, _ = json.Marshal(NetworkStatus{})
	m = nil
	_ = json.Unmarshal(b, &m)
	for _, key := range []string{"headAt", "lagSeconds", "lastSampleAt"} {
		if v, ok := m[key]; !ok || v != nil {
			t.Fatalf("%s should be present and null: %v", key, m)
		}
	}
	b, _ = json.Marshal(ListenerStatus{})
	m = nil
	_ = json.Unmarshal(b, &m)
	if v, ok := m["lastError"]; !ok || v != nil {
		t.Fatalf("listener lastError should be present and null: %v", m)
	}
	b, _ = json.Marshal(BlockPoint{Backlogs: []uint64{}, ConstraintBips: []int64{}})
	m = nil
	_ = json.Unmarshal(b, &m)
	for _, key := range []string{"constraintBips", "minBaseFee", "backlogs"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("BlockPoint missing %s: %v", key, m)
		}
	}
	b, _ = json.Marshal(SeriesPoint{})
	m = nil
	_ = json.Unmarshal(b, &m)
	for _, key := range []string{"coverage", "completeness", "constraintBips", "minBaseFee", "floorFeesWei", "surplusFeesWei"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("SeriesPoint missing %s: %v", key, m)
		}
	}
}

// TestHoles: a hole reports what is still missing from it, and the
// summary counts what waits for the gap filler apart from what can never
// be filled.
func TestHoles(t *testing.T) {
	cases := []struct {
		h     Hole
		start uint64
		left  uint64
	}{
		{Hole{From: 100, To: 199}, 100, 100},
		{Hole{From: 100, To: 199, Next: 150}, 150, 50},
		{Hole{From: 100, To: 199, Next: 199}, 199, 1},
		// A cursor past the range, or behind its start, leaves the range
		// itself in charge: the hole is complete or has not started.
		{Hole{From: 100, To: 199, Next: 200}, 100, 100},
		{Hole{From: 100, To: 99}, 100, 0},
	}
	for _, tc := range cases {
		if got := tc.h.Start(); got != tc.start {
			t.Fatalf("Start of %+v = %d, want %d", tc.h, got, tc.start)
		}
		if got := tc.h.Blocks(); got != tc.left {
			t.Fatalf("Blocks of %+v = %d, want %d", tc.h, got, tc.left)
		}
	}
	got := SummarizeHoles([]Hole{
		{From: 100, To: 199, Next: 150},
		{From: 0, To: 9, Reason: HoleReasonNoState},
		{From: 500, To: 509},
	})
	if got != (HolesStatus{Pending: 2, Blocks: 50 + 10 + 10, Unfillable: 1}) {
		t.Fatalf("summary: %+v", got)
	}
	if got := SummarizeHoles(nil); got != (HolesStatus{}) {
		t.Fatalf("no holes: %+v", got)
	}
}
