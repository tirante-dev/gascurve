// The fee gauge: where a multiplier over the floor sits on the arc, and the instrument drawn around it.
// The multiplier is the pricer's own figure, so 1x is the floor and the scale runs a decade per half turn.

/** The right end of the dial: a hundred times the floor, two decades from the left end. */
export const DIAL_MAX = 100;
export const DIAL_SCALE_LABEL = `logarithmic 1× to ${DIAL_MAX}×`;

/**
 * Where a multiplier sits on the dial, 0 at the left end (1x, the floor) and 1 at the right (DIAL_MAX),
 * on a log scale so each decade takes the same arc. Below the floor is not a state the pricer produces;
 * anything there, and NaN, sits at the left end, and anything past DIAL_MAX pins the right.
 */
export function dialPosition(multiplier: number): number {
  if (!(multiplier > 1)) return 0;
  return Math.min(1, Math.log10(multiplier) / Math.log10(DIAL_MAX));
}

/** The hub sits on the horizon near the foot of the box, so the box clears the numerals ring above the
 * arc and the hub's own lower half below it. */
export const DIAL_VIEW_W = 240;
export const DIAL_VIEW_H = 127;
export const DIAL_CX = 120;
export const DIAL_CY = 118;

/** The pressure ramp's centreline; every other radius is placed against it. */
export const DIAL_RADIUS = 84;
export const PRESSURE_WIDTH = 6;
export const R_BEZEL = 98;
export const R_TICK_OUT = 95;
export const R_TICK_MINOR = 91;
export const R_TICK_MAJOR = 88;
export const R_NUMERAL = 108;
export const R_NEEDLE = 78;
export const R_HUB = 7;

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

/** The endpoints and decade midpoint make the logarithmic context explicit. */
export const DIAL_MAJOR_TICKS: readonly number[] = [1, 10, DIAL_MAX];
export const DIAL_MINOR_TICKS: readonly number[] = [1.3, 1.6, 2, 3, 4, 5, 6, 7, 8, 20, 30, 50, 70];

/** Half the angular width of the needle's base, as a fraction of the half turn. */
const NEEDLE_HALF = 0.055;

/** The needle as a tapered blade: two points across the hub and one at the tip. */
export function needlePoints(multiplier: number, radius = R_NEEDLE): { x: number; y: number }[] {
  const here = dialPosition(multiplier);
  return [dialPoint(here - NEEDLE_HALF, R_HUB), dialPoint(here, radius), dialPoint(here + NEEDLE_HALF, R_HUB)];
}

/** The perspective grid below the horizon, in a box of its own: it stretches to whatever height the
 * readout needs, so it cannot share the arc's viewBox. */
export const GRID_W = 240;
export const GRID_H = 100;
export const GRID_VANISH_X = GRID_W / 2;

/** Vertical spacing at the bottom edge, and how hard the horizontals crowd the horizon. */
const GRID_SPREAD = 58;
const GRID_CROWD = 1.8;
const GRID_ROWS = 7;
const GRID_COLS = 8;

/** Where the grid's lines land: verticals by their x at the bottom edge, horizontals by their y. */
export function gridLines(): { verticals: number[]; horizontals: number[] } {
  const verticals: number[] = [];
  for (let k = -GRID_COLS; k <= GRID_COLS; k++) {
    if (k !== 0) verticals.push(GRID_VANISH_X + k * GRID_SPREAD);
  }
  const horizontals: number[] = [];
  for (let i = 1; i <= GRID_ROWS; i++) {
    horizontals.push(GRID_H * Math.pow(i / GRID_ROWS, GRID_CROWD));
  }
  return { verticals, horizontals };
}
