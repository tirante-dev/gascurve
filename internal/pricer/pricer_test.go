package pricer

import (
	"math"
	"math/big"
	"testing"
)

func robinhoodState() *State {
	return &State{
		Constraints: []Constraint{
			{Target: 60_000_000, Window: 15, Backlog: 3_111_506},
			{Target: 40_000_000, Window: 86_400, Backlog: 11_194_391_810_886},
		},
		MinBaseFee: big.NewInt(20_000_000),
	}
}

func TestApproxExpBips(t *testing.T) {
	cases := []struct {
		x        Bips
		accuracy uint64
		want     Bips
	}{
		{0, 4, 10_000},
		{10_000, 4, 27_083},
		{32_425, 4, 197_863},
		{0, 0, 10_000},
		{-10_000, 4, 3_692}, // 10_000 * 10_000 / 27_083
	}
	for _, tc := range cases {
		if got := ApproxExpBips(tc.x, tc.accuracy); got != tc.want {
			t.Errorf("ApproxExpBips(%d, %d) = %d, want %d", tc.x, tc.accuracy, got, tc.want)
		}
	}
	// Saturating multiplication keeps huge inputs finite, exactly as nitro does:
	// the last iteration is b + MaxUint64/b.
	if got := ApproxExpBips(Bips(math.MaxInt64), 4); got != Bips(10_000+uint64(math.MaxUint64)/10_000) {
		t.Errorf("saturation: got %d", got)
	}
	// The polynomial is a truncated exp: 1 + x + x^2/2 + x^3/6 + x^4/24.
	for _, x := range []Bips{5_000, 20_000, 32_425} {
		fx := float64(x) / 10_000
		want := 1 + fx + fx*fx/2 + fx*fx*fx/6 + fx*fx*fx*fx/24
		got := float64(ApproxExpBips(x, 4)) / 10_000
		if math.Abs(got-want)/want > 0.001 {
			t.Errorf("ApproxExpBips(%d) = %f, taylor = %f", x, got, want)
		}
	}
}

func TestStepRobinhoodVector(t *testing.T) {
	s := robinhoodState()
	baseFee, exponent, per := s.Step(0)
	if exponent != 32_425 {
		t.Fatalf("exponent = %d, want 32425", exponent)
	}
	if per[0] != 34 || per[1] != 32_391 {
		t.Fatalf("per constraint = %v, want [34 32391]", per)
	}
	if baseFee.Cmp(big.NewInt(395_726_000)) != 0 {
		t.Fatalf("base fee = %s, want 395726000", baseFee)
	}
	observed := big.NewInt(399_726_000)
	if e := ErrorBips(baseFee, observed); e > 200 {
		t.Fatalf("prediction differs from observed by %d bips, want < 200", e)
	}
	// Backlogs unchanged with dt = 0.
	if got := s.Backlogs(); got[0] != 3_111_506 || got[1] != 11_194_391_810_886 {
		t.Fatalf("backlogs = %v", got)
	}
}

func TestStepDrainsAndAddsGas(t *testing.T) {
	s := robinhoodState()
	s.Step(1)
	if got := s.Backlogs(); got[0] != 0 || got[1] != 11_194_391_810_886-40_000_000 {
		t.Fatalf("after dt=1: %v", got)
	}
	s.AddGas(1_000)
	if got := s.Backlogs(); got[0] != 1_000 || got[1] != 11_194_391_810_886-40_000_000+1_000 {
		t.Fatalf("after AddGas: %v", got)
	}
	// Zero backlog and zero divisor contribute nothing and do not panic.
	s2 := &State{Constraints: []Constraint{{Target: 0, Window: 0, Backlog: 5}}, MinBaseFee: big.NewInt(7)}
	fee, exp, per := s2.Step(0)
	if fee.Int64() != 7 || exp != 0 || per[0] != 0 {
		t.Fatalf("zero divisor: fee=%s exp=%d per=%v", fee, exp, per)
	}
	// Nil MinBaseFee is treated as zero.
	s3 := &State{Constraints: []Constraint{{Target: 1, Window: 1, Backlog: 5}}}
	if fee, _, _ := s3.Step(0); fee.Sign() != 0 {
		t.Fatalf("nil min base fee: %s", fee)
	}
}

