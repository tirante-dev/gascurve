import { describe, expect, it } from "vitest";
import { constraintExponentBips, legacyExponentBips, naturalToBips, saturatingCastToBips, saturatingUMul, toLegacyState } from "@/lib/pricer";
import type { Series, SeriesPoint } from "@/types";
import {
  backlogKey,
  buildChartPoints,
  constraintGauge,
  constraintGaugeSpanLabel,
  constraintGaugeNote,
  constraintLabel,
  contributionKey,
  contributionRampStep,
  gaugeMarks,
  hasUnknownSets,
  hasUnknownSplit,
  hasUnrecordedSplit,
  joinCosts,
  legacyGauge,
  legacyGaugeSpanLabel,
  legacyGaugeNote,
  gaugeSpanLabel,
  MAX_GAUGE_MARKS,
  latestSet,
  logDomain,
  NULL_SPLIT_LABEL,
  rampColor,
  rampInk,
  rampStep,
  FLOOR_COLOR,
  MARKER_COLOR,
  resampleBatches,
  resampleFees,
  bigFraction,
  segmentsFor,
  seriesColor,
  seriesCount,
  setLabel,
  shapeMatches,
  sharesOf,
  shortConstraintLabel,
  slotLabel,
  spanSeconds,
  sumFeesEth,
  sumKnownWeiEth,
  sumWeiEth,
  taylorCurve,
  targetKey,
  UNKNOWN_KEY,
  unknownBacklogKey,
  unknownSlotLabel,
  withSetBoundaries,
} from "./chart";

function point(overrides: Partial<SeriesPoint>): SeriesPoint {
  return {
    t: 0,
    blocks: 1,
    gasUsed: 0,
    posterGas: 0,
    gasPerSecond: 0,
    computeGasPerSecond: 0,
    coverage: 1,
    completeness: "complete",
    feesWei: "0",
    baseFeeMin: "1",
    baseFeeAvg: "1",
    baseFeeMax: "1",
    exponentBips: 0,
    constraintBips: [],
    backlogs: [],
    backlogsMax: [],
    minBaseFee: "1",
    floorFeesWei: "0",
    surplusFeesWei: "0",
    posterFeesWei: "0",
    constraintSetId: 0,
    replayErrorBips: 0,
    ...overrides,
  };
}

const series: Series = {
  range: "1h",
  resolution: "5s",
  from: 1788679200,
  to: 1788679215,
  constraintSets: [
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
  ],
  ownerActions: [],
  points: [
    point({
      t: 100,
      blocks: 50,
      gasUsed: 200_000_000,
      gasPerSecond: 40_000_000,
      feesWei: "1000000000000000000",
      baseFeeMin: "20000000",
      baseFeeAvg: "395726000",
      baseFeeMax: "400000000",
      exponentBips: 32_425,
      constraintBips: [34, 32_391],
      backlogs: [3_111_506, 11_194_391_810_886],
      backlogsMax: [3_111_506, 11_194_391_810_886],
      minBaseFee: "20000000",
      floorFeesWei: "4000000000000000",
      surplusFeesWei: "996000000000000000",
      constraintSetId: 6,
      replayErrorBips: 3,
    }),
    point({
      t: 105,
      blocks: 52,
      gasUsed: 100,
      gasPerSecond: 20,
      feesWei: "500000000000000000",
      baseFeeMin: "1",
      baseFeeAvg: "2",
      baseFeeMax: "3",
      exponentBips: 10_000,
      constraintBips: [4_000, 6_000],
      backlogs: [0, 5],
      backlogsMax: [0, 5],
      minBaseFee: "100000000",
      constraintSetId: 99,
    }),
    point({
      t: 110,
      exponentBips: 0,
      // A block that opened at zero backlog and then used 900M gas: the api's
      // start-of-block split is zero even though the end backlog is a full window.
      constraintBips: [0, 0],
      backlogs: [900_000_000, 900_000_000],
      backlogsMax: [900_000_000, 900_000_000],
      minBaseFee: "20000000",
      constraintSetId: 5,
    }),
  ],
};

describe("colours and ramps", () => {
  it("assigns series colours in fixed order and caps at six", () => {
    expect(seriesColor(0)).toBe("var(--series-1)");
    expect(seriesColor(7)).toBe("var(--series-6)");
    expect(FLOOR_COLOR).toBe("var(--floor)");
    expect(MARKER_COLOR).toBe("var(--marker)");
  });
  it("maps multipliers onto the sequential ramp", () => {
    expect(rampStep(10_000)).toBe(1);
    expect(rampStep(5_000)).toBe(1);
    expect(rampStep(100_000)).toBe(5);
    expect(rampStep(1_000_000)).toBe(9);
    expect(rampStep(50_000_000)).toBe(9);
    expect(rampColor(199_900)).toBe("var(--seq-6)");
    expect(rampInk(1)).toBe("var(--seq-ink-1)");
    expect(rampInk(6)).toBe("var(--seq-ink-6)");
    expect(rampInk(12)).toBe("var(--seq-ink-9)");
    expect(rampInk(0)).toBe("var(--seq-ink-1)");
    expect(contributionRampStep(0)).toBe(1);
    expect(contributionRampStep(20_000)).toBe(5);
    expect(contributionRampStep(90_000)).toBe(9);
  });
});

