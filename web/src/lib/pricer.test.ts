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
  multiplierBips,
  p4Multiplier,
  step,
  stepBacklogs,
  toLegacyState,
  toState,
  trueExpMultiplier,
} from "./pricer";

describe("approxExpBips", () => {
  it("matches nitro at x = 0 and x = 1", () => {
    expect(approxExpBips(0n)).toBe(10_000n);
    // 10000 + 10000/4 = 12500; 10000 + 12500*10000/30000 = 14166;
    // 10000 + 14166*10000/20000 = 17083; 10000 + 17083*10000/10000 = 27083.
    expect(approxExpBips(10_000n)).toBe(27_083n);
  });
  it("is a polynomial, not an exponential, above x = 2", () => {
    expect(Number(approxExpBips(50_000n)) / 10_000).toBeLessThan(Math.exp(5));
    expect(approxExpBips(20_000n, 1)).toBe(30_000n);
  });
});

describe("Robinhood replay vector (SPEC Appendix B)", () => {
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

describe("legacy model", () => {
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
});
