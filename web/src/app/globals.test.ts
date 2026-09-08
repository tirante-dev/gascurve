import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

// Vitest runs from web/, and the sheet is read rather than imported: the
// question is what the source declares, not what a bundler makes of it.
const css = readFileSync("src/app/globals.css", "utf8");

function token(name: string): string {
  const found = new RegExp(`--${name}:\\s*([^;]+);`).exec(css);
  expect(found, `--${name} is declared`).not.toBeNull();
  return found![1].trim();
}

describe("the page's own width", () => {
  /**
   * The hero sizes its chart column against the rail beside it, which stops
   * growing when the page does. Two ways that comes apart, both of which the
   * layout absorbs silently: a step at a different width from the cap, and a
   * breakpoint in px, which Tailwind emits ahead of the rem ones it ships with,
   * so the variant loses the cascade to lg wherever the two apply.
   */
  it("caps and steps at the same measure, in rem", () => {
    expect(token("breakpoint-page")).toBe(token("container-page"));
    expect(token("breakpoint-page")).toMatch(/^[\d.]+rem$/);
  });
});

describe("a stat panel", () => {
  // What the panel measures is not a theme's to choose: the rail's height is
  // what the chart column is drawn to, so a theme that pads its readouts
  // differently sizes the hero differently.
  it("takes its padding from the utility and not a theme token", () => {
    expect(css).toMatch(/@utility vw-stat-panel \{\s*padding: [^;]+;/);
    expect(css).not.toContain("--readout-pad");
  });
});