describe("labels", () => {
  it("describes constraints and sets", () => {
    expect(constraintLabel({ target: 60_000_000, window: 15 })).toBe("60 Mgas/s over 15 s");
    expect(shortConstraintLabel({ target: 40_000_000, window: 86_400 })).toBe("40 Mgas/s · 24 h");
    expect(setLabel({ id: 6, effectiveBlock: 53_578_754 })).toBe("set 6 (from block 53,578,754)");
  });
  it("labels backlog slots with every definition the slot had, oldest first", () => {
    expect(slotLabel(series, 1, "constraints")).toBe("C2 · 30 Mgas/s · 24 h (set 5) then 40 Mgas/s · 24 h (set 6)");
    expect(slotLabel(series, 4, "constraints")).toBe("C5 · definition unknown");
    expect(slotLabel({ constraintSets: [], points: [point({ backlogs: [1] })] }, 0, "legacy")).toBe("legacy backlog");
    // No sets and a constraints (or unknown) model: the panel is unlabelled, never called legacy.
    expect(slotLabel({ constraintSets: [], points: [point({ backlogs: [1] })] }, 0, "constraints")).toBe("C1 · definition unknown");
    expect(slotLabel({ constraintSets: [], points: [point({ backlogs: [1] })] }, 0, "unknown")).toBe(unknownSlotLabel(0));
    expect(unknownBacklogKey(2)).toBe("bu2");
  });
});

describe("segments", () => {
  it("keys one series per set and constraint, oldest set first, so replaced constraints never join", () => {
    const segments = segmentsFor(series, "constraints");
    expect(segments.map((s) => s.key)).toEqual(["c5_0", "c5_1", "c6_0", "c6_1"]);
    expect(segments.map((s) => s.backlogKey)).toEqual(["b5_0", "b5_1", "b6_0", "b6_1"]);
    expect(segments[1].label).toBe("C2 · 30 Mgas/s · 24 h · set 5 (from block 10)");
    expect(segments[3].label).toBe("C2 · 40 Mgas/s · 24 h · set 6 (from block 20)");
    expect(segments[3].color).toBe(seriesColor(1));
    expect(segments[3].constraint?.target).toBe(40_000_000);
    expect(contributionKey(6, 1)).toBe("c6_1");
    expect(backlogKey(6, 1)).toBe("b6_1");
    expect(targetKey(0)).toBe("tgt0");
  });
  it("gives legacy networks a single pseudo segment and nothing for empty series", () => {
    const legacy = segmentsFor({ constraintSets: [], points: [point({ backlogs: [7], constraintBips: [3] })] }, "legacy");
    expect(legacy).toHaveLength(1);
    expect(legacy[0]).toMatchObject({ key: "c0_0", setId: 0, index: 0, label: "legacy backlog", constraint: null });
    expect(segmentsFor({ constraintSets: [], points: [] }, "legacy")).toEqual([]);
    expect(segmentsFor({ constraintSets: [], points: [point({})] }, "legacy")).toEqual([]);
  });
  it("never infers legacy from an empty set list: the model decides", () => {
    const early = { constraintSets: [], points: [point({ constraintSetId: 0, exponentBips: 32_425, constraintBips: [34, 32_391], backlogs: [3_111_506, 11_194_391_810_886] })] };
    expect(segmentsFor(early, "constraints")).toEqual([]);
    expect(segmentsFor(early, "unknown")).toEqual([]);
    expect(hasUnknownSets(early, "constraints")).toBe(true);
    expect(hasUnknownSets(early, "unknown")).toBe(true);
    // Two backlogs cannot be a legacy series either: the model and the data disagree, so the split is unknown.
    expect(hasUnknownSets(early, "legacy")).toBe(true);
    expect(hasUnknownSets({ constraintSets: [], points: [point({ constraintSetId: 0, constraintBips: [34], backlogs: [3_111_506] })] }, "legacy")).toBe(false);
    expect(seriesCount(early)).toBe(2);
  });
  it("reports unknown sets", () => {
    expect(hasUnknownSets(series, "constraints")).toBe(true);
    expect(hasUnknownSets({ ...series, points: series.points.filter((p) => p.constraintSetId !== 99) }, "constraints")).toBe(false);
    expect(hasUnknownSets({ constraintSets: [], points: [point({ backlogs: [1] })] }, "legacy")).toBe(false);
  });
});

