import { describe, expect, it } from "vitest";
import {
  addGas,
  approxExpBips,
  baseFeeFromExponent,
  constraintExponentBips,
  contributionsBips,
  exponentBips,
  legacyAddGas,
  legacyExponentBips,
  legacyStep,
  MAX_INT64,
  MAX_UINT64,
  MIN_INT64,
  multiplierBips,
  naturalToBips,
  p4Multiplier,
  saturatingAddBips,
  saturatingCastToBips,
  saturatingUAdd,
  saturatingUMul,
  saturatingUSub,
  step,
  stepBacklogs,
  toLegacyState,
  toState,
  toUint64,
  trueExpMultiplier,
} from "./pricer";

describe("approxExpBips (vectors shared with internal/pricer TestApproxExpBips)", () => {
  it("matches nitro at x = 0 and x = 1", () => {
    expect(approxExpBips(0n)).toBe(10_000n);
    // 10000 + 10000/4 = 12500; 10000 + 12500*10000/30000 = 14166;
    // 10000 + 14166*10000/20000 = 17083; 10000 + 17083*10000/10000 = 27083.
    expect(approxExpBips(10_000n)).toBe(27_083n);
    expect(approxExpBips(32_425n)).toBe(197_863n);
  });
  it("is exactly 1 at accuracy 0 instead of dividing by zero", () => {
    expect(approxExpBips(0n, 0)).toBe(10_000n);
    expect(approxExpBips(50_000n, 0)).toBe(10_000n);
  });
  it("takes the reciprocal branch for negative exponents", () => {
    // 10_000 * 10_000 / 27_083.
    expect(approxExpBips(-10_000n)).toBe(3_692n);
    expect(approxExpBips(MIN_INT64)).toBeGreaterThanOrEqual(0n);
  });
  it("saturates huge inputs exactly as Go: the last iteration is b + MaxUint64/b", () => {
    expect(approxExpBips(MAX_INT64)).toBe(10_000n + MAX_UINT64 / 10_000n);
  });
  it("is a truncated exp: 1 + x + x^2/2 + x^3/6 + x^4/24", () => {
    for (const x of [5_000n, 20_000n, 32_425n]) {
      const fx = Number(x) / 10_000;
      const want = 1 + fx + fx ** 2 / 2 + fx ** 3 / 6 + fx ** 4 / 24;
      const got = Number(approxExpBips(x)) / 10_000;
      expect(Math.abs(got - want) / want).toBeLessThan(0.001);
    }
    expect(Number(approxExpBips(50_000n)) / 10_000).toBeLessThan(Math.exp(5));
    expect(approxExpBips(20_000n, 1)).toBe(30_000n);
  });
});

describe("saturating helpers (vectors shared with internal/pricer TestSaturation)", () => {
  it("add, sub and mul saturate at the uint64 bounds", () => {
    expect(saturatingUAdd(MAX_UINT64, 1n)).toBe(MAX_UINT64);
    expect(saturatingUAdd(1n, 2n)).toBe(3n);
    expect(saturatingUSub(1n, 2n)).toBe(0n);
    expect(saturatingUSub(5n, 2n)).toBe(3n);
    expect(saturatingUMul(MAX_UINT64, 2n)).toBe(MAX_UINT64);
    expect(saturatingUMul(0n, 5n)).toBe(0n);
    expect(saturatingUMul(5n, 0n)).toBe(0n);
    expect(saturatingUMul(3n, 4n)).toBe(12n);
  });
  it("casts to bips saturating at MaxInt64", () => {
    expect(naturalToBips(MAX_UINT64)).toBe(MAX_INT64);
    expect(naturalToBips(5n)).toBe(50_000n);
    expect(saturatingCastToBips(MAX_INT64 + 1n)).toBe(MAX_INT64);
    expect(saturatingAddBips(MAX_INT64, 1n)).toBe(MAX_INT64);
    expect(saturatingAddBips(MIN_INT64, -1n)).toBe(MIN_INT64);
    expect(saturatingAddBips(1n, 2n)).toBe(3n);
  });
  it("a valid backlog above MaxUint64 / 10_000 saturates NaturalToBips as Go does", () => {
    // 2e18 * 10_000 overflows uint64, so the numerator is MaxInt64: 2_668_799 bips, not 5_787_037.
    const c = { target: 40_000_000n, window: 86_400n, backlog: 2_000_000_000_000_000_000n };
    expect(constraintExponentBips(c)).toBe(2_668_799n);
  });
  it("a huge backlog saturates the exponent and the multiplier without failing", () => {
    const c = toState([{ target: 1, window: 1, backlog: Number.MAX_SAFE_INTEGER }]);
    c[0].backlog = MAX_UINT64;
    addGas(c, MAX_UINT64);
    expect(c[0].backlog).toBe(MAX_UINT64);
    const r = step(c, 0n, 1n);
    expect(r.exponent).toBe(MAX_INT64);
    expect(r.baseFee).toBeGreaterThan(0n);
    expect(exponentBips([c[0], c[0]])).toBe(MAX_INT64);
    const l = toLegacyState({ speedLimit: 1, inertia: 1, tolerance: 0, backlog: 0 });
    l.backlog = MAX_UINT64;
    legacyAddGas(l, 1n);
    expect(l.backlog).toBe(MAX_UINT64);
  });
  it("converts JSON numbers into uint64 with clamping", () => {
    expect(toUint64(-5)).toBe(0n);
    expect(toUint64(Number.NaN)).toBe(0n);
    expect(toUint64(7.9)).toBe(7n);
    expect(toUint64(2 ** 70)).toBe(MAX_UINT64);
  });
});

