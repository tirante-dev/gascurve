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

export function useSeries(network: string | null, range: SeriesRange): ApiState<Series> {
  const fetcher = useCallback((signal: AbortSignal) => getSeries(network ?? "", range, { signal }), [network, range]);
  return useApi<Series>(network ? `${network}:${range}` : null, fetcher, { refetchMs: refetchIntervalFor(range) });
}