describe("chart points", () => {
  const rows = buildChartPoints(series, "constraints");

  it("keeps a measured partial rate but hides one whose divisor is unknown or empty", () => {
    const measured = buildChartPoints({ ...series, points: [point({ gasPerSecond: 20, computeGasPerSecond: 20, coverage: 0.5, completeness: "partial" })] }, "constraints")[0];
    const unknown = buildChartPoints({ ...series, points: [point({ gasPerSecond: 20, computeGasPerSecond: 20, coverage: null, completeness: "unknown" })] }, "constraints")[0];
    const empty = buildChartPoints({ ...series, points: [point({ gasPerSecond: 20, computeGasPerSecond: 20, coverage: 0, completeness: "partial" })] }, "constraints")[0];
    expect(measured.gps).toBe(20);
    expect(unknown.gps).toBeNull();
    expect(empty.gps).toBeNull();
  });

  it("takes contributions from the api's start-of-block integer bips, never from end-of-block backlogs", () => {
    // 34 bips is 0.0034, not 3_111_506 / 900_000_000 = 0.00345.
    expect(rows[0].c6_0).toBe(0.0034);
    expect(rows[0].c6_1).toBe(3.2391);
    expect((rows[0].c6_0 ?? 0) + (rows[0].c6_1 ?? 0)).toBeCloseTo(rows[0].x, 10);
    expect(rows[0].x).toBeCloseTo(3.2425);
    // The block that opened at zero backlog contributes nothing even though its end backlog is a full window.
    expect(rows[2].c5_0).toBe(0);
    expect(rows[2].c5_1).toBe(0);
    expect(rows[2].x).toBe(0);
    expect(rows[2].b5_0).toBe(900_000_000);
  });
  it("segments by set: another set's keys are null (a gap, not a zero to taper towards) for contributions and backlogs", () => {
    expect(rows[0].c5_0).toBeNull();
    expect(rows[0].c5_1).toBeNull();
    expect(rows[0].b5_1).toBeNull();
    expect(rows[0].b6_1).toBe(11_194_391_810_886);
    expect(rows[2].c6_1).toBeNull();
    expect(rows[2].b6_1).toBeNull();
    expect(rows[0].setKnown).toBe(true);
    expect(rows[0].cUnknown).toBeNull();
    expect(rows[0].bu0).toBeNull();
    expect(Object.keys(rows[0]).filter((k) => k.startsWith("c") || k.startsWith("b"))).toEqual(["blocks", "coverage", "completeness", "constraintSetId", "cUnknown", "c5_0", "b5_0", "c5_1", "b5_1", "c6_0", "b6_0", "c6_1", "b6_1", "bu0", "bu1"]);
  });
  it("closes a set with a duplicated boundary row where its successor starts, so the replacement is a vertical edge", () => {
    const drawn = withSetBoundaries(rows);
    expect(drawn.map((r) => [r.t, r.boundary ?? false, r.setKnown ? r.constraintSetId : "unknown"])).toEqual([
      [100, false, 6],
      [105, true, 6],
      [105, false, "unknown"],
      [110, true, "unknown"],
      [110, false, 5],
    ]);
    // Set 6 to unknown: the edge carries set 6's split and backlogs at the new bucket's time, and the new bucket's fee, gas and x.
    const edge = drawn[1];
    expect(edge).toMatchObject({ t: 105, boundary: true, constraintSetId: 6, setKnown: true, c6_0: 0.0034, c6_1: 3.2391, b6_1: 11_194_391_810_886, tgt1: 40_000_000, cUnknown: null, c5_0: null, bu0: null, x: 1, feeAvg: rows[1].feeAvg, gps: rows[1].gps, blocks: rows[1].blocks });
    expect(drawn[2].c6_1).toBeNull();
    expect(drawn[2].cUnknown).toBe(1);
    // Unknown to set 5: the edge is the unknown split with its slot backlogs, and no target since none was known.
    const edge2 = drawn[3];
    expect(edge2).toMatchObject({ t: 110, boundary: true, constraintSetId: 99, setKnown: false, cUnknown: 1, bu0: 0, bu1: 5, c5_0: null, c5_1: null });
    expect(edge2.tgt1).toBeUndefined();
    expect(drawn[4].tgt1).toBe(30_000_000);
    expect(drawn[4].c5_0).toBe(0);
    // No boundary while the set stays the same; nothing for nothing.
    expect(withSetBoundaries([rows[0], { ...rows[0], t: 101 }])).toHaveLength(2);
    expect(withSetBoundaries([])).toEqual([]);
  });
  it("carries the target in force per point, stepped across sets", () => {
    expect(rows[0].tgt1).toBe(40_000_000);
    expect(rows[2].tgt1).toBe(30_000_000);
    expect(rows[1].tgt1).toBeUndefined();
  });
  it("puts an unknown set's total x into one unknown series instead of every slot", () => {
    const row = rows[1];
    expect(row.setKnown).toBe(false);
    expect(row[UNKNOWN_KEY]).toBe(1);
    expect(row.c5_0).toBeNull();
    expect(row.c5_1).toBeNull();
    expect(row.c6_0).toBeNull();
    expect(row.c6_1).toBeNull();
    expect(row.b6_0).toBeNull();
    // Its backlogs are still drawn, under the unknown-slot keys.
    expect(row.bu0).toBe(0);
    expect(row.bu1).toBe(5);
    const stacked = (row.c5_0 ?? 0) + (row.c5_1 ?? 0) + (row.c6_0 ?? 0) + (row.c6_1 ?? 0) + (row.cUnknown ?? 0);
    expect(stacked).toBe(row.x);
  });
  it("draws early history with no set metadata as the unknown split with unlabelled backlogs, not as legacy", () => {
    const early: Series = { ...series, constraintSets: [], points: [point({ constraintSetId: 0, exponentBips: 32_425, constraintBips: [34, 32_391], backlogs: [3_111_506, 11_194_391_810_886] })] };
    for (const model of ["constraints", "unknown"] as const) {
      const [row] = buildChartPoints(early, model);
      expect(row.setKnown).toBe(false);
      expect(row.cUnknown).toBeCloseTo(3.2425);
      expect(row.bu0).toBe(3_111_506);
      expect(row.bu1).toBe(11_194_391_810_886);
      expect(row.c0_0).toBeUndefined();
      expect(row.tgt0).toBeUndefined();
    }
    // Two backlogs on a legacy network disagree with the model: unknown as well, never squeezed into one slot.
    const [mismatch] = buildChartPoints(early, "legacy");
    expect(mismatch.setKnown).toBe(false);
    expect(mismatch.c0_0).toBeNull();
    expect(mismatch.bu1).toBe(11_194_391_810_886);
    // Single-backlog data on a legacy network is the one legacy series.
    const [legacy] = buildChartPoints({ ...early, points: [point({ constraintSetId: 0, exponentBips: 34, constraintBips: [34], backlogs: [3_111_506] })] }, "legacy");
    expect(legacy.setKnown).toBe(true);
    expect(legacy.c0_0).toBe(0.0034);
    expect(legacy.b0_0).toBe(3_111_506);
    expect(legacy.cUnknown).toBeNull();
  });
  it("carries the floor and fee split per bucket", () => {
    expect(rows[0].floor).toBeCloseTo(0.02);
    expect(rows[1].floor).toBeCloseTo(0.1);
    expect(rows[0].feeAvg).toBeCloseTo(0.395726);
    expect(rows[0].feesEth).toBeCloseTo(1);
    expect(rows[0].floorFeesEth).toBeCloseTo(0.004);
    expect(rows[0].surplusFeesEth).toBeCloseTo(0.996);
  });
  it("keeps total fees intact across all three receipt-backed destinations", () => {
    const [row] = buildChartPoints({
      ...series,
      points: [point({ gasUsed: 422_716, posterGas: 767, computeGasPerSecond: 421_949, feesWei: "8469537776000", floorFeesWei: "8438980000000", surplusFeesWei: "15190164000", posterFeesWei: "15367612000" })],
    }, "constraints");
    expect(row.gps).toBe(421_949);
    expect(row.floorFeesEth! + row.surplusFeesEth! + row.posterFeesEth!).toBeCloseTo(row.feesEth, 18);
  });

  it("leaves compute throughput unknown while historical poster gas is unavailable", () => {
    const [row] = buildChartPoints({ ...series, points: [point({ posterGas: null, computeGasPerSecond: null })] }, "constraints");
    expect(row.gps).toBeNull();
  });
  it("does not treat an old api total-gas rate as compute gas", () => {
    const [row] = buildChartPoints({ ...series, points: [point({ gasPerSecond: 123, computeGasPerSecond: undefined })] }, "constraints");
    expect(row.gps).toBeNull();
  });
  it("handles legacy networks as one series", () => {
    const legacy = buildChartPoints({ ...series, constraintSets: [], points: [point({ exponentBips: 1260, constraintBips: [1260], backlogs: [160_000_000] })] }, "legacy");
    expect(legacy[0].c0_0).toBe(0.126);
    expect(legacy[0].b0_0).toBe(160_000_000);
    expect(legacy[0].setKnown).toBe(true);
    expect(legacy[0].tgt0).toBeUndefined();
  });
  it("finds the latest set and the series count", () => {
    expect(latestSet(series)?.id).toBe(6);
    expect(latestSet({ constraintSets: [] })).toBeUndefined();
    expect(seriesCount(series)).toBe(2);
    expect(seriesCount({ ...series, constraintSets: [] })).toBe(2);
    expect(seriesCount({ ...series, constraintSets: [], points: [] })).toBe(0);
  });
  it("computes shares from integer bips", () => {
    expect(sharesOf([34, 32_391])).toEqual([34 / 32_425, 32_391 / 32_425]);
    expect(sharesOf([0, 0])).toEqual([0, 0]);
    expect(sharesOf([-1, 1])).toEqual([0, 1]);
  });
  it("sums and spans", () => {
    expect(sumFeesEth(series.points)).toBeCloseTo(1.5);
    expect(sumWeiEth(series.points, "feesWei")).toBeCloseTo(1.5);
    expect(sumKnownWeiEth(series.points, "floorFeesWei")).toEqual({ eth: expect.closeTo(0.004, 6), unknown: 0 });
    expect(spanSeconds(series.points)).toBe(15);
    expect(spanSeconds([{ t: 1 }])).toBe(1);
    expect(spanSeconds([])).toBe(0);
  });
});

