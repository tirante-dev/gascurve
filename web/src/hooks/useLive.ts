"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { getLive } from "@/lib/api/live";
import { LiveClient, resolveSocketFactory, resolveWsUrl } from "@/lib/api/ws";
import type { BlockPoint, LiveSnapshot, LiveStatus, Network, OwnerAction } from "@/types";
import { useDocumentVisible } from "./useDocumentVisible";

export const RECENT_BLOCKS_RING = 120;
export const LIVE_POLL_MS = 2000;

export type LiveState = {
  snapshot: LiveSnapshot | null;
  recentBlocks: BlockPoint[];
  status: LiveStatus;
  networkInfo: Network | null;
  /** Owner actions seen over the socket since the page loaded, newest first. */
  ownerActions: OwnerAction[];
  error: string | null;
};

type Feed = {
  network: string;
  snapshot: LiveSnapshot | null;
  recentBlocks: BlockPoint[];
  networkInfo: Network | null;
  ownerActions: OwnerAction[];
};

function emptyFeed(network: string): Feed {
  return { network, snapshot: null, recentBlocks: [], networkInfo: null, ownerActions: [] };
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
 * by the network the server confirmed with a hello, so switching shows an
 * empty feed until the new hello arrives and a late message for the previous
 * network is never shown under the new one.
 */
export function useLive(network: string): LiveState {
  const [feed, setFeed] = useState<Feed>(() => emptyFeed(network));
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
        onHello: (hello, confirmed) => {
          setFeed({ network: confirmed, snapshot: hello.snapshot, recentBlocks: appendBlocks([], hello.recentBlocks), networkInfo: hello.network, ownerActions: [] });
          setError(null);
        },
        onTick: (tick, confirmed) => {
          setFeed((prev) => ({ ...(prev.network === confirmed ? prev : emptyFeed(confirmed)), snapshot: tick }));
          setError(null);
        },
        onBlocks: (blocks, confirmed) => {
          setFeed((prev) => {
            const base = prev.network === confirmed ? prev : emptyFeed(confirmed);
            return { ...base, recentBlocks: appendBlocks(base.recentBlocks, blocks) };
          });
        },
        onOwnerAction: (action, confirmed) => {
          setFeed((prev) => {
            const base = prev.network === confirmed ? prev : emptyFeed(confirmed);
            return { ...base, ownerActions: [action, ...base.ownerActions] };
          });
        },
        onStatus: setStatus,
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
          const base = prev.network === network ? prev : emptyFeed(network);
          return isNewerSnapshot(base.snapshot, next) ? { ...base, snapshot: next } : base;
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
    const current = feed.network === network ? feed : emptyFeed(network);
    return { snapshot: current.snapshot, recentBlocks: current.recentBlocks, status, networkInfo: current.networkInfo, ownerActions: current.ownerActions, error };
  }, [feed, network, status, error]);
}
