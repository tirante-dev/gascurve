import { describe, expect, it } from "vitest";
import type { LiveSnapshot, Series, SeriesPoint } from "@/types";
import { DIAGNOSIS_STALE_AFTER_MS, MATCHED_HISTORY_MIN_COVERAGE, diagnose, dominantPressure, feeChange, historicalRate } from "./diagnosis";

const NOW = 2_000_000;
const NOW_MS = NOW * 1000;

const point = (t: number, rate = 39_000_000, fee = "20000000", completeness: SeriesPoint["completeness"] = "complete"): SeriesPoint => ({
  t,
  blocks: 1,
  gasUsed: 1,
  posterGas: 0,
  gasPerSecond: rate,
  computeGasPerSecond: rate,
  coverage: completeness === "complete" ? 1 : 0.5,
  completeness,
  feesWei: "0",
  baseFeeMin: fee,
  baseFeeAvg: fee,
  baseFeeMax: fee,
  exponentBips: 1,
  constraintBips: [1],
  backlogs: [1],
  backlogsMax: [1],
  minBaseFee: "20000000",
  floorFeesWei: "0",
  surplusFeesWei: "0",
  posterFeesWei: "0",
  constraintSetId: 1,
  replayErrorBips: 0,
});

const series = (points: SeriesPoint[]): Series => ({ range: "24h", resolution: "1m", from: NOW - 86_400, to: NOW, spreadSeconds: 1, constraintSets: [], ownerActions: [], points });

const snapshot: LiveSnapshot = {
  chainId: 1,
  sampledAt: new Date(NOW_MS).toISOString(),
  block: { number: 1, ts: NOW, gasUsed: 1, posterGas: 0, baseFee: "40000000", txCount: 1 },
  baseFee: "40000000",
  minBaseFee: "20000000",
  multiplierBips: 20_000,
  exponentBips: 10_000,
  model: "constraints",
  constraints: [
    { target: 60_000_000, window: 15, backlog: 1, exponentBips: 100 },
    { target: 40_000_000, window: 86_400, backlog: 1, exponentBips: 9_900 },
  ],
  prices: { perL2Tx: "0", perL1CalldataByte: "0", perL2Storage: "0", perArbGasBase: "0", perArbGasCongestion: "0", perArbGasTotal: "0" },
  gasPerSecond: { s10: 70_000_000, s60: 50_000_000 },
  computeGasPerSecond: { s10: 70_000_000, s60: 50_000_000 },
  replayErrorBips: 0,
  ethUsd: null,
};

describe("dominant pressure", () => {
  it("uses the per-constraint exponent share and preserves no-pressure as none", () => {
    expect(dominantPressure(snapshot.constraints)).toEqual({ index: 1, contributionBips: 9_900, share: 0.99 });
    expect(dominantPressure(snapshot.constraints.map((constraint) => ({ ...constraint, exponentBips: 0 })))).toBeNull();
    expect(dominantPressure([{ ...snapshot.constraints[0], exponentBips: Number.MAX_SAFE_INTEGER + 1 }])).toBeNull();
  });
});

describe("matched history", () => {
  const full = Array.from({ length: 1_440 }, (_, index) => point(NOW - 86_400 + index * 60));

  it("weights a complete matching window and tolerates only a narrow incomplete edge", () => {
    expect(historicalRate(series(full), NOW, 86_400)).toMatchObject({ rate: 39_000_000, periodSeconds: 86_400, coverage: 1, source: "history" });
    const edge = full.map((item, index) => (index === 0 ? { ...item, completeness: "partial" as const, coverage: 0.5 } : item));
    expect(historicalRate(series(edge), NOW, 86_400)?.coverage).toBeCloseTo(1 - 60 / 86_400);
    const missing = full.filter((_, index) => index >= Math.ceil((1 - MATCHED_HISTORY_MIN_COVERAGE) * 1_440));
    expect(historicalRate(series(missing), NOW, 86_400)).toBeNull();
  });

  it("rejects an unknown rate, a per-block series, and invalid windows", () => {
    expect(historicalRate(series([{ ...point(NOW - 60), computeGasPerSecond: null }]), NOW, 60)).toBeNull();
    expect(historicalRate({ ...series(full), resolution: "block" }, NOW, 60)).toBeNull();
    expect(historicalRate(series(full), NOW, 0)).toBeNull();
  });
});

