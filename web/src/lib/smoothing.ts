// The presentation layer of the live section, kept pure so the frame loop in useSmoothedLive is only a
// scheduler. The backlogs are the state, so they are what the tween moves; each frame's contributions and
// shares are derived from that frame's backlogs through the integer pricer, so a card can never show a
// backlog beside a contribution that is not its own. The constraint definition travels with the values as
// a signature: when an owner replaces a set, even with one of the same size, everything snaps. Short
// windows are averaged because nitro drains a backlog only when the block timestamp advances, which makes
// a 15 s constraint's backlog a 1 Hz sawtooth whose average is the number that is true.

import { contributionsBips, legacyExponentBips, toLegacyState } from "@/lib/pricer";
import type { BlockPoint, LiveSnapshot } from "@/types";
import { sharesOf } from "@/utils/chart";
import { costWei, weiToEthNumber, weiToGweiNumber } from "@/utils/format";

/** The display snapshot changes at most this often. */
export const DISPLAY_INTERVAL_MS = 250;
/** Time constant of the exponential approach: about 95% of a step in three of these. */
export const TWEEN_TAU_MS = 300;
/** Windows of this length or less are shown as a moving average with a sawtooth. */
export const SHORT_WINDOW_S = 60;
/** Length of that moving average, in whole timestamp seconds. */
export const AVERAGE_WINDOW_S = 2;
/** Span of the raw per-block sawtooth sparkline. */
export const SAWTOOTH_WINDOW_S = 15;

export const TRANSFER_GAS = 21_000;
export const SWAP_GAS = 150_000;

/** What prices a snapshot: the constraint targets and windows, or the legacy parameters. Backlogs are the
 * state that moves; this is the definition that turns them into an exponent. */
export type PricingDefinition =
  | { model: "constraints"; constraints: { target: number; window: number }[] }
  | { model: "legacy"; legacy: { speedLimit: number; inertia: number; tolerance: number } };

/** The definition a snapshot was priced under. A legacy snapshot without its parameters is read as an empty constraint set. */
export function definitionOf(snapshot: Pick<LiveSnapshot, "model" | "constraints" | "legacy">): PricingDefinition {
  if (snapshot.model === "legacy" && snapshot.legacy) {
    const { speedLimit, inertia, tolerance } = snapshot.legacy;
    return { model: "legacy", legacy: { speedLimit, inertia, tolerance } };
  }
  return { model: "constraints", constraints: snapshot.constraints.map((c) => ({ target: c.target, window: c.window })) };
}

/**
 * A stable string for a definition. Two snapshots with the same signature price the same way, so their
 * backlogs may be eased into one another; any other change makes the old figures meaningless and snaps.
 */
export function signatureOf(definition: PricingDefinition): string {
  if (definition.model === "legacy") {
    const l = definition.legacy;
    return `legacy:${l.speedLimit}/${l.inertia}/${l.tolerance}`;
  }
  return `constraints:${definition.constraints.map((c) => `${c.target}/${c.window}`).join(",")}`;
}

/** The exponent contributions of `backlogs` under `definition`, through the integer pricer. A missing backlog counts as zero gas. */
export function bipsFor(definition: PricingDefinition, backlogs: readonly number[]): number[] {
  if (definition.model === "legacy") {
    return [Number(legacyExponentBips(toLegacyState({ ...definition.legacy, backlog: backlogs[0] ?? 0 })))];
  }
  return contributionsBips(definition.constraints.map((c, i) => ({ target: c.target, window: c.window, backlog: backlogs[i] ?? 0 })));
}

/** Every number the live section animates, in display units (gwei, ETH, x), plus per-constraint arrays in snapshot order. */
export type LiveValues = {
  baseFeeGwei: number;
  multiplier: number;
  gasPerSecond10: number | null;
  gasPerSecond60: number | null;
  transferEth: number;
  swapEth: number;
  /** The exponent that priced the sampled block, as x. */
  exponent: number;
  /** Per constraint: the drained projection for long windows, the 2 s average for short ones. */
  backlogs: number[];
  /** Per constraint exponent contribution in bips, derived from `backlogs` every frame, never eased. */
  bips: number[];
  /** Each constraint's share of the total, from those same bips. */
  shares: number[];
  /** The definition `backlogs` are priced under, and its signature: the tween snaps whenever the signature changes. */
  definition: PricingDefinition;
  signature: string;
};

