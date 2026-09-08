// The fee dial: where a multiplier over the floor sits on a speedometer, and which of its three bands
// that is. The multiplier is the pricer's own figure (the base fee over the minimum the owner set), so
// 1x is the floor and the scale runs a factor of ten per half turn.

export type DialTone = "good" | "warning" | "critical";

export const GREEN_TO = 2;
/** From GREEN_TO up to this multiplier the fee is amber; above it, red. */
export const AMBER_TO = 10;

export function dialTone(multiplier: number): DialTone {
  if (multiplier <= GREEN_TO) return "good";
  if (multiplier <= AMBER_TO) return "warning";
  return "critical";
}

export const TONE_SENTENCE: Record<DialTone, string> = {
  good: "near the floor",
  warning: "above the floor",
  critical: "far above the floor",
};

/** The right end of the dial: a hundred times the floor, two decades from the left end. */
export const DIAL_MAX = 100;

/**
 * Where a multiplier sits on the dial, 0 at the left end (1x, the floor) and 1 at the right (DIAL_MAX),
 * on a log scale so each decade takes the same arc. Below the floor is not a state the pricer produces;
 * anything there, and NaN, sits at the left end, and anything past DIAL_MAX pins the right.
 */
export function dialPosition(multiplier: number): number {
  if (!(multiplier > 1)) return 0;
  return Math.min(1, Math.log10(multiplier) / Math.log10(DIAL_MAX));
}

export type DialBand = { tone: DialTone; from: number; to: number };

/** The three bands, meeting exactly at the thresholds the tone changes at, so the band under the needle
 * is the colour of the figure below it. */
export const DIAL_BANDS: readonly DialBand[] = [
  { tone: "good", from: 0, to: dialPosition(GREEN_TO) },
  { tone: "warning", from: dialPosition(GREEN_TO), to: dialPosition(AMBER_TO) },
  { tone: "critical", from: dialPosition(AMBER_TO), to: 1 },
];

export const DIAL_CX = 50;
export const DIAL_CY = 52;
export const DIAL_RADIUS = 40;

/** A point on the rim at `position`, `radius` from the hub: left end at 0, top at 0.5, right end at 1. */
export function dialPoint(position: number, radius = DIAL_RADIUS): { x: number; y: number } {
  const angle = Math.PI * (1 - Math.min(1, Math.max(0, position)));
  return { x: DIAL_CX + radius * Math.cos(angle), y: DIAL_CY - radius * Math.sin(angle) };
}

/** The SVG path of the rim from one position to another. An arc of a half circle never needs large-arc. */
export function dialArc(from: number, to: number, radius = DIAL_RADIUS): string {
  const a = dialPoint(from, radius);
  const b = dialPoint(to, radius);
  return `M ${a.x.toFixed(2)} ${a.y.toFixed(2)} A ${radius} ${radius} 0 0 1 ${b.x.toFixed(2)} ${b.y.toFixed(2)}`;
}
