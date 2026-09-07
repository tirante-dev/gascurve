"use client";

import { useCallback } from "react";
import { getSeries } from "@/lib/api/series";
import type { Series, SeriesRange } from "@/types";
import { useApi, type ApiState } from "./useApi";

export const SERIES_REFETCH_MS = 60_000;

/** Ranges short enough that new buckets arrive while the page is open. */
export function refetchIntervalFor(range: SeriesRange): number {
  return range === "1h" || range === "24h" ? SERIES_REFETCH_MS : 0;
}

/** One key per (network, range): the two views that may ask for the same range share it. */
export function seriesKey(network: string, range: SeriesRange): string {
  return `${network}:${range}`;
}

/** One in-flight request, the controller that can cancel it, and how many views are waiting on it. */
type Shared = { promise: Promise<Series>; controller: AbortController; consumers: number; settled: boolean };

/** The request in flight for each key, so a second asker joins it instead of starting another. */
const inFlight = new Map<string, Shared>();

/** Forgets any request still in flight. Nothing is cached, so this only ever costs an extra fetch. */
export function resetSharedSeries(): void {
  inFlight.clear();
}

/**
 * The Series for (network, range), sharing one request with whoever else is asking for the same one: the
 * hero's chart and the history section mount and refetch together. Nothing is held after the request
 * settles, so no view is handed a stale range. One caller's abort never cancels the fetch another is
 * waiting on: consumers are counted per key against a shared controller, so rapid range switching no
 * longer leaves an abandoned request per visited key running through its timeout and two retries.
 */
export function fetchSharedSeries(network: string, range: SeriesRange, signal?: AbortSignal): Promise<Series> {
  const key = seriesKey(network, range);
  let entry = inFlight.get(key);
  if (entry === undefined) {
    const controller = new AbortController();
    const started: Shared = { promise: getSeries(network, range, { signal: controller.signal }), controller, consumers: 0, settled: false };
    const done = () => {
      started.settled = true;
      if (inFlight.get(key) === started) inFlight.delete(key);
    };
    started.promise.then(done, done);
    inFlight.set(key, started);
    entry = started;
  }
  const shared = entry;
  shared.consumers += 1;
  let released = false;
  const release = () => {
    if (released) return;
    released = true;
    signal?.removeEventListener("abort", release);
    shared.consumers -= 1;
    if (shared.consumers > 0 || shared.settled) return;
    // Nobody is waiting on this request any more.
    if (inFlight.get(key) === shared) inFlight.delete(key);
    shared.controller.abort();
  };
  shared.promise.then(release, release);
  signal?.addEventListener("abort", release);
  return shared.promise;
}

/** The bucketed history of a range. A null network or range fetches nothing and reads as empty. */
export function useSeries(network: string | null, range: SeriesRange | null): ApiState<Series> {
  const fetcher = useCallback((signal: AbortSignal) => fetchSharedSeries(network ?? "", range ?? "1h", signal), [network, range]);
  return useApi<Series>(network !== null && range !== null ? seriesKey(network, range) : null, fetcher, { refetchMs: range === null ? 0 : refetchIntervalFor(range) });
}
