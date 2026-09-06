"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { getLive } from "@/lib/api/live";
import { keyMatches, LiveClient, resolveSocketFactory, resolveWsUrl, type NetworkKey } from "@/lib/api/ws";
import type { BlockPoint, LiveSnapshot, LiveStatus, Network, OwnerAction } from "@/types";
import { useDocumentVisible } from "./useDocumentVisible";

/**
 * Blocks kept for the sparklines. The short-window sawtooth needs fifteen
 * seconds of per-block backlogs, and Robinhood produces about ten a second.
 */
export const RECENT_BLOCKS_RING = 240;
export const LIVE_POLL_MS = 2000;

export type LiveState = {
  snapshot: LiveSnapshot | null;
  recentBlocks: BlockPoint[];
  status: LiveStatus;
  networkInfo: Network | null;
  /** Owner actions seen over the socket since the page loaded, newest first. */
  ownerActions: OwnerAction[];
  /** Reorgs applied to this feed since its hello; a change means the ring's tail was replaced. */
  reorgs: number;
  /** True while a reorg has orphaned the snapshot and the canonical tick has not arrived. */
  resyncing: boolean;
  error: string | null;
};

/**
 * The feed is keyed by the network the server confirmed (name and chain id),
 * so a route that switches from `/4663` to `/robinhood` keeps showing it. A
 * feed filled by polling knows the chain id from the snapshot and the name
 * only as the route parameter it was polled under.
 */
type Feed = {
  key: NetworkKey | null;
  snapshot: LiveSnapshot | null;
  recentBlocks: BlockPoint[];
  networkInfo: Network | null;
  ownerActions: OwnerAction[];
  reorgs: number;
  resyncing: boolean;
};

function emptyFeed(key: NetworkKey | null): Feed {
  return { key, snapshot: null, recentBlocks: [], networkInfo: null, ownerActions: [], reorgs: 0, resyncing: false };
}

/** True when the feed belongs to the keyed network: chain ids are the identity, names are aliases. */
function feedIs(feed: Feed, key: NetworkKey): boolean {
  return feed.key !== null && feed.key.chainId === key.chainId;
}

/** True when the feed is the one a route parameter (name or chain id) refers to. */
export function feedMatches(feed: Pick<Feed, "key">, network: string): boolean {
  return feed.key !== null && keyMatches(feed.key, network);
}

/** Appends `incoming` to `ring`, dropping duplicates and keeping the newest `size`. */
export function appendBlocks(ring: BlockPoint[], incoming: BlockPoint[], size = RECENT_BLOCKS_RING): BlockPoint[] {
  const last = ring.length > 0 ? ring[ring.length - 1].number : -1;
  const fresh = incoming.filter((b) => b.number > last).sort((a, b) => a.number - b.number);
  if (fresh.length === 0) return ring;
  const merged = ring.concat(fresh);
  return merged.length > size ? merged.slice(merged.length - size) : merged;
}

/**
 * Applies a reorg to `ring`: every block above `ancestor` is dropped and the
 * canonical `blocks` (oldest first) take their place. Returns `ring` itself
 * when nothing changes.
 */
export function applyReorg(ring: BlockPoint[], ancestor: number, blocks: BlockPoint[], size = RECENT_BLOCKS_RING): BlockPoint[] {
  const kept = ring.filter((b) => b.number <= ancestor);
  return appendBlocks(kept.length === ring.length ? ring : kept, blocks, size);
}

/**
 * True when `next` is newer than `prev`: a higher block number, or the same
 * block sampled later. A missing `prev` accepts anything. Out-of-order poll
 * responses are rejected with this.
 */
export function isNewerSnapshot(prev: LiveSnapshot | null, next: LiveSnapshot): boolean {
  if (!prev) return true;
  if (next.block.number !== prev.block.number) return next.block.number > prev.block.number;
  const prevAt = Date.parse(prev.sampledAt);
  const nextAt = Date.parse(next.sampledAt);
  if (Number.isNaN(prevAt) || Number.isNaN(nextAt)) return false;
  return nextAt > prevAt;
}

/**
 * Live data for one network: the latest snapshot, a ring of recent blocks and
 * the connection status. Uses the WebSocket, polls /live every 2 s while the
 * socket is down, and stops everything while the tab is hidden. State is keyed
 * by the network the server confirmed with a hello (name and chain id), so
 * switching shows an empty feed until the new hello arrives, a late message for
 * the previous network is never shown under the new one, and moving between a
 * chain-id route and its name keeps the feed. A reorg repairs the ring, the
 * snapshot and the live owner actions in one step, so what is on screen is
 * always one consistent chain.
 */
