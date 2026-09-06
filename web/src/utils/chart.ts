// Pure helpers that turn api shapes into what the charts draw.

import type { BatchPoint, ConstraintSet, ConstraintSetEntry, Series, SeriesPoint } from "@/types";
import { formatDuration, formatGas, weiToEthNumber, weiToGweiNumber } from "./format";

export const MAX_SERIES = 6;

/** CSS variable for a constraint's colour, by index in the set (fixed order, never cycled). */
export function seriesColor(index: number): string {
  return `var(--series-${Math.min(MAX_SERIES, index + 1)})`;
}

/** Colour of the "unknown split" series: the muted ink, never a constraint colour. */
export const UNKNOWN_COLOR = "var(--ink-3)";

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

/** Integer bips into x for display. Contributions are only ever divided here. */
export function bipsToXValue(bips: number): number {
  return bips / 10_000;
}

/** Each constraint's share of the total, 0 to 1, from integer bips. Zero total gives zero shares. */
export function sharesOf(bips: readonly number[]): number[] {
  const total = bips.reduce((sum, b) => sum + Math.max(0, b), 0);
  return bips.map((b) => (total > 0 ? Math.max(0, b) / total : 0));
}

/**
 * One drawable series: constraint `index` of constraint set `setId`. Series
 * are keyed by set and index so a replaced constraint never joins its
 * successor across an owner action. Legacy networks get a single pseudo
 * segment with set id 0 and no constraint definition.
 */
export type Segment = {
  key: `c${number}_${number}`;
  backlogKey: `b${number}_${number}`;
  setId: number;
  index: number;
  label: string;
  color: string;
  constraint: ConstraintSetEntry | null;
};

export const UNKNOWN_KEY = "cUnknown";
export const UNKNOWN_LABEL = "unknown split (total x, set not in range)";

export function contributionKey(setId: number, index: number): `c${number}_${number}` {
  return `c${setId}_${index}`;
}

export function backlogKey(setId: number, index: number): `b${number}_${number}` {
  return `b${setId}_${index}`;
}

export function targetKey(index: number): `tgt${number}` {
  return `tgt${index}`;
}

/** Short name of a set for legends and tooltips: "set 6 (from block 53,578,754)". */
export function setLabel(set: Pick<ConstraintSet, "id" | "effectiveBlock">): string {
  return `set ${set.id} (from block ${set.effectiveBlock.toLocaleString("en-US")})`;
}

export function segmentLabel(set: Pick<ConstraintSet, "id" | "effectiveBlock">, index: number, c: Pick<ConstraintSetEntry, "target" | "window">): string {
  return `C${index + 1} · ${shortConstraintLabel(c)} · ${setLabel(set)}`;
}

/** Sets sorted by the block they took effect, oldest first. */
export function sortedSets(series: Pick<Series, "constraintSets">): ConstraintSet[] {
  return [...series.constraintSets].sort((a, b) => a.effectiveBlock - b.effectiveBlock || a.id - b.id);
}

/** The constraint set with the newest effective block. */
export function latestSet(series: Pick<Series, "constraintSets">): ConstraintSet | undefined {
  return sortedSets(series).pop();
}

/** Segments for every set in the range, oldest set first; a single legacy segment when there are no sets. */
export function segmentsFor(series: Pick<Series, "constraintSets" | "points">): Segment[] {
  const sets = sortedSets(series);
  if (sets.length === 0) {
    const hasPoints = series.points.some((p) => p.constraintBips.length > 0 || p.backlogs.length > 0);
    return hasPoints ? [{ key: contributionKey(0, 0), backlogKey: backlogKey(0, 0), setId: 0, index: 0, label: "legacy backlog", color: seriesColor(0), constraint: null }] : [];
  }
  // A set is only usable for a point when its constraint count matches the
  // point's data; the collector can tag blocks with the latest *known* set
  // while the owner-action scan is still catching up (a 6-constraint genesis
  // set against a 2-constraint live model, for example). Such sets are left
  // out and their points fall back to the unknown split.
  const usable = sets.filter((set) => series.points.some((p) => p.constraintSetId === set.id && shapeMatches(set, p)));
  const out: Segment[] = [];
  for (const set of usable.length > 0 ? usable : sets) {
    set.constraints.slice(0, MAX_SERIES).forEach((c, i) => {
      out.push({ key: contributionKey(set.id, i), backlogKey: backlogKey(set.id, i), setId: set.id, index: i, label: segmentLabel(set, i, c), color: seriesColor(i), constraint: c });
    });
  }
  return out;
}

