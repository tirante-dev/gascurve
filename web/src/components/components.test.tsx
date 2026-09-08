import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { cloneElement, isValidElement } from "react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import type { BlockPoint, LiveSnapshot, Network, OwnerAction, Series } from "@/types";
import { createFrameStore, NO_PLACES, targetValues } from "@/lib/smoothing";
import { applyReorg } from "@/hooks/useLive";
import { BACKLOG_TITLE, backlogAxis, ConstraintCards, ConstraintCardsView, drainLabel, Sawtooth, sawtoothTooltipRows, secondsAgoLabel } from "./ConstraintCards";
import { ChartTooltip } from "./ChartTooltip";
import { DataFooter } from "./DataFooter";
import { COLLECTOR_LAG_S, CostTile, HeroChart, HeroChartPanel, heroTooltipRows, LiveHero, LiveHeroView, sampleAge } from "./LiveHero";
import { HERO_RANGE_KEY, setHeroRange } from "@/lib/hero";
import { HistoryTabs } from "./HistoryTabs";
import { NetworkSwitcher } from "./NetworkSwitcher";
import { OwnerActionTimeline } from "./OwnerActionTimeline";
import { applyTheme, readTheme, ThemeToggle } from "./ThemeToggle";
import { StatusPill } from "./primitives";

// The hero asks for a Series when its range is not Live; the test drives that
// state directly and records what was asked for, so nothing reaches the api.
const seriesMock = vi.hoisted(() => ({
  calls: [] as (string | null)[][],
  state: { data: null as Series | null, error: null as string | null, loading: false, updatedAt: null as number | null, refresh: () => undefined },
}));
vi.mock("@/hooks/useSeries", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/hooks/useSeries")>()),
  useSeries: (network: string | null, range: string | null) => {
    seriesMock.calls.push([network, range]);
    return seriesMock.state;
  },
}));

// ResponsiveContainer measures its box, and jsdom has none: hand the chart a
// fixed size instead, so recharts really lays its axes and marks out and the
// tests can read them rather than only the frame around them.
vi.mock("recharts", async (importOriginal) => {
  const original = await importOriginal<typeof import("recharts")>();
  const Sized = ({ children }: { children: React.ReactNode }) => (
    <div style={{ width: 400, height: 200 }}>
      {isValidElement<{ width?: number; height?: number }>(children) ? cloneElement(children, { width: 400, height: 200 }) : children}
    </div>
  );
  return { ...original, ResponsiveContainer: Sized };
});

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
    { target: 60_000_000, window: 15, backlog: 3_111_506, exponentBips: 34 },
    { target: 40_000_000, window: 86_400, backlog: 11_194_391_810_886, exponentBips: 32_391 },
  ],
  prices: { perL2Tx: "0", perL1CalldataByte: "0", perL2Storage: "0", perArbGasBase: "20000000", perArbGasCongestion: "379726000", perArbGasTotal: "399726000" },
  gasPerSecond: { s10: 38_000_000, s60: 40_500_000 },
  computeGasPerSecond: { s10: 38_000_000, s60: 40_500_000 },
  // The shared fixture carries no quote, so the cost tiles read in ETH; the USD cases below supply their own.
  replayErrorBips: 2,
  ethUsd: null,
};

/** Ten blocks a second for `seconds` seconds ending at `lastTs`, the short window climbing 4M a block and dropping at each boundary. */
function sawtoothBlocks(seconds: number, lastTs: number): BlockPoint[] {
  const out: BlockPoint[] = [];
  let n = 1;
  for (let ts = lastTs - seconds + 1; ts <= lastTs; ts++) {
    for (let k = 0; k < 10; k++) {
      out.push({ number: n++, ts, gasUsed: 4_000_000, posterGas: 0, baseFee: "399726000", predictedBaseFee: "399726000", backlogs: [(k + 1) * 4_000_000, 11_194_391_810_886], constraintBips: [], exponentBips: 0, minBaseFee: "20000000", anchored: k === 0 });
    }
  }
  return out;
}

/** Two buckets of history, enough for the hero's historical chart to have a line and a band. */
const history: Series = {
  range: "24h",
  resolution: "1m",
  from: 1788679200,
  to: 1788679320,
  constraintSets: [],
  ownerActions: [{ block: 20, at: "2026-09-06T07:21:00Z", txHash: "0x" + "ab".repeat(32), method: "setMinimumL2BaseFee", selector: "0xa0188cdb", args: { priceInWei: "20000000" } }],
  points: [
    { t: 1788679200, blocks: 12, gasUsed: 100, posterGas: 0, gasPerSecond: 1, computeGasPerSecond: 1, coverage: 1, completeness: "complete", feesWei: "0", baseFeeMin: "100000000", baseFeeAvg: "300000000", baseFeeMax: "400000000", exponentBips: 10_000, constraintBips: [10_000, 0], backlogs: [1, 2], backlogsMax: [1, 2], minBaseFee: "100000000", floorFeesWei: "0", surplusFeesWei: "0", posterFeesWei: "0", constraintSetId: 0, replayErrorBips: 0 },
    { t: 1788679260, blocks: 30, gasUsed: 100, posterGas: 0, gasPerSecond: 1, computeGasPerSecond: 1, coverage: 1, completeness: "complete", feesWei: "0", baseFeeMin: "20000000", baseFeeAvg: "395726000", baseFeeMax: "400000000", exponentBips: 32_425, constraintBips: [32_425, 0], backlogs: [3, 4], backlogsMax: [3, 4], minBaseFee: "20000000", floorFeesWei: "0", surplusFeesWei: "0", posterFeesWei: "0", constraintSetId: 0, replayErrorBips: 0 },
  ],
};


/**
 * The width a fixed figure reserves, read off the inline style rather than
 * through toHaveStyle: jsdom resolves ch units in computed style (6ch reads
 * back as 48px), so comparing the inline value is what keeps the assertion
 * about the unit the layout actually depends on.
 */
function reservedWidth(el: HTMLElement): string {
  return el.style.minWidth;
}