/**
 * Where each block of the ring sits on the time axis, by block number. A place is assigned once, when the
 * block enters the ring, so every chart draws the same block at the same place and no block ever moves.
 */
export type BlockPlaces = ReadonlyMap<number, number>;

/** The empty placement, for a frame with no ring yet. */
export const NO_PLACES: BlockPlaces = new Map<number, number>();

/** Where a block sits, or its bare timestamp for one the placement does not know. */
export function placeOf(places: BlockPlaces, block: { number: number; ts: number }): number {
  return places.get(block.number) ?? block.ts;
}

/** What the frame loop publishes each frame: the block ring with its placements, the eased values and the wall clock at the last cadence commit. */
export type LiveFrame = { blocks: BlockPoint[]; places: BlockPlaces; values: LiveValues | null; nowMs: number };

/** A tiny external store, so only the components that animate re-render per frame. */
export type FrameStore = {
  get(): LiveFrame;
  set(next: LiveFrame): void;
  subscribe(listener: () => void): () => void;
};

export function createFrameStore(initial: Partial<LiveFrame> = {}): FrameStore {
  let frame: LiveFrame = { blocks: [], places: NO_PLACES, values: null, nowMs: 0, ...initial };
  const listeners = new Set<() => void>();
  return {
    get: () => frame,
    set(next) {
      frame = next;
      for (const listener of listeners) listener();
    },
    subscribe(listener) {
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
      };
    },
  };
}

/** True for a constraint whose window is short enough that its backlog is a per-second sawtooth. A zero window (the legacy placeholder) is not short. */
export function isShortWindow(windowSeconds: number): boolean {
  return windowSeconds > 0 && windowSeconds <= SHORT_WINDOW_S;
}

/**
 * Mean of a constraint's end-of-block backlog over the blocks of the last `seconds` timestamp seconds
 * ending at `lastTs`, or null when the ring has no block in that span. The window is whole seconds and the
 * mean is per block, so every sample counts once. Blocks below `sinceBlock` are left out: they were priced
 * under another definition, and averaging across a change would mix two meanings of the same slot.
 */
export function averageBacklog(
  blocks: readonly BlockPoint[],
  index: number,
  lastTs: number,
  seconds = AVERAGE_WINDOW_S,
  sinceBlock = Number.NEGATIVE_INFINITY,
): number | null {
  const from = lastTs - seconds + 1;
  let sum = 0;
  let n = 0;
  for (let i = blocks.length - 1; i >= 0; i--) {
    const b = blocks[i];
    if (b.ts > lastTs) continue;
    if (b.ts < from) break;
    if (b.number < sinceBlock) continue;
    const v = b.backlogs[index];
    if (v === undefined) continue;
    sum += v;
    n++;
  }
  return n > 0 ? sum / n : null;
}

export type SawtoothSample = { number: number; ts: number; gasUsed: number; backlog: number };

/** The raw per-block backlog of one constraint over the last `seconds` timestamp seconds ending at `lastTs`, oldest first. */
export function sawtoothSamples(blocks: readonly BlockPoint[], index: number, lastTs: number, seconds = SAWTOOTH_WINDOW_S): SawtoothSample[] {
  const from = lastTs - seconds + 1;
  const out: SawtoothSample[] = [];
  for (const b of blocks) {
    if (b.ts < from || b.ts > lastTs) continue;
    const v = b.backlogs[index];
    if (v === undefined) continue;
    out.push({ number: b.number, ts: b.ts, gasUsed: b.gasUsed, backlog: v });
  }
  return out;
}

/** A sample placed on a time axis, in seconds before now: 0 is the right edge. */
export type SawtoothPoint = { x: number; number: number; ts: number; gasUsed: number; backlog: number };

/**
 * Where each block of a standalone list sits on a time axis, in seconds of block timestamp with a fraction
 * for its place within its second. Headers carry whole seconds and a Nitro chain makes several blocks a
 * second, so the k-th block of a second is spread across it rather than stacked on one tick.
 *
 * This is the assignment rule; `assignPlaces` is how the live ring uses it, placing only new blocks,
 * because recomputing every fraction moves points that have already been drawn.
 */
