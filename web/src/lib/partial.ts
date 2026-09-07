// Buckets whose aggregates are not known to be whole: partial at a time boundary, partial because
// blocks are missing inside, or unknown when a missing range cannot be placed. The sums are never
// presented as full bucket totals for any of those states.

import type { SeriesCompleteness } from "@/types";
import { formatPercent } from "@/utils/format";

/** What every point carries. Optional on purpose: an older api sends nothing, and a bucket nobody said
 * anything about is a whole bucket. */
export type Covered = { coverage?: number | null; completeness?: SeriesCompleteness };

export const WHOLE = 1;

/** Which kind of incomplete bucket a point is: "in-progress" at the right edge, "leading" elsewhere,
 * "unknown" when the bounds cannot decide. */
export type PartialKind = "in-progress" | "leading" | "unknown";

/** The share of the bucket a point covers, 0 to 1 when known. Null is preserved because a block-number
 * gap may have no exact timestamp share. */
export function coverageOf(point: Covered): number | null {
  const raw = point.coverage;
  if (raw === null) return null;
  if (raw === undefined || !Number.isFinite(raw)) return WHOLE;
  return Math.min(WHOLE, Math.max(0, raw));
}

export function completenessOf(point: Covered): SeriesCompleteness {
  if (point.completeness === "complete" || point.completeness === "partial" || point.completeness === "unknown") return point.completeness;
  const coverage = coverageOf(point);
  return coverage === null ? "unknown" : coverage < WHOLE ? "partial" : "complete";
}

export function isPartial(point: Covered): boolean {
  return completenessOf(point) !== "complete";
}

/** The right edge of the range, and one bucket's width: a partial bucket is only in progress when its own
 * bucket reaches that edge. */
export type RangeEdge = { to: number; step: number };

/**
 * True when the bucket reaches the range edge and its coverage matches elapsed time there. The API
 * window ends one second after its serving clock, so one bucket-second of tolerance keeps that from
 * misclassifying the live bucket. A bounded internal hole has lower coverage and stays generic.
 */
function isInProgress(point: Covered, t: number, edge: RangeEdge): boolean {
  if (t + edge.step < edge.to) return false;
  const coverage = coverageOf(point);
  if (coverage === null) return false;
  const elapsed = Math.min(WHOLE, Math.max(0, (edge.to - t) / edge.step));
  return Math.abs(coverage - elapsed) <= 1 / edge.step + Number.EPSILON;
}

/**
 * One entry per point: which kind of partial bucket it is, or null for a whole one. A partial bucket
 * is the one in progress only when it runs up to the right edge; anywhere else it is one the collector
 * reached part way into. With no window, the last point is taken as the one in progress.
 */
export function partialKinds(points: readonly (Covered & { t?: number })[], edge?: RangeEdge): (PartialKind | null)[] {
  const usable = edge !== undefined && Number.isFinite(edge.to) && Number.isFinite(edge.step) && edge.step > 0 ? edge : null;
  return points.map((p, i) => {
    const completeness = completenessOf(p);
    if (completeness === "complete") return null;
    if (completeness === "unknown") return "unknown";
    if (usable === null || typeof p.t !== "number") return i === points.length - 1 ? "in-progress" : "leading";
    return isInProgress(p, p.t, usable) ? "in-progress" : "leading";
  });
}

export const IN_PROGRESS_LABEL = "in progress";

export const PARTLY_INDEXED_LABEL = "partly indexed";

export const UNKNOWN_COVERAGE_LABEL = "coverage unknown";

export function partialBandLabel(kind: PartialKind): string {
  if (kind === "in-progress") return IN_PROGRESS_LABEL;
  return kind === "unknown" ? UNKNOWN_COVERAGE_LABEL : PARTLY_INDEXED_LABEL;
}

