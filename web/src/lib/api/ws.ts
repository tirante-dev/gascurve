// WebSocket client for docs/ARCHITECTURE.md section 7. It handles hello, tick,
// blocks, owner_action and ping (replying pong), switches network with a
// subscribe message on the same socket, and reconnects with exponential
// backoff from 1 s to 30 s. The hook layer polls /live while the status is
// anything other than open.

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

export type LiveClientHandlers = {
  onHello?: (data: HelloData) => void;
  onTick?: (snapshot: LiveSnapshot) => void;
  onBlocks?: (blocks: BlockPoint[]) => void;
  onOwnerAction?: (action: OwnerAction) => void;
  onStatus?: (status: LiveStatus) => void;
};

export type LiveClientOptions = LiveClientHandlers & {
  url: string;
  network: string;
  socketFactory: SocketFactory;
  minBackoffMs?: number;
  maxBackoffMs?: number;
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
      return parsed as ServerMessage;
    default:
      return null;
  }
}

export class LiveClient {
  private readonly options: LiveClientOptions;
  private socket: SocketLike | null = null;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private failures = 0;
  private closedByUser = false;
  private suspended = false;
  private currentStatus: LiveStatus = "connecting";
  private network: string;
  private subscribedNetwork: string | null = null;

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
    socket.onopen = () => {
      if (this.socket !== socket) return;
      this.failures = 0;
      this.setStatus("open");
      if (this.subscribedNetwork !== this.network) this.sendSubscribe(socket);
    };
    socket.onmessage = (ev) => {
      if (this.socket !== socket) return;
      this.handleMessage(socket, ev.data);
    };
    socket.onerror = () => {
      // The close event that follows drives reconnection.
    };
    socket.onclose = () => {
      if (this.socket !== socket) return;
      this.socket = null;
      if (this.closedByUser || this.suspended) return;
      this.scheduleReconnect();
    };
  }

  private handleMessage(socket: SocketLike, raw: unknown): void {
    const message = parseServerMessage(raw);
    if (!message) return;
    switch (message.type) {
      case "hello":
        this.options.onHello?.(message.data);
        break;
      case "tick":
        this.options.onTick?.(message.data);
        break;
      case "blocks":
        this.options.onBlocks?.(message.data);
        break;
      case "owner_action":
        this.options.onOwnerAction?.(message.data);
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

  /** Switches network on the open socket, or on the next connection. */
  subscribe(network: string): void {
    if (network === this.network) return;
    this.network = network;
    if (this.socket && this.socket.readyState === SOCKET_OPEN) {
      this.sendSubscribe(this.socket);
    }
  }

  /** Closes the socket without reconnecting, for hidden tabs. `resume` reopens it. */
  suspend(): void {
    this.suspended = true;
    this.clearTimer();
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
    const socket = this.socket;
    this.socket = null;
    socket?.close();
  }
}