func TestLegacyModel(t *testing.T) {
	// Robinhood testnet style parameters: speed limit 7M, inertia 102, tolerance 10.
	s := &State{
		Legacy:     &Legacy{SpeedLimit: 7_000_000, Inertia: 102, Tolerance: 10, Backlog: 0},
		MinBaseFee: big.NewInt(10_000_000),
	}
	if !s.IsLegacy() {
		t.Fatal("expected legacy")
	}
	fee, exp, per := s.Step(0)
	if fee.Int64() != 10_000_000 || exp != 0 || per[0] != 0 {
		t.Fatalf("at floor: fee=%s exp=%d per=%v", fee, exp, per)
	}
	// Push the backlog above tolerance*speedLimit (70M) by 714M gas: excess
	// 714M over inertia*speedLimit 714M gives exponent 10_000 bips.
	s.AddGas(70_000_000 + 714_000_000)
	fee, exp, per = s.Step(0)
	if exp != 10_000 || per[0] != 10_000 {
		t.Fatalf("exponent = %d per=%v, want 10000", exp, per)
	}
	if fee.Int64() != 27_083_000 {
		t.Fatalf("fee = %s, want 27083000", fee)
	}
	// Draining for 2 s removes 14M gas.
	s.Step(2)
	if got := s.Backlogs(); got[0] != 784_000_000-14_000_000 {
		t.Fatalf("backlog after drain = %v", got)
	}
	s.SetBacklogs([]uint64{5})
	if s.Backlogs()[0] != 5 {
		t.Fatal("SetBacklogs on legacy failed")
	}
	// Zero inertia never divides by zero.
	z := &State{Legacy: &Legacy{SpeedLimit: 1, Inertia: 0, Tolerance: 0, Backlog: 100}, MinBaseFee: big.NewInt(3)}
	if fee, exp, _ := z.Step(0); fee.Int64() != 3 || exp != 0 {
		t.Fatalf("zero inertia: %s %d", fee, exp)
	}
	// Legacy state without parameters is the floor.
	e := &State{MinBaseFee: big.NewInt(9)}
	if fee, exp, per := e.Step(1); fee.Int64() != 9 || exp != 0 || per != nil {
		t.Fatalf("empty state: %s %d %v", fee, exp, per)
	}
	e.AddGas(1)
	e.SetBacklogs([]uint64{1})
	if e.Backlogs() != nil {
		t.Fatal("empty state has no backlogs")
	}
}

func TestSaturation(t *testing.T) {
	if SaturatingUAdd(math.MaxUint64, 1) != math.MaxUint64 {
		t.Fatal("UAdd")
	}
	if SaturatingUSub(1, 2) != 0 || SaturatingUSub(5, 2) != 3 {
		t.Fatal("USub")
	}
	if SaturatingUMul(math.MaxUint64, 2) != math.MaxUint64 || SaturatingUMul(0, 5) != 0 || SaturatingUMul(3, 4) != 12 {
		t.Fatal("UMul")
	}
	if NaturalToBips(math.MaxUint64) != Bips(math.MaxInt64) {
		t.Fatal("NaturalToBips")
	}
	if SaturatingAddBips(Bips(math.MaxInt64), 1) != Bips(math.MaxInt64) {
		t.Fatal("AddBips overflow")
	}
	if SaturatingAddBips(Bips(math.MinInt64), -1) != Bips(math.MinInt64) {
		t.Fatal("AddBips underflow")
	}
	if SaturatingAddBips(1, 2) != 3 {
		t.Fatal("AddBips")
	}
	// A huge backlog saturates the exponent and the multiplier without panicking.
	s := &State{Constraints: []Constraint{{Target: 1, Window: 1, Backlog: math.MaxUint64}}, MinBaseFee: big.NewInt(1)}
	s.AddGas(math.MaxUint64)
	fee, exp, _ := s.Step(0)
	if exp != Bips(math.MaxInt64) || fee.Sign() <= 0 {
		t.Fatalf("saturated step: exp=%d fee=%s", exp, fee)
	}
	if s.Backlogs()[0] != math.MaxUint64 {
		t.Fatal("AddGas should saturate")
	}
	l := &State{Legacy: &Legacy{SpeedLimit: 1, Inertia: 1, Tolerance: 0, Backlog: math.MaxUint64}, MinBaseFee: big.NewInt(1)}
	l.AddGas(1)
	if l.Backlogs()[0] != math.MaxUint64 {
		t.Fatal("legacy AddGas should saturate")
	}
}

