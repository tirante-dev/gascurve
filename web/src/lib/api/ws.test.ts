import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { LiveStatus } from "@/types";
import { LiveClient, resolveSocketFactory, resolveWsUrl, SOCKET_OPEN, type SocketLike } from "./ws";

class FakeSocket implements SocketLike {
  static instances: FakeSocket[] = [];
  onopen: ((ev: Event) => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onclose: ((ev: CloseEvent) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  readyState = 0;
  sent: string[] = [];
  closed = false;
  readonly url: string;

  constructor(url: string) {
    this.url = url;
    FakeSocket.instances.push(this);
  }

  send(data: string): void {
    if (this.readyState !== SOCKET_OPEN) throw new Error("not open");
    this.sent.push(data);
  }

  close(): void {
    this.closed = true;
    this.readyState = 3;
    this.onclose?.(new CloseEvent("close"));
  }

  serverOpen(): void {
    this.readyState = SOCKET_OPEN;
    this.onopen?.(new Event("open"));
  }

  serverMessage(payload: unknown): void {
    this.onmessage?.(new MessageEvent("message", { data: typeof payload === "string" ? payload : JSON.stringify(payload) }));
  }

  serverDrop(): void {
    this.readyState = 3;
    this.onerror?.(new Event("error"));
    this.onclose?.(new CloseEvent("close"));
  }
}

const factory = (url: string) => new FakeSocket(url);

function latest(): FakeSocket {
  return FakeSocket.instances[FakeSocket.instances.length - 1];
}

describe("resolveWsUrl", () => {
  it("derives from the api URL or uses the explicit one", () => {
    expect(resolveWsUrl("http://localhost:8080/api/v1", undefined)).toBe("ws://localhost:8080/api/v1/ws");
    expect(resolveWsUrl("https://gascurve.example/api/v1", "")).toBe("wss://gascurve.example/api/v1/ws");
    expect(resolveWsUrl("http://x/api/v1", "wss://ws.example/api/v1/ws/")).toBe("wss://ws.example/api/v1/ws");
    expect(resolveWsUrl()).toBe("ws://localhost:8080/api/v1/ws");
  });
});

describe("resolveSocketFactory", () => {
  afterEach(() => vi.unstubAllEnvs());

  it("returns the mock socket when mock data is on", async () => {
    vi.stubEnv("NEXT_PUBLIC_USE_MOCK_DATA", "true");
    const make = await resolveSocketFactory();
    const socket = make("ws://x/ws?network=robinhood");
    expect(socket.readyState).toBe(0);
    socket.close();
  });

  it("returns the browser WebSocket otherwise", async () => {
    vi.stubEnv("NEXT_PUBLIC_USE_MOCK_DATA", "false");
    const original = globalThis.WebSocket;
    class StubWebSocket {
      readonly url: string;
      constructor(url: string) {
        this.url = url;
      }
    }
    globalThis.WebSocket = StubWebSocket as unknown as typeof WebSocket;
    try {
      const make = await resolveSocketFactory();
      const socket = make("ws://x");
      expect(socket).toBeInstanceOf(StubWebSocket);
    } finally {
      globalThis.WebSocket = original;
    }
  });
});

describe("LiveClient", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    FakeSocket.instances = [];
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  function makeClient(overrides: Partial<ConstructorParameters<typeof LiveClient>[0]> = {}) {
    const statuses: LiveStatus[] = [];
    const onHello = vi.fn();
    const onTick = vi.fn();
    const onBlocks = vi.fn();
    const onOwnerAction = vi.fn();
    const client = new LiveClient({
      url: "ws://api/ws",
      network: "robinhood",
      socketFactory: factory,
      onHello,
      onTick,
      onBlocks,
      onOwnerAction,
      onStatus: (s) => statuses.push(s),
      ...overrides,
    });
    return { client, statuses, onHello, onTick, onBlocks, onOwnerAction };
  }

  it("connects with the network in the URL and dispatches messages", () => {
    const { client, statuses, onHello, onTick, onBlocks, onOwnerAction } = makeClient();
    client.connect();
    expect(client.status).toBe("connecting");
    const socket = latest();
    expect(socket.url).toBe("ws://api/ws?network=robinhood");
    socket.serverOpen();
    expect(statuses).toEqual(["open"]);

    socket.serverMessage({ type: "hello", data: { network: { name: "robinhood" }, snapshot: { block: { number: 1 } }, recentBlocks: [] } });
    expect(onHello).toHaveBeenCalledWith(expect.objectContaining({ recentBlocks: [] }));
    socket.serverMessage({ type: "tick", data: { block: { number: 2 } } });
    expect(onTick).toHaveBeenCalledWith({ block: { number: 2 } });
    socket.serverMessage({ type: "blocks", data: [{ number: 2 }] });
    expect(onBlocks).toHaveBeenCalledWith([{ number: 2 }]);
    socket.serverMessage({ type: "owner_action", data: { method: "setGasPricingConstraints" } });
    expect(onOwnerAction).toHaveBeenCalledWith({ method: "setGasPricingConstraints" });
    socket.serverMessage({ type: "ping" });
    expect(socket.sent).toEqual([JSON.stringify({ type: "pong" })]);

    socket.serverMessage("not json");
    socket.serverMessage({ type: "unknown" });
    socket.serverMessage(42);
    socket.onmessage?.(new MessageEvent("message", { data: new Blob([]) }));
    expect(onTick).toHaveBeenCalledTimes(1);

    client.close();
    expect(socket.closed).toBe(true);
    expect(FakeSocket.instances).toHaveLength(1);
    vi.advanceTimersByTime(60_000);
    expect(FakeSocket.instances).toHaveLength(1);
  });

  it("reconnects with exponential backoff and reports polling after two failures", () => {
    const { client, statuses } = makeClient();
    client.connect();
    const first = latest();
    first.serverOpen();
    first.serverDrop();
    expect(client.status).toBe("reconnecting");
    expect(FakeSocket.instances).toHaveLength(1);
    vi.advanceTimersByTime(999);
    expect(FakeSocket.instances).toHaveLength(1);
    vi.advanceTimersByTime(1);
    expect(FakeSocket.instances).toHaveLength(2);

    latest().serverDrop();
    expect(client.status).toBe("polling");
    vi.advanceTimersByTime(2000);
    expect(FakeSocket.instances).toHaveLength(3);
    latest().serverDrop();
    vi.advanceTimersByTime(4000);
    expect(FakeSocket.instances).toHaveLength(4);
    latest().serverDrop();
    expect(client.backoffMs()).toBe(16_000);
    for (let i = 0; i < 10; i++) {
      vi.advanceTimersByTime(30_000);
      latest().serverDrop();
    }
    expect(client.backoffMs()).toBe(30_000);

    vi.advanceTimersByTime(30_000);
    latest().serverOpen();
    expect(client.status).toBe("open");
    expect(client.backoffMs()).toBe(1000);
    expect(statuses[0]).toBe("open");
    expect(statuses).toContain("polling");
    client.close();
  });

  it("handles a socket factory that throws", () => {
    let calls = 0;
    const { client } = makeClient({
      socketFactory: (url) => {
        calls += 1;
        if (calls === 1) throw new Error("no WebSocket");
        return new FakeSocket(url);
      },
    });
    client.connect();
    expect(client.status).toBe("reconnecting");
    vi.advanceTimersByTime(1000);
    expect(FakeSocket.instances).toHaveLength(1);
    client.close();
  });

  it("switches networks with subscribe on an open socket, or via the URL when closed", () => {
    const { client } = makeClient();
    client.connect();
    const socket = latest();
    socket.serverOpen();
    client.subscribe("robinhood");
    expect(socket.sent).toEqual([]);
    client.subscribe("arbitrum-one");
    expect(socket.sent).toEqual([JSON.stringify({ type: "subscribe", network: "arbitrum-one" })]);
    expect(client.activeNetwork).toBe("arbitrum-one");

    socket.serverDrop();
    client.subscribe("robinhood-testnet");
    vi.advanceTimersByTime(1000);
    expect(latest().url).toBe("ws://api/ws?network=robinhood-testnet");
    latest().serverOpen();
    expect(latest().sent).toEqual([]);
    client.close();
  });

  it("sends subscribe on open when the network changed while connecting", () => {
    const { client } = makeClient();
    client.connect();
    const socket = latest();
    client.subscribe("arbitrum-one");
    expect(socket.sent).toEqual([]);
    socket.serverOpen();
    expect(socket.sent).toEqual([JSON.stringify({ type: "subscribe", network: "arbitrum-one" })]);
    client.close();
  });

  it("swallows send failures", () => {
    const { client } = makeClient();
    client.connect();
    const socket = latest();
    socket.serverOpen();
    socket.readyState = 2;
    socket.serverMessage({ type: "ping" });
    expect(socket.sent).toEqual([]);
    client.close();
  });

  it("suspends without reconnecting and resumes", () => {
    const { client, statuses } = makeClient();
    client.connect();
    latest().serverOpen();
    client.suspend();
    expect(latest().closed).toBe(true);
    vi.advanceTimersByTime(60_000);
    expect(FakeSocket.instances).toHaveLength(1);
    client.resume();
    expect(FakeSocket.instances).toHaveLength(2);
    expect(statuses).toEqual(["open", "connecting"]);
    client.resume();
    expect(FakeSocket.instances).toHaveLength(2);
    client.close();
    client.resume();
    expect(FakeSocket.instances).toHaveLength(2);
  });

  it("ignores events from stale sockets", () => {
    const { client, onTick } = makeClient();
    client.connect();
    const first = latest();
    first.serverOpen();
    first.serverDrop();
    vi.advanceTimersByTime(1000);
    const second = latest();
    second.serverOpen();
    first.serverMessage({ type: "tick", data: {} });
    first.serverOpen();
    first.onclose?.(new CloseEvent("close"));
    expect(onTick).not.toHaveBeenCalled();
    expect(client.status).toBe("open");
    second.serverMessage({ type: "tick", data: {} });
    expect(onTick).toHaveBeenCalledTimes(1);
    client.close();
  });

  it("connect is idempotent while a socket exists", () => {
    const { client } = makeClient();
    client.connect();
    client.connect();
    expect(FakeSocket.instances).toHaveLength(1);
    client.close();
  });
});
