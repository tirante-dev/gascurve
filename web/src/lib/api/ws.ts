// WebSocket client for docs/ARCHITECTURE.md section 7. It handles hello, reorg,
// tick, blocks, owner_action and ping (replying pong), switches network with a
// subscribe message on the same socket, and reconnects with exponential
// backoff from 1 s to 30 s. The hook layer polls /live while the status is
// anything other than open.
//
// Identity: the client tracks what it asked for (`requested`, a name or a
// decimal chain id) and what the server last confirmed (`confirmed`: the
// canonical name, the chain id and the subscription generation the hello
// answered). Nothing is delivered until a hello matching the current request
// arrives; ticks and reorgs must carry the confirmed chain id; frames without
// a chain id (blocks, owner_action) are accepted only while the confirmed
// generation is the current one. A subscribe whose target is an alias of the
// confirmed network (its name or its chain id) is a no-op: the server would
// not answer it with a hello. The status is `open` only once the current
// generation has been confirmed, and an acknowledgement watchdog that pings
// cannot satisfy closes a socket whose hello never comes.

import type { BlockPoint, ClientMessage, HelloData, LiveSnapshot, LiveStatus, OwnerAction, ReorgData, ServerMessage } from "@/types";
import { API_BASE_URL } from "./core";

/** The subset of the WebSocket interface the client uses, so tests and the mock can stand in. */
export interface SocketLike {
  onopen: ((ev: Event) => void) | null;
  onmessage: ((ev: MessageEvent) => void) | null;
  onclose: ((ev: CloseEvent) => void) | null;
  onerror: ((ev: Event) => void) | null;
  readonly readyState: number;
  send(data: string): void;
  close(code?: number, reason?: string): void;
}

export type SocketFactory = (url: string) => SocketLike;

export const SOCKET_OPEN = 1;

/** The server pings every 30 s; a socket that delivers nothing for this long is treated as dead. */
export const WATCHDOG_MS = 75_000;

/** A connect or subscribe that is not answered by a hello within this long is abandoned; pings do not count. */
export const ACK_WATCHDOG_MS = 10_000;

/** WebSocket endpoint: NEXT_PUBLIC_WS_URL, or the api URL with http swapped for ws and /ws appended. */
export function resolveWsUrl(apiBaseUrl: string = API_BASE_URL, explicit: string | undefined = process.env.NEXT_PUBLIC_WS_URL): string {
  if (explicit && explicit.trim() !== "") return explicit.replace(/\/+$/, "");
  return apiBaseUrl.replace(/^http/i, "ws") + "/ws";
}

/** Picks the real WebSocket, or the mock one when the app runs on mock data. */
export async function resolveSocketFactory(): Promise<SocketFactory> {
  if (process.env.NEXT_PUBLIC_USE_MOCK_DATA === "true") {
    const { MockWebSocket } = await import("@/lib/mock");
    return (url) => new MockWebSocket(url);
  }
  return (url) => new WebSocket(url);
}

/** The network a feed belongs to: the canonical name and chain id from the hello that confirmed it. */
export type NetworkKey = { name: string; chainId: number };

/** True when `target` (a route parameter) names the keyed network, by name or by decimal chain id. */
export function keyMatches(key: NetworkKey, target: string): boolean {
  return key.name === target || String(key.chainId) === target;
}

/** True when a hello's network is the one that was asked for, by name or by chain id. */
export function helloMatches(hello: HelloData, wanted: string): boolean {
  const network = hello.network;
  if (typeof network !== "object" || network === null) return false;
  if (typeof network.name !== "string" || typeof network.chainId !== "number") return false;
  return keyMatches(network, wanted);
}

/** Handlers receive the confirmed network (from the hello that answered the subscription) with every message. */
export type LiveClientHandlers = {
  onHello?: (data: HelloData, network: NetworkKey) => void;
  onTick?: (snapshot: LiveSnapshot, network: NetworkKey) => void;
  /** A reorg, sent before the next tick: the ring above `ancestor` is stale and `blocks` replace it. */
  onReorg?: (reorg: ReorgData, network: NetworkKey) => void;
  onBlocks?: (blocks: BlockPoint[], network: NetworkKey) => void;
  onOwnerAction?: (action: OwnerAction, network: NetworkKey) => void;
  onStatus?: (status: LiveStatus) => void;
  onError?: (message: string) => void;
};

export type LiveClientOptions = LiveClientHandlers & {
  url: string;
  network: string;
  socketFactory: SocketFactory;
  minBackoffMs?: number;
  maxBackoffMs?: number;
  watchdogMs?: number;
  ackWatchdogMs?: number;
};

type Confirmed = NetworkKey & { generation: number };

