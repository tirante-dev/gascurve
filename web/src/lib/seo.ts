// Site level metadata: the canonical origin, the copy the crawlers and social cards read, and the
// network list the sitemap and index page fall back to. Pure, so it is testable without a DOM or api.

import type { Metadata } from "next";
import { networkLabel } from "@/utils/network";

export const SITE_NAME = "gascurve";

/** The origin every canonical URL, the sitemap and the social card images resolve against. The published
 * image bakes this at build time; the fallback matches, so forgetting the variable still emits absolute
 * URLs rather than localhost ones. */
export const DEFAULT_SITE_URL = "https://gascurve.com";

/**
 * `value` as a bare origin, or null when it is not an absolute http(s) URL. Any path is dropped: Next
 * joins metadataBase's pathname onto every relative metadata URL, so an origin carrying a path would
 * emit canonicals under it while the app, which sets no basePath, still serves them at the root.
 */
export function normalizeSiteUrl(value: string | undefined): string | null {
  if (value === undefined || value.trim() === "") return null;
  let url: URL;
  try {
    url = new URL(value.trim());
  } catch {
    return null;
  }
  if (url.protocol !== "http:" && url.protocol !== "https:") return null;
  return url.origin;
}

export const SITE_URL = normalizeSiteUrl(process.env.NEXT_PUBLIC_SITE_URL) ?? DEFAULT_SITE_URL;

export function absoluteUrl(path: string, base: string = SITE_URL): string {
  return `${base}${path.startsWith("/") ? path : `/${path}`}`;
}

/** The chain the site is about; everything else is there for comparison, which orders the index page. */
export const PRIMARY_NETWORK = "robinhood";
export const PRIMARY_NETWORK_NAME = "Robinhood Chain";

export type SiteNetwork = { name: string; displayName: string; primary: boolean };

/** The networks the published site serves, mirroring config.yaml. The api is the authority at runtime, but
 * the sitemap and server rendered titles are built where it cannot be fetched, so add a network here too. */
export const SITE_NETWORKS: readonly SiteNetwork[] = [
  { name: PRIMARY_NETWORK, displayName: PRIMARY_NETWORK_NAME, primary: true },
  { name: "robinhood-testnet", displayName: "Robinhood Chain Testnet", primary: false },
  { name: "arbitrum-one", displayName: "Arbitrum One", primary: false },
  { name: "arbitrum-sepolia", displayName: "Arbitrum Sepolia", primary: false },
];

/** What to call a network in server rendered copy: config.yaml's name, or the route parameter title cased. */
export function networkDisplayName(param: string): string {
  return SITE_NETWORKS.find((n) => n.name === param)?.displayName ?? networkLabel(param);
}

export const SITE_TAGLINE = "Nitro base fee telemetry";

export const SITE_DESCRIPTION = `Live and historical gas prices for ${PRIMARY_NETWORK_NAME}: Nitro base fees, constraint backlogs, owner changes, fee destinations and ArbOS-attributed batch costs.`;

export const SITE_TITLE = `${PRIMARY_NETWORK_NAME} gas tracker · ${SITE_NAME}`;

/** The social card every page shares, named here rather than dropped in as an opengraph-image file: a page
 * that sets its own openGraph replaces the whole object, images included. */
export const CARD_IMAGE = {
  url: "/og-card.png",
  width: 1200,
  height: 630,
  alt: `The ${SITE_NAME} wordmark over a rising base fee curve, above the line "Live and historical gas prices for ${PRIMARY_NETWORK_NAME}".`,
} as const;

/** The metadata a network scoped page carries. Every route under /[network] builds it here, so they agree. */
export function pageMetadata({ title, description, path, canonical }: { title: string; description: string; path: string; canonical: boolean }): Metadata {
  return {
    title,
    description,
    ...(canonical ? { alternates: { canonical: path } } : { robots: { index: false, follow: true } }),
    openGraph: { type: "website", siteName: SITE_NAME, url: absoluteUrl(path), title, description, images: [CARD_IMAGE] },
    twitter: { card: "summary_large_image", title, description, images: [CARD_IMAGE] },
  };
}
