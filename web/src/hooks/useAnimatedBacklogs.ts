"use client";

import { useEffect, useState } from "react";
import { stepBacklogs } from "@/lib/pricer";
import type { Constraint } from "@/types";

function prefersReducedMotion(): boolean {
  if (typeof window === "undefined" || typeof window.matchMedia !== "function") return false;
  return window.matchMedia("(prefers-reduced-motion: reduce)").matches;
}

/**
 * Backlogs that drain at each constraint's target rate between ticks, using
 * requestAnimationFrame, and snap to the sampled values whenever `tickKey`
 * changes. With reduced motion the values only snap. `constraints` must be
 * referentially stable for a tick (memoize it on the snapshot).
 */
export function useAnimatedBacklogs(constraints: readonly Constraint[], tickKey: string): number[] {
  const [animated, setAnimated] = useState<{ key: string; values: number[] } | null>(null);

  useEffect(() => {
    if (prefersReducedMotion() || typeof requestAnimationFrame !== "function") return;
    const startedAt = performance.now();
    let frame = 0;
    const loop = (now: number) => {
      setAnimated({ key: tickKey, values: stepBacklogs(constraints, (now - startedAt) / 1000) });
      frame = requestAnimationFrame(loop);
    };
    frame = requestAnimationFrame(loop);
    return () => cancelAnimationFrame(frame);
  }, [constraints, tickKey]);

  return animated && animated.key === tickKey ? animated.values : constraints.map((c) => c.backlog);
}