function parseServerMessage(raw: unknown): ServerMessage | null {
  if (typeof raw !== "string") return null;
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return null;
  }
  if (typeof parsed !== "object" || parsed === null) return null;
  const type = (parsed as { type?: unknown }).type;
  switch (type) {
    case "hello":
    case "reorg":
    case "tick":
    case "blocks":
    case "owner_action":
    case "ping":
    case "error":
      return parsed as ServerMessage;
    default:
      return null;
  }
}

export class LiveClient {
  private readonly options: LiveClientOptions;
  private socket: SocketLike | null = null;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private watchdogTimer: ReturnType<typeof setTimeout> | null = null;
  private ackTimer: ReturnType<typeof setTimeout> | null = null;
  private failures = 0;
  private closedByUser = false;
  private suspended = false;
  private currentStatus: LiveStatus = "connecting";
  /** What the caller asked for: a name or a decimal chain id. */
  private requested: string;
  /** The target the current socket was opened with or last sent a subscribe for. */
  private subscribedNetwork: string | null = null;
  /** Bumped on every connect and non-alias subscribe; a hello confirms the generation it answers. */
  private generation = 0;
  private confirmed: Confirmed | undefined;

  constructor(options: LiveClientOptions) {
    this.options = options;
    this.requested = options.network;
  }

  get status(): LiveStatus {
    return this.currentStatus;
  }

  /** The network the caller asked for, as given. */
  get activeNetwork(): string {
    return this.requested;
  }

  /** The network the server has confirmed with a hello for the current subscription, or null. */
  get confirmedNetwork(): NetworkKey | null {
    const c = this.confirmed;
    return c && c.generation === this.generation ? { name: c.name, chainId: c.chainId } : null;
  }

  private isConfirmed(): boolean {
    return this.confirmed !== undefined && this.confirmed.generation === this.generation;
  }

  private setStatus(status: LiveStatus): void {
    if (this.currentStatus === status) return;
    this.currentStatus = status;
    this.options.onStatus?.(status);
  }

  /** The status while a socket exists but the current subscription is not confirmed. */
  private pendingStatus(): LiveStatus {
    return this.failures === 0 ? "connecting" : this.failures === 1 ? "reconnecting" : "polling";
  }

  /** Opens the socket. Safe to call once; reconnects are scheduled internally. */
  connect(): void {
    this.closedByUser = false;
    this.suspended = false;
    this.open();
  }

  private open(): void {
    this.clearTimer();
    if (this.socket) return;
    this.setStatus(this.pendingStatus());
    const url = `${this.options.url}?network=${encodeURIComponent(this.requested)}`;
    let socket: SocketLike;
    try {
      socket = this.options.socketFactory(url);
    } catch {
      this.scheduleReconnect();
      return;
    }
    this.socket = socket;
    this.subscribedNetwork = this.requested;
    this.generation += 1;
    socket.onopen = () => {
      if (this.socket !== socket) return;
      // Neither the status nor the backoff counter change here: only the
      // hello for the current generation makes the feed live.
      this.armWatchdog(socket);
      if (this.subscribedNetwork !== this.requested) this.sendSubscribe(socket);
      else this.armAckWatchdog(socket);
    };
    socket.onmessage = (ev) => {
      if (this.socket !== socket) return;
      this.armWatchdog(socket);
      this.handleMessage(socket, ev.data);
    };
    socket.onerror = () => {
      // The close event that follows drives reconnection.
    };
    socket.onclose = () => {
      if (this.socket !== socket) return;
      this.socket = null;
      this.clearWatchdog();
      this.clearAckWatchdog();
      if (this.closedByUser || this.suspended) return;
      this.scheduleReconnect();
    };
  }

  private handleMessage(socket: SocketLike, raw: unknown): void {
    const message = parseServerMessage(raw);
    if (!message) return;
    switch (message.type) {
      case "hello": {
        if (!helloMatches(message.data, this.requested)) {
          // A hello for something else means the server is not on our
          // subscription: whatever was confirmed before no longer keys the
          // feed, the status is pending again, and the hello we do want has
          // the acknowledgement window to arrive.
          this.confirmed = undefined;
          this.setStatus(this.pendingStatus());
          this.armAckWatchdog(socket);
          return;
        }
        const key: NetworkKey = { name: message.data.network.name, chainId: message.data.network.chainId };
        this.confirmed = { ...key, generation: this.generation };
        this.failures = 0;
        this.clearAckWatchdog();
        this.setStatus("open");
        this.options.onHello?.(message.data, key);
        break;
      }
      case "tick": {
        const c = this.confirmed;
        if (!c || c.generation !== this.generation || message.data.chainId !== c.chainId) return;
        this.options.onTick?.(message.data, { name: c.name, chainId: c.chainId });
        break;
      }
      case "reorg": {
        const c = this.confirmed;
        if (!c || c.generation !== this.generation || message.data.chainId !== c.chainId) return;
        this.options.onReorg?.(message.data, { name: c.name, chainId: c.chainId });
        break;
      }
      case "blocks": {
        const c = this.confirmed;
        if (!c || c.generation !== this.generation) return;
        this.options.onBlocks?.(message.data, { name: c.name, chainId: c.chainId });
        break;
      }
      case "owner_action": {
        const c = this.confirmed;
        if (!c || c.generation !== this.generation) return;
        this.options.onOwnerAction?.(message.data, { name: c.name, chainId: c.chainId });
        break;
      }
      case "error":
        this.options.onError?.(message.error.message);
        // While confirmed the socket stays open (the server keeps it open too).
        // An error answering the current subscription means no hello is coming:
        // drop the socket so the backoff, and then polling, take over.
        if (!this.isConfirmed()) this.abandon(socket, 4001, "subscription rejected");
        break;
      case "ping":
        this.send(socket, { type: "pong" });
        break;
    }
  }

