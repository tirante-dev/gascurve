// The social card, drawn from the live snapshot when a link is shared. Rasterised by satori and resvg, so
// there is no stylesheet, no cascade, and no face of the card's own inside the dial's SVG: hence a frozen
// palette, geometry-only markup, and numerals handed out as positions. See docs/ARCHITECTURE.md.

import {
  BAND_WIDTH,
  DIAL_BANDS,
  DIAL_CX,
  DIAL_CY,
  DIAL_MAJOR_TICKS,
  DIAL_MINOR_TICKS,
  DIAL_VIEW_H,
  DIAL_VIEW_W,
  dialArc,
  dialPoint,
  dialPosition,
  dialTone,
  litBands,
  needlePoints,
  R_BEZEL,
  R_HUB,
  R_NUMERAL,
  R_TICK_MAJOR,
  R_TICK_MINOR,
  R_TICK_OUT,
  type DialTone,
} from "@/lib/dial";
import { CARD_SIZE } from "@/lib/seo";
import type { LiveSnapshot } from "@/types";
import { formatGwei, formatGweiFixed, formatInteger, formatMultiplierFixed, weiToGweiNumber } from "@/utils/format";

/** The dark theme's tokens, frozen: a card has no stylesheet and no viewer to have a preference, so these
 * repeat globals.css rather than referencing it. Changing one there means changing it here. */
export const CARD_COLORS = {
  page: "#0f0326",
  panel: "#140a2e",
  ink: "#f7f2ff",
  ink2: "#f2bfe6",
  ink3: "#ab9fcb",
  label: "#a8f0d8",
  accent: "#ff4fd8",
  accent2: "#2ee6ff",
  chromeMid: "#b07cff",
} as const;

export const CARD_TONE_COLORS: Record<DialTone, string> = {
  good: "#3ddc97",
  warning: "#ffb020",
  critical: "#ff5c7a",
};

/** How far the unlit ring is turned down, matching the gauge on the page. */
const DIM = 0.22;

/** The dial is drawn at its own viewBox and blown up by this much, so its stroke widths stay in the
 * proportions the page uses rather than being redesigned for the card. */
export const CARD_DIAL_SCALE = 2.4;

export const CARD_DIAL_W = DIAL_VIEW_W * CARD_DIAL_SCALE;
export const CARD_DIAL_H = DIAL_VIEW_H * CARD_DIAL_SCALE;

const NUMERAL_TEXT: Record<number, string> = { 2: "2×", 10: "10×" };

/** A ring numeral and the point it is centred on, in card pixels. Placed by the renderer rather than the
 * SVG so it comes out in the card's own face; see the note at the top of this file. */
export type CardNumeral = { text: string; x: number; y: number };

export function cardNumerals(scale = CARD_DIAL_SCALE): CardNumeral[] {
  return DIAL_MAJOR_TICKS.map((m) => {
    const p = dialPoint(dialPosition(m), R_NUMERAL);
    return { text: NUMERAL_TEXT[m], x: p.x * scale, y: p.y * scale };
  });
}

/**
 * The gauge's own glow, the same two filters FeeGauge draws with: the ring is a lit tube, the needle and
 * the horizon carry the softer halo. The region is in user space rather than the browser version's box
 * relative one, because the horizon is a horizontal line whose box has no height, and a filter region
 * measured against that box collapses to nothing and takes the line with it.
 */
const FILTER_REGION = `filterUnits="userSpaceOnUse" x="-24" y="-24" width="${DIAL_VIEW_W + 48}" height="${DIAL_VIEW_H + 48}"`;

const GLOW_FILTERS =
  `<filter id="neon" ${FILTER_REGION}><feGaussianBlur stdDeviation="2.8" result="b"/><feMerge><feMergeNode in="b"/><feMergeNode in="b"/><feMergeNode in="SourceGraphic"/></feMerge></filter>` +
  `<filter id="soft" ${FILTER_REGION}><feGaussianBlur stdDeviation="1.4" result="b"/><feMerge><feMergeNode in="b"/><feMergeNode in="SourceGraphic"/></feMerge></filter>`;

