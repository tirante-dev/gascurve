import type { BlockPoint, LiveSnapshot } from "@/types";
import { request, type RequestOptions } from "./core";

export function getLive(network: string, options?: RequestOptions): Promise<LiveSnapshot> {
  return request<LiveSnapshot>(`/networks/${encodeURIComponent(network)}/live`, options);
}

export function getBlocks(network: string, limit = 120, options: RequestOptions = {}): Promise<BlockPoint[]> {
  return request<BlockPoint[]>(`/networks/${encodeURIComponent(network)}/blocks`, {
    ...options,
    query: { ...options.query, limit },
  });
}
