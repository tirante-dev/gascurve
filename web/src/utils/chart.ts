// Pure helpers that turn api shapes into what the charts draw.

import type { BatchPoint, ConstraintSet, ConstraintSetEntry, Series, SeriesPoint } from "@/types";
import { formatDuration, formatGas, weiToEthNumber, weiToGweiNumber } from "./format";

export const MAX_SERIES = 6;

/** CSS variable for a constraint's colour, by index in the set (fixed order, never cycled). */
export function seriesColor(index: number): string {
  return `var(--series-${Math.min(MAX_SERIES, index + 1)})`;
}

/** Sequential ramp step (1 to 9) for a multiplier over the floor, on a log scale from 1x to 100x. */
export function rampStep(multiplierBips: number): number {
  const m = Math.max(1, multiplierBips / 10_000);
  const f = Math.min(1, Math.log10(m) / 2);
  return 1 + Math.round(f * 8);
}

export function rampColor(multiplierBips: number): string {
  return `var(--seq-${rampStep(multiplierBips)})`;
}

/** Text colour that clears contrast on the given ramp step. */
export function rampInk(step: number): string {
  return step >= 6 ? "var(--seq-ink-dark)" : "var(--seq-ink-light)";
}

/** Ramp step for a single constraint's exponent contribution, 0 to 4 spans the ramp. */
export function contributionRampStep(exponentBips: number): number {
  const x = Math.max(0, exponentBips / 10_000);
  return 1 + Math.round(Math.min(1, x / 4) * 8);
}

/** "60M gas/s over 15 s". */
export function constraintLabel(c: Pick<ConstraintSetEntry, "target" | "window">): string {
  return `${formatGas(c.target)} gas/s over ${formatDuration(c.window)}`;
}

export function shortConstraintLabel(c: Pick<ConstraintSetEntry, "target" | "window">): string {
  return `${formatGas(c.target)}/s · ${formatDuration(c.window)}`;
}

/** Per-constraint exponent contributions (as x, not bips) for a point under its constraint set. */
export function contributionsFor(backlogs: readonly number[], set: ConstraintSet | undefined): number[] {
  if (!set) return backlogs.map(() => 0);
  return backlogs.map((b, i) => {
    const c = set.constraints[i];
    if (!c || c.target <= 0 || c.window <= 0) return 0;
    return Math.max(0, b) / (c.target * c.window);
  });
}

export type ChartPoint = {
  t: number;
  feeAvg: number;
  feeMin: number;
  feeMax: number;
  x: number;
  gps: number;
  feesEth: number;
  blocks: number;
  replayErrorBips: number;
  constraintSetId: number;
  /** Contribution of constraint i to x, keyed c0..c5. */
  [key: `c${number}`]: number;
  /** Backlog of constraint i in gas, keyed b0..b5. */
  [key: `b${number}`]: number;
};

/** Flattens a Series into chart rows with numbers the axes can scale. */
export function buildChartPoints(series: Series): ChartPoint[] {
  const sets = new Map(series.constraintSets.map((s) => [s.id, s]));
  return series.points.map((p) => {
    const set = sets.get(p.constraintSetId);
    const row: ChartPoint = {
      t: p.t,
      feeAvg: weiToGweiNumber(p.baseFeeAvg),
      feeMin: weiToGweiNumber(p.baseFeeMin),
      feeMax: weiToGweiNumber(p.baseFeeMax),
      x: p.exponentBips / 10_000,
      gps: p.gasPerSecond,
      feesEth: weiToEthNumber(p.feesWei),
      blocks: p.blocks,
      replayErrorBips: p.replayErrorBips,
      constraintSetId: p.constraintSetId,
    };
    const contributions = contributionsFor(p.backlogs, set);
    for (let i = 0; i < Math.min(MAX_SERIES, p.backlogs.length); i++) {
      row[`c${i}`] = set ? contributions[i] : p.exponentBips / 10_000;
      row[`b${i}`] = p.backlogs[i];
    }
    return row;
  });
}

