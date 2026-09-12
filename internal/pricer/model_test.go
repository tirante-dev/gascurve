package pricer

import "testing"

func TestAvailableModel(t *testing.T) {
	for _, tt := range []struct {
		version uint64
		want    Model
	}{
		{0, ModelLegacy},
		{1, ModelLegacy},
		{49, ModelLegacy},
		{FirstConstraintVersion, ModelConstraints},
		{61, ModelConstraints},
	} {
		if got := Available(tt.version); got != tt.want {
			t.Errorf("Available(%d) = %v, want %v", tt.version, got, tt.want)
		}
	}
}

func TestModelString(t *testing.T) {
	for m, want := range map[Model]string{ModelUnknown: "unknown", ModelLegacy: "legacy", ModelConstraints: "constraints", Model(99): "unknown"} {
		if got := m.String(); got != want {
			t.Errorf("Model(%d).String() = %q, want %q", m, got, want)
		}
	}
}

// Verified answers for the measured versions and for nothing else, which is what keeps an upgrade from
// silently inheriting the standing of the version before it.
func TestVerifiedCoversTheMeasuredVersionsOnly(t *testing.T) {
	measured := VerifiedVersions(ModelConstraints)
	if len(measured) == 0 {
		t.Fatal("no ArbOS version is recorded as measured")
	}
	for i := range measured[1:] {
		if measured[i] >= measured[i+1] {
			t.Fatalf("VerifiedVersions must be ascending and distinct: %v", measured)
		}
	}
	for _, v := range measured {
		if !Verified(ModelConstraints, v) {
			t.Errorf("Verified(constraints, %d) = false, want true", v)
		}
	}
	for _, v := range []uint64{0, measured[0] - 1, measured[len(measured)-1] + 1} {
		if Verified(ModelConstraints, v) {
			t.Errorf("Verified(constraints, %d) = true, want false", v)
		}
	}
	if len(VerifiedVersions(ModelLegacy)) != 0 || Verified(ModelLegacy, 61) {
		t.Fatal("the constraint-model measurement must not vouch for a legacy replay on the same version")
	}
}

// A crossing is measured separately from either version it joins, because carrying backlogs from one
// model into the next is the thing in question. Two measured versions are not enough on their own.
func TestVerifiedRangeNeedsTheCrossingMeasuredToo(t *testing.T) {
	measured := VerifiedVersions(ModelConstraints)
	for _, v := range measured {
		if !VerifiedRange(ModelConstraints, v, v) {
			t.Errorf("VerifiedRange(%d, %d) = false, want true", v, v)
		}
	}
	unmeasured := measured[len(measured)-1] + 1
	for _, tt := range [][2]uint64{{measured[0], unmeasured}, {unmeasured, unmeasured}, {0, 0}} {
		if VerifiedRange(ModelConstraints, tt[0], tt[1]) {
			t.Errorf("VerifiedRange(%d, %d) = true, want false", tt[0], tt[1])
		}
	}
	crossings := 0
	for _, low := range measured {
		for _, high := range measured {
			if low < high && VerifiedRange(ModelConstraints, low, high) {
				crossings++
			}
		}
	}
	if crossings == 0 {
		t.Error("no crossing is recorded as measured, so no bucket spanning an upgrade can be verified")
	}
}

func TestSupportsConstraintsStartsAtVersion50(t *testing.T) {
	if SupportsConstraints(0) {
		t.Error("SupportsConstraints(0) = true, want false")
	}
	if SupportsConstraints(FirstConstraintVersion - 1) {
		t.Errorf("SupportsConstraints(%d) = true, want false", FirstConstraintVersion-1)
	}
	if !SupportsConstraints(FirstConstraintVersion) {
		t.Errorf("SupportsConstraints(%d) = false, want true", FirstConstraintVersion)
	}
}
