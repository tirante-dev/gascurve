import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { BatchSeries, L1Series, LiveSnapshot, Series, SeriesPoint } from "@/types";
import { buildChartPoints, segmentsFor } from "@/utils/chart";

vi.mock("recharts", async (importOriginal) => {
  const original = await importOriginal<typeof import("recharts")>();
  return { ...original, ResponsiveContainer: ({ children }: { children: React.ReactNode }) => <div style={{ width: 400, height: 100 }}>{children}</div> };
});

const getBatchesMock = vi.fn();
const getL1Mock = vi.fn();
vi.mock("@/lib/api/batches", () => ({ getBatches: (...args: unknown[]) => getBatchesMock(...args) }));
vi.mock("@/lib/api/l1", () => ({ getL1: (...args: unknown[]) => getL1Mock(...args) }));

import { ChartTooltip } from "./ChartTooltip";
import { cardValues, ConstraintCards } from "./ConstraintCards";
import { DataFooter } from "./DataFooter";
import { FeeFlows, feeTotals } from "./FeeFlows";
import { L1Section } from "./L1Section";
import { PricerEquation } from "./PricerEquation";
import { describeSplit, SeriesCharts } from "./SeriesCharts";

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

const series: Series = {
  range: "24h",
  resolution: "1m",
  constraintSets: [
    { id: 5, effectiveBlock: 10, effectiveAt: "2026-09-01T16:33:00Z", source: "owner_action", constraints: [{ target: 60_000_000, window: 15, startingBacklog: 0 }, { target: 30_000_000, window: 86_400, startingBacklog: 0 }] },
    { id: 6, effectiveBlock: 20, effectiveAt: "2026-09-03T17:08:00Z", source: "owner_action", constraints: [{ target: 60_000_000, window: 15, startingBacklog: 0 }, { target: 40_000_000, window: 86_400, startingBacklog: 0 }] },
  ],
  ownerActions: [{ block: 20, at: "2026-09-06T07:21:00Z", txHash: "0x" + "ab".repeat(32), method: "setMinimumL2BaseFee", selector: "0xa0188cdb", args: { priceInWei: "20000000" } }],
  points: [
    point({ t: 1788679200, gasUsed: 100, feesWei: "3000000000000000000", floorFeesWei: "1000000000000000000", surplusFeesWei: "2000000000000000000", baseFeeMin: "100000000", baseFeeAvg: "300000000", baseFeeMax: "400000000", minBaseFee: "100000000", exponentBips: 10_000, constraintBips: [4_000, 6_000], backlogs: [1, 2], backlogsMax: [1, 2], constraintSetId: 5 }),
    point({ t: 1788679260, gasUsed: 100, feesWei: "1000000000000000000", floorFeesWei: "200000000000000000", surplusFeesWei: "800000000000000000", baseFeeMin: "20000000", baseFeeAvg: "395726000", baseFeeMax: "400000000", minBaseFee: "20000000", exponentBips: 32_425, constraintBips: [34, 32_391], backlogs: [3_111_506, 11_194_391_810_886], backlogsMax: [3_111_506, 11_194_391_810_886], constraintSetId: 6 }),
    point({ t: 1788679320, gasUsed: 100, feesWei: "1000000000000000000", floorFeesWei: "1000000000000000000", surplusFeesWei: "0", baseFeeMin: "20000000", baseFeeAvg: "20000000", baseFeeMax: "20000000", minBaseFee: "20000000", exponentBips: 5_000, constraintBips: [5_000, 0], backlogs: [1, 1], backlogsMax: [1, 1], constraintSetId: 99 }),
  ],
};

