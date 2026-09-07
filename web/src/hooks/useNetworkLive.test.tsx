import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { BlockPoint, LiveSnapshot, Network } from "@/types";
import type { LiveState } from "./useLive";

// The feed itself is tested against a socket in hooks.test.tsx; here it stands
// in, so what is under test is the one hook a page opens it through.
const feed = vi.hoisted(() => ({ calls: [] as string[], state: null as LiveState | null }));
vi.mock("./useLive", () => ({
  useLive: (network: string) => {
    feed.calls.push(network);
    return feed.state;
  },
}));

import { useNetworkLive } from "./useNetworkLive";

const networkInfo: Network = { name: "robinhood", displayName: "Robinhood Chain", chainId: 4663, explorerUrl: "", model: "constraints", headBlock: 10, headAt: null, lagSeconds: null, enabled: true };

function block(number: number): BlockPoint {
  return { number, ts: 1000, gasUsed: 4_000_000, baseFee: "399726000", predictedBaseFee: "399726000", backlogs: [4_000_000], constraintBips: [], exponentBips: 0, minBaseFee: "20000000", anchored: false };
}

function snapshot(n: number): LiveSnapshot {
  return {
    chainId: 4663,
    sampledAt: "2026-09-06T07:20:00Z",
    block: { number: n, ts: 1000, gasUsed: 4_021_130, baseFee: "399726000", txCount: 90 },
    baseFee: "399726000",
    minBaseFee: "20000000",
    multiplierBips: 199_863,
    exponentBips: 32_425,
    model: "constraints",
    constraints: [{ target: 60_000_000, window: 15, backlog: 3_111_506, exponentBips: 34 }],
    prices: { perL2Tx: "0", perL1CalldataByte: "0", perL2Storage: "0", perArbGasBase: "20000000", perArbGasCongestion: "379726000", perArbGasTotal: "399726000" },
    gasPerSecond: { s10: 38_000_000, s60: 40_500_000 },
    replayErrorBips: 2,
    ethUsd: null,
  };
}

function state(overrides: Partial<LiveState> = {}): LiveState {
  return { snapshot: snapshot(1), recentBlocks: [block(1)], status: "open", networkInfo, ownerActions: [], reorgs: 0, resyncing: false, error: null, ...overrides };
}

let frames: FrameRequestCallback[] = [];

function runFrame(t: number) {
  const cb = frames[frames.length - 1];
  frames = [];
  act(() => cb(t));
}

describe("useNetworkLive", () => {
  beforeEach(() => {
    frames = [];
    feed.calls = [];
    feed.state = state();
    vi.stubGlobal("requestAnimationFrame", (cb: FrameRequestCallback) => {
      frames.push(cb);
      return frames.length;
    });
    vi.stubGlobal("cancelAnimationFrame", vi.fn());
    vi.spyOn(performance, "now").mockReturnValue(0);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("opens one feed for the network and hands back the smoothed view of it", () => {
    const { result, rerender } = renderHook(({ network }) => useNetworkLive(network), { initialProps: { network: "robinhood" } });
    expect(feed.calls).toEqual(["robinhood"]);
    // Nothing is displayed until the loop has committed a tick at the cadence.
    expect(result.current.snapshot).toBeNull();
    runFrame(16);
    expect(result.current.snapshot?.block.number).toBe(1);
    expect(result.current.smooth.display).toBe(result.current.snapshot);
    expect(result.current.smooth.frame.get().blocks).toEqual([block(1)]);
    expect(result.current.status).toBe("open");
    expect(result.current.networkInfo).toBe(networkInfo);
    expect(result.current.live).toBe(feed.state);

    // The same feed, re-rendered: the same object, so a page that follows it
    // does not re-render for nothing.
    const first = result.current;
    rerender({ network: "robinhood" });
    expect(result.current).toBe(first);
  });

  it("passes a network change on to the feed", () => {
    const { rerender } = renderHook(({ network }) => useNetworkLive(network), { initialProps: { network: "robinhood" } });
    feed.state = state({ snapshot: null, recentBlocks: [], status: "connecting", networkInfo: null });
    rerender({ network: "arbitrum-one" });
    expect(feed.calls).toEqual(["robinhood", "arbitrum-one"]);
  });
});