/** True when the set's constraint count matches the point's per-constraint data. */
export function shapeMatches(set: Pick<ConstraintSet, "constraints">, p: Pick<SeriesPoint, "constraintBips" | "backlogs">): boolean {
  const n = p.constraintBips.length > 0 ? p.constraintBips.length : p.backlogs.length;
  return n === 0 || set.constraints.length === n;
}

/** True when some point references a constraint set the series does not carry, or one whose shape does not match. */
export function hasUnknownSets(series: Pick<Series, "constraintSets" | "points">): boolean {
  if (series.constraintSets.length === 0) return false; // legacy model: one implicit segment
  const byId = new Map(series.constraintSets.map((s) => [s.id, s]));
  return series.points.some((p) => {
    const set = byId.get(p.constraintSetId);
    return !set || !shapeMatches(set, p);
  });
}

/** How many constraint slots the charts should draw: from the sets, or from the data for legacy networks. */
export function seriesCount(series: Pick<Series, "constraintSets" | "points">): number {
  const fromSets = Math.max(0, ...series.constraintSets.map((s) => s.constraints.length));
  if (fromSets > 0) return Math.min(MAX_SERIES, fromSets);
  const fromPoints = Math.max(0, ...series.points.map((p) => Math.max(p.backlogs.length, p.constraintBips.length)));
  return Math.min(MAX_SERIES, fromPoints);
}

/** Label for backlog panel `index`: every definition that slot had in the range, oldest first. */
export function slotLabel(series: Pick<Series, "constraintSets" | "points">, index: number): string {
  const segments = segmentsFor(series).filter((s) => s.index === index);
  if (segments.length === 0) return `C${index + 1}`;
  if (segments.length === 1 && segments[0].constraint === null) return segments[0].label;
  const parts = segments.map((s) => (s.constraint ? `${shortConstraintLabel(s.constraint)} (set ${s.setId})` : `set ${s.setId}`));
  return `C${index + 1} · ${parts.join(" then ")}`;
}

export type ChartPoint = {
  t: number;
  feeAvg: number;
  feeMin: number;
  feeMax: number;
  /** Floor in force at the bucket's last block, gwei; drawn as a stepped line. */
  floor: number;
  x: number;
  gps: number;
  feesEth: number;
  floorFeesEth: number;
  surplusFeesEth: number;
  blocks: number;
  replayErrorBips: number;
  constraintSetId: number;
  /** False when the point's set is not in the series; then only `cUnknown` carries x. */
  setKnown: boolean;
  /** Total x for points whose set is unknown, otherwise absent. */
  cUnknown?: number;
  /** Contribution of constraint i of set s to x, keyed cS_I; 0 while another set is in force. */
  [key: `c${number}_${number}`]: number;
  /** Backlog of constraint i of set s in gas, keyed bS_I; absent while another set is in force. */
  [key: `b${number}_${number}`]: number | undefined;
  /** Target of constraint slot i under the point's set, keyed tgtI; absent when unknown. */
  [key: `tgt${number}`]: number | undefined;
};

/**
 * Flattens a Series into chart rows with numbers the axes can scale.
 * Contributions come from the api's start-of-block `constraintBips`, never
 * from end-of-block backlogs, and are divided by 10,000 only for display.
 */
