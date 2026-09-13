import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { LiveSnapshot, NetworkStatus, Series, SeriesPoint } from "@/types";
import { WhyNow } from "./WhyNow";

const NOW = 1_788_679_200;

const snapshot: LiveSnapshot = {
  chainId: 4663,
  sampledAt: new Date(NOW * 1000).toISOString(),
  block: { number: 55_812_345, ts: NOW, gasUsed: 1, posterGas: 0, baseFee: "399726000", txCount: 1 },
  baseFee: "399726000",
  minBaseFee: "20000000",
  multiplierBips: 199_863,
  exponentBips: 32_425,
  model: "constraints",
  constraints: [
    { target: 60_000_000, window: 15, backlog: 3_111_506, exponentBips: 34 },
    { target: 40_000_000, window: 86_400, backlog: 11_194_391_810_886, exponentBips: 32_391 },
  ],
  prices: { perL2Tx: "0", perL1CalldataByte: "0", perL2Storage: "0", perArbGasBase: "0", perArbGasCongestion: "0", perArbGasTotal: "0" },
  gasPerSecond: { s10: 50_000_000, s60: 50_000_000 },
  computeGasPerSecond: { s10: 50_000_000, s60: 50_000_000 },
  replayErrorBips: 0,
  ethUsd: null,
};

const historyPoint = (t: number): SeriesPoint => ({
  t,
  blocks: 1,
  gasUsed: 1,
  posterGas: 0,
  gasPerSecond: 39_000_000,
  computeGasPerSecond: 39_000_000,
  coverage: 1,
  completeness: "complete",
  feesWei: "0",
  baseFeeMin: "200000000",
  baseFeeAvg: "200000000",
  baseFeeMax: "200000000",
  exponentBips: 1,
  constraintBips: [0, 1],
  backlogs: [0, 1],
  backlogsMax: [0, 1],
  minBaseFee: "20000000",
  floorFeesWei: "0",
  surplusFeesWei: "0",
  posterFeesWei: "0",
  constraintSetId: 1,
  replayErrorBips: 0,
});

const series: Series = {
  range: "24h",
  resolution: "1m",
  from: NOW - 86_400,
  to: NOW,
  constraintSets: [],
  ownerActions: [],
  points: Array.from({ length: 1_440 }, (_, index) => historyPoint(NOW - 86_400 + index * 60)),
};

const status: NetworkStatus = {
  name: "robinhood",
  chainId: 4663,
  enabled: true,
  headBlock: snapshot.block.number,
  headAt: snapshot.sampledAt,
  lagSeconds: 0,
  lastSampleAt: snapshot.sampledAt,
  lastError: null,
  rateLimitEvents: 0,
  last429At: null,
  backfillCursor: null,
  arbosVersion: "61",
  degraded: false,
  capacity: { configuredCallsPerSecond: 25, requiredCallsPerSecond: 20, observedCallsPerSecond: 20, headroomCallsPerSecond: 5, saturated: false, at: snapshot.sampledAt, checkpointError: false },
  holes: { pending: 0, blocks: 0, unfillable: 0, retrying: 0, oldestAgeSeconds: 0, checkpointError: false, pendingBlocks: 0, oldestPendingAt: null, oldestPendingAgeSeconds: null },
  status: "healthy",
  degradedReasons: [],
  collector: null,
  activeEndpoint: 0,
  failovers: 0,
  endpoints: [{ index: 0, ws: true, archive: true, disabled: false, error: null, wsCooling: false, wsError: null }],
};