export function useLive(network: string): LiveState {
  const [feed, setFeed] = useState<Feed>(() => emptyFeed(null));
  const [status, setStatus] = useState<LiveStatus>("connecting");
  const [error, setError] = useState<string | null>(null);
  const visible = useDocumentVisible();
  const clientRef = useRef<LiveClient | null>(null);
  const wantedNetwork = useRef(network);
  const visibleRef = useRef(visible);
  useEffect(() => {
    visibleRef.current = visible;
  }, [visible]);

  // One socket for the life of the hook; network changes use subscribe.
  useEffect(() => {
    let cancelled = false;
    let client: LiveClient | null = null;
    resolveSocketFactory().then((socketFactory) => {
      if (cancelled) return;
      const created = new LiveClient({
        url: resolveWsUrl(),
        network: wantedNetwork.current,
        socketFactory,
        onHello: (hello, key) => {
          setFeed((prev) => ({
            key,
            snapshot: hello.snapshot,
            recentBlocks: appendBlocks([], hello.recentBlocks),
            networkInfo: hello.network,
            // A hello carries no owner actions, so a reconnect to the same
            // chain keeps the live ones rather than erasing what was seen
            // since the page loaded; another chain starts empty.
            ownerActions: feedIs(prev, key) ? prev.ownerActions : [],
            reorgs: 0,
            resyncing: false,
          }));
          setError(null);
        },
        onReorg: (reorg, key) => {
          setFeed((prev) => {
            const base = feedIs(prev, key) ? prev : emptyFeed(key);
            // The ring repair and the snapshot move together: a sample taken
            // on an orphaned block is not a valid origin to ease from, so it
            // goes with the blocks it was priced on and the feed reports
            // itself resyncing until the canonical tick lands. Owner actions
            // above the ancestor are orphaned too; the page revalidates the
            // REST list so persisted results are replaced as well.
            const orphaned = base.snapshot !== null && base.snapshot.block.number > reorg.ancestor;
            return {
              ...base,
              key,
              recentBlocks: applyReorg(base.recentBlocks, reorg.ancestor, reorg.blocks),
              snapshot: orphaned ? null : base.snapshot,
              ownerActions: base.ownerActions.filter((a) => a.block <= reorg.ancestor),
              reorgs: base.reorgs + 1,
              resyncing: orphaned,
            };
          });
        },
        onTick: (tick, key) => {
          setFeed((prev) => ({ ...(feedIs(prev, key) ? prev : emptyFeed(key)), key, snapshot: tick, resyncing: false }));
          setError(null);
        },
        onBlocks: (blocks, key) => {
          setFeed((prev) => {
            const base = feedIs(prev, key) ? prev : emptyFeed(key);
            return { ...base, key, recentBlocks: appendBlocks(base.recentBlocks, blocks) };
          });
        },
        onOwnerAction: (action, key) => {
          setFeed((prev) => {
            const base = feedIs(prev, key) ? prev : emptyFeed(key);
            return { ...base, key, ownerActions: [action, ...base.ownerActions] };
          });
        },
        onStatus: setStatus,
        onError: setError,
      });
      client = created;
      clientRef.current = created;
      // The factory resolves asynchronously; the tab may have been hidden
      // since mount, in which case the client waits for the next visibility change.
      if (visibleRef.current) created.connect();
      else created.suspend();
    });
    return () => {
      cancelled = true;
      client?.close();
      clientRef.current = null;
    };
  }, []);

  useEffect(() => {
    wantedNetwork.current = network;
    clientRef.current?.subscribe(network);
  }, [network]);

  useEffect(() => {
    const client = clientRef.current;
    if (!client) return;
    if (visible) client.resume();
    else client.suspend();
  }, [visible]);

  const polling = visible && (status === "reconnecting" || status === "polling");
  useEffect(() => {
    if (!polling) return;
    let active = true;
    let timer: ReturnType<typeof setTimeout> | null = null;
    const controller = new AbortController();
    // One request in flight at a time: the next poll is scheduled only after
    // the previous one settled, and a result is applied only when it is newer
    // than what is already shown.
    const poll = async () => {
      try {
        const next = await getLive(network, { signal: controller.signal, retries: 0 });
        if (!active) return;
        setFeed((prev) => {
          // The route parameter stands in for the name until a hello supplies the canonical one.
          const key: NetworkKey = { name: prev.key && feedMatches(prev, network) ? prev.key.name : network, chainId: next.chainId };
          const base = feedMatches(prev, network) ? prev : emptyFeed(key);
          return isNewerSnapshot(base.snapshot, next) ? { ...base, key, snapshot: next, resyncing: false } : base;
        });
        setError(null);
      } catch (err: unknown) {
        if (!active || controller.signal.aborted) return;
        setError(err instanceof Error ? err.message : String(err));
      }
      if (active) timer = setTimeout(() => void poll(), LIVE_POLL_MS);
    };
    void poll();
    return () => {
      active = false;
      controller.abort();
      if (timer !== null) clearTimeout(timer);
    };
  }, [polling, network]);

  return useMemo(() => {
    const current = feedMatches(feed, network) ? feed : emptyFeed(null);
    return {
      snapshot: current.snapshot,
      recentBlocks: current.recentBlocks,
      status,
      networkInfo: current.networkInfo,
      ownerActions: current.ownerActions,
      reorgs: current.reorgs,
      resyncing: current.resyncing,
      error,
    };
  }, [feed, network, status, error]);
}
