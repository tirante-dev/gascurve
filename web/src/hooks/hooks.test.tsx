import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { BlockPoint, LiveSnapshot, Series } from "@/types";
import { SOCKET_OPEN, type SocketLike } from "@/lib/api/ws";

const pushMock = vi.fn();
let routeParams: { network?: string | string[] } = { network: "robinhood" };
vi.mock("next/navigation", () => ({
  useParams: () => routeParams,
  useRouter: () => ({ push: pushMock }),
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

import { appendBlocks, useLive } from "./useLive";
import { useApi } from "./useApi";
import { refetchIntervalFor, useSeries } from "./useSeries";
import { DEFAULT_NETWORK, isValidNetworkName, NETWORK_STORAGE_KEY, readStoredNetwork, storeNetwork, useNetwork } from "./useNetwork";
import { useDocumentVisible } from "./useDocumentVisible";
import { useAnimatedBacklogs } from "./useAnimatedBacklogs";

function block(number: number): BlockPoint {
  return { number, ts: 1, gasUsed: 1, baseFee: "1", predictedBaseFee: "1", backlogs: [], exponentBips: 0, anchored: false };
}

function snapshot(n: number): LiveSnapshot {
  return {
    chainId: 4663,
    sampledAt: "2026-09-06T07:20:00Z",
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
    expect(result.current.status).toBe("open");
    act(() =>
      socket.serverMessage({
        type: "hello",
        data: { network: { name: "robinhood" }, snapshot: snapshot(10), recentBlocks: [block(9), block(10)] },
      }),
    );
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

    // Reopen: polling stops.
    act(() => latest().serverOpen());
    expect(result.current.status).toBe("open");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000);
    });
    expect(getLiveMock).toHaveBeenCalledTimes(2);

    // Switching network subscribes on the same socket and clears state.
    rerender({ network: "arbitrum-one" });
    expect(latest().sent).toContain(JSON.stringify({ type: "subscribe", network: "arbitrum-one" }));
    expect(result.current.snapshot).toBeNull();
    expect(result.current.recentBlocks).toEqual([]);

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

  it("does not poll while hidden", async () => {
    setHidden(true);
    getLiveMock.mockResolvedValue(snapshot(1));
    const { result, unmount } = renderHook(() => useLive("robinhood"));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    act(() => latest().serverDrop());
    expect(result.current.status).toBe("reconnecting");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000);
    });
    expect(getLiveMock).not.toHaveBeenCalled();
    unmount();
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
    setHidden(false);
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("fetches, keeps previous data while loading, refetches on an interval and surfaces errors", async () => {
    const series = (range: string): Series => ({ range: range as Series["range"], resolution: "5s", constraintSets: [], ownerActions: [], points: [] });
    getSeriesMock.mockImplementation(async (_n: unknown, range: string) => series(range));
    const { result, rerender } = renderHook(({ range }) => useSeries("robinhood", range), {
      initialProps: { range: "1h" as Series["range"] },
    });
    expect(result.current.loading).toBe(true);
    await flush();
    expect(result.current.data?.range).toBe("1h");
    expect(result.current.loading).toBe(false);
    expect(result.current.updatedAt).not.toBeNull();
    expect(getSeriesMock).toHaveBeenCalledWith("robinhood", "1h", expect.objectContaining({ signal: expect.any(AbortSignal) }));

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

  it("does nothing without a network and pauses the interval while hidden", async () => {
    getSeriesMock.mockResolvedValue({ range: "24h", resolution: "1m", constraintSets: [], ownerActions: [], points: [] });
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

describe("useAnimatedBacklogs", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("drains between ticks with requestAnimationFrame and snaps on a new tick", () => {
    const frames: FrameRequestCallback[] = [];
    vi.stubGlobal("requestAnimationFrame", (cb: FrameRequestCallback) => {
      frames.push(cb);
      return frames.length;
    });
    const cancel = vi.fn();
    vi.stubGlobal("cancelAnimationFrame", cancel);
    let clock = 1000;
    vi.spyOn(performance, "now").mockImplementation(() => clock);
    const constraints = [
      { target: 60_000_000, window: 15, backlog: 90_000_000, exponentBips: 0 },
      { target: 40_000_000, window: 86_400, backlog: 1_000_000_000, exponentBips: 0 },
    ];
    const { result, rerender } = renderHook(({ c, key }) => useAnimatedBacklogs(c, key), {
      initialProps: { c: constraints, key: "t1" },
    });
    expect(result.current).toEqual([90_000_000, 1_000_000_000]);
    expect(frames).toHaveLength(1);
    clock = 1500;
    act(() => frames[0](clock));
    expect(result.current).toEqual([60_000_000, 980_000_000]);
    act(() => frames[1](3000));
    expect(result.current).toEqual([0, 920_000_000]);
    const next = [{ ...constraints[0], backlog: 12 }, { ...constraints[1], backlog: 34 }];
    rerender({ c: next, key: "t2" });
    expect(cancel).toHaveBeenCalled();
    expect(result.current).toEqual([12, 34]);
  });

  it("only snaps when reduced motion is preferred", () => {
    vi.stubGlobal("matchMedia", () => ({ matches: true }));
    const raf = vi.fn();
    vi.stubGlobal("requestAnimationFrame", raf);
    const { result } = renderHook(() => useAnimatedBacklogs([{ target: 1, window: 1, backlog: 5, exponentBips: 0 }], "k"));
    expect(result.current).toEqual([5]);
    expect(raf).not.toHaveBeenCalled();
  });
});