describe("WhyNow", () => {
  it("answers the constraint-model questions with a matched long-window rate", () => {
    render(<WhyNow snapshot={snapshot} series={series} seriesLoading={false} liveStatus="open" networkStatus={status} listener={{ ready: true, reconnects: 0, lastError: null }} now={NOW * 1000} />);
    expect(screen.getByRole("heading", { name: "Why now?" })).toBeInTheDocument();
    expect(screen.getByText(/Current base fee:/)).toHaveTextContent("0.3997 gwei, 19.99× the 0.02 gwei floor");
    expect(screen.getByText(/Up 99.9%/)).toBeInTheDocument();
    expect(screen.getByText(/C2, the 24 h window, contributes 99.9% of x/)).toBeInTheDocument();
    expect(screen.getByText(/24 h compute rate 39 Mgas\/s vs 40 Mgas\/s C2 target/)).toBeInTheDocument();
    expect(screen.getByText(/pressure is draining/)).toHaveTextContent("Deterministic scenario");
    expect(screen.getByText(/pressure is draining/)).toHaveTextContent("Not a forecast");
    expect(screen.getByText("Data healthy")).toBeInTheDocument();
    expect(screen.getByText("WebSocket")).toBeInTheDocument();
    expect(screen.getByText(/source endpoint 1 of 1/)).toBeInTheDocument();
    expect(screen.getByText(/24 h history is fully indexed/)).toBeInTheDocument();
  });

  it("uses honest legacy language with no invented constraint share", () => {
    const legacy = { ...snapshot, model: "legacy" as const, constraints: [], legacy: { speedLimit: 7_000_000, inertia: 102, tolerance: 10, backlog: 90_000_000 } };
    render(<WhyNow snapshot={legacy} series={series} seriesLoading={false} liveStatus="polling" networkStatus={status} now={NOW * 1000} />);
    expect(screen.getByText(/Legacy backlog 90 Mgas/)).toHaveTextContent("no constraint share applies");
    expect(screen.getByText(/1 min compute rate 50 Mgas\/s vs 7 Mgas\/s legacy speed limit/)).toBeInTheDocument();
    expect(screen.getByText(/pressure is building/)).toBeInTheDocument();
    expect(screen.getByText("REST fallback")).toBeInTheDocument();
    expect(screen.getByText("Live sample current")).toBeInTheDocument();
    expect(screen.queryByText("Data healthy")).toBeNull();
  });

  it("distinguishes a below-tolerance legacy backlog from current fee pressure", () => {
    const legacy = { ...snapshot, exponentBips: 0, model: "legacy" as const, constraints: [], legacy: { speedLimit: 7_000_000, inertia: 102, tolerance: 10, backlog: 50_000_000 }, computeGasPerSecond: { s10: 1_000_000, s60: 1_000_000 } };
    render(<WhyNow snapshot={legacy} series={series} seriesLoading={false} liveStatus="open" networkStatus={status} now={NOW * 1000} />);
    expect(screen.getByText(/Legacy backlog 50 Mgas/)).toHaveTextContent("Current sampled x is zero");
    expect(screen.getByText(/Legacy backlog 50 Mgas/)).toHaveTextContent("no fee pressure");
    expect(screen.getByText(/Fee pressure is clear at the measured rate/)).toHaveTextContent("Not a forecast");
  });

  it("makes a near-target direction visible even when the headline rates round alike", () => {
    const near = { ...series, points: series.points.map((point) => ({ ...point, computeGasPerSecond: 40_000_001 })) };
    render(<WhyNow snapshot={snapshot} series={near} seriesLoading={false} liveStatus="open" networkStatus={status} now={NOW * 1000} />);
    expect(screen.getByText(/24 h compute rate 40 Mgas\/s vs 40 Mgas\/s/)).toHaveTextContent("1 gas/s above target");
    expect(screen.getByText(/pressure is building/)).toBeInTheDocument();
  });

  it("withholds causal claims for stale and partial data", () => {
    const partial = { ...series, points: series.points.map((point, index) => (index === 500 ? { ...point, completeness: "unknown" as const, coverage: null } : point)) };
    render(<WhyNow snapshot={snapshot} series={partial} seriesLoading={false} liveStatus="open" networkStatus={{ ...status, status: "degraded", degraded: true, degradedReasons: ["collector trails observed head"], holes: { ...status.holes, blocks: 12 } }} now={NOW * 1000 + 16_000} />);
    expect(screen.getByText(/Latest indexed base fee/)).toBeInTheDocument();
    expect(screen.getByText("Unavailable while the live sample is stale.")).toBeInTheDocument();
    expect(screen.getByText("Pressure source is unavailable while the live sample is stale.")).toBeInTheDocument();
    expect(screen.queryByText(/C2, the 24 h window/)).toBeNull();
    expect(screen.getByText("Building or draining is unavailable without fresh, matched demand.")).toBeInTheDocument();
    expect(screen.getByText("Live data stale")).toBeInTheDocument();
    expect(screen.getByText(/collector trails observed head/)).toHaveTextContent("12 blocks are not indexed");
    expect(screen.getByText(/bucket has unknown completeness/)).toBeInTheDocument();
  });

  it("surfaces limited coverage even when a contradictory bucket says complete", () => {
    const limited = { ...series, points: series.points.map((point, index) => (index === 500 ? { ...point, coverage: 0.5 } : point)) };
    render(<WhyNow snapshot={snapshot} series={limited} seriesLoading={false} liveStatus="open" networkStatus={status} now={NOW * 1000} />);
    expect(screen.getByText("History partial")).toBeInTheDocument();
    expect(screen.getByText(/bucket has limited coverage/)).toBeInTheDocument();
  });

  it("does not call history healthy when a complete bucket has unknown coverage", () => {
    const unknownCoverage = { ...series, points: series.points.map((point, index) => (index === 500 ? { ...point, coverage: null } : point)) };
    render(<WhyNow snapshot={snapshot} series={unknownCoverage} seriesLoading={false} liveStatus="open" networkStatus={status} listener={{ ready: true, reconnects: 0, lastError: null }} now={NOW * 1000} />);
    expect(screen.getByText("History uncertain")).toBeInTheDocument();
    expect(screen.getByText(/complete bucket has unknown or invalid coverage/)).toBeInTheDocument();
    expect(screen.queryByText("Data healthy")).toBeNull();
  });

  it("keeps missing or invalid constraint pressure distinct from a known zero", () => {
    const { rerender } = render(<WhyNow snapshot={{ ...snapshot, constraints: [] }} series={series} seriesLoading={false} liveStatus="open" networkStatus={status} now={NOW * 1000} />);
    expect(screen.getByText("Constraint pressure is unavailable.")).toBeInTheDocument();
    rerender(<WhyNow snapshot={{ ...snapshot, constraints: snapshot.constraints.map((constraint) => ({ ...constraint, exponentBips: 0 })) }} series={series} seriesLoading={false} liveStatus="open" networkStatus={status} now={NOW * 1000} />);
    expect(screen.getByText("No constraint currently contributes backlog pressure.")).toBeInTheDocument();
    rerender(<WhyNow snapshot={{ ...snapshot, constraints: [{ ...snapshot.constraints[0], exponentBips: Number.NaN }] }} series={series} seriesLoading={false} liveStatus="open" networkStatus={status} now={NOW * 1000} />);
    expect(screen.getByText("Constraint pressure is unavailable.")).toBeInTheDocument();
  });

  it("states every unavailable answer before the first sample", () => {
    render(<WhyNow snapshot={null} series={null} seriesLoading liveStatus="connecting" networkStatus={null} now={NOW * 1000} />);
    expect(screen.getByText(/current fee, its pressure source, recent change, matched demand, and direction/)).toBeInTheDocument();
    expect(screen.getByText("No live sample")).toBeInTheDocument();
    expect(screen.getByText("Connecting transport")).toBeInTheDocument();
  });
});
