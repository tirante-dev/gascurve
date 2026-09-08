import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { cloneElement, isValidElement } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { createFrameStore, targetValues } from "@/lib/smoothing";
import { CHART_VIEWS } from "@/lib/chartViews";
import type { BlockPoint, LiveSnapshot, Network, Series } from "@/types";

// ResponsiveContainer measures its box, and jsdom has none: hand the chart a
// fixed size so the cards really render.
vi.mock("recharts", async (importOriginal) => {
  const original = await importOriginal<typeof import("recharts")>();
  const Sized = ({ children }: { children: React.ReactNode }) => (
    <div style={{ width: 400, height: 200 }}>{isValidElement<{ width?: number; height?: number }>(children) ? cloneElement(children, { width: 400, height: 200 }) : children}</div>
  );
  return { ...original, ResponsiveContainer: Sized };
});

vi.mock("next/navigation", () => ({
  useParams: () => ({ network: "robinhood" }),
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
}));

const networks: Network[] = [
  { name: "robinhood", displayName: "Robinhood Chain", chainId: 4663, explorerUrl: "https://explorer.example", model: "constraints", headBlock: 10, headAt: null, lagSeconds: null, enabled: true },
];

const emptyApi = { data: null, error: null, loading: false, updatedAt: null, refresh: () => undefined };
vi.mock("@/hooks/useApi", () => ({
  useApi: (key: string | null) => (key === "networks" ? { ...emptyApi, data: networks } : emptyApi),
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
    { target: 40_000_000, window: 86_400, backlog: 11_194_391_810_886, exponentBips: 32_391 },
  ],
  prices: { perL2Tx: "0", perL1CalldataByte: "0", perL2Storage: "0", perArbGasBase: "20000000", perArbGasCongestion: "379726000", perArbGasTotal: "399726000" },
  gasPerSecond: { s10: 38_000_000, s60: 40_500_000 },
  replayErrorBips: 2,
  ethUsd: null,
};

const blocks: BlockPoint[] = [1, 2, 3, 4].map((k) => ({
  number: 55_812_342 + k,
  ts: 1788679199,
  gasUsed: 4_000_000,
  baseFee: "399726000",
  predictedBaseFee: "399726000",
  backlogs: [k * 4_000_000, 11_194_391_810_886],
  constraintBips: [],
  exponentBips: 0,
  minBaseFee: "20000000",
  anchored: k === 1,
}));

const series: Series = {
  range: "24h",
  resolution: "1m",
  from: 1788679200,
  to: 1788679320,
  constraintSets: [{ id: 6, effectiveBlock: 10, effectiveAt: "2026-09-01T16:33:00Z", source: "owner_action", constraints: [{ target: 60_000_000, window: 15, startingBacklog: 0 }, { target: 40_000_000, window: 86_400, startingBacklog: 0 }] }],
  ownerActions: [],
  points: [1788679200, 1788679260].map((t) => ({
    t,
    blocks: 12,
    gasUsed: 100,
    gasPerSecond: 1,
    coverage: 1,
    completeness: "complete",
    feesWei: "1000000000000000000",
    baseFeeMin: "100000000",
    baseFeeAvg: "300000000",
    baseFeeMax: "400000000",
    exponentBips: 10_000,
    constraintBips: [4_000, 6_000],
    backlogs: [1, 2],
    backlogsMax: [1, 2],
    minBaseFee: "100000000",
    floorFeesWei: "200000000000000000",
    surplusFeesWei: "800000000000000000",
    constraintSetId: 6,
    replayErrorBips: 0,
  })),
};

vi.mock("@/hooks/useSeries", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/hooks/useSeries")>()),
  useSeries: () => ({ ...emptyApi, data: series }),
}));

// The live feed as the page consumes it, already smoothed: the network page's
// own hook is exercised by the hook tests.
const frame = createFrameStore({ blocks, values: targetValues(snapshot, blocks, 0), nowMs: Date.parse(snapshot.sampledAt) });
vi.mock("@/hooks/useNetworkLive", () => ({
  useNetworkLive: () => ({
    live: { snapshot, recentBlocks: blocks, status: "open", networkInfo: networks[0], ownerActions: [], reorgs: 0, resyncing: false, error: null },
    smooth: { display: snapshot, frame, resyncing: false },
    snapshot,
    status: "open",
    networkInfo: networks[0],
  }),
}));

