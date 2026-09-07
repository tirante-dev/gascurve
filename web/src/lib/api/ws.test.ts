import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { HelloData, LiveStatus } from "@/types";
import { ACK_WATCHDOG_MS, helloMatches, keyMatches, LiveClient, resolveSocketFactory, resolveWsUrl, SOCKET_OPEN, WATCHDOG_MS, type SocketLike } from "./ws";

const ROBINHOOD = { name: "robinhood", chainId: 4663 };
const TESTNET = { name: "robinhood-testnet", chainId: 46630 };

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

  it("resolves a relative api base against the page origin, since a socket has no relative form", () => {
    expect(resolveWsUrl("/api/v1", undefined, "https://gascurve.com")).toBe("wss://gascurve.com/api/v1/ws");
    expect(resolveWsUrl("/api/v1", undefined, "http://localhost:3000")).toBe("ws://localhost:3000/api/v1/ws");
    expect(resolveWsUrl("/api/v1", undefined, "https://gascurve.com/")).toBe("wss://gascurve.com/api/v1/ws");
    // An explicit setting still wins over anything derived.
    expect(resolveWsUrl("/api/v1", "wss://elsewhere/ws", "https://gascurve.com")).toBe("wss://elsewhere/ws");
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
    expect(helloMatches({ network: { name: 7, chainId: "4663" } } as unknown as HelloData, "4663")).toBe(false);
  });
});

