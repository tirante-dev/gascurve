//go:build fidelity

// Measures replay fidelity against a live chain so that what docs/SPEC.md says about ArbOS version
// boundaries is a measurement rather than an assumption. Behind a build tag because it needs a public
// RPC. Results and how to re-run it: docs/SPEC.md section 7.1.
package collector

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"sort"
	"testing"

	"github.com/tirante-dev/gascurve/internal/nitro"
	"github.com/tirante-dev/gascurve/internal/pricer"
)

var (
	fidelityURL     = flag.String("fidelity.url", "", "RPC endpoint to measure against")
	fidelityCPS     = flag.Float64("fidelity.cps", 4, "calls per second budget, 0 for unlimited")
	fidelityWindows = flag.String("fidelity.windows", "", "JSON array of windows to measure")
	fidelityOut     = flag.String("fidelity.out", "", "write the measured rows to this file as JSON")
)

// window is one measured stretch, replayed as a single run. Targets and windows are the recorded
// constraint set in force over it, from the owner-action history, never from a live call: a live call
// reports today's set, which is the very thing a deep replay cannot assume. Seed is the starting
// backlogs when the owner action recorded them; without it they are fitted to the window. Burn drops
// the first blocks from the score, which is what makes a zero seed at a floor-priced start sound: the
// unknown part of that state is bounded by the exponent threshold plus one block of gas, and every
// constraint drains that in seconds.
type window struct {
	Name    string   `json:"name"`
	From    uint64   `json:"from"`
	Blocks  uint64   `json:"blocks"`
	Targets []uint64 `json:"targets"`
	Windows []uint64 `json:"windows"`
	Seed    []uint64 `json:"seed"`
	MinFee  string   `json:"minFee"`
	Burn    uint64   `json:"burn"`
	// Legacy replaces the constraint set with the pre-ArbOS-50 model, whose parameters a chain that
	// never called a setter runs at nitro's defaults.
	Legacy *fidelityLegacy `json:"legacy"`
}

type fidelityLegacy struct {
	SpeedLimit uint64 `json:"speedLimit"`
	Inertia    uint64 `json:"inertia"`
	Tolerance  uint64 `json:"tolerance"`
}

// result is what one window measured. Errors are |predicted-actual| in bips against each block's own
// header, the same comparison replayErrorBips reports. Congested counts blocks priced above the floor:
// a window entirely at the floor exercises nothing of the model but the floor.
type result struct {
	Window     window `json:"window"`
	ArbOSFirst uint64 `json:"arbosFirst"`
	ArbOSLast  uint64 `json:"arbosLast"`
	Scored     int    `json:"scored"`
	Burned     uint64 `json:"burned"`
	// Seeded says where the starting backlogs came from: "given" from a recorded owner action, "floor"
	// from a zero seed at a block priced exactly at the floor (every backlog is then under its own
	// exponent threshold, so the seed is wrong by a bounded amount the burn-in drains), or "fitted",
	// which searches for them. A fitted seed is indicative only: the coordinate search does not
	// converge reliably on a set of six coupled constraints, so no claim rests on one.
	Seeded     string   `json:"seeded"`
	Exact      int      `json:"exact"`
	Congested  int      `json:"congested"`
	MedianBips int64    `json:"medianBips"`
	P99Bips    int64    `json:"p99Bips"`
	MaxBips    int64    `json:"maxBips"`
	Seed       []uint64 `json:"seedUsed"`
	MaxFee     string   `json:"maxBaseFee"`
}

// TestReplayFidelity replays each window with internal/pricer and reports bit-exactness, median and
// max error in bips. Where the owner action recorded starting backlogs the seed is ground truth; where
// it did not, the seed is fitted to the window's own observed fees, which is the strongest test
// available without historical state, since a handful of free parameters cannot reproduce thousands of
// observed fees unless the model doing the replaying is the model that produced them.
func TestReplayFidelity(t *testing.T) {
	if *fidelityWindows == "" || *fidelityURL == "" {
		t.Skip("set -fidelity.url and -fidelity.windows")
	}
	var windows []window
	if err := json.Unmarshal([]byte(*fidelityWindows), &windows); err != nil {
		t.Fatalf("decode windows: %v", err)
	}
	ctx := context.Background()
	client := nitro.NewClient(*fidelityURL, *fidelityCPS)
	rows := make([]result, 0, len(windows))
	for _, w := range windows {
		rows = append(rows, measureWindow(ctx, t, client, w))
	}
	report(t, rows)
	if *fidelityOut == "" {
		return
	}
	raw, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatalf("encode results: %v", err)
	}
	if err := os.WriteFile(*fidelityOut, raw, 0o600); err != nil {
		t.Fatalf("write results: %v", err)
	}
}