describe("history from before the split migration", () => {
  // A bucket under set 6 whose per-constraint split and fee destinations were never recorded.
  const unrecorded = point({
    t: 95,
    blocks: 40,
    feesWei: "1000000000000000000",
    baseFeeMin: "20000000",
    baseFeeAvg: "395726000",
    baseFeeMax: "400000000",
    exponentBips: 32_425,
    constraintBips: null,
    backlogs: [3_111_506, 11_194_391_810_886],
    backlogsMax: [3_111_506, 11_194_391_810_886],
    minBaseFee: "20000000",
    floorFeesWei: null,
    surplusFeesWei: null,
    constraintSetId: 6,
  });
  const mixed: Series = { ...series, points: [unrecorded, ...series.points] };

  it("draws a null split as the unknown split under its own set, keeping the backlogs and the target", () => {
    const [row, known] = buildChartPoints(mixed, "constraints");
    expect(row.setKnown).toBe(true);
    expect(row.splitKnown).toBe(false);
    expect(row.cUnknown).toBeCloseTo(3.2425);
    expect(row.c6_0).toBeNull();
    expect(row.c6_1).toBeNull();
    expect(row.b6_0).toBe(3_111_506);
    expect(row.b6_1).toBe(11_194_391_810_886);
    expect(row.bu0).toBeNull();
    expect(row.bu1).toBeNull();
    expect(row.tgt1).toBe(40_000_000);
    // The recorded bucket next to it is unchanged.
    expect(known.splitKnown).toBe(true);
    expect(known.cUnknown).toBeNull();
    expect(known.c6_1).toBe(3.2391);
    expect(NULL_SPLIT_LABEL).not.toBe(UNKNOWN_KEY);
  });

  it("tells an unrecorded split from an unknown set", () => {
    expect(hasUnrecordedSplit(mixed)).toBe(true);
    expect(hasUnrecordedSplit(series)).toBe(false);
    expect(hasUnknownSets(mixed, "constraints")).toBe(true);
    expect(hasUnknownSets({ ...series, points: [unrecorded, series.points[0]] }, "constraints")).toBe(false);
    expect(hasUnknownSplit({ ...series, points: [unrecorded, series.points[0]] }, "constraints")).toBe(true);
    expect(hasUnknownSplit({ ...series, points: [series.points[0]] }, "constraints")).toBe(false);
    expect(hasUnknownSplit({ ...series, points: [series.points[1]] }, "constraints")).toBe(true);
    // Without a split the point is shaped by its backlogs.
    expect(shapeMatches(series.constraintSets[0], unrecorded)).toBe(true);
    expect(shapeMatches({ constraints: series.constraintSets[0].constraints.slice(0, 1) }, unrecorded)).toBe(false);
    expect(seriesCount({ constraintSets: [], points: [unrecorded] })).toBe(2);
    expect(seriesCount({ constraintSets: [], points: [point({ constraintBips: null })] })).toBe(0);
    // A legacy network with an unrecorded split still has its one series from the backlog.
    expect(segmentsFor({ constraintSets: [], points: [point({ constraintBips: null, backlogs: [7] })] }, "legacy")).toHaveLength(1);
  });

  it("keeps a null fee split null rather than zero, and moves the bucket's fees to the unsplit series", () => {
    const [row, known] = buildChartPoints(mixed, "constraints");
    expect(row.floorFeesEth).toBeNull();
    expect(row.surplusFeesEth).toBeNull();
    expect(row.unsplitFeesEth).toBeCloseTo(1);
    expect(known.unsplitFeesEth).toBeNull();
    expect(known.floorFeesEth).toBeCloseTo(0.004);
    expect(known.surplusFeesEth).toBeCloseTo(0.996);
    // One missing part is enough for the split to be unknown.
    const [half] = buildChartPoints({ ...series, points: [point({ feesWei: "1000000000000000000", floorFeesWei: "1", surplusFeesWei: null })] }, "constraints");
    expect(half.floorFeesEth).toBeNull();
    expect(half.surplusFeesEth).toBeNull();
    expect(half.unsplitFeesEth).toBeCloseTo(1);
    expect(sumKnownWeiEth(mixed.points, "floorFeesWei")).toEqual({ eth: expect.closeTo(0.004, 6), unknown: 1 });
    expect(sumKnownWeiEth(mixed.points, "surplusFeesWei")).toEqual({ eth: expect.closeTo(0.996, 6), unknown: 1 });
    expect(sumKnownWeiEth([], "surplusFeesWei")).toEqual({ eth: 0, unknown: 0 });
  });

  it("closes the unrecorded split with a boundary edge where the recorded one starts", () => {
    const drawn = withSetBoundaries(buildChartPoints(mixed, "constraints").slice(0, 2));
    expect(drawn.map((r) => [r.t, r.boundary ?? false, r.setKnown, r.splitKnown])).toEqual([
      [95, false, true, false],
      [100, true, true, false],
      [100, false, true, true],
    ]);
    // The edge carries the unrecorded split at the new bucket's time: the whole x under the unknown series, set 6's backlogs and target.
    expect(drawn[1]).toMatchObject({ constraintSetId: 6, cUnknown: expect.closeTo(3.2425, 4), c6_0: null, c6_1: null, b6_1: 11_194_391_810_886, tgt1: 40_000_000, x: drawn[2].x });
    expect(drawn[2].cUnknown).toBeNull();
    expect(drawn[2].c6_1).toBe(3.2391);
    // Two unrecorded buckets in a row draw as one series.
    expect(withSetBoundaries(buildChartPoints({ ...series, points: [unrecorded, { ...unrecorded, t: 96 }] }, "constraints"))).toHaveLength(2);
  });
});