export function placeBlocks(blocks: readonly { ts: number }[]): number[] {
  if (blocks.length === 0) return [];
  const shape = secondShape(blocks);
  const placed = new Map<number, number>();
  return blocks.map((b) => {
    const k = placed.get(b.ts) ?? 0;
    placed.set(b.ts, k + 1);
    return b.ts + fraction(k, shape(b.ts));
  });
}

/**
 * How many blocks a second is spread over: its own count for every second but the newest, which is still
 * filling. A point that moved every time a sibling arrived is exactly the jitter the charts must not show,
 * so the newest is spread by the count of the second before it, widened only once it has overtaken that.
 */
function secondShape(blocks: readonly { ts: number }[]): (ts: number) => number {
  const counts = new Map<number, number>();
  for (const b of blocks) counts.set(b.ts, (counts.get(b.ts) ?? 0) + 1);
  const newest = blocks[blocks.length - 1].ts;
  let previous = 0;
  for (const ts of counts.keys()) {
    if (ts < newest && ts > previous) previous = ts;
  }
  const newestN = Math.max(counts.get(newest) ?? 1, previous > 0 ? (counts.get(previous) ?? 1) : 1);
  return (ts) => (ts === newest ? newestN : (counts.get(ts) ?? 1));
}

/**
 * Where the k-th block of a second sits inside it, as a fraction. `n` is what the second was expected to
 * hold when the block arrived; a block past that estimate divides the remainder rather than spilling into
 * the next second, so the fraction rises with k, stays below one, and never depends on later arrivals.
 */
function fraction(k: number, n: number): number {
  return k / Math.max(n, k + 1);
}

/**
 * The ring's placements after `blocks`: every block already placed keeps its place, evicted blocks lose
 * theirs, and only new blocks get one. Recomputing the whole ring from the current per-second counts moved
 * every existing point whenever a third block landed in the newest second. `previous` is handed back
 * unchanged when nothing entered or left, so a frame that only ticks the clock publishes the same map.
 */
export function assignPlaces(previous: BlockPlaces, blocks: readonly { number: number; ts: number }[]): BlockPlaces {
  if (blocks.length === 0) return previous.size === 0 ? previous : NO_PLACES;
  const shape = secondShape(blocks);
  const next = new Map<number, number>();
  const placed = new Map<number, number>();
  let added = false;
  for (const b of blocks) {
    const k = placed.get(b.ts) ?? 0;
    placed.set(b.ts, k + 1);
    const kept = previous.get(b.number);
    if (kept !== undefined) {
      next.set(b.number, kept);
      continue;
    }
    added = true;
    next.set(b.number, b.ts + fraction(k, shape(b.ts)));
  }
  return !added && next.size === previous.size ? previous : next;
}

/**
 * The right edge of a live time axis, in seconds: the wall clock, or the newest block's place when a slow
 * browser clock would put that block in the future. The axis then slides with the clock rather than
 * stepping once a second.
 */
export function liveNow(nowMs: number, newestPlace: number | undefined): number {
  const wall = nowMs / 1000;
  return newestPlace === undefined ? wall : Math.max(wall, newestPlace);
}

/**
 * The short-window chart's series against a wall-clock axis ending at `nowMs`. Each block keeps its place,
 * so the sawtooth slides left and never rearranges itself. `places` is the ring's own placement where the
 * caller has one, so this chart puts a block exactly where the hero above it does.
 */
export function sawtoothChart(samples: readonly SawtoothSample[], nowMs: number, places?: BlockPlaces): SawtoothPoint[] {
  const own = places === undefined ? placeBlocks(samples) : samples.map((s) => placeOf(places, s));
  const now = liveNow(nowMs, own[own.length - 1]);
  return samples.map((s, i) => ({ x: own[i] - now, number: s.number, ts: s.ts, gasUsed: s.gasUsed, backlog: s.backlog }));
}

/** A backlog paid down at `rate` gas per second for `seconds`, floored at zero. */
export function drained(backlog: number, rate: number, seconds: number): number {
  return Math.max(0, backlog - rate * Math.max(0, seconds));
}

/**
 * The values a snapshot asks the screen to show `elapsedS` seconds after it was sampled: long windows keep
 * draining at their target, short windows show the 2 s average from the block ring (falling back to the
 * sample, and never reaching past `sinceBlock`). Bips and shares follow from those backlogs.
 */
