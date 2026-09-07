import { act } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { assignPlaces, NO_PLACES } from "@/lib/smoothing";
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
  heroThroughputData,
  throughputAxis,
  throughputTick,
  stableFeeAxis,
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

/** The wall clock half a second into the newest block's second. */
const NOW_MS = (LAST_TS + 0.5) * 1000;

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

/** The ring's placement of `blocks`, as the frame store keeps it. */
function placed(blocks: readonly BlockPoint[]) {
  return assignPlaces(NO_PLACES, blocks);
}

/** The chart data of a ring placed in one go, the ordinary case for a caller with a fresh ring. */
function chart(blocks: readonly BlockPoint[], nowMs: number, seconds?: number) {
  return seconds === undefined ? heroChartData(blocks, placed(blocks), nowMs) : heroChartData(blocks, placed(blocks), nowMs, seconds);
}

/** The throughput data of a ring placed in one go. */
function throughput(blocks: readonly BlockPoint[], nowMs: number, seconds?: number) {
  return seconds === undefined ? heroThroughputData(blocks, placed(blocks), nowMs) : heroThroughputData(blocks, placed(blocks), nowMs, seconds);
}

describe("heroChartData", () => {
  it("places every block of the window on a clock-anchored axis, spreading a shared second across it", () => {
    const points = chart(ring(3, 4), NOW_MS);
    expect(points).toHaveLength(12);
    // The oldest second starts three seconds back and its four blocks are
    // spread evenly across it; the newest block is the right edge.
    expect(points.slice(0, 4).map((p) => p.x)).toEqual([-2.75, -2.5, -2.25, -2]);
    expect(points[points.length - 1].x).toBe(0);
    expect(points[0].fee).toBeCloseTo(0.399726, 6);
    expect(points[0].gasUsed).toBe(4_000_000);
    expect(points[0].number).toBe(1);
    expect(points[0].ts).toBe(LAST_TS - 2);
  });
  it("keeps only the blocks inside the window, measured from the clock", () => {
    const points = chart(ring(200, 1), NOW_MS);
    expect(points).toHaveLength(HERO_WINDOW_S);
    expect(points[0].x).toBe(-119.5);
    expect(points[points.length - 1].x).toBe(-0.5);
  });
  it("lets a block ahead of a slow browser clock set the edge instead of drawing past it", () => {
    const blocks = [...ring(2, 1), { ...ring(1, 1)[0], number: 999, ts: LAST_TS + 5 }];
    const points = chart(blocks, NOW_MS);
    expect(points[points.length - 1]).toMatchObject({ number: 999, x: 0 });
    expect(points[0].x).toBe(-6);
  });
  it("leaves the earlier points where they were when another block lands in the newest second", () => {
    const blocks = ring(2, 4);
    const before = chart(blocks.slice(0, 6), NOW_MS);
    const after = chart(blocks.slice(0, 7), NOW_MS);
    expect(after.slice(0, 6).map((p) => p.x)).toEqual(before.map((p) => p.x));
    expect(after[6].x).toBeGreaterThan(after[5].x);
  });
  it("slides with the clock", () => {
    // A clock already past the newest block, so the edge is the clock both times.
    const now = chart(ring(2, 4), NOW_MS + 500);
    const later = chart(ring(2, 4), NOW_MS + 1500);
    later.forEach((p, i) => expect(p.x).toBeCloseTo(now[i].x - 1, 9));
  });
  it("has nothing to draw from an empty ring", () => {
    expect(chart([], NOW_MS)).toEqual([]);
  });
});

