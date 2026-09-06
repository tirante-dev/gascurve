import { describe, expect, it, vi } from "vitest";
import type { BlockPoint, LiveSnapshot } from "@/types";
import {
  approach,
  AVERAGE_WINDOW_S,
  averageBacklog,
  createFrameStore,
  DISPLAY_INTERVAL_MS,
  drained,
  isShortWindow,
  SAWTOOTH_WINDOW_S,
  sawtoothSamples,
  SHORT_WINDOW_S,
  targetValues,
  tweenValues,
  TWEEN_TAU_MS,
} from "./smoothing";

function block(number: number, ts: number, backlogs: number[]): BlockPoint {
  return { number, ts, gasUsed: 4_000_000, baseFee: "399726000", predictedBaseFee: "399726000", backlogs, constraintBips: [], exponentBips: 0, minBaseFee: "20000000", anchored: false };
}

const snapshot: LiveSnapshot = {
  chainId: 4663,
  sampledAt: "2026-09-06T07:20:00Z",
  block: { number: 100, ts: 1000, gasUsed: 4_021_130, baseFee: "399726000", txCount: 90 },
  baseFee: "399726000",
  minBaseFee: "20000000",
  multiplierBips: 199_863,
  exponentBips: 32_425,
  model: "constraints",
  constraints: [
    { target: 60_000_000, window: 15, backlog: 3_111_506, exponentBips: 34 },
    { target: 40_000_000, window: 86_400, backlog: 11_194_391_810_886, exponentBips: 32_391 },
  ],
  prices: { perL2Tx: "0", perL1CalldataByte: "0", perL2Storage: "0", perArbGasBase: "20000000", perArbGasCongestion: "379726000", perArbGasTotal: "399726000" },
  gasPerSecond: { s10: 38_000_000, s60: 40_500_000 },
  replayErrorBips: 2,
};

/** A sawtooth: ten blocks a second each adding 4M, the drain landing at each second boundary. */
function sawtooth(seconds: number, lastTs: number, perSecond = 10): BlockPoint[] {
  const out: BlockPoint[] = [];
  let n = 1;
  for (let ts = lastTs - seconds + 1; ts <= lastTs; ts++) {
    for (let k = 0; k < perSecond; k++) out.push(block(n++, ts, [(k + 1) * 4_000_000, 11_194_391_810_886 - (lastTs - ts) * 40_000_000]));
  }
  return out;
}

describe("constants", () => {
  it("are what the owner asked for", () => {
    expect(DISPLAY_INTERVAL_MS).toBe(250);
    expect(TWEEN_TAU_MS).toBe(300);
    expect(SHORT_WINDOW_S).toBe(60);
    expect(AVERAGE_WINDOW_S).toBe(2);
    expect(SAWTOOTH_WINDOW_S).toBe(15);
  });
});

describe("isShortWindow", () => {
  it("is true up to 60 s and never for the zero legacy placeholder", () => {
    expect(isShortWindow(15)).toBe(true);
    expect(isShortWindow(60)).toBe(true);
    expect(isShortWindow(61)).toBe(false);
    expect(isShortWindow(86_400)).toBe(false);
    expect(isShortWindow(0)).toBe(false);
  });
});