describe("the answer-first diagnosis", () => {
  const full = Array.from({ length: 1_440 }, (_, index) => point(NOW - 86_400 + index * 60));

  it("uses a matching day rate for a dominant day constraint, not the 60 second burst", () => {
    const result = diagnose(snapshot, series(full), NOW_MS);
    expect(result.demand).toMatchObject({ rate: 39_000_000, target: 40_000_000, periodSeconds: 86_400, source: "history" });
    expect(result.direction).toBe("draining");
    expect(result.change?.percent).toBe(100);
    const steady = full.map((item) => ({ ...item, computeGasPerSecond: 40_000_000 }));
    expect(diagnose(snapshot, series(steady), NOW_MS).direction).toBe("steady");
  });

  it("uses the nearest live rate for a short dominant constraint", () => {
    const short = { ...snapshot, constraints: [{ ...snapshot.constraints[0], exponentBips: 10_000 }, { ...snapshot.constraints[1], exponentBips: 0 }] };
    const result = diagnose(short, series(full), NOW_MS);
    expect(result.demand).toMatchObject({ rate: 70_000_000, target: 60_000_000, periodSeconds: 10, source: "snapshot" });
    expect(result.direction).toBe("building");
  });

  it("handles the legacy model, clear pressure, missing inputs, and stale samples", () => {
    const legacy = { ...snapshot, model: "legacy" as const, constraints: [], legacy: { speedLimit: 7_000_000, inertia: 102, tolerance: 10, backlog: 0 } };
    expect(diagnose(legacy, series(full), NOW_MS)).toMatchObject({ dominant: null, demand: { rate: 50_000_000, target: 7_000_000, periodSeconds: 60 }, direction: "building" });
    expect(diagnose({ ...legacy, exponentBips: 0, computeGasPerSecond: { s10: 0, s60: 0 } }, series(full), NOW_MS).direction).toBe("clear");
    expect(diagnose({ ...legacy, exponentBips: 0, legacy: { ...legacy.legacy, backlog: 50_000_000 }, computeGasPerSecond: { s10: 1_000_000, s60: 1_000_000 } }, series(full), NOW_MS).direction).toBe("clear");
    expect(diagnose({ ...snapshot, computeGasPerSecond: undefined }, series(full), NOW_MS).demand).toMatchObject({ rate: 39_000_000, periodSeconds: 86_400, source: "history" });
    const short = { ...snapshot, computeGasPerSecond: undefined, constraints: [{ ...snapshot.constraints[0], exponentBips: 10_000 }, { ...snapshot.constraints[1], exponentBips: 0 }] };
    expect(diagnose(short, series(full), NOW_MS).demand).toBeNull();
    expect(diagnose(snapshot, series(full), NOW_MS + DIAGNOSIS_STALE_AFTER_MS)).toMatchObject({ stale: false });
    expect(diagnose(snapshot, series(full), NOW_MS + DIAGNOSIS_STALE_AFTER_MS + 1)).toMatchObject({ stale: true, dominant: null, change: null, demand: null, direction: null });
  });

  it("reports no recent move without a complete bucket at the comparison time", () => {
    expect(feeChange(snapshot, series([point(NOW - 3_600, 1, "20000000", "partial")]))).toBeNull();
    expect(feeChange(snapshot, series([{ ...point(NOW - 3_600), coverage: Number.NaN }]))).toBeNull();
    expect(feeChange(snapshot, series([{ ...point(NOW - 3_600), coverage: 1.1 }]))).toBeNull();
    expect(feeChange(snapshot, null)).toBeNull();
    expect(feeChange(snapshot, { ...series(full), resolution: "block" })).toBeNull();
  });
});
