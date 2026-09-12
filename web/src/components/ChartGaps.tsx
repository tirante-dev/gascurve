"use client";

import { DefaultZIndexes, Text, usePlotArea, useXAxisScale, ZIndexLayer } from "recharts";
import { gapBandLabel, gapCaption, type Gap, type GapModel } from "@/lib/gaps";
import { missingBandLabel, missingCaption, type MissingRun } from "@/lib/missing";
import { fidelityBandLabel, fidelityCaption, fidelityRuns, type FidelityBand } from "@/lib/fidelity";
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

type BandKind = "gap" | "partial" | "missing" | "fidelity";

type BandStyle = { kind: BandKind; fill: string; fillOpacity: number; stroke?: string; strokeOpacity?: number; strokeDasharray?: string };

type DrawnBand = { from: number; to: number; label: string };

/**
 * Every band of one kind as one layer of rects, not one `ReferenceArea` each: a reference area is a
 * component with its own store subscription, and a day of minute buckets carries hundreds, so any chart's
 * re-render woke thousands of subscribers. The rects sit in the default layer, above the grid and under
 * the marks, where nothing covers them because the lines break and the stack is null there. The words go
 * in the label layer the reference areas used, over every mark, so a target line crossing a band cannot
 * cross out what it says.
 */
function BandLayer({ bands, style }: { bands: readonly DrawnBand[]; style: BandStyle }) {
  const scale = useXAxisScale();
  const plot = usePlotArea();
  if (bands.length === 0 || scale === undefined || plot === undefined) return null;
  const left = plot.x;
  const right = plot.x + plot.width;
  const drawn = bands
    .map((band) => {
      const a = scale(band.from);
      const b = scale(band.to);
      if (a === undefined || b === undefined) return null;
      const x = Math.max(left, Math.min(a, b));
      const end = Math.min(right, Math.max(a, b));
      // The share is of what is on screen, not of the band's own span: a band running off the axis draws
      // as a sliver, and a word over a sliver lands on whatever sits beside it.
      return end > x ? { key: `${band.from}-${band.to}`, x, width: end - x, label: plot.width > 0 && (end - x) / plot.width >= GAP_LABEL_MIN_SHARE ? band.label : null } : null;
    })
    .filter((band): band is { key: string; x: number; width: number; label: string | null } => band !== null);
  const labelled = drawn.filter((band) => band.label !== null);
  return (
    <>
      <g className={`${BAND_CLASS}s`}>
        {drawn.map((band) => (
          <rect
            key={band.key}
            className={BAND_CLASS}
            data-band={style.kind}
            x={band.x}
            y={plot.y}
            width={band.width}
            height={plot.height}
            fill={style.fill}
            fillOpacity={style.fillOpacity}
            stroke={style.stroke}
            strokeOpacity={style.strokeOpacity}
            strokeDasharray={style.strokeDasharray}
          />
        ))}
      </g>
      {labelled.length > 0 ? (
        <ZIndexLayer zIndex={DefaultZIndexes.label}>
          <g className={`${BAND_CLASS}-labels`}>
            {labelled.map((band) => (
              <Text key={band.key} className="recharts-label" x={band.x + band.width / 2} y={plot.y + BAND_LABEL_OFFSET} textAnchor="middle" verticalAnchor="start">
                {band.label}
              </Text>
            ))}
          </g>
        </ZIndexLayer>
      ) : null}
    </>
  );
}

const GAP_STYLE: BandStyle = { kind: "gap", fill: GAP_FILL, fillOpacity: GAP_FILL_OPACITY };

/** The shaded spans of a chart's window. Put it before the series so they draw over it. */
export function GapBands({ gaps }: { gaps: readonly Gap[] }) {
  return <BandLayer bands={gaps.map((gap) => ({ from: gap.from, to: gap.to, label: gapBandLabel(gap) }))} style={GAP_STYLE} />;
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
export function PartialBands({ bands }: { bands: readonly PartialBand[] }) {
  return <BandLayer bands={partialRuns(bands).map((run) => ({ from: run.from, to: run.to, label: partialBandLabel(run.kind) }))} style={PARTIAL_STYLE} />;
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
export function MissingBands({ runs }: { runs: readonly MissingRun[] }) {
  return <BandLayer bands={runs.map((run) => ({ from: run.from, to: run.to, label: missingBandLabel(run.kind) }))} style={MISSING_STYLE} />;
}

/** The line under a chart that dots the runs a series had nothing to draw from. */
export function MissingNote({ runs, className = "" }: { runs: readonly MissingRun[]; className?: string }) {
  const text = missingCaption(runs);
  if (text === null) return null;
  return <p className={`mt-1 text-[11px] text-ink-3 ${className}`}>{text}</p>;
}

/**
 * The counter-hatch a bucket the replay cannot vouch for stands under. It leans the other way from the
 * partial hatch and wears the dashed edge, because a bucket still filling and a bucket whose pricing
 * model nobody has checked are different claims and a chart may carry both at once. Its marks are drawn
 * normally: the numbers exist, it is their standing that is in question.
 */
export const FIDELITY_FILL = "var(--ink-3)";
export const FIDELITY_FILL_OPACITY = 0.35;
export const FIDELITY_PATTERN_ID = "unverified-model-hatch";
export const FIDELITY_HATCH_ANGLE = -45;

/** The pattern the fidelity bands are filled from. Put it in the chart's own `defs`. */
export function FidelityHatch() {
  return <HatchPattern id={FIDELITY_PATTERN_ID} color={FIDELITY_FILL} angle={FIDELITY_HATCH_ANGLE} />;
}

const FIDELITY_STYLE: BandStyle = {
  kind: "fidelity",
  fill: `url(#${FIDELITY_PATTERN_ID})`,
  fillOpacity: FIDELITY_FILL_OPACITY,
  stroke: FIDELITY_FILL,
  strokeOpacity: MISSING_STROKE_OPACITY,
  strokeDasharray: MISSING_DASH,
};

/** The buckets whose replay is unverified, coalesced into runs so a stretch of them is one rect. */
export function FidelityBands({ bands }: { bands: readonly FidelityBand[] }) {
  return <BandLayer bands={fidelityRuns(bands).map((run) => ({ from: run.from, to: run.to, label: fidelityBandLabel(run.kind) }))} style={FIDELITY_STYLE} />;
}

/** The line under a chart that marks unverified buckets. */
export function FidelityNote({ bands, className = "" }: { bands: readonly FidelityBand[]; className?: string }) {
  const text = fidelityCaption(bands);
  if (text === null) return null;
  return <p className={`mt-1 text-[11px] text-ink-3 ${className}`}>{text}</p>;
}