func TestClone(t *testing.T) {
	s := robinhoodState()
	s.Legacy = &Legacy{SpeedLimit: 1}
	c := s.Clone()
	c.Constraints[0].Backlog = 1
	c.Legacy.SpeedLimit = 2
	c.MinBaseFee.SetInt64(1)
	if s.Constraints[0].Backlog == 1 || s.Legacy.SpeedLimit == 2 || s.MinBaseFee.Int64() == 1 {
		t.Fatal("clone shares state")
	}
	empty := (&State{}).Clone()
	if empty.MinBaseFee == nil || empty.Constraints != nil || empty.Legacy != nil {
		t.Fatalf("empty clone: %+v", empty)
	}
}

func TestErrorBips(t *testing.T) {
	if ErrorBips(big.NewInt(101), big.NewInt(100)) != 100 {
		t.Fatal("1% should be 100 bips")
	}
	if ErrorBips(big.NewInt(99), big.NewInt(100)) != 100 {
		t.Fatal("abs")
	}
	if ErrorBips(big.NewInt(1), nil) != 0 || ErrorBips(nil, big.NewInt(1)) != 0 || ErrorBips(big.NewInt(1), big.NewInt(0)) != 0 {
		t.Fatal("nil/zero")
	}
	huge := new(big.Int).Lsh(big.NewInt(1), 80)
	if ErrorBips(huge, big.NewInt(1)) != math.MaxInt64 {
		t.Fatal("saturate")
	}
}

func TestReplay(t *testing.T) {
	s := robinhoodState()
	blocks := []Block{
		{Number: 100, Timestamp: 1000, GasUsed: 10_000_000, BaseFee: big.NewInt(395_726_000)},
		{Number: 101, Timestamp: 1000, GasUsed: 5_000_000, BaseFee: big.NewInt(396_000_000)},
		{Number: 102, Timestamp: 1001, GasUsed: 0, BaseFee: big.NewInt(0)},
	}
	anchor := func(n uint64) ([]uint64, bool) {
		if n == 101 {
			return []uint64{1, 2}, true
		}
		return nil, false
	}
	res := Replay(s, 0, blocks, anchor)
	if len(res) != 3 {
		t.Fatalf("results = %d", len(res))
	}
	// Block 100: dt=0, predicted from the vector, then 10M gas added.
	if res[0].Predicted.Int64() != 395_726_000 || res[0].ErrorBips != 0 || res[0].Exponent != 32_425 {
		t.Fatalf("block 100: %+v", res[0])
	}
	if res[0].Backlogs[0] != 13_111_506 || res[0].Anchored {
		t.Fatalf("block 100 backlogs: %+v", res[0])
	}
	// Block 101: dt=0, exponent grew, anchored afterwards.
	if res[1].Exponent <= 32_425 || !res[1].Anchored || res[1].Backlogs[0] != 1 || res[1].Backlogs[1] != 2 {
		t.Fatalf("block 101: %+v", res[1])
	}
	if res[1].ErrorBips == 0 {
		t.Fatalf("block 101 should record an error against 396000000: %+v", res[1])
	}
	// Block 102: dt=1 drains the tiny anchored backlogs to zero, fee at floor.
	if res[2].Predicted.Int64() != 20_000_000 || res[2].Backlogs[0] != 0 || res[2].Backlogs[1] != 0 || res[2].ErrorBips != 0 {
		t.Fatalf("block 102: %+v", res[2])
	}
	// Continuing from a previous timestamp applies dt to the first block.
	s2 := robinhoodState()
	res2 := Replay(s2, 999, blocks[:1], nil)
	if res2[0].Backlogs[0] != 10_000_000 {
		t.Fatalf("prevTimestamp not applied: %+v", res2[0])
	}
	if Replay(s2, 0, nil, nil) == nil {
		t.Fatal("empty replay should return an empty slice")
	}
}
