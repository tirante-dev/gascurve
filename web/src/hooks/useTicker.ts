"use client";

import { useEffect, useState } from "react";
import { useDocumentVisible } from "./useDocumentVisible";

/**
 * Current time in milliseconds, refreshed every `intervalMs` while the tab is
 * visible. An interval of 0 holds the first reading and starts no timer, which
 * is what a component that took its clock as a prop passes so it re-renders on
 * the prop alone.
 */
export function useTicker(intervalMs = 250): number {
  const [now, setNow] = useState<number>(() => Date.now());
  const visible = useDocumentVisible();
  useEffect(() => {
    if (!visible || intervalMs <= 0) return;
    const timer = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(timer);
  }, [intervalMs, visible]);
  return now;
}