const snapshot: LiveSnapshot = {
  chainId: 4663,
  sampledAt: "2026-09-06T07:20:00Z",
  block: { number: 55_812_345, ts: 1788679199, gasUsed: 4_021_130, baseFee: "399726000", txCount: 90 },
  baseFee: "399726000",
  minBaseFee: "20000000",
  multiplierBips: 199_863,
  exponentBips: 32_425,
  model: "constraints",
  constraints: [
    { target: 60_000_000, window: 15, backlog: 30_000_000, exponentBips: 333 },
    { target: 40_000_000, window: 86_400, backlog: 11_194_391_810_886, exponentBips: 32_391 },
  ],
  prices: { perL2Tx: "0", perL1CalldataByte: "0", perL2Storage: "0", perArbGasBase: "20000000", perArbGasCongestion: "379726000", perArbGasTotal: "399726000" },
  gasPerSecond: { s10: 38_000_000, s60: 40_500_000 },
  replayErrorBips: 2,
};

describe("SeriesCharts", () => {
  it("draws one series per constraint set with set-aware legends and an explicit unknown-split series", () => {
    render(<SeriesCharts series={series} loading={false} />);
    expect(screen.getAllByText("C2 · 30M/s · 24 h · set 5 (from block 10)").length).toBeGreaterThan(0);
    expect(screen.getAllByText("C2 · 40M/s · 24 h · set 6 (from block 20)").length).toBeGreaterThan(0);
    expect(screen.getAllByText("unknown split (total x, set not in range)").length).toBeGreaterThan(0);
    expect(screen.getByText("floor in force (stepped)")).toBeInTheDocument();
    expect(screen.getByText("target C2 (stepped, per set)")).toBeInTheDocument();
    expect(screen.getByText("C2 · 30M/s · 24 h (set 5) then 40M/s · 24 h (set 6)")).toBeInTheDocument();
    // The owner action is listed with a zoned timestamp.
    expect(screen.getByText("2026-09-06 02:21 CDT")).toBeInTheDocument();
  });

  it("exposes every bucket in a table and lets the keyboard inspect any point", () => {
    render(<SeriesCharts series={series} loading={false} />);
    const summary = screen.getByText(/Data table \(3 buckets/);
    const details = summary.closest("details") as HTMLDetailsElement;
    expect(within(details).queryByRole("table")).toBeNull();
    details.open = true;
    fireEvent(details, new Event("toggle"));
    const table = within(details).getByRole("table");
    expect(within(table).getAllByRole("row")).toHaveLength(4);
    expect(within(table).getByText("C1 0.0034 · C2 3.2391 (set 6)")).toBeInTheDocument();
    expect(within(table).getByText("unknown split (total x, set not in range): 0.5000 (unknown set)")).toBeInTheDocument();
    expect(within(table).getByText("11.2T")).toBeInTheDocument();
    expect(within(table).getAllByText("n/a").length).toBeGreaterThan(0);

    const slider = screen.getByRole("slider", { name: /Select a bucket/ });
    expect(slider).toHaveValue("2");
    fireEvent.change(slider, { target: { value: "1" } });
    expect(screen.getByText("2026-09-06 02:21 CDT", { selector: "label span" })).toBeInTheDocument();
    expect(screen.getAllByText("3.2425").length).toBeGreaterThanOrEqual(2);
    expect(screen.getByText("Owner action at block 20: setMinimumL2BaseFee: 0.02 gwei")).toBeInTheDocument();
    fireEvent.change(slider, { target: { value: "0" } });
    expect(screen.getByText("0.4000")).toBeInTheDocument();
    expect(screen.getByText("30M gas/s")).toBeInTheDocument();
    expect(screen.queryByText("40M gas/s")).toBeNull();
  });

  it("handles empty, loading and legacy series", () => {
    const { rerender } = render(<SeriesCharts series={null} loading />);
    expect(screen.getByText("Loading history.")).toBeInTheDocument();
    rerender(<SeriesCharts series={{ ...series, points: [] }} loading={false} />);
    expect(screen.getByText("No buckets in this range yet.")).toBeInTheDocument();
    rerender(<SeriesCharts series={{ ...series, constraintSets: [], ownerActions: [], points: [point({ t: 1, exponentBips: 1260, constraintBips: [1260], backlogs: [7], backlogsMax: [7] })] }} loading={false} />);
    expect(screen.getAllByText("legacy backlog").length).toBeGreaterThan(0);
  });

  it("describes a point's split under its own set", () => {
    const rows = buildChartPoints(series);
    const segments = segmentsFor(series);
    expect(describeSplit(rows[0], segments)).toBe("C1 0.4000 · C2 0.6000");
    expect(describeSplit(rows[2], segments)).toBe("unknown split (total x, set not in range): 0.5000");
  });
});

describe("ChartTooltip", () => {
  it("hides rows that do not apply to the hovered point", () => {
    const row = { t: 1788679200, constraintSetId: 6, a: 1 };
    render(
      <ChartTooltip
        active
        payload={[{ payload: row, value: 1, name: "a", dataKey: "a", graphicalItemId: "a" }]}
        label={1788679200}
        title={(t) => String(t)}
        rows={[
          { label: "shown", value: () => "1", when: (r) => r.constraintSetId === 6 },
          { label: "hidden", value: () => "2", when: (r) => r.constraintSetId === 5 },
          { label: "always", value: () => "3" },
        ]}
      />,
    );
    expect(screen.getByText("shown")).toBeInTheDocument();
    expect(screen.queryByText("hidden")).toBeNull();
    expect(screen.getByText("always")).toBeInTheDocument();
    expect(screen.getByText("1788679200")).toBeInTheDocument();
  });
  it("renders nothing while inactive", () => {
    const { container } = render(<ChartTooltip active={false} payload={[]} rows={[]} title={() => ""} />);
    expect(container).toBeEmptyDOMElement();
  });
});

describe("FeeFlows", () => {
  it("splits fees by the floor in force at each block, from the api's exact sums, not today's floor", () => {
    const totals = feeTotals(series);
    expect(totals.total).toBeCloseTo(5);
    expect(totals.floorEth).toBeCloseTo(2.2);
    expect(totals.surplusEth).toBeCloseTo(2.8);
    render(<FeeFlows snapshot={snapshot} series={series} />);
    expect(screen.getByText("2.2")).toBeInTheDocument();
    expect(screen.getByText("2.8")).toBeInTheDocument();
    const summary = screen.getByText(/Data table \(3 buckets\)/);
    const details = summary.closest("details") as HTMLDetailsElement;
    details.open = true;
    fireEvent(details, new Event("toggle"));
    expect(within(details).getAllByRole("row")).toHaveLength(4);
    expect(within(details).getByText("0.1")).toBeInTheDocument();
  });
  it("renders without a snapshot or history", () => {
    render(<FeeFlows snapshot={null} series={null} />);
    expect(screen.getByText("No history loaded.")).toBeInTheDocument();
    expect(screen.getByText(/none yet/)).toBeInTheDocument();
  });
});

describe("ConstraintCards", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("recomputes x, shares and colours from the animated backlogs and labels them as a projection", () => {
    const frames: FrameRequestCallback[] = [];
    vi.stubGlobal("requestAnimationFrame", (cb: FrameRequestCallback) => {
      frames.push(cb);
      return frames.length;
    });
    vi.stubGlobal("cancelAnimationFrame", vi.fn());
    vi.spyOn(performance, "now").mockImplementation(() => 1000);
    render(<ConstraintCards snapshot={snapshot} />);
    // At the sample: 333 bips for constraint 1, 1.0% of x.
    expect(screen.getByText("0.0333")).toBeInTheDocument();
    expect(screen.getByText("1.0% of x")).toBeInTheDocument();
    expect(screen.queryByText(/projected/)).toBeNull();
    // One second later the 30M backlog has drained at 60M/s: x1 is 0, share 0, and the card says so.
    act(() => frames[0](2000));
    expect(screen.getByText("0.0000")).toBeInTheDocument();
    expect(screen.getByText("no contribution")).toBeInTheDocument();
    expect(screen.getByText("100.0% of x")).toBeInTheDocument();
    expect(screen.getAllByText(/\(projected\)/).length).toBeGreaterThan(0);
    expect(screen.getByText(/Between samples the backlogs are a projection/)).toBeInTheDocument();
  });

  it("derives card values through integer bips", () => {
    const values = cardValues(snapshot.constraints, [0, 11_194_391_810_886]);
    expect(values.bips).toEqual([0, 32_391]);
    expect(values.shares).toEqual([0, 1]);
    expect(values.projected).toBe(true);
    const same = cardValues(snapshot.constraints, [30_000_000, 11_194_391_810_886]);
    expect(same.bips).toEqual([333, 32_391]);
    expect(same.projected).toBe(false);
    expect(cardValues(snapshot.constraints, []).projected).toBe(false);
  });

  it("recomputes the legacy exponent from the projected backlog", () => {
    const frames: FrameRequestCallback[] = [];
    vi.stubGlobal("requestAnimationFrame", (cb: FrameRequestCallback) => {
      frames.push(cb);
      return frames.length;
    });
    vi.stubGlobal("cancelAnimationFrame", vi.fn());
    vi.spyOn(performance, "now").mockImplementation(() => 1000);
    render(<ConstraintCards snapshot={{ ...snapshot, model: "legacy", constraints: [], legacy: { speedLimit: 7_000_000, inertia: 102, tolerance: 10, backlog: 160_000_000 } }} />);
    expect(screen.getByText(/x = 0.1260/)).toBeInTheDocument();
    act(() => frames[0](11_000));
    // 70M drained: (90M - 70M) * 10000 / 714M = 280 bips.
    expect(screen.getByText(/x = 0.0280/)).toBeInTheDocument();
    expect(screen.getByText("Backlog (projected)")).toBeInTheDocument();
  });
});

