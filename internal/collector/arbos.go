package collector

import (
	"github.com/tirante-dev/gascurve/internal/nitro"
	"github.com/tirante-dev/gascurve/internal/pricer"
)

// unsupportedModel reports the first header a constraint replay must not price, and how far the run of
// such headers reaches. Nitro records the ArbOS version that produced a block in its mix digest, so a
// block older than the multi-constraint pricer says so itself. Pricing one with a constraint set is not
// drift the replay error would show, it is a different model: the range is left unpriced instead, the
// same answer a range with no state to replay from gets.
//
// A header carrying no version at all is not evidence and never triggers this, and neither is a legacy
// replay state, which is the model those blocks actually ran.
func unsupportedModel(st *pricer.State, headers []nitro.Header) (from, to uint64, found bool) {
	if st == nil || st.IsLegacy() {
		return 0, 0, false
	}
	for _, h := range headers {
		if pricer.SupportsConstraints(h.ArbOSVersion) {
			continue
		}
		if !found {
			from, found = h.Number, true
		}
		to = h.Number
	}
	return from, to, found
}

// spansArbOSUpgrade reports the block an ArbOS upgrade takes effect at inside a run of headers, which
// is where a replay carries backlogs from one pricing model into the next. Headers that recorded no
// version are skipped rather than read as a change.
func spansArbOSUpgrade(headers []nitro.Header) (uint64, bool) {
	prev := uint64(0)
	for _, h := range headers {
		if h.ArbOSVersion == 0 {
			continue
		}
		if prev != 0 && h.ArbOSVersion != prev {
			return h.Number, true
		}
		prev = h.ArbOSVersion
	}
	return 0, false
}