describe("LiveHero", () => {
  it("lays the figures out in the left four columns and the chart in the right eight", () => {
    const { container } = render(<LiveHeroView network="robinhood" snapshot={snapshot} values={null} blocks={[]} nowMs={Date.parse(snapshot.sampledAt)} status="open" />);
    const grid = container.querySelector(".lg\\:grid-cols-12");
    expect(grid).not.toBeNull();
    expect(grid?.querySelector(".lg\\:col-span-4")).not.toBeNull();
    expect(grid?.querySelector(".lg\\:col-span-8")).not.toBeNull();
  });
  it("shows the fee, multiplier, floor, stats and costs at fixed widths", () => {
    render(<LiveHeroView network="robinhood" snapshot={snapshot} values={null} blocks={[]} nowMs={Date.parse(snapshot.sampledAt)} status="open" />);
    const hero = screen.getByText("0.3997");
    expect(hero).toHaveClass("tabular-nums");
    expect(reservedWidth(hero)).toBe("6ch");
    expect(hero.nextSibling).toHaveTextContent("gwei");
    // The hero figure steps down a size from the old strip, so the chart beside it is the taller mark.
    expect(hero.parentElement).toHaveClass("text-4xl");
    expect(hero.parentElement).toHaveClass("sm:text-5xl");
    expect(reservedWidth(screen.getByText("19.99"))).toBe("5ch");
    expect(screen.getByText(/floor 0.02 gwei/)).toBeInTheDocument();
    expect(screen.getByText("55,812,345")).toBeInTheDocument();
    expect(screen.getByText("38.0")).toBeInTheDocument();
    expect(screen.getByText("40.5")).toBeInTheDocument();
    expect(screen.getByText("3.2425")).toBeInTheDocument();
    expect(reservedWidth(screen.getByText("0.00000839"))).toBe("10ch");
    expect(screen.getByText("0.0000600")).toBeInTheDocument();
    expect(screen.getByText(/4.02 Mgas in block 55,812,345/)).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("live");
  });
  it("does not relabel total throughput as compute throughput against an old api", () => {
    render(<LiveHeroView network="robinhood" snapshot={{ ...snapshot, computeGasPerSecond: undefined }} values={null} blocks={[]} nowMs={Date.parse(snapshot.sampledAt)} status="open" />);
    expect(screen.getByText("Network load (10 s)").closest(".min-w-0")).toHaveTextContent("n/a");
    expect(screen.getByText("Network load (60 s)").closest(".min-w-0")).toHaveTextContent("n/a");
  });
  it("labels the figures in plain words and keeps the precise term a hover away, in the accessible name too", () => {
    render(<LiveHeroView network="robinhood" snapshot={snapshot} values={null} blocks={[]} nowMs={Date.parse(snapshot.sampledAt)} status="open" />);
    for (const [label, term] of [
      ["Base fee now", "the price of one unit of gas right now, in gwei"],
      ["Network load (10 s)", "compute gas per second, averaged over the last 10 s"],
      ["Send", "a 21,000 gas transfer at the base fee now"],
      ["Swap", "a 150,000 gas swap at the base fee now"],
    ]) {
      const word = screen.getByText(label);
      // The term is in the note, set as text rather than as a figure, and in the description a screen reader gets instead.
      const note = screen.getByText(term);
      expect(note).not.toHaveClass("num");
      expect(word.closest(".group")).toContainElement(note);
      const literal = (text: string) => text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
      expect(word.nextSibling).toHaveTextContent(new RegExp(`^${literal(label)}: ${literal(term)}`));
    }
    // The old labels went into the notes, not away.
    expect(screen.queryByText("21k transfer")).toBeNull();
    expect(screen.queryByText("150k swap")).toBeNull();
    expect(screen.queryByText("Compute gas/s (10 s)")).toBeNull();
  });
  it("draws the multiplier over the floor as a lit gauge, coloured by which of three bands it falls in", () => {
    const at = (multiplier: number) => ({ ...targetValues(snapshot, [], 0), multiplier });
    const position = (gauge: HTMLElement) => within(gauge).getByTestId("fee-gauge-needle").getAttribute("data-position");
    // The sample is 19.99 times its floor: on the red band, and the needle two thirds round (a decade per half turn).
    const { rerender } = render(<LiveHeroView network="robinhood" snapshot={snapshot} values={null} blocks={[]} nowMs={Date.parse(snapshot.sampledAt)} status="open" />);
    const gauge = screen.getByTestId("fee-gauge");
    expect(reservedWidth(within(gauge).getByText("19.99"))).toBe("5ch");
    expect(within(gauge).getByTestId("fee-gauge-multiplier")).toHaveClass("text-dial-critical");
    expect(within(gauge).getByText("over floor")).toBeInTheDocument();
    expect(gauge.querySelectorAll("[data-band]")).toHaveLength(3);
    expect(gauge.querySelector("[data-band='warning']")).toHaveAttribute("stroke", "var(--dial-band-warning)");
    // Past the last threshold every band is lit, and the reading cuts the last one short.
    expect(gauge.querySelectorAll("[data-lit]")).toHaveLength(3);
    expect(position(gauge)).toBe("0.6504");
    // The base fee itself is inside the gauge now, and nothing but the needle is drawn inside the arc.
    expect(within(gauge).getByText("0.3997")).toBeInTheDocument();
    // The working is the hover note, and the description says the same to a screen reader.
    expect(within(gauge).getByText("19.99× the 0.02 gwei floor")).toBeInTheDocument();
    expect(within(gauge).getByText("0.3997 gwei ≈ 19.99 × 0.02 gwei")).toBeInTheDocument();
    expect(within(gauge).getByText("19.99 times the 0.02 gwei floor, far above the floor.")).toBeInTheDocument();
    // The arc is decoration: everything it shows is in the readout below it, so it is not announced twice.
    expect(gauge.querySelector("svg")).toHaveAttribute("aria-hidden", "true");
    // At the floor the needle rests at the left end, in the green, and no stretch of the ring is lit.
    rerender(<LiveHeroView network="robinhood" snapshot={snapshot} values={at(1)} blocks={[]} nowMs={Date.parse(snapshot.sampledAt)} status="open" />);
    expect(within(gauge).getByTestId("fee-gauge-multiplier")).toHaveClass("text-dial-good");
    expect(position(gauge)).toBe("0.0000");
    expect(gauge.querySelectorAll("[data-lit]")).toHaveLength(0);
    // Ten times the floor is the top of the arc, still amber, with the green and amber bands lit.
    rerender(<LiveHeroView network="robinhood" snapshot={snapshot} values={at(10)} blocks={[]} nowMs={Date.parse(snapshot.sampledAt)} status="open" />);
    expect(within(gauge).getByTestId("fee-gauge-multiplier")).toHaveClass("text-dial-warning");
    expect(position(gauge)).toBe("0.5000");
    expect(gauge.querySelectorAll("[data-lit]")).toHaveLength(2);
    rerender(<LiveHeroView network="robinhood" snapshot={snapshot} values={at(10.01)} blocks={[]} nowMs={Date.parse(snapshot.sampledAt)} status="open" />);
    expect(within(gauge).getByTestId("fee-gauge-multiplier")).toHaveClass("text-dial-critical");
    // The tone follows the printed figure: 10.004 prints as "10.00×", which is still amber.
    rerender(<LiveHeroView network="robinhood" snapshot={snapshot} values={at(10.004)} blocks={[]} nowMs={Date.parse(snapshot.sampledAt)} status="open" />);
    expect(within(gauge).getByText("10.00")).toBeInTheDocument();
    expect(within(gauge).getByTestId("fee-gauge-multiplier")).toHaveClass("text-dial-warning");
    // A thousandfold spike pins the needle at the right end rather than swinging it off the dial.
    rerender(<LiveHeroView network="robinhood" snapshot={snapshot} values={at(1000)} blocks={[]} nowMs={Date.parse(snapshot.sampledAt)} status="open" />);
    expect(position(gauge)).toBe("1.0000");
    // The floor and x moved into the readout with the figure they qualify.
    expect(within(gauge).getByText(/floor 0.02 gwei/)).toHaveTextContent("floor 0.02 gwei · x 3.2425");
  });
  it("keeps the lagging-collector pill set as a readout, like the stats beside it", () => {
    const stale = { ...snapshot, sampledAt: new Date(Date.parse(snapshot.sampledAt) - 120_000).toISOString() };
    render(<LiveHeroView network="robinhood" snapshot={stale} values={null} blocks={[]} nowMs={Date.parse(snapshot.sampledAt)} status="open" />);
    const pill = screen.getByText(/collector lagging/);
    expect(pill.closest(".vw-stat-panel")).not.toBeNull();
  });
  it("bands the rail into what the chain is doing and what it costs", () => {
    render(<LiveHeroView network="robinhood" snapshot={snapshot} values={null} blocks={[]} nowMs={Date.parse(snapshot.sampledAt)} status="open" />);
    expect(screen.getByText("Chain")).toBeInTheDocument();
    expect(screen.getByText("What it costs")).toBeInTheDocument();
  });
  it("renders the eased figures rather than the sample when a frame has them", () => {
    const values = { ...targetValues(snapshot, [], 0), baseFeeGwei: 0.5, multiplier: 25, gasPerSecond10: 41_000_000, transferEth: 1.05e-5, exponent: 3.3 };
    render(<LiveHeroView network="robinhood" snapshot={snapshot} values={values} blocks={[]} nowMs={Date.parse(snapshot.sampledAt)} status="open" />);
    expect(screen.getByText("0.5000")).toBeInTheDocument();
    expect(screen.getByText("25.00")).toBeInTheDocument();
    expect(screen.getByText("41.0")).toBeInTheDocument();
    expect(screen.getByText("0.0000105")).toBeInTheDocument();
    expect(screen.getByText("3.3000")).toBeInTheDocument();
  });
  it("subscribes to the frame store", () => {
    const frame = createFrameStore({ nowMs: Date.parse(snapshot.sampledAt) });
    render(<LiveHero network="robinhood" live={{ display: snapshot, frame, resyncing: false }} status="open" model="constraints" />);
    expect(screen.getByText("0.3997")).toBeInTheDocument();
    act(() => frame.set({ blocks: [], places: NO_PLACES, values: { ...targetValues(snapshot, [], 0), baseFeeGwei: 0.75 }, nowMs: Date.parse(snapshot.sampledAt) }));
    expect(screen.getByText("0.7500")).toBeInTheDocument();
    expect(screen.getByText(/0 blocks · /)).toBeInTheDocument();
  });
  it("charts the block ring against the floor, with a relative time axis", () => {
    render(<LiveHeroView network="robinhood" snapshot={snapshot} values={null} blocks={sawtoothBlocks(2, snapshot.block.ts)} nowMs={Date.parse(snapshot.sampledAt)} status="open" />);
    const chart = screen.getByRole("figure", { name: "Base fee per block over the last 120 seconds, 20 blocks, 0.3997 to 0.3997 gwei, with the floor at 0.0200 gwei" });
    // Always two minutes, whatever the ring covers, so a filling ring never rescales the axis; it reads in relative time.
    expect(within(chart).getByText("-2:00")).toBeInTheDocument();
    expect(within(chart).getByText("-0:30")).toBeInTheDocument();
    expect(within(chart).getByText("now")).toBeInTheDocument();
    // The gwei axis and the cyan floor rule, which is a threshold and so the one dashed mark.
    expect(within(chart).getByText("floor 0.02 gwei")).toBeInTheDocument();
    const floor = chart.querySelector("line.recharts-reference-line-line");
    expect(floor).toHaveAttribute("stroke", "var(--floor)");
    expect(floor).toHaveAttribute("stroke-dasharray", "4 3");
    // A flat low-alpha tint, never a gradient, on the one data mark.
    const area = chart.querySelector("path.recharts-area-area");
    expect(area).toHaveAttribute("fill", "var(--accent)");
    expect(area).toHaveAttribute("fill-opacity", "0.12");
    // The chart is 180 px on a phone and 260 px from lg up.
    expect(chart.firstElementChild).toHaveClass("h-[180px]");
    expect(chart.firstElementChild).toHaveClass("lg:h-[260px]");
    // The per-block gas strip that used to sit under it is gone: the hero
    // keeps the one big chart.
    expect(screen.queryByRole("img", { name: /Gas used per block/ })).toBeNull();
  });
  it("shades the part of a range that was never indexed, over the whole window", () => {
    // A day's window with two minutes of buckets in it: the axis is the window,
    // and the hour before the first bucket is shaded and labelled.
    const early = { ...history, from: history.points[0].t - 3600 };
    const { container } = render(<LiveHeroView network="robinhood" snapshot={snapshot} values={null} blocks={[]} nowMs={Date.parse(snapshot.sampledAt)} status="open" range="24h" series={early} model="constraints" />);
    const band = container.querySelector('rect[data-band="gap"]');
    expect(band).not.toBeNull();
    expect(band).toHaveAttribute("fill", "var(--ink-3)");
    expect(band).toHaveAttribute("fill-opacity", "0.1");
    expect(screen.getAllByText("not indexed yet").length).toBeGreaterThan(0);
    expect(screen.getAllByText("Shaded: not indexed yet, history before 2026-09-06 02:20 CDT").length).toBeGreaterThan(0);
  });
  it("draws the chain's throughput under the fee, on the hero's own range", () => {
    // Six seconds of blocks: the newest second is still being delivered and is
    // left out, so a shorter ring has nothing complete to draw.
    const blocks = sawtoothBlocks(6, snapshot.block.ts);
    const { rerender } = render(<LiveHeroView network="robinhood" snapshot={snapshot} values={null} blocks={blocks} nowMs={Date.parse(snapshot.sampledAt)} status="open" range="live" />);
    // Live: whole seconds of blocks from the ring, in one unit named beside the chart.
    const throughput = screen.getByRole("figure", { name: /^Compute gas carried per second over the last 120 seconds/ });
    expect(throughput.firstElementChild).toHaveClass("h-[120px]");
    expect(throughput.firstElementChild).toHaveClass("lg:h-[140px]");
    // The caption leads with the plain reading and keeps the measurement after it.
    expect(screen.getByText("Network load, second by second, over the last 120 s")).toBeInTheDocument();
    expect(screen.getByText(/compute gas per second across the chain · Mgas\/s/)).toBeInTheDocument();
    expect(screen.getByText("Base fee, block by block, over the last 120 s")).toBeInTheDocument();
    // Its own enlarge control, at the range on screen.
    expect(screen.getByRole("link", { name: "Open Gas throughput enlarged" })).toHaveAttribute("href", "/robinhood/charts/gas-per-second?range=live");

    // A history range: the bucketed rate against every target in force.
    rerender(<LiveHeroView network="robinhood" snapshot={snapshot} values={null} blocks={blocks} nowMs={Date.parse(snapshot.sampledAt)} status="open" range="24h" series={history} model="constraints" />);
    expect(screen.getByRole("figure", { name: /^Compute gas used per second in .* with each constraint target/ })).toBeInTheDocument();
    expect(screen.getByText("Network load per bucket against each target in force, 24h")).toBeInTheDocument();
    expect(screen.getByText(/^· compute gas per second · /)).toBeInTheDocument();
    // The band the api measured is named in the caption, because what it is a spread of changes with the range.
    const banded = { ...history, spreadSeconds: 1, points: history.points.map((p) => ({ ...p, computeGasPerSecondMin: 1, computeGasPerSecondMax: 9 })) };
    rerender(<LiveHeroView network="robinhood" snapshot={snapshot} values={null} blocks={blocks} nowMs={Date.parse(snapshot.sampledAt)} status="open" range="24h" series={banded} model="constraints" />);
    expect(screen.getByText(/· min to max per second$/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open Gas throughput enlarged" })).toHaveAttribute("href", "/robinhood/charts/gas-per-second?range=24h");
  });
  it("draws the canonical blocks after a reorg, not the orphaned ones", () => {
    const ring = sawtoothBlocks(2, snapshot.block.ts);
    const head = ring[ring.length - 1].number;
    const canonical = ring.slice(-3).map((b) => ({ ...b, baseFee: "800000000" }));
    const blocks = applyReorg(ring, head - 3, canonical);
    expect(blocks).toHaveLength(20);
    const { rerender } = render(<LiveHeroView network="robinhood" snapshot={snapshot} values={null} blocks={ring} nowMs={Date.parse(snapshot.sampledAt)} status="open" />);
    expect(screen.getByRole("figure", { name: /0.3997 to 0.3997 gwei/ })).toBeInTheDocument();
    rerender(<LiveHeroView network="robinhood" snapshot={snapshot} values={null} blocks={blocks} nowMs={Date.parse(snapshot.sampledAt)} status="open" />);
    expect(screen.getByRole("figure", { name: /0.3997 to 0.8000 gwei/ })).toBeInTheDocument();
  });
  it("keeps the hero clear of inspectors and tables, at every range", async () => {
    const { rerender } = render(
      <LiveHeroView
        network="robinhood"
        snapshot={snapshot}
        values={null}
        blocks={sawtoothBlocks(5, snapshot.block.ts)}
        nowMs={Date.parse(snapshot.sampledAt)}
        status="open"
        range="live"
      />,
    );
    // The top box is the figures and the two charts, nothing else: the
    // inspector and the table belong to the enlarged view, which has room.
    expect(screen.queryByRole("slider")).toBeNull();
    expect(screen.queryByText("Block inspector")).toBeNull();
    expect(screen.queryByText("Second inspector")).toBeNull();
    expect(screen.queryByText(/as a table/)).toBeNull();
    // Still the charts themselves, and the enlarge link that leads to them.
    expect(screen.getByRole("figure", { name: /^Base fee per block/ })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Open Base fee enlarged/ })).toBeInTheDocument();
    rerender(
      <LiveHeroView
        network="robinhood"
        snapshot={snapshot}
        values={null}
        blocks={[]}
        nowMs={Date.parse(snapshot.sampledAt)}
        status="open"
        range="24h"
        series={history}
        model="constraints"
      />,
    );
    expect(screen.queryByRole("slider")).toBeNull();
    expect(screen.queryByText(/as a table/)).toBeNull();
  });

  it("reads every block out without a pointer once the chart is enlarged", async () => {
    render(
      <HeroChartPanel
        readout
        snapshot={snapshot}
        blocks={sawtoothBlocks(5, snapshot.block.ts)}
        nowMs={Date.parse(snapshot.sampledAt)}
        range="live"
        series={null}
      />,
    );
    // A slider picks a block, which gives arrow keys, Home and End for free,
    // and the values are read out in a live region.
    const slider = screen.getByRole("slider", { name: "Select a block to read its values" });
    expect(screen.getByText("Block inspector")).toBeInTheDocument();
    expect(screen.getAllByText("base fee").length).toBeGreaterThan(0);
    // The whole series is available as a table for a reader who wants all of it.
    await userEvent.click(screen.getByText(/Base fee per block, as a table/));
    expect(screen.getByRole("table", { name: /Every block of the live base fee chart/ })).toBeInTheDocument();
    fireEvent.change(slider, { target: { value: "0" } });
    expect(slider).toHaveValue("0");
  });

  it("reads a bucketed range out without a pointer once enlarged", async () => {
    render(<HeroChartPanel readout snapshot={snapshot} blocks={[]} nowMs={Date.parse(snapshot.sampledAt)} range="24h" series={history} model="constraints" />);
    expect(screen.getByRole("slider", { name: "Select a bucket to read its values" })).toBeInTheDocument();
    await userEvent.click(screen.getByText(/Base fee over 24h, as a table/));
    expect(screen.getByRole("table", { name: /Every bucket of the base fee chart over 24h/ })).toBeInTheDocument();
  });

  it("names what a hovered block carried", () => {
    const rows = heroTooltipRows();
    const row = { number: 55_812_345, fee: 0.3997, gasUsed: 4_021_130, ts: 1788679199 };
    expect(rows.map((r) => r.label)).toEqual(["block", "base fee", "gas used", "time"]);
    expect(rows.map((r) => r.value(row))).toEqual(["55,812,345", "0.3997 gwei", "4.02 Mgas", "02:19:59"]);
  });
  it("keeps the chart's frame, at its full height, before any block has arrived", () => {
    const { container } = render(<HeroChart points={[]} floorGwei={0.02} floorText="0.02" />);
    expect(container.querySelector("svg")).toBeNull();
    const empty = screen.getByRole("figure", { name: "Base fee per block, waiting for blocks" });
    expect(empty.firstElementChild).toHaveClass("h-[180px]");
    expect(empty.firstElementChild).toHaveClass("lg:h-[260px]");
    expect(screen.getByText("Waiting for blocks.")).toBeInTheDocument();
  });
  it("swaps the chart body for the chosen range and puts the live ring back, leaving the figures on the left alone", async () => {
    const onRangeChange = vi.fn();
    const { rerender } = render(<LiveHeroView network="robinhood" snapshot={snapshot} values={null} blocks={sawtoothBlocks(2, snapshot.block.ts)} nowMs={Date.parse(snapshot.sampledAt)} status="open" range="live" onRangeChange={onRangeChange} />);
    expect(screen.getByRole("button", { name: "Live" })).toHaveAttribute("aria-pressed", "true");
    await userEvent.click(screen.getByRole("button", { name: "24h" }));
    expect(onRangeChange).toHaveBeenCalledWith("24h");

    rerender(<LiveHeroView network="robinhood" snapshot={snapshot} values={null} blocks={sawtoothBlocks(2, snapshot.block.ts)} nowMs={Date.parse(snapshot.sampledAt)} status="open" range="24h" onRangeChange={onRangeChange} series={history} model="constraints" />);
    const chart = screen.getByRole("figure", { name: /^Base fee over 24h on a log scale, 2 buckets/ });
    expect(chart.firstElementChild).toHaveClass("h-[180px]");
    expect(chart.firstElementChild).toHaveClass("lg:h-[260px]");
    expect(screen.getByText("Base fee average with the min to max band · 24h · 2 buckets")).toBeInTheDocument();
    // The band, the average and the stepped floor, plus the owner-action marker.
    expect(chart.querySelectorAll("path.recharts-area-area")).toHaveLength(2);
    expect(chart.querySelectorAll("path.recharts-line-curve")).toHaveLength(2);
    expect(chart.querySelector("line.recharts-reference-line-line")).toHaveAttribute("stroke", "var(--marker)");
    // The live figures never stopped: the block, the fee and the gas rate are still the sample's.
    expect(screen.getByText("0.3997")).toBeInTheDocument();
    expect(screen.getByText("55,812,345")).toBeInTheDocument();
    expect(screen.getByText("38.0")).toBeInTheDocument();

    rerender(<LiveHeroView network="robinhood" snapshot={snapshot} values={null} blocks={sawtoothBlocks(2, snapshot.block.ts)} nowMs={Date.parse(snapshot.sampledAt)} status="open" range="live" onRangeChange={onRangeChange} series={history} model="constraints" />);
    expect(screen.getByRole("figure", { name: /^Base fee per block over the last 120 seconds/ })).toBeInTheDocument();
    expect(screen.queryByRole("figure", { name: /log scale/ })).toBeNull();
  });

  it("keeps the chart box at one height while a range loads, when it is empty and when it fails", () => {
    // The hero holds two charts now, the base fee and the throughput under it;
    // the first figure is the base fee's box.
    const box = () => screen.getAllByRole("figure")[0].firstElementChild;
    const props = { network: "robinhood", snapshot, values: null, blocks: [], nowMs: Date.parse(snapshot.sampledAt), status: "open" as const, range: "30d" as const, model: "constraints" as const };
    // Both boxes say the same thing while the range loads: the base fee and the
    // throughput under it are one range control.
    const throughputBox = () => screen.getAllByRole("figure")[1].firstElementChild;
    const { rerender } = render(<LiveHeroView {...props} series={null} seriesLoading />);
    expect(screen.getAllByText("Loading 30d.")).toHaveLength(2);
    expect(box()).toHaveClass("h-[180px]");
    expect(box()).toHaveClass("lg:h-[260px]");
    expect(throughputBox()).toHaveClass("h-[120px]");
    expect(throughputBox()).toHaveClass("lg:h-[140px]");

    rerender(<LiveHeroView {...props} series={{ ...history, points: [] }} />);
    expect(screen.getAllByText("Nothing indexed for this range yet.").length).toBeGreaterThan(0);
    expect(box()).toHaveClass("h-[180px]");

    rerender(<LiveHeroView {...props} series={null} seriesError="boom" />);
    expect(screen.getAllByText(/Could not load 30d: boom/)).toHaveLength(2);
    expect(box()).toHaveClass("h-[180px]");
    expect(box()).toHaveClass("lg:h-[260px]");
  });

  it("comes back to the range this browser last chose, asks for that range only, and remembers a new one", async () => {
    setHeroRange("24h");
    seriesMock.calls.length = 0;
    const frame = createFrameStore({ nowMs: Date.parse(snapshot.sampledAt) });
    render(<LiveHero network="robinhood" live={{ display: snapshot, frame, resyncing: false }} status="open" model="constraints" />);
    expect(screen.getByRole("button", { name: "24h" })).toHaveAttribute("aria-pressed", "true");
    expect(seriesMock.calls.at(-1)).toEqual(["robinhood", "24h"]);
    // Back to Live: nothing is asked of the api at all.
    await userEvent.click(screen.getByRole("button", { name: "Live" }));
    expect(window.localStorage.getItem(HERO_RANGE_KEY)).toBe("live");
    expect(seriesMock.calls.at(-1)).toEqual([null, null]);
  });
  it("waits for the first sample, and says so differently while a reorg is being repaired", () => {
    const { rerender } = render(<LiveHero network="robinhood" live={{ display: null, frame: createFrameStore(), resyncing: false }} status="connecting" model="constraints" />);
    expect(screen.getByText("Waiting for the first sample.")).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("connecting");
    // A reorg took the last canonical state away: the hero must not claim it
    // is waiting for a first sample, and must show no orphaned figures.
    rerender(<LiveHero network="robinhood" live={{ display: null, frame: createFrameStore(), resyncing: true }} status="open" model="constraints" />);
    expect(screen.getByText("Resyncing after a reorg.")).toBeInTheDocument();
    expect(screen.queryByText("Waiting for the first sample.")).toBeNull();
  });
  it("shows the chain's cadence while the sample is fresh and a collector lag warning once it is stale", () => {
    // The block closed at 07:19:59Z and was sampled at 07:20:00Z; three seconds later both are fresh.
    const { rerender } = render(<LiveHeroView network="robinhood" snapshot={snapshot} values={null} blocks={[]} nowMs={Date.parse("2026-09-06T07:20:03Z")} status="open" />);
    expect(screen.getByText("Since last block")).toBeInTheDocument();
    expect(screen.getByText("4.0")).toBeInTheDocument();
    expect(screen.queryByText(/collector lagging/)).toBeNull();
    // Twelve seconds after the sample the number would read as a chain stall; it is the collector that is behind.
    rerender(<LiveHeroView network="robinhood" snapshot={snapshot} values={null} blocks={[]} nowMs={Date.parse("2026-09-06T07:20:12.4Z")} status="open" />);
    const pill = screen.getByText("collector lagging 12 s");
    expect(pill).toHaveClass("text-warning");
    expect(pill).toHaveAttribute("title", expect.stringContaining("12 s old"));
    expect(screen.getByText("Since last block")).toBeInTheDocument();
    expect(screen.queryByText("13.4")).toBeNull();
  });
  it("prices the transfer and swap tiles in dollars when the quote is fresh, keeping the working on hover and in the description", () => {
    const priced = { ...snapshot, ethUsd: { price: "4200.00", at: "2026-09-06T07:15:00Z", source: "coingecko" } };
    render(<LiveHeroView network="robinhood" snapshot={priced} values={null} blocks={[]} nowMs={Date.parse(snapshot.sampledAt)} status="open" />);
    // 21,000 gas at 0.3997 gwei is 0.0000084 ETH: about four cents.
    expect(reservedWidth(screen.getByText("0.04"))).toBe("6ch");
    expect(screen.getByText("0.25")).toBeInTheDocument();
    // The dollar sign is outside the reserved box, so a changing digit cannot move it.
    expect(screen.getByText("0.04").previousSibling).toHaveTextContent("$");
    // The ETH figure is never lost, and the quote that priced it is named: the note and the accessible description both carry the working.
    expect(screen.getByText("0.00000839 ETH × $4,200.0/ETH = $0.04")).toBeInTheDocument();
    // Both tiles name the quote that priced them.
    expect(screen.getAllByText("coingecko, 5 min ago")).toHaveLength(2);
    expect(screen.getByText("0.04 US dollars, 0.00000839 ETH at 4,200.0 dollars per ETH, quoted by coingecko 5 min ago")).toBeInTheDocument();
    expect(screen.queryByText("0.00000839")).toBeNull();
    // The two tiles sit side by side, so the right-hand note opens leftwards to stay inside the card.
    expect(screen.getByText("0.25").closest(".group")?.querySelector(".right-0")).not.toBeNull();
  });
  it("falls back to ETH when there is no quote at all and when the one there is has gone stale", () => {
    // Eleven minutes old: past the ten minute cutoff, so the fee has moved on and the price has not.
    const stale = { ...snapshot, ethUsd: { price: "4200.00", at: "2026-09-06T07:09:00Z", source: "coingecko" } };
    const { rerender } = render(<LiveHeroView network="robinhood" snapshot={stale} values={null} blocks={[]} nowMs={Date.parse(snapshot.sampledAt)} status="open" />);
    expect(reservedWidth(screen.getByText("0.00000839"))).toBe("10ch");
    expect(screen.queryByText("0.04")).toBeNull();
    // And with no quote the tiles are exactly what they were before there was one.
    rerender(<LiveHeroView network="robinhood" snapshot={snapshot} values={null} blocks={[]} nowMs={Date.parse(snapshot.sampledAt)} status="open" />);
    expect(screen.getByText("0.00000839")).toBeInTheDocument();
    expect(screen.getByText("0.0000600")).toBeInTheDocument();
  });
  it("shows a cost tile in ETH without a price and in dollars with one, hovering to the arithmetic", () => {
    const now = Date.parse("2026-09-06T07:20:00Z");
    const { rerender } = render(<CostTile label="21k transfer" eth={0.0000084} ethUsd={null} nowMs={now} />);
    expect(screen.getByText("0.00000840")).toBeInTheDocument();
    // No quote, no note: an ETH figure has no arithmetic to show.
    expect(document.querySelector(".cursor-help")).toBeNull();
    rerender(<CostTile label="21k transfer" eth={0.0000084} ethUsd={{ price: "4200.00", at: "2026-09-06T07:19:26Z", source: "coinbase" }} nowMs={now} />);
    // The dollars are what is drawn; the multiplication and the quote behind it
    // are in the note the trigger opens, not a title the reader cannot see.
    const usd = screen.getByText("0.04");
    expect(usd.closest("[title]")).toBeNull();
    expect(screen.getByText("0.00000840 ETH × $4,200.0/ETH = $0.04")).toBeInTheDocument();
    expect(screen.getByText("coinbase, 34 s ago")).toBeInTheDocument();
    expect(screen.getByText("0.04 US dollars, 0.00000840 ETH at 4,200.0 dollars per ETH, quoted by coinbase 34 s ago")).toBeInTheDocument();
    // And the figure says it is inspectable rather than leaving the reader to guess.
    const trigger = usd.closest(".cursor-help");
    expect(trigger).not.toBeNull();
    expect(trigger).toHaveAttribute("tabindex", "0");
  });
  it("closes the note on Escape and offers it again the next time the reader asks for it", async () => {
    const now = Date.parse("2026-09-06T07:20:00Z");
    render(<CostTile label="21k transfer" eth={0.0000084} ethUsd={{ price: "4200.00", at: "2026-09-06T07:19:26Z", source: "coinbase" }} nowMs={now} />);
    const trigger = screen.getByText("0.04").closest(".cursor-help") as HTMLElement;
    const note = screen.getByText("0.00000840 ETH × $4,200.0/ETH = $0.04").parentElement?.parentElement as HTMLElement;
    // Hover and focus are what open it, so those are the classes to watch.
    const opens = () => note.className.includes("group-hover:block");
    trigger.focus();
    expect(opens()).toBe(true);
    // WCAG 1.4.13 wants content shown on focus to be dismissable without moving focus.
    await userEvent.keyboard("{Escape}");
    expect(opens()).toBe(false);
    expect(trigger).toHaveFocus();
    // And asking for it again brings it back, rather than the tile losing its working for good.
    fireEvent.mouseEnter(trigger.parentElement as HTMLElement);
    expect(opens()).toBe(true);
  });
  it("closes a note the pointer opened, where Escape never reaches the figure", async () => {
    const now = Date.parse("2026-09-06T07:20:00Z");
    render(<CostTile label="21k transfer" eth={0.0000084} ethUsd={{ price: "4200.00", at: "2026-09-06T07:19:26Z", source: "coinbase" }} nowMs={now} />);
    const group = (screen.getByText("0.04").closest(".cursor-help") as HTMLElement).parentElement as HTMLElement;
    const note = screen.getByText("0.00000840 ETH × $4,200.0/ETH = $0.04").parentElement?.parentElement as HTMLElement;
    const opens = () => note.className.includes("group-hover:block");
    // Hovering moves no focus, so the key goes to the body, not to the figure.
    fireEvent.mouseEnter(group);
    expect(opens()).toBe(true);
    expect(document.body).toHaveFocus();
    await userEvent.keyboard("{Escape}");
    expect(opens()).toBe(false);
    // The pointer never moved, which is what the requirement is about.
    fireEvent.mouseLeave(group);
    fireEvent.mouseEnter(group);
    expect(opens()).toBe(true);
  });
  it("measures the sample age from the wall clock", () => {
    expect(COLLECTOR_LAG_S).toBe(5);
    expect(sampleAge("2026-09-06T07:20:00Z", Date.parse("2026-09-06T07:20:07.9Z"))).toBeCloseTo(7.9);
    expect(sampleAge("2026-09-06T07:20:00Z", Date.parse("2026-09-06T07:19:00Z"))).toBe(0);
    expect(sampleAge("not a date", 1)).toBe(0);
  });
});

