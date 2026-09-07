// Where a range has nothing indexed. The api serves only the buckets it has, so a chart drawn over the
// extent of its data made two hours of history read as a full day. Every range chart draws the window
// that was asked for instead, breaks its lines where a bucket is missing and shades what was never
// indexed. These are the pure parts of that.

import type { SeriesResolution } from "@/types";
import { formatDateTime, formatInteger } from "@/utils/format";

/** Which end of the window a span with no data sits at: history from before indexing began, a hole
 * between two points, or the stretch up to the end of the window. */
export type GapKind = "leading" | "interior" | "trailing";

/** A span of the time axis with no data in it, in unix seconds. */
export type Gap = { from: number; to: number; kind: GapKind };

/** The window a range covers, in unix seconds: what the x axis spans. */
export type GapWindow = { from: number; to: number };

/** A stretch of a per-block range with no block for longer than this is a gap, not a quiet moment:
 * Robinhood produces about ten blocks a second. */
export const BLOCK_GAP_S = 30;

/** Fallback spacing when neither the resolution nor the points say what one is. */
export const DEFAULT_STEP_SECONDS = 60;

/** The spacing points of each resolution arrive at. A per-block range carries one point per block, so
 * it is judged by the block rule instead of by a bucket width. */
const STEP_SECONDS: Record<SeriesResolution, number> = { block: BLOCK_GAP_S, "5s": 5, "1m": 60, "15m": 900, "1h": 3600 };

/**
 * The width of one bucket at each resolution, a different question from how far apart two points may be
 * before the space is a gap: a per-block range's bucket is one second while its threshold is BLOCK_GAP_S.
 */
const BUCKET_SECONDS: Record<SeriesResolution, number> = { block: 1, "5s": 5, "1m": 60, "15m": 900, "1h": 3600 };

function known(table: Record<SeriesResolution, number>, resolution: string | undefined): number | null {
  if (resolution === undefined) return null;
  const step: number | undefined = table[resolution as SeriesResolution];
  return step === undefined ? null : step;
}

function knownStep(resolution: string | undefined): number | null {
  return known(STEP_SECONDS, resolution);
}

/**
 * The width of one bucket of a range: the resolution's own, never inferred from the spacing of the first
 * two points, which made zero-width buckets from points sharing a timestamp and inflated the bucket to
 * the size of a hole. Only an unknown resolution falls back to the points.
 */
export function bucketSeconds(resolution: string | undefined, points: readonly { t: number }[] = []): number {
  const width = known(BUCKET_SECONDS, resolution);
  if (width !== null) return width;
  let smallest = Number.POSITIVE_INFINITY;
  for (let i = 1; i < points.length; i++) {
    const delta = points[i].t - points[i - 1].t;
    if (delta > 0 && delta < smallest) smallest = delta;
  }
  return Number.isFinite(smallest) ? smallest : DEFAULT_STEP_SECONDS;
}

/** How far apart two points may be before the space between them is a gap: the resolution's own spacing,
 * else the smallest the points show, else a minute. */
export function stepSeconds(resolution: string | undefined, points: readonly { t: number }[] = []): number {
  const known = knownStep(resolution);
  if (known !== null) return known;
  let smallest = Number.POSITIVE_INFINITY;
  for (let i = 1; i < points.length; i++) {
    const delta = points[i].t - points[i - 1].t;
    if (delta > 0 && delta < smallest) smallest = delta;
  }
  return Number.isFinite(smallest) ? smallest : DEFAULT_STEP_SECONDS;
}

/**
 * A tolerant step for points that arrive on their own cadence rather than on a grid: batch posting
 * reports land every twelve to twenty-four seconds, so the bucket width is the wrong threshold. A
 * multiple of the median spacing instead, never less than the bucket.
 */
export function irregularStep(points: readonly { t: number }[], bucketSeconds: number, factor = 3): number {
  const deltas: number[] = [];
  for (let i = 1; i < points.length; i++) {
    const delta = points[i].t - points[i - 1].t;
    if (delta > 0) deltas.push(delta);
  }
  if (deltas.length === 0) return Math.max(bucketSeconds, 0);
  deltas.sort((a, b) => a - b);
  const median = deltas[Math.floor(deltas.length / 2)];
  return Math.max(bucketSeconds, median * factor);
}

/** The window the axis spans: the one the api says it answered, or the extent of the data when it
 * carries none. The end is never before the start. */
export function windowOf(range: { from?: number; to?: number }, points: readonly { t: number }[], step: number): GapWindow {
  const first = points.length > 0 ? points[0].t : 0;
  const last = points.length > 0 ? points[points.length - 1].t + Math.max(0, step) : first;
  const from = Number.isFinite(range.from) ? Number(range.from) : first;
  const to = Number.isFinite(range.to) ? Number(range.to) : last;
  return { from, to: Math.max(from, to) };
}

/**
 * The spans of `window` that carry no data. Points arrive sorted, oldest first, and `step` is the spacing
 * they are expected at: two points further apart than one step have a hole starting one step after the
 * earlier one. A window with no points is one leading gap. Every span is clamped to the window, so a
 * point outside the range the api answered can never shade axis the chart does not draw.
 */
