import { describe, expect, it } from "vitest";
import type { OwnerAction, Series, SeriesPoint } from "@/types";
import {
  actionsInBucket,
  bucketNote,
  DEFAULT_BUCKET_SECONDS,
  describeAction,
  feeChartCaption,
  feeChartData,
  feeChartLabel,
  feeDomain,
  feeTooltipRows,
  formatFloor,
  markersFor,
  ownerActionNote,
} from "./feeChart";
import { isGapRow, NO_GAPS } from "@/lib/gaps";

function point(overrides: Partial<SeriesPoint>): SeriesPoint {
  return {
    t: 0,
    blocks: 1,
    gasUsed: 0,
    gasPerSecond: 0,
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
    constraintSetId: 0,
    replayErrorBips: 0,
    ...overrides,
  };
}

const setFloor: OwnerAction = { block: 20, at: "2026-09-06T07:21:00Z", txHash: "0x" + "ab".repeat(32), method: "setMinimumL2BaseFee", selector: "0xa0188cdb", args: { priceInWei: "20000000" } };
const setConstraints: OwnerAction = {
  block: 21,
  at: "2026-09-06T07:22:00Z",
  txHash: "0x" + "cd".repeat(32),
  method: "setGasPricingConstraints",
  selector: "0xcc0d556a",
  args: { constraints: [[60_000_000, 15, 0], "oops"] },
};

const series: Series = {
  range: "24h",
  resolution: "1m",
  from: 1788679200,
  to: 1788679320,
  constraintSets: [
    { id: 5, effectiveBlock: 10, effectiveAt: "2026-09-01T16:33:00Z", source: "owner_action", constraints: [{ target: 60_000_000, window: 15, startingBacklog: 0 }, { target: 30_000_000, window: 86_400, startingBacklog: 0 }] },
    { id: 6, effectiveBlock: 20, effectiveAt: "2026-09-03T17:08:00Z", source: "owner_action", constraints: [{ target: 60_000_000, window: 15, startingBacklog: 0 }, { target: 40_000_000, window: 86_400, startingBacklog: 0 }] },
  ],
  ownerActions: [setFloor],
  points: [
    point({ t: 1788679200, blocks: 12, baseFeeMin: "100000000", baseFeeAvg: "300000000", baseFeeMax: "400000000", minBaseFee: "100000000", exponentBips: 10_000, constraintBips: [4_000, 6_000], backlogs: [1, 2], backlogsMax: [1, 2], constraintSetId: 5 }),
    point({ t: 1788679260, blocks: 30, baseFeeMin: "20000000", baseFeeAvg: "395726000", baseFeeMax: "400000000", minBaseFee: "20000000", exponentBips: 32_425, constraintBips: [34, 32_391], backlogs: [3_111_506, 11_194_391_810_886], backlogsMax: [3_111_506, 11_194_391_810_886], constraintSetId: 6 }),
  ],
};

describe("feeChartData", () => {
  it("maps a series to the rows, markers, domain and span the chart draws", () => {
    const data = feeChartData(series, "constraints");
    expect(data.points).toHaveLength(2);
    expect(data.bucketSeconds).toBe(60);
    expect(data.span).toBe(120);
    expect(data.markers.map((m) => m.label)).toEqual(["setMinimumL2BaseFee"]);
    expect(data.markers[0].t).toBe(Math.floor(Date.parse("2026-09-06T07:21:00Z") / 1000));
    expect(data.fidelityBands).toEqual([]);
    // The set in force changes between the two buckets, so the drawn rows
    // carry the extra edge that keeps the replacement vertical.
    expect(data.drawn).toHaveLength(3);
    expect(data.drawn[1].boundary).toBe(true);
    // A log domain that holds the whole band, 0.02 to 0.4 gwei, on clean decades.
    expect(data.domain).toEqual([0.01, 1]);
  });

  it("carries fidelity bands for every chart built from the fee data", () => {
    const marked = { ...series, points: [{ ...series.points[0], replayFidelity: "unverified" as const }, series.points[1]] };
    const data = feeChartData(marked, "constraints");
    expect(data.fidelityBands).toEqual([{ from: series.points[0].t, to: series.points[0].t + 60, kind: "unverified" }]);
  });

  it("draws nothing at all without a series, and still names a bucket width", () => {
    const data = feeChartData(null, "constraints");
    expect(data).toEqual({ points: [], drawn: [], markers: [], domain: [0.001, 1], span: 0, bucketSeconds: DEFAULT_BUCKET_SECONDS, fidelityBands: [], gaps: NO_GAPS });
  });

  it("measures a single bucket by the fallback width rather than by nothing", () => {
    const data = feeChartData({ ...series, points: [series.points[0]], from: series.points[0].t, to: series.points[0].t + 60 }, "constraints");
    expect(data.bucketSeconds).toBe(DEFAULT_BUCKET_SECONDS);
    // The axis is the window the range asked for, not the one bucket in it.
    expect(data.span).toBe(60);
  });

  it("keeps the whole band inside the log domain", () => {
    expect(feeDomain([])).toEqual([0.001, 1]);
    expect(feeDomain(feeChartData(series, "constraints").points)).toEqual([0.01, 1]);
  });
});