describe("ConstraintCards", () => {
  it("renders one card per constraint with its share of x, the sample standing in before any frame", () => {
    render(<ConstraintCardsView network="robinhood" snapshot={snapshot} values={null} blocks={[]} />);
    expect(screen.getByText("Constraint 1")).toBeInTheDocument();
    expect(screen.getByText("Constraint 2")).toBeInTheDocument();
    expect(screen.getByText("99.9%").parentElement).toHaveTextContent("99.9% of x");
    expect(screen.getByText("0.1%").parentElement).toHaveTextContent("0.1% of x");
    expect(screen.getAllByRole("meter")).toHaveLength(2);
    // The unit is always on the figure, and under it what the figure means:
    // how long the chain must run at exactly the target to drain it.
    const backlog = screen.getByText("11.2").closest("dd");
    expect(backlog).toHaveTextContent("11.2 Tgas");
    expect(backlog?.parentElement).toHaveAttribute("title", BACKLOG_TITLE);
    expect(screen.getByText("= 77.7 h at 40 Mgas/s")).toBeInTheDocument();
    expect(screen.getByText("= 0.05 s at 60 Mgas/s")).toBeInTheDocument();
    // The 15 s window is short: averaged, with its note, but no sparkline until blocks arrive.
    expect(screen.getByText("Backlog (avg 2 s)")).toBeInTheDocument();
    expect(screen.getAllByText("Backlog")).toHaveLength(1);
    expect(screen.getByText(/drains 60 Mgas\/s at each second boundary/)).toBeInTheDocument();
    expect(screen.queryByRole("figure", { name: /backlog per block/ })).toBeNull();
    expect(screen.getByText(/Windows of 1 min or less are shown as a 2 s average/)).toBeInTheDocument();
    expect(screen.getByText("3.11")).toBeInTheDocument();
    expect(screen.getAllByText("Mgas/s")).toHaveLength(2);
  });
  it("charts the raw sawtooth of a short window against its drain threshold, and shows the eased figures", () => {
    const blocks = sawtoothBlocks(20, snapshot.block.ts);
    const values = { ...targetValues(snapshot, blocks, 0), backlogs: [22_000_000, 11_100_000_000_000], bips: [244.4, 32_118.5] };
    render(<ConstraintCardsView network="robinhood" snapshot={snapshot} values={values} blocks={blocks} />);
    // The label names the span, the scale and the threshold, so the chart is
    // readable without seeing it: the peak is 40M, the threshold 60M, and the
    // axis tops out at the larger of the two.
    const chart = screen.getByRole("figure", {
      name: "Constraint 1 backlog per block over the last 15 s, 150 blocks, 0 to 80 Mgas, with a dashed threshold at 60 Mgas: it drains 60 Mgas/s at each second boundary",
    });
    // One thin line: the per-block backlog.
    expect(chart.querySelectorAll("path.recharts-curve.recharts-line-curve")).toHaveLength(1);
    // The y axis reads in gas with the SI prefix on the unit, the x axis in seconds before now.
    expect(within(chart).getByText("40 Mgas")).toBeInTheDocument();
    expect(within(chart).getByText("80 Mgas")).toBeInTheDocument();
    expect(within(chart).getByText("-15s")).toBeInTheDocument();
    expect(within(chart).getByText("now")).toBeInTheDocument();
    // And the threshold line carries its own label.
    expect(within(chart).getByText("drains 60 Mgas/s at each second")).toBeInTheDocument();
    expect(screen.getByText("22.0")).toBeInTheDocument();
    expect(screen.getByText("11.1")).toBeInTheDocument();
    expect(screen.getByText("0.0244")).toBeInTheDocument();
    expect(screen.getByText("0.0244").closest("div")).toHaveTextContent("backlog / (target × window), 244 bips");
    expect(screen.getByText("3.2119").closest("div")).toHaveTextContent("backlog / (target × window), 32,119 bips");
    // The unit says what it means rather than leaving the reader to guess: the
    // note gives the figure in ordinary decimal, and the same facts are in the
    // description for a reader who never gets a hover.
    expect(screen.getByText("244 bips = 0.0244")).toBeInTheDocument();
    expect(screen.getByText("32,119 bips = 3.2119")).toBeInTheDocument();
    expect(screen.getByText("244 bips is 0.0244. basis points: 1 bip is 1/10,000. The pricer holds these as integers, never as floats.")).toBeInTheDocument();
    // Focus opens the note as hover does, and nothing is left on a `title` the reader cannot see.
    const bips = screen.getByText("244 bips = 0.0244").closest(".group")?.querySelector(".cursor-help");
    expect(bips).not.toBeNull();
    expect(bips).toHaveAttribute("tabindex", "0");
    expect(screen.getByText("0.0244").closest("[title]")).toBeNull();
    // The x cell is the right-hand column, so its note opens leftwards to stay
    // inside the card, and it anchors to the line rather than to the word:
    // against the word a 42ch panel runs off the right of a 375px screen.
    expect(screen.getByText("244 bips = 0.0244").closest(".right-0")).not.toBeNull();
    expect(screen.getByText("244 bips = 0.0244").closest("dd")).toHaveClass("relative");
  });
  it("keeps the note reachable by pointer and dismissible by keyboard", async () => {
    const blocks = sawtoothBlocks(20, snapshot.block.ts);
    const values = { ...targetValues(snapshot, blocks, 0), backlogs: [22_000_000, 11_100_000_000_000], bips: [244.4, 32_118.5] };
    render(<ConstraintCardsView network="robinhood" snapshot={snapshot} values={values} blocks={blocks} />);
    const panel = screen.getByText("244 bips = 0.0244").closest(".absolute") as HTMLElement;
    // The pointer has to be able to reach the note it opened: the gap between
    // the figure and the panel is padding on a hoverable box, not a margin
    // across which `:hover` would be lost.
    expect(panel).not.toHaveClass("pointer-events-none");
    expect(panel).toHaveClass("pb-2");
    expect(panel).toHaveClass("group-hover:block");
    // And a keyboard reader can put it away without moving focus, which is what
    // WCAG 1.4.13 asks of content that opens on focus.
    const trigger = screen.getByText("244 bips = 0.0244").closest(".group")?.querySelector(".cursor-help") as HTMLElement;
    trigger.focus();
    await userEvent.keyboard("{Escape}");
    expect(panel).not.toHaveClass("group-hover:block");
    expect(panel).toHaveClass("hidden");
    // It is the note in the way that was dismissed, not the note as such: the
    // next hover brings it back.
    fireEvent.mouseEnter(screen.getByText("244 bips = 0.0244").closest(".group") as HTMLElement);
    expect(panel).toHaveClass("group-hover:block");
  });
  it("subscribes to the frame store and says so when nothing contributes", () => {
    const frame = createFrameStore();
    render(<ConstraintCards network="robinhood" live={{ display: snapshot, frame, resyncing: false }} />);
    expect(screen.getByText("3.11")).toBeInTheDocument();
    act(() => frame.set({ blocks: [], places: NO_PLACES, values: { ...targetValues(snapshot, [], 0), backlogs: [0, 0], bips: [0, 0], shares: [0, 0] }, nowMs: 0 }));
    expect(screen.getAllByText("no contribution")).toHaveLength(2);
    expect(screen.getAllByText("0.0000")).toHaveLength(2);
  });
  it("renders the legacy card for legacy networks", () => {
    render(<ConstraintCardsView network="robinhood" snapshot={{ ...snapshot, model: "legacy", constraints: [], legacy: { speedLimit: 7_000_000, inertia: 102, tolerance: 10, backlog: 90_000_000 } }} values={null} blocks={[]} />);
    expect(screen.getByText(/Legacy pricer/)).toBeInTheDocument();
    expect(screen.getByText("70 Mgas free")).toBeInTheDocument();
    expect(screen.getByText(/x = 0.0280/)).toBeInTheDocument();
    expect(screen.getByText("90.0")).toBeInTheDocument();
    expect(screen.queryByText(/2 s average/)).toBeNull();
  });
  it("handles a missing snapshot, and names a reorg repair as one", () => {
    const { rerender } = render(<ConstraintCardsView network="robinhood" snapshot={null} values={null} blocks={[]} />);
    expect(screen.getByText("Waiting for the first sample.")).toBeInTheDocument();
    rerender(<ConstraintCardsView network="robinhood" snapshot={null} values={null} blocks={[]} resyncing />);
    expect(screen.getByText("Resyncing after a reorg.")).toBeInTheDocument();
  });
  it("draws nothing until there are two samples, and scales the axis to the threshold when the backlog stays under it", () => {
    const { container, rerender } = render(<Sawtooth samples={[{ number: 1, ts: 1, gasUsed: 1, backlog: 5 }]} color="red" target={60_000_000} index={0} />);
    expect(container.querySelector("svg")).toBeNull();
    rerender(
      <Sawtooth
        samples={[
          { number: 1, ts: 1, gasUsed: 4_000_000, backlog: 5_000_000 },
          { number: 2, ts: 2, gasUsed: 4_000_000, backlog: 10_000_000 },
        ]}
        color="red"
        target={60_000_000}
        index={1}
      />,
    );
    expect(screen.getByRole("figure", { name: /^Constraint 2 backlog per block/ })).toBeInTheDocument();
    expect(container.querySelector("path.recharts-line-curve")).toHaveAttribute("stroke", "red");
    // Ticks are zero, the midpoint and the top; the top is a round step above
    // the threshold (the tallest value here) so its label has headroom.
    expect(backlogAxis(60_000_000)).toEqual({ top: 80_000_000, ticks: [0, 40_000_000, 80_000_000] });
    expect(backlogAxis(100_000_000).top).toBe(125_000_000);
    expect(backlogAxis(0).top).toBeGreaterThan(0);
    expect(drainLabel(60_000_000)).toBe("drains 60 Mgas/s at each second");
  });
  it("reads a hovered block out as its number, its gas and the backlog it left", () => {
    const row = { number: 55_812_345, gasUsed: 4_021_130, backlog: 22_000_000 };
    render(
      <ChartTooltip
        active
        payload={[{ payload: row, value: 1, name: "backlog", dataKey: "backlog", graphicalItemId: "backlog" }]}
        label={-3.4}
        title={secondsAgoLabel}
        rows={sawtoothTooltipRows("red")}
      />,
    );
    expect(screen.getByText("3.4 s ago")).toBeInTheDocument();
    expect(screen.getByText("55,812,345")).toBeInTheDocument();
    expect(screen.getByText("4.02 Mgas")).toBeInTheDocument();
    expect(screen.getByText("22 Mgas")).toBeInTheDocument();
    expect(secondsAgoLabel(0)).toBe("now");
  });
});

