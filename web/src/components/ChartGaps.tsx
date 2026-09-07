"use client";

import { ReferenceArea } from "recharts";
import { gapBandLabel, gapCaption, type Gap, type GapModel, type GapWindow } from "@/lib/gaps";
import { missingBandLabel, missingCaption, type MissingRun } from "@/lib/missing";
import { partialBandLabel, partialCaption, type PartialBand } from "@/lib/partial";
import { HatchPattern } from "./primitives";

/** Shading for a span with nothing in it: the muted ink at a tenth. It is a background and not a mark,
 * so it carries no colour of its own. */
export const GAP_FILL = "var(--ink-3)";
export const GAP_FILL_OPACITY = 0.1;

/** A band narrower than this share of the axis carries no word: it would collide with whatever sits
 * beside it, and the caption says the same thing in full. */
export const GAP_LABEL_MIN_SHARE = 0.09;

/**
 * The shaded spans of a chart's window, as reference areas. Returned as an array rather than wrapped in a
 * component because recharts reads its children by type. Call it before the series so they sit on top.
 */
export function gapBands(gaps: readonly Gap[], window: GapWindow) {
  const span = window.to - window.from;
  return gaps.map((gap) => {
    const wide = span > 0 && (gap.to - gap.from) / span >= GAP_LABEL_MIN_SHARE;
    return (
      <ReferenceArea
        key={`${gap.kind}-${gap.from}`}
        x1={gap.from}
        x2={gap.to}
        fill={GAP_FILL}
        fillOpacity={GAP_FILL_OPACITY}
        label={wide ? { value: gapBandLabel(gap), position: "insideTop", fill: "var(--ink-2)", fontSize: 10 } : undefined}
      />
    );
  });
}

/** The line under a chart with shaded spans. Nothing at all when none are shaded. */
export function GapNote({ gaps, className = "" }: { gaps: GapModel; className?: string }) {
  const text = gapCaption(gaps.gaps, gaps.first);
  if (text === null) return null;
  return <p className={`mt-1 text-[11px] text-ink-3 ${className}`}>{text}</p>;
}

/**
 * The hatch a bucket the collector has not finished stands under: the same recessive ink as the gap
 * shading, but a hatch, because a hole in the record and a bucket still filling must not read the same.
 */
export const PARTIAL_FILL = "var(--ink-3)";
export const PARTIAL_FILL_OPACITY = 0.4;
export const PARTIAL_PATTERN_ID = "partial-bucket-hatch";

/** The pattern the partial bands are filled from. Put it in the chart's own `defs`. */
export function PartialHatch() {
  return <HatchPattern id={PARTIAL_PATTERN_ID} color={PARTIAL_FILL} />;
}

/** The buckets a sum chart leaves out, as reference areas one bucket wide. An array for the same reason
 * `gapBands` is. */
export function partialBandAreas(bands: readonly PartialBand[], window: GapWindow) {
  const span = window.to - window.from;
  return bands.map((band) => {
    const wide = span > 0 && (band.to - band.from) / span >= GAP_LABEL_MIN_SHARE;
    return (
      <ReferenceArea
        key={`partial-${band.from}`}
        x1={band.from}
        x2={band.to}
        fill={`url(#${PARTIAL_PATTERN_ID})`}
        fillOpacity={PARTIAL_FILL_OPACITY}
        label={wide ? { value: partialBandLabel(band.kind), position: "insideTop", fill: "var(--ink-2)", fontSize: 10 } : undefined}
      />
    );
  });
}

/** The line under a chart that hatches partial buckets. */
export function PartialNote({ bands, className = "" }: { bands: readonly PartialBand[]; className?: string }) {
  const text = partialCaption(bands);
  if (text === null) return null;
  return <p className={`mt-1 text-[11px] text-ink-3 ${className}`}>{text}</p>;
}

/**
 * The dots a series with no input at all stands under: the same recessive ink again, but a stipple, since
 * an unindexed stretch, a bucket still filling and a series with nothing to draw are three facts.
 */
export const MISSING_FILL = "var(--ink-3)";
export const MISSING_FILL_OPACITY = 0.55;
export const MISSING_PATTERN_ID = "missing-series-dots";

/** The stipple geometry, tight enough that a band a few pixels wide still catches a column of dots. */
export const DOT_SPACING = 5;
export const DOT_RADIUS = 1;

/**
 * The dashed edge every band wears. A run of a handful of buckets is too narrow for a fill to read as a
 * texture, so the mark cannot rest on the stipple alone; neither of the other two marks is outlined.
 */
export const MISSING_STROKE_OPACITY = 0.5;
export const MISSING_DASH = "2 2";

/**
 * The pattern the missing-series bands are filled from. Put it in the chart's own `defs`, and only where a
 * band is drawn: every copy under the same id is the same definition, so `url(#id)` paints the same dots.
 */
export function MissingDots() {
  return (
    <pattern id={MISSING_PATTERN_ID} width={DOT_SPACING} height={DOT_SPACING} patternUnits="userSpaceOnUse">
      <circle cx={DOT_SPACING / 2} cy={DOT_SPACING / 2} r={DOT_RADIUS} fill={MISSING_FILL} />
    </pattern>
  );
}

/**
 * The runs a series has no value over, as reference areas. An array for the same reason `gapBands` is.
 * Call it before the series in the chart's children and the bands sit under them.
 */
export function missingBandAreas(runs: readonly MissingRun[], window: GapWindow) {
  const span = window.to - window.from;
  return runs.map((run) => {
    const wide = span > 0 && (run.to - run.from) / span >= GAP_LABEL_MIN_SHARE;
    return (
      <ReferenceArea
        key={`missing-${run.kind}-${run.from}`}
        x1={run.from}
        x2={run.to}
        fill={`url(#${MISSING_PATTERN_ID})`}
        fillOpacity={MISSING_FILL_OPACITY}
        stroke={MISSING_FILL}
        strokeOpacity={MISSING_STROKE_OPACITY}
        strokeDasharray={MISSING_DASH}
        label={wide ? { value: missingBandLabel(run.kind), position: "insideTop", fill: "var(--ink-2)", fontSize: 10 } : undefined}
      />
    );
  });
}

/** The line under a chart that dots the runs a series had nothing to draw from. */
export function MissingNote({ runs, className = "" }: { runs: readonly MissingRun[]; className?: string }) {
  const text = missingCaption(runs);
  if (text === null) return null;
  return <p className={`mt-1 text-[11px] text-ink-3 ${className}`}>{text}</p>;
}
