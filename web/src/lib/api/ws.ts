// WebSocket client for docs/ARCHITECTURE.md section 7. It handles hello, tick,
// blocks, owner_action and ping (replying pong), switches network with a
// subscribe message on the same socket, and reconnects with exponential
// backoff from 1 s to 30 s. The hook layer polls /live while the status is
// anything other than open.
//
// Every message is keyed by the network the server has confirmed: after a
// connect or subscribe nothing is delivered until a hello for the wanted
// network arrives, ticks must carry that network's chain id, and messages
// without a chain id (blocks, owner_action) are accepted only while the
// current subscription generation is the confirmed one.

import type { BlockPoint, ClientMessage, HelloData, LiveSnapshot, LiveStatus, OwnerAction, ServerMessage } from "@/types";
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

/** True when a hello's network is the one that was asked for, by name or by chain id. */
export function helloMatches(hello: HelloData, wanted: string): boolean {
  const network = hello.network;
  if (typeof network !== "object" || network === null) return false;
  return network.name === wanted || String(network.chainId) === wanted;
}

/** Handlers receive the confirmed network (the subscribe target the hello answered) with every message. */
export type LiveClientHandlers = {
  onHello?: (data: HelloData, network: string) => void;
  onTick?: (snapshot: LiveSnapshot, network: string) => void;
  onBlocks?: (blocks: BlockPoint[], network: string) => void;
  onOwnerAction?: (action: OwnerAction, network: string) => void;
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
};

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
  private failures = 0;
  private closedByUser = false;
  private suspended = false;
  private currentStatus: LiveStatus = "connecting";
  private network: string;
  private subscribedNetwork: string | null = null;
  /** Bumped on every connect and subscribe; a hello confirms the generation it answers. */
  private generation = 0;
  private confirmedGeneration = -1;
  private confirmedChainId: number | null = null;

  constructor(options: LiveClientOptions) {
    this.options = options;
    this.network = options.network;
  }

  get status(): LiveStatus {
    return this.currentStatus;
  }

  get activeNetwork(): string {
    return this.network;
  }

  /** The network the server has confirmed with a hello for the current subscription, or null. */
  get confirmedNetwork(): string | null {
    return this.confirmedGeneration === this.generation ? this.network : null;
  }

  private setStatus(status: LiveStatus): void {
    if (this.currentStatus === status) return;
    this.currentStatus = status;
    this.options.onStatus?.(status);
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
    this.setStatus(this.failures === 0 ? "connecting" : this.failures === 1 ? "reconnecting" : "polling");
    const url = `${this.options.url}?network=${encodeURIComponent(this.network)}`;
    let socket: SocketLike;
    try {
      socket = this.options.socketFactory(url);
    } catch {
      this.scheduleReconnect();
      return;
    }
    this.socket = socket;
    this.subscribedNetwork = this.network;
    this.generation += 1;
    socket.onopen = () => {
      if (this.socket !== socket) return;
      // The backoff counter is reset by the first valid hello, not here: a
      // server that accepts and immediately drops the socket must still back off.
      this.setStatus("open");
      this.armWatchdog(socket);
      if (this.subscribedNetwork !== this.network) this.sendSubscribe(socket);
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
      if (this.closedByUser || this.suspended) return;
      this.scheduleReconnect();
    };
  }

  private handleMessage(socket: SocketLike, raw: unknown): void {
    const message = parseServerMessage(raw);
    if (!message) return;
    const confirmed = this.confirmedGeneration === this.generation;
    switch (message.type) {
      case "hello":
        if (!helloMatches(message.data, this.network)) return;
        this.confirmedGeneration = this.generation;
        this.confirmedChainId = message.data.network.chainId;
        this.failures = 0;
        this.options.onHello?.(message.data, this.network);
        break;
      case "tick":
        if (!confirmed || message.data.chainId !== this.confirmedChainId) return;
        this.options.onTick?.(message.data, this.network);
        break;
      case "blocks":
        if (!confirmed) return;
        this.options.onBlocks?.(message.data, this.network);
        break;
      case "error":
        // The server keeps the socket open (for example an unknown subscribe target).
        this.options.onError?.(message.error.message);
        break;
      case "owner_action":
        if (!confirmed) return;
        this.options.onOwnerAction?.(message.data, this.network);
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
    this.subscribedNetwork = this.network;
    this.send(socket, { type: "subscribe", network: this.network });
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

  /** Restarts the receive watchdog; a silent socket is closed and reconnected. */
  private armWatchdog(socket: SocketLike): void {
    this.clearWatchdog();
    this.watchdogTimer = setTimeout(() => {
      this.watchdogTimer = null;
      if (this.socket !== socket) return;
      this.socket = null;
      try {
        socket.close(4000, "no message within the watchdog interval");
      } catch {
        // A socket that refuses to close is abandoned; its events are ignored as stale.
      }
      if (this.closedByUser || this.suspended) return;
      this.scheduleReconnect();
    }, this.options.watchdogMs ?? WATCHDOG_MS);
  }

  private clearWatchdog(): void {
    if (this.watchdogTimer !== null) {
      clearTimeout(this.watchdogTimer);
      this.watchdogTimer = null;
    }
  }

  /** Switches network on the open socket, or on the next connection. Messages are dropped until the new hello. */
  subscribe(network: string): void {
    if (network === this.network) return;
    this.network = network;
    this.generation += 1;
    if (this.socket && this.socket.readyState === SOCKET_OPEN) {
      this.sendSubscribe(this.socket);
    }
  }

  /** Closes the socket without reconnecting, for hidden tabs. `resume` reopens it. */
  suspend(): void {
    this.suspended = true;
    this.clearTimer();
    this.clearWatchdog();
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
    const socket = this.socket;
    this.socket = null;
    socket?.close();
  }
}
