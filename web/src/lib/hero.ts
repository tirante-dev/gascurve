// What the live hero draws: the last two minutes of per-block base fee from
// the smoothing store's block ring against the wall clock, and which range its chart is set to. Kept
// pure so the chart component is only a renderer and the axis, the domain and
// the range control can be tested without a DOM.

import { isSeriesRange, SERIES_RANGES } from "@/lib/api/series";
import { liveNow, placeOf, type BlockPlaces } from "@/lib/smoothing";
import type { BlockPoint, SeriesRange } from "@/types";
import { gasScale, weiToGweiNumber } from "@/utils/format";

/** The span the hero reaches back over, in seconds of block timestamp. */
export const HERO_WINDOW_S = 120;

/** Tick spacings the relative-time axis may use, smallest first. */
export const HERO_TICK_STEPS = [10, 15, 30, 60];

/** Most ticks the axis carries, the newest one included. */
const HERO_MAX_TICKS = 5;

/**
 * One block on the hero chart. `x` is seconds before now (the right edge), so
 * the axis runs negative to the left. Blocks that share a timestamp are spread
 * across the second they belong to by the ring's placement, so a burst keeps
 * its shape instead of stacking on one tick, and a block never moves once
 * placed.
 */
export type HeroPoint = { x: number; number: number; ts: number; fee: number; gasUsed: number };

/**
 * The per-block base fee of the blocks within `seconds` of the wall clock
 * `nowMs`, oldest first, in gwei. `places` is the ring's own placement, which
 * the frame store keeps by block number, so a block's place is decided once
 * and this chart and the throughput chart under it draw it in the same spot.
 * The axis is anchored to the clock, not to the newest block's whole-second
 * timestamp, so the chart slides continuously instead of stepping once a
 * second; a block sits left of the edge by its real age, which is also what
 * the freshness pill reports.
 */
export function heroChartData(blocks: readonly BlockPoint[], places: BlockPlaces, nowMs: number, seconds = HERO_WINDOW_S): HeroPoint[] {
  if (blocks.length === 0) return [];
  const now = liveNow(nowMs, placeOf(places, blocks[blocks.length - 1]));
  const out: HeroPoint[] = [];
  for (const b of blocks) {
    const x = placeOf(places, b) - now;
    if (x < -seconds) continue;
    out.push({ x, number: b.number, ts: b.ts, fee: weiToGweiNumber(b.baseFee), gasUsed: b.gasUsed });
  }
  return out;
}

/**
 * The span the axis covers: always the whole window. A ring that does not
 * yet reach back that far draws over the right part of the axis and fills in
 * as blocks arrive; an axis that grew with the coverage rescaled the whole
 * chart every time the oldest block crossed a tick, which read as the chart
 * jumping back and forth.
 */