describe("PricerEquation", () => {
  it("labels the output as the fee implied with dt = 0, not a next-block prediction", () => {
    render(<PricerEquation snapshot={snapshot} />);
    expect(screen.getByText("implied, dt = 0")).toBeInTheDocument();
    expect(screen.queryByText(/predicted/i)).toBeNull();
    expect(screen.getByText(/fee they imply with dt = 0/)).toBeInTheDocument();
    expect(screen.getByText(/not known until that timestamp is/)).toBeInTheDocument();
  });
  it("renders the legacy form and the empty state", () => {
    const { rerender } = render(<PricerEquation snapshot={{ ...snapshot, model: "legacy", constraints: [], legacy: { speedLimit: 7_000_000, inertia: 102, tolerance: 10, backlog: 160_000_000 } }} />);
    expect(screen.getByText("legacy exponent")).toBeInTheDocument();
    rerender(<PricerEquation snapshot={null} />);
    expect(screen.queryByText("implied, dt = 0")).toBeNull();
  });
});

describe("DataFooter", () => {
  it("handles a network without a head yet and shows zoned timestamps", () => {
    render(
      <DataFooter
        snapshot={snapshot}
        series={series}
        networkInfo={{ name: "robinhood", displayName: "Robinhood Chain", chainId: 4663, explorerUrl: "", model: "constraints", headBlock: 0, headAt: null, lagSeconds: null, enabled: true }}
        status="open"
        apiStatus={{ version: "1", networks: [{ name: "robinhood", chainId: 4663, headBlock: 0, headAt: null, lagSeconds: null, lastSampleAt: null, lastError: null, rateLimitEvents: 0 }] }}
        now={Date.parse("2026-09-06T07:20:03Z")}
      />,
    );
    expect(screen.getByText("0, no head yet")).toBeInTheDocument();
    expect(screen.getByText("2026-09-06 02:20 CDT (3 s ago)")).toBeInTheDocument();
    expect(screen.getByText("none")).toBeInTheDocument();
  });
});