export function buildChartPoints(series: Series): ChartPoint[] {
  const segments = segmentsFor(series);
  const bySet = new Map<number, Segment[]>();
  for (const s of segments) {
    const list = bySet.get(s.setId) ?? [];
    list.push(s);
    bySet.set(s.setId, list);
  }
  return series.points.map((p) => {
    const candidate = bySet.get(p.constraintSetId);
    const n = p.constraintBips.length > 0 ? p.constraintBips.length : p.backlogs.length;
    const own = candidate && (n === 0 || candidate.length === Math.min(n, MAX_SERIES)) ? candidate : undefined;
    const row: ChartPoint = {
      t: p.t,
      feeAvg: weiToGweiNumber(p.baseFeeAvg),
      feeMin: weiToGweiNumber(p.baseFeeMin),
      feeMax: weiToGweiNumber(p.baseFeeMax),
      floor: weiToGweiNumber(p.minBaseFee),
      x: bipsToXValue(p.exponentBips),
      gps: p.gasPerSecond,
      feesEth: weiToEthNumber(p.feesWei),
      floorFeesEth: weiToEthNumber(p.floorFeesWei),
      surplusFeesEth: weiToEthNumber(p.surplusFeesWei),
      blocks: p.blocks,
      replayErrorBips: p.replayErrorBips,
      constraintSetId: p.constraintSetId,
      setKnown: own !== undefined,
    };
    for (const s of segments) row[s.key] = 0;
    if (own) {
      for (const s of own) {
        row[s.key] = bipsToXValue(p.constraintBips[s.index] ?? 0);
        row[s.backlogKey] = p.backlogs[s.index];
        if (s.constraint) row[targetKey(s.index)] = s.constraint.target;
      }
    } else {
      row.cUnknown = bipsToXValue(p.exponentBips);
    }
    return row;
  });
}

/** Sum of a wei-string field over points, as ETH. */
export function sumFeesEth(points: readonly Pick<SeriesPoint, "feesWei">[]): number {
  let total = 0n;
  for (const p of points) total += BigInt(p.feesWei);
  return weiToEthNumber(total);
}

/** Sum of a wei-string field over points, as ETH. */
export function sumWeiEth<K extends string>(points: readonly Record<K, string>[], key: K): number {
  let total = 0n;
  for (const p of points) total += BigInt(p[key]);
  return weiToEthNumber(total);
}

/** Span in seconds covered by the points (first to last bucket start plus one bucket). */
export function spanSeconds(points: readonly { t: number }[]): number {
  if (points.length < 2) return points.length === 1 ? 1 : 0;
  const step = points[1].t - points[0].t;
  return points[points.length - 1].t - points[0].t + step;
}

export function bucketStart(t: number, bucketSeconds: number): number {
  return Math.floor(t / bucketSeconds) * bucketSeconds;
}

/** Aggregates L2 fees into buckets of `bucketSeconds`, keyed by bucket start, wei summed before one conversion. */
export function resampleFees(points: readonly Pick<SeriesPoint, "t" | "feesWei">[], bucketSeconds: number): Map<number, number> {
  const out = new Map<number, bigint>();
  for (const p of points) {
    const start = bucketStart(p.t, bucketSeconds);
    out.set(start, (out.get(start) ?? 0n) + BigInt(p.feesWei));
  }
  return new Map([...out.entries()].map(([t, wei]) => [t, weiToEthNumber(wei)]));
}

export type BatchBucket = { t: number; weiSpent: bigint; batches: number };

/** Aggregates batch reports into buckets of `bucketSeconds`, so several reports in one bucket become one row. */
export function resampleBatches(batches: readonly Pick<BatchPoint, "t" | "weiSpent" | "batches">[], bucketSeconds: number): BatchBucket[] {
  const out = new Map<number, BatchBucket>();
  for (const b of batches) {
    const t = bucketStart(b.t, bucketSeconds);
    const acc = out.get(t) ?? { t, weiSpent: 0n, batches: 0 };
    acc.weiSpent += BigInt(b.weiSpent);
    acc.batches += b.batches;
    out.set(t, acc);
  }
  return [...out.values()].sort((a, b) => a.t - b.t);
}

export type CostRow = { t: number; l1Eth: number; l2Eth: number; batches: number };

/**
 * Joins batch spend with resampled L2 fees on shared buckets. Both sides are
 * grouped first, so each L2 bucket is attached exactly once however many
 * reports fall into it.
 */
export function joinCosts(batches: readonly Pick<BatchPoint, "t" | "weiSpent" | "batches">[], fees: Map<number, number>, bucketSeconds = 1): CostRow[] {
  return resampleBatches(batches, bucketSeconds).map((b) => ({ t: b.t, l1Eth: weiToEthNumber(b.weiSpent), l2Eth: fees.get(b.t) ?? 0, batches: b.batches }));
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