describe("DataFooter", () => {
  it("describes the fast and slow collection cadences", () => {
    render(<DataFooter snapshot={null} series={null} networkInfo={null} status="open" apiStatus={null} now={0} />);
    const live = screen.getByText(/update on new heads when a WebSocket head feed is configured/);
    expect(live).toHaveTextContent("every 3 s by default on public RPC networks");
    expect(live).toHaveTextContent("separate slow sample every 60 s");
    expect(live).toHaveTextContent("pushed over a WebSocket (live)");
    expect(live).not.toHaveTextContent("one-second sample");
  });

  it("credits tirante.dev with an external link after the version line", () => {
    render(<DataFooter snapshot={null} series={null} networkInfo={null} status="open" apiStatus={{ version: "1.2.3", status: "healthy", networks: [] }} now={0} />);
    const link = screen.getByRole("link", { name: "powered by tirante.dev" });
    expect(link).toHaveAttribute("href", "https://tirante.dev");
    expect(link).toHaveAttribute("target", "_blank");
    expect(link.getAttribute("rel")).toContain("noopener");
    expect(link.parentElement).toHaveClass("text-label");
    const versions = screen.getByText("versions");
    expect(versions.compareDocumentPosition(link) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getByText(/api 1.2.3/)).toBeInTheDocument();
  });

  it("says off scale rather than quoting int64 max, and counts one bucket as one", () => {
    // The api saturates replay error at int64 max, which survives the wire but
    // not the parse into a double: printed it reads 9,223,372,036,854,776,000
    // bips, a figure neither the api nor the pricer ever produced.
    const saturated: Series = { ...history, points: [history.points[0], { ...history.points[1], replayErrorBips: 9_223_372_036_854_775_807 }] };
    render(<DataFooter snapshot={null} series={saturated} networkInfo={null} status="open" apiStatus={null} now={0} />);
    expect(screen.queryByText(/9,223,372,036,854,776,000/)).toBeNull();
    expect(screen.getByText("off scale")).toBeInTheDocument();
    expect(screen.getByText("past 2^53, where a browser's numbers stop being exact, so these are not the digits the api sent. The pricer's int64 ceiling saturates into this range.")).toBeInTheDocument();
    expect(screen.getByText("off scale: past 2^53, where a browser's numbers stop being exact, so these are not the digits the api sent. The pricer's int64 ceiling saturates into this range. basis points: 1 bip is 1/10,000. The pricer holds these as integers, never as floats.")).toBeInTheDocument();
    expect(screen.getByText("max in range").nextElementSibling).toHaveTextContent("(1 bucket above 2% is an estimate)");
  });

  it("says off scale below int64 too, wherever the digits stopped being the ones sent", () => {
    // 2^53 + 1 is a perfectly ordinary int64 that the api never saturated, and
    // it still parses as 9007199254740992: the cutoff is where a double stops
    // being exact, not where the pricer stops counting.
    const inexact: Series = { ...history, points: [history.points[0], { ...history.points[1], replayErrorBips: 9_007_199_254_740_993 }] };
    render(<DataFooter snapshot={null} series={inexact} networkInfo={null} status="open" apiStatus={null} now={0} />);
    expect(screen.getByText("off scale")).toBeInTheDocument();
    expect(screen.queryByText(/9,007,199,254,740,99/)).toBeNull();
  });

  it("keeps the plural when more than one bucket is estimated", () => {
    const estimated: Series = { ...history, points: history.points.map((p) => ({ ...p, replayErrorBips: 7_093 })) };
    render(<DataFooter snapshot={null} series={estimated} networkInfo={null} status="open" apiStatus={null} now={0} />);
    expect(screen.getByText("7,093 bips = 0.7093")).toBeInTheDocument();
    expect(screen.getByText("max in range").nextElementSibling).toHaveTextContent("(2 buckets above 2% are estimates)");
  });
});