export function targetValues(snapshot: LiveSnapshot, blocks: readonly BlockPoint[], elapsedS: number, sinceBlock = Number.NEGATIVE_INFINITY): LiveValues {
  const fee = snapshot.baseFee;
  const definition = definitionOf(snapshot);
  const base = {
    baseFeeGwei: weiToGweiNumber(fee),
    multiplier: snapshot.multiplierBips / 10_000,
    gasPerSecond10: snapshot.computeGasPerSecond?.s10 ?? null,
    gasPerSecond60: snapshot.computeGasPerSecond?.s60 ?? null,
    transferEth: weiToEthNumber(costWei(TRANSFER_GAS, fee)),
    swapEth: weiToEthNumber(costWei(SWAP_GAS, fee)),
    exponent: snapshot.exponentBips / 10_000,
    definition,
    signature: signatureOf(definition),
  };
  if (definition.model === "legacy" && snapshot.legacy) {
    const backlogs = [drained(snapshot.legacy.backlog, snapshot.legacy.speedLimit, elapsedS)];
    return { ...base, backlogs, ...derived(definition, backlogs) };
  }
  const backlogs = snapshot.constraints.map((c, i) =>
    isShortWindow(c.window) ? (averageBacklog(blocks, i, snapshot.block.ts, AVERAGE_WINDOW_S, sinceBlock) ?? c.backlog) : drained(c.backlog, c.target, elapsedS),
  );
  return { ...base, backlogs, ...derived(definition, backlogs) };
}

/** The bips and shares that follow from `backlogs`, so the two are never eased on their own. */
function derived(definition: PricingDefinition, backlogs: readonly number[]): { bips: number[]; shares: number[] } {
  const bips = bipsFor(definition, backlogs);
  return { bips, shares: sharesOf(bips) };
}

/** Below this distance from the target the tween settles exactly, so an idle frame publishes nothing. */
function settleTolerance(target: number): number {
  return Math.max(1e-12, Math.abs(target) * 1e-6);
}

/** One step of an exponential approach: the gap to the target shrinks by e^(-dt/tau). A non-finite side
 * snaps to the target. */
export function approach(current: number, target: number, dtMs: number, tauMs = TWEEN_TAU_MS): number {
  if (!Number.isFinite(current) || !Number.isFinite(target) || tauMs <= 0) return target;
  const gap = target - current;
  if (gap === 0) return target;
  const next = current + gap * (1 - Math.exp(-Math.max(0, dtMs) / tauMs));
  return Math.abs(target - next) <= settleTolerance(target) ? target : next;
}

/**
 * Eases every figure of `current` toward `target` by `dtMs`. Only the backlogs are eased; bips and shares
 * are computed from them, so a card's contribution is always the contribution of the backlog beside it.
 * Returns `current` itself when nothing moved, and snaps when there is nothing to ease from or the
 * constraint signature changed.
 */
export function tweenValues(current: LiveValues | null, target: LiveValues, dtMs: number, tauMs = TWEEN_TAU_MS): LiveValues {
  if (!current || current.signature !== target.signature || current.backlogs.length !== target.backlogs.length) return target;
  let moved = false;
  const ease = (a: number, b: number): number => {
    const v = approach(a, b, dtMs, tauMs);
    if (v !== a) moved = true;
    return v;
  };
  const easeNullable = (a: number | null, b: number | null): number | null => {
    if (a === null || b === null) {
      if (a !== b) moved = true;
      return b;
    }
    return ease(a, b);
  };
  const backlogs = current.backlogs.map((v, i) => ease(v, target.backlogs[i]));
  const next: LiveValues = {
    baseFeeGwei: ease(current.baseFeeGwei, target.baseFeeGwei),
    multiplier: ease(current.multiplier, target.multiplier),
    gasPerSecond10: easeNullable(current.gasPerSecond10, target.gasPerSecond10),
    gasPerSecond60: easeNullable(current.gasPerSecond60, target.gasPerSecond60),
    transferEth: ease(current.transferEth, target.transferEth),
    swapEth: ease(current.swapEth, target.swapEth),
    exponent: ease(current.exponent, target.exponent),
    backlogs,
    definition: target.definition,
    signature: target.signature,
    ...derived(target.definition, backlogs),
  };
  return moved ? next : current;
}
