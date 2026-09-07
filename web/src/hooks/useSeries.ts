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

/** The request in flight for each key, so a second asker joins it instead of starting another. */
const inFlight = new Map<string, Promise<Series>>();

/** Forgets any request still in flight. Nothing is cached, so this only ever costs an extra fetch. */
export function resetSharedSeries(): void {
  inFlight.clear();
}

/**
 * The Series for (network, range), sharing one request with whoever else is
 * asking for the same one: the hero's chart and the history section mount and
 * refetch together, so two views on one range must not become two requests to
 * the api. Nothing is held after the request settles, so the refetch interval
 * still decides when data is refreshed and no view is ever handed a stale
 * range. No caller's abort reaches the request: one view walking away must not
 * cancel the fetch the other is waiting on.
 */
export function fetchSharedSeries(network: string, range: SeriesRange): Promise<Series> {
  const key = seriesKey(network, range);
  const existing = inFlight.get(key);
  if (existing) return existing;
  const promise = getSeries(network, range);
  inFlight.set(key, promise);
  const done = () => {
    if (inFlight.get(key) === promise) inFlight.delete(key);
  };
  promise.then(done, done);
  return promise;
}

/** The bucketed history of a range. A null network or range fetches nothing and reads as empty. */
export function useSeries(network: string | null, range: SeriesRange | null): ApiState<Series> {
  const fetcher = useCallback(() => fetchSharedSeries(network ?? "", range ?? "1h"), [network, range]);
  return useApi<Series>(network !== null && range !== null ? seriesKey(network, range) : null, fetcher, { refetchMs: range === null ? 0 : refetchIntervalFor(range) });
}
