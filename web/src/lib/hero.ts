// What the live hero draws: the last two minutes of per-block base fee from
// the smoothing store's block ring, and which range its chart is set to. Kept
// pure so the chart component is only a renderer and the axis, the domain and
// the range control can be tested without a DOM.

import { isSeriesRange, SERIES_RANGES } from "@/lib/api/series";
import type { BlockPoint, SeriesRange } from "@/types";
import { weiToGweiNumber } from "@/utils/format";

/** The span the hero reaches back over, in seconds of block timestamp. */
export const HERO_WINDOW_S = 120;

/** Tick spacings the relative-time axis may use, smallest first. */
export const HERO_TICK_STEPS = [10, 15, 30, 60];

/** Most ticks the axis carries, the newest one included. */
const HERO_MAX_TICKS = 5;

/**
 * One block on the hero chart. `x` is seconds before the end of the newest
 * block's second, so 0 is now and the axis runs negative to the left; blocks
 * that share a timestamp are spread evenly across the second they belong to,
 * as the constraint sawtooth does, so a burst keeps its shape instead of
 * stacking on one tick.
 */
export type HeroPoint = { x: number; number: number; ts: number; fee: number; gasUsed: number };

/**
 * The per-block base fee of the blocks within `seconds` of `lastTs`, oldest
 * first, in gwei. Blocks outside the window (and any that claim a timestamp
 * above the newest one) are left out.
 */
export function heroChartData(blocks: readonly BlockPoint[], lastTs: number, seconds = HERO_WINDOW_S): HeroPoint[] {
  const from = lastTs - seconds + 1;
  const inWindow = blocks.filter((b) => b.ts >= from && b.ts <= lastTs);
  const counts = new Map<number, number>();
  for (const b of inWindow) counts.set(b.ts, (counts.get(b.ts) ?? 0) + 1);
  const placed = new Map<number, number>();
  return inWindow.map((b) => {
    const k = placed.get(b.ts) ?? 0;
    placed.set(b.ts, k + 1);
    const n = counts.get(b.ts) ?? 1;
    return { x: b.ts - lastTs - 1 + k / n, number: b.number, ts: b.ts, fee: weiToGweiNumber(b.baseFee), gasUsed: b.gasUsed };
  });
}

/**
 * The span the axis covers: the two-minute window when the ring reaches that
 * far back, otherwise the coverage rounded up to a whole ten seconds, so a
 * short ring draws over the width it has rather than against a mostly empty
 * axis. Never below ten seconds, never above the window.
 */
export function heroSpan(points: readonly HeroPoint[], seconds = HERO_WINDOW_S): number {
  if (points.length === 0) return seconds;
  const oldest = Math.abs(points[0].x);
  return Math.min(seconds, Math.max(10, Math.ceil(oldest / 10) * 10));
}

/** The tick spacing for a span: the smallest step that keeps the axis to five ticks. */
export function heroTickStep(span: number): number {
  for (const step of HERO_TICK_STEPS) {
    if (span / step <= HERO_MAX_TICKS - 1) return step;
  }
  return HERO_TICK_STEPS[HERO_TICK_STEPS.length - 1];
}

/** Ticks from minus the span up to now, evenly spaced, oldest first. */
export function heroTicks(span: number): number[] {
  const step = heroTickStep(span);
  const out: number[] = [];
  for (let t = -Math.round(span / step) * step; t < 0; t += step) out.push(t);
  out.push(0);
  return out;
}

/** A point on the relative-time axis: "-2:00", "-0:30", and "now" at the right edge. */
export function heroTimeLabel(x: number): string {
  if (x >= 0) return "now";
  const total = Math.round(Math.abs(x));
  const minutes = Math.floor(total / 60);
  const seconds = total % 60;
  return `-${minutes}:${seconds.toString().padStart(2, "0")}`;
}

/** What the tooltip's heading says for a hovered point: where it sits on the axis, in words. */
export function heroPointTitle(x: number): string {
  if (x >= -1) return "now";
  return `${Math.round(Math.abs(x))} s ago`;
}

/**
 * The y domain in gwei: the fees and the floor line together, padded by a
 * twentieth so neither the peak nor the floor sits on the frame. A flat
 * series (every block at the floor) still gets a band to draw in.
 */
