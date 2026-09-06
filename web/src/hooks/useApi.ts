"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { useDocumentVisible } from "./useDocumentVisible";

export type ApiState<T> = {
  data: T | null;
  error: string | null;
  loading: boolean;
  /** Unix milliseconds of the last successful fetch. */
  updatedAt: number | null;
  refresh: () => void;
};

export type UseApiOptions = {
  /** Refetch interval in milliseconds; 0 disables it. Paused while the tab is hidden. */
  refetchMs?: number;
  /** When false nothing is fetched and the state reads as empty. */
  enabled?: boolean;
};

export function errorMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}

type Slot<T> = { key: string; request: number; data: T | null; error: string | null; updatedAt: number | null };

/**
 * Runs `fetcher` whenever `key` changes, keeping the previous data on screen
 * while the next result for the same key loads (no skeleton flash), and
 * refetching on an interval while the tab is visible. `fetcher` must be
 * referentially stable for a given key (wrap it in useCallback).
 */
export function useApi<T>(key: string | null, fetcher: (signal: AbortSignal) => Promise<T>, options: UseApiOptions = {}): ApiState<T> {
  const { refetchMs = 0, enabled = true } = options;
  const [slot, setSlot] = useState<Slot<T> | null>(null);
  const [version, setVersion] = useState(0);
  const visible = useDocumentVisible();
  const active = enabled && key !== null;

  const refresh = useCallback(() => setVersion((v) => v + 1), []);

  useEffect(() => {
    if (!enabled || key === null) return;
    const controller = new AbortController();
    let live = true;
    fetcher(controller.signal)
      .then((result) => {
        if (!live) return;
        setSlot({ key, request: version, data: result, error: null, updatedAt: Date.now() });
      })
      .catch((err: unknown) => {
        if (!live || controller.signal.aborted) return;
        setSlot((prev) => ({
          key,
          request: version,
          data: prev && prev.key === key ? prev.data : null,
          error: errorMessage(err),
          updatedAt: prev && prev.key === key ? prev.updatedAt : null,
        }));
      });
    return () => {
      live = false;
      controller.abort();
    };
  }, [key, enabled, version, fetcher]);

  useEffect(() => {
    if (!active || refetchMs <= 0 || !visible) return;
    const timer = setInterval(() => setVersion((v) => v + 1), refetchMs);
    return () => clearInterval(timer);
  }, [active, refetchMs, visible]);

  return useMemo(() => {
    const current = active && slot && slot.key === key ? slot : null;
    return {
      data: current?.data ?? null,
      error: current?.error ?? null,
      loading: active && (!current || current.request !== version),
      updatedAt: current?.updatedAt ?? null,
      refresh,
    };
  }, [active, slot, key, version, refresh]);
}