describe("HistoryTabs", () => {
  it("marks the selected range and reports clicks", async () => {
    const onChange = vi.fn();
    render(<HistoryTabs range="24h" onChange={onChange} loading />);
    expect(screen.getByRole("button", { name: "24h" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByText("updating")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "All" }));
    expect(onChange).toHaveBeenCalledWith("all");
  });

  it("is a labelled button group, not a tablist it has no keyboard model for", () => {
    const { container } = render(<HistoryTabs range="24h" onChange={vi.fn()} loading />);
    // Nothing claims tab semantics, so no reader is promised arrow keys, Home
    // and End and a panel per tab that the control does not implement.
    expect(screen.queryByRole("tablist")).toBeNull();
    expect(screen.queryAllByRole("tab")).toHaveLength(0);
    const group = screen.getByRole("group", { name: "History range" });
    // Every option is in the tab order and says whether it is on.
    const options = within(group).getAllByRole("button");
    expect(options.map((b) => b.getAttribute("aria-pressed"))).toEqual(["false", "true", "false", "false"]);
    expect(options.every((b) => b.tabIndex === 0)).toBe(true);
    // It wraps inside its container rather than overflowing a phone.
    expect(group).toHaveClass("flex-wrap");
    expect(group).toHaveClass("max-w-full");
    // The status sits outside the group, so it cannot widen the control.
    const status = screen.getByText("updating");
    expect(group.contains(status)).toBe(false);
    expect(container.firstElementChild).toHaveClass("min-w-0");
  });
});