describe("a range with holes in it", () => {
  it("keeps the window as the axis and shades what was never indexed", () => {
    // A day was asked for, two buckets came back: the rest is not history that
    // was flat, it is history that does not exist.
    const first = series.points[0].t;
    const data = feeChartData({ ...series, from: first - 86_400, to: first + 120 }, "constraints");
    expect(data.gaps.window).toEqual({ from: first - 86_400, to: first + 120 });
    expect(data.gaps.gaps).toEqual([{ from: first - 86_400, to: first, kind: "leading" }]);
    expect(data.gaps.first).toBe(first);
    expect(data.span).toBe(86_520);
    // A leading span needs no break row: the line simply starts where the data does.
    expect(data.drawn.some((row) => isGapRow(row as Record<string, unknown>))).toBe(false);
  });

  it("breaks the line over a missing bucket rather than bridging it", () => {
    const [a, b] = series.points;
    const late = { ...b, t: b.t + 600 };
    const data = feeChartData({ ...series, points: [a, late], from: a.t, to: late.t + 60 }, "constraints");
    expect(data.gaps.gaps).toEqual([{ from: a.t + 60, to: late.t, kind: "interior" }]);
    const breaks = data.drawn.filter((row) => isGapRow(row as Record<string, unknown>));
    expect(breaks).toHaveLength(1);
    expect(breaks[0].t).toBe(a.t + 60);
    // Nothing on the break row carries a value: a zero would be a bucket that
    // collected nothing, which is a different fact.
    const row = breaks[0] as Record<string, unknown>;
    expect(row.feeAvg).toBeNull();
    expect(Object.entries(row).filter(([key]) => key !== "t" && key !== "gapRow").every(([, value]) => value === null)).toBe(true);
  });
});

describe("owner actions on the fee chart", () => {
  it("places every action on the axis at the second it landed", () => {
    expect(markersFor({ ownerActions: [] })).toEqual([]);
    expect(markersFor(series)).toHaveLength(1);
  });

  it("finds the actions inside one bucket and leaves the neighbours alone", () => {
    const markers = markersFor({ ownerActions: [setFloor, setConstraints] });
    const bucket = markers[0].t;
    expect(actionsInBucket(markers, bucket, 60).map((m) => m.action.block)).toEqual([20]);
    expect(actionsInBucket(markers, bucket, 120).map((m) => m.action.block)).toEqual([20, 21]);
    expect(actionsInBucket(markers, bucket - 600, 60)).toEqual([]);
  });

  it("says what each kind of action did, and passes an unknown one through by name", () => {
    expect(describeAction(setFloor)).toBe("setMinimumL2BaseFee: 0.02 gwei");
    // The constraint tuple reads in the unit the rest of the page uses, and an
    // argument that is not a tuple is shown as it arrived rather than dropped.
    expect(describeAction(setConstraints)).toBe("setGasPricingConstraints: 60 Mgas/s · 15 s, oops");
    expect(describeAction({ ...setFloor, args: {} })).toBe("setMinimumL2BaseFee");
    expect(describeAction({ ...setConstraints, args: {} })).toBe("setGasPricingConstraints");
    expect(describeAction({ ...setFloor, method: "unknown", args: {} })).toBe("unknown");
  });

  it("reads the constraint objects the api actually sends, not just the tuples", () => {
    // The contract decodes setGasPricingConstraints into objects. They used to
    // stringify as "[object Object]" on every real marker.
    const decoded: OwnerAction = {
      ...setConstraints,
      args: {
        constraints: [
          { gasTargetPerSecond: 60_000_000, adjustmentWindowSeconds: 15, startingBacklog: 0 },
          { gasTargetPerSecond: 40_000_000, adjustmentWindowSeconds: 86_400, startingBacklog: 11_194_391_810_886 },
        ],
      },
    };
    expect(describeAction(decoded)).toBe("setGasPricingConstraints: 60 Mgas/s · 15 s, 40 Mgas/s · 24 h");
    expect(describeAction(decoded)).not.toContain("[object Object]");
  });

  it("footnotes a hovered bucket with the actions inside it, and says nothing when there are none", () => {
    const markers = markersFor({ ownerActions: [setFloor] });
    const note = ownerActionNote(markers, 60);
    expect(note({ t: markers[0].t })).toBe("Owner action at block 20: setMinimumL2BaseFee: 0.02 gwei");
    expect(note({ t: markers[0].t + 600 })).toBeNull();
  });

  it("puts what the collector has of the bucket in front of the actions in it", () => {
    const markers = markersFor({ ownerActions: [setFloor] });
    const note = bucketNote(markers, 60);
    expect(note({ t: markers[0].t, partial: "in-progress", coverage: 0.5 })).toBe("bucket in progress, 50% elapsed · Owner action at block 20: setMinimumL2BaseFee: 0.02 gwei");
    expect(note({ t: markers[0].t + 600, partial: "leading", coverage: 0.25 })).toBe("partially indexed, 25% of the bucket");
    // A chart that draws sums leaves the bucket out, and says so.
    expect(bucketNote([], 60, true)({ t: 0, partial: "in-progress", coverage: 0.5 })).toBe("bucket in progress, 50% elapsed; not drawn as a bucket total");
    expect(note({ t: markers[0].t + 600, partial: null, coverage: 1 })).toBeNull();
  });
});