describe("averageBacklog and sawtoothSamples", () => {
  const blocks = sawtooth(20, 1000);

  it("averages the per-block backlog over the last two timestamp seconds", () => {
    // Twenty blocks at 4M, 8M, ... 40M twice: the mean of the sawtooth.
    expect(averageBacklog(blocks, 0, 1000)).toBe(22_000_000);
    expect(averageBacklog(blocks, 0, 1000, 1)).toBe(22_000_000);
    // The long window is steady within a second and drains 40M per second.
    expect(averageBacklog(blocks, 1, 1000)).toBe(11_194_391_810_886 - 20_000_000);
  });

  it("returns null when the ring has nothing recent, and skips blocks newer than the sample", () => {
    expect(averageBacklog([], 0, 1000)).toBeNull();
    expect(averageBacklog(blocks, 0, 1005)).toBeNull();
    expect(averageBacklog(blocks, 0, 999)).toBe(22_000_000);
    expect(averageBacklog(blocks, 2, 1000)).toBeNull();
    expect(averageBacklog([block(1, 1000, [5]), block(2, 1000, [])], 0, 1000)).toBe(5);
  });

  it("extracts the raw sawtooth over the last fifteen seconds, oldest first", () => {
    const samples = sawtoothSamples(blocks, 0, 1000);
    expect(samples).toHaveLength(150);
    expect(samples[0]).toEqual({ number: 51, ts: 986, backlog: 4_000_000 });
    expect(samples[samples.length - 1]).toEqual({ number: 200, ts: 1000, backlog: 40_000_000 });
    // Within a second the backlog only climbs; at each boundary it falls.
    for (let i = 1; i < samples.length; i++) {
      if (samples[i].ts === samples[i - 1].ts) expect(samples[i].backlog).toBeGreaterThan(samples[i - 1].backlog);
      else expect(samples[i].backlog).toBeLessThan(samples[i - 1].backlog);
    }
    expect(sawtoothSamples(blocks, 0, 1000, 1)).toHaveLength(10);
    expect(sawtoothSamples(blocks, 0, 950)).toEqual([]);
    expect(sawtoothSamples(blocks, 5, 1000)).toEqual([]);
  });
});

describe("targetValues", () => {
  it("projects long windows, averages short ones and prices both through the pricer", () => {
    const blocks = sawtooth(20, 1000);
    const v = targetValues(snapshot, blocks, 0.5);
    expect(v.baseFeeGwei).toBeCloseTo(0.399726);
    expect(v.multiplier).toBeCloseTo(19.9863);
    expect(v.gasPerSecond10).toBe(38_000_000);
    expect(v.gasPerSecond60).toBe(40_500_000);
    expect(v.transferEth).toBeCloseTo(8.394246e-6, 12);
    expect(v.swapEth).toBeCloseTo(5.99589e-5, 10);
    expect(v.exponent).toBeCloseTo(3.2425);
    // The short window shows the 2 s average, not the sample; the long one drains 20M in half a second.
    expect(v.backlogs).toEqual([22_000_000, 11_194_391_810_886 - 20_000_000]);
    // 22M / (60M × 15) = 0.0244 → 244 bips; the long window's share dominates.
    expect(v.bips[0]).toBe(244);
    expect(v.bips[1]).toBe(32_391);
    expect(v.shares[0]).toBeCloseTo(244 / (244 + 32_391));
    expect(v.shares[0] + v.shares[1]).toBeCloseTo(1);
  });

  it("falls back to the sampled backlog for a short window when no block is recent", () => {
    const v = targetValues(snapshot, [], 0);
    expect(v.backlogs).toEqual([3_111_506, 11_194_391_810_886]);
    expect(v.bips).toEqual([34, 32_391]);
    // Nothing drains before any time has passed, and the projection never goes negative.
    expect(targetValues(snapshot, [], 1e9).backlogs[1]).toBe(0);
  });

  it("drains the legacy backlog at the speed limit and prices it with the legacy exponent", () => {
    const legacy: LiveSnapshot = { ...snapshot, model: "legacy", constraints: [], legacy: { speedLimit: 7_000_000, inertia: 102, tolerance: 10, backlog: 97_000_000 } };
    const v = targetValues(legacy, [], 1);
    expect(v.backlogs).toEqual([90_000_000]);
    expect(v.bips).toEqual([280]);
    expect(v.shares).toEqual([1]);
    expect(targetValues(legacy, [], 100).bips).toEqual([0]);
    expect(targetValues(legacy, [], 100).shares).toEqual([0]);
  });

  it("drains without going below zero", () => {
    expect(drained(100, 10, 5)).toBe(50);
    expect(drained(100, 10, 50)).toBe(0);
    expect(drained(100, 10, -5)).toBe(100);
  });
});

