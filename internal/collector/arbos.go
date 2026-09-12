package collector

import (
	"github.com/tirante-dev/gascurve/internal/nitro"
	"github.com/tirante-dev/gascurve/internal/pricer"
)

// unsupportedModel reports the lowest and highest header a constraint replay must not price: blocks
// whose own header says ArbOS is older than the multi-constraint pricer. Pricing one with a constraint
// set is not drift the replay error would show, it is a different model, so the range is left unpriced
// instead. A header carrying no version is not evidence and never triggers this, and neither is a
// legacy replay state, which is the model those blocks actually ran.
func unsupportedModel(st *pricer.State, headers []nitro.Header) (from, to uint64, found bool) {
	if st == nil || st.IsLegacy() {
		return 0, 0, false
	}
	for _, h := range headers {
		if !h.HasArbOSVersion() || pricer.SupportsConstraints(h.ArbOSVersion) {
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
	var prev uint64
	havePrev := false
	for _, h := range headers {
		if !h.HasArbOSVersion() {
			continue
		}
		if havePrev && h.ArbOSVersion != prev {
			return h.Number, true
		}
		prev = h.ArbOSVersion
		havePrev = true
	}
	return 0, false
}