func measureWindow(ctx context.Context, t *testing.T, c *nitro.Client, w window) result {
	t.Helper()
	if w.Legacy == nil && (len(w.Targets) != len(w.Windows) || len(w.Targets) == 0) {
		t.Fatalf("%s: targets and windows must be the same non-empty length", w.Name)
	}
	minFee, ok := new(big.Int).SetString(w.MinFee, 10)
	if !ok {
		t.Fatalf("%s: minFee %q", w.Name, w.MinFee)
	}
	shape := make([]pricer.Constraint, len(w.Targets))
	for i := range w.Targets {
		shape[i] = pricer.Constraint{Target: w.Targets[i], Window: w.Windows[i]}
	}
	headers := fetchHeaders(ctx, t, c, w)
	blocks := make([]pricer.Block, 0, len(headers))
	for _, h := range headers {
		blocks = append(blocks, pricer.Block{Number: h.Number, Timestamp: h.Timestamp, GasUsed: h.ComputeGas()})
	}
	slots := len(shape)
	if w.Legacy != nil {
		slots = 1
	}
	seed, seeded := w.Seed, "given"
	switch {
	case len(seed) == slots && allZero(seed) && headers[0].BaseFee.Cmp(minFee) == 0:
		seeded = "floor"
	case len(seed) != slots:
		seed, seeded = fitSeed(w, shape, minFee, headers, blocks, slots), "fitted"
	}
	errs, exact, congested, maxFee := score(w, shape, minFee, seed, headers, blocks, int(w.Burn))
	sort.Slice(errs, func(i, j int) bool { return errs[i] < errs[j] })
	r := result{
		Window: w, ArbOSFirst: headers[0].ArbOSVersion, ArbOSLast: headers[len(headers)-1].ArbOSVersion,
		Scored: len(errs), Burned: w.Burn, Exact: exact, Congested: congested, Seed: seed, Seeded: seeded, MaxFee: maxFee.String(),
	}
	if len(errs) > 0 {
		r.MedianBips = errs[len(errs)/2]
		r.P99Bips = errs[min(len(errs)-1, len(errs)*99/100)]
		r.MaxBips = errs[len(errs)-1]
	}
	return r
}

// fetchHeaders reads the window plus the block after it: the group a replay computes at block N is
// block N+1's fee, so the last header of the window scores the one past it.
func fetchHeaders(ctx context.Context, t *testing.T, c *nitro.Client, w window) []nitro.Header {
	t.Helper()
	numbers := make([]uint64, 0, w.Blocks+1)
	for i := uint64(0); i <= w.Blocks; i++ {
		numbers = append(numbers, w.From+i)
	}
	out := make([]nitro.Header, 0, len(numbers))
	for start := 0; start < len(numbers); start += 100 {
		chunk := numbers[start:min(start+100, len(numbers))]
		got, err := c.HeadersByNumbers(ctx, chunk)
		if err != nil {
			t.Fatalf("headers %d..%d: %v", chunk[0], chunk[len(chunk)-1], err)
		}
		out = append(out, got...)
	}
	t.Logf("%s: %d headers from %d", w.Name, len(out), w.From)
	return out
}

// score replays from seed and compares each block's predicted fee with the header carrying it, shifted
// by one exactly as the collector stores it.
func score(w window, shape []pricer.Constraint, minFee *big.Int, seed []uint64, headers []nitro.Header, blocks []pricer.Block, burn int) (errs []int64, exact, congested int, maxFee *big.Int) {
	st := fidelityState(w, shape, minFee, seed)
	results := pricer.Replay(st, headers[0].Timestamp, blocks, nil)
	maxFee = new(big.Int)
	for i := burn; i+1 < len(results); i++ {
		actual := headers[i+1].BaseFee
		errs = append(errs, pricer.ErrorBips(results[i].Predicted, actual))
		if results[i].Predicted != nil && results[i].Predicted.Cmp(actual) == 0 {
			exact++
		}
		if actual.Cmp(minFee) > 0 {
			congested++
		}
		if actual.Cmp(maxFee) > 0 {
			maxFee.Set(actual)
		}
	}
	return errs, exact, congested, maxFee
}