describe("approach", () => {
  it("closes the gap exponentially with the time constant and settles exactly", () => {
    const after = approach(0, 100, 300);
    expect(after).toBeCloseTo(100 * (1 - Math.exp(-1)));
    expect(approach(0, 100, 0)).toBe(0);
    expect(approach(50, 50, 16)).toBe(50);
    // Iterating converges to the target itself, not to within a float of it.
    let v = 0;
    for (let i = 0; i < 500; i++) v = approach(v, 1, 16);
    expect(v).toBe(1);
    v = 1e13;
    for (let i = 0; i < 500; i++) v = approach(v, 1e13 + 1e9, 16);
    expect(v).toBe(1e13 + 1e9);
  });

  it("snaps when either side is not finite or the time constant is off", () => {
    expect(approach(Number.NaN, 5, 16)).toBe(5);
    expect(approach(1, Number.POSITIVE_INFINITY, 16)).toBe(Number.POSITIVE_INFINITY);
    expect(approach(1, 5, 16, 0)).toBe(5);
    expect(approach(1, 5, -16)).toBe(1);
  });
});

describe("tweenValues", () => {
  const blocks = sawtooth(20, 1000);
  const target = targetValues(snapshot, blocks, 0);

  it("eases every field toward the target and returns the same object when nothing moves", () => {
    const start = targetValues({ ...snapshot, baseFee: "200000000", multiplierBips: 100_000, exponentBips: 20_000, gasPerSecond: { s10: 1, s60: 2 } }, [], 0);
    const mid = tweenValues(start, target, 300);
    const k = 1 - Math.exp(-1);
    expect(mid.baseFeeGwei).toBeCloseTo(0.2 + (0.399726 - 0.2) * k);
    expect(mid.multiplier).toBeCloseTo(10 + (19.9863 - 10) * k);
    expect(mid.exponent).toBeCloseTo(2 + (3.2425 - 2) * k);
    expect(mid.gasPerSecond10).toBeCloseTo(1 + (38_000_000 - 1) * k);
    expect(mid.gasPerSecond60).toBeCloseTo(2 + (40_500_000 - 2) * k);
    expect(mid.transferEth).toBeCloseTo(start.transferEth + (target.transferEth - start.transferEth) * k, 12);
    expect(mid.swapEth).toBeCloseTo(start.swapEth + (target.swapEth - start.swapEth) * k, 12);
    expect(mid.backlogs[0]).toBeCloseTo(3_111_506 + (22_000_000 - 3_111_506) * k);
    expect(mid.bips[0]).toBeCloseTo(34 + (244 - 34) * k);
    expect(mid.shares[0]).toBeGreaterThan(start.shares[0]);
    expect(mid.shares[0]).toBeLessThan(target.shares[0]);
    let v = mid;
    for (let i = 0; i < 600; i++) v = tweenValues(v, target, 16);
    expect(v).toEqual(target);
    expect(tweenValues(v, target, 16)).toBe(v);
  });

  it("snaps when there is nothing to ease from or the constraint set changed shape", () => {
    expect(tweenValues(null, target, 16)).toBe(target);
    const other = targetValues({ ...snapshot, constraints: snapshot.constraints.slice(0, 1) }, [], 0);
    expect(tweenValues(other, target, 16)).toBe(target);
  });

  it("honours a custom time constant", () => {
    const start = targetValues({ ...snapshot, baseFee: "0" }, [], 0);
    expect(tweenValues(start, target, 100, 100).baseFeeGwei).toBeCloseTo(0.399726 * (1 - Math.exp(-1)));
  });
});

describe("createFrameStore", () => {
  it("publishes to subscribers and lets them leave", () => {
    const store = createFrameStore({ nowMs: 5 });
    expect(store.get()).toEqual({ blocks: [], values: null, nowMs: 5 });
    const seen = vi.fn();
    const stop = store.subscribe(seen);
    const next = { blocks: [], values: null, nowMs: 6 };
    store.set(next);
    expect(store.get()).toBe(next);
    expect(seen).toHaveBeenCalledTimes(1);
    stop();
    store.set({ ...next, nowMs: 7 });
    expect(seen).toHaveBeenCalledTimes(1);
  });
});