describe("L1 cost join", () => {
  it("resamples fees and batches into the same buckets, summing wei before converting", () => {
    const fees = resampleFees(series.points, 60);
    expect([...fees.entries()]).toEqual([[60, 1.5]]);
    const buckets = resampleBatches(
      [
        { t: 60, batches: 1, weiSpent: "500000000000" },
        { t: 72, batches: 1, weiSpent: "500000000000" },
        { t: 120, batches: 0, weiSpent: "0" },
      ],
      15,
    );
    expect(buckets).toEqual([
      { t: 60, batches: 2, weiSpent: 1_000_000_000_000n },
      { t: 120, batches: 0, weiSpent: 0n },
    ]);
  });
  it("attaches each L2 bucket exactly once however many reports fall into it", () => {
    // Two reports at seconds 0 and 12 of the same 15 s bucket, users paid 0.25 ETH in it.
    const fees = resampleFees([point({ t: 3, feesWei: "250000000000000000" })], 15);
    const rows = joinCosts(
      [
        { t: 0, batches: 1, weiSpent: "6000000000000" },
        { t: 12, batches: 1, weiSpent: "4000000000000" },
        { t: 30, batches: 1, weiSpent: "0" },
      ],
      fees,
      15,
    );
    expect(rows).toEqual([
      { t: 0, l1Eth: 0.00001, l2Eth: 0.25, batches: 2 },
      { t: 30, l1Eth: 0, l2Eth: 0, batches: 1 },
    ]);
    expect(rows.reduce((s, r) => s + r.l2Eth, 0)).toBe(0.25);
  });
  it("keeps sub-microether batch costs", () => {
    const rows = joinCosts([{ t: 0, batches: 1, weiSpent: "500000000000" }], new Map(), 1);
    expect(rows[0].l1Eth).toBeCloseTo(5e-7, 12);
  });
});

