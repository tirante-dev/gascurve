import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createFrameStore, targetValues } from "@/lib/smoothing";
import type { BlockPoint, LiveSnapshot } from "@/types";
import { prefersReducedMotion, useLiveFrame, useSmoothedLive } from "./useSmoothedLive";

function block(number: number, ts: number, backlogs: number[]): BlockPoint {
  return { number, ts, gasUsed: 4_000_000, baseFee: "399726000", predictedBaseFee: "399726000", backlogs, constraintBips: [], exponentBips: 0, minBaseFee: "20000000", anchored: false };
}

function snapshot(n: number, baseFee = "399726000", longBacklog = 11_194_391_810_886): LiveSnapshot {
  return {
    chainId: 4663,
    sampledAt: "2026-09-06T07:20:00Z",
    block: { number: n, ts: 1000, gasUsed: 4_021_130, baseFee, txCount: 90 },
    baseFee,
    minBaseFee: "20000000",
    multiplierBips: Number((BigInt(baseFee) * 10_000n) / 20_000_000n),
    exponentBips: 32_425,
    model: "constraints",
    constraints: [
      { target: 60_000_000, window: 15, backlog: 3_111_506, exponentBips: 34 },
      { target: 40_000_000, window: 86_400, backlog: longBacklog, exponentBips: 32_391 },
    ],
    prices: { perL2Tx: "0", perL1CalldataByte: "0", perL2Storage: "0", perArbGasBase: "20000000", perArbGasCongestion: "0", perArbGasTotal: baseFee },
    gasPerSecond: { s10: 38_000_000, s60: 40_500_000 },
    replayErrorBips: 2,
  };
}

/** A snapshot with only a short window, so nothing drains between samples and idle frames stay idle. */
function shortOnly(n: number): LiveSnapshot {
  const s = snapshot(n);
  return { ...s, constraints: s.constraints.slice(0, 1) };
}

let frames: FrameRequestCallback[] = [];
let clock = 0;
const cancel = vi.fn();

function runFrame(t: number) {
  clock = t;
  const cb = frames[frames.length - 1];
  frames = [];
  act(() => cb(t));
}

function setHidden(hidden: boolean) {
  Object.defineProperty(document, "hidden", { configurable: true, get: () => hidden });
  document.dispatchEvent(new Event("visibilitychange"));
}

