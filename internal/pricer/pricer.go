// Package pricer is a pure re-implementation of the Arbitrum Nitro L2 gas pricing model
// (arbos/l2pricing/model.go). It uses the same integer basis point arithmetic as nitro, including
// saturation, so a replay over block headers reproduces the chain's base fee bit for bit.
//
// Two models are supported: the multi-constraint model introduced with ArbOS 50 (a list of
// {target, window, backlog} triples) and the legacy single backlog model.
package pricer

import (
	"math"
	"math/big"
)

// Bips is a value in basis points (1/10_000), nitro's arbmath.Bips.
type Bips int64

// OneInBips is 100% expressed in basis points.
const OneInBips Bips = 10_000

// InitialMinimumBaseFeeWei is nitro's genesis minimum base fee (0.1 gwei), in force until the first
// recorded setMinimumL2BaseFee owner action.
const InitialMinimumBaseFeeWei = 100_000_000

// Constraint is one gas pricing constraint: a gas target per second, an adjustment window in seconds,
// and the current backlog in gas.
type Constraint struct {
	Target  uint64
	Window  uint64
	Backlog uint64
}

// Legacy holds the pre-constraint pricer parameters.
type Legacy struct {
	SpeedLimit uint64
	Inertia    uint64
	Tolerance  uint64
	Backlog    uint64
}

// State is the mutable pricer state. When Constraints is non-empty the
// multi-constraint model is used, otherwise Legacy.
type State struct {
	Constraints []Constraint
	Legacy      *Legacy
	MinBaseFee  *big.Int
}

func (s *State) IsLegacy() bool {
	return len(s.Constraints) == 0
}

func (s *State) Clone() *State {
	out := &State{MinBaseFee: new(big.Int)}
	if s.MinBaseFee != nil {
		out.MinBaseFee.Set(s.MinBaseFee)
	}
	if len(s.Constraints) > 0 {
		out.Constraints = make([]Constraint, len(s.Constraints))
		copy(out.Constraints, s.Constraints)
	}
	if s.Legacy != nil {
		l := *s.Legacy
		out.Legacy = &l
	}
	return out
}

// Backlogs returns the current backlogs, one per constraint (one element for the legacy model).
func (s *State) Backlogs() []uint64 {
	if s.IsLegacy() {
		if s.Legacy == nil {
			return nil
		}
		return []uint64{s.Legacy.Backlog}
	}
	out := make([]uint64, len(s.Constraints))
	for i, c := range s.Constraints {
		out[i] = c.Backlog
	}
	return out
}

// SetBacklogs overwrites the backlogs. Extra values are ignored and missing ones leave the existing
// backlog in place.
func (s *State) SetBacklogs(backlogs []uint64) {
	if s.IsLegacy() {
		if s.Legacy != nil && len(backlogs) > 0 {
			s.Legacy.Backlog = backlogs[0]
		}
		return
	}
	for i := range s.Constraints {
		if i < len(backlogs) {
			s.Constraints[i].Backlog = backlogs[i]
		}
	}
}

// AddGas adds gas used by a transaction to every constraint backlog, saturating.
func (s *State) AddGas(gas uint64) {
	if s.IsLegacy() {
		if s.Legacy != nil {
			s.Legacy.Backlog = SaturatingUAdd(s.Legacy.Backlog, gas)
		}
		return
	}
	for i := range s.Constraints {
		s.Constraints[i].Backlog = SaturatingUAdd(s.Constraints[i].Backlog, gas)
	}
}

// Step advances the pricer by dt seconds as ArbOS does at the start of a block: backlogs are paid down
// at the target rate, then the exponent and base fee are computed. It returns the base fee, the total
// exponent and the per-constraint exponents.
func (s *State) Step(dt uint64) (baseFee *big.Int, exponent Bips, perConstraint []Bips) {
	minBaseFee := s.MinBaseFee
	if minBaseFee == nil {
		minBaseFee = new(big.Int)
	}
	if s.IsLegacy() {
		return s.stepLegacy(dt, minBaseFee)
	}
	perConstraint = make([]Bips, len(s.Constraints))
	for i := range s.Constraints {
		c := &s.Constraints[i]
		c.Backlog = SaturatingUSub(c.Backlog, SaturatingUMul(dt, c.Target))
		if c.Backlog == 0 {
			continue
		}
		divisor := SaturatingUMul(c.Window, c.Target)
		if divisor == 0 {
			continue
		}
		perConstraint[i] = NaturalToBips(c.Backlog) / saturatingCastToBips(divisor)
		exponent = SaturatingAddBips(exponent, perConstraint[i])
	}
	return BaseFeeFromExponent(minBaseFee, exponent), exponent, perConstraint
}