describe("the bucket width a range is read at", () => {
  it("comes from the resolution, so the owner-action buckets do not depend on point spacing", () => {
    expect(feeChartData(series, "constraints").bucketSeconds).toBe(60);
    // Two per-block points at the same second used to give a zero-width
    // bucket, which put no action in any bucket at all.
    const perBlock: Series = { ...series, resolution: "block", points: [point({ t: 1788679200 }), point({ t: 1788679200 })] };
    expect(feeChartData(perBlock, "constraints").bucketSeconds).toBe(1);
    // One bucket in a 15-minute range used to read as a minute.
    const singleton: Series = { ...series, resolution: "15m", points: [point({ t: 1788679200 })] };
    expect(feeChartData(singleton, "constraints").bucketSeconds).toBe(900);
    // A hole after the first point used to inflate the bucket to its size.
    const holed: Series = { ...series, resolution: "1m", points: [point({ t: 1788679200 }), point({ t: 1788679200 + 600 })] };
    expect(feeChartData(holed, "constraints").bucketSeconds).toBe(60);
    expect(feeChartData(null, "constraints").bucketSeconds).toBe(DEFAULT_BUCKET_SECONDS);
  });
});

describe("a bucket whose floor was never recorded", () => {
  // Pricing version 0 history: the contract makes minBaseFee nullable, and a
  // null floor used to reach `.trim()` and take the whole chart down.
  const nullFloor: Series = { ...series, points: series.points.map((p, i) => (i === 0 ? { ...p, minBaseFee: null, floorFeesWei: null, surplusFeesWei: null } : p)) };

  it("draws no floor for it rather than throwing", () => {
    const data = feeChartData(nullFloor, "constraints");
    expect(data.points[0].floor).toBeNull();
    expect(data.points[1].floor).toBeCloseTo(0.02, 9);
  });

  it("leaves it out of the log domain, so a missing floor never pulls the axis", () => {
    const data = feeChartData(nullFloor, "constraints");
    // The same domain the range has with that bucket's floor simply absent.
    expect(feeDomain(data.points)).toEqual(feeDomain(data.points.map((p) => ({ ...p, floor: p.floor }))));
    expect(feeDomain([{ ...data.points[0], feeMin: 0.4, feeMax: 0.4, floor: null }])).toEqual([0.1, 1]);
  });

  it("reads out as n/a in a tooltip and a table, never as a floor of zero", () => {
    const data = feeChartData(nullFloor, "constraints");
    const floorRow = feeTooltipRows()[2];
    expect(floorRow.value(data.points[0] as unknown as Record<string, unknown>)).toBe("n/a");
    expect(floorRow.value(data.points[1] as unknown as Record<string, unknown>)).toBe("0.02 gwei");
    expect(formatFloor(null)).toBe("n/a");
    expect(formatFloor(0.02)).toBe("0.02");
  });
});

describe("the fee chart's readouts", () => {
  it("reads a hovered bucket out as the average, the band, the floor, x and the blocks", () => {
    const [, row] = feeChartData(series, "constraints").points;
    const rows = feeTooltipRows();
    expect(rows.map((r) => r.label)).toEqual(["base fee, average", "min to max in bucket", "floor in force", "x", "blocks"]);
    expect(rows.map((r) => r.value(row as unknown as Record<string, unknown>))).toEqual(["0.3957 gwei", "0.02 to 0.4 gwei", "0.02 gwei", "3.2425", "30"]);
  });

  it("captions the chart with the range and how many buckets are in it", () => {
    const { points } = feeChartData(series, "constraints");
    expect(feeChartCaption("24h", points)).toBe("Base fee average with the min to max band · 24h · 2 buckets");
  });

  it("describes the chart for a reader who cannot see it, empty range included", () => {
    const { points } = feeChartData(series, "constraints");
    expect(feeChartLabel("24h", points)).toBe("Base fee over 24h on a log scale, 2 buckets, 0.02 to 0.4 gwei, with the floor in force stepped under it");
    expect(feeChartLabel("30d", [])).toBe("Base fee over 30d, no buckets yet");
  });
});
