"use client";

import { ReferenceArea } from "recharts";
import { gapBandLabel, gapCaption, type Gap, type GapModel, type GapWindow } from "@/lib/gaps";
import { missingBandLabel, missingCaption, type MissingRun } from "@/lib/missing";
import { partialBandLabel, partialCaption, type PartialBand } from "@/lib/partial";
import { HatchPattern } from "./primitives";

/**
 * Shading for a span with nothing in it: the muted ink at a tenth, a
 * recessive tint that sits under every series rather than competing with
 * one. It is a background and not a mark, so it carries no colour of its own.
 */
export const GAP_FILL = "var(--ink-3)";
export const GAP_FILL_OPACITY = 0.1;

/**
 * A band narrower than this share of the axis carries no word: "not indexed
 * yet" over a sliver would collide with whatever sits beside it, and the
 * caption under the chart says the same thing in full.
 */
export const GAP_LABEL_MIN_SHARE = 0.09;

/**
 * The shaded spans of a chart's window, as reference areas. Returned as an
 * array rather than wrapped in a component because recharts reads its
 * children by type, so a wrapper would never be drawn. Call it before the
 * series in the chart's children and the bands sit under them.
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

/** The line under a chart with shaded spans, saying what they are and where the history starts. Nothing at all when none are shaded. */
export function GapNote({ gaps, className = "" }: { gaps: GapModel; className?: string }) {
  const text = gapCaption(gaps.gaps, gaps.first);
  if (text === null) return null;
  return <p className={`mt-1 text-[11px] text-ink-3 ${className}`}>{text}</p>;
}

/**
 * The hatch a bucket the collector has not finished stands under. The same
 * recessive ink the gap shading uses, so it is background and not a mark, but
 * a hatch rather than a flat tint: a hole in the record and a bucket that is
 * still filling are not the same thing and must not read the same.
 */
export const PARTIAL_FILL = "var(--ink-3)";
export const PARTIAL_FILL_OPACITY = 0.4;
export const PARTIAL_PATTERN_ID = "partial-bucket-hatch";

/** The pattern the partial bands are filled from. Put it in the chart's own `defs`. */
export function PartialHatch() {
  return <HatchPattern id={PARTIAL_PATTERN_ID} color={PARTIAL_FILL} />;
}

/**
 * The buckets a sum chart leaves out, as reference areas one bucket wide.
 * Returned as an array for the same reason `gapBands` is: recharts reads its
 * children by type, so a wrapper would never be drawn.
 */
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

/** The line under a chart that hatches partial buckets, saying what they are and how much of each is there. */
export function PartialNote({ bands, className = "" }: { bands: readonly PartialBand[]; className?: string }) {
  const text = partialCaption(bands);
  if (text === null) return null;
  return <p className={`mt-1 text-[11px] text-ink-3 ${className}`}>{text}</p>;
}

/**
 * The dots a series with no input at all stands under. The same recessive ink
 * the other two marks use, so it is background and not a mark, but a stipple
 * rather than a flat tint or a hatch: a stretch nobody indexed, a bucket that
 * is still filling, and a whole bucket whose series had nothing to draw from
 * are three different facts and must not read the same.
 */
export const MISSING_FILL = "var(--ink-3)";
export const MISSING_FILL_OPACITY = 0.55;
export const MISSING_PATTERN_ID = "missing-series-dots";

/**
 * The stipple geometry. Tight enough that a band only a few pixels wide (one
 * short run of buckets on a long range) still catches a column of dots.
 */
export const DOT_SPACING = 5;
export const DOT_RADIUS = 1;

/**
 * The dashed edge every band wears. A run of a handful of buckets is a few
 * pixels of axis, too narrow for any fill to read as a texture, so the mark
 * cannot rest on the stipple alone: the edges say where it starts and stops
 * at any width, and neither of the other two marks is outlined.
 */
export const MISSING_STROKE_OPACITY = 0.5;
export const MISSING_DASH = "2 2";

/**
 * The pattern the missing-series bands are filled from. Put it in the chart's
 * own `defs`, and only where a band is actually drawn: several charts on one
 * page each carry their own copy under the same id, and every copy is the
 * same definition, so whichever one `url(#id)` finds paints the same dots.
 */
export function MissingDots() {
  return (
    <pattern id={MISSING_PATTERN_ID} width={DOT_SPACING} height={DOT_SPACING} patternUnits="userSpaceOnUse">
      <circle cx={DOT_SPACING / 2} cy={DOT_SPACING / 2} r={DOT_RADIUS} fill={MISSING_FILL} />
    </pattern>
  );
}

/**
 * The runs a series has no value over, as reference areas. Returned as an
 * array for the same reason `gapBands` is: recharts reads its children by
 * type, so a wrapper would never be drawn. Call it before the series in the
 * chart's children and the bands sit under them.
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

/** The line under a chart that dots the runs a series had nothing to draw from, saying what is missing and over how many buckets. */
export function MissingNote({ runs, className = "" }: { runs: readonly MissingRun[]; className?: string }) {
  const text = missingCaption(runs);
  if (text === null) return null;
  return <p className={`mt-1 text-[11px] text-ink-3 ${className}`}>{text}</p>;
}
