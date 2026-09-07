// Route parameter helpers. The api accepts a network by name or by chain id
// (docs/ARCHITECTURE.md section 6), so the web app must treat `/4663` as
// Robinhood, not as an unknown network.

import type { Network } from "@/types";

/** The network a route parameter names, by name or by decimal chain id. */
export function findNetwork<T extends Pick<Network, "name" | "chainId">>(networks: readonly T[], param: string): T | undefined {
  return networks.find((n) => n.name === param || String(n.chainId) === param);
}

/** True when the api's network list is loaded and does not contain `param` under either form. */
export function isUnknownNetwork(networks: readonly Pick<Network, "name" | "chainId">[] | null, param: string): boolean {
  return networks !== null && findNetwork(networks, param) === undefined;
}

/**
 * The canonical route for `param` once the server has confirmed the network:
 * `/4663` resolves to `robinhood`. Null when the route already uses the name
 * or the confirmed network does not correspond to the parameter.
 */
export function canonicalNetworkName(param: string, confirmed: Pick<Network, "name" | "chainId"> | null): string | null {
  if (!confirmed || confirmed.name === param) return null;
  return String(confirmed.chainId) === param ? confirmed.name : null;
}

/** True when a route parameter names a network by chain id rather than by name. */
export function isChainIdParam(param: string): boolean {
  return /^\d+$/.test(param);
}

/**
 * A readable name for a route parameter, for page titles and headings that are
 * rendered on the server, before the api has confirmed the network and its
 * display name. Slugs become title case ("arbitrum-one" is "Arbitrum One");
 * a chain id route, which is a duplicate of the network's own route, is named
 * after the id rather than guessed at.
 */
export function networkLabel(param: string): string {
  if (isChainIdParam(param)) return `Chain ${param}`;
  return param
    .split("-")
    .filter((part) => part !== "")
    .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
    .join(" ");
}
