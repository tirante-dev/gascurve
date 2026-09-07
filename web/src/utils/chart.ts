// Pure helpers that turn api shapes into what the charts draw.

import { coverageOf, partialKinds, type PartialKind } from "@/lib/partial";
import { saturatingCastToBips, saturatingUMul, toUint64 } from "@/lib/pricer";
import type { BatchPoint, ConstraintSet, ConstraintSetEntry, PricerModel, Series, SeriesPoint } from "@/types";
import { formatDuration, formatGas, formatGasPerSecond, formatInteger, weiToEthNumber, weiToGweiNumber } from "./format";

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

/**
 * Text colour that clears AA contrast on the given ramp step. Each step has
 * its own ink token because the step at which the ramp flips from light ink
 * to dark ink differs between the light and dark surfaces.
 */
export function rampInk(step: number): string {
  return `var(--seq-ink-${Math.max(1, Math.min(9, Math.round(step)))})`;
}

/** The floor line and its legend swatch: the cyan accent, never a constraint colour. */
export const FLOOR_COLOR = "var(--floor)";

/** Owner-action markers on the history charts: the magenta accent. */
export const MARKER_COLOR = "var(--marker)";

/** Ramp step for a single constraint's exponent contribution, 0 to 4 spans the ramp. */
export function contributionRampStep(exponentBips: number): number {
  const x = Math.max(0, exponentBips / 10_000);
  return 1 + Math.round(Math.min(1, x / 4) * 8);
}

/** "60 Mgas/s over 15 s". */
export function constraintLabel(c: Pick<ConstraintSetEntry, "target" | "window">): string {
  return `${formatGasPerSecond(c.target)} over ${formatDuration(c.window)}`;
}

