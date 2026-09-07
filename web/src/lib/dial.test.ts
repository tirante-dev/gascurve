import { describe, expect, it } from "vitest";
import { AMBER_TO, BANDS_LINE, DIAL_BANDS, DIAL_CX, DIAL_CY, DIAL_MAX, DIAL_RADIUS, dialArc, dialPoint, dialPosition, dialTone, GREEN_TO, TONE_SENTENCE } from "./dial";

describe("dialTone", () => {
  it("is green to twice the floor, amber to ten times, red above", () => {
    expect(dialTone(1)).toBe("good");
    expect(dialTone(GREEN_TO)).toBe("good");
    expect(dialTone(2.01)).toBe("warning");
    expect(dialTone(AMBER_TO)).toBe("warning");
    expect(dialTone(10.01)).toBe("critical");
    expect(dialTone(1000)).toBe("critical");
    expect(BANDS_LINE).toBe("green to 2× the floor, amber to 10×, red above");
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
