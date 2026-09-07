import { act } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { BlockPoint } from "@/types";
import {
  HERO_RANGE_KEY,
  HERO_RANGE_LABELS,
  HERO_RANGES,
  HERO_WINDOW_S,
  heroChartData,
  heroFeeAxis,
  heroFeeDomain,
  heroPointTitle,
  heroSpan,
  heroTickStep,
  heroTicks,
  heroTimeLabel,
  isHeroRange,
  niceStep,
  readHeroRange,
  setHeroRange,
  storeHeroRange,
} from "./hero";

const LAST_TS = 1_788_679_199;

/** `perSecond` blocks in each of the `seconds` seconds ending at `lastTs`, oldest first. */
function ring(seconds: number, perSecond: number, lastTs = LAST_TS, gasUsed = 4_000_000): BlockPoint[] {
  const out: BlockPoint[] = [];
  let n = 1;
  for (let ts = lastTs - seconds + 1; ts <= lastTs; ts++) {
    for (let k = 0; k < perSecond; k++) {
      out.push({ number: n++, ts, gasUsed, baseFee: "399726000", predictedBaseFee: "399726000", backlogs: [], constraintBips: [], exponentBips: 0, minBaseFee: "20000000", anchored: k === 0 });
    }
  }
  return out;
}

describe("heroChartData", () => {
  it("places every block of the window on a relative time axis, spreading a shared second across it", () => {
    const points = heroChartData(ring(3, 4), LAST_TS);
    expect(points).toHaveLength(12);
    // The oldest second starts three seconds back, and the four blocks that
    // share it are spread evenly across it rather than stacked on one tick.
    expect(points.slice(0, 4).map((p) => p.x)).toEqual([-3, -2.75, -2.5, -2.25]);
    expect(points[points.length - 1].x).toBe(-0.25);
    expect(points[0].fee).toBeCloseTo(0.399726, 6);
    expect(points[0].gasUsed).toBe(4_000_000);
    expect(points[0].number).toBe(1);
    expect(points[0].ts).toBe(LAST_TS - 2);
  });
  it("keeps only the blocks inside the window, and never one from the future", () => {
    const blocks = [...ring(200, 1), { ...ring(1, 1)[0], number: 999, ts: LAST_TS + 5 }];
    const points = heroChartData(blocks, LAST_TS);
    expect(points).toHaveLength(HERO_WINDOW_S);
    expect(points.some((p) => p.number === 999)).toBe(false);
    expect(points[0].x).toBe(-HERO_WINDOW_S);
  });
  it("has nothing to draw from an empty ring", () => {
    expect(heroChartData([], LAST_TS)).toEqual([]);
  });
});

describe("the relative time axis", () => {
  it("spans the two minute window when the ring reaches back that far, and the coverage when it does not", () => {
    expect(heroSpan(heroChartData(ring(200, 1), LAST_TS))).toBe(HERO_WINDOW_S);
    // A 23 second ring draws over 30 seconds, not against a mostly empty axis.
    expect(heroSpan(heroChartData(ring(23, 10), LAST_TS))).toBe(30);
    expect(heroSpan(heroChartData(ring(2, 10), LAST_TS))).toBe(10);
    expect(heroSpan([])).toBe(HERO_WINDOW_S);
  });
  it("keeps the axis to five ticks whatever the span", () => {
    expect(heroTickStep(120)).toBe(30);
    expect(heroTickStep(60)).toBe(15);
    expect(heroTickStep(30)).toBe(10);
    expect(heroTickStep(600)).toBe(60);
    expect(heroTicks(120)).toEqual([-120, -90, -60, -30, 0]);
    expect(heroTicks(60)).toEqual([-60, -45, -30, -15, 0]);
    expect(heroTicks(30)).toEqual([-30, -20, -10, 0]);
  });
  it("labels ticks as minutes and seconds back from now", () => {
    expect(heroTimeLabel(-120)).toBe("-2:00");
    expect(heroTimeLabel(-90)).toBe("-1:30");
    expect(heroTimeLabel(-45)).toBe("-0:45");
    expect(heroTimeLabel(-5)).toBe("-0:05");
    expect(heroTimeLabel(0)).toBe("now");
  });
  it("says where a hovered point sits in words", () => {
    expect(heroPointTitle(0)).toBe("now");
    expect(heroPointTitle(-0.75)).toBe("now");
    expect(heroPointTitle(-3.4)).toBe("3 s ago");
    expect(heroPointTitle(-118.5)).toBe("119 s ago");
  });
});

describe("heroFeeDomain", () => {
  it("holds the fees and the floor with a little padding, so neither sits on the frame", () => {
    const points = heroChartData(ring(2, 1), LAST_TS);
    const [min, max] = heroFeeDomain(points, 0.02);
    // The floor is the low end, the fee the high one, padded by a twentieth of the span.
    expect(min).toBeCloseTo(0.02 - (0.399726 - 0.02) / 20, 6);
    expect(max).toBeCloseTo(0.399726 + (0.399726 - 0.02) / 20, 6);
  });
  it("gives a flat series a band to draw in, and never asks for a negative fee", () => {
    const flat = heroChartData(ring(2, 1), LAST_TS).map((p) => ({ ...p, fee: 0.02 }));
    const [min, max] = heroFeeDomain(flat, 0.02);
    expect(min).toBeCloseTo(0.018, 6);
    expect(max).toBeCloseTo(0.022, 6);
    expect(heroFeeDomain([], 0)).toEqual([0, 1]);
    expect(heroFeeDomain([], Number.NaN)).toEqual([0, 1]);
    expect(heroFeeDomain(heroChartData(ring(2, 1), LAST_TS), 0)[0]).toBe(0);
  });
});

