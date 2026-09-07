package pricer

import "math/big"

// Block is the per-block input to a replay: header timestamp, gas used and
// the observed base fee.
type Block struct {
	Number    uint64
	Timestamp uint64
	GasUsed   uint64
	BaseFee   *big.Int
}

// Result is the replay output for one block. Backlogs are the end of block values; Exponent and
// PerConstraint are the values that produced the block's predicted base fee at its start.
type Result struct {
	Number        uint64
	Backlogs      []uint64
	Exponent      Bips
	PerConstraint []Bips
	Predicted     *big.Int
	ErrorBips     int64
	Anchored      bool
}

// Anchor is consulted after each block. When it returns (backlogs, true) the replay overwrites its
// backlogs, which is how the collector pins the replay to precompile samples.
type Anchor func(number uint64) ([]uint64, bool)

// Replay runs blocks through the state in order, exactly as ArbOS does: Step(dt) first, then
// AddGas(gasUsed). The state is mutated so a caller can continue with later blocks.
//
// ArbOS stores the fee computed by Step(dt) of block N and uses it as the header base fee of block N+1,
// so a prediction for block N is compared against block N's own header here and carries at most one
// block of lag. At Nitro block rates that error is far below the 2% threshold the UI calls estimated.
func Replay(state *State, prevTimestamp uint64, blocks []Block, anchor Anchor) []Result {
	results := make([]Result, 0, len(blocks))
	prev := prevTimestamp
	for _, b := range blocks {
		var dt uint64
		if prev != 0 && b.Timestamp > prev {
			dt = b.Timestamp - prev
		}
		prev = b.Timestamp
		predicted, exponent, per := state.Step(dt)
		state.AddGas(b.GasUsed)
		r := Result{
			Number:        b.Number,
			Exponent:      exponent,
			PerConstraint: per,
			Predicted:     predicted,
			ErrorBips:     ErrorBips(predicted, b.BaseFee),
		}
		if anchor != nil {
			if backlogs, ok := anchor(b.Number); ok {
				state.SetBacklogs(backlogs)
				r.Anchored = true
			}
		}
		r.Backlogs = state.Backlogs()
		results = append(results, r)
	}
	return results
}

// ErrorBips returns |predicted - actual| * 10_000 / actual, or 0 when actual is nil or zero.
func ErrorBips(predicted, actual *big.Int) int64 {
	if actual == nil || actual.Sign() == 0 || predicted == nil {
		return 0
	}
	diff := new(big.Int).Sub(predicted, actual)
	diff.Abs(diff)
	diff.Mul(diff, big.NewInt(int64(OneInBips)))
	diff.Div(diff, actual)
	if !diff.IsInt64() {
		return 1<<63 - 1
	}
	return diff.Int64()
}