describe("useSmoothedLive", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(1_000_000);
    frames = [];
    clock = 0;
    cancel.mockReset();
    vi.stubGlobal("requestAnimationFrame", (cb: FrameRequestCallback) => {
      frames.push(cb);
      return frames.length;
    });
    vi.stubGlobal("cancelAnimationFrame", cancel);
    vi.spyOn(performance, "now").mockImplementation(() => clock);
    setHidden(false);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it("commits the newest tick at most every 250 ms and moves the wall clock with it", () => {
    const { result, rerender } = renderHook(({ s }) => useSmoothedLive({ snapshot: s, recentBlocks: [] }), { initialProps: { s: snapshot(1) as LiveSnapshot | null } });
    expect(result.current.display).toBeNull();
    expect(frames).toHaveLength(1);
    runFrame(16);
    expect(result.current.display?.block.number).toBe(1);
    expect(result.current.frame.get().nowMs).toBe(1_000_000);
    expect(result.current.frame.get().values?.baseFeeGwei).toBeCloseTo(0.399726);

    // Two ticks inside the window: neither shows until the window ends, then only the newest does.
    clock = 50;
    rerender({ s: snapshot(2) });
    clock = 100;
    rerender({ s: snapshot(3, "800000000") });
    vi.setSystemTime(1_000_116);
    runFrame(116);
    expect(result.current.display?.block.number).toBe(1);
    expect(result.current.frame.get().nowMs).toBe(1_000_000);
    vi.setSystemTime(1_000_266);
    runFrame(266);
    expect(result.current.display?.block.number).toBe(3);
    expect(result.current.frame.get().nowMs).toBe(1_000_266);

    // The figure eases toward the new sample rather than jumping to it.
    const first = result.current.frame.get().values?.baseFeeGwei ?? 0;
    expect(first).toBeGreaterThan(0.399726);
    expect(first).toBeLessThan(0.8);
    runFrame(282);
    const second = result.current.frame.get().values?.baseFeeGwei ?? 0;
    expect(second).toBeGreaterThan(first);
    let t = 282;
    for (let i = 0; i < 400; i++) runFrame((t += 16));
    expect(result.current.frame.get().values?.baseFeeGwei).toBe(0.8);
    expect(result.current.frame.get().values?.multiplier).toBe(40);
  });

  it("hands blocks on in one batch per frame and publishes nothing on an idle frame", () => {
    const { result, rerender } = renderHook(({ blocks }) => useSmoothedLive({ snapshot: shortOnly(1), recentBlocks: blocks }), { initialProps: { blocks: [] as BlockPoint[] } });
    runFrame(16);
    const ring = [block(1, 1000, [4_000_000]), block(2, 1000, [8_000_000])];
    rerender({ blocks: ring });
    runFrame(32);
    const frame = result.current.frame.get();
    expect(frame.blocks).toBe(ring);
    // The short window now shows the 2 s average of the ring, eased in from the sample.
    expect(frame.values?.backlogs[0]).toBeGreaterThan(3_111_506);
    let t = 32;
    for (let i = 0; i < 400; i++) runFrame((t += 16));
    expect(result.current.frame.get().values?.backlogs[0]).toBe(6_000_000);
    // Settled and inside a cadence window: the store is untouched.
    const settled = result.current.frame.get();
    runFrame(t + 16);
    expect(result.current.frame.get()).toBe(settled);
    // The cadence still refreshes the wall clock on its own.
    vi.setSystemTime(2_000_000);
    runFrame(t + 300);
    expect(result.current.frame.get().nowMs).toBe(2_000_000);
    expect(result.current.frame.get().values).toBe(settled.values);
  });

  it("drains long windows toward a moving target so a new sample never snaps the gauge", () => {
    const { result, rerender } = renderHook(({ s }) => useSmoothedLive({ snapshot: s, recentBlocks: [] }), { initialProps: { s: snapshot(1) } });
    runFrame(0);
    const seen: number[] = [];
    let t = 0;
    for (let i = 0; i < 30; i++) {
      runFrame((t += 16));
      seen.push(result.current.frame.get().values?.backlogs[1] ?? 0);
    }
    // Half a second in, the projection has paid down about 20M of the 40M/s target.
    expect(seen[seen.length - 1]).toBeLessThan(11_194_391_810_886 - 15_000_000);
    expect(seen[seen.length - 1]).toBeGreaterThan(11_194_391_810_886 - 25_000_000);
    // A sample that already includes that drain lands where the projection is.
    clock = t;
    rerender({ s: snapshot(2, "399726000", 11_194_391_810_886 - 40_000_000 * (t / 1000)) });
    for (let i = 0; i < 30; i++) {
      runFrame((t += 16));
      seen.push(result.current.frame.get().values?.backlogs[1] ?? 0);
    }
    for (let i = 1; i < seen.length; i++) expect(seen[i]).toBeLessThanOrEqual(seen[i - 1]);
  });

  it("only applies the cadence under reduced motion: no tween, no drain", () => {
    vi.stubGlobal("matchMedia", () => ({ matches: true }));
    expect(prefersReducedMotion()).toBe(true);
    const { result, rerender } = renderHook(({ s }) => useSmoothedLive({ snapshot: s, recentBlocks: [] }), { initialProps: { s: snapshot(1) } });
    runFrame(16);
    expect(result.current.frame.get().values).toEqual(targetValues(snapshot(1), [], 0));
    runFrame(32);
    runFrame(48);
    expect(result.current.frame.get().values?.backlogs[1]).toBe(11_194_391_810_886);
    rerender({ s: snapshot(2, "800000000") });
    runFrame(64);
    expect(result.current.display?.block.number).toBe(1);
    runFrame(266);
    expect(result.current.display?.block.number).toBe(2);
    expect(result.current.frame.get().values?.baseFeeGwei).toBe(0.8);
    runFrame(282);
    expect(result.current.frame.get().values?.baseFeeGwei).toBe(0.8);
  });

  it("clears at once when the feed clears, and stops while the tab is hidden", () => {
    const { result, rerender } = renderHook(({ s }) => useSmoothedLive({ snapshot: s, recentBlocks: [block(1, 1000, [1, 2])] }), { initialProps: { s: snapshot(1) as LiveSnapshot | null } });
    runFrame(16);
    expect(result.current.display).not.toBeNull();
    rerender({ s: null });
    expect(result.current.display).toBeNull();
    expect(result.current.frame.get().values).toBeNull();
    expect(result.current.frame.get().blocks).toEqual([]);
    runFrame(32);
    expect(result.current.display).toBeNull();

    // The same chain's next tick may show the last committed sample until the cadence; another chain's never does.
    rerender({ s: snapshot(5) });
    expect(result.current.display?.block.number).toBe(1);
    rerender({ s: { ...snapshot(6), chainId: 42161 } });
    expect(result.current.display).toBeNull();

    act(() => setHidden(true));
    expect(cancel).toHaveBeenCalled();
    frames = [];
    rerender({ s: { ...snapshot(7), chainId: 42161 } });
    expect(frames).toHaveLength(0);
    expect(result.current.display).toBeNull();
    act(() => setHidden(false));
    expect(frames).toHaveLength(1);
    runFrame(48);
    expect(result.current.display).toBeNull();
    runFrame(300);
    expect(result.current.display?.block.number).toBe(7);
    expect(result.current.display?.chainId).toBe(42161);
  });

  it("treats the first tick after a reorg as a fresh commit: shown on the next frame, values snapped, never eased from the orphaned block", () => {
    const { result, rerender } = renderHook(({ s, reorgs }) => useSmoothedLive({ snapshot: s, recentBlocks: [], reorgs }), { initialProps: { s: snapshot(1), reorgs: 0 } });
    runFrame(16);
    expect(result.current.display?.block.number).toBe(1);
    runFrame(100);
    // The reorg orphans block 1; the canonical block's tick lands inside the cadence window.
    clock = 100;
    rerender({ s: snapshot(2, "800000000"), reorgs: 1 });
    runFrame(116);
    expect(result.current.display?.block.number).toBe(2);
    expect(result.current.frame.get().values?.baseFeeGwei).toBe(0.8);
    expect(result.current.frame.get().values?.multiplier).toBe(40);
    // The reorg is consumed: the next tick waits for the cadence and eases again.
    clock = 132;
    const third = snapshot(3, "400000000");
    rerender({ s: third, reorgs: 1 });
    runFrame(132);
    expect(result.current.display?.block.number).toBe(2);
    runFrame(400);
    expect(result.current.display?.block.number).toBe(3);
    const eased = result.current.frame.get().values?.baseFeeGwei ?? 0;
    expect(eased).toBeGreaterThan(0.4);
    expect(eased).toBeLessThan(0.8);
    // A reorg with no tick behind it yet changes nothing on screen; the tick that follows is the fresh one.
    rerender({ s: third, reorgs: 2 });
    runFrame(416);
    expect(result.current.display?.block.number).toBe(3);
    clock = 420;
    rerender({ s: snapshot(4, "400000000"), reorgs: 2 });
    runFrame(432);
    expect(result.current.display?.block.number).toBe(4);
    expect(result.current.frame.get().values?.baseFeeGwei).toBe(0.4);
  });

  it("reads the store with useLiveFrame", () => {
    const store = createFrameStore({ nowMs: 1 });
    const { result } = renderHook(() => useLiveFrame(store));
    expect(result.current.nowMs).toBe(1);
    act(() => store.set({ blocks: [], values: null, nowMs: 2 }));
    expect(result.current.nowMs).toBe(2);
  });

  it("reports no reduced-motion preference without matchMedia", () => {
    vi.stubGlobal("matchMedia", undefined);
    expect(prefersReducedMotion()).toBe(false);
  });
});