func (s *State) stepLegacy(dt uint64, minBaseFee *big.Int) (baseFee *big.Int, exponent Bips, perConstraint []Bips) {
	if s.Legacy == nil {
		return new(big.Int).Set(minBaseFee), 0, nil
	}
	l := s.Legacy
	l.Backlog = SaturatingUSub(l.Backlog, SaturatingUMul(dt, l.SpeedLimit))
	// Mirrors nitro's updatePricingModelLegacy exactly: the tolerance threshold is a plain uint64
	// multiply (it wraps on overflow), the excess is cast to int64 saturating, and the inertia
	// denominator is a saturating multiply cast to Bips saturating. Nitro would panic on a zero
	// denominator; that case yields no exponent here.
	threshold := l.Tolerance * l.SpeedLimit
	if l.Backlog > threshold {
		inertia := saturatingCastToBips(SaturatingUMul(l.Inertia, l.SpeedLimit))
		if inertia > 0 {
			exponent = NaturalToBips(l.Backlog-threshold) / inertia
		}
	}
	return BaseFeeFromExponent(minBaseFee, exponent), exponent, []Bips{exponent}
}

// BaseFeeFromExponent is minBaseFee * approxExp(exponent) / 10_000 for a positive exponent and
// minBaseFee otherwise (nitro's BigMulByBips).
func BaseFeeFromExponent(minBaseFee *big.Int, exponent Bips) *big.Int {
	if exponent <= 0 {
		return new(big.Int).Set(minBaseFee)
	}
	mult := ApproxExpBips(exponent, 4)
	out := new(big.Int).Mul(minBaseFee, big.NewInt(int64(mult)))
	return out.Div(out, big.NewInt(int64(OneInBips)))
}

// ApproxExpBips is nitro's arbmath.ApproxExpBasisPoints: a degree `accuracy` Taylor polynomial of exp
// evaluated in basis points with saturating unsigned arithmetic. ApproxExpBips(0, n) == OneInBips.
func ApproxExpBips(x Bips, accuracy uint64) Bips {
	if accuracy == 0 {
		return OneInBips
	}
	negative := x < 0
	if negative {
		x = -x
	}
	input := uint64(x)
	b := uint64(OneInBips)
	res := b + input/accuracy
	for i := accuracy - 1; i > 0; i-- {
		res = SaturatingUAdd(b, SaturatingUMul(res, input)/(i*b))
	}
	if negative {
		return saturatingCastToBips(b * b / res)
	}
	return saturatingCastToBips(res)
}

// NaturalToBips converts a natural number into basis points, saturating at MaxInt64 (nitro's
// NaturalToBips/SaturatingCastToBips).
func NaturalToBips(v uint64) Bips {
	return saturatingCastToBips(SaturatingUMul(v, uint64(OneInBips)))
}

func saturatingCastToBips(v uint64) Bips {
	if v > math.MaxInt64 {
		return Bips(math.MaxInt64)
	}
	return Bips(v)
}

func SaturatingAddBips(a, b Bips) Bips {
	sum := a + b
	if b > 0 && sum < a {
		return Bips(math.MaxInt64)
	}
	if b < 0 && sum > a {
		return Bips(math.MinInt64)
	}
	return sum
}

func SaturatingUAdd(a, b uint64) uint64 {
	sum := a + b
	if sum < a {
		return math.MaxUint64
	}
	return sum
}

func SaturatingUSub(a, b uint64) uint64 {
	if b >= a {
		return 0
	}
	return a - b
}

func SaturatingUMul(a, b uint64) uint64 {
	if a == 0 || b == 0 {
		return 0
	}
	if a > math.MaxUint64/b {
		return math.MaxUint64
	}
	return a * b
}