function line(from: { x: number; y: number }, to: { x: number; y: number }, stroke: string, width: number): string {
  return `<line x1="${from.x.toFixed(2)}" y1="${from.y.toFixed(2)}" x2="${to.x.toFixed(2)}" y2="${to.y.toFixed(2)}" stroke="${stroke}" stroke-width="${width}" stroke-linecap="round"/>`;
}

function arc(from: number, to: number, stroke: string, width: number, opacity = 1): string {
  return `<path d="${dialArc(from, to)}" fill="none" stroke="${stroke}" stroke-width="${width}" opacity="${opacity}"/>`;
}

/**
 * The gauge as a standalone SVG document, at `multiplier` over the floor. Null is a reading the card could
 * not take: the instrument is drawn unlit and without a needle, which reads as no measurement rather than
 * as a measurement of zero.
 */
export function cardDialSvg(multiplier: number | null): string {
  const parts: string[] = [
    `<defs>${GLOW_FILTERS}<linearGradient id="chrome" x1="0" y1="0" x2="1" y2="0"><stop offset="0%" stop-color="${CARD_COLORS.accent}"/><stop offset="50%" stop-color="${CARD_COLORS.chromeMid}"/><stop offset="100%" stop-color="${CARD_COLORS.accent2}"/></linearGradient></defs>`,
    `<path d="${dialArc(0, 1, R_BEZEL)}" fill="none" stroke="url(#chrome)" stroke-width="1.6" opacity="0.9"/>`,
  ];
  for (const m of DIAL_MINOR_TICKS) {
    parts.push(line(dialPoint(dialPosition(m), R_TICK_MINOR), dialPoint(dialPosition(m), R_TICK_OUT), CARD_COLORS.ink3, 1));
  }
  for (const m of DIAL_MAJOR_TICKS) {
    parts.push(line(dialPoint(dialPosition(m), R_TICK_MAJOR), dialPoint(dialPosition(m), R_TICK_OUT), CARD_COLORS.ink2, 1.8));
  }
  for (const band of DIAL_BANDS) {
    parts.push(arc(band.from, band.to, CARD_TONE_COLORS[band.tone], BAND_WIDTH, DIM));
  }
  if (multiplier !== null) {
    const lit = litBands(multiplier).map((band) => arc(band.from, band.to, CARD_TONE_COLORS[band.tone], BAND_WIDTH));
    parts.push(`<g filter="url(#neon)">${lit.join("")}</g>`);
  }
  parts.push(`<g filter="url(#soft)">${line({ x: 6, y: DIAL_CY }, { x: DIAL_VIEW_W - 6, y: DIAL_CY }, CARD_COLORS.accent2, 1.1)}</g>`);
  if (multiplier !== null) {
    const points = needlePoints(multiplier)
      .map((p) => `${p.x.toFixed(2)},${p.y.toFixed(2)}`)
      .join(" ");
    parts.push(`<polygon points="${points}" fill="${CARD_COLORS.ink}" filter="url(#soft)"/>`);
  }
  parts.push(`<circle cx="${DIAL_CX}" cy="${DIAL_CY}" r="${R_HUB}" fill="url(#chrome)"/>`);
  parts.push(`<circle cx="${DIAL_CX}" cy="${DIAL_CY}" r="3" fill="${CARD_COLORS.ink}"/>`);
  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${DIAL_VIEW_W} ${DIAL_VIEW_H}" width="${CARD_DIAL_W}" height="${CARD_DIAL_H}">${parts.join("")}</svg>`;
}

/** An SVG document as a source a renderer will accept in place of a file. */
export function svgDataUri(svg: string): string {
  return `data:image/svg+xml;base64,${Buffer.from(svg, "utf8").toString("base64")}`;
}

/** The card's own layout. The fitting below measures against these, so the box a figure is set to fit and
 * the box it is drawn in cannot drift apart. */
export const CARD_PAD_X = 64;
export const READOUT_GAP = 44;
export const READOUT_W = CARD_SIZE.width - 2 * CARD_PAD_X - CARD_DIAL_W - READOUT_GAP;

/** A readout line as it is drawn: the exact string, and the size that string fits at. The size travels
 * with the text so the two cannot be derived from different strings. */
export type CardLine = { text: string; size: number };