/** What a tooltip says about a partial bucket: "bucket in progress, 33% elapsed", or "partially indexed". */
export function partialNote(coverage: number | null, kind: PartialKind): string {
  if (kind === "unknown") return "bucket completeness unknown";
  if (coverage === null || !Number.isFinite(coverage)) return kind === "in-progress" ? "bucket in progress, coverage unknown" : "partially indexed, coverage unknown";
  if (kind === "leading" && coverage >= WHOLE) return "partially indexed, missing blocks share a timestamp";
  const share = formatPercent(Math.min(WHOLE, Math.max(0, coverage)), 0);
  return kind === "in-progress" ? `bucket in progress, ${share} elapsed` : `partially indexed, ${share} of the bucket`;
}

/** The same for a chart that draws sums: the bucket is not drawn at all, and the tooltip says so. */
export function partialSumNote(coverage: number | null, kind: PartialKind): string {
  return `${partialNote(coverage, kind)}; not drawn as a bucket total`;
}

export type PartialRow = { partial: PartialKind | null; coverage: number | null };

export function isPartialRow(row: Record<string, unknown>): boolean {
  return row.partial === "in-progress" || row.partial === "leading" || row.partial === "unknown";
}

/** The tooltip footnote a hovered row earns for being partial, or null for a whole bucket. `sums` is true
 * for a chart that draws sums per bucket, which leaves the bucket out rather than drawing it short. */
export function partialRowNote(row: Record<string, unknown>, sums = false): string | null {
  if (!isPartialRow(row)) return null;
  const kind: PartialKind = row.partial === "in-progress" ? "in-progress" : row.partial === "unknown" ? "unknown" : "leading";
  const coverage = typeof row.coverage === "number" ? row.coverage : row.coverage === null ? null : WHOLE;
  return sums ? partialSumNote(coverage, kind) : partialNote(coverage, kind);
}

export type PartialBand = { from: number; to: number; kind: PartialKind; coverage: number | null };

/** The bands over the partial buckets of a range, one bucket wide each, so a chart that leaves them out of
 * its marks still shows where they are. A `step` that is not positive leaves them out, and `to` decides
 * which partial bucket is the one in progress. */
export function partialBands(points: readonly (Covered & { t: number })[], step: number, to?: number): PartialBand[] {
  if (!Number.isFinite(step) || step <= 0) return [];
  const kinds = partialKinds(points, to === undefined ? undefined : { to, step });
  const out: PartialBand[] = [];
  points.forEach((p, i) => {
    const kind = kinds[i];
    if (kind !== null) out.push({ from: p.t, to: p.t + step, kind, coverage: coverageOf(p) });
  });
  return out;
}

/** The line under a chart that hatches partial buckets. Null when nothing is hatched. */
export function partialCaption(bands: readonly PartialBand[]): string | null {
  if (bands.length === 0) return null;
  return `Hatched and left out: ${bands.map((b) => partialNote(b.coverage, b.kind)).join(" · ")}`;
}

/** The fee destination stack as a chart draws it. The keys are separate from the bucket's own fee fields,
 * so a bucket in progress can be left out while its tooltip still reads out what it has. */
export type FeeStack = { stackFloorEth: number | null; stackSurplusEth: number | null; stackPosterEth: number | null; stackUnsplitEth: number | null };

export type FeeSums = PartialRow & { floorFeesEth: number | null; surplusFeesEth: number | null; posterFeesEth?: number | null; unsplitFeesEth: number | null };

/** `rows` with the stack keys added: the bucket's own parts on a whole bucket, nothing on a partial one, so
 * a stacked area with connectNulls={false} ends at the last whole bucket. */
export function withFeeStack<T extends FeeSums>(rows: readonly T[]): (T & FeeStack)[] {
  return rows.map((row) =>
    row.partial === null
      ? { ...row, stackFloorEth: row.floorFeesEth, stackSurplusEth: row.surplusFeesEth, stackPosterEth: row.posterFeesEth ?? null, stackUnsplitEth: row.unsplitFeesEth }
      : { ...row, stackFloorEth: null, stackSurplusEth: null, stackPosterEth: null, stackUnsplitEth: null },
  );
}
