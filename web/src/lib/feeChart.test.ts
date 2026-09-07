import { describe, expect, it } from "vitest";
import type { OwnerAction, Series, SeriesPoint } from "@/types";
import {
  actionsInBucket,
  DEFAULT_BUCKET_SECONDS,
  describeAction,
  feeChartCaption,
  feeChartData,
  feeChartLabel,
  feeDomain,
  feeTooltipRows,
  markersFor,
  ownerActionNote,
} from "./feeChart";

function point(overrides: Partial<SeriesPoint>): SeriesPoint {
  return {
    t: 0,
    blocks: 1,
    gasUsed: 0,
    gasPerSecond: 0,
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
    // The set in force changes between the two buckets, so the drawn rows
    // carry the extra edge that keeps the replacement vertical.
    expect(data.drawn).toHaveLength(3);
    expect(data.drawn[1].boundary).toBe(true);
    // A log domain that holds the whole band, 0.02 to 0.4 gwei, on clean decades.
    expect(data.domain).toEqual([0.01, 1]);
  });

  it("draws nothing at all without a series, and still names a bucket width", () => {
    const data = feeChartData(null, "constraints");
    expect(data).toEqual({ points: [], drawn: [], markers: [], domain: [0.001, 1], span: 0, bucketSeconds: DEFAULT_BUCKET_SECONDS });
  });

  it("measures a single bucket by the fallback width rather than by nothing", () => {
    const data = feeChartData({ ...series, points: [series.points[0]] }, "constraints");
    expect(data.bucketSeconds).toBe(DEFAULT_BUCKET_SECONDS);
    expect(data.span).toBe(1);
  });

  it("keeps the whole band inside the log domain", () => {
    expect(feeDomain([])).toEqual([0.001, 1]);
    expect(feeDomain(feeChartData(series, "constraints").points)).toEqual([0.01, 1]);
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

  it("footnotes a hovered bucket with the actions inside it, and says nothing when there are none", () => {
    const markers = markersFor({ ownerActions: [setFloor] });
    const note = ownerActionNote(markers, 60);
    expect(note({ t: markers[0].t })).toBe("Owner action at block 20: setMinimumL2BaseFee: 0.02 gwei");
    expect(note({ t: markers[0].t + 600 })).toBeNull();
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