export function heroSpan(seconds = HERO_WINDOW_S): number {
  return seconds;
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

export type FeeAxis = ReturnType<typeof heroFeeAxis>;

/**
 * The axis to draw this frame, with hysteresis: the previous axis stands while
 * the padded band of the data still fits inside it and still covers at least
 * half of it. Otherwise the axis is recomputed. Without this the domain
 * snapped to a new set of ticks on small moves of the fee, and the whole plot
 * rescaled under the reader several times a minute.
 */
export function stableFeeAxis(previous: FeeAxis | null, points: readonly HeroPoint[], floorGwei: number): FeeAxis {
  if (previous === null) return heroFeeAxis(points, floorGwei);
  const [lo, hi] = heroFeeDomain(points, floorGwei);
  const [plo, phi] = previous.domain;
  const inside = lo >= plo && hi <= phi;
  const covers = hi - lo >= (phi - plo) / 2;
  if (inside && covers) return previous;
  const fresh = heroFeeAxis(points, floorGwei);
  // The same axis again (a flat or empty series recomputes to what it had):
  // hand back the previous object, so a renderer comparing identities settles.
  return sameAxis(previous, fresh) ? previous : fresh;
}

function sameAxis(a: FeeAxis, b: FeeAxis): boolean {
  return a.domain[0] === b.domain[0] && a.domain[1] === b.domain[1] && a.ticks.length === b.ticks.length && a.ticks.every((t, i) => t === b.ticks[i]);
}

/**
 * One second of the live throughput chart: all the gas the blocks with that
 * timestamp carried. `x` is seconds before now, as on the fee chart, and the
 * second sits at its own end because that is when its gas is complete. `gas`
 * is null when poster gas was not recorded. `blocks` remains known in that
 * case and is null only where the ring itself has a gap.
 */
export type ThroughputPoint = { x: number; ts: number; gas: number | null; blocks: number | null };

/** Nitro pricer input for one block. Null or absent poster gas is unknown. */
export function blockComputeGas(block: Pick<BlockPoint, "gasUsed" | "posterGas">): number | null {
  if (block.posterGas === null || block.posterGas === undefined) return null;
  return Math.max(0, block.gasUsed - block.posterGas);
}

/**
 * Gas per second from the block ring, against the same clock-anchored axis
 * the fee chart uses. Blocks are summed per timestamp second and each second
 * sits at its own end, which is when its gas is complete.
 *
 * Only the seconds the ring holds whole are drawn. The newest is left out
 * whatever the clock says: blocks reach the browser a tick behind the chain,
 * so that second is still being delivered and its sum is a fraction of what it
 * will be, which drew a plunge to near zero at the right edge on every frame.
 * The oldest goes for the same reason at the other end: the ring is bounded by
 * block count, so it usually starts part way through a second, and that half
 * second read as the chain ramping up.
 *
 * Every second between those two ends gets a point, whether or not it holds a
 * block. A second with none is a quiet second and carries zero: leaving it out
 * let the area interpolate a positive rate straight across it, which says the
 * chain kept working through a moment it did not. The exception is a second
 * the ring itself has a hole across, which block numbers give away: consecutive
 * blocks either side means the second really was quiet, a jump in the numbers
 * means blocks are missing from the ring and the second is a null gap rather
 * than a zero.
 */
export function heroThroughputData(blocks: readonly BlockPoint[], places: BlockPlaces, nowMs: number, seconds = HERO_WINDOW_S): ThroughputPoint[] {
  if (blocks.length === 0) return [];
  const now = liveNow(nowMs, placeOf(places, blocks[blocks.length - 1]));
  const newest = blocks[blocks.length - 1].ts;
  const oldest = blocks[0].ts;
  const perSecond = new Map<number, { gas: number; blocks: number; known: boolean }>();
  for (const b of blocks) {
    if (b.ts >= newest || b.ts <= oldest) continue;
    const acc = perSecond.get(b.ts) ?? { gas: 0, blocks: 0, known: true };
    const compute = blockComputeGas(b);
    if (compute === null) acc.known = false;
    else acc.gas += compute;
    acc.blocks += 1;
    perSecond.set(b.ts, acc);
  }
  const out: ThroughputPoint[] = [];
  // A pointer into the ring: the last block at or before the second in hand,
  // so the continuity test either side of an empty second costs one step.
  let before = 0;
  // Only the seconds the axis actually shows: x is ts + 1 - now, so the window
  // is [now - seconds - 1, now - 1] intersected with the ring's whole seconds.
  const from = Math.max(oldest + 1, Math.ceil(now - seconds - 1));
  const to = Math.min(newest - 1, Math.floor(now - 1));
  for (let ts = from; ts <= to; ts++) {
    while (before + 1 < blocks.length && blocks[before + 1].ts <= ts) before += 1;
    const x = ts + 1 - now;
    const acc = perSecond.get(ts);
    if (acc !== undefined) {
      out.push({ x, ts, gas: acc.known ? acc.gas : null, blocks: acc.blocks });
      continue;
    }
    const complete = before + 1 < blocks.length && blocks[before + 1].number === blocks[before].number + 1;
    out.push({ x, ts, gas: complete ? 0 : null, blocks: complete ? 0 : null });
  }
  return out;
}

/**
 * A y axis in one gas unit: what it spans, where its ticks are, what to divide
 * a value by to label it, and the decimal count every label on it carries.
 */
export type ThroughputAxis = { top: number; ticks: number[]; divisor: number; decimals: number; unit: string };

/**
 * The throughput axis: a round top above the tallest value with ticks at
 * zero, the middle and the top, and one unit for the whole axis taken from
 * that top. Every label is then a bare figure with the same decimal count, so
 * the plot never shifts sideways as the rate moves; the unit rides on the
 * chart's caption instead, once.
 */
export function throughputAxis(max: number): ThroughputAxis {
  const peak = Number.isFinite(max) && max > 0 ? max : 1;
  const step = niceStep(peak / 2);
  const top = Math.max(step, Math.ceil((peak * 1.1) / step) * step);
  const { divisor, prefix } = gasScale(top);
  // The decimal count is the top tick's, not each tick's, so every label on the
  // axis has the same shape and the plot keeps its left edge.
  const decimals = divisor === 1 || top / divisor >= 100 ? 0 : 1;
  return { top, ticks: [0, top / 2, top], divisor, decimals, unit: `${prefix}gas/s` };
}

/** One label on that axis: the figure alone, in the axis's own unit and at the axis's own decimal count. */
export function throughputTick(value: number, axis: ThroughputAxis): string {
  const scaled = value / axis.divisor;
  return axis.decimals === 0 ? Math.round(scaled).toLocaleString("en-US") : scaled.toFixed(axis.decimals);
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