describe("the relative time axis", () => {
  it("always spans the whole window, so a filling ring never rescales the axis", () => {
    expect(heroSpan()).toBe(HERO_WINDOW_S);
    expect(heroSpan(30)).toBe(30);
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
    const points = chart(ring(2, 1), NOW_MS);
    const [min, max] = heroFeeDomain(points, 0.02);
    // The floor is the low end, the fee the high one, padded by a twentieth of the span.
    expect(min).toBeCloseTo(0.02 - (0.399726 - 0.02) / 20, 6);
    expect(max).toBeCloseTo(0.399726 + (0.399726 - 0.02) / 20, 6);
  });
  it("gives a flat series a band to draw in, and never asks for a negative fee", () => {
    const flat = chart(ring(2, 1), NOW_MS).map((p) => ({ ...p, fee: 0.02 }));
    const [min, max] = heroFeeDomain(flat, 0.02);
    expect(min).toBeCloseTo(0.018, 6);
    expect(max).toBeCloseTo(0.022, 6);
    expect(heroFeeDomain([], 0)).toEqual([0, 1]);
    expect(heroFeeDomain([], Number.NaN)).toEqual([0, 1]);
    expect(heroFeeDomain(chart(ring(2, 1), NOW_MS), 0)[0]).toBe(0);
  });
});

