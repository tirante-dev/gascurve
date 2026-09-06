"use client";

import { useParams, useRouter } from "next/navigation";
import { useCallback, useEffect } from "react";

export const DEFAULT_NETWORK = "robinhood";
export const NETWORK_STORAGE_KEY = "gascurve:network";

const NAME_PATTERN = /^[a-z0-9-]{1,64}$/;

export function isValidNetworkName(value: unknown): value is string {
  return typeof value === "string" && NAME_PATTERN.test(value);
}

export function readStoredNetwork(): string | null {
  try {
    const value = window.localStorage.getItem(NETWORK_STORAGE_KEY);
    return isValidNetworkName(value) ? value : null;
  } catch {
    return null;
  }
}

export function storeNetwork(name: string): void {
  try {
    window.localStorage.setItem(NETWORK_STORAGE_KEY, name);
  } catch {
    // Private mode or blocked storage: the choice simply is not remembered.
  }
}

/** The network from the route, persisted as the last choice. */
export function useNetwork(): { network: string; setNetwork: (name: string) => void } {
  const params = useParams<{ network?: string | string[] }>();
  const router = useRouter();
  const raw = Array.isArray(params?.network) ? params.network[0] : params?.network;
  const network = isValidNetworkName(raw) ? raw : DEFAULT_NETWORK;

  useEffect(() => {
    storeNetwork(network);
  }, [network]);

  const setNetwork = useCallback(
    (name: string) => {
      if (!isValidNetworkName(name) || name === network) return;
      storeNetwork(name);
      router.push(`/${encodeURIComponent(name)}`);
    },
    [network, router],
  );

  return { network, setNetwork };
}
