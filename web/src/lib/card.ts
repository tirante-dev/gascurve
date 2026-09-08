// The social card: the same gauge the page draws, rendered at share time from the live snapshot, so a
// link posted anywhere unfurls with the fee the chain is charging rather than a fixed picture.
//
// Two constraints shape everything here. The card is rasterised by satori and resvg, which lay out flexbox
// and a subset of CSS but no stylesheet, no cascade and no media query; and the dial arrives as an SVG
// data URI, whose text would be drawn in resvg's fallback face rather than the card's own. So the palette
// is frozen dark rather than tokenised, and the SVG carries geometry only: every glyph is placed by the
// renderer around it, which is why the ring's numerals come out of here as positions rather than markup.

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
import type { LiveSnapshot } from "@/types";
import { formatGwei, formatGweiFixed, formatInteger, formatMultiplierFixed, weiToGweiNumber } from "@/utils/format";

/**
 * The dark theme's tokens, frozen. A card is rasterised without a stylesheet and without a viewer to have
 * a preference, so the values from globals.css are repeated rather than referenced; changing one there
 * means changing it here.
 */
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
 *
 * The lit stretch's glow is a wider translucent stroke under it rather than a blur filter, because the
 * card's rasteriser renders filters unevenly and a card is one still frame that has to come out right.
 */
export function cardDialSvg(multiplier: number | null): string {
  const parts: string[] = [
    `<defs><linearGradient id="chrome" x1="0" y1="0" x2="1" y2="0"><stop offset="0%" stop-color="${CARD_COLORS.accent}"/><stop offset="50%" stop-color="${CARD_COLORS.chromeMid}"/><stop offset="100%" stop-color="${CARD_COLORS.accent2}"/></linearGradient></defs>`,
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
    for (const band of litBands(multiplier)) {
      parts.push(arc(band.from, band.to, CARD_TONE_COLORS[band.tone], BAND_WIDTH * 2, 0.16));
      parts.push(arc(band.from, band.to, CARD_TONE_COLORS[band.tone], BAND_WIDTH, 1));
    }
  }
  parts.push(line({ x: 6, y: DIAL_CY }, { x: DIAL_VIEW_W - 6, y: DIAL_CY }, CARD_COLORS.accent2, 1.1));
  if (multiplier !== null) {
    const points = needlePoints(multiplier)
      .map((p) => `${p.x.toFixed(2)},${p.y.toFixed(2)}`)
      .join(" ");
    parts.push(`<polygon points="${points}" fill="${CARD_COLORS.ink}"/>`);
  }
  parts.push(`<circle cx="${DIAL_CX}" cy="${DIAL_CY}" r="${R_HUB}" fill="url(#chrome)"/>`);
  parts.push(`<circle cx="${DIAL_CX}" cy="${DIAL_CY}" r="3" fill="${CARD_COLORS.ink}"/>`);
  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${DIAL_VIEW_W} ${DIAL_VIEW_H}" width="${CARD_DIAL_W}" height="${CARD_DIAL_H}">${parts.join("")}</svg>`;
}

/** An SVG document as a source a renderer will accept in place of a file. */
export function svgDataUri(svg: string): string {
  return `data:image/svg+xml;base64,${Buffer.from(svg, "utf8").toString("base64")}`;
}

/** Everything the card prints, already formatted. Null where the api could not be reached. */
export type CardReading = {
  baseFee: string;
  multiplier: number;
  multiplierText: string;
  tone: DialTone;
  floor: string;
  block: string;
} | null;

export function cardReading(snapshot: LiveSnapshot): NonNullable<CardReading> {
  const multiplier = snapshot.multiplierBips / 10_000;
  // The tone follows the figure as printed rather than the raw multiplier, as it does on the page: a
  // "2.00x" in amber would put the figure a band away from where a reader can see it rounds to.
  const multiplierText = formatMultiplierFixed(multiplier);
  return {
    baseFee: formatGweiFixed(weiToGweiNumber(snapshot.baseFee)),
    multiplier,
    multiplierText,
    tone: dialTone(Number(multiplierText)),
    floor: formatGwei(snapshot.minBaseFee),
    block: formatInteger(snapshot.block.number),
  };
}