describe("NetworkSwitcher", () => {
  const networks: Network[] = [
    { name: "robinhood", displayName: "Robinhood Chain", chainId: 4663, explorerUrl: "", model: "constraints", headBlock: 1, headAt: null, lagSeconds: null, enabled: true },
    { name: "robinhood-testnet", displayName: "Robinhood Chain Testnet", chainId: 46630, explorerUrl: "", model: "constraints", headBlock: 1, headAt: null, lagSeconds: null, enabled: true },
    { name: "arbitrum-one", displayName: "Arbitrum One", chainId: 42161, explorerUrl: "", model: "constraints", headBlock: 1, headAt: "", lagSeconds: 0, enabled: false },
  ];
  it("lists the networks that are on and changes selection", async () => {
    const onChange = vi.fn();
    render(<NetworkSwitcher networks={networks} current="robinhood" onChange={onChange} loading={false} />);
    const select = screen.getByRole("combobox", { name: "Network" });
    expect(within(select).getAllByRole("option")).toHaveLength(2);
    expect(screen.queryByRole("option", { name: "Arbitrum One (42161)" })).not.toBeInTheDocument();
    await userEvent.selectOptions(select, "robinhood-testnet");
    expect(onChange).toHaveBeenCalledWith("robinhood-testnet");
  });
  it("keeps a disabled current network named while it is still the route", () => {
    render(<NetworkSwitcher networks={networks} current="arbitrum-one" onChange={vi.fn()} loading={false} />);
    expect(screen.getByRole("option", { name: "arbitrum-one" })).toBeInTheDocument();
    expect(screen.queryByRole("option", { name: "Arbitrum One (42161)" })).not.toBeInTheDocument();
  });
  it("keeps an unknown current network selectable while the list loads", () => {
    render(<NetworkSwitcher networks={null} current="mystery" onChange={vi.fn()} loading />);
    expect(screen.getByRole("option", { name: "mystery" })).toBeInTheDocument();
  });
});