describe("heroFeeAxis", () => {
  it("snaps the band out to round ticks that all read at one width", () => {
    const points = heroChartData(ring(2, 1), LAST_TS);
    const axis = heroFeeAxis(points, 0.02);
    expect(axis.ticks).toEqual([0, 0.1, 0.2, 0.3, 0.4, 0.5]);
    expect(axis.domain).toEqual([0, 0.5]);
  });
  it("keeps a step the eye reads as round", () => {
    expect(niceStep(0.0895)).toBe(0.1);
    expect(niceStep(0.1)).toBe(0.1);
    expect(niceStep(0.11)).toBe(0.2);
    expect(niceStep(2.4)).toBe(2.5);
    expect(niceStep(6)).toBe(10);
    expect(niceStep(0)).toBe(1);
    expect(niceStep(Number.NaN)).toBe(1);
  });
  it("still has an axis before any block has arrived", () => {
    expect(heroFeeAxis([], 0)).toEqual({ domain: [0, 1], ticks: [0, 0.2, 0.4, 0.6, 0.8, 1] });
  });
});

describe("the hero range", () => {
  beforeEach(() => {
    window.localStorage.removeItem(HERO_RANGE_KEY);
  });

  it("offers the live view first and then every range the api serves", () => {
    expect(HERO_RANGES).toEqual(["live", "1h", "24h", "30d", "all"]);
    expect(HERO_RANGES.map((r) => HERO_RANGE_LABELS[r])).toEqual(["Live", "1h", "24h", "30d", "All"]);
  });

  it("recognises the live view and the api's ranges, and nothing else", () => {
    for (const range of HERO_RANGES) expect(isHeroRange(range)).toBe(true);
    expect(isHeroRange("7d")).toBe(false);
    expect(isHeroRange("")).toBe(false);
  });

  it("remembers the chosen range for the next visit", () => {
    expect(readHeroRange()).toBe("live");
    storeHeroRange("30d");
    expect(window.localStorage.getItem(HERO_RANGE_KEY)).toBe("30d");
    expect(readHeroRange()).toBe("30d");
  });

  it("falls back to the live view for anything it cannot use", () => {
    window.localStorage.setItem(HERO_RANGE_KEY, "yesterday");
    expect(readHeroRange()).toBe("live");
  });

  it("seeds itself from storage the first time it is read, then answers from memory", async () => {
    window.localStorage.setItem(HERO_RANGE_KEY, "30d");
    vi.resetModules();
    const fresh = await import("./hero");
    expect(fresh.heroRange()).toBe("30d");
    expect(fresh.heroRangeOnServer()).toBe("live");
    fresh.setHeroRange("1h");
    expect(fresh.heroRange()).toBe("1h");
    expect(window.localStorage.getItem(HERO_RANGE_KEY)).toBe("1h");
  });

  it("tells its subscribers about this tab's change and about another tab's", async () => {
    vi.resetModules();
    const fresh = await import("./hero");
    const seen: string[] = [];
    const stop = fresh.subscribeHeroRange(() => seen.push(fresh.heroRange()));
    fresh.setHeroRange("24h");
    expect(seen).toEqual(["24h"]);
    // Another tab stored a range: this page takes it on rather than drifting.
    window.localStorage.setItem(HERO_RANGE_KEY, "30d");
    act(() => {
      window.dispatchEvent(new StorageEvent("storage", { key: HERO_RANGE_KEY, newValue: "30d" }));
    });
    expect(seen).toEqual(["24h", "30d"]);
    expect(fresh.heroRange()).toBe("30d");
    // An unrelated key is not this store's business.
    act(() => {
      window.dispatchEvent(new StorageEvent("storage", { key: "gascurve:theme", newValue: "dark" }));
    });
    expect(seen).toEqual(["24h", "30d"]);
    stop();
    fresh.setHeroRange("live");
    expect(seen).toEqual(["24h", "30d"]);
  });

  it("still switches range in a browser that refuses storage, it just does not remember", () => {
    const storage = Object.getOwnPropertyDescriptor(window, "localStorage");
    Object.defineProperty(window, "localStorage", {
      configurable: true,
      get() {
        throw new Error("blocked");
      },
    });
    setHeroRange("all");
    expect(() => setHeroRange("all")).not.toThrow();
    if (storage) Object.defineProperty(window, "localStorage", storage);
    setHeroRange("live");
  });

  it("survives a browser that refuses storage, and simply does not remember", () => {
    const storage = Object.getOwnPropertyDescriptor(window, "localStorage");
    Object.defineProperty(window, "localStorage", {
      configurable: true,
      get() {
        throw new Error("blocked");
      },
    });
    expect(readHeroRange()).toBe("live");
    expect(() => storeHeroRange("24h")).not.toThrow();
    if (storage) Object.defineProperty(window, "localStorage", storage);
    expect(readHeroRange()).toBe("live");
  });
});
