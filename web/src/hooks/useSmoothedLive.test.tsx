import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { approach, createFrameStore, NO_PLACES, targetValues } from "@/lib/smoothing";
import type { BlockPoint, LiveSnapshot, OwnerAction } from "@/types";
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
    ethUsd: null,
  };
}

/** A snapshot with only a short window, so nothing drains between samples and idle frames stay idle. */
function shortOnly(n: number): LiveSnapshot {
  const s = snapshot(n);
  return { ...s, constraints: s.constraints.slice(0, 1) };
}

/** A ring that never changes identity: a fresh array each render would publish a frame the loop did not ask for. */
const NO_BLOCKS: BlockPoint[] = [];

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
    const { result, rerender } = renderHook(({ s }) => useSmoothedLive({ snapshot: s, recentBlocks: NO_BLOCKS }), { initialProps: { s: snapshot(1) as LiveSnapshot | null } });
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
    const published = result.current.frame.get();
    runFrame(282);
    expect(result.current.frame.get()).toBe(published);
    runFrame(298);
    const second = result.current.frame.get().values?.baseFeeGwei ?? 0;
    expect(second).toBeGreaterThan(first);
    let t = 298;
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

  it("publishes eased values at the value interval rather than on every frame", () => {
    // A backlog small enough that the tween eases rather than settling inside its tolerance, so the size
    // of one step is what the assertions below can see.
    const only = snapshot(1, "399726000", 500_000_000_000);
    const { result } = renderHook(() => useSmoothedLive({ snapshot: only, recentBlocks: NO_BLOCKS }));
    runFrame(16);
    const first = result.current.frame.get();
    // 16 ms on the long window has drained further, but nothing new is published.
    runFrame(32);
    expect(result.current.frame.get()).toBe(first);
    // 32 ms after the last publish the figures move again, and by a whole 32 ms of easing: a skipped
    // frame lengthens the next step rather than halving the rate the tween approaches its target at.
    runFrame(48);
    const second = result.current.frame.get();
    expect(second).not.toBe(first);
    const target = targetValues(only, [], 48 / 1000);
    const stepped = approach(first.values?.backlogs[1] ?? 0, target.backlogs[1], 32);
    expect(second.values?.backlogs[1]).toBeCloseTo(stepped, 0);
    expect(stepped).toBeLessThan(approach(first.values?.backlogs[1] ?? 0, target.backlogs[1], 16));
    runFrame(64);
    expect(result.current.frame.get()).toBe(second);
  });

  it("drains long windows toward a moving target so a new sample never snaps the gauge", () => {
    const { result, rerender } = renderHook(({ s }) => useSmoothedLive({ snapshot: s, recentBlocks: NO_BLOCKS }), { initialProps: { s: snapshot(1) } });
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
    const { result, rerender } = renderHook(({ s }) => useSmoothedLive({ snapshot: s, recentBlocks: NO_BLOCKS }), { initialProps: { s: snapshot(1) } });
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
    const { result, rerender } = renderHook(({ s, reorgs }) => useSmoothedLive({ snapshot: s, recentBlocks: NO_BLOCKS, reorgs }), { initialProps: { s: snapshot(1), reorgs: 0 } });
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

  it("snaps and stops averaging across a constraint definition change", () => {
    // A short window, so the displayed backlog is the 2 s average of the ring.
    const ring = [block(1, 1000, [4_000_000]), block(2, 1000, [8_000_000])];
    const before = shortOnly(1);
    const { result, rerender } = renderHook(({ s, blocks }) => useSmoothedLive({ snapshot: s, recentBlocks: blocks }), { initialProps: { s: before, blocks: ring } });
    runFrame(16);
    let t = 16;
    for (let i = 0; i < 400; i++) runFrame((t += 16));
    expect(result.current.frame.get().values?.backlogs[0]).toBe(6_000_000);
    // The owner replaces the constraint with another of the same shape but a
    // different window. Nothing eases into it, and the average does not reach
    // back over blocks priced under the old definition.
    const after: LiveSnapshot = { ...before, block: { ...before.block, number: 3 }, constraints: [{ ...before.constraints[0], window: 30, backlog: 1_000_000 }] };
    clock = t;
    rerender({ s: after, blocks: [...ring, block(3, 1000, [20_000_000])] });
    // The new sample commits at the next cadence, and takes its definition with it.
    runFrame((t += 250));
    const values = result.current.frame.get().values;
    // Block 3 is the only one known to be priced under the new definition.
    expect(values?.backlogs[0]).toBe(20_000_000);
    expect(values?.bips).toEqual(targetValues(after, [block(3, 1000, [20_000_000])], 0).bips);
    // Later blocks under the same definition join the average again.
    rerender({ s: after, blocks: [...ring, block(3, 1000, [20_000_000]), block(4, 1000, [30_000_000])] });
    for (let i = 0; i < 400; i++) runFrame((t += 16));
    expect(result.current.frame.get().values?.backlogs[0]).toBe(25_000_000);
  });

  it("consumes the fresh flag only for a sample that arrived after the reorg", () => {
    const { result, rerender } = renderHook(({ s, reorgs }) => useSmoothedLive({ snapshot: s, recentBlocks: NO_BLOCKS, reorgs }), { initialProps: { s: snapshot(1), reorgs: 0 } });
    runFrame(16);
    // A tick lands inside the cadence window, then the reorg that orphans it
    // arrives. That pending sample must not be committed as the fresh one.
    clock = 32;
    const orphan = snapshot(2, "800000000");
    rerender({ s: orphan, reorgs: 0 });
    rerender({ s: orphan, reorgs: 1 });
    runFrame(48);
    expect(result.current.display?.block.number).toBe(1);
    // The tick that follows the reorg is the fresh one: shown at once, snapped.
    clock = 64;
    rerender({ s: snapshot(3, "400000000"), reorgs: 1 });
    runFrame(80);
    expect(result.current.display?.block.number).toBe(3);
    expect(result.current.frame.get().values?.baseFeeGwei).toBe(0.4);
  });

  it("clears the display for a reorg that orphaned the snapshot and says it is resyncing", () => {
    const { result, rerender } = renderHook(({ s, reorgs, resyncing }) => useSmoothedLive({ snapshot: s, recentBlocks: NO_BLOCKS, reorgs, resyncing }), {
      initialProps: { s: snapshot(1) as LiveSnapshot | null, reorgs: 0, resyncing: false },
    });
    runFrame(16);
    expect(result.current.display?.block.number).toBe(1);
    expect(result.current.resyncing).toBe(false);
    // useLive drops the orphaned snapshot with the ring it was priced on.
    rerender({ s: null, reorgs: 1, resyncing: true });
    expect(result.current.display).toBeNull();
    expect(result.current.resyncing).toBe(true);
    expect(result.current.frame.get().values).toBeNull();
    clock = 32;
    rerender({ s: snapshot(4, "800000000"), reorgs: 1, resyncing: false });
    runFrame(48);
    expect(result.current.display?.block.number).toBe(4);
    expect(result.current.resyncing).toBe(false);
    // Nothing was eased from the orphan: the canonical tick is shown as it is.
    expect(result.current.frame.get().values?.baseFeeGwei).toBe(0.8);
  });

  it("follows a reduced-motion preference that changes while the page is open", () => {
    const listeners = new Set<() => void>();
    let matches = false;
    vi.stubGlobal("matchMedia", () => ({
      get matches() {
        return matches;
      },
      addEventListener: (_: string, listener: () => void) => listeners.add(listener),
      removeEventListener: (_: string, listener: () => void) => listeners.delete(listener),
    }));
    const { result, rerender } = renderHook(({ s }) => useSmoothedLive({ snapshot: s, recentBlocks: NO_BLOCKS }), { initialProps: { s: snapshot(1) } });
    runFrame(16);
    clock = 16;
    rerender({ s: snapshot(2, "800000000") });
    runFrame(300);
    // Motion is allowed: the figure eases toward the new sample.
    const eased = result.current.frame.get().values?.baseFeeGwei ?? 0;
    expect(eased).toBeGreaterThan(0.399726);
    expect(eased).toBeLessThan(0.8);
    // The preference changes while the loop runs; the next frame respects it.
    matches = true;
    act(() => listeners.forEach((listener) => listener()));
    runFrame(316);
    expect(result.current.frame.get().values?.baseFeeGwei).toBe(0.8);
    // And back again: the tween resumes without restarting the loop.
    matches = false;
    act(() => listeners.forEach((listener) => listener()));
    clock = 320;
    rerender({ s: snapshot(3, "400000000") });
    runFrame(600);
    const back = result.current.frame.get().values?.baseFeeGwei ?? 0;
    expect(back).toBeGreaterThan(0.4);
    expect(back).toBeLessThan(0.8);
  });

  it("resumes easing where the figures are when reduced motion is turned off after a quiet stretch", () => {
    const listeners = new Set<() => void>();
    let matches = true;
    vi.stubGlobal("matchMedia", () => ({
      get matches() {
        return matches;
      },
      addEventListener: (_: string, listener: () => void) => listeners.add(listener),
      removeEventListener: (_: string, listener: () => void) => listeners.delete(listener),
    }));
    const { result, rerender } = renderHook(({ s }) => useSmoothedLive({ snapshot: s, recentBlocks: NO_BLOCKS }), { initialProps: { s: snapshot(1) } });
    runFrame(16);
    expect(result.current.frame.get().values?.baseFeeGwei).toBe(0.399726);
    // A second of frames with the preference on: the figures stand still, and the tween clock keeps pace
    // with them rather than banking the whole stretch.
    let t = 16;
    for (let i = 0; i < 63; i++) runFrame((t += 16));
    expect(result.current.frame.get().values?.baseFeeGwei).toBe(0.399726);
    // Motion is allowed again and the next cadence commits a much higher sample. One frame of easing is
    // one frame, not the second the preference was on for.
    matches = false;
    act(() => listeners.forEach((listener) => listener()));
    clock = t;
    rerender({ s: snapshot(2, "800000000") });
    runFrame((t += 16));
    expect(result.current.display?.block.number).toBe(2);
    const eased = result.current.frame.get().values?.baseFeeGwei ?? 0;
    expect(eased).toBeGreaterThan(0.4);
    expect(eased).toBeLessThan(0.45);
  });

  it("publishes a placement of the ring, assigned once per block and evicted with it", () => {
    const ring = [block(1, 1000, [10]), block(2, 1000, [20]), block(3, 1001, [30])];
    const { result, rerender } = renderHook(({ blocks }) => useSmoothedLive({ snapshot: snapshot(1), recentBlocks: blocks }), { initialProps: { blocks: ring } });
    runFrame(16);
    const first = result.current.frame.get().places;
    expect([...first.entries()]).toEqual([
      [1, 1000],
      [2, 1000.5],
      [3, 1001],
    ]);
    // A second block lands in the newest second: the one already there keeps
    // its place rather than being recomputed with it.
    clock = 16;
    rerender({ blocks: [...ring, block(4, 1001, [40])] });
    runFrame(300);
    const grown = result.current.frame.get().places;
    expect(grown.get(3)).toBe(1001);
    expect(grown.get(4)).toBe(1001.5);
    // The oldest block leaves the ring and loses its place with it.
    clock = 300;
    rerender({ blocks: [block(2, 1000, [20]), block(3, 1001, [30]), block(4, 1001, [40])] });
    runFrame(600);
    const evicted = result.current.frame.get().places;
    expect(evicted.has(1)).toBe(false);
    expect(evicted.get(2)).toBe(1000.5);
    expect(evicted.get(3)).toBe(1001);
  });

  it("snaps and stops averaging across an owner call that reinstalls the same constraints", () => {
    const blocks = [block(10, 1000, [3_000_000, 0]), block(11, 1000, [3_100_000, 0])];
    const replaced: OwnerAction = { block: 12, at: "2026-09-06T07:20:01Z", txHash: "0xabc", method: "setGasPricingConstraints", selector: "0xcc0d556a", args: { constraints: [] } };
    const { result, rerender } = renderHook(
      ({ actions, s, recentBlocks }) => useSmoothedLive({ snapshot: s, recentBlocks, ownerActions: actions }),
      { initialProps: { actions: [] as OwnerAction[], s: snapshot(11, "399726000"), recentBlocks: blocks } },
    );
    runFrame(16);
    expect(result.current.frame.get().values?.baseFeeGwei).toBe(0.399726);

    // The owner reinstalls the very same targets and windows with new starting
    // backlogs. The signature does not change, so nothing used to snap: the
    // figures eased through state the chain never had and the short-window
    // average still counted blocks from before the reset.
    const after = [...blocks, block(12, 1000, [9_000_000, 0])];
    clock = 16;
    rerender({ actions: [replaced], s: snapshot(12, "800000000"), recentBlocks: after });
    runFrame(300);
    // Snapped, not eased a third of the way there.
    expect(result.current.frame.get().values?.baseFeeGwei).toBe(0.8);
    // And the short-window average is block 12 alone, not the mean of the three.
    expect(result.current.frame.get().values?.backlogs[0]).toBe(9_000_000);
  });

  it("runs no frame loop at all while it is disabled", () => {
    const { result } = renderHook(() => useSmoothedLive({ snapshot: snapshot(1), recentBlocks: [block(1, 1000, [1])] }, false));
    expect(frames).toHaveLength(0);
    expect(result.current.display).toBeNull();
    expect(result.current.frame.get().values).toBeNull();
  });

  it("reads the store with useLiveFrame", () => {
    const store = createFrameStore({ nowMs: 1 });
    const { result } = renderHook(() => useLiveFrame(store));
    expect(result.current.nowMs).toBe(1);
    act(() => store.set({ blocks: [], places: NO_PLACES, values: null, nowMs: 2 }));
    expect(result.current.nowMs).toBe(2);
  });

  it("reports no reduced-motion preference without matchMedia", () => {
    vi.stubGlobal("matchMedia", undefined);
    expect(prefersReducedMotion()).toBe(false);
  });
});