func fidelityState(w window, shape []pricer.Constraint, minFee *big.Int, seed []uint64) *pricer.State {
	st := &pricer.State{MinBaseFee: new(big.Int).Set(minFee)}
	if w.Legacy != nil {
		st.Legacy = &pricer.Legacy{SpeedLimit: w.Legacy.SpeedLimit, Inertia: w.Legacy.Inertia, Tolerance: w.Legacy.Tolerance}
	} else {
		st.Constraints = append([]pricer.Constraint(nil), shape...)
	}
	st.SetBacklogs(seed)
	return st
}

// fitSeed searches for the starting backlogs that minimise total error over the window. The fee is
// monotone in every backlog, so a ternary search per constraint converges, and repeated passes settle
// the interaction between them.
func fitSeed(w window, shape []pricer.Constraint, minFee *big.Int, headers []nitro.Header, blocks []pricer.Block, slots int) []uint64 {
	seed := make([]uint64, slots)
	for range 4 {
		for i := range slots {
			seed[i] = fitOne(w, shape, minFee, headers, blocks, seed, i)
		}
	}
	return seed
}

// fitBound is the widest starting backlog worth searching for one slot: many times what the model pays
// off in its own window, which is far past anything a live chain carries.
func fitBound(w window, shape []pricer.Constraint, i int) uint64 {
	if w.Legacy != nil {
		return pricer.SaturatingUMul(w.Legacy.SpeedLimit, pricer.SaturatingUMul(w.Legacy.Tolerance+1, 1024))
	}
	return pricer.SaturatingUMul(shape[i].Target, pricer.SaturatingUMul(shape[i].Window, 64))
}

func fitOne(w window, shape []pricer.Constraint, minFee *big.Int, headers []nitro.Header, blocks []pricer.Block, seed []uint64, i int) uint64 {
	lo, hi := uint64(0), fitBound(w, shape, i)
	for range 96 {
		if hi <= lo+1 {
			break
		}
		a, b := lo+(hi-lo)/3, hi-(hi-lo)/3
		ea := totalError(w, shape, minFee, withAt(seed, i, a), headers, blocks)
		eb := totalError(w, shape, minFee, withAt(seed, i, b), headers, blocks)
		if ea.Cmp(eb) <= 0 {
			hi = b
		} else {
			lo = a
		}
	}
	best, bestErr := seed[i], totalError(w, shape, minFee, seed, headers, blocks)
	for _, cand := range []uint64{lo, (lo + hi) / 2, hi} {
		if e := totalError(w, shape, minFee, withAt(seed, i, cand), headers, blocks); e.Cmp(bestErr) < 0 {
			best, bestErr = cand, e
		}
	}
	return best
}

func allZero(seed []uint64) bool {
	for _, v := range seed {
		if v != 0 {
			return false
		}
	}
	return true
}

func withAt(seed []uint64, i int, v uint64) []uint64 {
	out := append([]uint64(nil), seed...)
	out[i] = v
	return out
}

func totalError(w window, shape []pricer.Constraint, minFee *big.Int, seed []uint64, headers []nitro.Header, blocks []pricer.Block) *big.Int {
	st := fidelityState(w, shape, minFee, seed)
	results := pricer.Replay(st, headers[0].Timestamp, blocks, nil)
	total := new(big.Int)
	for i := 0; i+1 < len(results); i++ {
		total.Add(total, big.NewInt(pricer.ErrorBips(results[i].Predicted, headers[i+1].BaseFee)))
	}
	return total
}

func report(t *testing.T, rows []result) {
	t.Helper()
	t.Log("window | arbos | seed | scored | bit-exact | congested | median bips | p99 bips | max bips")
	for _, r := range rows {
		arbos := fmt.Sprint(r.ArbOSFirst)
		if r.ArbOSLast != r.ArbOSFirst {
			arbos = fmt.Sprintf("%d->%d", r.ArbOSFirst, r.ArbOSLast)
		}
		t.Logf("%s | %s | %s | %d | %.2f%% | %d | %d | %d | %d", r.Window.Name, arbos, r.Seeded, r.Scored,
			100*float64(r.Exact)/float64(max(r.Scored, 1)), r.Congested, r.MedianBips, r.P99Bips, r.MaxBips)
	}
}
