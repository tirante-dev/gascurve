import { fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
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

import { applicableRows, ChartTooltip } from "./ChartTooltip";
import { ChartFrame, HATCH_SPACING, HATCH_STROKE, HatchPattern } from "./primitives";
import { targetValues } from "@/lib/smoothing";
import { ConstraintCardsView } from "./ConstraintCards";
import { DataFooter } from "./DataFooter";
import { FeeFlows, feeFlowRows, feeTotals, incompleteTotalsNote, unsplitNote } from "./FeeFlows";
import { L1Section } from "./L1Section";
import { PricerEquation } from "./PricerEquation";
import { buildSeriesModel, describeSplit, GasPerSecondChart, SeriesCharts } from "./SeriesCharts";
import { LineChart, XAxis, YAxis } from "recharts";
import { GAP_LABEL_MIN_SHARE, GapBands, PartialBands, PartialNote } from "./ChartGaps";
import { partialBands } from "@/lib/partial";
import type { Gap } from "@/lib/gaps";

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
  range: "24h",
  resolution: "1m",
  from: 1788679200,
  to: 1788679380,
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
  computeGasPerSecond: { s10: 38_000_000, s60: 40_500_000 },
  replayErrorBips: 2,
  ethUsd: null,
};

/**
 * jsdom lays nothing out, so these read the rule the frame emits rather than a
 * measured width: the layout itself was checked in a browser at 375 CSS px.
 * What they are here to catch is a chart pinned to a width its card cannot give
 * it, which is what made the contribution chart scroll sideways on a phone.
 */
describe("ChartFrame", () => {
  it("clamps the width a chart asks for to the frame it is drawn in", () => {
    const { rerender } = render(
      <ChartFrame height={200} minWidth={420} label="a chart">
        <div />
      </ChartFrame>,
    );
    const frame = () => screen.getByRole("figure").firstElementChild as HTMLElement;
    expect(frame().style.minWidth).toBe("min(420px, 100%)");
    rerender(
      <ChartFrame height={200} label="a chart">
        <div />
      </ChartFrame>,
    );
    expect(frame().style.minWidth).toBe("min(560px, 100%)");
  });

  it("draws every chart in a frame that yields to the card, not one that widens it", () => {
    render(
      <>
        <SeriesCharts network="robinhood" range="24h" series={series} loading={false} model="constraints" />
        <FeeFlows network="robinhood" range="24h" snapshot={snapshot} series={series} model="constraints" nowMs={NOW_MS} />
        <ConstraintCardsView network="robinhood" snapshot={snapshot} values={null} blocks={[]} />
      </>,
    );
    const frames = screen.getAllByRole("figure");
    expect(frames.length).toBeGreaterThan(3);
    for (const figure of frames) {
      expect((figure.firstElementChild as HTMLElement).style.minWidth).toMatch(/^min\(\d+px, 100%\)$/);
    }
  });
});

