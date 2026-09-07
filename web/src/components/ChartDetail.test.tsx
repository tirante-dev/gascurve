import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { cloneElement, isValidElement } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { assignPlaces, createFrameStore, NO_PLACES, targetValues } from "@/lib/smoothing";
import { CHART_VIEWS, DETAIL_FRAME_CLASS } from "@/lib/chartViews";
import type { BatchSeries, BlockPoint, LiveSnapshot, Network, Series } from "@/types";

vi.mock("recharts", async (importOriginal) => {
  const original = await importOriginal<typeof import("recharts")>();
  const Sized = ({ children }: { children: React.ReactNode }) => (
    <div style={{ width: 400, height: 200 }}>{isValidElement<{ width?: number; height?: number }>(children) ? cloneElement(children, { width: 400, height: 200 }) : children}</div>
  );
  return { ...original, ResponsiveContainer: Sized };
});

const replaceMock = vi.fn();
let query = "";
vi.mock("next/navigation", () => ({
  useParams: () => ({ network: "robinhood", chart: "base-fee" }),
  useRouter: () => ({ push: vi.fn(), replace: replaceMock }),
  usePathname: () => "/robinhood/charts/base-fee",
  useSearchParams: () => new URLSearchParams(query),
}));

const networks: Network[] = [
  { name: "robinhood", displayName: "Robinhood Chain", chainId: 4663, explorerUrl: "https://explorer.example", model: "constraints", headBlock: 10, headAt: null, lagSeconds: null, enabled: true },
];

const batches: BatchSeries = {
  range: "24h",
  resolution: "1m",
  from: 1788679200,
  to: 1788679320,
  points: [
    { t: 1788679200, weiSpent: "1000000000000000", batches: 1, gasSpent: 1, l1BaseFeeAvg: "1000000000", calldataBytes: 1 },
    { t: 1788679260, weiSpent: "2000000000000000", batches: 1, gasSpent: 1, l1BaseFeeAvg: "1000000000", calldataBytes: 1 },
  ],
};

const emptyApi = { data: null, error: null, loading: false, updatedAt: null, refresh: () => undefined };
const apiKeys: (string | null)[] = [];
vi.mock("@/hooks/useApi", () => ({
  useApi: (key: string | null) => {
    apiKeys.push(key);
    if (key === "networks") return { ...emptyApi, data: networks };
    if (key !== null && key.endsWith(":batches")) return { ...emptyApi, data: batches };
    return emptyApi;
  },
}));

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
    { target: 30_000_000, window: 60, backlog: 2_000_000, exponentBips: 40 },
    { target: 40_000_000, window: 86_400, backlog: 11_194_391_810_886, exponentBips: 32_391 },
  ],
  prices: { perL2Tx: "0", perL1CalldataByte: "0", perL2Storage: "0", perArbGasBase: "20000000", perArbGasCongestion: "379726000", perArbGasTotal: "399726000" },
  gasPerSecond: { s10: 38_000_000, s60: 40_500_000 },
  replayErrorBips: 2,
  ethUsd: null,
};

/** Two seconds of blocks, ten a second, so the sawtooth has something to draw. */
const blocks: BlockPoint[] = [1788679198, 1788679199].flatMap((ts) =>
  Array.from({ length: 10 }, (_, k) => ({
    number: ts * 10 + k,
    ts,
    gasUsed: 4_000_000,
    baseFee: "399726000",
    predictedBaseFee: "399726000",
    backlogs: [(k + 1) * 4_000_000, 2_000_000, 11_194_391_810_886],
    constraintBips: [],
    exponentBips: 0,
    minBaseFee: "20000000",
    anchored: k === 0,
  })),
);

const constraintSet = {
  id: 6,
  effectiveBlock: 10,
  effectiveAt: "2026-09-01T16:33:00Z",
  source: "owner_action" as const,
  constraints: [
    { target: 60_000_000, window: 15, startingBacklog: 0 },
    { target: 30_000_000, window: 60, startingBacklog: 0 },
    { target: 40_000_000, window: 86_400, startingBacklog: 0 },
  ],
};

const series: Series = {
  range: "24h",
  resolution: "1m",
  from: 1788679200,
  to: 1788679320,
  constraintSets: [constraintSet],
  ownerActions: [],
  points: [1788679200, 1788679260].map((t) => ({
    t,
    blocks: 12,
    gasUsed: 100,
    gasPerSecond: 41_000_000,
    coverage: 1,
    completeness: "complete",
    feesWei: "1000000000000000000",
    baseFeeMin: "100000000",
    baseFeeAvg: "300000000",
    baseFeeMax: "400000000",
    exponentBips: 10_000,
    constraintBips: [3_000, 3_000, 4_000],
    backlogs: [1, 2, 3],
    backlogsMax: [1, 2, 3],
    minBaseFee: "100000000",
    floorFeesWei: "200000000000000000",
    surplusFeesWei: "800000000000000000",
    constraintSetId: 6,
    replayErrorBips: 0,
  })),
};

