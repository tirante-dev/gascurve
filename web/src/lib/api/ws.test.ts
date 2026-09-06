import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { HelloData, LiveStatus } from "@/types";
import { helloMatches, LiveClient, resolveSocketFactory, resolveWsUrl, SOCKET_OPEN, WATCHDOG_MS, type SocketLike } from "./ws";

class FakeSocket implements SocketLike {
  static instances: FakeSocket[] = [];
  onopen: ((ev: Event) => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onclose: ((ev: CloseEvent) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  readyState = 0;
  sent: string[] = [];
  closed = false;
  closeCode: number | undefined;
  readonly url: string;

  constructor(url: string) {
    this.url = url;
    FakeSocket.instances.push(this);
  }

  send(data: string): void {
    if (this.readyState !== SOCKET_OPEN) throw new Error("not open");
    this.sent.push(data);
  }

  close(code?: number): void {
    this.closed = true;
    this.closeCode = code;
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

  serverHello(name: string, chainId: number): void {
    this.serverMessage({ type: "hello", data: { network: { name, chainId }, snapshot: { chainId, block: { number: 1 } }, recentBlocks: [] } });
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

describe("helloMatches", () => {
  it("accepts the wanted name or chain id and rejects anything else", () => {
    const hello = { network: { name: "robinhood", chainId: 4663 } } as HelloData;
    expect(helloMatches(hello, "robinhood")).toBe(true);
    expect(helloMatches(hello, "4663")).toBe(true);
    expect(helloMatches(hello, "arbitrum-one")).toBe(false);
    expect(helloMatches({ network: null } as unknown as HelloData, "robinhood")).toBe(false);
    expect(helloMatches({} as HelloData, "robinhood")).toBe(false);
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
    const onError = vi.fn();
    const client = new LiveClient({
      url: "ws://api/ws",
      network: "robinhood",
      socketFactory: factory,
      onHello,
      onTick,
      onBlocks,
      onOwnerAction,
      onError,
      onStatus: (s) => statuses.push(s),
      ...overrides,
    });
    return { client, statuses, onHello, onTick, onBlocks, onOwnerAction, onError };
  }

  it("connects with the network in the URL and dispatches messages keyed by the confirmed network", () => {
    const { client, statuses, onHello, onTick, onBlocks, onOwnerAction, onError } = makeClient();
    client.connect();
    expect(client.status).toBe("connecting");
    expect(client.confirmedNetwork).toBeNull();
    const socket = latest();
    expect(socket.url).toBe("ws://api/ws?network=robinhood");
    socket.serverOpen();
    expect(statuses).toEqual(["open"]);

    socket.serverHello("robinhood", 4663);
    expect(onHello).toHaveBeenCalledWith(expect.objectContaining({ recentBlocks: [] }), "robinhood");
    expect(client.confirmedNetwork).toBe("robinhood");
    socket.serverMessage({ type: "tick", data: { chainId: 4663, block: { number: 2 } } });
    expect(onTick).toHaveBeenCalledWith({ chainId: 4663, block: { number: 2 } }, "robinhood");
    socket.serverMessage({ type: "blocks", data: [{ number: 2 }] });
    expect(onBlocks).toHaveBeenCalledWith([{ number: 2 }], "robinhood");
    socket.serverMessage({ type: "owner_action", data: { method: "setGasPricingConstraints" } });
    expect(onOwnerAction).toHaveBeenCalledWith({ method: "setGasPricingConstraints" }, "robinhood");
    socket.serverMessage({ type: "ping" });
    expect(socket.sent).toEqual([JSON.stringify({ type: "pong" })]);
    socket.serverMessage({ type: "error", error: { code: "unknown_network", message: "no such network" } });
    expect(onError).toHaveBeenCalledWith("no such network");

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

  it("drops ticks, blocks and owner actions until a hello for the wanted network arrives", () => {
    const { client, onHello, onTick, onBlocks, onOwnerAction } = makeClient();
    client.connect();
    const socket = latest();
    socket.serverOpen();
    // Nothing before the hello.
    socket.serverMessage({ type: "tick", data: { chainId: 4663 } });
    socket.serverMessage({ type: "blocks", data: [{ number: 1 }] });
    socket.serverMessage({ type: "owner_action", data: { method: "x" } });
    expect(onTick).not.toHaveBeenCalled();
    expect(onBlocks).not.toHaveBeenCalled();
    expect(onOwnerAction).not.toHaveBeenCalled();
    // A hello for another network is not an acknowledgement either.
    socket.serverHello("arbitrum-one", 42161);
    expect(onHello).not.toHaveBeenCalled();
    socket.serverMessage({ type: "tick", data: { chainId: 42161 } });
    expect(onTick).not.toHaveBeenCalled();
    socket.serverHello("robinhood", 4663);
    expect(onHello).toHaveBeenCalledTimes(1);
    // A tick carrying another chain id, or none, is rejected even once confirmed.
    socket.serverMessage({ type: "tick", data: { chainId: 42161 } });
    socket.serverMessage({ type: "tick", data: {} });
    expect(onTick).not.toHaveBeenCalled();
    socket.serverMessage({ type: "tick", data: { chainId: 4663 } });
    expect(onTick).toHaveBeenCalledTimes(1);
    client.close();
  });

  it("after subscribe, old-network messages are ignored until the new hello, and a stale hello never confirms", () => {
    const { client, onHello, onTick, onBlocks } = makeClient();
    client.connect();
    const socket = latest();
    socket.serverOpen();
    socket.serverHello("robinhood", 4663);
    client.subscribe("arbitrum-one");
    expect(client.confirmedNetwork).toBeNull();
    // Ticks and blocks queued for robinhood while the server builds the new hello.
    socket.serverMessage({ type: "tick", data: { chainId: 4663 } });
    socket.serverMessage({ type: "blocks", data: [{ number: 5 }] });
    expect(onTick).not.toHaveBeenCalled();
    expect(onBlocks).not.toHaveBeenCalled();
    // Rapid A to B to C: B's hello arrives after C was requested and is dropped.
    client.subscribe("robinhood-testnet");
    socket.serverHello("arbitrum-one", 42161);
    expect(onHello).toHaveBeenCalledTimes(1);
    socket.serverMessage({ type: "tick", data: { chainId: 42161 } });
    expect(onTick).not.toHaveBeenCalled();
    socket.serverHello("robinhood-testnet", 46630);
    expect(onHello).toHaveBeenLastCalledWith(expect.anything(), "robinhood-testnet");
    expect(client.confirmedNetwork).toBe("robinhood-testnet");
    socket.serverMessage({ type: "tick", data: { chainId: 46630 } });
    expect(onTick).toHaveBeenCalledWith({ chainId: 46630 }, "robinhood-testnet");
    socket.serverMessage({ type: "blocks", data: [{ number: 6 }] });
    expect(onBlocks).toHaveBeenCalledWith([{ number: 6 }], "robinhood-testnet");
    client.close();
  });

  it("accepts a hello by chain id when the route used one", () => {
    const { client, onHello, onTick } = makeClient({ network: "4663" });
    client.connect();
    const socket = latest();
    socket.serverOpen();
    socket.serverHello("robinhood", 4663);
    expect(onHello).toHaveBeenCalledWith(expect.anything(), "4663");
    socket.serverMessage({ type: "tick", data: { chainId: 4663 } });
    expect(onTick).toHaveBeenCalledTimes(1);
    client.close();
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
    // Opening the TCP socket does not reset the backoff; the hello does.
    expect(client.backoffMs()).toBe(30_000);
    latest().serverHello("robinhood", 4663);
    expect(client.backoffMs()).toBe(1000);
    expect(statuses[0]).toBe("open");
    expect(statuses).toContain("polling");
    client.close();
  });

  it("keeps backing off when the server accepts the socket and drops it before hello", () => {
    const { client } = makeClient();
    client.connect();
    const delays: number[] = [];
    for (let i = 0; i < 6; i++) {
      const socket = latest();
      socket.serverOpen();
      delays.push(client.backoffMs());
      socket.serverDrop();
      vi.advanceTimersByTime(client.backoffMs());
    }
    expect(delays).toEqual([1000, 2000, 4000, 8000, 16_000, 30_000]);
    client.close();
  });

  it("closes and reconnects a socket that delivers nothing within the watchdog interval", () => {
    const { client, statuses } = makeClient();
    client.connect();
    const socket = latest();
    socket.serverOpen();
    socket.serverHello("robinhood", 4663);
    // Pings keep it alive.
    for (let i = 0; i < 4; i++) {
      vi.advanceTimersByTime(30_000);
      socket.serverMessage({ type: "ping" });
    }
    expect(socket.closed).toBe(false);
    expect(client.status).toBe("open");
    // Then the network goes black without a close event.
    vi.advanceTimersByTime(WATCHDOG_MS - 1);
    expect(socket.closed).toBe(false);
    vi.advanceTimersByTime(1);
    expect(socket.closed).toBe(true);
    expect(socket.closeCode).toBe(4000);
    expect(client.status).toBe("reconnecting");
    expect(statuses).toEqual(["open", "reconnecting"]);
    vi.advanceTimersByTime(1000);
    expect(FakeSocket.instances).toHaveLength(2);
    // A tick also resets the watchdog.
    latest().serverOpen();
    latest().serverHello("robinhood", 4663);
    vi.advanceTimersByTime(WATCHDOG_MS - 1);
    latest().serverMessage({ type: "tick", data: { chainId: 4663 } });
    vi.advanceTimersByTime(WATCHDOG_MS - 1);
    expect(latest().closed).toBe(false);
    client.close();
  });

  it("watchdog honours suspend, close and a socket that throws on close", () => {
    const { client } = makeClient({ watchdogMs: 100 });
    client.connect();
    latest().serverOpen();
    client.suspend();
    vi.advanceTimersByTime(1000);
    expect(FakeSocket.instances).toHaveLength(1);
    client.resume();
    const socket = latest();
    socket.serverOpen();
    socket.close = () => {
      throw new Error("already gone");
    };
    vi.advanceTimersByTime(100);
    expect(client.status).toBe("reconnecting");
    vi.advanceTimersByTime(1000);
    expect(FakeSocket.instances).toHaveLength(3);
    client.close();
    vi.advanceTimersByTime(10_000);
    expect(FakeSocket.instances).toHaveLength(3);
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
    second.serverHello("robinhood", 4663);
    first.serverMessage({ type: "tick", data: { chainId: 4663 } });
    first.serverOpen();
    first.onclose?.(new CloseEvent("close"));
    expect(onTick).not.toHaveBeenCalled();
    expect(client.status).toBe("open");
    second.serverMessage({ type: "tick", data: { chainId: 4663 } });
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
