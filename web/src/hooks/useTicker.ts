"use client";

import { useEffect, useState } from "react";
import { useDocumentVisible } from "./useDocumentVisible";

/** Current time in milliseconds, refreshed every `intervalMs` while the tab is visible. */
export function useTicker(intervalMs = 250): number {
  const [now, setNow] = useState<number>(() => Date.now());
  const visible = useDocumentVisible();
  useEffect(() => {
    if (!visible) return;
    const timer = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(timer);
  }, [intervalMs, visible]);
  return now;
}
