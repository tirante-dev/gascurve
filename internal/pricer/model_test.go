package pricer

import "testing"

func TestAvailableModel(t *testing.T) {
	for _, tt := range []struct {
		version uint64
		want    Model
	}{
		{0, ModelUnknown},
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
	measured := VerifiedVersions()
	if len(measured) == 0 {
		t.Fatal("no ArbOS version is recorded as measured")
	}
	for i := range measured[1:] {
		if measured[i] >= measured[i+1] {
			t.Fatalf("VerifiedVersions must be ascending and distinct: %v", measured)
		}
	}
	for _, v := range measured {
		if !Verified(v) {
			t.Errorf("Verified(%d) = false, want true", v)
		}
	}
	for _, v := range []uint64{0, measured[0] - 1, measured[len(measured)-1] + 1} {
		if Verified(v) {
			t.Errorf("Verified(%d) = true, want false", v)
		}
	}
}

// A header that recorded no version is no evidence against the constraint set the collector holds, so
// it must not be read as a chain too old for the model.
func TestSupportsConstraintsTreatsAnUnrecordedVersionAsOpen(t *testing.T) {
	if !SupportsConstraints(0) {
		t.Error("SupportsConstraints(0) = false, want true")
	}
	if SupportsConstraints(FirstConstraintVersion - 1) {
		t.Errorf("SupportsConstraints(%d) = true, want false", FirstConstraintVersion-1)
	}
	if !SupportsConstraints(FirstConstraintVersion) {
		t.Errorf("SupportsConstraints(%d) = false, want true", FirstConstraintVersion)
	}
}
