// The presentation layer of the live section, kept pure so the frame loop in
// useSmoothedLive is only a scheduler: the render cadence, the tween that
// eases every figure toward its sample, the drain projection for long windows
// and the moving average for short ones.
//
// Why the short windows are averaged: nitro drains a backlog only when the
// block timestamp advances. Within one wall-clock second every block adds its
// gas and nothing is paid down; at the next second the whole second's worth
// of target (60M gas on Robinhood) comes off at once. A 15 s constraint's
// backlog is therefore a 1 Hz sawtooth, and the number that is true of it is
// its average, not its instantaneous value.

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

/** Every number the live section animates, in display units (gwei, ETH, x), plus per-constraint arrays in snapshot order. */
export type LiveValues = {
  baseFeeGwei: number;
  multiplier: number;
  gasPerSecond10: number;
  gasPerSecond60: number;
  transferEth: number;
  swapEth: number;
  /** The exponent that priced the sampled block, as x. */
  exponent: number;
  /** Per constraint: the drained projection for long windows, the 2 s average for short ones; the legacy backlog for the legacy model. */
  backlogs: number[];
  /** Per constraint exponent contribution in bips, from `backlogs` through the integer pricer at the sample, then eased. */
  bips: number[];
  shares: number[];
};

/** What the frame loop publishes each frame: the block ring, the eased values and the wall clock at the last cadence commit. */
export type LiveFrame = { blocks: BlockPoint[]; values: LiveValues | null; nowMs: number };

/** A tiny external store, so only the components that animate re-render per frame. */
export type FrameStore = {
  get(): LiveFrame;
  set(next: LiveFrame): void;
  subscribe(listener: () => void): () => void;
};

export function createFrameStore(initial: Partial<LiveFrame> = {}): FrameStore {
  let frame: LiveFrame = { blocks: [], values: null, nowMs: 0, ...initial };
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
 * Mean of a constraint's end-of-block backlog over the blocks of the last
 * `seconds` timestamp seconds ending at `lastTs`, or null when the ring has
 * no block in that span (a polled feed carries no blocks). Blocks within one
 * second share a timestamp, so the window is whole seconds and the mean is
 * per block: every sample the collector took counts once.
 */
export function averageBacklog(blocks: readonly BlockPoint[], index: number, lastTs: number, seconds = AVERAGE_WINDOW_S): number | null {
  const from = lastTs - seconds + 1;
  let sum = 0;
  let n = 0;
  for (let i = blocks.length - 1; i >= 0; i--) {
    const b = blocks[i];
    if (b.ts > lastTs) continue;
    if (b.ts < from) break;
    const v = b.backlogs[index];
    if (v === undefined) continue;
    sum += v;
    n++;
  }
  return n > 0 ? sum / n : null;
}

export type SawtoothSample = { number: number; ts: number; backlog: number };

/** The raw per-block backlog of one constraint over the last `seconds` timestamp seconds ending at `lastTs`, oldest first. */
export function sawtoothSamples(blocks: readonly BlockPoint[], index: number, lastTs: number, seconds = SAWTOOTH_WINDOW_S): SawtoothSample[] {
  const from = lastTs - seconds + 1;
  const out: SawtoothSample[] = [];
  for (const b of blocks) {
    if (b.ts < from || b.ts > lastTs) continue;
    const v = b.backlogs[index];
    if (v === undefined) continue;
    out.push({ number: b.number, ts: b.ts, backlog: v });
  }
  return out;
}

/** A backlog paid down at `rate` gas per second for `seconds`, floored at zero. */
export function drained(backlog: number, rate: number, seconds: number): number {
  return Math.max(0, backlog - rate * Math.max(0, seconds));
}

/**
 * The values a snapshot asks the screen to show `elapsedS` seconds after it
 * was sampled: long windows keep draining at their target, short windows show
 * the 2 s average from the block ring (falling back to the sample when the
 * ring has nothing recent), bips and shares follow through the integer pricer.
 */
export function targetValues(snapshot: LiveSnapshot, blocks: readonly BlockPoint[], elapsedS: number): LiveValues {
  const fee = snapshot.baseFee;
  const base = {
    baseFeeGwei: weiToGweiNumber(fee),
    multiplier: snapshot.multiplierBips / 10_000,
    gasPerSecond10: snapshot.gasPerSecond.s10,
    gasPerSecond60: snapshot.gasPerSecond.s60,
    transferEth: weiToEthNumber(costWei(TRANSFER_GAS, fee)),
    swapEth: weiToEthNumber(costWei(SWAP_GAS, fee)),
    exponent: snapshot.exponentBips / 10_000,
  };
  if (snapshot.model === "legacy" && snapshot.legacy) {
    const backlog = drained(snapshot.legacy.backlog, snapshot.legacy.speedLimit, elapsedS);
    const bips = [Number(legacyExponentBips(toLegacyState({ ...snapshot.legacy, backlog })))];
    return { ...base, backlogs: [backlog], bips, shares: sharesOf(bips) };
  }
  const backlogs = snapshot.constraints.map((c, i) =>
    isShortWindow(c.window) ? (averageBacklog(blocks, i, snapshot.block.ts) ?? c.backlog) : drained(c.backlog, c.target, elapsedS),
  );
  const bips = contributionsBips(snapshot.constraints.map((c, i) => ({ target: c.target, window: c.window, backlog: backlogs[i] })));
  return { ...base, backlogs, bips, shares: sharesOf(bips) };
}

/** Below this distance from the target the tween settles exactly, so an idle frame publishes nothing. */
function settleTolerance(target: number): number {
  return Math.max(1e-12, Math.abs(target) * 1e-6);
}

/**
 * One step of an exponential approach: the gap to the target shrinks by
 * e^(-dt/tau). A non-finite side snaps to the target.
 */
export function approach(current: number, target: number, dtMs: number, tauMs = TWEEN_TAU_MS): number {
  if (!Number.isFinite(current) || !Number.isFinite(target) || tauMs <= 0) return target;
  const gap = target - current;
  if (gap === 0) return target;
  const next = current + gap * (1 - Math.exp(-Math.max(0, dtMs) / tauMs));
  return Math.abs(target - next) <= settleTolerance(target) ? target : next;
}

/**
 * Eases every figure of `current` toward `target` by `dtMs`. Returns `current`
 * itself when nothing moved, so callers can skip a publish; snaps to the
 * target when there is nothing to ease from or the constraint set changed shape.
 */
export function tweenValues(current: LiveValues | null, target: LiveValues, dtMs: number, tauMs = TWEEN_TAU_MS): LiveValues {
  if (!current || current.backlogs.length !== target.backlogs.length) return target;
  let moved = false;
  const ease = (a: number, b: number): number => {
    const v = approach(a, b, dtMs, tauMs);
    if (v !== a) moved = true;
    return v;
  };
  const next: LiveValues = {
    baseFeeGwei: ease(current.baseFeeGwei, target.baseFeeGwei),
    multiplier: ease(current.multiplier, target.multiplier),
    gasPerSecond10: ease(current.gasPerSecond10, target.gasPerSecond10),
    gasPerSecond60: ease(current.gasPerSecond60, target.gasPerSecond60),
    transferEth: ease(current.transferEth, target.transferEth),
    swapEth: ease(current.swapEth, target.swapEth),
    exponent: ease(current.exponent, target.exponent),
    backlogs: current.backlogs.map((v, i) => ease(v, target.backlogs[i])),
    bips: current.bips.map((v, i) => ease(v, target.bips[i])),
    shares: current.shares.map((v, i) => ease(v, target.shares[i])),
  };
  return moved ? next : current;
}