describe("curves and domains", () => {
  it("builds the Taylor comparison and log domains", () => {
    const curve = taylorCurve((x) => 1 + x, Math.exp, 1, 0.5);
    expect(curve.map((p) => p.x)).toEqual([0, 0.5, 1]);
    expect(curve[2].exp).toBeCloseTo(Math.E);
    expect(logDomain([0.02, 0.39, 5.1])).toEqual([0.01, 10]);
    expect(logDomain([0.5], 0.02)).toEqual([0.01, 1]);
    expect(logDomain([0, -1, Number.NaN])).toEqual([0.001, 1]);
  });
});

describe("shape-aware set resolution", () => {
  const genesis = { id: 1, effectiveBlock: 28, effectiveAt: "2026-04-30T20:37:23Z", source: "genesis" as const, constraints: [60e6, 41e6, 29e6, 20e6, 14e6, 10e6].map((target, i) => ({ target, window: [9, 52, 329, 2105, 13485, 86400][i], startingBacklog: 0 })) };
  const point = { t: 1, blocks: 1, gasUsed: 1, gasPerSecond: 1, coverage: 1, completeness: "complete" as const, feesWei: "0", baseFeeMin: "20000000", baseFeeAvg: "20000000", baseFeeMax: "20000000", exponentBips: 31313, constraintBips: [0, 31313], backlogs: [6042415, 10822088492758], backlogsMax: [6042415, 10822088492758], minBaseFee: "20000000", floorFeesWei: "0", surplusFeesWei: "0", constraintSetId: 1, replayErrorBips: 0 };
  it("treats a set whose constraint count differs from the point's data as unknown", () => {
    const series = { range: "1h" as const, resolution: "block" as const, from: 0, to: 0, constraintSets: [genesis], ownerActions: [], points: [point] };
    expect(hasUnknownSets(series, "constraints")).toBe(true);
    const rows = buildChartPoints(series, "constraints");
    expect(rows[0].setKnown).toBe(false);
    expect(rows[0].cUnknown).toBeCloseTo(3.1313, 4);
    // The six-constraint set matches no point in the range, so it is not
    // drawable and offers no slots: the switcher would otherwise have shown
    // C1 to C6 with empty charts behind C3 to C6.
    expect(segmentsFor(series, "constraints").map((s) => s.setId)).toEqual([]);
    expect(seriesCount(series, "constraints")).toBe(2);
  });
  it("offers no constraint slot the data cannot fill", () => {
    // The api returns the six-constraint genesis set while the points carry a
    // two-slot live shape, which the owner-action scan has not caught up with.
    // The switcher used to offer C1 to C6 with empty charts behind C3 to C6.
    const series = { range: "1h" as const, resolution: "block" as const, from: 0, to: 0, constraintSets: [genesis], ownerActions: [], points: [point] };
    expect(seriesCount(series, "constraints")).toBe(2);
    expect(slotLabel(series, 0, "constraints")).toBe(unknownSlotLabel(0));
    // The unmatched data is drawn as the unknown split, under the unlabelled slots.
    const rows = buildChartPoints(series, "constraints");
    expect(rows[0].splitKnown).toBe(false);
    expect(rows[0][unknownBacklogKey(0)]).toBe(6042415);
    expect(rows[0][unknownBacklogKey(1)]).toBe(10822088492758);
    // With no points at all there is nothing to contradict the set.
    expect(seriesCount({ ...series, points: [] }, "constraints")).toBe(6);
  });
  it("keeps a set whose shape matches", () => {
    const current = { ...genesis, id: 6, effectiveBlock: 53_578_754, constraints: genesis.constraints.slice(0, 2) };
    const series = { range: "1h" as const, resolution: "block" as const, from: 0, to: 0, constraintSets: [genesis, current], ownerActions: [], points: [{ ...point, constraintSetId: 6 }] };
    expect(hasUnknownSets(series, "constraints")).toBe(false);
    expect(segmentsFor(series, "constraints").map((s) => s.setId)).toEqual([6, 6]);
    expect(buildChartPoints(series, "constraints")[0].setKnown).toBe(true);
  });
});

