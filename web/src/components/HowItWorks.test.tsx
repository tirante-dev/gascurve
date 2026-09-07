import { render, screen } from "@testing-library/react";
import { cloneElement, isValidElement } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { ConstraintsResponse, LiveSnapshot, Network } from "@/types";

// ResponsiveContainer measures its box and jsdom has none: give the chart a
// size so recharts really draws it.
vi.mock("recharts", async (importOriginal) => {
  const original = await importOriginal<typeof import("recharts")>();
  const Sized = ({ children }: { children: React.ReactNode }) => (
    <div style={{ width: 400, height: 200 }}>
      {isValidElement<{ width?: number; height?: number }>(children) ? cloneElement(children, { width: 400, height: 200 }) : children}
    </div>
  );
  return { ...original, ResponsiveContainer: Sized };
});

const networks: Network[] = [
  { name: "robinhood", displayName: "Robinhood Chain", chainId: 4663, explorerUrl: "https://explorer.example", model: "constraints", headBlock: 10, headAt: null, lagSeconds: null, enabled: true },
];

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

const constraints: ConstraintsResponse = {
  current: {
    id: 6,
    effectiveBlock: 53_578_754,
    effectiveAt: "2026-09-03T17:08:00Z",
    source: "owner_action",
    constraints: [
      { target: 60_000_000, window: 15, startingBacklog: 0 },
      { target: 40_000_000, window: 86_400, startingBacklog: 9_989_000_000_000 },
    ],
  },
  history: [],
};

/** What each key's request answered with, so a test can starve one of them. */
let answers: Record<string, unknown> = {};
let failures: Record<string, string> = {};
vi.mock("@/hooks/useApi", () => ({
  useApi: (key: string | null) => ({
    data: key === null ? null : (answers[key] ?? null),
    error: key === null ? null : (failures[key] ?? null),
    loading: false,
    updatedAt: null,
    refresh: () => undefined,
  }),
}));

import { HowItWorks, quotedConstraints } from "./HowItWorks";
import HowItWorksRoute, { generateMetadata } from "@/app/[network]/how-it-works/page";

describe("HowItWorks", () => {
  beforeEach(() => {
    answers = { "robinhood:live-quote": snapshot, "robinhood:constraints": constraints, networks };
    failures = {};
  });

  it("puts the explainer and the P4 chart on a network-scoped page that quotes that chain's floor and set", () => {
    render(<HowItWorks network="robinhood" />);
    expect(screen.getByRole("heading", { name: "How the fee works", level: 2 })).toBeInTheDocument();
    // The prose, with this chain's floor filled into it.
    expect(screen.getByRole("heading", { name: "Two parts, one fee" })).toBeInTheDocument();
    expect(screen.getByText(/the floor \(0.02 gwei\) multiplied by/)).toBeInTheDocument();
    // The quoted floor and the set in force, from the api.
    expect(screen.getByText("0.02 gwei")).toBeInTheDocument();
    expect(screen.getByText("60 Mgas/s · 15 s")).toBeInTheDocument();
    expect(screen.getByText("40 Mgas/s · 24 h")).toBeInTheDocument();
    expect(screen.getByText(/in force since block 53,578,754, 2026-09-03 17:08 UTC \(owner action\)/)).toBeInTheDocument();
    // The chart moved here with its live marker.
    expect(screen.getByRole("figure", { name: /Degree-4 Taylor polynomial/ })).toBeInTheDocument();
    expect(screen.getByText(/The dot marks the live x/)).toBeInTheDocument();
    // And the way back to the live page.
    expect(screen.getByRole("link", { name: "← Live view" })).toHaveAttribute("href", "/robinhood");
    expect(screen.getByText("Robinhood Chain · chain 4663")).toBeInTheDocument();
  });

  it("still reads when the api answers with nothing, quoting no figures it does not have", () => {
    answers = {};
    failures = { "robinhood:live-quote": "boom" };
    render(<HowItWorks network="robinhood" />);
    // The generic form of the prose, and no invented floor or set.
    expect(screen.getByRole("heading", { name: "Long windows ratchet, short windows spike" })).toBeInTheDocument();
    expect(screen.getAllByText("unavailable right now")).toHaveLength(2);
    expect(screen.getByText(/did not answer for the live values/)).toBeInTheDocument();
    expect(screen.queryByText(/The dot marks the live x/)).toBeNull();
    expect(screen.getByRole("figure", { name: /Degree-4 Taylor polynomial/ })).toBeInTheDocument();
    // The header falls back to the route's name for a network the api has not described.
    expect(screen.getByText("robinhood")).toBeInTheDocument();
  });

  it("falls back to the live snapshot's own definition when the constraint endpoint has nothing", () => {
    answers = { "robinhood:live-quote": snapshot, networks };
    render(<HowItWorks network="robinhood" />);
    expect(screen.getByText("60 Mgas/s · 15 s")).toBeInTheDocument();
    expect(screen.queryByText(/in force since block/)).toBeNull();
    // And a legacy chain has no set to quote at all.
    expect(quotedConstraints(null, { ...snapshot, model: "legacy", constraints: [] })).toBeNull();
    expect(quotedConstraints(null, null)).toBeNull();
    expect(quotedConstraints([{ target: 1, window: 2, startingBacklog: 3 }], null)).toHaveLength(1);
  });

  it("names the legacy pricer instead of a constraint set", () => {
    answers = { "robinhood:live-quote": { ...snapshot, model: "legacy", constraints: [], legacy: { speedLimit: 7_000_000, inertia: 102, tolerance: 10, backlog: 90_000_000 } }, networks };
    render(<HowItWorks network="robinhood" />);
    expect(screen.getByText(/legacy speed-limit pricer/)).toBeInTheDocument();
    expect(screen.queryByText("Constraint set in force")).toBeNull();
  });
});

describe("the /[network]/how-it-works route", () => {
  beforeEach(() => {
    answers = { "robinhood:live-quote": snapshot, "robinhood:constraints": constraints, networks };
    failures = {};
  });

  it("renders the explainer for the network in the path and titles the tab with it", async () => {
    await expect(generateMetadata({ params: Promise.resolve({ network: "robinhood" }) })).resolves.toEqual({ title: "How the robinhood fee works" });
    render(await HowItWorksRoute({ params: Promise.resolve({ network: "robinhood" }) }));
    expect(screen.getByRole("heading", { name: "How the fee works", level: 2 })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "← Live view" })).toHaveAttribute("href", "/robinhood");
  });
});
