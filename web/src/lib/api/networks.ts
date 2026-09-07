import type { Network, StatusResponse } from "@/types";
import { request, type RequestOptions } from "./core";

export function listNetworks(options?: RequestOptions): Promise<Network[]> {
  return request<Network[]>("/networks", options);
}

export function getNetwork(network: string, options?: RequestOptions): Promise<Network> {
  return request<Network>(`/networks/${encodeURIComponent(network)}`, options);
}

export function getStatus(options?: RequestOptions): Promise<StatusResponse> {
  return request<StatusResponse>("/status", options);
}
