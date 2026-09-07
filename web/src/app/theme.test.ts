// The palette is a contract with the eye, so it is checked like any other
// contract: the tokens are read out of globals.css and their contrast ratios
// computed, rather than trusted to review.

import { readFileSync } from "node:fs";
import path from "node:path";
import { describe, expect, it } from "vitest";

const css = readFileSync(path.join(__dirname, "globals.css"), "utf8");
const components = path.join(__dirname, "..", "components");

/** The declarations of one selector block, as a token map. */
function tokens(selector: string): Record<string, string> {
  const at = css.indexOf(selector);
  expect(at, `${selector} is in globals.css`).toBeGreaterThanOrEqual(0);
  const open = css.indexOf("{", at);
  const close = css.indexOf("\n}", open);
  const body = css.slice(open + 1, close);
  const out: Record<string, string> = {};
  for (const line of body.split("\n")) {
    const match = /^\s*(--[a-z0-9-]+):\s*([^;]+);/i.exec(line);
    if (match) out[match[1]] = match[2].trim();
  }
  return out;
}

function channel(v: number): number {
  const s = v / 255;
  return s <= 0.04045 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
}

export function luminance(hex: string): number {
  const m = /^#([0-9a-f]{6})$/i.exec(hex.trim());
  if (!m) throw new Error(`not a six digit hex colour: ${hex}`);
  const n = Number.parseInt(m[1], 16);
  return 0.2126 * channel((n >> 16) & 255) + 0.7152 * channel((n >> 8) & 255) + 0.0722 * channel(n & 255);
}

export function contrast(a: string, b: string): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

const light = tokens(":root {");
const dark = tokens(':root[data-theme="dark"] {');
const darkMedia = tokens(':root:not([data-theme="light"]) {');

describe("palette", () => {
  it("keeps the two dark blocks identical, so the media query and the explicit choice cannot drift", () => {
    expect(darkMedia).toEqual(dark);
  });

  it("gives text a cyan that clears 4.5:1 on every surface it sits on, in both themes", () => {
    for (const palette of [light, dark]) {
      for (const surface of ["--page", "--surface", "--surface-2"]) {
        expect(contrast(palette["--accent-2-text"], palette[surface])).toBeGreaterThanOrEqual(4.5);
      }
    }
    // The graphical cyan stays what it was, and in the light theme it is the
    // one that does not clear normal text: that is why the split exists.
    expect(light["--accent-2"]).toBe("#0a86a0");
    expect(contrast(light["--accent-2"], light["--page"])).toBeLessThan(4.5);
    expect(contrast(light["--accent-2"], light["--page"])).toBeGreaterThanOrEqual(3);
  });

  it("ends the wordmark gradient on the text cyan, since 18px bold is normal text", () => {
    const chrome = light["--chrome-text"];
    expect(chrome).toContain(light["--accent-2-text"]);
    expect(chrome).not.toContain(light["--accent-2"]);
    for (const stop of chrome.match(/#[0-9a-f]{6}/gi) ?? []) {
      expect(contrast(stop, light["--page"])).toBeGreaterThanOrEqual(4.5);
    }
  });

  it("keeps the unknown-fee hatch above 3:1 on the chart surface in both themes", () => {
    // UNKNOWN_COLOR is var(--ink-3), drawn at full strength over --chart.
    for (const palette of [light, dark]) {
      expect(contrast(palette["--ink-3"], palette["--chart"])).toBeGreaterThanOrEqual(3);
    }
  });

  it("gives the wordmark a line box that holds the g's descender", () => {
    // Gradient text is painted through the text's own box, so a tight line
    // box cuts the descender off. The box has to be an inline-block with room
    // below the baseline, and nothing above it may clip.
    const at = css.indexOf("@utility vw-wordmark {");
    expect(at).toBeGreaterThanOrEqual(0);
    const rule = css.slice(at, css.indexOf("\n}", at));
    expect(rule).toContain("display: inline-block;");
    expect(rule).toContain("padding-bottom:");
    expect(rule).not.toContain("overflow: hidden");
    const lineHeight = /line-height:\s*([\d.]+)/.exec(rule);
    expect(lineHeight).not.toBeNull();
    expect(Number(lineHeight?.[1])).toBeGreaterThanOrEqual(1.2);
    // A utility class on the mark itself must not put the tight line box back.
    const header = readFileSync(path.join(components, "PageHeader.tsx"), "utf8");
    expect(header).toContain("vw-wordmark");
    expect(/vw-wordmark[^"]*leading-none/.test(header)).toBe(false);
  });

  it("never sets text in the graphical cyan", () => {
    const sources = readFileSync(path.join(components, "DataFooter.tsx"), "utf8") + readFileSync(path.join(components, "ThemeToggle.tsx"), "utf8");
    expect(sources).toContain("accent-2-text");
    expect(/text-accent-2(?!-text)/.test(sources)).toBe(false);
  });
});
