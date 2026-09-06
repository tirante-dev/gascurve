// A WebSocket stand-in that speaks docs/ARCHITECTURE.md section 7 against the
// mock world: hello on open, then a tick and a blocks message for every block
// (the collector samples every block, about ten a second on Robinhood, spread
// across the second they belong to), a ping every 30 s, and subscribe to
// switch networks. Blocks within one second share a timestamp, so the short
// window's backlog climbs block by block and drops at each second boundary.

import type { SocketLike } from "@/lib/api/ws";
import type { ServerMessage } from "@/types";
import { findMockWorld, mockNow } from "./registry";

const OPEN_DELAY_MS = 30;
const TICK_MS = 1000;
const PING_MS = 30_000;

export class MockWebSocket implements SocketLike {
  onopen: ((ev: Event) => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onclose: ((ev: CloseEvent) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  readyState = 0;
  readonly url: string;

  private network: string;
  private lastBlock = 0;
  private openTimer: ReturnType<typeof setTimeout> | null = null;
  private tickTimer: ReturnType<typeof setInterval> | null = null;
  private pingTimer: ReturnType<typeof setInterval> | null = null;
  private blockTimers: ReturnType<typeof setTimeout>[] = [];

  constructor(url: string) {
    this.url = url;
    let network = "robinhood";
    try {
      network = new URL(url, "ws://localhost").searchParams.get("network") ?? network;
    } catch {
      // Keep the default network for URLs the URL parser rejects.
    }
    this.network = network;
    this.openTimer = setTimeout(() => this.open(), OPEN_DELAY_MS);
  }

  private open(): void {
    this.openTimer = null;
    this.readyState = 1;
    this.onopen?.(new Event("open"));
    this.sendHello();
    this.tickTimer = setInterval(() => this.tick(), TICK_MS);
    this.pingTimer = setInterval(() => this.emit({ type: "ping" }), PING_MS);
  }

  private emit(message: ServerMessage): void {
    if (this.readyState !== 1) return;
    this.onmessage?.(new MessageEvent("message", { data: JSON.stringify(message) }));
  }

  private clearBlockTimers(): void {
    for (const timer of this.blockTimers) clearTimeout(timer);
    this.blockTimers = [];
  }

  private sendHello(): void {
    this.clearBlockTimers();
    const world = findMockWorld(this.network);
    if (!world) {
      this.close(4004, "unknown network");
      return;
    }
    const now = mockNow();
    world.advanceTo(now);
    const recentBlocks = world.recentBlocks(120);
    this.lastBlock = recentBlocks.length > 0 ? recentBlocks[recentBlocks.length - 1].number : 0;
    this.emit({ type: "hello", data: { network: world.network(now), snapshot: world.snapshot(now), recentBlocks } });
  }

  /**
   * Once a second the world advances and the second's blocks are published one
   * at a time, evenly spaced over the coming second: the first at once, the
   * rest on timers. Each carries its own tick, sampled at that moment.
   */
  private tick(): void {
    const world = findMockWorld(this.network);
    if (!world) return;
    const now = mockNow();
    world.advanceTo(now);
    const blocks = world.blocksAfter(this.lastBlock);
    if (blocks.length === 0) {
      this.emit({ type: "tick", data: world.snapshot(now) });
      return;
    }
    this.lastBlock = blocks[blocks.length - 1].number;
    this.clearBlockTimers();
    const spacing = TICK_MS / blocks.length;
    blocks.forEach((block, k) => {
      const publish = () => {
        this.emit({ type: "tick", data: world.snapshotForBlock(block, Date.now()) });
        this.emit({ type: "blocks", data: [block] });
      };
      if (k === 0) publish();
      else this.blockTimers.push(setTimeout(publish, Math.round(k * spacing)));
    });
  }

  send(data: string): void {
    if (this.readyState !== 1) throw new Error("socket is not open");
    let parsed: unknown;
    try {
      parsed = JSON.parse(data);
    } catch {
      return;
    }
    if (typeof parsed !== "object" || parsed === null) return;
    const message = parsed as { type?: unknown; network?: unknown };
    if (message.type === "subscribe" && typeof message.network === "string") {
      this.network = message.network;
      this.sendHello();
    }
  }

  close(code?: number, reason?: string): void {
    if (this.readyState === 3) return;
    if (this.openTimer !== null) clearTimeout(this.openTimer);
    if (this.tickTimer !== null) clearInterval(this.tickTimer);
    if (this.pingTimer !== null) clearInterval(this.pingTimer);
    this.clearBlockTimers();
    this.openTimer = null;
    this.tickTimer = null;
    this.pingTimer = null;
    this.readyState = 3;
    this.onclose?.(new CloseEvent("close", { code: code ?? 1000, reason: reason ?? "" }));
  }
}
