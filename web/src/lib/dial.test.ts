import { describe, expect, it } from "vitest";
import {
  AMBER_TO,
  DIAL_BANDS,
  DIAL_CX,
  DIAL_CY,
  DIAL_MAJOR_TICKS,
  DIAL_MAX,
  DIAL_MINOR_TICKS,
  DIAL_RADIUS,
  DIAL_VIEW_H,
  DIAL_VIEW_W,
  dialArc,
  dialPoint,
  dialPosition,
  dialTone,
  GREEN_TO,
  GRID_H,
  GRID_VANISH_X,
  gridLines,
  litBands,
  needlePoints,
  R_HUB,
  R_NEEDLE,
  R_NUMERAL,
  TONE_SENTENCE,
} from "./dial";

describe("dialTone", () => {
  it("is green to twice the floor, amber to ten times, red above", () => {
    expect(dialTone(1)).toBe("good");
    expect(dialTone(GREEN_TO)).toBe("good");
    expect(dialTone(2.01)).toBe("warning");
    expect(dialTone(AMBER_TO)).toBe("warning");
    expect(dialTone(10.01)).toBe("critical");
    expect(dialTone(1000)).toBe("critical");
    expect(Object.keys(TONE_SENTENCE)).toEqual(["good", "warning", "critical"]);
  });
});

describe("dialPosition", () => {
  it("starts at the floor and takes a decade per half turn", () => {
    expect(dialPosition(1)).toBe(0);
    expect(dialPosition(10)).toBeCloseTo(0.5);
    expect(dialPosition(DIAL_MAX)).toBe(1);
    expect(dialPosition(Math.sqrt(10))).toBeCloseTo(0.25);
  });
  it("pins the needle at the ends rather than swinging it off the dial", () => {
    expect(dialPosition(1000)).toBe(1);
    expect(dialPosition(Number.POSITIVE_INFINITY)).toBe(1);
    // The pricer never prices below the floor; anything that says so, and NaN, reads as the floor.
    expect(dialPosition(0.5)).toBe(0);
    expect(dialPosition(0)).toBe(0);
    expect(dialPosition(Number.NaN)).toBe(0);
  });
  it("draws the bands so they meet exactly where the tone changes", () => {
    expect(DIAL_BANDS.map((b) => b.tone)).toEqual(["good", "warning", "critical"]);
    expect(DIAL_BANDS[0].from).toBe(0);
    expect(DIAL_BANDS[0].to).toBe(dialPosition(GREEN_TO));
    expect(DIAL_BANDS[1].from).toBe(DIAL_BANDS[0].to);
    expect(DIAL_BANDS[1].to).toBe(dialPosition(AMBER_TO));
    expect(DIAL_BANDS[2].from).toBe(DIAL_BANDS[1].to);
    expect(DIAL_BANDS[2].to).toBe(1);
  });
});

describe("dial geometry", () => {
  it("runs from the left end over the top to the right end", () => {
    expect(dialPoint(0)).toEqual({ x: DIAL_CX - DIAL_RADIUS, y: expect.closeTo(DIAL_CY, 6) });
    expect(dialPoint(0.5)).toEqual({ x: expect.closeTo(DIAL_CX, 6), y: DIAL_CY - DIAL_RADIUS });
    expect(dialPoint(1)).toEqual({ x: DIAL_CX + DIAL_RADIUS, y: expect.closeTo(DIAL_CY, 6) });
    // A shorter radius for the needle, and a position outside the dial is clamped.
    expect(dialPoint(0.5, 10)).toEqual({ x: expect.closeTo(DIAL_CX, 6), y: DIAL_CY - 10 });
    expect(dialPoint(2)).toEqual(dialPoint(1));
    expect(dialPoint(-1)).toEqual(dialPoint(0));
  });
  it("describes a band as one arc of the half circle", () => {
    expect(dialArc(0, 0.5)).toBe(`M ${DIAL_CX - DIAL_RADIUS}.00 ${DIAL_CY}.00 A ${DIAL_RADIUS} ${DIAL_RADIUS} 0 0 1 ${DIAL_CX}.00 ${DIAL_CY - DIAL_RADIUS}.00`);
  });
});

