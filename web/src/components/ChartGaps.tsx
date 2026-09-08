"use client";

import { usePlotArea, useXAxisScale } from "recharts";
import { gapBandLabel, gapCaption, type Gap, type GapModel, type GapWindow } from "@/lib/gaps";
import { missingBandLabel, missingCaption, type MissingRun } from "@/lib/missing";
import { partialBandLabel, partialCaption, partialRuns, type PartialBand } from "@/lib/partial";
import { HatchPattern } from "./primitives";

/** Shading for a span with nothing in it: the muted ink at a tenth. It is a background and not a mark,
 * so it carries no colour of its own. */
export const GAP_FILL = "var(--ink-3)";
export const GAP_FILL_OPACITY = 0.1;

/** A band narrower than this share of the axis carries no word: it would collide with whatever sits
 * beside it, and the caption says the same thing in full. */
export const GAP_LABEL_MIN_SHARE = 0.09;

/** What a test finds a band by, and how far under the top of the plot its word sits. */
export const BAND_CLASS = "gascurve-band";
export const BAND_LABEL_OFFSET = 5;
const BAND_LABEL_SIZE = 10;

type BandKind = "gap" | "partial" | "missing";

type BandStyle = { kind: BandKind; fill: string; fillOpacity: number; stroke?: string; strokeOpacity?: number; strokeDasharray?: string };

type DrawnBand = { from: number; to: number; label: string };

/**
 * Every band of one kind as a single layer of rects rather than one `ReferenceArea` each. A day of
 * one-minute buckets carries hundreds of them, and each reference area is a component with its own store
 * subscription, so a chart's re-render woke thousands of subscribers. The hooks put this in the chart's
 * default layer, above the grid and below the marks, which is where the bands belong: nothing is drawn
 * over a band, since the lines break and the stack is null there.
 */
function BandLayer({ bands, window, style }: { bands: readonly DrawnBand[]; window: GapWindow; style: BandStyle }) {
  const scale = useXAxisScale();
  const plot = usePlotArea();
  if (bands.length === 0 || scale === undefined || plot === undefined) return null;
  const span = window.to - window.from;
  const left = plot.x;
  const right = plot.x + plot.width;
  return (
    <g className={`${BAND_CLASS}s`}>
      {bands.map((band) => {
        const a = scale(band.from);
        const b = scale(band.to);
        if (a === undefined || b === undefined) return null;
        const x = Math.max(left, Math.min(a, b));
        const end = Math.min(right, Math.max(a, b));
        if (!(end > x)) return null;
        const wide = span > 0 && (band.to - band.from) / span >= GAP_LABEL_MIN_SHARE;
        return (
          <g key={`${band.from}-${band.to}`}>
            <rect
              className={BAND_CLASS}
              data-band={style.kind}
              x={x}
              y={plot.y}
              width={end - x}
              height={plot.height}
              fill={style.fill}
              fillOpacity={style.fillOpacity}
              stroke={style.stroke}
              strokeOpacity={style.strokeOpacity}
              strokeDasharray={style.strokeDasharray}
            />
            {wide ? (
              <text x={(x + end) / 2} y={plot.y + BAND_LABEL_OFFSET} textAnchor="middle" dominantBaseline="hanging" fill="var(--ink-2)" fontSize={BAND_LABEL_SIZE}>
                {band.label}
              </text>
            ) : null}
          </g>
        );
      })}
    </g>
  );
}

const GAP_STYLE: BandStyle = { kind: "gap", fill: GAP_FILL, fillOpacity: GAP_FILL_OPACITY };

/** The shaded spans of a chart's window. Put it before the series so they draw over it. */
export function GapBands({ gaps, window }: { gaps: readonly Gap[]; window: GapWindow }) {
  return <BandLayer bands={gaps.map((gap) => ({ from: gap.from, to: gap.to, label: gapBandLabel(gap) }))} window={window} style={GAP_STYLE} />;
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

const PARTIAL_STYLE: BandStyle = { kind: "partial", fill: `url(#${PARTIAL_PATTERN_ID})`, fillOpacity: PARTIAL_FILL_OPACITY };

/** The buckets a sum chart leaves out, coalesced into runs so a stretch of them is one rect. */
export function PartialBands({ bands, window }: { bands: readonly PartialBand[]; window: GapWindow }) {
  return <BandLayer bands={partialRuns(bands).map((run) => ({ from: run.from, to: run.to, label: partialBandLabel(run.kind) }))} window={window} style={PARTIAL_STYLE} />;
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

const MISSING_STYLE: BandStyle = {
  kind: "missing",
  fill: `url(#${MISSING_PATTERN_ID})`,
  fillOpacity: MISSING_FILL_OPACITY,
  stroke: MISSING_FILL,
  strokeOpacity: MISSING_STROKE_OPACITY,
  strokeDasharray: MISSING_DASH,
};

/** The runs a series has no value over. Put it before the series in the chart's children. */
export function MissingBands({ runs, window }: { runs: readonly MissingRun[]; window: GapWindow }) {
  return <BandLayer bands={runs.map((run) => ({ from: run.from, to: run.to, label: missingBandLabel(run.kind) }))} window={window} style={MISSING_STYLE} />;
}

/** The line under a chart that dots the runs a series had nothing to draw from. */
export function MissingNote({ runs, className = "" }: { runs: readonly MissingRun[]; className?: string }) {
  const text = missingCaption(runs);
  if (text === null) return null;
  return <p className={`mt-1 text-[11px] text-ink-3 ${className}`}>{text}</p>;
}
