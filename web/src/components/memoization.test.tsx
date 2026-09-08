import { act, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { LiveSnapshot, Series, SeriesPoint } from "@/types";

const enlarged = vi.hoisted(() => vi.fn());
const framed = vi.hoisted(() => vi.fn());

vi.mock("recharts", async (importOriginal) => {
  const original = await importOriginal<typeof import("recharts")>();
  return { ...original, ResponsiveContainer: ({ children }: { children: React.ReactNode }) => <div style={{ width: 400, height: 100 }}>{children}</div> };
});

// The enlarge control is rendered in the body of every chart card, so its call
// count is the render count of the card around it.
vi.mock("./ChartActions", async (importOriginal) => {
  const original = await importOriginal<typeof import("./ChartActions")>();
  return {
    ...original,
    EnlargeLink: (props: Parameters<typeof original.EnlargeLink>[0]) => {
      enlarged();
      return <original.EnlargeLink {...props} />;
    },
  };
});

// Every chart body sits in a ChartFrame, so its call count is the render count
// of the charts inside a card rather than of the card itself.
vi.mock("./primitives", async (importOriginal) => {
  const original = await importOriginal<typeof import("./primitives")>();
  return {
    ...original,
    ChartFrame: (props: Parameters<typeof original.ChartFrame>[0]) => {
      framed();
      return <original.ChartFrame {...props} />;
    },
  };
});

vi.mock("@/lib/gaps", async (importOriginal) => {
  const original = await importOriginal<typeof import("@/lib/gaps")>();
  return { ...original, withGapBreaks: vi.fn(original.withGapBreaks) };
});

import { withGapBreaks } from "@/lib/gaps";
import { DataFooter } from "./DataFooter";
import { FeeFlows } from "./FeeFlows";
import { SeriesCharts } from "./SeriesCharts";

function point(overrides: Partial<SeriesPoint>): SeriesPoint {
  return {
    t: 0,
    blocks: 1,
    gasUsed: 100,
    posterGas: 0,
    gasPerSecond: 0,
    computeGasPerSecond: 0,
    coverage: 1,
    completeness: "complete",
    feesWei: "1000000000000000000",
    baseFeeMin: "1",
    baseFeeAvg: "1",
    baseFeeMax: "1",
    exponentBips: 0,
    constraintBips: [0],
    backlogs: [0],
    backlogsMax: [0],
    minBaseFee: "1",
    floorFeesWei: "400000000000000000",
    surplusFeesWei: "500000000000000000",
    posterFeesWei: "100000000000000000",
    constraintSetId: 5,
    replayErrorBips: 0,
    ...overrides,
  };
}

const series: Series = {
  range: "24h",
  resolution: "1m",
  from: 1788679200,
  to: 1788679320,
  constraintSets: [{ id: 5, effectiveBlock: 10, effectiveAt: "2026-09-01T16:33:00Z", source: "owner_action", constraints: [{ target: 60_000_000, window: 15, startingBacklog: 0 }] }],
  ownerActions: [],
  points: [point({ t: 1788679200 }), point({ t: 1788679260 })],
};

function snapshotAt(sampledAt: string): LiveSnapshot {
  return {
    chainId: 4663,
    sampledAt,
    block: { number: 55_812_345, ts: 1788679199, gasUsed: 4_021_130, baseFee: "399726000", txCount: 90 },
    baseFee: "399726000",
    minBaseFee: "20000000",
    multiplierBips: 199_863,
    exponentBips: 0,
    model: "constraints",
    constraints: [{ target: 60_000_000, window: 15, backlog: 0, exponentBips: 0 }],
    prices: { perL2Tx: "0", perL1CalldataByte: "0", perL2Storage: "0", perArbGasBase: "20000000", perArbGasCongestion: "379726000", perArbGasTotal: "399726000" },
    gasPerSecond: { s10: 0, s60: 0 },
    computeGasPerSecond: { s10: 0, s60: 0 },
    replayErrorBips: 0,
    ethUsd: null,
  };
}

/** The page around the charts: `tick` stands in for a live message or the ticker. */
function Page({ tick, loading = false }: { tick: number; loading?: boolean }) {
  return (
    <div>
      <span data-testid="tick">{tick}</span>
      <SeriesCharts network="robinhood" range="24h" series={series} loading={loading} model="constraints" />
    </div>
  );
}

describe("history charts across live re-renders", () => {
  it("does not redraw SeriesCharts when the page around it re-renders", () => {
    const { rerender } = render(<Page tick={0} />);
    const drawn = enlarged.mock.calls.length;
    expect(drawn).toBeGreaterThan(0);
    rerender(<Page tick={1} />);
    rerender(<Page tick={2} />);
    expect(enlarged.mock.calls.length).toBe(drawn);
  });

  it("does not redraw the charts inside SeriesCharts when a refetch flips loading", () => {
    const { rerender } = render(<Page tick={0} />);
    const cards = enlarged.mock.calls.length;
    const charts = framed.mock.calls.length;
    expect(charts).toBeGreaterThan(0);
    rerender(<Page tick={0} loading />);
    expect(enlarged.mock.calls.length).toBeGreaterThan(cards);
    expect(framed.mock.calls.length).toBe(charts);
  });

  it("does not rebuild the fee chart rows when only the live snapshot changes", () => {
    const { rerender } = render(<FeeFlows network="robinhood" range="24h" snapshot={snapshotAt("2026-09-06T07:20:00Z")} series={series} model="constraints" nowMs={1788679380000} />);
    const built = vi.mocked(withGapBreaks).mock.calls.length;
    expect(built).toBeGreaterThan(0);
    rerender(<FeeFlows network="robinhood" range="24h" snapshot={snapshotAt("2026-09-06T07:20:01Z")} series={series} model="constraints" nowMs={1788679381000} />);
    expect(vi.mocked(withGapBreaks).mock.calls.length).toBe(built);
  });
});

describe("components that keep their own clock", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(Date.parse("2026-09-06T07:20:03Z"));
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("ages the footer's last sample without a clock from the page", () => {
    render(<DataFooter snapshot={snapshotAt("2026-09-06T07:20:00Z")} series={series} networkInfo={null} status="open" apiStatus={null} />);
    expect(screen.getByText(/\(3 s ago\)/)).toBeInTheDocument();
    act(() => {
      vi.advanceTimersByTime(2_000);
    });
    expect(screen.getByText(/\(5 s ago\)/)).toBeInTheDocument();
  });
});