export function findGaps(points: readonly { t: number }[], window: GapWindow, step: number): Gap[] {
  const { from, to } = window;
  if (!Number.isFinite(step) || step <= 0 || !(to > from)) return [];
  if (points.length === 0) return [{ from, to, kind: "leading" }];
  const out: Gap[] = [];
  const clamp = (start: number, end: number, kind: GapKind) => {
    const lo = Math.max(from, start);
    const hi = Math.min(to, end);
    if (hi > lo) out.push({ from: lo, to: hi, kind });
  };
  const first = points[0].t;
  const last = points[points.length - 1].t;
  // A window is not aligned to the bucket grid, so a first point less than one step after the start is
  // the grid and not a gap.
  if (first - from >= step) clamp(from, first, "leading");
  for (let i = 1; i < points.length; i++) {
    const before = points[i - 1].t;
    const after = points[i].t;
    if (after - before > step) clamp(before + step, after, "interior");
  }
  if (to - last > step) clamp(last + step, to, "trailing");
  return out;
}

/** The time the first indexed point sits at, or null when the range indexed nothing. */
export function firstIndexed(points: readonly { t: number }[]): number | null {
  return points.length > 0 ? points[0].t : null;
}

/** Marks a row that exists only to break a line across a gap. */
export type GapRow = { t: number; gapRow: true; [key: string]: number | boolean | null };

/** True for a chart row that stands for a gap rather than for data. */
export function isGapRow(row: Record<string, unknown>): boolean {
  return row.gapRow === true;
}

function emptyRow(keys: ReadonlySet<string>, t: number): GapRow {
  const row: GapRow = { t, gapRow: true };
  for (const key of keys) {
    if (key !== "t") row[key] = null;
  }
  return row;
}

/**
 * `rows` with one empty row inside every interior gap, so a line breaks there (with connectNulls={false})
 * rather than bridging a hole that never happened. Every key is null on it and `gapRow` marks it, so no
 * series reads a zero. Leading and trailing gaps need no row.
 */
export function withGapBreaks<T extends { t: number }>(rows: readonly T[], gaps: readonly Gap[]): (T | GapRow)[] {
  const interior = gaps.filter((g) => g.kind === "interior").sort((a, b) => a.from - b.from);
  if (interior.length === 0 || rows.length === 0) return [...rows];
  const keys = new Set<string>();
  for (const row of rows) for (const key of Object.keys(row)) keys.add(key);
  const out: (T | GapRow)[] = [];
  let next = 0;
  for (const row of rows) {
    while (next < interior.length && interior[next].from <= row.t) {
      out.push(emptyRow(keys, interior[next].from));
      next += 1;
    }
    out.push(row);
  }
  return out;
}

/** What the leading (and trailing) shading says on the chart. */
export const NOT_INDEXED_LABEL = "not indexed yet";

/** What an interior hole says on the chart. */
export const GAP_LABEL = "gap";

/** The word a shaded span wears. */
export function gapBandLabel(gap: Gap): string {
  return gap.kind === "interior" ? GAP_LABEL : NOT_INDEXED_LABEL;
}

/** The line under a chart that has shaded spans: what the shading means, and where the indexed history
 * begins. Null when there is nothing shaded to explain. */
export function gapCaption(gaps: readonly Gap[], first: number | null): string | null {
  if (gaps.length === 0) return null;
  const interior = gaps.filter((g) => g.kind === "interior").length;
  const parts: string[] = [];
  if (gaps.some((g) => g.kind === "leading")) {
    parts.push(first === null ? NOT_INDEXED_LABEL : `${NOT_INDEXED_LABEL}, history before ${formatDateTime(first)}`);
  }
  if (interior > 0) parts.push(`${formatInteger(interior)} ${interior === 1 ? "gap with no buckets" : "gaps with no buckets"}`);
  if (gaps.some((g) => g.kind === "trailing")) parts.push(`${NOT_INDEXED_LABEL}, up to the end of the range`);
  return `Shaded: ${parts.join(" · ")}`;
}

/** What a range with nothing in it says, in the same box the chart would fill. */
export const NOTHING_INDEXED = "Nothing indexed for this range yet.";

/** That, and when indexing began where the api says so. */
export function emptyRangeNote(began: number | null): string {
  return began === null ? NOTHING_INDEXED : `${NOTHING_INDEXED} Indexing began ${formatDateTime(began)}.`;
}

/** Everything a range chart needs to draw its window honestly, derived once. */
export type GapModel = {
  window: GapWindow;
  gaps: Gap[];
  step: number;
  /** The first indexed point in the range, or null when it holds none. */
  first: number | null;
  /** True when the range has no points at all: the chart draws its empty state instead. */
  empty: boolean;
};

/** The window, the step and the gaps of a range the api answered. */
export function gapModel(range: { from?: number; to?: number; resolution?: string }, points: readonly { t: number }[], step = stepSeconds(range.resolution, points)): GapModel {
  const window = windowOf(range, points, step);
  return { window, gaps: findGaps(points, window, step), step, first: firstIndexed(points), empty: points.length === 0 };
}

/** The empty model, for a chart with no series at all: no window, nothing shaded. */
export const NO_GAPS: GapModel = { window: { from: 0, to: 0 }, gaps: [], step: DEFAULT_STEP_SECONDS, first: null, empty: true };
