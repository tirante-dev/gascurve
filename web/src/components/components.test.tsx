import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import type { LiveSnapshot, Network, OwnerAction } from "@/types";
import { ConstraintCards } from "./ConstraintCards";
import { FeeSplitBar, LiveStrip } from "./LiveStrip";
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

describe("LiveStrip", () => {
  it("shows the fee, multiplier, costs and fee split", () => {
    render(<LiveStrip snapshot={snapshot} recentBlocks={[]} status="open" />);
    expect(screen.getByText("0.3997")).toBeInTheDocument();
    expect(screen.getByText("19.99×")).toBeInTheDocument();
    expect(screen.getByText("55,812,345")).toBeInTheDocument();
    expect(screen.getByText("4.02M")).toBeInTheDocument();
    expect(screen.getByText("0.00000839")).toBeInTheDocument();
    expect(screen.getByText("0.0000599")).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("live");
    expect(screen.getByRole("img", { name: /Floor 5.0% to the infra account/ })).toBeInTheDocument();
  });
  it("waits for the first sample", () => {
    render(<LiveStrip snapshot={null} recentBlocks={[]} status="connecting" />);
    expect(screen.getByText("Waiting for the first sample.")).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("connecting");
  });
  it("splits a fee at the floor entirely to infra", () => {
    render(<FeeSplitBar snapshot={{ ...snapshot, prices: { ...snapshot.prices, perArbGasCongestion: "0" } }} />);
    expect(screen.getByText(/0.02 gwei · 100%/)).toBeInTheDocument();
  });
});

describe("ConstraintCards", () => {
  it("renders one card per constraint with its share of x", () => {
    render(<ConstraintCards snapshot={snapshot} />);
    expect(screen.getByText("Constraint 1")).toBeInTheDocument();
    expect(screen.getByText("Constraint 2")).toBeInTheDocument();
    expect(screen.getByText("99.9% of x")).toBeInTheDocument();
    expect(screen.getByText("0.1% of x")).toBeInTheDocument();
    expect(screen.getAllByRole("meter")).toHaveLength(2);
    expect(screen.getByText("77.7 h of target")).toBeInTheDocument();
  });
  it("renders the legacy card for legacy networks", () => {
    render(<ConstraintCards snapshot={{ ...snapshot, model: "legacy", constraints: [], legacy: { speedLimit: 7_000_000, inertia: 102, tolerance: 10, backlog: 90_000_000 } }} />);
    expect(screen.getByText(/Legacy pricer/)).toBeInTheDocument();
    expect(screen.getByText("70M gas free")).toBeInTheDocument();
    expect(screen.getByText(/x = 0.0280/)).toBeInTheDocument();
  });
  it("handles a missing snapshot", () => {
    render(<ConstraintCards snapshot={null} />);
    expect(screen.getByText("Waiting for the first sample.")).toBeInTheDocument();
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
    { block: 53_578_754, at: "2026-09-03T17:08:00Z", txHash: "0x" + "ab".repeat(32), method: "setGasPricingConstraints", selector: "0xcc0d556a", args: { constraints: [[60_000_000, 15, 0], [40_000_000, 86_400, 9_989_000_000_000]] } },
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