describe("Robinhood replay vector (SPEC Appendix B, internal/pricer TestStepRobinhoodVector)", () => {
  const constraints = toState([
    { target: 60_000_000, window: 15, backlog: 3_111_506 },
    { target: 40_000_000, window: 86_400, backlog: 11_194_391_810_886 },
  ]);
  const minBaseFee = 20_000_000n;

  it("sums per-constraint contributions with integer division", () => {
    expect(constraintExponentBips(constraints[0])).toBe(34n);
    expect(constraintExponentBips(constraints[1])).toBe(32_391n);
    expect(exponentBips(constraints)).toBe(32_425n);
    expect(contributionsBips([
      { target: 60_000_000, window: 15, backlog: 3_111_506 },
      { target: 40_000_000, window: 86_400, backlog: 11_194_391_810_886 },
    ])).toEqual([34, 32_391]);
  });

  it("prices within 2% of the observed 399726000 wei", () => {
    const fee = baseFeeFromExponent(minBaseFee, exponentBips(constraints));
    const observed = 399_726_000;
    const error = Math.abs(Number(fee) - observed) / observed;
    expect(error).toBeLessThan(0.02);
    expect(fee).toBe(395_726_000n);
    expect(multiplierBips(32_425n)).toBe(197_863n);
  });

  it("returns the floor when the exponent is zero", () => {
    expect(baseFeeFromExponent(minBaseFee, 0n)).toBe(minBaseFee);
    expect(multiplierBips(0n)).toBe(10_000n);
    expect(constraintExponentBips({ target: 1n, window: 0n, backlog: 5n })).toBe(0n);
    expect(constraintExponentBips({ target: 0n, window: 0n, backlog: 5n })).toBe(0n);
  });

  it("step with dt = 0 leaves the backlogs alone and reports the vector", () => {
    const c = toState([
      { target: 60_000_000, window: 15, backlog: 3_111_506 },
      { target: 40_000_000, window: 86_400, backlog: 11_194_391_810_886 },
    ]);
    const r = step(c, 0n, minBaseFee);
    expect(r.exponent).toBe(32_425n);
    expect(r.contributions).toEqual([34n, 32_391n]);
    expect(r.baseFee).toBe(395_726_000n);
    expect(c.map((x) => x.backlog)).toEqual([3_111_506n, 11_194_391_810_886n]);
  });
});

