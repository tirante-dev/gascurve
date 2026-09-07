import type { L1Series, SeriesRange } from "@/types";
import { request, type RequestOptions } from "./core";
import { orderPoints } from "./order";

export function getL1(network: string, range: SeriesRange, options: RequestOptions = {}): Promise<L1Series> {
  return request<L1Series>(`/networks/${encodeURIComponent(network)}/l1`, {
    ...options,
    query: { ...options.query, range },
  }).then(orderPoints);
}