describe("OwnerActionTimeline", () => {
  const actions: OwnerAction[] = [
    { block: 53_578_754, at: "2026-09-03T17:08:00Z", txHash: "0x" + "ab".repeat(32), method: "setGasPricingConstraints", selector: "0xcc0d556a", args: { constraints: [{ gasTargetPerSecond: 60_000_000, adjustmentWindowSeconds: 15, startingBacklog: 0 }, [40_000_000, 86_400, 9_989_000_000_000]] } },
    { block: 174_150, at: "2026-06-24T20:28:00Z", txHash: "0x" + "cd".repeat(32), method: "setMinimumL2BaseFee", selector: "0xa0188cdb", args: { priceInWei: "20000000" } },
    { block: 1, at: "2026-06-01T00:00:00Z", txHash: "0x" + "ef".repeat(32), method: "unknown", selector: "0x00000000", args: { raw: "0x" + "12".repeat(40), n: 3 } },
    { block: 2, at: "2026-06-02T00:00:00Z", txHash: "0x" + "01".repeat(32), method: "setGasPricingConstraints", selector: "0xcc0d556a", args: { constraints: ["oops"] } },
  ];
  it("decodes known methods and links to the explorer", () => {
    render(<OwnerActionTimeline actions={actions} explorerUrl="https://explorer.example/" loading={false} error={null} />);
    expect(screen.getByText("40 Mgas/s · 24 h · start 9.99 Tgas")).toBeInTheDocument();
    expect(screen.getByText("floor 0.02 gwei")).toBeInTheDocument();
    expect(screen.getByText(/raw: 0x12121212/)).toBeInTheDocument();
    expect(screen.getByText("n: 3")).toBeInTheDocument();
    expect(screen.getByText("oops")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "53,578,754" })).toHaveAttribute("href", "https://explorer.example/block/53578754");
    expect(screen.getByText("2026-09-03 17:08 UTC")).toBeInTheDocument();
  });
  it("shows loading, empty and error states", () => {
    const { rerender } = render(<OwnerActionTimeline actions={null} loading error={null} />);
    expect(screen.getByText("Loading owner actions.")).toBeInTheDocument();
    rerender(<OwnerActionTimeline actions={[]} loading={false} error={null} />);
    expect(screen.getByText(/No owner actions decoded/)).toBeInTheDocument();
    rerender(<OwnerActionTimeline actions={[]} loading={false} error="boom" />);
    expect(screen.getByText(/boom/)).toBeInTheDocument();
  });
});

