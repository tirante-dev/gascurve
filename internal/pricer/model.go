package pricer

import "sort"

// The multi-constraint model arrived with ArbOS 50; below it a Nitro chain has only the legacy speed
// limit, inertia and tolerance, whatever the precompile reports today. A block header carries the
// version that produced it (Nitro stores ArbOSFormatVersion in the mix digest), so a replay can check
// the model it is about to apply against the block rather than assume it.
const FirstConstraintVersion uint64 = 50

// verifiedVersions are the ArbOS versions this package has been measured against on a live chain, and
// verifiedCrossings the upgrades measured by replaying straight through them. Both are sets and not
// ranges on purpose: a version nobody has run is a version nobody has checked, and adding an entry is
// a claim that belongs with a new measurement rather than with the upgrade that produced it. One row
// of the table in docs/SPEC.md section 7.1 backs each entry.
var (
	verifiedVersions = map[Model]map[uint64]bool{
		ModelConstraints: {
			51: true,
			61: true,
		},
	}
	verifiedCrossings = map[Model]map[[2]uint64]bool{
		ModelConstraints: {
			{51, 61}: true,
		},
	}
)

// Names of the models, as the api and the docs spell them.
const (
	nameUnknown     = "unknown"
	nameLegacy      = "legacy"
	nameConstraints = "constraints"
)

// Model is a pricing model this package implements.
type Model int

const (
	// ModelUnknown is a replay whose actual pricing model was not recorded.
	ModelUnknown Model = iota
	ModelLegacy
	ModelConstraints
)

func (m Model) String() string {
	switch m {
	case ModelLegacy:
		return nameLegacy
	case ModelConstraints:
		return nameConstraints
	default:
		return nameUnknown
	}
}

// Available is the newest model an ArbOS version can run. A chain on ArbOS 50 or later may still price
// with the legacy model when no constraint set is configured, so this bounds the model rather than
// naming it. Header presence is tracked separately because zero is a real ArbOS version.
func Available(arbosVersion uint64) Model {
	if arbosVersion >= FirstConstraintVersion {
		return ModelConstraints
	}
	return ModelLegacy
}

// Verified reports whether a replay using this model on this ArbOS version is covered by the
// measurement. A version alone is insufficient because newer ArbOS releases can run either model.
func Verified(pricingModel Model, arbosVersion uint64) bool {
	return verifiedVersions[pricingModel][arbosVersion]
}

// VerifiedRange reports whether a replay over blocks running low through high is covered: both ends
// measured, and, where they differ, the crossing between them measured by replaying through it. A
// crossing is its own measurement because carrying backlogs from one model into the next is the thing
// in question, not either model on its own.
func VerifiedRange(pricingModel Model, low, high uint64) bool {
	versions := verifiedVersions[pricingModel]
	if !versions[low] || !versions[high] {
		return false
	}
	return low == high || verifiedCrossings[pricingModel][[2]uint64{low, high}]
}

// VerifiedVersions lists the measured versions, ascending, for anything that reports what the replay
// stands on.
func VerifiedVersions(pricingModel Model) []uint64 {
	versions := verifiedVersions[pricingModel]
	out := make([]uint64, 0, len(versions))
	for v := range versions {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// SupportsConstraints reports whether the constraint model existed at this ArbOS version. Header
// presence is checked by the caller, so version zero here means the real pre-constraint release.
func SupportsConstraints(arbosVersion uint64) bool {
	return arbosVersion >= FirstConstraintVersion
}