describe("step and addGas", () => {
  it("drains at target rate, saturating at zero, then prices", () => {
    const c = toState([
      { target: 60_000_000, window: 15, backlog: 100_000_000 },
      { target: 40_000_000, window: 86_400, backlog: 3_456_000_000_000 },
    ]);
    const r = step(c, 1n, 20_000_000n);
    expect(c[0].backlog).toBe(40_000_000n);
    expect(c[1].backlog).toBe(3_456_000_000_000n - 40_000_000n);
    expect(r.contributions[0]).toBe(444n);
    expect(r.exponent).toBe(444n + r.contributions[1]);
    expect(r.baseFee).toBeGreaterThan(20_000_000n);
    const again = step(c, 10n, 20_000_000n);
    expect(c[0].backlog).toBe(0n);
    expect(again.contributions[0]).toBe(0n);
    addGas(c, 5n);
    expect(c[0].backlog).toBe(5n);
  });

  it("matches TestStepDrainsAndAddsGas", () => {
    const c = toState([
      { target: 60_000_000, window: 15, backlog: 3_111_506 },
      { target: 40_000_000, window: 86_400, backlog: 11_194_391_810_886 },
    ]);
    step(c, 1n, 20_000_000n);
    expect(c.map((x) => x.backlog)).toEqual([0n, 11_194_391_810_886n - 40_000_000n]);
    addGas(c, 1_000n);
    expect(c.map((x) => x.backlog)).toEqual([1_000n, 11_194_391_810_886n - 40_000_000n + 1_000n]);
    const zero = step([{ target: 0n, window: 0n, backlog: 5n }], 0n, 7n);
    expect(zero.baseFee).toBe(7n);
    expect(zero.exponent).toBe(0n);
    expect(zero.contributions).toEqual([0n]);
  });

  it("stepBacklogs drains with fractional dt and never goes negative", () => {
    const constraints = [
      { target: 60_000_000, window: 15, backlog: 30_000_000, exponentBips: 0 },
      { target: 40_000_000, window: 86_400, backlog: 1_000_000_000, exponentBips: 0 },
    ];
    expect(stepBacklogs(constraints, 0.25)).toEqual([15_000_000, 990_000_000]);
    expect(stepBacklogs(constraints, 10)).toEqual([0, 600_000_000]);
    expect(stepBacklogs(constraints, -1)).toEqual([30_000_000, 1_000_000_000]);
  });

  it("toState clamps negative backlogs", () => {
    expect(toState([{ target: 1, window: 1, backlog: -5 }])[0].backlog).toBe(0n);
  });
});

describe("comparison helpers", () => {
  it("evaluates P4 and e^x", () => {
    expect(p4Multiplier(0)).toBe(1);
    expect(p4Multiplier(1)).toBeCloseTo(2.7083, 4);
    expect(p4Multiplier(3.2426)).toBeCloseTo(19.79, 1);
    expect(trueExpMultiplier(3.2426)).toBeCloseTo(25.6, 1);
  });
});

describe("legacy model (internal/pricer TestLegacyModel)", () => {
  it("is at the floor until backlog exceeds tolerance * speedLimit", () => {
    const s = toLegacyState({ speedLimit: 7_000_000, inertia: 102, tolerance: 10, backlog: 60_000_000 });
    expect(legacyExponentBips(s)).toBe(0n);
    const r = legacyStep(s, 0n, 10_000_000n);
    expect(r.baseFee).toBe(10_000_000n);
    legacyAddGas(s, 100_000_000n);
    expect(s.backlog).toBe(160_000_000n);
    // (160M - 70M) * 10000 / (102 * 7M) = 1260 bips
    expect(legacyExponentBips(s)).toBe(1260n);
    const priced = legacyStep(s, 1n, 10_000_000n);
    expect(s.backlog).toBe(153_000_000n);
    expect(priced.exponent).toBe(1162n);
    expect(priced.baseFee).toBeGreaterThan(10_000_000n);
    legacyStep(s, 1000n, 10_000_000n);
    expect(s.backlog).toBe(0n);
    expect(legacyExponentBips({ speedLimit: 0n, inertia: 1n, tolerance: 0n, backlog: 5n })).toBe(0n);
    expect(toLegacyState({ speedLimit: 1, inertia: 1, tolerance: 1, backlog: -2 }).backlog).toBe(0n);
  });
  it("matches the Go vector: 784M gas over a 70M threshold with 714M inertia is exactly 10_000 bips", () => {
    const s = toLegacyState({ speedLimit: 7_000_000, inertia: 102, tolerance: 10, backlog: 0 });
    legacyAddGas(s, 70_000_000n + 714_000_000n);
    const r = legacyStep(s, 0n, 10_000_000n);
    expect(r.exponent).toBe(10_000n);
    expect(r.baseFee).toBe(27_083_000n);
    legacyStep(s, 2n, 10_000_000n);
    expect(s.backlog).toBe(784_000_000n - 14_000_000n);
    // Zero inertia never divides by zero.
    const z = toLegacyState({ speedLimit: 1, inertia: 0, tolerance: 0, backlog: 100 });
    expect(legacyStep(z, 0n, 3n)).toEqual({ baseFee: 3n, exponent: 0n });
  });
});
