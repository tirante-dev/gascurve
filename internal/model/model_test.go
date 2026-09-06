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