  private send(socket: SocketLike, message: ClientMessage): void {
    try {
      socket.send(JSON.stringify(message));
    } catch {
      // A send on a closing socket is dropped; the close handler reconnects.
    }
  }

  private sendSubscribe(socket: SocketLike): void {
    this.subscribedNetwork = this.requested;
    this.send(socket, { type: "subscribe", network: this.requested });
    this.armAckWatchdog(socket);
  }

  /** Backoff: min * 2^failures, capped at max. */
  backoffMs(): number {
    const min = this.options.minBackoffMs ?? 1000;
    const max = this.options.maxBackoffMs ?? 30_000;
    return Math.min(max, min * 2 ** this.failures);
  }

  private scheduleReconnect(): void {
    const delay = this.backoffMs();
    this.failures += 1;
    this.setStatus(this.failures >= 2 ? "polling" : "reconnecting");
    this.clearTimer();
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this.open();
    }, delay);
  }

  private clearTimer(): void {
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
  }

  /** Closes `socket` as unusable and schedules a reconnect unless the client is stopped. */
  private abandon(socket: SocketLike, code: number, reason: string): void {
    if (this.socket !== socket) return;
    this.socket = null;
    this.clearWatchdog();
    this.clearAckWatchdog();
    try {
      socket.close(code, reason);
    } catch {
      // A socket that refuses to close is abandoned; its events are ignored as stale.
    }
    if (this.closedByUser || this.suspended) return;
    this.scheduleReconnect();
  }

  /** Restarts the receive watchdog; a silent socket is closed and reconnected. */
  private armWatchdog(socket: SocketLike): void {
    this.clearWatchdog();
    this.watchdogTimer = setTimeout(() => {
      this.watchdogTimer = null;
      this.abandon(socket, 4000, "no message within the watchdog interval");
    }, this.options.watchdogMs ?? WATCHDOG_MS);
  }

  private clearWatchdog(): void {
    if (this.watchdogTimer !== null) {
      clearTimeout(this.watchdogTimer);
      this.watchdogTimer = null;
    }
  }

  /** Starts waiting for the hello that answers the current subscription; only that hello clears it. */
  private armAckWatchdog(socket: SocketLike): void {
    this.clearAckWatchdog();
    this.ackTimer = setTimeout(() => {
      this.ackTimer = null;
      this.abandon(socket, 4002, "subscription not acknowledged");
    }, this.options.ackWatchdogMs ?? ACK_WATCHDOG_MS);
  }

  private clearAckWatchdog(): void {
    if (this.ackTimer !== null) {
      clearTimeout(this.ackTimer);
      this.ackTimer = null;
    }
  }

  /**
   * Switches network on the open socket, or on the next connection. Messages
   * are dropped until the new hello. A target that is an alias of the confirmed
   * network (its name for a chain-id route, or the reverse) keeps the
   * subscription as it is: the server would treat the subscribe as a no-op and
   * send no hello, so the feed stays live under the new reference.
   */
  subscribe(network: string): void {
    if (network === this.requested) return;
    const c = this.confirmed;
    if (c && c.generation === this.generation && keyMatches(c, network)) {
      this.requested = network;
      return;
    }
    this.requested = network;
    this.generation += 1;
    if (this.socket && this.socket.readyState === SOCKET_OPEN) {
      this.setStatus(this.pendingStatus());
      this.sendSubscribe(this.socket);
    }
  }

  /** Closes the socket without reconnecting, for hidden tabs. `resume` reopens it. */
  suspend(): void {
    this.suspended = true;
    this.clearTimer();
    this.clearWatchdog();
    this.clearAckWatchdog();
    const socket = this.socket;
    this.socket = null;
    socket?.close();
  }

  resume(): void {
    if (!this.suspended || this.closedByUser) return;
    this.suspended = false;
    this.failures = 0;
    this.open();
  }

  close(): void {
    this.closedByUser = true;
    this.clearTimer();
    this.clearWatchdog();
    this.clearAckWatchdog();
    const socket = this.socket;
    this.socket = null;
    socket?.close();
  }
}