describe("SeriesCharts", () => {
  it("draws one series per constraint set with set-aware legends and an explicit unknown-split series", () => {
    render(<SeriesCharts network="robinhood" range="24h" series={series} loading={false} model="constraints" />);
    expect(screen.getAllByText("C2 · 30 Mgas/s · 24 h · set 5 (from block 10)").length).toBeGreaterThan(0);
    expect(screen.getAllByText("C2 · 40 Mgas/s · 24 h · set 6 (from block 20)").length).toBeGreaterThan(0);
    expect(screen.getAllByText("unknown split (total x, constraint set unknown)").length).toBeGreaterThan(0);
    // The rate against each target is drawn in the hero now, on the hero's own
    // range, so the history section no longer repeats it.
    expect(screen.queryByText("Gas per second against each target")).toBeNull();
    expect(screen.getByText("C2 · 30 Mgas/s · 24 h (set 5) then 40 Mgas/s · 24 h (set 6)")).toBeInTheDocument();
    // The owner action is listed with a zoned timestamp.
    expect(screen.getByText("2026-09-06 02:21 CDT")).toBeInTheDocument();
  });

  it("draws the window that was asked for and shades what was never indexed", () => {
    // An hour of window with only the last three minutes indexed: the buckets
    // must not spread over the whole axis as though the hour were flat.
    const early = { ...series, from: series.points[0].t - 3600, to: series.points[2].t + 60 };
    render(<SeriesCharts network="robinhood" range="24h" series={early} loading={false} model="constraints" />);
    // One caption per chart, saying what the shading is and where the record starts.
    expect(screen.getAllByText("Shaded: not indexed yet, history before 2026-09-06 02:20 CDT").length).toBeGreaterThan(0);
  });

  it("breaks a line over a bucket that was never indexed rather than bridging it", () => {
    const [a, b, c] = series.points;
    const holed = { ...series, from: a.t, to: c.t + 3600, points: [a, b, { ...c, t: c.t + 3600 }] };
    render(<SeriesCharts network="robinhood" range="24h" series={holed} loading={false} model="constraints" />);
    expect(screen.getAllByText("Shaded: 1 gap with no buckets").length).toBeGreaterThan(0);
  });

  it("dots the buckets with no receipts behind them, which are whole buckets the gap and partial marks say nothing about", () => {
    // The exact shape the api serves for repaired history it cannot repair:
    // full coverage, complete, and a null compute gas rate because a source
    // block was stored without receipts.
    const [a, b, c] = series.points;
    const holed = { ...series, points: [a, { ...b, computeGasPerSecond: null }, { ...c, computeGasPerSecond: null }] };
    const m = buildSeriesModel(holed, "constraints");
    expect(m.gasMissing).toEqual([{ from: b.t, to: c.t + 60, kind: "receipts", buckets: 2 }]);
    render(<GasPerSecondChart m={m} />);
    expect(screen.getByText("Dotted: no receipt data for 2 buckets")).toBeInTheDocument();
    // The three marks stay apart: nothing was never indexed, and no bucket is partial.
    expect(screen.queryByText(/^Shaded:/)).toBeNull();
    expect(screen.queryByText(/^Hatched/)).toBeNull();
    // The tooltip says the cause rather than leaving the reader with a break in the line.
    expect(m.gasNote(m.points[2])).toBe("no receipt data for this bucket, so compute gas per second is not drawn");
    expect(m.gasNote(m.points[0])).toBeNull();
  });

  it("claims nothing about receipts when the api never reports a compute rate at all", () => {
    // An api older than the field sends no rate anywhere. The chart has
    // nothing to draw, but that is the client meeting an older api and not a
    // chain whose receipts are missing, so it must not say it is.
    const omitRate = (p: SeriesPoint): SeriesPoint => {
      const copy = { ...p };
      delete copy.computeGasPerSecond;
      return copy;
    };
    const legacy = { ...series, points: series.points.map(omitRate) };
    const m = buildSeriesModel(legacy, "constraints");
    expect(m.gasMissing).toEqual([]);
    expect(m.gasNote(m.points[0])).toBeNull();
    render(<GasPerSecondChart m={m} />);
    expect(screen.queryByText(/^Dotted:/)).toBeNull();
  });

  it("says nothing about a slot a constraint set never defined, which is not a hole in the record", () => {
    // One set with a single constraint, drawn beside a set with two: the
    // second panel is empty over the first set's buckets because that
    // constraint did not exist then.
    const sets = [series.constraintSets[0], { ...series.constraintSets[1], constraints: [series.constraintSets[1].constraints[0]] }];
    const [a, b] = series.points;
    const short = {
      ...series,
      constraintSets: sets,
      to: b.t + 60,
      points: [a, { ...b, constraintBips: [32_425], backlogs: [3_111_506], backlogsMax: [3_111_506] }],
    };
    render(<SeriesCharts network="robinhood" range="24h" series={short} loading={false} model="constraints" />);
    expect(screen.queryByText(/no backlog data/)).toBeNull();
  });

  it("exposes every bucket in a table and lets the keyboard inspect any point", () => {
    render(<SeriesCharts network="robinhood" range="24h" series={series} loading={false} model="constraints" />);
    const summary = screen.getByText(/Data table \(3 buckets/);
    const details = summary.closest("details") as HTMLDetailsElement;
    expect(within(details).queryByRole("table")).toBeNull();
    details.open = true;
    fireEvent(details, new Event("toggle"));
    const table = within(details).getByRole("table");
    // One row per bucket: the boundary duplicates the charts draw are not listed.
    expect(within(table).getAllByRole("row")).toHaveLength(4);
    expect(within(table).getByText("C1 0.0034 · C2 3.2391 (set 6)")).toBeInTheDocument();
    const unknownCell = within(table).getByText("unknown split (total x, constraint set unknown): 0.5000 (unknown set)");
    expect(within(table).getByText("11.2 Tgas")).toBeInTheDocument();
    // The unknown set's backlogs are still listed, under the unlabelled slots.
    const cells = Array.from((unknownCell.closest("tr") as HTMLTableRowElement).querySelectorAll("td")).map((td) => td.textContent);
    expect(cells.slice(7, 9)).toEqual(["1 gas", "1 gas"]);
    expect(within(table).queryByText("n/a")).toBeNull();

    const slider = screen.getByRole("slider", { name: /Select a bucket/ });
    expect(slider).toHaveValue("2");
    fireEvent.change(slider, { target: { value: "1" } });
    expect(screen.getByText("2026-09-06 02:21 CDT", { selector: "label span" })).toBeInTheDocument();
    expect(screen.getAllByText("3.2425").length).toBeGreaterThanOrEqual(2);
    expect(screen.getByText("Owner action at block 20: setMinimumL2BaseFee: 0.02 gwei")).toBeInTheDocument();
    fireEvent.change(slider, { target: { value: "0" } });
    expect(screen.getByText("0.4000")).toBeInTheDocument();
    expect(screen.getByText("30 Mgas/s")).toBeInTheDocument();
    expect(screen.queryByText("40 Mgas/s")).toBeNull();
  });

  it("handles empty, loading and legacy series", () => {
    const { rerender } = render(<SeriesCharts network="robinhood" range="24h" series={null} loading model="constraints" />);
    expect(screen.getByText("Loading history.")).toBeInTheDocument();
    rerender(<SeriesCharts network="robinhood" range="24h" series={{ ...series, points: [] }} loading={false} model="constraints" />);
    expect(screen.getByText("Nothing indexed for this range yet.")).toBeInTheDocument();
    rerender(<SeriesCharts network="robinhood" range="24h" series={{ ...series, constraintSets: [], ownerActions: [], points: [point({ t: 1, exponentBips: 1260, constraintBips: [1260], backlogs: [7], backlogsMax: [7] })] }} loading={false} model="legacy" />);
    expect(screen.getAllByText("legacy backlog").length).toBeGreaterThan(0);
  });

  it("labels history by the network's model, never by an empty set list", () => {
    // Early history: a constrained chain before any set is known. Two backlogs, two contributions, no sets.
    const early: Series = { ...series, constraintSets: [], ownerActions: [], points: [point({ t: 1, constraintSetId: 0, exponentBips: 32_425, constraintBips: [34, 32_391], backlogs: [3_111_506, 11_194_391_810_886], backlogsMax: [3_111_506, 11_194_391_810_886] })] };
    const { rerender } = render(<SeriesCharts network="robinhood" range="24h" series={early} loading={false} model="constraints" />);
    expect(screen.queryByText("legacy backlog")).toBeNull();
    expect(screen.getByText("C1 · definition unknown")).toBeInTheDocument();
    expect(screen.getByText("C2 · definition unknown")).toBeInTheDocument();
    expect(screen.getAllByText("unknown split (total x, constraint set unknown)").length).toBeGreaterThan(0);
    // The point inspector reads the whole x out under the unknown split, and the table describes it the same way.
    expect(screen.getAllByText("3.2425").length).toBeGreaterThanOrEqual(2);
    const details = screen.getByText(/Data table \(1 buckets/).closest("details") as HTMLDetailsElement;
    details.open = true;
    fireEvent(details, new Event("toggle"));
    expect(within(details).getByText("unknown split (total x, constraint set unknown): 3.2425 (unknown set)")).toBeInTheDocument();
    expect(within(details).queryByText("n/a")).toBeNull();
    // Both backlog panels carry data: the second is not left empty.
    expect(screen.getAllByText("11.2 Tgas").length).toBeGreaterThan(0);
    expect(screen.getAllByText("3.11 Mgas").length).toBeGreaterThan(0);
    rerender(<SeriesCharts network="robinhood" range="24h" series={early} loading={false} model="unknown" />);
    expect(screen.queryByText("legacy backlog")).toBeNull();
    expect(screen.getByText("C1 · definition unknown")).toBeInTheDocument();
    rerender(<SeriesCharts network="robinhood" range="24h" series={early} loading={false} model="legacy" />);
    expect(screen.getAllByText("legacy backlog").length).toBeGreaterThan(0);
  });

  it("describes a point's split under its own set", () => {
    const rows = buildChartPoints(series, "constraints");
    const segments = segmentsFor(series, "constraints");
    expect(describeSplit(rows[0], segments)).toBe("C1 0.4000 · C2 0.6000");
    expect(describeSplit(rows[2], segments)).toBe("unknown split (total x, constraint set unknown): 0.5000");
    const [unrecorded] = buildChartPoints({ ...series, points: [{ ...series.points[0], constraintBips: null }] }, "constraints");
    expect(describeSplit(unrecorded, segments)).toBe("unknown split (total x, split not recorded): 1.0000");
  });

  it("shows history from before the split migration as the unrecorded split, with n/a for the fee parts", () => {
    const early = point({ t: 1788679140, gasUsed: 100, feesWei: "2000000000000000000", floorFeesWei: null, surplusFeesWei: null, baseFeeMin: "100000000", baseFeeAvg: "300000000", baseFeeMax: "400000000", minBaseFee: "100000000", exponentBips: 10_000, constraintBips: null, backlogs: [1, 2], backlogsMax: [1, 2], constraintSetId: 5 });
    render(<SeriesCharts network="robinhood" range="24h" series={{ ...series, points: [early, ...series.points] }} loading={false} model="constraints" />);
    // Both unknown-split reasons are in the legend, since the range has both.
    expect(screen.getAllByText("unknown split (total x, split not recorded)").length).toBeGreaterThan(0);
    expect(screen.getAllByText("unknown split (total x, constraint set unknown)").length).toBeGreaterThan(0);
    const details = screen.getByText(/Data table \(4 buckets/).closest("details") as HTMLDetailsElement;
    details.open = true;
    fireEvent(details, new Event("toggle"));
    const table = within(details).getByRole("table");
    const cell = within(table).getByText("unknown split (total x, split not recorded): 1.0000 (set 5)");
    const cells = Array.from((cell.closest("tr") as HTMLTableRowElement).querySelectorAll("td")).map((td) => td.textContent);
    // The backlogs are still listed under the set; the fee parts are not known, the total is.
    expect(cells.slice(7, 9)).toEqual(["1 gas", "2 gas"]);
    expect(cells.slice(9, 12)).toEqual(["2", "n/a", "n/a"]);
    expect(within(table).getAllByText("n/a")).toHaveLength(3);
    // The inspector reads the whole x out under the unrecorded split for that bucket, and under the set for a recorded one.
    const slider = screen.getByRole("slider", { name: /Select a bucket/ });
    fireEvent.change(slider, { target: { value: "0" } });
    expect(screen.getByText("unknown split (total x, split not recorded)", { selector: "dt" })).toBeInTheDocument();
    expect(screen.queryByText("unknown split (total x, constraint set unknown)", { selector: "dt" })).toBeNull();
    fireEvent.change(slider, { target: { value: "1" } });
    expect(screen.queryByText("unknown split (total x, split not recorded)", { selector: "dt" })).toBeNull();
  });

  it("draws no floor for a bucket that recorded none, and reads it out as n/a", () => {
    // Pricing version 0 history: the contract makes minBaseFee nullable, and
    // a null floor used to take the whole history section down.
    const noFloor = point({ t: 1788679140, minBaseFee: null, floorFeesWei: null, surplusFeesWei: null, baseFeeMin: "100000000", baseFeeAvg: "100000000", baseFeeMax: "100000000", constraintBips: [4_000, 6_000], backlogs: [1, 2], backlogsMax: [1, 2], constraintSetId: 5 });
    render(<SeriesCharts network="robinhood" range="24h" series={{ ...series, points: [noFloor, ...series.points] }} loading={false} model="constraints" />);
    const slider = screen.getByRole("slider", { name: /Select a bucket/ });
    fireEvent.change(slider, { target: { value: "0" } });
    // The inspector says the floor is unknown rather than quoting a zero.
    const floorTerm = screen.getByText("floor in force", { selector: "dt" });
    expect(floorTerm.nextElementSibling).toHaveTextContent("n/a");
    // And so does the table.
    const details = screen.getByText(/Data table \(4 buckets/).closest("details") as HTMLDetailsElement;
    details.open = true;
    fireEvent(details, new Event("toggle"));
    const table = within(details).getByRole("table");
    const first = within(table).getAllByRole("row")[1];
    expect(Array.from(first.querySelectorAll("td")).map((td) => td.textContent)[3]).toBe("n/a");
  });

  it("never reads a null split as a zero contribution, and never contradicts itself in the same readout", () => {
    // A bucket whose set is known but whose per-constraint split was never
    // recorded: the tooltip and the inspector may say that, and must not also
    // list each constraint at 0.0000.
    const unrecorded = point({ t: 1788679140, exponentBips: 10_000, constraintBips: null, backlogs: [1, 2], backlogsMax: [1, 2], constraintSetId: 5, minBaseFee: "100000000", baseFeeMin: "100000000", baseFeeAvg: "100000000", baseFeeMax: "100000000" });
    render(<SeriesCharts network="robinhood" range="24h" series={{ ...series, points: [unrecorded, ...series.points] }} loading={false} model="constraints" />);
    const slider = screen.getByRole("slider", { name: /Select a bucket/ });
    fireEvent.change(slider, { target: { value: "0" } });
    expect(screen.getByText("unknown split (total x, split not recorded)", { selector: "dt" })).toBeInTheDocument();
    expect(screen.queryByText("C1 · 60 Mgas/s · 15 s · set 5 (from block 10)", { selector: "dt" })).toBeNull();
    expect(screen.queryByText("C2 · 30 Mgas/s · 24 h · set 5 (from block 10)", { selector: "dt" })).toBeNull();
    expect(screen.queryByText("0.0000", { selector: "dd" })).toBeNull();
    // The bucket next to it, with a recorded split, still lists every constraint.
    fireEvent.change(slider, { target: { value: "1" } });
    expect(screen.getByText("C1 · 60 Mgas/s · 15 s · set 5 (from block 10)", { selector: "dt" })).toBeInTheDocument();
    expect(screen.queryByText("unknown split (total x, split not recorded)", { selector: "dt" })).toBeNull();
  });

  it("hides a tooltip row whose value was never recorded rather than showing it as zero", () => {
    const segments = segmentsFor(series, "constraints");
    const [row] = buildChartPoints({ ...series, points: [{ ...series.points[0], constraintBips: null }] }, "constraints");
    // describeSplit is the same readout in the data table: no zeroes invented.
    expect(describeSplit({ ...row, splitKnown: true }, segments)).toBe("C1 n/a · C2 n/a");
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

/** The wall clock the fee-flow tests measure a quote's age against: the fixture's own sample time. */
const NOW_MS = Date.parse("2026-09-06T07:20:00Z");

describe("FeeFlows", () => {
  it("splits fees by the floor in force at each block, from the api's exact sums, not today's floor", () => {
    const totals = feeTotals(series);
    expect(totals.total).toBeCloseTo(5);
    expect(totals.floorEth).toBeCloseTo(2.2);
    expect(totals.surplusEth).toBeCloseTo(2.8);
    expect(totals.completeness).toBe("complete");
    expect(totals.perDay).not.toBeNull();
    expect(incompleteTotalsNote(totals)).toBeNull();
    render(<FeeFlows network="robinhood" range="24h" snapshot={snapshot} series={series} model="constraints" nowMs={NOW_MS} />);
    expect(screen.getByText("2.2")).toBeInTheDocument();
    expect(screen.getByText("2.8")).toBeInTheDocument();
    const summary = screen.getByText(/Data table \(3 buckets\)/);
    const details = summary.closest("details") as HTMLDetailsElement;
    details.open = true;
    fireEvent(details, new Event("toggle"));
    expect(within(details).getAllByRole("row")).toHaveLength(4);
    expect(within(details).getByText("0.1")).toBeInTheDocument();
  });
  it("puts a dollar line under each ETH total while the quote is fresh, and drops it once the quote is stale", () => {
    const now = NOW_MS;
    const priced = { ...snapshot, ethUsd: { price: "4200.00", at: "2026-09-06T07:15:00Z", source: "coingecko" } };
    const { rerender } = render(<FeeFlows network="robinhood" range="24h" snapshot={priced} series={series} model="constraints" nowMs={now} />);
    // 5 ETH of fees, 2.2 to the infra account and 2.8 to the network account, at 4,200 dollars.
    expect(screen.getByText("$21,000.0")).toBeInTheDocument();
    // Each dollar line opens a note with the multiplication that produced it,
    // quoting the ETH total drawn above it, rather than a title the reader
    // cannot see and has to wait on.
    const usd = screen.getByText("$21,000.0");
    expect(usd.closest("[title]")).toBeNull();
    expect(screen.getByText("5 ETH × $4,200.0/ETH = $21,000.0")).toBeInTheDocument();
    // All five totals name the quote they used, not just the one being read.
    expect(screen.getAllByText("coingecko, 5 min ago")).toHaveLength(5);
    // And the figure says it is inspectable rather than leaving the reader to guess.
    const trigger = usd.closest(".cursor-help");
    expect(trigger).toHaveAttribute("tabindex", "0");
    expect(screen.getByText("$9,240.0")).toBeInTheDocument();
    expect(screen.getByText("$11,760.0")).toBeInTheDocument();
    // Eleven minutes old: the same rule as the live tiles, so the totals go back to ETH alone.
    rerender(<FeeFlows network="robinhood" range="24h" snapshot={{ ...priced, ethUsd: { ...priced.ethUsd, at: "2026-09-06T07:09:00Z" } }} series={series} model="constraints" nowMs={now} />);
    expect(screen.queryByText("$21,000.0")).toBeNull();
    // And a network whose collector has no price feed never shows one.
    rerender(<FeeFlows network="robinhood" range="24h" snapshot={snapshot} series={series} model="constraints" nowMs={now} />);
    expect(screen.queryByText(/^\$/)).toBeNull();
    expect(screen.getByText("2.2")).toBeInTheDocument();
  });
  it("opens each note from the edge that keeps it inside the card at both column counts", () => {
    const priced = { ...snapshot, ethUsd: { price: "4200.00", at: "2026-09-06T07:15:00Z", source: "coingecko" } };
    const { container } = render(<FeeFlows network="robinhood" range="24h" snapshot={priced} series={series} model="constraints" nowMs={NOW_MS} />);
    // The stats grid is two columns narrow and five wide, so a stat's column
    // changes with the breakpoint: the notes have to change edge with it, or a
    // panel wider than one column leaves the card at one of the two widths.
    const stats = container.querySelector(".sm\\:grid-cols-5") as HTMLElement;
    const edges = [...stats.querySelectorAll(".cursor-help")].map((t) => {
      const panel = t.nextElementSibling as HTMLElement;
      return [panel.classList.contains("right-0") ? "end" : "start", panel.classList.contains("sm:right-0") ? "end" : "start"];
    });
    // Narrow, the odd stats are the right-hand column; wide, only the last two sit near the right edge.
    expect(edges).toEqual([
      ["start", "start"],
      ["end", "start"],
      ["start", "start"],
      ["end", "end"],
      ["start", "end"],
    ]);
  });
  it("renders without a snapshot or history", () => {
    render(<FeeFlows network="robinhood" range="24h" snapshot={null} series={null} nowMs={NOW_MS} />);
    expect(screen.getByText("No history loaded.")).toBeInTheDocument();
    expect(screen.getByText(/none yet/)).toBeInTheDocument();
    expect(screen.queryByText(/predate the fee split/)).toBeNull();
  });
  it("treats buckets that predate the fee split as unknown: hatched rather than zero, left out of the totals, footnoted", () => {
    const early = [
      point({ t: 1788679080, feesWei: "4000000000000000000", floorFeesWei: null, surplusFeesWei: null, posterFeesWei: "500000000000000000", minBaseFee: "100000000" }),
      point({ t: 1788679140, feesWei: "2000000000000000000", floorFeesWei: null, surplusFeesWei: null, minBaseFee: "100000000" }),
    ];
    const mixed: Series = { ...series, points: [...early, ...series.points] };
    const totals = feeTotals(mixed);
    expect(totals.total).toBeCloseTo(11);
    expect(totals.floorEth).toBeCloseTo(2.2);
    expect(totals.surplusEth).toBeCloseTo(2.8);
    expect(totals.posterEth).toBe(0);
    expect(totals.unsplit).toBe(2);
    expect(feeTotals(series).unsplit).toBe(0);
    expect(unsplitNote(1)).toBe("1 bucket has no recorded destination split");
    expect(unsplitNote(1200)).toBe("1,200 buckets have no recorded destination split");
    render(<FeeFlows network="robinhood" range="24h" snapshot={snapshot} series={mixed} model="constraints" nowMs={NOW_MS} />);
    expect(screen.getByText(/^2 buckets have no recorded destination split/)).toBeInTheDocument();
    expect(screen.getByText("destination split unavailable")).toBeInTheDocument();
    expect(screen.getByRole("figure", { name: /hatched where the split is unavailable/ })).toBeInTheDocument();
    const details = screen.getByText(/Data table \(5 buckets\)/).closest("details") as HTMLDetailsElement;
    details.open = true;
    fireEvent(details, new Event("toggle"));
    const rows = within(details).getAllByRole("row");
    expect(rows).toHaveLength(6);
    expect(within(rows[1]).getAllByText("n/a")).toHaveLength(3);
    expect(within(rows[1]).getByText("4")).toBeInTheDocument();
    expect(within(rows[3]).queryByText("n/a")).toBeNull();
    expect(within(details).getAllByText("n/a")).toHaveLength(6);
  });
  it("draws the unknown series as a hatch in the chart and the same hatch in its legend", () => {
    const mixed: Series = { ...series, points: [point({ t: 1788679080, feesWei: "4000000000000000000", floorFeesWei: null, surplusFeesWei: null, minBaseFee: "100000000" }), ...series.points] };
    render(<FeeFlows network="robinhood" range="24h" snapshot={snapshot} series={mixed} model="constraints" nowMs={NOW_MS} />);
    const item = screen.getByText("destination split unavailable", { selector: "li span" }).closest("li") as HTMLLIElement;
    const swatch = item.querySelector("span[aria-hidden]") as HTMLElement;
    // The legend carries the pattern, not a solid square: the association with
    // the hatched area does not depend on colour alone.
    expect(swatch.style.backgroundImage).toContain("repeating-linear-gradient(45deg");
    expect(swatch.style.background).not.toBe("var(--ink-3)");
    // The chart fills with the same pattern component, at the same geometry.
    const { container } = render(
      <svg>
        <HatchPattern id="fee-unsplit-hatch" color="var(--ink-3)" />
      </svg>,
    );
    const line = container.querySelector("#fee-unsplit-hatch line") as SVGLineElement;
    // Full strength: the 55% opaque hatch composited below 3:1 on the light chart surface.
    expect(line.getAttribute("stroke-opacity")).toBeNull();
    expect(line.getAttribute("stroke-width")).toBe(String(HATCH_STROKE));
    expect(swatch.style.backgroundImage).toContain(`${HATCH_STROKE}px`);
    expect(swatch.style.backgroundImage).toContain(`${HATCH_SPACING}px`);
  });

  it("hatches the bucket the collector is still filling rather than stacking a total it has not finished collecting", () => {
    const filling: Series = { ...series, to: series.points[2].t + 25, points: [...series.points.slice(0, 2), { ...series.points[2], coverage: 0.4, completeness: "partial" }] };
    const totals = feeTotals(filling);
    expect(totals.completeness).toBe("partial");
    expect(totals.perDay).toBeNull();
    render(<FeeFlows network="robinhood" range="24h" snapshot={snapshot} series={filling} model="constraints" nowMs={NOW_MS} />);
    expect(screen.getByText("Hatched and left out: bucket in progress, 40% elapsed")).toBeInTheDocument();
    expect(screen.getByRole("figure", { name: /or the bucket is incomplete/ })).toBeInTheDocument();
    // Only the stack leaves it out: the bucket keeps every figure it has.
    const details = screen.getByText(/Data table \(3 buckets\)/).closest("details") as HTMLDetailsElement;
    details.open = true;
    fireEvent(details, new Event("toggle"));
    expect(within(details).getAllByRole("row")).toHaveLength(4);
    expect(within(within(details).getAllByRole("row")[3]).queryByText("n/a")).toBeNull();
  });

  it("treats a populated bucket with a bounded block hole as incomplete", () => {
    const holed: Series = { ...series, points: [series.points[0], { ...series.points[1], gasPerSecond: 25, computeGasPerSecond: 25, coverage: 0.8, completeness: "partial" }, series.points[2]] };
    const rows = buildChartPoints(holed, "constraints");
    expect(rows[1].gps).toBe(25);
    const totals = feeTotals(holed);
    expect(totals).toMatchObject({ total: 5, completeness: "partial", partialBuckets: 1, unknownBuckets: 0, emptyIntervals: 0, perDay: null });
    expect(incompleteTotalsNote(totals)).toBe("Indexed-block sums are lower bounds: 1 partially indexed bucket. The per-day estimate waits for complete coverage.");

    render(<FeeFlows network="robinhood" range="24h" snapshot={snapshot} series={holed} model="constraints" nowMs={NOW_MS} />);
    expect(screen.getByText("Hatched and left out: partially indexed, 80% of the bucket")).toBeInTheDocument();
    expect(screen.getByText("Indexed fees in last 24h")).toBeInTheDocument();
    expect(screen.getByText("n/a")).toBeInTheDocument();
    expect(screen.getByText(/Indexed-block sums are lower bounds/)).toBeInTheDocument();
  });

  it("keeps unknown bucket completeness distinct in the totals", () => {
    const unknown: Series = { ...series, points: [series.points[0], { ...series.points[1], coverage: null, completeness: "unknown" }, series.points[2]] };
    const totals = feeTotals(unknown);
    expect(totals).toMatchObject({ completeness: "unknown", partialBuckets: 0, unknownBuckets: 1, perDay: null });
    expect(incompleteTotalsNote(totals)).toContain("1 bucket has unknown completeness");
  });

  it("reads a bucket in progress out as what it has collected so far, not as what the bucket holds", () => {
    const rows = feeFlowRows(false);
    expect(rows.map((r) => r.label)).toEqual(["fees in bucket", "fees so far", "floor to infra", "congestion to network", "poster fee to L1 pricer", "floor in force"]);
    const whole = { feesEth: 1, floorFeesEth: 1, surplusFeesEth: 0, posterFeesEth: 0, unsplitFeesEth: null, floor: 0.02, partial: null, coverage: 1 };
    expect(applicableRows(rows, whole).map((r) => r.label)).toEqual(["fees in bucket", "floor to infra", "congestion to network", "poster fee to L1 pricer", "floor in force"]);
    expect(applicableRows(rows, { ...whole, partial: "in-progress", coverage: 0.4 }).map((r) => r.label)).toEqual(["fees so far", "floor to infra", "congestion to network", "poster fee to L1 pricer", "floor in force"]);
  });

  it("shows no unknown-split legend or footnote when every bucket carries the split", () => {
    render(<FeeFlows network="robinhood" range="24h" snapshot={snapshot} series={series} model="constraints" nowMs={NOW_MS} />);
    expect(screen.queryByText(/predate the fee split/)).toBeNull();
    expect(screen.queryByText("destination split unavailable")).toBeNull();
    expect(screen.getByText("congestion to network", { selector: "li span" })).toBeInTheDocument();
  });
});

describe("ConstraintCards", () => {
  it("recomputes x, shares and colours from the eased backlogs", () => {
    // A 30M backlog over 60M × 120 s: 41 bips at the sample, 1.3% of x.
    const long = { ...snapshot, constraints: [{ ...snapshot.constraints[0], window: 120 }, snapshot.constraints[1]] };
    const { rerender } = render(<ConstraintCardsView network="robinhood" snapshot={long} values={null} blocks={[]} />);
    expect(screen.getByText("0.0041")).toBeInTheDocument();
    expect(screen.getByText("0.1%").parentElement).toHaveTextContent("0.1% of x");
    expect(screen.queryByText(/avg 2 s/)).toBeNull();
    // One second later the 30M backlog has drained at 60M/s: x1 is 0, share 0, and the card says so.
    rerender(<ConstraintCardsView network="robinhood" snapshot={long} values={targetValues(long, [], 1)} blocks={[]} />);
    expect(screen.getByText("0.0000")).toBeInTheDocument();
    expect(screen.getByText("no contribution")).toBeInTheDocument();
    expect(screen.getByText("100.0%").parentElement).toHaveTextContent("100.0% of x");
    expect(screen.getByText(/Long windows keep draining at their target rate/)).toBeInTheDocument();
  });

  it("draws a bounded number of gauge marks however large the backlog", () => {
    // Target 1, window 1, backlog a billion: a billion windows of target.
    const huge = { ...snapshot, constraints: [{ target: 1, window: 1, backlog: 1_000_000_000, exponentBips: 0 }] };
    render(<ConstraintCardsView network="robinhood" snapshot={huge} values={null} blocks={[]} />);
    const meter = screen.getByRole("meter");
    expect(meter.querySelectorAll("span").length).toBeLessThanOrEqual(24);
    expect(meter).toHaveAttribute("aria-valuenow", "100");
    // The far end says what it is, not what to multiply out.
    expect(screen.getByText("1,000,000,000 windows of target (1 Ggas)")).toBeInTheDocument();
    expect(screen.getByRole("meter", { name: /1,000,000,000 windows/ })).toBeInTheDocument();
    // And it opens as a note rather than a title the reader has to find on a
    // 10 px bar and then wait on: the pricer's own divisor, multiplied out.
    expect(meter).not.toHaveAttribute("title");
    expect(screen.getByText("1 window of target = 1 gas/s × 1 s = 1 gas")).toBeInTheDocument();
    // Twice over, in the panel and in the description that stands in for it:
    // the panel is aria-hidden, so a reader without it still gets the working.
    expect(screen.getAllByText(/A backlog of one window adds exactly 1.0 to x/)).toHaveLength(2);
    // The scale is not a fixed ceiling, which is the part the count alone hides.
    expect(screen.getAllByText(/the far end moves out as the backlog crosses one/)).toHaveLength(2);
    // The figure says it is inspectable, and the panel is not announced twice.
    const trigger = screen.getByText("1,000,000,000 windows of target (1 Ggas)").closest(".cursor-help");
    expect(trigger).toHaveAttribute("tabindex", "0");
  });

  it("defines the legacy gauge for zero tolerance and for a zero denominator", () => {
    const { rerender } = render(<ConstraintCardsView network="robinhood" snapshot={{ ...snapshot, model: "legacy", constraints: [], legacy: { speedLimit: 7_000_000, inertia: 102, tolerance: 0, backlog: 1_000_000_000 } }} values={null} blocks={[]} />);
    expect(screen.getByText("no free gas: every unit prices")).toBeInTheDocument();
    expect(screen.getByText("x = 1 at 714 Mgas")).toBeInTheDocument();
    expect(screen.getByText("2 units of x (1.43 Ggas)")).toBeInTheDocument();
    expect(screen.getByText("1 unit of x = inertia × speed limit = 714 Mgas")).toBeInTheDocument();
    const meter = screen.getByRole("meter", { name: /units of inertia/ });
    expect(meter).toHaveAttribute("aria-valuenow", "70");
    expect(meter.querySelectorAll("span")).toHaveLength(1);
    // (1B * 10000) / 714M = 14005 bips.
    expect(screen.getByText(/x = 1.4005/)).toBeInTheDocument();
    rerender(<ConstraintCardsView network="robinhood" snapshot={{ ...snapshot, model: "legacy", constraints: [], legacy: { speedLimit: 7_000_000, inertia: 0, tolerance: 0, backlog: 5 } }} values={null} blocks={[]} />);
    expect(screen.getByText("no scale")).toBeInTheDocument();
    expect(screen.getByText("no scale (zero inertia or speed limit)")).toBeInTheDocument();
    // The legacy far end opens the same way, and says why there is no scale.
    expect(screen.getByText("no scale: the inertia or the speed limit is zero")).toBeInTheDocument();
    expect(screen.getByRole("meter")).toHaveAttribute("aria-valuenow", "0");
    expect(screen.getByText(/x = 0.0000/)).toBeInTheDocument();
  });

  it("recomputes the legacy exponent from the drained backlog", () => {
    const legacy: LiveSnapshot = { ...snapshot, model: "legacy", constraints: [], legacy: { speedLimit: 7_000_000, inertia: 102, tolerance: 10, backlog: 160_000_000 } };
    const { rerender } = render(<ConstraintCardsView network="robinhood" snapshot={legacy} values={null} blocks={[]} />);
    expect(screen.getByText(/x = 0.1260/)).toBeInTheDocument();
    rerender(<ConstraintCardsView network="robinhood" snapshot={legacy} values={targetValues(legacy, [], 10)} blocks={[]} />);
    // 70M drained: (90M - 70M) * 10000 / 714M = 280 bips.
    expect(screen.getByText(/x = 0.0280/)).toBeInTheDocument();
    expect(screen.getByText("90.0")).toBeInTheDocument();
  });
});

describe("PricerEquation", () => {
  it("labels the output as the fee implied with dt = 0, not a next-block prediction", () => {
    render(<PricerEquation snapshot={snapshot} />);
    expect(screen.getByText("implied, dt = 0")).toBeInTheDocument();
    expect(screen.queryByText(/predicted/i)).toBeNull();
    expect(screen.getByText(/fee they imply with dt = 0/)).toBeInTheDocument();
    expect(screen.getByText(/not known until that timestamp is/)).toBeInTheDocument();
    // The P4 against e^x comparison belongs to the explainer page; the live
    // equation states what the chain did, not what another curve would have.
    expect(screen.queryByText(/True e/)).toBeNull();
    expect(screen.queryByText(/would give/)).toBeNull();
    expect(screen.queryByText(/instead of/)).toBeNull();
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
        apiStatus={{
          version: "1",
          status: "degraded",
          networks: [
            {
              name: "robinhood",
              chainId: 4663,
              enabled: true,
              headBlock: 0,
              headAt: null,
              lagSeconds: null,
              lastSampleAt: null,
              lastError: null,
              rateLimitEvents: 0,
              last429At: null,
              backfillCursor: null,
              arbosVersion: null,
              degraded: false,
              capacity: {
                configuredCallsPerSecond: 0,
                requiredCallsPerSecond: 0,
                observedCallsPerSecond: 0,
                headroomCallsPerSecond: null,
                saturated: false,
                at: null,
                checkpointError: false,
              },
              holes: {
                pending: 0,
                blocks: 0,
                unfillable: 0,
                retrying: 0,
                oldestAgeSeconds: 0,
                checkpointError: false,
                pendingBlocks: 0,
                oldestPendingAt: null,
                oldestPendingAgeSeconds: null,
              },
              status: "degraded",
              degradedReasons: ["collector heartbeat missing"],
              collector: null,
              activeEndpoint: 0,
              failovers: 0,
              endpoints: [],
            },
          ],
        }}
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
  const costSeries: Series = { range: "1h", resolution: "5s", from: 1788679200, to: 1788679210, constraintSets: [], ownerActions: [], points: [costPoint(1788679200, "250000000000000000"), costPoint(1788679205, "0")] };
  const batches: BatchSeries = {
    range: "1h",
    resolution: "batch",
    from: 1788679200,
    to: 1788679215,
    points: [
      { t: 1788679200, batches: 1, gasSpent: 1, weiSpent: "6000000000000", l1BaseFeeAvg: "1", calldataBytes: 1 },
      { t: 1788679212, batches: 1, gasSpent: 1, weiSpent: "4000000000000", l1BaseFeeAvg: "1", calldataBytes: 1 },
      { t: 1788679230, batches: 1, gasSpent: 1, weiSpent: "500000000000", l1BaseFeeAvg: "1", calldataBytes: 1 },
    ],
  };
  const l1: L1Series = { range: "1h", from: 1788679200, to: 1788679215, points: [{ t: 1788679200, baseFeeEstimate: "2369608", surplus: "1", feesAvailable: "1", unitsSinceUpdate: 1 }] };
  const l1Snapshot = {
    ...snapshot,
    l1: { baseFeeEstimate: "2369608", surplus: "190000000000000", feesAvailable: "1240000000000000", unitsSinceUpdate: 100, lastUpdateAt: "2026-09-06T07:20:00Z", equilibrationUnits: 160_000_000, perBatchGasCharge: 210_000, rewardRate: 10 },
  };

  it("counts each L2 bucket once for the batch resolution and lists every row in a table", async () => {
    getBatchesMock.mockResolvedValue(batches);
    getL1Mock.mockResolvedValue(l1);
    render(<L1Section network="robinhood" range="1h" snapshot={l1Snapshot} series={costSeries} />);
    const details = screen.getByText(/L1 pricer and attributed batch costs/).closest("details") as HTMLDetailsElement;
    details.open = true;
    fireEvent(details, new Event("toggle"));
    // Two reports share the first 15 s bucket: 0.25 ETH of L2 fees is counted once, not twice.
    expect(await screen.findByText(/L2 fees 0.25 ETH/)).toBeInTheDocument();
    expect(screen.getByText(/ArbOS batch cost 0.0000105 ETH/)).toBeInTheDocument();
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
    getBatchesMock.mockResolvedValue({ range: "1h", resolution: "batch", from: 1788679200, to: 1788679215, points: [] });
    getL1Mock.mockResolvedValue({ range: "1h", from: 1788679200, to: 1788679215, points: [] });
    render(<L1Section network="robinhood" range="1h" snapshot={null} series={null} />);
    expect(screen.getByText(/L1 values arrive with the slow/)).toBeInTheDocument();
    expect(screen.getByText("No batch reports in this range.")).toBeInTheDocument();
  });
});

describe("the band layer", () => {
  // One layer of rects per kind rather than a ReferenceArea per band: a day of
  // minute buckets otherwise puts thousands of store subscribers on the page.
  const WINDOW = { from: 0, to: 600 };
  const bandChart = (children: React.ReactNode) =>
    render(
      <LineChart width={400} height={100} data={[{ t: WINDOW.from }, { t: WINDOW.to }]}>
        <XAxis dataKey="t" type="number" domain={[WINDOW.from, WINDOW.to]} />
        <YAxis />
        {children}
      </LineChart>,
    ).container;

  const rects = (container: HTMLElement, kind: string) => [...container.querySelectorAll(`rect[data-band="${kind}"]`)];

  const gapChart = (gaps: Gap[]) => bandChart(<GapBands gaps={gaps} window={WINDOW} />);

  it("clamps a band that runs past the window to the plot area", () => {
    const wide = rects(gapChart([{ from: -600, to: 1200, kind: "interior" }]), "gap");
    const whole = rects(gapChart([{ from: WINDOW.from, to: WINDOW.to, kind: "interior" }]), "gap");
    expect(wide).toHaveLength(1);
    expect(wide[0].getAttribute("x")).toBe(whole[0].getAttribute("x"));
    expect(wide[0].getAttribute("width")).toBe(whole[0].getAttribute("width"));
    expect(Number(wide[0].getAttribute("x"))).toBeGreaterThan(0);
    expect(Number(wide[0].getAttribute("x")) + Number(wide[0].getAttribute("width"))).toBeLessThanOrEqual(400);
  });

  it("draws nothing at all for a band that clamps to nothing", () => {
    expect(rects(gapChart([{ from: -1200, to: -600, kind: "leading" }]), "gap")).toHaveLength(0);
  });

  it("leaves a band too narrow for its word unlabelled", () => {
    const narrow = gapChart([{ from: 0, to: 600 * (GAP_LABEL_MIN_SHARE / 2), kind: "interior" }]);
    expect(rects(narrow, "gap")).toHaveLength(1);
    expect(within(narrow).queryByText("gap")).toBeNull();
    expect(within(gapChart([{ from: 0, to: 300, kind: "interior" }])).getByText("gap")).toBeInTheDocument();
  });

  it("coalesces hundreds of partial buckets into a handful of rects and one short caption", () => {
    // Four hundred one-second buckets, half partly indexed and half of unknown
    // completeness, in eight stretches: eight rects, not four hundred.
    const points = Array.from({ length: 400 }, (_, i) => ({ t: i, coverage: i % 100 < 50 ? 0.5 : null }));
    const bands = partialBands(points, 1, 4000);
    expect(bands).toHaveLength(400);
    const container = bandChart(<PartialBands bands={bands} window={{ from: 0, to: 400 }} />);
    expect(rects(container, "partial")).toHaveLength(8);
    render(<PartialNote bands={bands} />);
    expect(screen.getByText("Hatched and left out: partly indexed for 200 buckets in 4 stretches · coverage unknown for 200 buckets in 4 stretches")).toBeInTheDocument();
  });
});
