package pricer

import "math/big"

// Block is the per-block input to a replay: header timestamp and gas used. The observed base fee is
// deliberately absent, because the fee a block's Step produces belongs to the next block; scoring a
// prediction is the caller's job, once it has aligned the two.
type Block struct {
	Number    uint64
	Timestamp uint64
	GasUsed   uint64
}

// Result is the replay output for one block. Backlogs are the end of block values. Exponent,
// PerConstraint and Predicted are what Step computed at this block, which is the fee of the NEXT
// block: ArbOS stores the fee it computes while processing block N in the header of N+1. A caller
// that reports a prediction per block has to move the group forward one block first.
type Result struct {
	Number        uint64
	Backlogs      []uint64
	Exponent      Bips
	PerConstraint []Bips
	Predicted     *big.Int
	Anchored      bool
}

// Anchor is consulted after each block. When it returns (backlogs, true) the replay overwrites its
// backlogs, which is how the collector pins the replay to precompile samples.
type Anchor func(number uint64) ([]uint64, bool)

// Replay runs blocks through the state in order, exactly as ArbOS does: Step(dt) first, then
// AddGas(gasUsed). The state is mutated so a caller can continue with later blocks. Each Result holds
// the Step output at its own block, which prices the next one; see Result.
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
