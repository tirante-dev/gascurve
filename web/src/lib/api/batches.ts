import type { BatchSeries, SeriesRange } from "@/types";
import { request, type RequestOptions } from "./core";

export function getBatches(network: string, range: SeriesRange, options: RequestOptions = {}): Promise<BatchSeries> {
  return request<BatchSeries>(`/networks/${encodeURIComponent(network)}/batches`, {
    ...options,
    query: { ...options.query, range },
  });
}