/** Everything the card prints, already formatted and already fitted. Null where there was no reading. */
export type CardReading = {
  fee: CardLine;
  /** False for an off scale fee: there is no figure left for a unit to belong to. */
  feeUnit: boolean;
  multiplier: CardLine;
  /** Where the needle is set, which is the reading itself rather than anything printed. */
  value: number;
  tone: DialTone;
  floor: string;
  block: string;
} | null;

/** What the card prints in place of a figure past 2^53, where the digits that reached the browser are no
 * longer the ones the api sent. The same words the footer's bips figure uses, for the same reason. */
export const OFF_SCALE = "off scale";

/** How wide a figure's character runs against its size, and what the words beside it take. Measured off a
 * rendered card, since only the rasteriser knows the face's advances; the advance is rounded up from the
 * 0.48 measured, so a fit is never optimistic. */
export const FIGURE_ADVANCE = 0.5;
export const FEE_UNIT_W = 80;
export const MULTIPLIER_UNIT_W = 170;

/** Whether a figure of `length` characters, set at `size`, still leaves room for the words beside it. A
 * card is a fixed box with no reflow and no ellipsis: a figure that does not fit is lost off the edge. */
export function figureFits(length: number, size: number, unitW: number): boolean {
  return length * FIGURE_ADVANCE * size + unitW <= READOUT_W;
}

/** The sizes each line steps down through as its figure lengthens, paired with the longest string that
 * size may set. A test holds every step to fitting, so an edit here cannot quietly start clipping. */
export const FEE_SIZES: readonly (readonly [number, number])[] = [
  [7, 96],
  [10, 72],
  [14, 52],
  [18, 40],
  [23, 32],
];

export const MULTIPLIER_SIZES: readonly (readonly [number, number])[] = [
  [6, 44],
  [10, 34],
  [14, 28],
  [19, 24],
];

/** The longest figure a line can be handed: nothing past 2^53 is printed, and the widest value under it is
 * 9,007,199,254,740,991 with a decimal place. The multiplier's sign puts it in the same place. */
export const MAX_FIGURE_CHARS = 23;

/** `text` as a line, at the size for the first step its length falls within. Not the largest size it would
 * physically fit at: the steps are the ladder, so a figure of a given length is always set the same. */
export function fit(text: string, steps: readonly (readonly [number, number])[]): CardLine {
  for (const [length, size] of steps) {
    if (text.length <= length) return { text, size };
  }
  return { text, size: steps[steps.length - 1][1] };
}

/** True for a figure whose digits a double no longer carries exactly, so none of them are the api's. */
function pastExact(value: number): boolean {
  return Math.abs(value) > Number.MAX_SAFE_INTEGER;
}

/** Null when the snapshot carries no reading to draw: a figure that is not finite and non-negative is a
 * broken reading rather than a small one, and the card draws the instrument unlit rather than an "n/a" in
 * the colour of a fee at the top of the scale. */
export function cardReading(snapshot: LiveSnapshot): CardReading {
  const bips = snapshot.multiplierBips;
  const gwei = weiToGweiNumber(snapshot.baseFee);
  if (!Number.isFinite(bips) || !Number.isFinite(gwei) || bips < 0 || gwei < 0) return null;
  const offScale = pastExact(bips);
  const feeOffScale = pastExact(gwei);
  const value = bips / 10_000;
  // The tone follows the figure as printed rather than the raw multiplier, as it does on the page: a
  // "2.004x" prints as "2.00x", and amber would put it a band away from where a reader can see it rounds
  // to. Rounded as a number, not read back from the text, which carries separators Number cannot parse.
  const rounded = Math.round(value * 100) / 100;
  // The sign is part of the line, so it is part of what the line is sized to fit. Nothing follows
  // "off scale": there is no figure for a sign to belong to.
  const multiplier = offScale ? OFF_SCALE : `${formatMultiplierFixed(value)}×`;
  return {
    fee: fit(feeOffScale ? OFF_SCALE : formatGweiFixed(gwei), FEE_SIZES),
    feeUnit: !feeOffScale,
    multiplier: fit(multiplier, MULTIPLIER_SIZES),
    value,
    // A reading with no digits left is still one the pricer saturated to reach, the top of the scale.
    tone: offScale ? "critical" : dialTone(rounded),
    floor: formatGwei(snapshot.minBaseFee),
    block: formatInteger(snapshot.block.number),
  };
}