describe("ThemeToggle", () => {
  it("cycles system, light, dark and stamps the root element", async () => {
    window.localStorage.removeItem("gascurve:theme");
    render(<ThemeToggle />);
    const button = screen.getByRole("button");
    expect(button).toHaveTextContent("theme: system");
    await userEvent.click(button);
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
    expect(readTheme()).toBe("light");
    await userEvent.click(button);
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
    expect(button).toHaveTextContent("theme: dark");
    await userEvent.click(button);
    expect(document.documentElement.hasAttribute("data-theme")).toBe(false);
    applyTheme("system");
  });

  it("applies another tab's choice to this document, not only to the control", async () => {
    window.localStorage.removeItem("gascurve:theme");
    applyTheme("system");
    render(<ThemeToggle />);
    const button = screen.getByRole("button");
    // Another tab stored dark: the page has to wear it, not just name it.
    window.localStorage.setItem("gascurve:theme", "dark");
    await act(async () => {
      window.dispatchEvent(new StorageEvent("storage", { key: "gascurve:theme", newValue: "dark" }));
    });
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
    expect(button).toHaveTextContent("theme: dark");
    // And a removal takes the page back to following the system.
    window.localStorage.removeItem("gascurve:theme");
    await act(async () => {
      window.dispatchEvent(new StorageEvent("storage", { key: "gascurve:theme", newValue: null }));
    });
    expect(document.documentElement.hasAttribute("data-theme")).toBe(false);
    expect(button).toHaveTextContent("theme: system");
    // An unrelated key changes nothing.
    document.documentElement.setAttribute("data-theme", "light");
    await act(async () => {
      window.dispatchEvent(new StorageEvent("storage", { key: "gascurve:network", newValue: "robinhood" }));
    });
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
    applyTheme("system");
  });
});

describe("StatusPill", () => {
  it("names every status", () => {
    render(
      <>
        <StatusPill status="polling" />
        <StatusPill status="reconnecting" />
      </>,
    );
    expect(screen.getAllByRole("status").map((el) => el.textContent)).toEqual(["polling", "reconnecting"]);
  });
});