describe("gauges", () => {
  it("caps the marks at a small constant whatever the scale", () => {
    expect(gaugeMarks(1)).toEqual([]);
    expect(gaugeMarks(2)).toEqual([0.5]);
    expect(gaugeMarks(4)).toEqual([0.25, 0.5, 0.75]);
    expect(gaugeMarks(MAX_GAUGE_MARKS + 1)).toHaveLength(MAX_GAUGE_MARKS);
    expect(gaugeMarks(MAX_GAUGE_MARKS + 2)).toEqual(Array.from({ length: 12 }, (_, i) => ((i + 1) * 2) / 26));
    const billion = gaugeMarks(1_000_000_000);
    expect(billion.length).toBeGreaterThan(0);
    expect(billion.length).toBeLessThanOrEqual(MAX_GAUGE_MARKS);
    expect(billion.every((m) => m > 0 && m < 1)).toBe(true);
    expect(gaugeMarks(Number.POSITIVE_INFINITY)).toEqual([]);
    expect(gaugeMarks(Number.NaN)).toEqual([]);
    expect(gaugeMarks(0)).toEqual([]);
  });
  it("takes ratios on integers, exactly below 2^53 and in BigInt above it", () => {
    expect(bigFraction(3n, 4n)).toBe(0.75);
    expect(bigFraction(1n, 0n)).toBe(0);
    expect(bigFraction(0n, 4n)).toBe(0);
    expect(bigFraction(-1n, 2n)).toBe(0);
    // Two uint64-scale integers whose ratio a double could not hold apart.
    const max = (1n << 64n) - 1n;
    expect(bigFraction(max / 4n, max)).toBeCloseTo(0.25, 9);
    expect(bigFraction(max, max)).toBe(1);
  });
  it("lays out a constraint gauge in whole windows of target", () => {
    // Target 1, window 1, backlog a billion: a billion windows, but only a handful of marks.
    const huge = constraintGauge({ target: 1, window: 1 }, 1_000_000_000);
    expect(huge.scale).toBe(1_000_000_000);
    expect(huge.fraction).toBe(1);
    expect(huge.marks.length).toBeLessThanOrEqual(MAX_GAUGE_MARKS);
    const two = constraintGauge({ target: 60_000_000, window: 15 }, 1_800_000_000);
    expect(two).toEqual({ scale: 2, fraction: 1, marks: [0.5], denominator: 900_000_000 });
    expect(constraintGauge({ target: 60_000_000, window: 15 }, 450_000_000).fraction).toBe(0.5);
    expect(constraintGauge({ target: 0, window: 15 }, 5)).toEqual({ scale: 1, fraction: 0, marks: [], denominator: 0 });
    expect(constraintGauge({ target: 60_000_000, window: 15 }, 0)).toEqual({ scale: 1, fraction: 0, marks: [], denominator: 900_000_000 });
  });
  it("takes the window of target from the pricer, saturating as nitro does", () => {
    // target × window overflows uint64: the pricer's divisor saturates at
    // MaxUint64 and the gauge spans that, not an unbounded float product.
    const target = 2 ** 63;
    const maxInt64 = (1n << 63n) - 1n;
    const gauge = constraintGauge({ target, window: 4 }, target);
    // Unbounded float arithmetic would have made this 3.7e19; the pricer's
    // divisor saturates at MaxUint64 and is cast into int64 bips.
    expect(target * 4).toBeGreaterThan(Number(maxInt64));
    expect(gauge.denominator).toBe(Number(maxInt64));
    expect(gauge.fraction).toBeCloseTo(0.5, 6);
    // The same numbers through the pricer: that saturated divisor is the one it divides by.
    expect(constraintExponentBips({ target: BigInt(target), window: 4n, backlog: BigInt(target) })).toBe(naturalToBips(BigInt(target)) / saturatingCastToBips(saturatingUMul(4n, BigInt(target))));
  });
  it("lays out the legacy gauge, including zero tolerance", () => {
    expect(legacyGauge({ speedLimit: 7_000_000, inertia: 102, tolerance: 10 }, 90_000_000)).toEqual({ free: 70_000_000, unit: 714_000_000, span: 210_000_000, fraction: 90 / 210, marks: [1 / 3] });
    expect(legacyGauge({ speedLimit: 7_000_000, inertia: 102, tolerance: 10 }, 0)).toEqual({ free: 70_000_000, unit: 714_000_000, span: 140_000_000, fraction: 0, marks: [0.5] });
    // Zero tolerance: no free region, so the gauge spans whole units of x instead.
    const zero = legacyGauge({ speedLimit: 7_000_000, inertia: 102, tolerance: 0 }, 1_000_000_000);
    expect(zero).toEqual({ free: 0, unit: 714_000_000, span: 1_428_000_000, fraction: 1_000_000_000 / 1_428_000_000, marks: [0.5] });
    expect(Number.isFinite(zero.fraction)).toBe(true);
    expect(legacyGauge({ speedLimit: 7_000_000, inertia: 102, tolerance: 0 }, 0)).toEqual({ free: 0, unit: 714_000_000, span: 714_000_000, fraction: 0, marks: [] });
    // Zero inertia or speed limit: nothing to scale by, and no NaN anywhere.
    expect(legacyGauge({ speedLimit: 7_000_000, inertia: 0, tolerance: 0 }, 5)).toEqual({ free: 0, unit: 0, span: 0, fraction: 0, marks: [] });
    expect(legacyGauge({ speedLimit: 0, inertia: 102, tolerance: 10 }, 5)).toEqual({ free: 0, unit: 0, span: 0, fraction: 0, marks: [] });
    expect(legacyGauge({ speedLimit: 1, inertia: 1, tolerance: 0 }, 1_000_000_000).marks.length).toBeLessThanOrEqual(MAX_GAUGE_MARKS);
  });
  it("wraps the legacy tolerance threshold exactly as the pricer does, so a wrapped threshold shows no free gas", () => {
    // tolerance × speedLimit wraps to zero in uint64: the pricer charges from
    // the first unit of gas, so the gauge must not promise a free region.
    const speedLimit = 2 ** 63;
    const legacy = { speedLimit, inertia: 102, tolerance: 2 };
    const state = toLegacyState({ ...legacy, backlog: 1_000_000_000 });
    expect(BigInt.asUintN(64, state.tolerance * state.speedLimit)).toBe(0n);
    // Float arithmetic would have shown a free region of 1.8e19 gas where the
    // pricer has none: every unit of gas above zero is priced.
    expect(legacy.tolerance * legacy.speedLimit).toBeGreaterThan(1e19);
    expect(legacyExponentBips({ ...state, backlog: 0n })).toBe(0n);
    expect(legacyExponentBips({ ...state, backlog: (1n << 63n) })).toBeGreaterThan(0n);
    const gauge = legacyGauge(legacy, 1_000_000_000);
    expect(gauge.free).toBe(0);
    // The unit of x saturates into int64 bips, as the pricer's denominator does.
    expect(gauge.unit).toBe(Number(saturatingCastToBips(saturatingUMul(102n, BigInt(speedLimit)))));
    expect(gauge.fraction).toBeGreaterThan(0);
    expect(gauge.fraction).toBeLessThanOrEqual(1);
  });

  it("says what a gauge's far end is in words, and what one step of it is worth", () => {
    // Robinhood's two constraints: 60 Mgas/s over 15 s, and 40 Mgas/s over a day.
    const short = constraintGauge({ target: 60_000_000, window: 15 }, 450_000_000);
    expect(constraintGaugeSpanLabel(short)).toBe("1 window of target (900 Mgas)");
    // The equation is the constraint's own two parameters multiplied out, so
    // the reader can check it against the target and window on the card.
    expect(constraintGaugeNote({ target: 60_000_000, window: 15 }, short).lines[0]).toBe("1 window of target = 60 Mgas/s × 15 s = 900 Mgas");
    const long = constraintGauge({ target: 40_000_000, window: 86_400 }, 11_194_391_810_886);
    expect(constraintGaugeSpanLabel(long)).toBe("4 windows of target (13.8 Tgas)");
    const note = constraintGaugeNote({ target: 40_000_000, window: 86_400 }, long);
    expect(note.lines).toEqual([
      "1 window of target = 40 Mgas/s × 24 h = 3.46 Tgas",
      "The gas the chain uses in one whole window at exactly the target rate. A backlog of one window adds exactly 1.0 to x.",
      "The bar spans whole windows, so each mark is one more unit of x and the far end moves out as the backlog crosses one.",
    ]);
    // A reader who gets no panel gets the label back with the same facts after it.
    expect(note.description).toBe(
      "4 windows of target (13.8 Tgas). 1 window of target = 40 Mgas/s × 24 h = 3.46 Tgas. The gas the chain uses in one whole window at exactly the target rate. A backlog of one window adds exactly 1.0 to x. The bar spans whole windows, so each mark is one more unit of x and the far end moves out as the backlog crosses one.",
    );
    // Nothing to divide by: the note says so rather than defining a zero window.
    const none = constraintGauge({ target: 0, window: 15 }, 5);
    expect(constraintGaugeNote({ target: 0, window: 15 }, none).lines).toEqual(["no scale: the target or the window is zero"]);
    expect(constraintGaugeNote({ target: 0, window: 15 }, none).description).toBe("1 window of target (0 gas). no scale: the target or the window is zero.");
    // The plural follows the count, and the gas is the whole span.
    expect(gaugeSpanLabel(1, 900_000_000, "window of target", "windows of target")).toBe("1 window of target (900 Mgas)");
    expect(gaugeSpanLabel(3, 900_000_000, "window of target", "windows of target")).toBe("3 windows of target (2.7 Ggas)");
  });

  it("says the same of the legacy gauge, in the legacy pricer's own terms", () => {
    const tolerance = legacyGauge({ speedLimit: 7_000_000, inertia: 102, tolerance: 10 }, 90_000_000);
    expect(legacyGaugeSpanLabel(tolerance)).toBe("3 tolerance thresholds (210 Mgas)");
    expect(legacyGaugeNote(tolerance).lines[0]).toBe("1 tolerance threshold = tolerance × speed limit = 70 Mgas");
    expect(legacyGaugeNote(tolerance).lines[1]).toMatch(/charges nothing at all/);
    expect(legacyGaugeNote(tolerance).description).toMatch(/^3 tolerance thresholds \(210 Mgas\)\. 1 tolerance threshold = /);
    const zero = legacyGauge({ speedLimit: 7_000_000, inertia: 102, tolerance: 0 }, 1_000_000_000);
    expect(legacyGaugeSpanLabel(zero)).toBe("2 units of x (1.43 Ggas)");
    expect(legacyGaugeNote(zero).lines[0]).toBe("1 unit of x = inertia × speed limit = 714 Mgas");
    expect(legacyGaugeNote(zero).lines[1]).toMatch(/no free region/);
    const nothing = legacyGauge({ speedLimit: 0, inertia: 0, tolerance: 0 }, 5);
    expect(legacyGaugeSpanLabel(nothing)).toBe("no scale (zero inertia or speed limit)");
    expect(legacyGaugeNote(nothing).lines).toEqual(["no scale: the inertia or the speed limit is zero"]);
  });
});