describe("litBands", () => {
  it("lights the ring from the floor up to the reading and no further", () => {
    expect(litBands(1)).toEqual([]);
    expect(litBands(GREEN_TO).map((b) => b.tone)).toEqual(["good"]);
    // Inside a band the lit stretch stops at the reading rather than at the band's own end.
    const amber = litBands(5);
    expect(amber.map((b) => b.tone)).toEqual(["good", "warning"]);
    expect(amber[0]).toEqual(DIAL_BANDS[0]);
    expect(amber[1].from).toBe(DIAL_BANDS[1].from);
    expect(amber[1].to).toBeCloseTo(dialPosition(5), 12);
    expect(amber[1].to).toBeLessThan(DIAL_BANDS[1].to);
    // Past the last threshold every band is lit, and a spike lights the ring to its end.
    expect(litBands(20).map((b) => b.tone)).toEqual(["good", "warning", "critical"]);
    expect(litBands(1000).at(-1)).toEqual(DIAL_BANDS[2]);
  });
});

describe("needlePoints", () => {
  it("is a blade across the hub with its tip on the reading", () => {
    const [left, tip, right] = needlePoints(10);
    expect(tip).toEqual(dialPoint(0.5, R_NEEDLE));
    // The base straddles the hub at the hub's own radius, so the blade tapers.
    expect(Math.hypot(left.x - DIAL_CX, left.y - DIAL_CY)).toBeCloseTo(R_HUB, 6);
    expect(Math.hypot(right.x - DIAL_CX, right.y - DIAL_CY)).toBeCloseTo(R_HUB, 6);
    expect(left.x).toBeLessThan(right.x);
    expect(needlePoints(1000)[1]).toEqual(dialPoint(1, R_NEEDLE));
  });
});

describe("the instrument's box", () => {
  it("clears the numerals ring above the arc", () => {
    expect(DIAL_CY - R_NUMERAL).toBeGreaterThan(0);
    expect(DIAL_CX - R_NUMERAL).toBeGreaterThan(0);
    expect(DIAL_CX + R_NUMERAL).toBeLessThan(DIAL_VIEW_W);
    // The hub sits on the horizon near the foot of the box, not in the middle of it, and the box has to
    // clear its lower half: the root svg clips to its viewport, so a hub past the edge renders flattened.
    expect(DIAL_VIEW_H).toBeGreaterThanOrEqual(DIAL_CY + R_HUB);
    expect(DIAL_VIEW_H - DIAL_CY).toBeLessThan(DIAL_RADIUS);
  });
  it("ticks the minors between the majors, and numbers only the thresholds", () => {
    expect(DIAL_MAJOR_TICKS).toEqual([GREEN_TO, AMBER_TO]);
    expect(DIAL_MINOR_TICKS).not.toContain(GREEN_TO);
    expect(DIAL_MINOR_TICKS.every((m) => m > 1 && m < DIAL_MAX)).toBe(true);
    expect([...DIAL_MINOR_TICKS]).toEqual([...DIAL_MINOR_TICKS].sort((a, b) => a - b));
  });
});

describe("gridLines", () => {
  it("fans the verticals off the vanishing point and crowds the horizontals toward it", () => {
    const { verticals, horizontals } = gridLines();
    expect(verticals).not.toContain(GRID_VANISH_X);
    expect(verticals).toHaveLength(16);
    // Symmetric about the vanishing point, so the grid reads as one road.
    expect(verticals.map((x) => x - GRID_VANISH_X).reduce((a, b) => a + b, 0)).toBeCloseTo(0, 6);
    expect(horizontals.at(-1)).toBe(GRID_H);
    expect([...horizontals]).toEqual([...horizontals].sort((a, b) => a - b));
    // Each row is further from the last: the gaps open up as the grid comes toward the reader.
    const gaps = horizontals.map((y, i) => y - (horizontals[i - 1] ?? 0));
    expect(gaps.every((g, i) => i === 0 || g > gaps[i - 1])).toBe(true);
  });
});
