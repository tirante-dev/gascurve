import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { BlockPoint, LiveSnapshot, OwnerAction, Series } from "@/types";
import { SOCKET_OPEN, type SocketLike } from "@/lib/api/ws";

const pushMock = vi.fn();
const replaceMock = vi.fn();
let routeParams: { network?: string | string[] } = { network: "robinhood" };
vi.mock("next/navigation", () => ({
  useParams: () => routeParams,
  useRouter: () => ({ push: pushMock, replace: replaceMock }),
}));

class FakeSocket implements SocketLike {
  static instances: FakeSocket[] = [];
  onopen: ((ev: Event) => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onclose: ((ev: CloseEvent) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  readyState = 0;
  sent: string[] = [];
  readonly url: string;
  constructor(url: string) {
    this.url = url;
    FakeSocket.instances.push(this);
  }
  send(data: string): void {
    this.sent.push(data);
  }
  close(): void {
    this.readyState = 3;
    this.onclose?.(new CloseEvent("close"));
  }
  serverOpen(): void {
    this.readyState = SOCKET_OPEN;
    this.onopen?.(new Event("open"));
  }
  serverMessage(payload: unknown): void {
    this.onmessage?.(new MessageEvent("message", { data: JSON.stringify(payload) }));
  }
  serverHello(name: string, chainId: number, snapshotBlock: number | null, recentBlocks: BlockPoint[] = []): void {
    this.serverMessage({ type: "hello", data: { network: { name, chainId }, snapshot: snapshotBlock === null ? null : snapshot(snapshotBlock, chainId), recentBlocks } });
  }
  serverDrop(): void {
    this.readyState = 3;
    this.onclose?.(new CloseEvent("close"));
  }
}

vi.mock("@/lib/api/ws", async (importOriginal) => {
  const original = await importOriginal<typeof import("@/lib/api/ws")>();
  return {
    ...original,
    resolveSocketFactory: vi.fn(async () => (url: string) => new FakeSocket(url)),
  };
});

const getLiveMock = vi.fn();
vi.mock("@/lib/api/live", () => ({
  getLive: (...args: unknown[]) => getLiveMock(...args),
}));

const getSeriesMock = vi.fn();
vi.mock("@/lib/api/series", async (importOriginal) => {
  const original = await importOriginal<typeof import("@/lib/api/series")>();
  return { ...original, getSeries: (...args: unknown[]) => getSeriesMock(...args) };
});

import { appendBlocks, applyReorg, feedMatches, isNewerSnapshot, useLive } from "./useLive";
import { useApi } from "./useApi";
import { fetchSharedSeries, refetchIntervalFor, resetSharedSeries, seriesKey, useRefreshOnOwnerAction, useSeries } from "./useSeries";
import { DEFAULT_NETWORK, isValidNetworkName, NETWORK_STORAGE_KEY, readStoredNetwork, storeNetwork, useNetwork } from "./useNetwork";
import { useDocumentVisible } from "./useDocumentVisible";

function block(number: number): BlockPoint {
  return { number, ts: 1, gasUsed: 1, baseFee: "1", predictedBaseFee: "1", backlogs: [], constraintBips: [], exponentBips: 0, minBaseFee: "1", anchored: false };
}

function snapshot(n: number, chainId = 4663, sampledAt = "2026-09-06T07:20:00Z"): LiveSnapshot {
  return {
    chainId,
    sampledAt,
    block: { number: n, ts: 1, gasUsed: 1, baseFee: "1", txCount: 1 },
    baseFee: "1",
    minBaseFee: "1",
    multiplierBips: 10000,
    exponentBips: 0,
    model: "constraints",
    constraints: [],
    prices: { perL2Tx: "0", perL1CalldataByte: "0", perL2Storage: "0", perArbGasBase: "1", perArbGasCongestion: "0", perArbGasTotal: "1" },
    gasPerSecond: { s10: 0, s60: 0 },
    replayErrorBips: 0,
    ethUsd: null,
  };
}

function latest(): FakeSocket {
  return FakeSocket.instances[FakeSocket.instances.length - 1];
}

async function flush(ms = 0) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

function setHidden(hidden: boolean) {
  Object.defineProperty(document, "hidden", { configurable: true, get: () => hidden });
  document.dispatchEvent(new Event("visibilitychange"));
}

describe("appendBlocks", () => {
  it("keeps a bounded ring without duplicates", () => {
    const ring = appendBlocks([], [block(3), block(1), block(2)], 2);
    expect(ring.map((b) => b.number)).toEqual([2, 3]);
    expect(appendBlocks(ring, [block(2), block(3)])).toBe(ring);
    expect(appendBlocks(ring, [block(4)], 2).map((b) => b.number)).toEqual([3, 4]);
  });
});

describe("applyReorg", () => {
  it("drops the blocks above the ancestor and appends the canonical ones, within the ring size", () => {
    const ring = [block(9), block(10), block(11), block(12)];
    const canonical = [{ ...block(11), baseFee: "2" }, { ...block(12), baseFee: "2" }, block(13)];
    const next = applyReorg(ring, 10, canonical, 3);
    expect(next.map((b) => [b.number, b.baseFee])).toEqual([
      [11, "2"],
      [12, "2"],
      [13, "1"],
    ]);
    expect(applyReorg(ring, 10, canonical).map((b) => b.number)).toEqual([9, 10, 11, 12, 13]);
    // Nothing above the ancestor and nothing new: the ring is kept as it is.
    expect(applyReorg(ring, 12, [])).toBe(ring);
    expect(applyReorg(ring, 12, [block(12)])).toBe(ring);
    // A fork deeper than the ring empties it before the canonical blocks land.
    expect(applyReorg(ring, 5, [block(6)]).map((b) => b.number)).toEqual([6]);
    expect(applyReorg([], 5, [block(6)]).map((b) => b.number)).toEqual([6]);
  });
});

describe("isNewerSnapshot", () => {
  it("accepts a higher block, or the same block sampled later", () => {
    expect(isNewerSnapshot(null, snapshot(1))).toBe(true);
    expect(isNewerSnapshot(snapshot(10), snapshot(11))).toBe(true);
    expect(isNewerSnapshot(snapshot(10), snapshot(9))).toBe(false);
    expect(isNewerSnapshot(snapshot(10), snapshot(10, 4663, "2026-09-06T07:20:01Z"))).toBe(true);
    expect(isNewerSnapshot(snapshot(10, 4663, "2026-09-06T07:20:01Z"), snapshot(10))).toBe(false);
    expect(isNewerSnapshot(snapshot(10), snapshot(10))).toBe(false);
    expect(isNewerSnapshot(snapshot(10, 4663, "garbage"), snapshot(10))).toBe(false);
  });
});

describe("useLive", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    FakeSocket.instances = [];
    getLiveMock.mockReset();
    setHidden(false);
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("connects, applies hello, ticks and blocks, and polls while disconnected", async () => {
    const { result, rerender, unmount } = renderHook(({ network }) => useLive(network), { initialProps: { network: "robinhood" } });
    expect(result.current.status).toBe("connecting");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(FakeSocket.instances).toHaveLength(1);
    const socket = latest();
    expect(socket.url).toBe("ws://localhost:8080/api/v1/ws?network=robinhood");
    act(() => socket.serverOpen());
    // A TCP open is not live yet; the hello is.
    expect(result.current.status).toBe("connecting");
    act(() => socket.serverHello("robinhood", 4663, 10, [block(9), block(10)]));
    expect(result.current.status).toBe("open");
    expect(result.current.snapshot?.block.number).toBe(10);
    expect(result.current.recentBlocks.map((b) => b.number)).toEqual([9, 10]);
    expect(result.current.networkInfo?.name).toBe("robinhood");
    act(() => socket.serverMessage({ type: "tick", data: snapshot(11) }));
    expect(result.current.snapshot?.block.number).toBe(11);
    act(() => socket.serverMessage({ type: "blocks", data: [block(11), block(12)] }));
    expect(result.current.recentBlocks.map((b) => b.number)).toEqual([9, 10, 11, 12]);
    act(() => socket.serverMessage({ type: "owner_action", data: { block: 1, method: "setSpeedLimit" } }));
    expect(result.current.ownerActions).toHaveLength(1);

    // Drop the socket: status goes to reconnecting and polling starts.
    getLiveMock.mockResolvedValue(snapshot(20));
    act(() => socket.serverDrop());
    expect(result.current.status).toBe("reconnecting");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(getLiveMock).toHaveBeenCalledTimes(1);
    expect(result.current.snapshot?.block.number).toBe(20);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000);
    });
    // The reconnect attempt opened a second socket after 1 s.
    expect(FakeSocket.instances).toHaveLength(2);
    getLiveMock.mockRejectedValueOnce(new Error("api down"));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000);
    });
    expect(getLiveMock).toHaveBeenCalledTimes(2);
    expect(result.current.error).toBe("api down");

    // Reopen: polling carries on until the hello confirms the subscription, then stops.
    getLiveMock.mockResolvedValue(snapshot(20));
    act(() => latest().serverOpen());
    expect(result.current.status).toBe("reconnecting");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2000);
    });
    expect(getLiveMock).toHaveBeenCalledTimes(3);
    act(() => latest().serverHello("robinhood", 4663, 21));
    expect(result.current.status).toBe("open");
    expect(result.current.snapshot?.block.number).toBe(21);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000);
    });
    expect(getLiveMock).toHaveBeenCalledTimes(3);

    // Switching network subscribes on the same socket, clears state and is pending until the hello.
    rerender({ network: "arbitrum-one" });
    expect(latest().sent).toContain(JSON.stringify({ type: "subscribe", network: "arbitrum-one" }));
    expect(result.current.status).toBe("connecting");
    expect(result.current.snapshot).toBeNull();
    expect(result.current.recentBlocks).toEqual([]);
    // Robinhood messages queued behind the subscribe never show up as Arbitrum One.
    act(() => latest().serverMessage({ type: "tick", data: snapshot(22) }));
    act(() => latest().serverMessage({ type: "blocks", data: [block(22)] }));
    act(() => latest().serverMessage({ type: "owner_action", data: { block: 2, method: "setSpeedLimit" } }));
    expect(result.current.snapshot).toBeNull();
    expect(result.current.recentBlocks).toEqual([]);
    expect(result.current.ownerActions).toEqual([]);
    act(() => latest().serverHello("arbitrum-one", 42161, 500, [block(500)]));
    expect(result.current.status).toBe("open");
    expect(result.current.snapshot?.chainId).toBe(42161);
    expect(result.current.networkInfo?.name).toBe("arbitrum-one");
    act(() => latest().serverMessage({ type: "tick", data: snapshot(23, 4663) }));
    expect(result.current.snapshot?.block.number).toBe(500);
    act(() => latest().serverMessage({ type: "tick", data: snapshot(501, 42161) }));
    expect(result.current.snapshot?.block.number).toBe(501);
    act(() => latest().serverMessage({ type: "blocks", data: [block(501)] }));
    expect(result.current.recentBlocks.map((b) => b.number)).toEqual([500, 501]);
    act(() => latest().serverMessage({ type: "owner_action", data: { block: 3, method: "setSpeedLimit" } }));
    expect(result.current.ownerActions).toHaveLength(1);

    // Hiding the tab suspends the socket; showing it resumes.
    act(() => setHidden(true));
    expect(latest().readyState).toBe(3);
    expect(FakeSocket.instances).toHaveLength(2);
    act(() => setHidden(false));
    expect(FakeSocket.instances).toHaveLength(3);
    expect(latest().url).toContain("network=arbitrum-one");

    unmount();
    expect(latest().readyState).toBe(3);
  });

  it("replaces the ring's tail on a reorg and counts it, and ignores another chain's", async () => {
    const { result } = renderHook(() => useLive("robinhood"));
    await flush();
    const socket = latest();
    act(() => socket.serverOpen());
    act(() => socket.serverHello("robinhood", 4663, 12, [block(9), block(10), block(11), block(12)]));
    expect(result.current.reorgs).toBe(0);
    const canonical = [
      { ...block(11), baseFee: "7" },
      { ...block(12), baseFee: "7" },
    ];
    act(() => socket.serverMessage({ type: "reorg", data: { chainId: 4663, ancestor: 10, blocks: canonical } }));
    expect(result.current.recentBlocks.map((b) => [b.number, b.baseFee])).toEqual([
      [9, "1"],
      [10, "1"],
      [11, "7"],
      [12, "7"],
    ]);
    expect(result.current.reorgs).toBe(1);
    // The snapshot was taken on an orphaned block, so it goes with the blocks
    // it was priced on: the feed reports itself resyncing rather than showing
    // a canonical ring under an orphan snapshot.
    expect(result.current.snapshot).toBeNull();
    expect(result.current.resyncing).toBe(true);
    act(() => socket.serverMessage({ type: "tick", data: snapshot(13) }));
    act(() => socket.serverMessage({ type: "blocks", data: [block(13)] }));
    expect(result.current.snapshot?.block.number).toBe(13);
    expect(result.current.resyncing).toBe(false);
    expect(result.current.recentBlocks.map((b) => b.number)).toEqual([9, 10, 11, 12, 13]);
    // Another chain's reorg is not this feed's.
    act(() => socket.serverMessage({ type: "reorg", data: { chainId: 42161, ancestor: 1, blocks: [] } }));
    expect(result.current.reorgs).toBe(1);
    expect(result.current.recentBlocks).toHaveLength(5);
    // A new hello starts the count over.
    act(() => socket.serverHello("robinhood", 4663, 20, [block(20)]));
    expect(result.current.reorgs).toBe(0);
  });

  it("keeps a snapshot the reorg does not orphan, and never shows a canonical ring under an orphan sample", async () => {
    const { result } = renderHook(() => useLive("robinhood"));
    await flush();
    const socket = latest();
    act(() => socket.serverOpen());
    act(() => socket.serverHello("robinhood", 4663, 10, [block(9), block(10), block(11)]));
    // The ancestor is at or above the sampled block: the sample is still
    // canonical, so the last canonical state stays on screen.
    act(() => socket.serverMessage({ type: "reorg", data: { chainId: 4663, ancestor: 10, blocks: [block(11)] } }));
    expect(result.current.snapshot?.block.number).toBe(10);
    expect(result.current.resyncing).toBe(false);
    expect(result.current.reorgs).toBe(1);
    // A deeper one does orphan it, and a poll result clears the resync too.
    act(() => socket.serverMessage({ type: "reorg", data: { chainId: 4663, ancestor: 8, blocks: [block(9)] } }));
    expect(result.current.snapshot).toBeNull();
    expect(result.current.resyncing).toBe(true);
    expect(result.current.recentBlocks.map((b) => b.number)).toEqual([9]);
    getLiveMock.mockResolvedValue(snapshot(9));
    act(() => socket.serverDrop());
    await flush();
    expect(result.current.snapshot?.block.number).toBe(9);
    expect(result.current.resyncing).toBe(false);
  });

  it("keeps live owner actions across a same-chain hello and drops the ones a reorg orphans", async () => {
    const { result, rerender } = renderHook(({ network }) => useLive(network), { initialProps: { network: "robinhood" } });
    await flush();
    const socket = latest();
    act(() => socket.serverOpen());
    act(() => socket.serverHello("robinhood", 4663, 12, [block(12)]));
    act(() => socket.serverMessage({ type: "owner_action", data: { block: 8, method: "setSpeedLimit" } }));
    act(() => socket.serverMessage({ type: "owner_action", data: { block: 12, method: "setMinimumL2BaseFee" } }));
    expect(result.current.ownerActions.map((a) => a.block)).toEqual([12, 8]);
    // A reconnect to the same chain: the hello carries no owner actions, so
    // the ones seen over the socket must survive it.
    getLiveMock.mockResolvedValue(snapshot(12));
    act(() => socket.serverDrop());
    await flush(1000);
    const second = latest();
    act(() => second.serverOpen());
    act(() => second.serverHello("robinhood", 4663, 13, [block(13)]));
    expect(result.current.ownerActions.map((a) => a.block)).toEqual([12, 8]);
    // A reorg below block 12 orphans that action; the one at block 8 stands.
    act(() => second.serverMessage({ type: "reorg", data: { chainId: 4663, ancestor: 10, blocks: [block(11)] } }));
    expect(result.current.ownerActions.map((a) => a.block)).toEqual([8]);
    // Another chain starts empty.
    rerender({ network: "arbitrum-one" });
    act(() => second.serverHello("arbitrum-one", 42161, 1, [block(1)]));
    expect(result.current.ownerActions).toEqual([]);
  });

  it("keeps the feed when the route moves between a chain id and the network's name", async () => {
    const { result, rerender } = renderHook(({ network }) => useLive(network), { initialProps: { network: "4663" } });
    await flush();
    const socket = latest();
    expect(socket.url).toContain("network=4663");
    act(() => socket.serverOpen());
    act(() => socket.serverHello("robinhood", 4663, 10, [block(10)]));
    expect(result.current.snapshot?.block.number).toBe(10);
    expect(result.current.networkInfo?.name).toBe("robinhood");
    // The page canonicalises the route. Nothing is sent, nothing is dropped, nothing waits for a hello.
    rerender({ network: "robinhood" });
    expect(socket.sent).toEqual([]);
    expect(result.current.status).toBe("open");
    expect(result.current.snapshot?.block.number).toBe(10);
    expect(result.current.recentBlocks.map((b) => b.number)).toEqual([10]);
    expect(result.current.networkInfo?.chainId).toBe(4663);
    act(() => socket.serverMessage({ type: "tick", data: snapshot(11) }));
    act(() => socket.serverMessage({ type: "blocks", data: [block(11)] }));
    expect(result.current.snapshot?.block.number).toBe(11);
    expect(result.current.recentBlocks.map((b) => b.number)).toEqual([10, 11]);
    // And the other way round.
    rerender({ network: "4663" });
    expect(socket.sent).toEqual([]);
    expect(result.current.snapshot?.block.number).toBe(11);
    // A genuinely different network still clears the feed.
    rerender({ network: "arbitrum-one" });
    expect(socket.sent).toEqual([JSON.stringify({ type: "subscribe", network: "arbitrum-one" })]);
    expect(result.current.snapshot).toBeNull();
  });

  it("surfaces a subscription error, falls back to polling, and lets the hello replace a polled feed", async () => {
    getLiveMock.mockResolvedValue(snapshot(30));
    const { result } = renderHook(() => useLive("4663"));
    await flush();
    const first = latest();
    act(() => first.serverOpen());
    act(() => first.serverMessage({ type: "error", error: { code: "unavailable", message: "database is warming up" } }));
    expect(result.current.error).toBe("database is warming up");
    expect(first.readyState).toBe(3);
    expect(result.current.status).toBe("reconnecting");
    await flush();
    // The polled snapshot is shown under the chain-id route.
    expect(getLiveMock).toHaveBeenCalledWith("4663", expect.anything());
    expect(result.current.snapshot?.block.number).toBe(30);
    expect(result.current.error).toBeNull();
    await flush(1000);
    const second = latest();
    act(() => second.serverOpen());
    act(() => second.serverHello("robinhood", 4663, 31));
    expect(result.current.status).toBe("open");
    expect(result.current.snapshot?.block.number).toBe(31);
    expect(result.current.networkInfo?.name).toBe("robinhood");
  });

  it("matches a feed by name or chain id", () => {
    expect(feedMatches({ key: null }, "robinhood")).toBe(false);
    expect(feedMatches({ key: { name: "robinhood", chainId: 4663 } }, "4663")).toBe(true);
    expect(feedMatches({ key: { name: "robinhood", chainId: 4663 } }, "robinhood")).toBe(true);
    expect(feedMatches({ key: { name: "robinhood", chainId: 4663 } }, "arbitrum-one")).toBe(false);
  });

  it("polls one request at a time and never lets a stale response overwrite a newer one", async () => {
    const pending: { block: number; resolve: (s: LiveSnapshot) => void }[] = [];
    getLiveMock.mockImplementation(() => new Promise<LiveSnapshot>((resolve) => pending.push({ block: pending.length, resolve })));
    const { result, unmount } = renderHook(() => useLive("robinhood"));
    await flush();
    act(() => latest().serverDrop());
    expect(result.current.status).toBe("reconnecting");
    await flush();
    expect(getLiveMock).toHaveBeenCalledTimes(1);
    // Ten seconds pass with the first request still in flight: no second request is started.
    await flush(10_000);
    expect(getLiveMock).toHaveBeenCalledTimes(1);
    await act(async () => {
      pending[0].resolve(snapshot(30));
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(result.current.snapshot?.block.number).toBe(30);
    // The next poll starts 2 s after the previous one settled.
    await flush(1999);
    expect(getLiveMock).toHaveBeenCalledTimes(1);
    await flush(1);
    expect(getLiveMock).toHaveBeenCalledTimes(2);
    // An older snapshot (the server answered from a lagging replica) is rejected.
    await act(async () => {
      pending[1].resolve(snapshot(29));
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(result.current.snapshot?.block.number).toBe(30);
    await flush(2000);
    // The same block sampled later is accepted.
    await act(async () => {
      pending[2].resolve(snapshot(30, 4663, "2026-09-06T07:20:05Z"));
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(result.current.snapshot?.sampledAt).toBe("2026-09-06T07:20:05Z");
    unmount();
    // No poll is scheduled after unmount.
    await flush(10_000);
    expect(getLiveMock).toHaveBeenCalledTimes(3);
  });

  it("does not open a socket while the tab is hidden at mount, and connects once it becomes visible", async () => {
    setHidden(true);
    const { result, unmount } = renderHook(() => useLive("robinhood"));
    await flush();
    expect(FakeSocket.instances).toHaveLength(0);
    expect(result.current.status).toBe("connecting");
    await flush(5000);
    expect(FakeSocket.instances).toHaveLength(0);
    act(() => setHidden(false));
    expect(FakeSocket.instances).toHaveLength(1);
    expect(latest().url).toContain("network=robinhood");
    act(() => setHidden(true));
    expect(latest().readyState).toBe(3);
    unmount();
  });

  it("does not poll while hidden", async () => {
    getLiveMock.mockResolvedValue(snapshot(1));
    const { result, unmount } = renderHook(() => useLive("robinhood"));
    await flush();
    act(() => latest().serverDrop());
    expect(result.current.status).toBe("reconnecting");
    await flush();
    expect(getLiveMock).toHaveBeenCalledTimes(1);
    act(() => setHidden(true));
    await flush(10_000);
    expect(getLiveMock).toHaveBeenCalledTimes(1);
    expect(FakeSocket.instances).toHaveLength(1);
    unmount();
  });

  it("opens nothing at all while it is disabled, and connects when it is not", async () => {
    // A chart page drawing only bucketed history has no use for the feed, so
    // it holds no socket open and does no polling.
    getLiveMock.mockResolvedValue(snapshot(20));
    const { result, rerender } = renderHook(({ on }) => useLive("robinhood", on), { initialProps: { on: false } });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(FakeSocket.instances).toHaveLength(0);
    expect(result.current.snapshot).toBeNull();
    await flush(10_000);
    expect(getLiveMock).not.toHaveBeenCalled();
    // Turned on, it behaves exactly as it always has.
    rerender({ on: true });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(FakeSocket.instances).toHaveLength(1);
  });

  it("ignores a socket factory that resolves after unmount", async () => {
    const { unmount } = renderHook(() => useLive("robinhood"));
    unmount();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(FakeSocket.instances).toHaveLength(0);
  });
});

describe("useApi and useSeries", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    getSeriesMock.mockReset();
    resetSharedSeries();
    setHidden(false);
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("fetches, keeps previous data while loading, refetches on an interval and surfaces errors", async () => {
    const series = (range: string): Series => ({ range: range as Series["range"], resolution: "5s", from: 0, to: 0, constraintSets: [], ownerActions: [], points: [] });
    getSeriesMock.mockImplementation(async (_n: unknown, range: string) => series(range));
    const { result, rerender } = renderHook(({ range }) => useSeries("robinhood", range), {
      initialProps: { range: "1h" as Series["range"] },
    });
    expect(result.current.loading).toBe(true);
    await flush();
    expect(result.current.data?.range).toBe("1h");
    expect(result.current.loading).toBe(false);
    expect(result.current.updatedAt).not.toBeNull();
    expect(getSeriesMock).toHaveBeenCalledWith("robinhood", "1h", expect.anything());

    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(getSeriesMock).toHaveBeenCalledTimes(2);

    rerender({ range: "30d" });
    expect(result.current.data).toBeNull();
    await flush();
    expect(result.current.data?.range).toBe("30d");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(120_000);
    });
    expect(getSeriesMock).toHaveBeenCalledTimes(3);

    getSeriesMock.mockRejectedValueOnce(new Error("boom"));
    act(() => result.current.refresh());
    await flush();
    expect(result.current.error).toBe("boom");
    expect(result.current.data?.range).toBe("30d");

    getSeriesMock.mockRejectedValueOnce("string failure");
    act(() => result.current.refresh());
    await flush();
    expect(result.current.error).toBe("string failure");
  });

  it("shares one request per network and range, and holds nothing once it settles", async () => {
    const resolvers: ((value: Series) => void)[] = [];
    const answer: Series = { range: "24h", resolution: "1m", from: 0, to: 0, constraintSets: [], ownerActions: [], points: [] };
    getSeriesMock.mockImplementation(() => new Promise<Series>((resolve) => resolvers.push(resolve)));
    expect(seriesKey("robinhood", "24h")).toBe("robinhood:24h");
    // Two views on the same range wait on one request.
    const first = fetchSharedSeries("robinhood", "24h");
    expect(fetchSharedSeries("robinhood", "24h")).toBe(first);
    expect(getSeriesMock).toHaveBeenCalledTimes(1);
    // A different range, or a different network, is a different request.
    fetchSharedSeries("robinhood", "1h");
    fetchSharedSeries("arbitrum-one", "24h");
    expect(getSeriesMock).toHaveBeenCalledTimes(3);
    resolvers.forEach((resolve) => resolve(answer));
    await expect(first).resolves.toBe(answer);
    // Nothing is kept: the next ask is a fresh request, so the refetch
    // interval is still what decides when data is refreshed.
    fetchSharedSeries("robinhood", "24h");
    expect(getSeriesMock).toHaveBeenCalledTimes(4);
  });

  it("cancels a shared request only when the last consumer has walked away", async () => {
    const answer: Series = { range: "24h", resolution: "1m", from: 0, to: 0, constraintSets: [], ownerActions: [], points: [] };
    const signals: AbortSignal[] = [];
    getSeriesMock.mockImplementation((_n: unknown, _r: unknown, options: { signal: AbortSignal }) => {
      signals.push(options.signal);
      return new Promise<Series>(() => undefined);
    });
    const one = new AbortController();
    const two = new AbortController();
    const promise = fetchSharedSeries("robinhood", "24h", one.signal);
    expect(fetchSharedSeries("robinhood", "24h", two.signal)).toBe(promise);
    expect(getSeriesMock).toHaveBeenCalledTimes(1);
    // One view leaves. The other is still waiting on the answer, so the
    // request keeps running.
    one.abort();
    expect(signals[0].aborted).toBe(false);
    // The last one leaves: nobody is waiting, so the request is cancelled
    // rather than left to run through its timeout and retries.
    two.abort();
    expect(signals[0].aborted).toBe(true);
    // And the key is free, so the next asker starts a fresh request.
    getSeriesMock.mockResolvedValueOnce(answer);
    await expect(fetchSharedSeries("robinhood", "24h")).resolves.toBe(answer);
    expect(getSeriesMock).toHaveBeenCalledTimes(2);
  });

  it("leaves a settled request alone when its consumers leave afterwards", async () => {
    const answer: Series = { range: "1h", resolution: "5s", from: 0, to: 0, constraintSets: [], ownerActions: [], points: [] };
    const signals: AbortSignal[] = [];
    getSeriesMock.mockImplementation(async (_n: unknown, _r: unknown, options: { signal: AbortSignal }) => {
      signals.push(options.signal);
      return answer;
    });
    const controller = new AbortController();
    await expect(fetchSharedSeries("robinhood", "1h", controller.signal)).resolves.toBe(answer);
    controller.abort();
    expect(signals[0].aborted).toBe(false);
  });

  it("does not hold on to a request that failed", async () => {
    const answer: Series = { range: "30d", resolution: "1h", from: 0, to: 0, constraintSets: [], ownerActions: [], points: [] };
    getSeriesMock.mockRejectedValueOnce(new Error("boom"));
    await expect(fetchSharedSeries("robinhood", "30d")).rejects.toThrow("boom");
    getSeriesMock.mockResolvedValueOnce(answer);
    await expect(fetchSharedSeries("robinhood", "30d")).resolves.toBe(answer);
  });

  it("makes one request when the hero and the history section land on the same range", async () => {
    getSeriesMock.mockResolvedValue({ range: "24h", resolution: "1m", from: 0, to: 0, constraintSets: [], ownerActions: [], points: [] });
    renderHook(() => {
      useSeries("robinhood", "24h");
      useSeries("robinhood", "24h");
      useSeries("robinhood", "1h");
    });
    await flush();
    expect(getSeriesMock).toHaveBeenCalledTimes(2);
    expect(getSeriesMock.mock.calls.map((c) => c[1]).sort()).toEqual(["1h", "24h"]);
  });

  it("asks for nothing at all without a range, which is how the hero says it is live", async () => {
    getSeriesMock.mockResolvedValue({ range: "24h", resolution: "1m", from: 0, to: 0, constraintSets: [], ownerActions: [], points: [] });
    const { result, rerender } = renderHook(({ range }) => useSeries("robinhood", range), { initialProps: { range: null as Series["range"] | null } });
    expect(getSeriesMock).not.toHaveBeenCalled();
    expect(result.current.data).toBeNull();
    expect(result.current.loading).toBe(false);
    rerender({ range: "24h" });
    await flush();
    expect(getSeriesMock).toHaveBeenCalledTimes(1);
  });

  it("does nothing without a network and pauses the interval while hidden", async () => {
    getSeriesMock.mockResolvedValue({ range: "24h", resolution: "1m", from: 0, to: 0, constraintSets: [], ownerActions: [], points: [] });
    const { result, rerender } = renderHook(({ network }) => useSeries(network, "24h"), {
      initialProps: { network: null as string | null },
    });
    expect(result.current.data).toBeNull();
    expect(getSeriesMock).not.toHaveBeenCalled();
    rerender({ network: "robinhood" });
    await flush();
    expect(result.current.data).not.toBeNull();
    act(() => setHidden(true));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(120_000);
    });
    expect(getSeriesMock).toHaveBeenCalledTimes(1);
    rerender({ network: null });
    expect(result.current.data).toBeNull();
  });

  it("ignores results that arrive after the key changed", async () => {
    let resolveFirst: (value: string) => void = () => undefined;
    const fetcher = vi.fn((signal: AbortSignal) => {
      if (fetcher.mock.calls.length === 1) return new Promise<string>((resolve) => { resolveFirst = resolve; });
      return Promise.resolve(signal.aborted ? "aborted" : "second");
    });
    const { result, rerender } = renderHook(({ key }) => useApi(key, fetcher), { initialProps: { key: "a" } });
    rerender({ key: "b" });
    await flush();
    expect(result.current.data).toBe("second");
    resolveFirst("first");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(result.current.data).toBe("second");
    expect(refetchIntervalFor("all")).toBe(0);
    expect(refetchIntervalFor("24h")).toBe(60_000);
  });

  it("refetches a range when an owner action arrives, once per action", () => {
    const action = (block: number): OwnerAction => ({ block, at: "2026-09-06T07:20:00Z", txHash: `0x${block}`, method: "setGasPricingConstraints", selector: "0xcc0d556a", args: {} });
    const refresh = vi.fn();
    const { rerender } = renderHook(({ actions }) => useRefreshOnOwnerAction(actions, refresh), { initialProps: { actions: [] as OwnerAction[] } });
    expect(refresh).not.toHaveBeenCalled();
    rerender({ actions: [action(10)] });
    expect(refresh).toHaveBeenCalledTimes(1);
    // The same feed rendered again is not a new action.
    rerender({ actions: [action(10)] });
    expect(refresh).toHaveBeenCalledTimes(1);
    rerender({ actions: [action(11), action(10)] });
    expect(refresh).toHaveBeenCalledTimes(2);
    // A reorg that takes the action back leaves nothing to fetch a range for.
    rerender({ actions: [] });
    expect(refresh).toHaveBeenCalledTimes(2);
  });
});