describe("L1Section", () => {
  function costPoint(t: number, feesWei: string): SeriesPoint {
    return point({ t, feesWei, floorFeesWei: feesWei });
  }
  const costSeries: Series = { range: "1h", resolution: "5s", constraintSets: [], ownerActions: [], points: [costPoint(1788679200, "250000000000000000"), costPoint(1788679205, "0")] };
  const batches: BatchSeries = {
    range: "1h",
    resolution: "batch",
    points: [
      { t: 1788679200, batches: 1, gasSpent: 1, weiSpent: "6000000000000", l1BaseFeeAvg: "1", calldataBytes: 1 },
      { t: 1788679212, batches: 1, gasSpent: 1, weiSpent: "4000000000000", l1BaseFeeAvg: "1", calldataBytes: 1 },
      { t: 1788679230, batches: 1, gasSpent: 1, weiSpent: "500000000000", l1BaseFeeAvg: "1", calldataBytes: 1 },
    ],
  };
  const l1: L1Series = { range: "1h", points: [{ t: 1788679200, baseFeeEstimate: "2369608", surplus: "1", feesAvailable: "1", unitsSinceUpdate: 1 }] };
  const l1Snapshot = {
    ...snapshot,
    l1: { baseFeeEstimate: "2369608", surplus: "190000000000000", feesAvailable: "1240000000000000", unitsSinceUpdate: 100, lastUpdateAt: "2026-09-06T07:20:00Z", equilibrationUnits: 160_000_000, perBatchGasCharge: 210_000, rewardRate: 10 },
  };

  it("counts each L2 bucket once for the batch resolution and lists every row in a table", async () => {
    getBatchesMock.mockResolvedValue(batches);
    getL1Mock.mockResolvedValue(l1);
    render(<L1Section network="robinhood" range="1h" snapshot={l1Snapshot} series={costSeries} />);
    const details = screen.getByText(/L1 pricer and posting costs/).closest("details") as HTMLDetailsElement;
    details.open = true;
    fireEvent(details, new Event("toggle"));
    // Two reports share the first 15 s bucket: 0.25 ETH of L2 fees is counted once, not twice.
    expect(await screen.findByText(/L2 fees 0.25 ETH/)).toBeInTheDocument();
    expect(screen.getByText(/L1 posting 0.0000105 ETH/)).toBeInTheDocument();
    expect(screen.getByText("3")).toBeInTheDocument();
    const summary = screen.getByText(/Data table \(2 buckets\)/);
    const table = summary.closest("details") as HTMLDetailsElement;
    table.open = true;
    fireEvent(table, new Event("toggle"));
    const rows = within(within(table).getByRole("table")).getAllByRole("row");
    expect(rows).toHaveLength(3);
    expect(within(rows[1]).getByText("0.25")).toBeInTheDocument();
    expect(within(rows[1]).getByText("0.00001")).toBeInTheDocument();
    expect(within(rows[1]).getByText("2")).toBeInTheDocument();
    expect(within(rows[2]).getByText("0.0000005")).toBeInTheDocument();
    expect(screen.getByText(/1 L1 pricer samples in range/)).toBeInTheDocument();
    expect(getBatchesMock).toHaveBeenCalledWith("robinhood", "1h", expect.anything());
  });

  it("shows the waiting copy before the slow sample and the empty state", () => {
    getBatchesMock.mockResolvedValue({ range: "1h", resolution: "batch", points: [] });
    getL1Mock.mockResolvedValue({ range: "1h", points: [] });
    render(<L1Section network="robinhood" range="1h" snapshot={null} series={null} />);
    expect(screen.getByText(/L1 values arrive with the slow/)).toBeInTheDocument();
    expect(screen.getByText("No batch reports in this range.")).toBeInTheDocument();
  });
});
