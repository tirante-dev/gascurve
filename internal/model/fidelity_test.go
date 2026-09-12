package model

import "testing"

func version(v uint64) *uint64 { return &v }

func TestReplayFidelity(t *testing.T) {
	// Two versions measured on their own, and one crossing measured by replaying through it.
	verified := func(low, high uint64) bool {
		measured := map[uint64]bool{50: true, 51: true, 61: true}
		if !measured[low] || !measured[high] {
			return false
		}
		return low == high || (low == 51 && high == 61)
	}
	tests := []struct {
		name   string
		lo, hi *uint64
		want   string
	}{
		{"no version recorded", nil, nil, FidelityUnknown},
		{"only one bound recorded", version(61), nil, FidelityUnknown},
		{"one measured version", version(61), version(61), FidelityVerified},
		{"the lowest measured version", version(50), version(50), FidelityVerified},
		{"a version nobody measured", version(49), version(49), FidelityUnverified},
		{"a version newer than the measurement", version(62), version(62), FidelityUnverified},
		{"across a crossing someone replayed through", version(51), version(61), FidelityVerified},
		{"across a crossing nobody replayed through", version(50), version(61), FidelityBoundary},
		{"across an upgrade nobody measured either side of", version(40), version(49), FidelityBoundary},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ReplayFidelity(tt.lo, tt.hi, verified); got != tt.want {
				t.Errorf("ReplayFidelity = %q, want %q", got, tt.want)
			}
		})
	}
}
