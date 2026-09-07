import type { Series, SeriesRange } from "@/types";
import { request, type RequestOptions } from "./core";

export const SERIES_RANGES: readonly SeriesRange[] = ["1h", "24h", "30d", "all"];

export function isSeriesRange(value: string): value is SeriesRange {
  return (SERIES_RANGES as readonly string[]).includes(value);
}

export function getSeries(network: string, range: SeriesRange, options: RequestOptions = {}): Promise<Series> {
  return request<Series>(`/networks/${encodeURIComponent(network)}/series`, {
    ...options,
    query: { ...options.query, range },
  });
}
