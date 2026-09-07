"use client";

import { ReferenceArea } from "recharts";
import { gapBandLabel, gapCaption, type Gap, type GapModel, type GapWindow } from "@/lib/gaps";
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