/** The constraint set with the newest effective time, used for legends and target lines. */
export function latestSet(series: Pick<Series, "constraintSets">): ConstraintSet | undefined {
  return [...series.constraintSets].sort((a, b) => a.effectiveBlock - b.effectiveBlock).pop();
}

/** How many constraint series the charts should draw: from the sets, or from the data for legacy networks. */
export function seriesCount(series: Series): number {
  const fromSets = Math.max(0, ...series.constraintSets.map((s) => s.constraints.length));
  if (fromSets > 0) return Math.min(MAX_SERIES, fromSets);
  const fromPoints = Math.max(0, ...series.points.map((p) => p.backlogs.length));
  return Math.min(MAX_SERIES, fromPoints);
}

/** Legend label for series index i, from the latest set or a generic legacy label. */
export function seriesLabel(series: Series, index: number): string {
  const set = latestSet(series);
  const c = set?.constraints[index];
  if (c) return `C${index + 1} · ${shortConstraintLabel(c)}`;
  return series.constraintSets.length === 0 ? "legacy backlog" : `C${index + 1} · earlier set`;
}

/** Sum of a wei-string field over points, as ETH. */
export function sumFeesEth(points: readonly Pick<SeriesPoint, "feesWei">[]): number {
  let total = 0n;
  for (const p of points) total += BigInt(p.feesWei);
  return weiToEthNumber(total);
}

/** Span in seconds covered by the points (first to last bucket start plus one bucket). */
export function spanSeconds(points: readonly { t: number }[]): number {
  if (points.length < 2) return points.length === 1 ? 1 : 0;
  const step = points[1].t - points[0].t;
  return points[points.length - 1].t - points[0].t + step;
}

/** Aggregates L2 fees into buckets of `bucketSeconds`, keyed by bucket start. */
export function resampleFees(points: readonly Pick<SeriesPoint, "t" | "feesWei">[], bucketSeconds: number): Map<number, number> {
  const out = new Map<number, bigint>();
  for (const p of points) {
    const start = Math.floor(p.t / bucketSeconds) * bucketSeconds;
    out.set(start, (out.get(start) ?? 0n) + BigInt(p.feesWei));
  }
  return new Map([...out.entries()].map(([t, wei]) => [t, weiToEthNumber(wei)]));
}

export type CostRow = { t: number; l1Eth: number; l2Eth: number; batches: number };

/** Joins batch spend with resampled L2 fees on the batch buckets. */
export function joinCosts(batches: readonly BatchPoint[], fees: Map<number, number>): CostRow[] {
  return batches.map((b) => ({ t: b.t, l1Eth: weiToEthNumber(b.weiSpent), l2Eth: fees.get(b.t) ?? 0, batches: b.batches }));
}

/** Points for the P4 versus e^x comparison, x in [0, max]. */
export function taylorCurve(p4: (x: number) => number, exp: (x: number) => number, max = 5, step = 0.1): { x: number; p4: number; exp: number }[] {
  const out: { x: number; p4: number; exp: number }[] = [];
  for (let i = 0; i * step <= max + 1e-9; i++) {
    const x = Math.round(i * step * 100) / 100;
    out.push({ x, p4: p4(x), exp: exp(x) });
  }
  return out;
}

/** Log-axis domain padding: a clean pair of bounds around the data. */
export function logDomain(values: readonly number[], floor?: number): [number, number] {
  const positive = values.filter((v) => Number.isFinite(v) && v > 0);
  if (floor !== undefined && floor > 0) positive.push(floor);
  if (positive.length === 0) return [0.001, 1];
  const min = Math.min(...positive);
  const max = Math.max(...positive);
  return [10 ** Math.floor(Math.log10(min)), 10 ** Math.ceil(Math.log10(max * 1.0001))];
}
