import { describe, expect, it } from "vitest";
import { CARD_DIAL_H, CARD_DIAL_W, CARD_TONE_COLORS, FEE_SIZES, FEE_UNIT_W, MAX_FIGURE_CHARS, MULTIPLIER_SIZES, MULTIPLIER_UNIT_W, OFF_SCALE, cardDialSvg, cardNumerals, cardReading, figureFits, readoutSize, svgDataUri } from "@/lib/card";
import { AMBER_TO, DIAL_CY, DIAL_MAX, DIAL_VIEW_H, DIAL_VIEW_W, GREEN_TO, dialPoint, dialPosition, needlePoints, R_NUMERAL } from "@/lib/dial";
import type { LiveSnapshot } from "@/types";

/** The tones lit inside the glow group, in the order they are drawn. The dimmed ring outside it carries
 * every tone whatever the reading, so counting colours across the whole document proves nothing. */
function litToneNames(svg: string): string[] {
  const group = /<g filter="url\(#neon\)">(.*?)<\/g>/.exec(svg);
  if (group === null) return [];
  return (["good", "warning", "critical"] as const).filter((tone) => group[1].includes(CARD_TONE_COLORS[tone]));
}

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

  it("points the needle at the reading", () => {
    const needle = needlePoints(1.5)
      .map((p) => `${p.x.toFixed(2)},${p.y.toFixed(2)}`)
      .join(" ");
    expect(cardDialSvg(1.5)).toContain(`<polygon points="${needle}"`);
  });

  it.each([
    [0.5, []],
    [1.5, ["good"]],
    [GREEN_TO, ["good"]],
    [GREEN_TO + 0.01, ["good", "warning"]],
    [AMBER_TO, ["good", "warning"]],
    [AMBER_TO + 1, ["good", "warning", "critical"]],
    [DIAL_MAX * 10, ["good", "warning", "critical"]],
    [Number.POSITIVE_INFINITY, ["good", "warning", "critical"]],
    [Number.NaN, []],
  ])("lights exactly the bands %p has passed", (multiplier, expected) => {
    expect(litToneNames(cardDialSvg(multiplier))).toEqual(expected);
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
    expect(litToneNames(svg)).toEqual([]);
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

describe("readout sizing", () => {
  it.each([
    ["fee", FEE_SIZES, FEE_UNIT_W],
    ["multiplier", MULTIPLIER_SIZES, MULTIPLIER_UNIT_W],
  ])("sets every %s step at a size its longest figure fits", (_name, steps, unitW) => {
    for (const [length, size] of steps) expect(figureFits(length, size, unitW)).toBe(true);
  });

  it.each([
    ["fee", FEE_SIZES, MAX_FIGURE_CHARS],
    // The multiplier's own figure is two characters shorter than the fee's, and carries a sign.
    ["multiplier", MULTIPLIER_SIZES, MAX_FIGURE_CHARS - 4],
  ])("has a %s step for the longest figure it can be handed", (_name, steps, longest) => {
    expect(steps[steps.length - 1][0]).toBeGreaterThanOrEqual(longest);
  });

  it("falls back to the smallest step for a figure longer than any of them", () => {
    // The off scale cutoff means nothing that long reaches here, so this is what happens if it ever does.
    expect(readoutSize("x".repeat(MAX_FIGURE_CHARS + 10), FEE_SIZES)).toBe(FEE_SIZES[FEE_SIZES.length - 1][1]);
  });

  it("keeps the everyday fee at full size and steps down only as the figure grows", () => {
    expect(readoutSize("0.4090", FEE_SIZES)).toBe(96);
    expect(readoutSize("9,007,199,254,740,991.0", FEE_SIZES)).toBe(32);
    expect(readoutSize("1.00×", MULTIPLIER_SIZES)).toBe(44);
    expect(readoutSize(OFF_SCALE, MULTIPLIER_SIZES)).toBe(34);
  });
});

describe("cardReading", () => {
  it("formats every figure the card prints", () => {
    expect(cardReading(snapshot)).toEqual({
      baseFee: "0.4090",
      multiplier: 20.45,
      multiplierText: "20.45",
      offScale: false,
      feeOffScale: false,
      tone: "critical",
      floor: "0.02",
      block: "62,912,450",
    });
  });

  it("says off scale past 2^53, where the digits are no longer the ones the api sent", () => {
    // The api's own int64 ceiling lands here: 9223372036854775807 parses back as 9223372036854776000.
    const saturated = { ...snapshot, multiplierBips: 9_223_372_036_854_775_807 } as LiveSnapshot;
    expect(cardReading(saturated)).toMatchObject({ multiplierText: OFF_SCALE, offScale: true, tone: "critical" });
    const under = { ...snapshot, multiplierBips: Number.MAX_SAFE_INTEGER } as LiveSnapshot;
    expect(cardReading(under)).toMatchObject({ multiplierText: "900,719,925,474.10", offScale: false });
  });

  it("says off scale for a fee whose digits a double no longer carries", () => {
    const huge = { ...snapshot, baseFee: "9007199254740992000000000" } as LiveSnapshot;
    expect(cardReading(huge)).toMatchObject({ baseFee: OFF_SCALE, feeOffScale: true });
    // Just under the cutoff still prints, and it is the longest figure the fee line is sized for.
    const edge = { ...snapshot, baseFee: "9007199254740991000000000" } as LiveSnapshot;
    const printed = cardReading(edge);
    expect(printed?.feeOffScale).toBe(false);
    expect(printed?.baseFee.length).toBeLessThanOrEqual(MAX_FIGURE_CHARS);
  });

  it("has no reading at all for a figure the pricer cannot produce, rather than a red n/a", () => {
    // A fee cannot sit below its own floor, so a negative multiplier is a broken reading, not a low one:
    // drawn as a figure it would be a green needle pinned left under a minus sign.
    for (const bips of [Number.NaN, Number.POSITIVE_INFINITY, Number.NEGATIVE_INFINITY, -1, -50_000]) {
      expect(cardReading({ ...snapshot, multiplierBips: bips } as LiveSnapshot)).toBeNull();
    }
  });

  it("bands a figure past a thousand, where its printed form carries a separator", () => {
    const thousand = { ...snapshot, multiplierBips: 12_340_000 } as LiveSnapshot;
    expect(cardReading(thousand)).toMatchObject({ multiplierText: "1,234.00", tone: "critical" });
  });

  it("takes the tone from the figure as printed, so a rounded reading matches its band", () => {
    // 2.0049x prints as 2.00x, which is the top of the good band; the raw multiplier is above it.
    const rounded = { ...snapshot, multiplierBips: 20_049 } as LiveSnapshot;
    expect(cardReading(rounded)).toMatchObject({ multiplierText: "2.00", tone: "good" });
  });
});