describe("keyMatches", () => {
  it("treats a name and its decimal chain id as the same network", () => {
    expect(keyMatches(ROBINHOOD, "robinhood")).toBe(true);
    expect(keyMatches(ROBINHOOD, "4663")).toBe(true);
    expect(keyMatches(ROBINHOOD, "arbitrum-one")).toBe(false);
    expect(keyMatches(ROBINHOOD, "466")).toBe(false);
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
    const onReorg = vi.fn();
    const onBlocks = vi.fn();
    const onOwnerAction = vi.fn();
    const onError = vi.fn();
    const client = new LiveClient({
      url: "ws://api/ws",
      network: "robinhood",
      socketFactory: factory,
      onHello,
      onTick,
      onReorg,
      onBlocks,
      onOwnerAction,
      onError,
      onStatus: (s) => statuses.push(s),
      ...overrides,
    });
    return { client, statuses, onHello, onTick, onReorg, onBlocks, onOwnerAction, onError };
  }

  it("dispatches a reorg keyed by the confirmed network, and only one that carries its chain id", () => {
    const { client, onReorg, onBlocks } = makeClient();
    client.connect();
    const socket = latest();
    socket.serverOpen();
    const reorg = { chainId: 4663, ancestor: 10, blocks: [{ number: 11 }, { number: 12 }] };
    // Nothing before the hello.
    socket.serverMessage({ type: "reorg", data: reorg });
    expect(onReorg).not.toHaveBeenCalled();
    socket.serverHello("robinhood", 4663);
    socket.serverMessage({ type: "reorg", data: reorg });
    expect(onReorg).toHaveBeenCalledWith(reorg, ROBINHOOD);
    // Another chain's reorg, or one without a chain id, never reaches the feed.
    socket.serverMessage({ type: "reorg", data: { ...reorg, chainId: 42161 } });
    socket.serverMessage({ type: "reorg", data: { ancestor: 10, blocks: [] } });
    expect(onReorg).toHaveBeenCalledTimes(1);
    // The blocks that follow it are delivered as usual.
    socket.serverMessage({ type: "blocks", data: [{ number: 13 }] });
    expect(onBlocks).toHaveBeenCalledWith([{ number: 13 }], ROBINHOOD);
    // After a subscribe the old network's reorg is dropped until the new hello.
    client.subscribe("arbitrum-one");
    socket.serverMessage({ type: "reorg", data: reorg });
    expect(onReorg).toHaveBeenCalledTimes(1);
    socket.serverHello("arbitrum-one", 42161);
    socket.serverMessage({ type: "reorg", data: { ...reorg, chainId: 42161 } });
    expect(onReorg).toHaveBeenLastCalledWith({ ...reorg, chainId: 42161 }, { name: "arbitrum-one", chainId: 42161 });
    client.close();
  });

  it("connects with the network in the URL and dispatches messages keyed by the confirmed network", () => {
    const { client, statuses, onHello, onTick, onBlocks, onOwnerAction, onError } = makeClient();
    client.connect();
    expect(client.status).toBe("connecting");
    expect(client.confirmedNetwork).toBeNull();
    const socket = latest();
    expect(socket.url).toBe("ws://api/ws?network=robinhood");
    socket.serverOpen();
    // A TCP open is not live: the status waits for the hello.
    expect(statuses).toEqual([]);
    expect(client.status).toBe("connecting");

    socket.serverHello("robinhood", 4663);
    expect(statuses).toEqual(["open"]);
    expect(onHello).toHaveBeenCalledWith(expect.objectContaining({ recentBlocks: [] }), ROBINHOOD);
    expect(client.confirmedNetwork).toEqual(ROBINHOOD);
    socket.serverMessage({ type: "tick", data: { chainId: 4663, block: { number: 2 } } });
    expect(onTick).toHaveBeenCalledWith({ chainId: 4663, block: { number: 2 } }, ROBINHOOD);
    socket.serverMessage({ type: "blocks", data: [{ number: 2 }] });
    expect(onBlocks).toHaveBeenCalledWith([{ number: 2 }], ROBINHOOD);
    socket.serverMessage({ type: "owner_action", data: { method: "setGasPricingConstraints" } });
    expect(onOwnerAction).toHaveBeenCalledWith({ method: "setGasPricingConstraints" }, ROBINHOOD);
    socket.serverMessage({ type: "ping" });
    expect(socket.sent).toEqual([JSON.stringify({ type: "pong" })]);
    // An error on a confirmed subscription is reported and the socket stays.
    socket.serverMessage({ type: "error", error: { code: "internal", message: "hiccup" } });
    expect(onError).toHaveBeenCalledWith("hiccup");
    expect(socket.closed).toBe(false);
    expect(client.status).toBe("open");

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
    expect(onHello).toHaveBeenLastCalledWith(expect.anything(), TESTNET);
    expect(client.confirmedNetwork).toEqual(TESTNET);
    socket.serverMessage({ type: "tick", data: { chainId: 46630 } });
    expect(onTick).toHaveBeenCalledWith({ chainId: 46630 }, TESTNET);
    socket.serverMessage({ type: "blocks", data: [{ number: 6 }] });
    expect(onBlocks).toHaveBeenCalledWith([{ number: 6 }], TESTNET);
    client.close();
  });

  it("treats a subscribe to an alias of the confirmed network as the same subscription: no message, no hello awaited, feed kept", () => {
    const { client, onHello, onTick, onBlocks, statuses } = makeClient({ network: "4663" });
    client.connect();
    const socket = latest();
    expect(socket.url).toBe("ws://api/ws?network=4663");
    socket.serverOpen();
    socket.serverHello("robinhood", 4663);
    expect(statuses).toEqual(["open"]);
    // The page canonicalises /4663 to /robinhood: the server would ignore a
    // subscribe for the chain it already serves, so none is sent.
    client.subscribe("robinhood");
    expect(socket.sent).toEqual([]);
    expect(client.activeNetwork).toBe("robinhood");
    expect(client.confirmedNetwork).toEqual(ROBINHOOD);
    expect(client.status).toBe("open");
    socket.serverMessage({ type: "tick", data: { chainId: 4663 } });
    expect(onTick).toHaveBeenCalledWith({ chainId: 4663 }, ROBINHOOD);
    socket.serverMessage({ type: "blocks", data: [{ number: 3 }] });
    expect(onBlocks).toHaveBeenCalledWith([{ number: 3 }], ROBINHOOD);
    // No acknowledgement is awaited: pings alone carry the socket past the window.
    vi.advanceTimersByTime(ACK_WATCHDOG_MS);
    socket.serverMessage({ type: "ping" });
    vi.advanceTimersByTime(ACK_WATCHDOG_MS);
    expect(socket.closed).toBe(false);
    expect(statuses).toEqual(["open"]);
    // Back to the chain id: still the same subscription.
    client.subscribe("4663");
    expect(socket.sent).toEqual([JSON.stringify({ type: "pong" })]);
    expect(onHello).toHaveBeenCalledTimes(1);
    // A different network is a real switch and the status is pending again.
    client.subscribe("arbitrum-one");
    expect(socket.sent).toContain(JSON.stringify({ type: "subscribe", network: "arbitrum-one" }));
    expect(client.confirmedNetwork).toBeNull();
    expect(client.status).toBe("connecting");
    socket.serverMessage({ type: "tick", data: { chainId: 4663 } });
    expect(onTick).toHaveBeenCalledTimes(1);
    // After a drop, the reconnect URL carries whatever reference was requested last.
    socket.serverDrop();
    client.subscribe("42161");
    vi.advanceTimersByTime(1000);
    expect(latest().url).toBe("ws://api/ws?network=42161");
    client.close();
  });

  it("revokes the confirmation on a mismatching hello, so a stale hello for the final target cannot admit another network's frames", () => {
    // Rapid A -> C -> B -> C: the hello answering the first C matches the
    // final target and confirms it; B's hello must undo that, and B's blocks
    // (which carry no chain id) must be dropped until the final C hello.
    const { client, onBlocks, onOwnerAction, onTick, statuses } = makeClient();
    client.connect();
    const socket = latest();
    socket.serverOpen();
    socket.serverHello("robinhood", 4663);
    client.subscribe("robinhood-testnet");
    client.subscribe("arbitrum-one");
    client.subscribe("robinhood-testnet");
    socket.serverHello("robinhood-testnet", 46630);
    expect(client.confirmedNetwork).toEqual(TESTNET);
    expect(client.status).toBe("open");
    socket.serverHello("arbitrum-one", 42161);
    expect(client.confirmedNetwork).toBeNull();
    expect(client.status).toBe("connecting");
    socket.serverMessage({ type: "blocks", data: [{ number: 9 }] });
    socket.serverMessage({ type: "owner_action", data: { method: "x" } });
    socket.serverMessage({ type: "tick", data: { chainId: 42161 } });
    socket.serverMessage({ type: "tick", data: { chainId: 46630 } });
    expect(onBlocks).not.toHaveBeenCalled();
    expect(onOwnerAction).not.toHaveBeenCalled();
    expect(onTick).not.toHaveBeenCalled();
    socket.serverHello("robinhood-testnet", 46630);
    expect(client.status).toBe("open");
    socket.serverMessage({ type: "blocks", data: [{ number: 10 }] });
    expect(onBlocks).toHaveBeenCalledWith([{ number: 10 }], TESTNET);
    expect(statuses).toEqual(["open", "connecting", "open", "connecting", "open"]);
    client.close();
  });

  it("drops a socket whose subscription the server rejects, and reconnects into polling", () => {
    const { client, onError } = makeClient();
    client.connect();
    const first = latest();
    first.serverOpen();
    first.serverMessage({ type: "error", error: { code: "unknown_network", message: "no such network" } });
    expect(onError).toHaveBeenCalledWith("no such network");
    expect(first.closed).toBe(true);
    expect(first.closeCode).toBe(4001);
    expect(client.status).toBe("reconnecting");
    vi.advanceTimersByTime(1000);
    const second = latest();
    second.serverOpen();
    second.serverMessage({ type: "error", error: { code: "unknown_network", message: "still no such network" } });
    expect(client.status).toBe("polling");
    vi.advanceTimersByTime(2000);
    const third = latest();
    third.serverOpen();
    third.serverHello("robinhood", 4663);
    expect(client.status).toBe("open");
    // An error answering a subscribe on a confirmed socket means no hello follows: drop it too.
    client.subscribe("arbitrum-one");
    third.serverMessage({ type: "error", error: { code: "unknown_network", message: "no arbitrum" } });
    expect(third.closed).toBe(true);
    expect(client.status).toBe("reconnecting");
    vi.advanceTimersByTime(1000);
    expect(latest().url).toBe("ws://api/ws?network=arbitrum-one");
    client.close();
  });

  it("abandons a subscription that is never acknowledged; pings do not count, the matching hello does", () => {
    const { client, statuses } = makeClient();
    client.connect();
    const socket = latest();
    socket.serverOpen();
    for (let i = 0; i < 3; i++) {
      vi.advanceTimersByTime(3000);
      socket.serverMessage({ type: "ping" });
    }
    expect(socket.closed).toBe(false);
    vi.advanceTimersByTime(ACK_WATCHDOG_MS - 9000 - 1);
    expect(socket.closed).toBe(false);
    vi.advanceTimersByTime(1);
    expect(socket.closed).toBe(true);
    expect(socket.closeCode).toBe(4002);
    expect(client.status).toBe("reconnecting");
    expect(statuses).toEqual(["reconnecting"]);
    vi.advanceTimersByTime(1000);
    const second = latest();
    second.serverOpen();
    vi.advanceTimersByTime(ACK_WATCHDOG_MS - 1);
    second.serverHello("robinhood", 4663);
    vi.advanceTimersByTime(ACK_WATCHDOG_MS * 2);
    expect(second.closed).toBe(false);
    // A subscribe re-arms it and its hello clears it.
    client.subscribe("arbitrum-one");
    vi.advanceTimersByTime(ACK_WATCHDOG_MS - 1);
    second.serverHello("arbitrum-one", 42161);
    vi.advanceTimersByTime(ACK_WATCHDOG_MS * 2);
    expect(second.closed).toBe(false);
    // A hello for the wrong network re-arms it as well.
    client.subscribe("robinhood-testnet");
    second.serverHello("arbitrum-one", 42161);
    vi.advanceTimersByTime(ACK_WATCHDOG_MS - 1);
    expect(second.closed).toBe(false);
    vi.advanceTimersByTime(1);
    expect(second.closed).toBe(true);
    client.close();
  });

  it("honours a custom acknowledgement window", () => {
    const { client } = makeClient({ ackWatchdogMs: 50 });
    client.connect();
    const socket = latest();
    socket.serverOpen();
    vi.advanceTimersByTime(50);
    expect(socket.closed).toBe(true);
    client.close();
  });

  it("accepts a hello by chain id when the route used one", () => {
    const { client, onHello, onTick } = makeClient({ network: "4663" });
    client.connect();
    const socket = latest();
    socket.serverOpen();
    socket.serverHello("robinhood", 4663);
    expect(onHello).toHaveBeenCalledWith(expect.anything(), ROBINHOOD);
    expect(client.confirmedNetwork).toEqual(ROBINHOOD);
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
    // Opening the TCP socket resets neither the status nor the backoff; the hello does both.
    expect(client.status).toBe("polling");
    expect(client.backoffMs()).toBe(30_000);
    latest().serverHello("robinhood", 4663);
    expect(client.status).toBe("open");
    expect(client.backoffMs()).toBe(1000);
    expect(statuses[0]).toBe("reconnecting");
    expect(statuses).toContain("polling");
    expect(statuses[statuses.length - 1]).toBe("open");
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
    expect(statuses).toEqual([]);
    expect(client.status).toBe("connecting");
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
