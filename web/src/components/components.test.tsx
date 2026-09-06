import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import type { BlockPoint, LiveSnapshot, Network, OwnerAction } from "@/types";
import { createFrameStore, targetValues } from "@/lib/smoothing";
import { applyReorg } from "@/hooks/useLive";
import { ConstraintCards, ConstraintCardsView, Sawtooth, sawtoothPoints } from "./ConstraintCards";
import { DataFooter } from "./DataFooter";
import { COLLECTOR_LAG_S, FeeSplitBar, LiveStrip, LiveStripView, sampleAge } from "./LiveStrip";
import { HistoryTabs } from "./HistoryTabs";
import { NetworkSwitcher } from "./NetworkSwitcher";
import { OwnerActionTimeline } from "./OwnerActionTimeline";
import { applyTheme, readTheme, ThemeToggle } from "./ThemeToggle";
import { StatusPill } from "./primitives";

vi.mock("recharts", async (importOriginal) => {
  const original = await importOriginal<typeof import("recharts")>();
  // ResponsiveContainer needs layout; render children in a fixed box under jsdom.
  return { ...original, ResponsiveContainer: ({ children }: { children: React.ReactNode }) => <div style={{ width: 400, height: 100 }}>{children}</div> };
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
  replayErrorBips: 2,
};

/** Ten blocks a second for `seconds` seconds ending at `lastTs`, the short window climbing 4M a block and dropping at each boundary. */
function sawtoothBlocks(seconds: number, lastTs: number): BlockPoint[] {
  const out: BlockPoint[] = [];
  let n = 1;
  for (let ts = lastTs - seconds + 1; ts <= lastTs; ts++) {
    for (let k = 0; k < 10; k++) {
      out.push({ number: n++, ts, gasUsed: 4_000_000, baseFee: "399726000", predictedBaseFee: "399726000", backlogs: [(k + 1) * 4_000_000, 11_194_391_810_886], constraintBips: [], exponentBips: 0, minBaseFee: "20000000", anchored: k === 0 });
    }
  }
  return out;
}

describe("LiveStrip", () => {
  it("shows the fee, multiplier, costs and fee split at fixed widths", () => {
    render(<LiveStripView snapshot={snapshot} values={null} blocks={[]} nowMs={Date.parse(snapshot.sampledAt)} status="open" />);
    const hero = screen.getByText("0.3997");
    expect(hero).toHaveClass("tabular-nums");
    expect(hero).toHaveStyle({ minWidth: "6ch" });
    expect(hero.nextSibling).toHaveTextContent("gwei");
    expect(screen.getByText("19.99")).toHaveStyle({ minWidth: "5ch" });
    expect(screen.getByText("55,812,345")).toBeInTheDocument();
    expect(screen.getByText("4.02M")).toBeInTheDocument();
    expect(screen.getByText("38.0")).toBeInTheDocument();
    expect(screen.getByText("40.5")).toBeInTheDocument();
    expect(screen.getByText("3.2425")).toBeInTheDocument();
    expect(screen.getByText("0.00000839")).toHaveStyle({ minWidth: "10ch" });
    expect(screen.getByText("0.0000600")).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("live");
    expect(screen.getByRole("img", { name: /Floor 5.0% to the infra account/ })).toBeInTheDocument();
  });
  it("renders the eased figures rather than the sample when a frame has them", () => {
    const values = { ...targetValues(snapshot, [], 0), baseFeeGwei: 0.5, multiplier: 25, gasPerSecond10: 41_000_000, transferEth: 1.05e-5, exponent: 3.3 };
    render(<LiveStripView snapshot={snapshot} values={values} blocks={[]} nowMs={Date.parse(snapshot.sampledAt)} status="open" />);
    expect(screen.getByText("0.5000")).toBeInTheDocument();
    expect(screen.getByText("25.00")).toBeInTheDocument();
    expect(screen.getByText("41.0")).toBeInTheDocument();
    expect(screen.getByText("0.0000105")).toBeInTheDocument();
    expect(screen.getByText("3.3000")).toBeInTheDocument();
  });
  it("subscribes to the frame store", () => {
    const frame = createFrameStore({ nowMs: Date.parse(snapshot.sampledAt) });
    render(<LiveStrip live={{ display: snapshot, frame, resyncing: false }} status="open" />);
    expect(screen.getByText("0.3997")).toBeInTheDocument();
    act(() => frame.set({ blocks: [], values: { ...targetValues(snapshot, [], 0), baseFeeGwei: 0.75 }, nowMs: Date.parse(snapshot.sampledAt) }));
    expect(screen.getByText("0.7500")).toBeInTheDocument();
    expect(screen.getByText("last 0 blocks · floor 0.02 gwei")).toBeInTheDocument();
  });
  it("draws the block ring", () => {
    render(<LiveStripView snapshot={snapshot} values={null} blocks={sawtoothBlocks(2, snapshot.block.ts)} nowMs={Date.parse(snapshot.sampledAt)} status="open" />);
    expect(screen.getByRole("img", { name: /Base fee over the last 20 blocks/ })).toBeInTheDocument();
    expect(screen.getByText("last 20 blocks · floor 0.02 gwei")).toBeInTheDocument();
  });
  it("draws the canonical blocks after a reorg, not the orphaned ones", () => {
    const ring = sawtoothBlocks(2, snapshot.block.ts);
    const head = ring[ring.length - 1].number;
    const canonical = ring.slice(-3).map((b) => ({ ...b, baseFee: "800000000" }));
    const blocks = applyReorg(ring, head - 3, canonical);
    expect(blocks).toHaveLength(20);
    const { rerender } = render(<LiveStripView snapshot={snapshot} values={null} blocks={ring} nowMs={Date.parse(snapshot.sampledAt)} status="open" />);
    expect(screen.getByRole("img", { name: "Base fee over the last 20 blocks, from 0.3997 to 0.3997 gwei" })).toBeInTheDocument();
    rerender(<LiveStripView snapshot={snapshot} values={null} blocks={blocks} nowMs={Date.parse(snapshot.sampledAt)} status="open" />);
    expect(screen.getByRole("img", { name: "Base fee over the last 20 blocks, from 0.3997 to 0.8 gwei" })).toBeInTheDocument();
    expect(screen.getByText(`Latest block ${head}`)).toBeInTheDocument();
    expect(screen.getByText("last 20 blocks · floor 0.02 gwei")).toBeInTheDocument();
  });
  it("waits for the first sample, and says so differently while a reorg is being repaired", () => {
    const { rerender } = render(<LiveStrip live={{ display: null, frame: createFrameStore(), resyncing: false }} status="connecting" />);
    expect(screen.getByText("Waiting for the first sample.")).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("connecting");
    // A reorg took the last canonical state away: the strip must not claim it
    // is waiting for a first sample, and must show no orphaned figures.
    rerender(<LiveStrip live={{ display: null, frame: createFrameStore(), resyncing: true }} status="open" />);
    expect(screen.getByText("Resyncing after a reorg.")).toBeInTheDocument();
    expect(screen.queryByText("Waiting for the first sample.")).toBeNull();
  });
  it("splits a fee at the floor entirely to infra", () => {
    render(<FeeSplitBar snapshot={{ ...snapshot, prices: { ...snapshot.prices, perArbGasCongestion: "0" } }} />);
    expect(screen.getByText(/0.02 gwei · 100%/)).toBeInTheDocument();
  });
  it("shows the chain's cadence while the sample is fresh and a collector lag warning once it is stale", () => {
    // The block closed at 07:19:59Z and was sampled at 07:20:00Z; three seconds later both are fresh.
    const { rerender } = render(<LiveStripView snapshot={snapshot} values={null} blocks={[]} nowMs={Date.parse("2026-09-06T07:20:03Z")} status="open" />);
    expect(screen.getByText("Since last block")).toBeInTheDocument();
    expect(screen.getByText("4.0")).toBeInTheDocument();
    expect(screen.queryByText(/collector lagging/)).toBeNull();
    // Twelve seconds after the sample the number would read as a chain stall; it is the collector that is behind.
    rerender(<LiveStripView snapshot={snapshot} values={null} blocks={[]} nowMs={Date.parse("2026-09-06T07:20:12.4Z")} status="open" />);
    const pill = screen.getByText("collector lagging 12 s");
    expect(pill).toHaveClass("text-warning");
    expect(pill).toHaveAttribute("title", expect.stringContaining("12 s old"));
    expect(screen.getByText("Since last block")).toBeInTheDocument();
    expect(screen.queryByText("13.4")).toBeNull();
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
    render(<ConstraintCardsView snapshot={snapshot} values={null} blocks={[]} />);
    expect(screen.getByText("Constraint 1")).toBeInTheDocument();
    expect(screen.getByText("Constraint 2")).toBeInTheDocument();
    expect(screen.getByText("99.9%").parentElement).toHaveTextContent("99.9% of x");
    expect(screen.getByText("0.1%").parentElement).toHaveTextContent("0.1% of x");
    expect(screen.getAllByRole("meter")).toHaveLength(2);
    expect(screen.getByText("77.7 h of target")).toBeInTheDocument();
    // The 15 s window is short: averaged, with its note, but no sparkline until blocks arrive.
    expect(screen.getByText("Backlog (avg 2 s)")).toBeInTheDocument();
    expect(screen.getAllByText("Backlog")).toHaveLength(1);
    expect(screen.getByText("drains 60M gas at each second boundary; bursts show as sawteeth")).toBeInTheDocument();
    expect(screen.queryByRole("img", { name: /Backlog per block/ })).toBeNull();
    expect(screen.getByText(/Windows of 1 min or less are shown as a 2 s average/)).toBeInTheDocument();
    expect(screen.getByText("3.11M")).toBeInTheDocument();
    expect(screen.getByText("11.2T")).toBeInTheDocument();
  });
  it("draws the raw sawtooth of a short window and shows the eased figures", () => {
    const blocks = sawtoothBlocks(20, snapshot.block.ts);
    const values = { ...targetValues(snapshot, blocks, 0), backlogs: [22_000_000, 11_100_000_000_000], bips: [244.4, 32_118.5] };
    render(<ConstraintCardsView snapshot={snapshot} values={values} blocks={blocks} />);
    const spark = screen.getByRole("img", { name: "Backlog per block over the last 15 s, 150 blocks, peak 40M gas" });
    expect(spark.querySelector("polyline")?.getAttribute("points")).toBe(sawtoothPoints(values.backlogs.length > 0 ? blocks.slice(-150).map((b) => ({ number: b.number, ts: b.ts, backlog: b.backlogs[0] })) : []));
    expect(screen.getByText("22.0M")).toBeInTheDocument();
    expect(screen.getByText("11.1T")).toBeInTheDocument();
    expect(screen.getByText("0.0244")).toBeInTheDocument();
    expect(screen.getByText(/244 bips/)).toBeInTheDocument();
    expect(screen.getByText(/32,119 bips/)).toBeInTheDocument();
  });
  it("subscribes to the frame store and says so when nothing contributes", () => {
    const frame = createFrameStore();
    render(<ConstraintCards live={{ display: snapshot, frame, resyncing: false }} />);
    expect(screen.getByText("3.11M")).toBeInTheDocument();
    act(() => frame.set({ blocks: [], values: { ...targetValues(snapshot, [], 0), backlogs: [0, 0], bips: [0, 0], shares: [0, 0] }, nowMs: 0 }));
    expect(screen.getAllByText("no contribution")).toHaveLength(2);
    expect(screen.getAllByText("0.0000")).toHaveLength(2);
  });
  it("renders the legacy card for legacy networks", () => {
    render(<ConstraintCardsView snapshot={{ ...snapshot, model: "legacy", constraints: [], legacy: { speedLimit: 7_000_000, inertia: 102, tolerance: 10, backlog: 90_000_000 } }} values={null} blocks={[]} />);
    expect(screen.getByText(/Legacy pricer/)).toBeInTheDocument();
    expect(screen.getByText("70M gas free")).toBeInTheDocument();
    expect(screen.getByText(/x = 0.0280/)).toBeInTheDocument();
    expect(screen.getByText("90.0M")).toBeInTheDocument();
    expect(screen.queryByText(/2 s average/)).toBeNull();
  });
  it("handles a missing snapshot, and names a reorg repair as one", () => {
    const { rerender } = render(<ConstraintCardsView snapshot={null} values={null} blocks={[]} />);
    expect(screen.getByText("Waiting for the first sample.")).toBeInTheDocument();
    rerender(<ConstraintCardsView snapshot={null} values={null} blocks={[]} resyncing />);
    expect(screen.getByText("Resyncing after a reorg.")).toBeInTheDocument();
  });
  it("draws the sawtooth as a polyline scaled to its peak, or a blank with fewer than two samples", () => {
    expect(sawtoothPoints([{ number: 1, ts: 1, backlog: 0 }, { number: 2, ts: 1, backlog: 50 }, { number: 3, ts: 1, backlog: 100 }])).toBe("0.0,31.0 100.0,16.0 200.0,1.0");
    expect(sawtoothPoints([{ number: 1, ts: 1, backlog: 0 }])).toBe("0.0,31.0");
    const { container, rerender } = render(<Sawtooth samples={[{ number: 1, ts: 1, backlog: 5 }]} color="red" />);
    expect(container.querySelector("svg")).toBeNull();
    rerender(<Sawtooth samples={[{ number: 1, ts: 1, backlog: 5 }, { number: 2, ts: 1, backlog: 10 }]} color="red" />);
    expect(container.querySelector("polyline")).toHaveAttribute("stroke", "red");
  });
});

describe("DataFooter", () => {
  it("credits tirante.dev with an external link after the version line", () => {
    render(<DataFooter snapshot={null} series={null} networkInfo={null} status="open" apiStatus={{ version: "1.2.3", networks: [] }} now={0} />);
    const link = screen.getByRole("link", { name: "powered by tirante.dev" });
    expect(link).toHaveAttribute("href", "https://tirante.dev");
    expect(link).toHaveAttribute("target", "_blank");
    expect(link.getAttribute("rel")).toContain("noopener");
    expect(link.parentElement).toHaveClass("text-label");
    const versions = screen.getByText("versions");
    expect(versions.compareDocumentPosition(link) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getByText(/api 1.2.3/)).toBeInTheDocument();
  });
});

describe("HistoryTabs", () => {
  it("marks the selected range and reports clicks", async () => {
    const onChange = vi.fn();
    render(<HistoryTabs range="24h" onChange={onChange} loading />);
    expect(screen.getByRole("tab", { name: "24h" })).toHaveAttribute("aria-selected", "true");
    expect(screen.getByText("updating")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("tab", { name: "All" }));
    expect(onChange).toHaveBeenCalledWith("all");
  });
});

describe("NetworkSwitcher", () => {
  const networks: Network[] = [
    { name: "robinhood", displayName: "Robinhood Chain", chainId: 4663, explorerUrl: "", model: "constraints", headBlock: 1, headAt: null, lagSeconds: null, enabled: true },
    { name: "arbitrum-one", displayName: "Arbitrum One", chainId: 42161, explorerUrl: "", model: "constraints", headBlock: 1, headAt: "", lagSeconds: 0, enabled: false },
  ];
  it("lists networks and changes selection", async () => {
    const onChange = vi.fn();
    render(<NetworkSwitcher networks={networks} current="robinhood" onChange={onChange} loading={false} />);
    const select = screen.getByRole("combobox", { name: "Network" });
    expect(within(select).getAllByRole("option")).toHaveLength(2);
    expect(screen.getByRole("option", { name: "Arbitrum One (42161)" })).toBeDisabled();
    await userEvent.selectOptions(select, "arbitrum-one");
    expect(onChange).not.toHaveBeenCalled();
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
    expect(screen.getByText("40M/s · 24 h · start 9.99T")).toBeInTheDocument();
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