describe("heroFeeAxis", () => {
  it("snaps the band out to round ticks that all read at one width", () => {
    const points = chart(ring(2, 1), NOW_MS);
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

describe("stableFeeAxis", () => {
  const at = (fee: number) => chart(ring(2, 1), NOW_MS).map((p) => ({ ...p, fee }));
  it("starts from the fresh axis", () => {
    expect(stableFeeAxis(null, at(0.4), 0.02)).toEqual(heroFeeAxis(at(0.4), 0.02));
  });
  it("keeps the previous axis while the data still fits it and covers at least half of it", () => {
    const previous = heroFeeAxis(at(0.4), 0.02);
    expect(previous.domain).toEqual([0, 0.5]);
    expect(stableFeeAxis(previous, at(0.45), 0.02)).toBe(previous);
    expect(stableFeeAxis(previous, at(0.3), 0.02)).toBe(previous);
  });
  it("rescales when a point leaves the axis", () => {
    const previous = heroFeeAxis(at(0.4), 0.02);
    const next = stableFeeAxis(previous, at(0.6), 0.02);
    expect(next).not.toBe(previous);
    expect(next.domain[1]).toBeGreaterThanOrEqual(0.6);
  });
  it("hands back the previous axis when the fresh one is the same, so a renderer settles", () => {
    const previous = heroFeeAxis([], 0.02);
    expect(stableFeeAxis(previous, [], 0.02)).toBe(previous);
  });
  it("rescales when the data has shrunk to less than half of the axis", () => {
    const previous = heroFeeAxis(at(0.8), 0.02);
    const next = stableFeeAxis(previous, at(0.1), 0.02);
    expect(next).not.toBe(previous);
    expect(next.domain[1]).toBeLessThan(previous.domain[1]);
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

describe("the live throughput series", () => {
  it("sums each whole second's blocks and places the second at its own end", () => {
    // Three blocks a second for five seconds, 4 Mgas each: 12 Mgas a second.
    const points = throughput(ring(5, 3), NOW_MS);
    // The seconds at either end of the ring are partial and are left out.
    expect(points.map((p) => p.ts)).toEqual([LAST_TS - 3, LAST_TS - 2, LAST_TS - 1]);
    expect(points.map((p) => p.gas)).toEqual([12_000_000, 12_000_000, 12_000_000]);
    expect(points.map((p) => p.blocks)).toEqual([3, 3, 3]);
    // Placed at the end of its own second, and the right edge is the newest
    // block's place when the clock is behind it, so the newest whole second
    // sits two thirds of a second inside the edge.
    expect(points[points.length - 1].x).toBeCloseTo(-2 / 3, 5);
    expect(points[0].x).toBeCloseTo(-2 - 2 / 3, 5);
  });

  it("leaves out the second that is still being delivered, however far behind the clock is", () => {
    // The newest second has three of its ten blocks so far, and the clock has
    // already moved past it: without the rule it would read as a chain that
    // stopped carrying gas.
    const partial = [...ring(4, 10, LAST_TS - 1), ...ring(1, 3, LAST_TS)];
    const points = throughput(partial, (LAST_TS + 2) * 1000);
    expect(points.map((p) => p.ts)).toEqual([LAST_TS - 3, LAST_TS - 2, LAST_TS - 1]);
    expect(points.every((p) => p.blocks === 10)).toBe(true);
  });

  it("draws a quiet second as zero rather than letting the line carry over it", () => {
    // Blocks in every second but one. The ring is continuous across the quiet
    // one (consecutive block numbers), so the chain really did carry nothing
    // through it, and an area that interpolated across it said otherwise.
    // Renumbered, because a second the chain produced no block in leaves no
    // hole in the block numbers either.
    const blocks = ring(5, 2)
      .filter((b) => b.ts !== LAST_TS - 2)
      .map((b, i) => ({ ...b, number: i + 1 }));
    const points = throughput(blocks, NOW_MS);
    expect(points.map((p) => p.ts)).toEqual([LAST_TS - 3, LAST_TS - 2, LAST_TS - 1]);
    expect(points.map((p) => p.gas)).toEqual([8_000_000, 0, 8_000_000]);
    expect(points.map((p) => p.blocks)).toEqual([2, 0, 2]);
  });

  it("draws a hole in the ring itself as a break, because nobody measured that second", () => {
    // The same shape, but the block numbers jump across the empty second: the
    // blocks of that second are missing from the ring rather than absent from
    // the chain, and zero would be a claim nobody can make.
    // Here the block numbers keep the two the ring never received, so the jump
    // says the blocks are missing rather than that the chain was idle.
    const gapped = ring(5, 2).filter((b) => b.ts !== LAST_TS - 2);
    const points = throughput(gapped, NOW_MS);
    expect(points.map((p) => p.ts)).toEqual([LAST_TS - 3, LAST_TS - 2, LAST_TS - 1]);
    expect(points.map((p) => p.gas)).toEqual([8_000_000, null, 8_000_000]);
    expect(points.map((p) => p.blocks)).toEqual([2, null, 2]);
  });

  it("reaches back over the window and no further, and has nothing to say about an empty ring", () => {
    expect(throughput([], NOW_MS)).toEqual([]);
    const long = throughput(ring(HERO_WINDOW_S + 30, 1), NOW_MS);
    expect(long).toHaveLength(HERO_WINDOW_S);
    expect(long[0].x).toBeGreaterThanOrEqual(-HERO_WINDOW_S);
    // A shorter window keeps only what fits in it.
    expect(throughput(ring(20, 1), NOW_MS, 5)).toHaveLength(5);
    // A ring with nothing whole in it draws nothing rather than a fraction.
    expect(throughput(ring(2, 3), NOW_MS)).toEqual([]);
  });
});

describe("the throughput axis", () => {
  it("puts a round top above the peak and carries one unit for every label", () => {
    const axis = throughputAxis(41_000_000);
    expect(axis.top).toBe(50_000_000);
    expect(axis.ticks).toEqual([0, 25_000_000, 50_000_000]);
    expect(axis.unit).toBe("Mgas/s");
    expect(axis.ticks.map((t) => throughputTick(t, axis))).toEqual(["0.0", "25.0", "50.0"]);
  });

  it("takes its unit from its own top, so a busy chain reads in Ggas/s and a quiet one in gas/s", () => {
    const big = throughputAxis(1_400_000_000);
    expect(big.unit).toBe("Ggas/s");
    expect(big.ticks.map((t) => throughputTick(t, big))).toEqual(["0.0", "1.0", "2.0"]);
    const small = throughputAxis(800);
    expect(small.unit).toBe("gas/s");
    expect(small.ticks.map((t) => throughputTick(t, small))).toEqual(["0", "500", "1,000"]);
    // Above a hundred in the band the decimal buys nothing.
    const wide = throughputAxis(250_000_000);
    expect(wide.top).toBe(400_000_000);
    expect(wide.ticks.map((t) => throughputTick(t, wide))).toEqual(["0", "200", "400"]);
  });

  it("still draws a band for a chain that carried nothing at all", () => {
    expect(throughputAxis(0).top).toBeGreaterThan(0);
    expect(throughputAxis(Number.NaN).top).toBeGreaterThan(0);
  });
});
