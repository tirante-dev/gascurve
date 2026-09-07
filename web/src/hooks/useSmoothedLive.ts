"use client";

import { useEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import { latestConstraintBlock } from "@/lib/ownerActions";
import { assignPlaces, createFrameStore, DISPLAY_INTERVAL_MS, definitionOf, NO_PLACES, signatureOf, targetValues, tweenValues, type BlockPlaces, type FrameStore, type LiveFrame, type LiveValues } from "@/lib/smoothing";
import type { BlockPoint, LiveSnapshot } from "@/types";
import { useDocumentVisible } from "./useDocumentVisible";
import type { LiveState } from "./useLive";

/**
 * The live section's view of the feed: `display` is the latest tick, changed at most every
 * DISPLAY_INTERVAL_MS; `frame` carries what moves faster and is read with useLiveFrame by the components
 * that animate, so the rest of the page re-renders only at the cadence. `resyncing` is true while a reorg
 * has taken the displayed state away.
 */
export type SmoothedLive = { display: LiveSnapshot | null; frame: FrameStore; resyncing: boolean };

const REDUCED_MOTION_QUERY = "(prefers-reduced-motion: reduce)";

/** The reduced-motion media query, or null where matchMedia is not available (the server, old jsdom). */
export function reducedMotionQuery(): MediaQueryList | null {
  if (typeof window === "undefined" || typeof window.matchMedia !== "function") return null;
  return window.matchMedia(REDUCED_MOTION_QUERY);
}

export function prefersReducedMotion(): boolean {
  return reducedMotionQuery()?.matches ?? false;
}

type Pending = {
  snapshot: LiveSnapshot;
  at: number;
  /** The reorg count this sample arrived under; only a sample from the current one consumes `fresh`. */
  seq: number;
};

type Loop = {
  /** The committed tick and the frame-clock time it arrived, the origin of the drain projection. */
  snapshot: LiveSnapshot | null;
  sampleAt: number;
  /** The newest tick waiting for the cadence; a newer one replaces it (trailing edge). */
  pending: Pending | null;
  blocks: BlockPoint[];
  /** Where each block of the ring sits, assigned once per block and evicted with it. */
  places: BlockPlaces;
  lastCommit: number | null;
  lastFrame: number | null;
  values: LiveValues | null;
  /** True after a reorg: the tick that follows commits on the next frame and its values snap, as a first commit does. */
  fresh: boolean;
  /** Reorgs seen; stamped on each pending sample so `fresh` is consumed only by a post-reorg one. */
  seq: number;
  /** The constraint definition the committed snapshot was priced under, and the first block known to use it. */
  signature: string | null;
  signatureSince: number;
  /** The block of an owner call that replaced the constraints, waiting for the next commit. A replacement
   * that reinstalls identical targets still resets the backlogs, so the signature alone does not see it. */
  pendingConstraintBlock: number | null;
};

function emptyLoop(): Loop {
  return {
    snapshot: null,
    sampleAt: 0,
    pending: null,
    blocks: [],
    places: NO_PLACES,
    lastCommit: null,
    lastFrame: null,
    values: null,
    fresh: false,
    seq: 0,
    signature: null,
    signatureSince: Number.NEGATIVE_INFINITY,
    pendingConstraintBlock: null,
  };
}

/**
 * Smooths a feed that ticks many times a second into something the eye can follow. One
 * requestAnimationFrame loop does all of it: the newest pending tick becomes `display` every
 * DISPLAY_INTERVAL_MS, blocks are handed on in batches, and the figures ease toward their targets (see
 * lib/smoothing) with long-window backlogs draining at the constraint's rate. A changed constraint
 * definition snaps every figure; prefers-reduced-motion, read again every frame, leaves only the cadence.
 * A reorg clears the display rather than easing from an orphan, as does a cleared feed.
 */
export function useSmoothedLive(
  live: Pick<LiveState, "snapshot" | "recentBlocks"> & Partial<Pick<LiveState, "reorgs" | "resyncing" | "ownerActions">>,
  enabled = true,
): SmoothedLive {
  const [committed, setCommitted] = useState<LiveSnapshot | null>(null);
  const [frame] = useState(() => createFrameStore({ nowMs: Date.now() }));
  const visible = useDocumentVisible();
  const loop = useRef<Loop>(emptyLoop());
  const reorgs = live.reorgs ?? 0;
  const seenReorgs = useRef(reorgs);
  const reduced = useRef(prefersReducedMotion());
  // The newest owner call that replaced what prices a block. One with identical targets leaves the
  // signature alone but still installs new starting backlogs, so it is tracked in its own right.
  const ownerActions = live.ownerActions;
  const constraintBlock = useMemo(() => (ownerActions ? latestConstraintBlock(ownerActions) : null), [ownerActions]);
  const seenConstraintBlock = useRef<number | null>(constraintBlock);

  useEffect(() => {
    if (constraintBlock === seenConstraintBlock.current) return;
    seenConstraintBlock.current = constraintBlock;
    if (constraintBlock === null) return;
    loop.current.pendingConstraintBlock = constraintBlock;
  }, [constraintBlock]);

  useEffect(() => {
    if (reorgs === seenReorgs.current) return;
    seenReorgs.current = reorgs;
    const s = loop.current;
    s.seq += 1;
    s.fresh = true;
  }, [reorgs]);

  // The preference can change while the page stays open, so the loop reads it every frame.
  useEffect(() => {
    const query = reducedMotionQuery();
    if (!query) return;
    reduced.current = query.matches;
    if (typeof query.addEventListener !== "function") return;
    const onChange = () => {
      reduced.current = query.matches;
    };
    query.addEventListener("change", onChange);
    return () => query.removeEventListener("change", onChange);
  }, []);

  useEffect(() => {
    const s = loop.current;
    if (live.snapshot) {
      s.pending = { snapshot: live.snapshot, at: performance.now(), seq: s.seq };
      return;
    }
    s.pending = null;
    s.snapshot = null;
    s.values = null;
    s.lastCommit = null;
    s.signature = null;
    s.signatureSince = Number.NEGATIVE_INFINITY;
    s.pendingConstraintBlock = null;
    frame.set({ ...frame.get(), blocks: [], places: NO_PLACES, values: null });
  }, [live.snapshot, frame]);

  useEffect(() => {
    const s = loop.current;
    s.blocks = live.recentBlocks;
    // A block is placed once, when it enters the ring. Every live chart reads the same map, so the fee
    // chart and the throughput chart under it agree on where a block is.
    s.places = assignPlaces(s.places, live.recentBlocks);
  }, [live.recentBlocks]);

  useEffect(() => {
    if (!enabled || !visible || typeof requestAnimationFrame !== "function") return;
    const s = loop.current;
    let handle = 0;
    const step = (t: number) => {
      const prev = frame.get();
      const dt = s.lastFrame === null ? 0 : t - s.lastFrame;
      s.lastFrame = t;
      const forced = s.fresh && s.pending !== null && s.pending.seq === s.seq;
      const cadence = s.lastCommit === null || t - s.lastCommit >= DISPLAY_INTERVAL_MS || forced;
      let snapshotChanged = false;
      let values = s.values;
      if (cadence) {
        s.lastCommit = t;
        if (s.pending) {
          const committing = s.pending;
          s.snapshot = committing.snapshot;
          s.sampleAt = committing.at;
          s.pending = null;
          snapshotChanged = true;
          if (s.fresh && committing.seq === s.seq) {
            // The first tick after a reorg: nothing to ease from.
            s.fresh = false;
            s.values = null;
            values = null;
          }
          // A new constraint definition makes the old figures meaningless: the tween snaps on the
          // signature, and the short-window average stops before the block that introduced it.
          const signature = signatureOf(definitionOf(committing.snapshot));
          if (s.signature !== null && s.signature !== signature) s.signatureSince = committing.snapshot.block.number;
          // An owner call that replaced the constraints, parameter-identical or not: the backlogs it
          // installed have nothing to do with the ones before it.
          if (s.pendingConstraintBlock !== null) {
            s.signatureSince = s.pendingConstraintBlock;
            s.pendingConstraintBlock = null;
            s.values = null;
            values = null;
          }
          s.signature = signature;
          setCommitted(s.snapshot);
        }
      }
      const blocksChanged = s.blocks !== prev.blocks;
      const still = reduced.current;
      if (s.snapshot && (!still || snapshotChanged || blocksChanged || values === null)) {
        const elapsedS = still ? 0 : Math.max(0, t - s.sampleAt) / 1000;
        const target = targetValues(s.snapshot, s.blocks, elapsedS, s.signatureSince);
        values = still ? target : tweenValues(values, target, dt);
      }
      const valuesChanged = values !== s.values;
      s.values = values;
      if (cadence || blocksChanged || valuesChanged) {
        frame.set({ blocks: s.blocks, places: s.places, values, nowMs: cadence ? Date.now() : prev.nowMs });
      }
      handle = requestAnimationFrame(step);
    };
    handle = requestAnimationFrame(step);
    return () => {
      cancelAnimationFrame(handle);
      s.lastFrame = null;
    };
  }, [enabled, visible, frame]);

  // A cleared feed hides the committed snapshot at once, so a new network's first tick is never preceded
  // by the old network's last.
  const display = live.snapshot && committed && committed.chainId === live.snapshot.chainId ? committed : null;
  const resyncing = (live.resyncing ?? false) && display === null;
  return useMemo(() => ({ display, frame, resyncing }), [display, frame, resyncing]);
}

/** Subscribes a component to the per-frame values; only call it where the numbers animate. */
export function useLiveFrame(store: FrameStore): LiveFrame {
  return useSyncExternalStore(store.subscribe, store.get, store.get);
}