const seriesCalls: (string | null)[][] = [];
let seriesData: Series | null = series;
vi.mock("@/hooks/useSeries", () => ({
  useSeries: (network: string | null, range: string | null) => {
    seriesCalls.push([network, range]);
    return { ...emptyApi, data: seriesData };
  },
}));

const frame = createFrameStore({ blocks, places: assignPlaces(NO_PLACES, blocks), values: targetValues(snapshot, blocks, 0), nowMs: Date.parse(snapshot.sampledAt) });
const liveEnabled: boolean[] = [];
vi.mock("@/hooks/useNetworkLive", () => ({
  useNetworkLive: (_network: string, enabled = true) => {
    liveEnabled.push(enabled);
    return {
    live: { snapshot, recentBlocks: blocks, status: "open", networkInfo: networks[0], ownerActions: [], reorgs: 0, resyncing: false, error: null },
    smooth: { display: snapshot, frame, resyncing: false },
      snapshot,
      status: "open",
      networkInfo: networks[0],
    };
  },
}));

import { ChartDetail } from "./ChartDetail";

/** The enlarged frame every chart draws in, whichever chart it is. */
function enlargedFrame(container: HTMLElement): Element | null {
  return container.querySelector('[class*="62vh"]');
}

describe("a chart on a page of its own", () => {
  beforeEach(() => {
    query = "";
    seriesData = series;
    replaceMock.mockReset();
    seriesCalls.length = 0;
    apiKeys.length = 0;
    liveEnabled.length = 0;
  });

  it("opens the feed only for the charts that draw it, and names the chain from REST for the rest", () => {
    for (const view of CHART_VIEWS) {
      liveEnabled.length = 0;
      const { unmount } = render(<ChartDetail network="robinhood" chart={view.id} />);
      expect(liveEnabled.every((on) => on === view.live)).toBe(true);
      // Every page still names its chain, live or not: the REST network list
      // is asked for on all of them.
      expect(apiKeys).toContain("networks");
      expect(screen.getByText("Robinhood Chain · chain 4663")).toBeInTheDocument();
      unmount();
    }
    // A chart id that names nothing needs no feed either.
    liveEnabled.length = 0;
    render(<ChartDetail network="robinhood" chart="nonsense" />);
    expect(liveEnabled.every((on) => on === false)).toBe(true);
  });

  it("reads the enlarged base fee out without a pointer, as the page does", async () => {
    render(<ChartDetail network="robinhood" chart="base-fee" />);
    expect(screen.getByRole("slider", { name: "Select a block to read its values" })).toBeInTheDocument();
    await userEvent.click(screen.getByText(/Base fee per block, as a table/));
    expect(screen.getByRole("table", { name: /Every block of the live base fee chart/ })).toBeInTheDocument();
  });

  it("draws every registered chart at the enlarged height, under its own name", () => {
    for (const view of CHART_VIEWS) {
      const { container, unmount } = render(<ChartDetail network="robinhood" chart={view.id} />);
      expect(screen.getByRole("heading", { name: view.title, level: 2 })).toBeInTheDocument();
      expect(screen.getByText(view.description)).toBeInTheDocument();
      expect(enlargedFrame(container)).not.toBeNull();
      // The way back is to the section the chart was lifted out of.
      expect(screen.getByRole("link", { name: /Back to the dashboard/ })).toHaveAttribute("href", `/robinhood#${view.section}`);
      unmount();
    }
    expect(DETAIL_FRAME_CLASS).toContain("62vh");
  });

  it("marks the chart on screen in a tab row over all of them", () => {
    render(<ChartDetail network="robinhood" chart="contribution" />);
    const tabs = within(screen.getByRole("navigation", { name: "Charts" })).getAllByRole("link");
    expect(tabs).toHaveLength(CHART_VIEWS.length);
    expect(screen.getByRole("link", { name: "Contribution" })).toHaveAttribute("aria-current", "page");
    expect(screen.getByRole("link", { name: "Base fee" })).not.toHaveAttribute("aria-current");
  });

  it("says so when there is no such chart, and still offers every one there is", () => {
    render(<ChartDetail network="robinhood" chart="nonsense" />);
    expect(screen.getByRole("heading", { name: "Chart not found" })).toBeInTheDocument();
    expect(screen.getByText("nonsense")).toBeInTheDocument();
    expect(within(screen.getByRole("navigation", { name: "Charts" })).getAllByRole("link")).toHaveLength(CHART_VIEWS.length);
    // With no chart there is no section to go back to, so the link goes to the page itself.
    expect(screen.getByRole("link", { name: /Back to the dashboard/ })).toHaveAttribute("href", "/robinhood");
  });

  it("takes the range from the URL, and puts it on the controls, the tabs and the request", () => {
    query = "range=30d";
    render(<ChartDetail network="robinhood" chart="contribution" />);
    expect(within(screen.getByRole("group", { name: "History range" })).getByRole("button", { name: "30d" })).toHaveAttribute("aria-pressed", "true");
    // The throughput chart takes the hero's ranges, and 30d is one of them.
    expect(screen.getByRole("link", { name: "Gas/s" })).toHaveAttribute("href", "/robinhood/charts/gas-per-second?range=30d");
    expect(seriesCalls.at(-1)).toEqual(["robinhood", "30d"]);
  });

  it("writes a range change back to the URL rather than keeping it in the page", async () => {
    query = "range=24h";
    render(<ChartDetail network="robinhood" chart="fee-flows" />);
    await userEvent.click(within(screen.getByRole("group", { name: "History range" })).getByRole("button", { name: "1h" }));
    expect(replaceMock).toHaveBeenCalledWith("/robinhood/charts/base-fee?range=1h", { scroll: false });
  });

  it("asks for no buckets at all while the base fee is on Live", () => {
    query = "range=live";
    const { container } = render(<ChartDetail network="robinhood" chart="base-fee" />);
    expect(seriesCalls.at(-1)).toEqual([null, null]);
    expect(within(container).getByRole("figure", { name: /Base fee per block over the last 120 seconds/ })).toBeInTheDocument();
  });

  it("draws the bucketed base fee at a history range", () => {
    query = "range=24h";
    render(<ChartDetail network="robinhood" chart="base-fee" />);
    expect(seriesCalls.at(-1)).toEqual(["robinhood", "24h"]);
    expect(screen.getByRole("figure", { name: /Base fee over 24h on a log scale/ })).toBeInTheDocument();
  });

  it("switches between the short windows the sawtooth can draw, and defaults to the first", async () => {
    render(<ChartDetail network="robinhood" chart="backlog-sawtooth" />);
    const control = screen.getByRole("group", { name: "Constraint" });
    // Two short windows in the set; the 24 h one is not on offer.
    expect(within(control).getAllByRole("button").map((t) => t.textContent)).toEqual(["C1", "C2"]);
    expect(within(control).getByRole("button", { name: "C1" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("figure", { name: /^Constraint 1 backlog per block/ })).toBeInTheDocument();
    await userEvent.click(within(control).getByRole("button", { name: "C2" }));
    expect(replaceMock).toHaveBeenCalledWith("/robinhood/charts/base-fee?constraint=1", { scroll: false });
  });

  it("draws the constraint the URL names, and the first one when it names one that is not there", () => {
    query = "constraint=1";
    const { rerender } = render(<ChartDetail network="robinhood" chart="backlog-sawtooth" />);
    expect(screen.getByRole("figure", { name: /^Constraint 2 backlog per block/ })).toBeInTheDocument();
    query = "constraint=9";
    rerender(<ChartDetail network="robinhood" chart="backlog-sawtooth" />);
    expect(screen.getByRole("figure", { name: /^Constraint 1 backlog per block/ })).toBeInTheDocument();
  });

  it("gives the backlog chart one slot at a time, named as the history names it", () => {
    query = "range=24h&constraint=2";
    render(<ChartDetail network="robinhood" chart="backlogs" />);
    const control = screen.getByRole("group", { name: "Constraint" });
    // Slot numbers on the control, so it fits a phone, with the slot it picked out named beside it.
    expect(within(control).getAllByRole("button").map((t) => t.textContent)).toEqual(["C1", "C2", "C3"]);
    expect(within(control).getByRole("button", { name: "C3" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByText("C3 · 40 Mgas/s · 24 h (set 6)")).toBeInTheDocument();
    expect(screen.getByRole("figure", { name: /^Backlog of C3 · 40 Mgas\/s · 24 h/ })).toBeInTheDocument();
  });

  it("draws the fee flows and the L1 costs from the same hooks the page uses", () => {
    query = "range=1h";
    const { unmount } = render(<ChartDetail network="robinhood" chart="fee-flows" />);
    expect(screen.getByRole("figure", { name: /Fees collected per bucket in ETH/ })).toBeInTheDocument();
    unmount();
    render(<ChartDetail network="robinhood" chart="l1" />);
    expect(screen.getByRole("figure", { name: /L2 fees and L1 posting cost per bucket/ })).toBeInTheDocument();
    // Only the L1 page asks for batches, and it asks for the range on screen.
    expect(apiKeys).toContain("robinhood:1h:batches");
  });

  it("says what is missing rather than drawing an empty chart", () => {
    // A bucketed range, not Live: the throughput chart draws the block ring on
    // Live and so has nothing to say about buckets there.
    query = "range=24h";
    seriesData = null;
    render(<ChartDetail network="robinhood" chart="contribution" />);
    expect(screen.getByText("No history yet.")).toBeInTheDocument();
    seriesData = { ...series, points: [] };
    render(<ChartDetail network="robinhood" chart="gas-per-second" />);
    expect(screen.getAllByText("Nothing indexed for this range yet.").length).toBeGreaterThan(0);
  });
});