import { HowItWorks } from "./HowItWorks";
import { NetworkPage } from "./NetworkPage";

/** The link a card's enlarge control is, by what it is announced as. */
function enlarge(name: RegExp | string): HTMLElement {
  return screen.getByRole("link", { name: typeof name === "string" ? `Open ${name} enlarged` : name });
}

describe("the enlarge control on every chart card", () => {
  beforeEach(() => {
    window.localStorage.clear();
  });

  it("links the hero, the sawtooth and every history chart to its own page, at the range on screen", () => {
    render(<NetworkPage network="robinhood" />);

    // The hero carries the range its own control is set to.
    expect(enlarge("Base fee")).toHaveAttribute("href", "/robinhood/charts/base-fee?range=live");
    // Only the short window has a sparkline to enlarge; the 24 h constraint has none.
    expect(enlarge("the constraint 1 backlog")).toHaveAttribute("href", "/robinhood/charts/backlog-sawtooth?constraint=0");
    expect(screen.queryByRole("link", { name: "Open the constraint 2 backlog enlarged" })).toBeNull();

    // The history charts carry the section's range.
    expect(enlarge("Contribution to x per constraint")).toHaveAttribute("href", "/robinhood/charts/contribution?range=24h");
    // Throughput sits in the hero now, on the hero's range and not the history section's.
    expect(enlarge("Gas throughput")).toHaveAttribute("href", "/robinhood/charts/gas-per-second?range=live");
    // One per slot, each naming the slot it enlarges.
    expect(enlarge(/^Open the C1 .* backlog enlarged$/)).toHaveAttribute("href", "/robinhood/charts/backlogs?range=24h&constraint=0");
    expect(enlarge(/^Open the C2 .* backlog enlarged$/)).toHaveAttribute("href", "/robinhood/charts/backlogs?range=24h&constraint=1");
    expect(enlarge("Fees collected per bucket")).toHaveAttribute("href", "/robinhood/charts/fee-flows?range=24h");

    expect(enlarge("L2 fees against ArbOS-attributed batch cost")).toHaveAttribute("href", "/robinhood/charts/l1?range=24h");

    // Every control says what it does on hover as well as to a screen reader.
    for (const link of screen.getAllByRole("link", { name: /enlarged$/ })) expect(link).toHaveAttribute("title", "Enlarge chart");
  });

  it("follows the range control, so a link is to what the reader is looking at", async () => {
    render(<NetworkPage network="robinhood" />);
    await userEvent.click(within(screen.getByRole("group", { name: "History range" })).getByRole("button", { name: "30d" }));
    expect(enlarge("Contribution to x per constraint")).toHaveAttribute("href", "/robinhood/charts/contribution?range=30d");
    expect(enlarge("Fees collected per bucket")).toHaveAttribute("href", "/robinhood/charts/fee-flows?range=30d");
    // The hero has a range of its own, and keeps it.
    expect(enlarge("Base fee")).toHaveAttribute("href", "/robinhood/charts/base-fee?range=live");
    await userEvent.click(within(screen.getByRole("group", { name: "Base fee chart range" })).getByRole("button", { name: "1h" }));
    expect(enlarge("Base fee")).toHaveAttribute("href", "/robinhood/charts/base-fee?range=1h");
  });

  it("lands every back anchor on a section the network page has", () => {
    const { container } = render(<NetworkPage network="robinhood" />);
    for (const view of CHART_VIEWS) {
      expect(container.querySelector(`section#${view.section}`)).not.toBeNull();
    }
  });

  it("enlarges the P4 curve from the explainer, the one chart that lives there", () => {
    render(<HowItWorks network="robinhood" />);
    expect(enlarge("P4(x) against e^x")).toHaveAttribute("href", "/robinhood/charts/taylor");
  });
});
