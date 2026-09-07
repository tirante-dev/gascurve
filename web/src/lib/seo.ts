// Site level metadata: the canonical origin, the copy the crawlers and the
// social cards read, and the network list the sitemap and the index page fall
// back to. Kept pure so the titles, the descriptions and the absolute URLs are
// testable without a DOM or a running api.

import type { Metadata } from "next";
import { networkLabel } from "@/utils/network";

export const SITE_NAME = "gascurve";

/**
 * The origin the deployment is served from, which every canonical URL, the
 * sitemap and the social card images are resolved against. The published
 * image bakes this at build time (Dockerfile.web, and SITE_URL in
 * .github/workflows/docker-publish.yml); the fallback is the same value those
 * default to, so a deployment that forgets the variable still emits correct
 * absolute URLs rather than localhost ones.
 */
export const DEFAULT_SITE_URL = "https://gascurve.com";

/**
 * `value` as a bare origin, or null when it is not an absolute http(s) URL.
 *
 * Any path is dropped rather than kept. Next joins `metadataBase`'s pathname
 * onto every relative metadata URL, so an origin of `https://example.com/gas`
 * would emit canonicals and sitemap entries under `/gas/...` while the app,
 * which sets no `basePath`, still serves those routes at the origin root: the
 * metadata would point search engines at URLs that 404. Serving under a
 * sub-path is a `basePath` change in next.config.ts first, and this should
 * read that rather than a path smuggled in through the origin.
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

/** The absolute URL of a site-relative path, for a canonical link or a card image. */
export function absoluteUrl(path: string, base: string = SITE_URL): string {
  return `${base}${path.startsWith("/") ? path : `/${path}`}`;
}

/**
 * The chain the site is about. Everything else it serves is there for
 * comparison, which is what orders the index page, weights the sitemap and
 * decides whose name the default title and the social card carry.
 */
export const PRIMARY_NETWORK = "robinhood";
export const PRIMARY_NETWORK_NAME = "Robinhood Chain";

export type SiteNetwork = { name: string; displayName: string; primary: boolean };

/**
 * The networks the published site serves, mirroring the `networks` block of
 * config.yaml. The api is the authority at runtime, but the sitemap, the
 * server rendered titles and the index page's crawlable fallback are all
 * built on the server, where the api base URL is a path on the site's own
 * origin (NEXT_PUBLIC_API_URL is /api/v1 in the published image) and so
 * cannot be fetched. Add a network here when one is added to config.yaml.
 */
export const SITE_NETWORKS: readonly SiteNetwork[] = [
  { name: PRIMARY_NETWORK, displayName: PRIMARY_NETWORK_NAME, primary: true },
  { name: "robinhood-testnet", displayName: "Robinhood Chain Testnet", primary: false },
  { name: "arbitrum-one", displayName: "Arbitrum One", primary: false },
  { name: "arbitrum-sepolia", displayName: "Arbitrum Sepolia", primary: false },
];

/**
 * What to call a network in server rendered copy: the name config.yaml gives
 * it when the site knows it, and the route parameter title cased when it does
 * not, so an api that grows a network still reads correctly.
 */
export function networkDisplayName(param: string): string {
  return SITE_NETWORKS.find((n) => n.name === param)?.displayName ?? networkLabel(param);
}

export const SITE_TAGLINE = "Nitro base fee telemetry";

/**
 * What the site is, in one sentence, for the default description and the
 * social cards. It leads with the chain the site is for, and stays inside the
 * length a search result shows without truncating the useful half.
 */
export const SITE_DESCRIPTION = `Live and historical gas prices for ${PRIMARY_NETWORK_NAME}: the Nitro base fee pricer, its constraint backlogs, owner changes, fee destinations and L1 posting costs.`;

/** The default title, which is also the one the homepage wears. */
export const SITE_TITLE = `${PRIMARY_NETWORK_NAME} gas tracker · ${SITE_NAME}`;

/**
 * The social card every page shares. It is served from public/ and named here
 * rather than dropped in as an opengraph-image file, because a page that sets
 * its own `openGraph` replaces the whole object, images included: a card that
 * came from the file convention would be present on the index and missing on
 * every network page, which are the ones people actually share. Naming it in
 * one place and spreading it into both objects keeps it on all of them.
 */
export const CARD_IMAGE = {
  url: "/og-card.png",
  width: 1200,
  height: 630,
  alt: `The ${SITE_NAME} wordmark over a rising base fee curve, above the line "Live and historical gas prices for ${PRIMARY_NETWORK_NAME}".`,
} as const;

/**
 * The metadata a network scoped page carries: its own title and description,
 * the social card, and either a canonical link or, for the duplicate a chain
 * id route serves, a request not to index it. Every route under /[network]
 * builds its metadata through here so the three of them stay in step.
 */
export function pageMetadata({ title, description, path, canonical }: { title: string; description: string; path: string; canonical: boolean }): Metadata {
  return {
    title,
    description,
    ...(canonical ? { alternates: { canonical: path } } : { robots: { index: false, follow: true } }),
    openGraph: { type: "website", siteName: SITE_NAME, url: absoluteUrl(path), title, description, images: [CARD_IMAGE] },
    twitter: { card: "summary_large_image", title, description, images: [CARD_IMAGE] },
  };
}
