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
	for _, key := range []string{"constraintBips", "minBaseFee", "floorFeesWei", "surplusFeesWei"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("SeriesPoint missing %s: %v", key, m)
		}
	}
}
