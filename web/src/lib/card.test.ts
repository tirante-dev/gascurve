import { describe, expect, it } from "vitest";
import { CARD_DIAL_H, CARD_DIAL_W, CARD_TONE_COLORS, cardDialSvg, cardNumerals, cardReading, svgDataUri } from "@/lib/card";
import { AMBER_TO, DIAL_CY, DIAL_VIEW_H, DIAL_VIEW_W, GREEN_TO, dialPoint, dialPosition, needlePoints, R_NUMERAL } from "@/lib/dial";
import type { LiveSnapshot } from "@/types";

const snapshot = {
  block: { number: 62_912_450, ts: 0, gasUsed: 0, baseFee: "409000000", txCount: 0 },
  baseFee: "409000000",
  minBaseFee: "20000000",
  multiplierBips: 204_500,
} as unknown as LiveSnapshot;

describe("cardDialSvg", () => {
  it("is a standalone document at the dial's own viewBox, blown up to the card's size", () => {
    const svg = cardDialSvg(1.5);
    expect(svg.startsWith(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${DIAL_VIEW_W} ${DIAL_VIEW_H}"`)).toBe(true);
    expect(svg).toContain(`width="${CARD_DIAL_W}" height="${CARD_DIAL_H}"`);
    expect(svg.endsWith("</svg>")).toBe(true);
  });

  it("lights the ring up to the reading and points the needle at it", () => {
    const svg = cardDialSvg(1.5);
    const needle = needlePoints(1.5)
      .map((p) => `${p.x.toFixed(2)},${p.y.toFixed(2)}`)
      .join(" ");
    expect(svg).toContain(`<polygon points="${needle}"`);
    // Only the good band is lit at 1.5x: the two above it appear once each, dimmed.
    expect(svg.split(CARD_TONE_COLORS.good).length - 1).toBeGreaterThan(svg.split(CARD_TONE_COLORS.critical).length - 1);
  });

  it("lights every band the fee has passed", () => {
    const svg = cardDialSvg(AMBER_TO + 1);
    for (const tone of ["good", "warning", "critical"] as const) {
      // Each band is drawn dimmed, and again lit inside the glow.
      expect(svg.split(CARD_TONE_COLORS[tone]).length - 1).toBe(2);
    }
  });

  it("measures the glow in user space, where the horizon's flat box cannot collapse it", () => {
    const svg = cardDialSvg(3);
    for (const filter of ["neon", "soft"]) {
      // A region in box relative units is zero high for the horizon, a horizontal line, and takes it with it.
      expect(svg).toMatch(new RegExp(`<filter id="${filter}" filterUnits="userSpaceOnUse" x="-?\\d+" y="-?\\d+" width="\\d+" height="\\d+"`));
      expect(svg).toContain(`filter="url(#${filter})"`);
    }
    // The horizon is one of the elements that carries the softer halo.
    expect(svg).toContain(`<g filter="url(#soft)"><line x1="6.00" y1="${DIAL_CY}.00"`);
  });

  it("draws the instrument unlit and without a needle when there is no reading", () => {
    const svg = cardDialSvg(null);
    expect(svg).not.toContain("<polygon");
    for (const tone of ["good", "warning", "critical"] as const) {
      expect(svg.split(CARD_TONE_COLORS[tone]).length - 1).toBe(1);
    }
  });

  it("carries no text, which the card's rasteriser would draw in the wrong face", () => {
    expect(cardDialSvg(3)).not.toContain("<text");
  });
});

describe("cardNumerals", () => {
  it("places the two ring numerals on the numeral ring, scaled to the card", () => {
    const numerals = cardNumerals(2);
    expect(numerals.map((n) => n.text)).toEqual(["2×", "10×"]);
    const first = dialPoint(dialPosition(GREEN_TO), R_NUMERAL);
    expect(numerals[0]).toEqual({ text: "2×", x: first.x * 2, y: first.y * 2 });
  });
});

describe("svgDataUri", () => {
  it("round trips the document a renderer will be handed", () => {
    const svg = cardDialSvg(2);
    const uri = svgDataUri(svg);
    expect(uri.startsWith("data:image/svg+xml;base64,")).toBe(true);
    expect(Buffer.from(uri.slice("data:image/svg+xml;base64,".length), "base64").toString("utf8")).toBe(svg);
  });
});

describe("cardReading", () => {
  it("formats every figure the card prints", () => {
    expect(cardReading(snapshot)).toEqual({
      baseFee: "0.4090",
      multiplier: 20.45,
      multiplierText: "20.45",
      tone: "critical",
      floor: "0.02",
      block: "62,912,450",
    });
  });

  it("takes the tone from the figure as printed, so a rounded reading matches its band", () => {
    // 2.0049x prints as 2.00x, which is the top of the good band; the raw multiplier is above it.
    const rounded = { ...snapshot, multiplierBips: 20_049 } as LiveSnapshot;
    expect(cardReading(rounded)).toMatchObject({ multiplierText: "2.00", tone: "good" });
  });
});