export function heroFeeDomain(points: readonly HeroPoint[], floorGwei: number): [number, number] {
  const values = points.map((p) => p.fee);
  const floor = Number.isFinite(floorGwei) ? floorGwei : 0;
  const min = Math.min(floor, ...values);
  const max = Math.max(floor, ...values);
  if (!Number.isFinite(min) || !Number.isFinite(max)) return [0, 1];
  if (min === max) return [Math.max(0, min * 0.9), max === 0 ? 1 : max * 1.1];
  const pad = (max - min) / 20;
  return [Math.max(0, min - pad), max + pad];
}

/**
 * A step the eye reads as round: one, two, two and a half or five times a
 * power of ten, at or above `raw`.
 */
export function niceStep(raw: number): number {
  if (!Number.isFinite(raw) || raw <= 0) return 1;
  const base = 10 ** Math.floor(Math.log10(raw));
  for (const m of [1, 2, 2.5, 5]) {
    if (raw <= m * base) return m * base;
  }
  return 10 * base;
}

/**
 * The gwei axis: the padded band snapped out to round ticks, so the labels
 * read as figures rather than as whatever the extremes happened to be, and
 * every one of them carries the same decimal count and so the same width.
 */
export function heroFeeAxis(points: readonly HeroPoint[], floorGwei: number): { domain: [number, number]; ticks: number[] } {
  const [lo, hi] = heroFeeDomain(points, floorGwei);
  const step = niceStep((hi - lo) / 5);
  const min = Math.max(0, Math.floor(lo / step) * step);
  const max = Math.ceil(hi / step) * step;
  const ticks: number[] = [];
  for (let i = 0; min + i * step <= max + step / 2; i++) ticks.push(Number((min + i * step).toPrecision(12)));
  return { domain: [ticks[0], ticks[ticks.length - 1]], ticks };
}

/**
 * The range the hero's chart draws: the live block ring, or one of the ranges
 * the api serves buckets for. Live is the default and the fallback.
 */
export type HeroRange = "live" | SeriesRange;

/** The hero's range control, in order: the live view first, then the api's ranges. */
export const HERO_RANGES: readonly HeroRange[] = ["live", ...SERIES_RANGES];

/** The word on each range tab. */
export const HERO_RANGE_LABELS: Record<HeroRange, string> = { live: "Live", "1h": "1h", "24h": "24h", "30d": "30d", all: "All" };

/** Where the chosen range is kept, so a reload comes back to the same view. */
export const HERO_RANGE_KEY = "gascurve:hero-range";

export function isHeroRange(value: string): value is HeroRange {
  return value === "live" || isSeriesRange(value);
}

/**
 * The range this browser last chose, or Live when it chose none, stored
 * something that is not a range, or has no storage to read at all.
 */
export function readHeroRange(): HeroRange {
  try {
    const stored = window.localStorage.getItem(HERO_RANGE_KEY);
    return stored !== null && isHeroRange(stored) ? stored : "live";
  } catch {
    return "live";
  }
}

/** Remembers the chosen range. A browser that refuses storage simply does not remember it. */
export function storeHeroRange(range: HeroRange): void {
  try {
    window.localStorage.setItem(HERO_RANGE_KEY, range);
  } catch {
    // Private mode or a blocked origin: the choice lasts for this page only.
  }
}

const listeners = new Set<() => void>();

/**
 * The range this tab is on. Storage seeds it the first time it is read and
 * remembers every change, but the value itself lives in memory, so a browser
 * that refuses storage still switches ranges (it just forgets them).
 */
let current: HeroRange | null = null;

export function heroRange(): HeroRange {
  if (current === null) current = readHeroRange();
  return current;
}

/** Live, always: what the server renders, and what hydration starts from. */
export function heroRangeOnServer(): HeroRange {
  return "live";
}

/** Sets the range for this tab and remembers it for the next visit. */
export function setHeroRange(range: HeroRange): void {
  current = range;
  storeHeroRange(range);
  for (const listener of listeners) listener();
}

/**
 * Subscribes to the range, this tab's changes and another tab's alike: a
 * choice made next door reaches this page as a storage event, and the two
 * windows agree rather than drifting apart.
 */
export function subscribeHeroRange(onChange: () => void): () => void {
  listeners.add(onChange);
  const onStorage = (event: StorageEvent) => {
    if (event.key !== null && event.key !== HERO_RANGE_KEY) return;
    current = readHeroRange();
    onChange();
  };
  window.addEventListener("storage", onStorage);
  return () => {
    listeners.delete(onChange);
    window.removeEventListener("storage", onStorage);
  };
}