describe("useNetwork", () => {
  beforeEach(() => {
    pushMock.mockReset();
    window.localStorage.clear();
    routeParams = { network: "robinhood" };
  });

  it("reads the route param, persists it and navigates on change", () => {
    const { result } = renderHook(() => useNetwork());
    expect(result.current.network).toBe("robinhood");
    expect(window.localStorage.getItem(NETWORK_STORAGE_KEY)).toBe("robinhood");
    act(() => result.current.setNetwork("arbitrum-one"));
    expect(pushMock).toHaveBeenCalledWith("/arbitrum-one");
    expect(readStoredNetwork()).toBe("arbitrum-one");
    act(() => result.current.setNetwork("robinhood"));
    act(() => result.current.setNetwork("Bad Name!"));
    expect(pushMock).toHaveBeenCalledTimes(1);
  });

  it("replaces a chain-id route with the canonical name without a history entry", () => {
    replaceMock.mockReset();
    routeParams = { network: "4663" };
    const { result } = renderHook(() => useNetwork());
    expect(result.current.network).toBe("4663");
    act(() => result.current.replaceNetwork("robinhood"));
    expect(replaceMock).toHaveBeenCalledWith("/robinhood");
    expect(pushMock).not.toHaveBeenCalled();
    expect(readStoredNetwork()).toBe("robinhood");
    act(() => result.current.replaceNetwork("4663"));
    act(() => result.current.replaceNetwork("Bad Name!"));
    expect(replaceMock).toHaveBeenCalledTimes(1);
  });

  it("falls back to the default for missing or invalid params", () => {
    routeParams = {};
    expect(renderHook(() => useNetwork()).result.current.network).toBe(DEFAULT_NETWORK);
    routeParams = { network: ["arbitrum-one", "x"] };
    expect(renderHook(() => useNetwork()).result.current.network).toBe("arbitrum-one");
    routeParams = { network: "NOT VALID" };
    expect(renderHook(() => useNetwork()).result.current.network).toBe(DEFAULT_NETWORK);
    expect(isValidNetworkName(42)).toBe(false);
  });

  it("survives blocked storage", () => {
    const original = window.localStorage;
    Object.defineProperty(window, "localStorage", {
      configurable: true,
      get: () => {
        throw new Error("blocked");
      },
    });
    expect(readStoredNetwork()).toBeNull();
    expect(() => storeNetwork("robinhood")).not.toThrow();
    Object.defineProperty(window, "localStorage", { configurable: true, value: original });
    window.localStorage.setItem(NETWORK_STORAGE_KEY, "??");
    expect(readStoredNetwork()).toBeNull();
  });
});

describe("useDocumentVisible", () => {
  it("tracks visibility changes", () => {
    setHidden(false);
    const { result } = renderHook(() => useDocumentVisible());
    expect(result.current).toBe(true);
    act(() => setHidden(true));
    expect(result.current).toBe(false);
    act(() => setHidden(false));
    expect(result.current).toBe(true);
  });
});
