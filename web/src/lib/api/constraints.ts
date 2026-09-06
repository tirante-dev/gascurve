import type { ConstraintsResponse, OwnerAction } from "@/types";
import { request, type RequestOptions } from "./core";

export function getConstraints(network: string, options?: RequestOptions): Promise<ConstraintsResponse> {
  return request<ConstraintsResponse>(`/networks/${encodeURIComponent(network)}/constraints`, options);
}

export function getOwnerActions(network: string, options?: RequestOptions): Promise<OwnerAction[]> {
  return request<OwnerAction[]>(`/networks/${encodeURIComponent(network)}/owner-actions`, options);
}
