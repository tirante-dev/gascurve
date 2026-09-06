import { describe, expect, it } from "vitest";
import type { Series } from "@/types";
import {
  buildChartPoints,
  constraintLabel,
  contributionRampStep,
  contributionsFor,
  joinCosts,
  latestSet,
  logDomain,
  rampColor,
  rampInk,
  rampStep,
  resampleFees,
  seriesColor,
  seriesCount,
  seriesLabel,
  shortConstraintLabel,
  spanSeconds,
  sumFeesEth,
  taylorCurve,
} from "./chart";

const series: Series = {
  range: "1h",
  resolution: "5s",
  constraintSets: [
    {
      id: 5,
      effectiveBlock: 10,
      effectiveAt: "2026-09-01T16:33:00Z",
      source: "owner_action",
      constraints: [
        { target: 60_000_000, window: 15, startingBacklog: 0 },
        { target: 30_000_000, window: 86_400, startingBacklog: 0 },
      ],
    },
    {
      id: 6,
      effectiveBlock: 20,
      effectiveAt: "2026-09-03T17:08:00Z",
      source: "owner_action",
      constraints: [
        { target: 60_000_000, window: 15, startingBacklog: 0 },
        { target: 40_000_000, window: 86_400, startingBacklog: 0 },
      ],
    },
  ],
  ownerActions: [],
  points: [
    {
      t: 100,
      blocks: 50,
      gasUsed: 200_000_000,
      gasPerSecond: 40_000_000,
      feesWei: "1000000000000000000",
      baseFeeMin: "20000000",
      baseFeeAvg: "395726000",
      baseFeeMax: "400000000",
      exponentBips: 32_425,
      backlogs: [3_111_506, 11_194_391_810_886],
      backlogsMax: [3_111_506, 11_194_391_810_886],
      constraintSetId: 6,
      replayErrorBips: 3,
    },
    {
      t: 105,
      blocks: 52,
      gasUsed: 100,
      gasPerSecond: 20,
      feesWei: "500000000000000000",
      baseFeeMin: "1",
      baseFeeAvg: "2",
      baseFeeMax: "3",
      exponentBips: 10,
      backlogs: [0, 5],
      backlogsMax: [0, 5],
      constraintSetId: 99,
      replayErrorBips: 0,
    },
  ],
};

describe("colours and ramps", () => {
  it("assigns series colours in fixed order and caps at six", () => {
    expect(seriesColor(0)).toBe("var(--series-1)");
    expect(seriesColor(7)).toBe("var(--series-6)");
  });
  it("maps multipliers onto the sequential ramp", () => {
    expect(rampStep(10_000)).toBe(1);
    expect(rampStep(5_000)).toBe(1);
    expect(rampStep(100_000)).toBe(5);
    expect(rampStep(1_000_000)).toBe(9);
    expect(rampStep(50_000_000)).toBe(9);
    expect(rampColor(199_900)).toBe("var(--seq-6)");
    expect(rampInk(1)).toBe("var(--seq-ink-light)");
    expect(rampInk(6)).toBe("var(--seq-ink-dark)");
    expect(contributionRampStep(0)).toBe(1);
    expect(contributionRampStep(20_000)).toBe(5);
    expect(contributionRampStep(90_000)).toBe(9);
  });
});

describe("labels", () => {
  it("describes constraints", () => {
    expect(constraintLabel({ target: 60_000_000, window: 15 })).toBe("60M gas/s over 15 s");
    expect(shortConstraintLabel({ target: 40_000_000, window: 86_400 })).toBe("40M/s · 24 h");
    expect(seriesLabel(series, 1)).toBe("C2 · 40M/s · 24 h");
    expect(seriesLabel(series, 4)).toBe("C5 · earlier set");
    expect(seriesLabel({ ...series, constraintSets: [] }, 0)).toBe("legacy backlog");
  });
});

describe("chart points", () => {
  it("computes contributions under the point's set and falls back for unknown sets", () => {
    expect(contributionsFor([3_111_506, 11_194_391_810_886], series.constraintSets[1])).toEqual([
      3_111_506 / 900_000_000,
      11_194_391_810_886 / 3_456_000_000_000,
    ]);
    expect(contributionsFor([1, 2], undefined)).toEqual([0, 0]);
    expect(contributionsFor([1, 2, 3], series.constraintSets[1])).toEqual([1 / 900_000_000, 2 / 3_456_000_000_000, 0]);
    const rows = buildChartPoints(series);
    expect(rows[0].feeAvg).toBeCloseTo(0.395726);
    expect(rows[0].x).toBeCloseTo(3.2425);
    expect(rows[0].c1).toBeCloseTo(3.2391, 3);
    expect(rows[0].b1).toBe(11_194_391_810_886);
    expect(rows[0].feesEth).toBeCloseTo(1);
    expect(rows[1].c0).toBeCloseTo(0.001);
    expect(rows[1].c1).toBeCloseTo(0.001);
  });
  it("finds the latest set and the series count", () => {
    expect(latestSet(series)?.id).toBe(6);
    expect(latestSet({ constraintSets: [] })).toBeUndefined();
    expect(seriesCount(series)).toBe(2);
    expect(seriesCount({ ...series, constraintSets: [] })).toBe(2);
    expect(seriesCount({ ...series, constraintSets: [], points: [] })).toBe(0);
  });
  it("sums and spans", () => {
    expect(sumFeesEth(series.points)).toBeCloseTo(1.5);
    expect(spanSeconds(series.points)).toBe(10);
    expect(spanSeconds([{ t: 1 }])).toBe(1);
    expect(spanSeconds([])).toBe(0);
  });
  it("resamples fees and joins with batches", () => {
    const fees = resampleFees(series.points, 60);
    expect([...fees.entries()]).toEqual([[60, 1.5]]);
    const rows = joinCosts(
      [
        { t: 60, batches: 3, gasSpent: 90_000, weiSpent: "6000000000000", l1BaseFeeAvg: "1", calldataBytes: 1 },
        { t: 120, batches: 0, gasSpent: 0, weiSpent: "0", l1BaseFeeAvg: "1", calldataBytes: 0 },
      ],
      fees,
    );
    expect(rows).toEqual([
      { t: 60, l1Eth: 0.000006, l2Eth: 1.5, batches: 3 },
      { t: 120, l1Eth: 0, l2Eth: 0, batches: 0 },
    ]);
  });
  it("builds the Taylor comparison and log domains", () => {
    const curve = taylorCurve((x) => 1 + x, Math.exp, 1, 0.5);
    expect(curve.map((p) => p.x)).toEqual([0, 0.5, 1]);
    expect(curve[2].exp).toBeCloseTo(Math.E);
    expect(logDomain([0.02, 0.39, 5.1])).toEqual([0.01, 10]);
    expect(logDomain([0.5], 0.02)).toEqual([0.01, 1]);
    expect(logDomain([0, -1, Number.NaN])).toEqual([0.001, 1]);
  });
});
