// The base fee chart over a bucketed range: the rows it draws, its log
// domain, the owner-action markers on it and the rows its tooltip reads out.
// The hero draws this chart for every range but Live, and the history section
// draws the other charts from the same rows, so the mapping lives here rather
// than in either component.

import type { OwnerAction, PricerModel, Series } from "@/types";
import type { TooltipRow } from "@/components/ChartTooltip";
import { gapModel, withGapBreaks, NO_GAPS, type GapModel, type GapRow } from "@/lib/gaps";
import { buildChartPoints, FLOOR_COLOR, logDomain, shortConstraintLabel, spanSeconds, withSetBoundaries, type ChartPoint } from "@/utils/chart";
import { formatGwei, formatInteger, formatSignificant } from "@/utils/format";

/** An owner action placed on the time axis: the second it landed, and the short name of the call. */
export type Marker = { t: number; action: OwnerAction; label: string };

/** Fallback bucket width when a range has fewer than two buckets to measure one from. */
export const DEFAULT_BUCKET_SECONDS = 60;

/** Every owner action in the range as a marker on the time axis. */
export function markersFor(series: Pick<Series, "ownerActions">): Marker[] {
  return series.ownerActions.map((a) => ({ t: Math.floor(new Date(a.at).getTime() / 1000), action: a, label: a.method }));
}

/** Owner actions that fall inside the bucket that starts at `t`. */
export function actionsInBucket(markers: readonly Marker[], t: number, bucketSeconds: number): Marker[] {
  return markers.filter((m) => m.t >= t && m.t < t + bucketSeconds);
}

/** What an owner action did, in one line: the constraint set it installed, the floor it set, or just its name. */
export function describeAction(a: OwnerAction): string {
  if (a.method === "setGasPricingConstraints") {
    const raw = a.args.constraints;
    if (Array.isArray(raw)) {
      const parts = raw.map((c) => (Array.isArray(c) && c.length >= 2 ? shortConstraintLabel({ target: Number(c[0]), window: Number(c[1]) }) : String(c)));
      return `setGasPricingConstraints: ${parts.join(", ")}`;
    }
  }
  if (a.method === "setMinimumL2BaseFee" && typeof a.args.priceInWei === "string") {
    return `setMinimumL2BaseFee: ${formatGwei(a.args.priceInWei)} gwei`;
  }
  return a.method;
}

/**
 * The tooltip footnote for a hovered bucket: every owner action that landed
 * inside it, or null when none did.
 */
export function ownerActionNote(markers: readonly Marker[], bucketSeconds: number): (row: Record<string, unknown>) => string | null {
  return (row) => {
    const hits = actionsInBucket(markers, Number(row.t), bucketSeconds);
    return hits.length > 0 ? hits.map((h) => `Owner action at block ${formatInteger(h.action.block)}: ${describeAction(h.action)}`).join(" · ") : null;
  };
}

/** The log y domain of the fee chart: the band, the average and the floor in force all fit inside it. */
export function feeDomain(points: readonly ChartPoint[]): [number, number] {
  return logDomain(points.flatMap((p) => [p.feeMin, p.feeMax, p.floor]));
}

/** What a hovered bucket says about the fee: the average, the band, the floor under it, x and how many blocks it holds. */
export function feeTooltipRows(): TooltipRow[] {
  return [
    { label: "base fee, average", color: "var(--series-1)", value: (r) => `${formatSignificant(Number(r.feeAvg), 4)} gwei` },
    { label: "min to max in bucket", value: (r) => `${formatSignificant(Number(r.feeMin), 3)} to ${formatSignificant(Number(r.feeMax), 3)} gwei` },
    { label: "floor in force", color: FLOOR_COLOR, value: (r) => `${formatSignificant(Number(r.floor), 3)} gwei` },
    { label: "x", value: (r) => Number(r.x).toFixed(4) },
    { label: "blocks", value: (r) => formatInteger(Number(r.blocks)) },
  ];
}

/** The line under the chart header for a bucketed range: what the chart draws, and over how much. */
export function feeChartCaption(rangeLabel: string, points: readonly ChartPoint[]): string {
  return `Base fee average with the min to max band · ${rangeLabel} · ${formatInteger(points.length)} buckets`;
}

/** The accessible description of a bucketed fee chart: the range it covers and the band of fees in it. */
export function feeChartLabel(rangeLabel: string, points: readonly ChartPoint[]): string {
  if (points.length === 0) return `Base fee over ${rangeLabel}, no buckets yet`;
  const min = Math.min(...points.map((p) => p.feeMin));
  const max = Math.max(...points.map((p) => p.feeMax));
  return `Base fee over ${rangeLabel} on a log scale, ${formatInteger(points.length)} buckets, ${formatSignificant(min, 3)} to ${formatSignificant(max, 3)} gwei, with the floor in force stepped under it`;
}

/**
 * Everything a bucketed range is drawn from. `points` is one row per bucket
 * (the table and the inspector read those); `drawn` duplicates each bucket
 * where the set in force changes, so a replacement is a vertical edge rather
 * than a slope across the bucket.
 */
/** A row the charts draw: a bucket, or the empty row that breaks a line across a gap. */
export type DrawnRow = ChartPoint | GapRow;

export type FeeChartData = {
  points: ChartPoint[];
  drawn: DrawnRow[];
  markers: Marker[];
  domain: [number, number];
  span: number;
  bucketSeconds: number;
  /** The window the range asked for, the spans of it with nothing in them, and the step they were judged at. */
  gaps: GapModel;
};

/** The rows, markers, domain and axis span of a range. An absent series draws nothing at all. */
export function feeChartData(series: Series | null, model: PricerModel): FeeChartData {
  if (!series) return { points: [], drawn: [], markers: [], domain: logDomain([]), span: 0, bucketSeconds: DEFAULT_BUCKET_SECONDS, gaps: NO_GAPS };
  const points = buildChartPoints(series, model);
  const gaps = gapModel(series, points);
  return {
    points,
    // Empty rows inside the holes, so a missing bucket breaks the line rather
    // than being bridged by a segment that stands for nothing.
    drawn: withGapBreaks(withSetBoundaries(points), gaps.gaps),
    markers: markersFor(series),
    domain: feeDomain(points),
    // The axis spans the window, so its ticks are formatted for the range that
    // was asked for and not for the part of it that happens to hold buckets.
    span: gaps.window.to > gaps.window.from ? gaps.window.to - gaps.window.from : spanSeconds(points),
    bucketSeconds: points.length > 1 ? points[1].t - points[0].t : DEFAULT_BUCKET_SECONDS,
    gaps,
  };
}
