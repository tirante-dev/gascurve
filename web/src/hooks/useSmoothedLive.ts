"use client";

import { useEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import { createFrameStore, DISPLAY_INTERVAL_MS, targetValues, tweenValues, type FrameStore, type LiveFrame, type LiveValues } from "@/lib/smoothing";
import type { BlockPoint, LiveSnapshot } from "@/types";
import { useDocumentVisible } from "./useDocumentVisible";
import type { LiveState } from "./useLive";

/**
 * The live section's view of the feed: `display` is the latest tick, changed
 * at most every DISPLAY_INTERVAL_MS; `frame` carries what moves faster (the
 * block ring, the eased figures, the wall clock at the last cadence commit)
 * and is read with useLiveFrame by the components that animate, so the rest
 * of the page re-renders only at the cadence.
 */
export type SmoothedLive = { display: LiveSnapshot | null; frame: FrameStore };

export function prefersReducedMotion(): boolean {
  if (typeof window === "undefined" || typeof window.matchMedia !== "function") return false;
  return window.matchMedia("(prefers-reduced-motion: reduce)").matches;
}

type Loop = {
  /** The committed tick and the frame-clock time it arrived, the origin of the drain projection. */
  snapshot: LiveSnapshot | null;
  sampleAt: number;
  /** The newest tick waiting for the cadence; a newer one replaces it (trailing edge). */
  pending: { snapshot: LiveSnapshot; at: number } | null;
  blocks: BlockPoint[];
  lastCommit: number | null;
  lastFrame: number | null;
  values: LiveValues | null;
  /** True after a reorg: the next tick commits on the next frame and its values snap, as a first commit does. */
  fresh: boolean;
};

function emptyLoop(): Loop {
  return { snapshot: null, sampleAt: 0, pending: null, blocks: [], lastCommit: null, lastFrame: null, values: null, fresh: false };
}

/**
 * Smooths a feed that ticks many times a second into something the eye can
 * follow. One requestAnimationFrame loop does all of it, keyed on frame
 * timestamps rather than timers:
 *
 * - every DISPLAY_INTERVAL_MS the newest pending tick becomes `display` and
 *   the frame's wall clock is refreshed, so block number, "since last block"
 *   and everything else at the cadence move together;
 * - blocks are handed on every frame in which the ring changed, in one batch;
 * - the figures ease toward their targets with an exponential approach
 *   (see lib/smoothing), long-window backlogs toward a target that itself
 *   drains at the constraint's rate since the sample, so nothing snaps;
 * - with prefers-reduced-motion the tween and the drain are off and only the
 *   cadence applies;
 * - a hidden tab stops the loop; the feed itself is suspended by useLive;
 * - a reorg (`reorgs` changed) orphans the sampled block, so nothing on
 *   screen is a valid origin to ease from: the tick that follows is treated
 *   as a fresh commit, shown on the next frame with its values snapped.
 *
 * A cleared feed (network switch) clears the display at once rather than at
 * the next cadence, so a stale network is never shown under a new one.
 */
export function useSmoothedLive(live: Pick<LiveState, "snapshot" | "recentBlocks"> & Partial<Pick<LiveState, "reorgs">>): SmoothedLive {
  const [committed, setCommitted] = useState<LiveSnapshot | null>(null);
  const [frame] = useState(() => createFrameStore({ nowMs: Date.now() }));
  const visible = useDocumentVisible();
  const loop = useRef<Loop>(emptyLoop());
  const reorgs = live.reorgs ?? 0;
  const seenReorgs = useRef(reorgs);

  useEffect(() => {
    if (reorgs === seenReorgs.current) return;
    seenReorgs.current = reorgs;
    loop.current.fresh = true;
  }, [reorgs]);

  useEffect(() => {
    const s = loop.current;
    if (live.snapshot) {
      s.pending = { snapshot: live.snapshot, at: performance.now() };
      return;
    }
    s.pending = null;
    s.snapshot = null;
    s.values = null;
    s.lastCommit = null;
    frame.set({ ...frame.get(), blocks: [], values: null });
  }, [live.snapshot, frame]);

  useEffect(() => {
    loop.current.blocks = live.recentBlocks;
  }, [live.recentBlocks]);

  useEffect(() => {
    if (!visible || typeof requestAnimationFrame !== "function") return;
    const reduced = prefersReducedMotion();
    const s = loop.current;
    let handle = 0;
    const step = (t: number) => {
      const prev = frame.get();
      const dt = s.lastFrame === null ? 0 : t - s.lastFrame;
      s.lastFrame = t;
      const cadence = s.lastCommit === null || t - s.lastCommit >= DISPLAY_INTERVAL_MS || (s.fresh && s.pending !== null);
      let snapshotChanged = false;
      if (cadence) {
        s.lastCommit = t;
        if (s.pending) {
          s.snapshot = s.pending.snapshot;
          s.sampleAt = s.pending.at;
          s.pending = null;
          snapshotChanged = true;
          if (s.fresh) {
            // The first tick after a reorg: nothing to ease from.
            s.fresh = false;
            s.values = null;
          }
          setCommitted(s.snapshot);
        }
      }
      const blocksChanged = s.blocks !== prev.blocks;
      let values = s.values;
      if (s.snapshot && (!reduced || snapshotChanged || blocksChanged || values === null)) {
        const elapsedS = reduced ? 0 : Math.max(0, t - s.sampleAt) / 1000;
        const target = targetValues(s.snapshot, s.blocks, elapsedS);
        values = reduced ? target : tweenValues(values, target, dt);
      }
      const valuesChanged = values !== s.values;
      s.values = values;
      if (cadence || blocksChanged || valuesChanged) {
        frame.set({ blocks: s.blocks, values, nowMs: cadence ? Date.now() : prev.nowMs });
      }
      handle = requestAnimationFrame(step);
    };
    handle = requestAnimationFrame(step);
    return () => {
      cancelAnimationFrame(handle);
      s.lastFrame = null;
    };
  }, [visible, frame]);

  // A cleared feed hides the committed snapshot at once, and a new network's
  // first tick is never preceded by the old network's last one.
  const display = live.snapshot && committed && committed.chainId === live.snapshot.chainId ? committed : null;
  return useMemo(() => ({ display, frame }), [display, frame]);
}

/** Subscribes a component to the per-frame values; only call it where the numbers animate. */
export function useLiveFrame(store: FrameStore): LiveFrame {
  return useSyncExternalStore(store.subscribe, store.get, store.get);
}