export function shortConstraintLabel(c: Pick<ConstraintSetEntry, "target" | "window">): string {
  return `${formatGasPerSecond(c.target)} · ${formatDuration(c.window)}`;
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
 * successor across an owner action. Legacy networks (by the network's
 * `model`, never inferred from an empty set list) get a single pseudo segment
 * with set id 0 and no constraint definition.
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
export const UNKNOWN_LABEL = "unknown split (total x, constraint set unknown)";
/** The same series for points whose set is known but whose per-constraint split predates the record (a null `constraintBips`). */
export const NULL_SPLIT_LABEL = "unknown split (total x, split not recorded)";
/** Legend and tooltip label of the fee destination series for buckets whose floor and surplus predate the record. */
export const UNSPLIT_FEES_LABEL = "unknown split (predates the fee split)";

/** Backlog of slot `index` for points whose constraint set is unknown, keyed buI. */
export function unknownBacklogKey(index: number): `bu${number}` {
  return `bu${index}`;
}

/** Label of backlog panel `index` when no definition for it is known. */
export function unknownSlotLabel(index: number): string {
  return `C${index + 1} · definition unknown`;
}

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

/** How many constraint slots a point's data has: from its split when recorded, else from its backlogs. */
function slotCount(p: Pick<SeriesPoint, "constraintBips" | "backlogs">): number {
  const bips = p.constraintBips;
  return bips !== null && bips.length > 0 ? bips.length : p.backlogs.length;
}

/** True when a point carries per-constraint data at all. */
function hasConstraintData(p: Pick<SeriesPoint, "constraintBips" | "backlogs">): boolean {
  return slotCount(p) > 0;
}

/**
 * Segments for every set in the range, oldest set first. A legacy network
 * gets a single pseudo segment. With no sets and a constraints (or unknown)
 * model there is nothing to label: every point is drawn as the unknown split
 * and its backlogs under unknown-slot keys.
 */
export function segmentsFor(series: Pick<Series, "constraintSets" | "points">, model: PricerModel): Segment[] {
  const sets = sortedSets(series);
  if (sets.length === 0) {
    if (model !== "legacy" || !series.points.some(hasConstraintData)) return [];
    return [{ key: contributionKey(0, 0), backlogKey: backlogKey(0, 0), setId: 0, index: 0, label: "legacy backlog", color: seriesColor(0), constraint: null }];
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
  const n = slotCount(p);
  return n === 0 || set.constraints.length === n;
}

/** The segments that describe a point: those of its set when the shape agrees, otherwise none (unknown split). */
function ownSegments(bySet: Map<number, Segment[]>, p: Pick<SeriesPoint, "constraintSetId" | "constraintBips" | "backlogs">): Segment[] | undefined {
  const candidate = bySet.get(p.constraintSetId);
  const n = slotCount(p);
  return candidate && (n === 0 || candidate.length === Math.min(n, MAX_SERIES)) ? candidate : undefined;
}

function groupBySet(segments: readonly Segment[]): Map<number, Segment[]> {
  const bySet = new Map<number, Segment[]>();
  for (const s of segments) {
    const list = bySet.get(s.setId) ?? [];
    list.push(s);
    bySet.set(s.setId, list);
  }
  return bySet;
}

/**
 * True when some point has no segments to draw under: its set is not in the
 * series, its shape does not match, or the model gives no sets at all. An
 * empty set list is never taken as legacy on its own.
 */
export function hasUnknownSets(series: Pick<Series, "constraintSets" | "points">, model: PricerModel): boolean {
  const bySet = groupBySet(segmentsFor(series, model));
  return series.points.some((p) => ownSegments(bySet, p) === undefined);
}

/** True when some point's per-constraint split was never recorded (history from before migration 000006). */
export function hasUnrecordedSplit(series: Pick<Series, "points">): boolean {
  return series.points.some((p) => p.constraintBips === null);
}

/** True when some point is drawn as the unknown split: its set is unknown, or its split was not recorded. */
export function hasUnknownSplit(series: Pick<Series, "constraintSets" | "points">, model: PricerModel): boolean {
  return hasUnrecordedSplit(series) || hasUnknownSets(series, model);
}

/** How many constraint slots the charts should draw: from the sets, or from the data for legacy networks. */
export function seriesCount(series: Pick<Series, "constraintSets" | "points">): number {
  const fromSets = Math.max(0, ...series.constraintSets.map((s) => s.constraints.length));
  if (fromSets > 0) return Math.min(MAX_SERIES, fromSets);
  const fromPoints = Math.max(0, ...series.points.map((p) => Math.max(p.backlogs.length, p.constraintBips?.length ?? 0)));
  return Math.min(MAX_SERIES, fromPoints);
}

/** Label for backlog panel `index`: every definition that slot had in the range, oldest first; unlabelled when none is known. */
export function slotLabel(series: Pick<Series, "constraintSets" | "points">, index: number, model: PricerModel): string {
  const segments = segmentsFor(series, model).filter((s) => s.index === index);
  if (segments.length === 0) return unknownSlotLabel(index);
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
  /** The floor and congestion parts of `feesEth`; null for buckets that predate the fee split, when `unsplitFeesEth` carries the whole. */
  floorFeesEth: number | null;
  surplusFeesEth: number | null;
  /** `feesEth` for buckets whose destination split is unknown, null otherwise. */
  unsplitFeesEth: number | null;
  blocks: number;
  /** The share of the bucket the collector indexed; 1 for a whole one. */
  coverage: number;
  /**
   * Which kind of partial bucket this is, null for a whole one. Rates and
   * averages are drawn on a partial bucket as they are; the sums are not.
   */
  partial: PartialKind | null;
  replayErrorBips: number;
  constraintSetId: number;
  /** False when the point's set is not in the series; then its backlogs sit under the `buI` keys. */
  setKnown: boolean;
  /** False when the point's per-constraint split is not drawn under its set (set unknown, or split not recorded); then only `cUnknown` carries x. */
  splitKnown: boolean;
  /** Total x for points whose split is unknown, null otherwise. */
  cUnknown: number | null;
  /** True for the duplicated row that closes a set at the boundary where its successor starts. */
  boundary?: boolean;
  /** Contribution of constraint i of set s to x, keyed cS_I; null while another set is in force (a gap, never a taper to zero). */
  [key: `c${number}_${number}`]: number | null;
  /** Backlog of constraint i of set s in gas, keyed bS_I; null while another set is in force. */
  [key: `b${number}_${number}`]: number | null;
  /** Backlog of slot i for points whose set is unknown, keyed buI; null otherwise. */
  [key: `bu${number}`]: number | null;
  /** Target of constraint slot i under the point's set, keyed tgtI; absent when unknown. */
  [key: `tgt${number}`]: number | undefined;
};

/**
 * Flattens a Series into chart rows with numbers the axes can scale.
 * Contributions come from the api's start-of-block `constraintBips`, never
 * from end-of-block backlogs, and are divided by 10,000 only for display. A
 * point with a null split (history from before migration 000006) keeps its
 * set for backlogs and targets but puts its whole x under `cUnknown`; null
 * floor and surplus fees leave both null and put the bucket's fees under
 * `unsplitFeesEth`, never zero. One row per bucket; `withSetBoundaries` adds
 * the rows the stacked charts need.
 */
export function buildChartPoints(series: Series, model: PricerModel): ChartPoint[] {
  const segments = segmentsFor(series, model);
  const bySet = groupBySet(segments);
  const slots = seriesCount(series);
  // Which buckets the collector indexed only part of, from their position in
  // the range: the last one is still filling, an earlier one is where
  // indexing began.
  const kinds = partialKinds(series.points);
  return series.points.map((p, i) => {
    const own = ownSegments(bySet, p);
    const split = p.constraintBips;
    const feesEth = weiToEthNumber(p.feesWei);
    const floorWei = p.floorFeesWei;
    const surplusWei = p.surplusFeesWei;
    const feeSplitKnown = floorWei !== null && surplusWei !== null;
    const row: ChartPoint = {
      t: p.t,
      feeAvg: weiToGweiNumber(p.baseFeeAvg),
      feeMin: weiToGweiNumber(p.baseFeeMin),
      feeMax: weiToGweiNumber(p.baseFeeMax),
      floor: weiToGweiNumber(p.minBaseFee),
      x: bipsToXValue(p.exponentBips),
      gps: p.gasPerSecond,
      feesEth,
      floorFeesEth: feeSplitKnown ? weiToEthNumber(floorWei) : null,
      surplusFeesEth: feeSplitKnown ? weiToEthNumber(surplusWei) : null,
      unsplitFeesEth: feeSplitKnown ? null : feesEth,
      blocks: p.blocks,
      coverage: coverageOf(p),
      partial: kinds[i],
      replayErrorBips: p.replayErrorBips,
      constraintSetId: p.constraintSetId,
      setKnown: own !== undefined,
      splitKnown: own !== undefined && split !== null,
      cUnknown: null,
    };
    for (const s of segments) {
      row[s.key] = null;
      row[s.backlogKey] = null;
    }
    for (let i = 0; i < slots; i++) row[unknownBacklogKey(i)] = null;
    if (own) {
      for (const s of own) {
        // A split that was never recorded stays null: zero would read as a
        // constraint that contributed nothing, which is a different fact.
        const contribution = split ? split[s.index] : undefined;
        row[s.key] = contribution === undefined ? null : bipsToXValue(contribution);
        row[s.backlogKey] = p.backlogs[s.index] ?? null;
        if (s.constraint) row[targetKey(s.index)] = s.constraint.target;
      }
    } else {
      for (let i = 0; i < slots; i++) row[unknownBacklogKey(i)] = p.backlogs[i] ?? null;
    }
    if (!row.splitKnown) row.cUnknown = bipsToXValue(p.exponentBips);
    return row;
  });
}

/** Which drawn series a row belongs to: its set when known (with or without a recorded split), otherwise the unknown split. */
function drawnSeriesOf(row: ChartPoint): string {
  return `${row.setKnown ? row.constraintSetId : "unknown"}:${row.splitKnown ? "split" : "unsplit"}`;
}

/**
 * Rows for the stacked and per-set charts: wherever the set in force changes,
 * a duplicate of the boundary bucket is inserted first, carrying the previous
 * set's contributions, backlogs and targets at the new bucket's time. The
 * outgoing series therefore ends with a vertical edge exactly where the
 * incoming one starts, an instantaneous replacement rather than a taper across
 * the bucket. Everything else on the duplicate (fee, gas, x) is the new
 * bucket's, so the shared lines gain a zero-length segment and nothing more.
 */
export function withSetBoundaries(rows: readonly ChartPoint[]): ChartPoint[] {
  const out: ChartPoint[] = [];
  for (let i = 0; i < rows.length; i++) {
    const row = rows[i];
    const prev = i > 0 ? rows[i - 1] : undefined;
    if (prev && drawnSeriesOf(prev) !== drawnSeriesOf(row)) {
      const edge: ChartPoint = { ...row, boundary: true, constraintSetId: prev.constraintSetId, setKnown: prev.setKnown, splitKnown: prev.splitKnown, cUnknown: prev.cUnknown };
      for (const key of Object.keys(prev)) {
        if (/^(c\d+_\d+|b\d+_\d+|bu\d+|tgt\d+)$/.test(key)) {
          const k = key as `c${number}_${number}`;
          edge[k] = prev[k];
        }
      }
      for (const key of Object.keys(row)) {
        if (/^tgt\d+$/.test(key) && !(key in prev)) delete edge[key as `tgt${number}`];
      }
      out.push(edge);
    }
    out.push(row);
  }
  return out;
}

/** The most tick marks a gauge draws, whatever its scale; a billion windows must not become a billion elements. */
export const MAX_GAUGE_MARKS = 24;

/** The largest integer a double represents exactly; above it a ratio is taken in BigInt instead. */
const MAX_EXACT = BigInt(Number.MAX_SAFE_INTEGER);
/** Fixed-point scale for ratios of uint64-scale integers. */
const FRACTION_SCALE = 1_000_000_000_000n;

/**
 * `numerator / denominator` as a bounded, non-negative fraction, computed on
 * integers. Both sides divide exactly as doubles in every ordinary case;
 * above 2^53 the ratio is taken in BigInt at a fixed scale, so a uint64-scale
 * threshold keeps its meaning instead of being rounded away. Only the bounded
 * result becomes a number.
 */
export function bigFraction(numerator: bigint, denominator: bigint): number {
  if (denominator <= 0n || numerator <= 0n) return 0;
  if (numerator <= MAX_EXACT && denominator <= MAX_EXACT) return Number(numerator) / Number(denominator);
  return Number((numerator * FRACTION_SCALE) / denominator) / Number(FRACTION_SCALE);
}

/** Ceiling of `a / b` for non-negative integers; one for an empty numerator or denominator. */
function ceilScale(a: bigint, b: bigint): bigint {
  if (a <= 0n || b <= 0n) return 1n;
  return (a + b - 1n) / b;
}

/**
 * Positions (0 to 1) of the window boundaries on a gauge that spans `scale`
 * windows: every boundary when there are few, every k-th otherwise, so at
 * most MAX_GAUGE_MARKS are drawn however large the scale.
 */
export function gaugeMarks(scale: number, max = MAX_GAUGE_MARKS): number[] {
  if (!Number.isFinite(scale) || scale <= 1) return [];
  const every = Math.ceil(scale / (max + 1));
  const out: number[] = [];
  for (let m = every; m < scale; m += every) out.push(m / scale);
  return out;
}

/** What one constraint's backlog gauge spans, and where the backlog sits on it. */
export type ConstraintGauge = { scale: number; fraction: number; marks: number[]; denominator: number };

/**
 * What a constraint's backlog gauge spans: `scale` windows of target, marks at
 * each window boundary. The window of target is the pricer's own divisor,
 * `SaturatingUMul(window, target)` cast into bips, so a uint64-scale parameter
 * that saturates in nitro saturates here too and the gauge cannot promise a
 * free region the pricer does not give.
 */
export function constraintGauge(c: { target: number; window: number }, backlog: number): ConstraintGauge {
  const denominator = saturatingCastToBips(saturatingUMul(toUint64(c.window), toUint64(c.target)));
  if (denominator <= 0n) return { scale: 1, fraction: 0, marks: [], denominator: 0 };
  const gas = toUint64(backlog);
  const scale = ceilScale(gas, denominator);
  return { scale: Number(scale), fraction: bigFraction(gas, denominator * scale), marks: gaugeMarks(Number(scale)), denominator: Number(denominator) };
}

/**
 * The right-hand end of a backlog gauge, in the reader's terms rather than in
 * the model's: how many windows of target the bar spans, and the gas that
 * comes to. "4 windows of target (13.8 Tgas)" says what the far end is
 * without asking anyone to multiply two parameters together first.
 */
export function gaugeSpanLabel(count: number, unitGas: number, one: string, many: string): string {
  return `${formatInteger(count)} ${count === 1 ? one : many} (${formatGas(count * unitGas)})`;
}

/** The right-hand end of a constraint's gauge: whole windows of target. */
export function constraintGaugeSpanLabel(gauge: Pick<ConstraintGauge, "scale" | "denominator">): string {
  return gaugeSpanLabel(gauge.scale, gauge.denominator, "window of target", "windows of target");
}

/**
 * What one window is, on the gauge itself: the pricer's own divisor and what
 * a full one does to x, so the scale is readable without the equation.
 */
export function constraintGaugeTitle(denominator: number): string {
  return `one window = target × window = ${formatGas(denominator)}; each full window adds 1.0 to x`;
}

/**
 * The right-hand end of the legacy gauge: whole tolerance thresholds where
 * the pricer has a free region, whole units of x where it has none.
 */
export function legacyGaugeSpanLabel(gauge: LegacyGauge): string {
  if (gauge.free > 0) return gaugeSpanLabel(Math.round(gauge.span / gauge.free), gauge.free, "tolerance threshold", "tolerance thresholds");
  if (gauge.unit > 0) return gaugeSpanLabel(Math.round(gauge.span / gauge.unit), gauge.unit, "unit of x", "units of x");
  return "no scale (zero inertia or speed limit)";
}

/** What one step of the legacy gauge is, and what it does to the fee. */
export function legacyGaugeTitle(gauge: LegacyGauge): string {
  if (gauge.free > 0) return `one threshold = tolerance × speed limit = ${formatGas(gauge.free)}; below it the pricer charges nothing at all`;
  if (gauge.unit > 0) return `one unit = inertia × speed limit = ${formatGas(gauge.unit)}; each full unit adds 1.0 to x`;
  return "no scale: the inertia or the speed limit is zero";
}

export type LegacyGauge = {
  /** Gas that is free before the pricer reacts: tolerance times the speed limit. */
  free: number;
  /** Gas per whole unit of x: inertia times the speed limit. */
  unit: number;
  /** Right edge of the gauge in gas; zero when nothing can be drawn. */
  span: number;
  fraction: number;
  marks: number[];
};

/**
 * The legacy gauge. Both thresholds are the pricer's own: the free region is
 * the tolerance threshold `tolerance * speedLimit` as a plain uint64 multiply
 * that wraps exactly as nitro's does (a wrapped threshold of zero therefore
 * shows no free region, as the pricer charges from the first unit of gas),
 * and the unit of x is the saturating `inertia * speedLimit` cast into bips.
 * With a free region the span is a whole number of thresholds (at least two,
 * so the threshold sits inside the bar) and the single mark is the threshold.
 * Without one the span is whole units of x with a mark at each, like a
 * constraint gauge. Zero inertia or speed limit gives an empty gauge rather
 * than NaN.
 */
export function legacyGauge(legacy: { speedLimit: number; inertia: number; tolerance: number }, backlog: number): LegacyGauge {
  const speedLimit = toUint64(legacy.speedLimit);
  const free = BigInt.asUintN(64, toUint64(legacy.tolerance) * speedLimit);
  const unit = saturatingCastToBips(saturatingUMul(toUint64(legacy.inertia), speedLimit));
  const gas = toUint64(backlog);
  if (free > 0n) {
    const scale = ceilScale(gas, free) + (gas > 0n ? 1n : 0n);
    const bounded = scale < 2n ? 2n : scale;
    const span = free * bounded;
    return { free: Number(free), unit: Number(unit), span: Number(span), fraction: bigFraction(gas, span), marks: [1 / Number(bounded)] };
  }
  if (unit > 0n) {
    const scale = ceilScale(gas, unit);
    const span = unit * scale;
    return { free: 0, unit: Number(unit), span: Number(span), fraction: bigFraction(gas, span), marks: gaugeMarks(Number(scale)) };
  }
  return { free: 0, unit: 0, span: 0, fraction: 0, marks: [] };
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

/** Sum of a nullable wei-string field over the points that carry it, as ETH, and how many points were left out for lacking it. */
export function sumKnownWeiEth<K extends string>(points: readonly Record<K, string | null>[], key: K): { eth: number; unknown: number } {
  let total = 0n;
  let unknown = 0;
  for (const p of points) {
    const value = p[key];
    if (value === null) unknown += 1;
    else total += BigInt(value);
  }
  return { eth: weiToEthNumber(total), unknown };
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
