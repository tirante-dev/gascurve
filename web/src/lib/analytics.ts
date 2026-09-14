// Client side interface to the self hosted Umami instance. The tracker script and the endpoint it
// reports to are both served from this app's own origin by app/api/stats/[...path]/route.ts, so no
// analytics hostname appears in the page and a filter list keyed on one does not block collection.

/** Where the proxy serves the tracker from, and what the tracker reports back to. */
export const ANALYTICS_PROXY_PATH = "/api/stats";

export const ANALYTICS_SCRIPT_PATH = `${ANALYTICS_PROXY_PATH}/script.js`;

/** The tracker appends its collection path to data-host-url verbatim, so a root relative value resolves
 * against whichever origin served the page: one build reports correctly from production, a preview and
 * localhost alike, with no origin in the bundle and no CORS preflight. */
export const ANALYTICS_HOST_URL = ANALYTICS_PROXY_PATH;

/** Baked in by Dockerfile.web. Next inlines NEXT_PUBLIC_ values at build time, so an id supplied only at
 * run time never reaches the browser. Empty is the off switch: no tracker is loaded at all. */
export const ANALYTICS_WEBSITE_ID = process.env.NEXT_PUBLIC_UMAMI_WEBSITE_ID ?? "";

/** Custom events and the properties each carries. Umami turns properties into filter facets rather than
 * log lines, so keep the values scalar and low cardinality. An empty `previous` is a page that was still
 * on its default, since these two read the outgoing value off the URL. */
interface AnalyticsEvents {
  /** A visitor changed chain from the header's network picker. */
  "network-switch": { from: string; to: string };
  /** A visitor changed a chart page's range. */
  "range-change": { chart: string; range: string; previous: string };
  /** A visitor changed a chart page's constraint slot. */
  "constraint-change": { chart: string; constraint: string; previous: string };
}

export type AnalyticsEventName = keyof AnalyticsEvents;

type UmamiTracker = { track: (name: string, data?: Record<string, unknown>) => void };

/** The part of the tracker's payload the hook below reads. `name` is absent on a pageview. */
interface TrackerPayload {
  url?: string;
  name?: string;
}

/** Name of the global the tracker calls before every beacon, passed to it as data-before-send. The literal
 * type keeps it from drifting from the Window property declared below. */
export const ANALYTICS_BEFORE_SEND = "gascurveBeforeSend" as const;

declare global {
  interface Window {
    umami?: UmamiTracker;
    gascurveBeforeSend?: (type: string, payload: TrackerPayload) => TrackerPayload | undefined;
  }
}

/** Query parameters that hold a chart's state rather than naming a page. */
const STATE_PARAMS = ["range", "constraint"] as const;

let lastPageviewKey: string | null = null;

/** Path and query of a pageview URL, minus the state parameters. The tracker reports `url` as a path, not
 * an absolute URL, so parsing needs a base; an absolute one still parses as itself. */
function pageviewKey(url: string): string | null {
  let parsed: URL;
  try {
    parsed = new URL(url, "http://gascurve.invalid");
  } catch {
    return null;
  }
  for (const param of STATE_PARAMS) parsed.searchParams.delete(param);
  return `${parsed.pathname}${parsed.search}`;
}

/**
 * Drops the pageview that changing a chart's range or constraint would otherwise produce. Both live in the
 * query string so a view can be linked, and the tracker reports on any URL change, so the site's most
 * common interaction would inflate pageviews on exactly the pages that get used most. Each switch is still
 * recorded, as an event carrying both sides of it, which is the more useful shape anyway.
 *
 * Returning undefined cancels the beacon. Everything else passes through untouched: custom events, real
 * navigations, and every other query parameter, campaign tags included.
 */
export function beforeSend(type: string, payload: TrackerPayload): TrackerPayload | undefined {
  if (type !== "event" || payload?.name || typeof payload?.url !== "string") return payload;

  const key = pageviewKey(payload.url);
  if (key === null) return payload;
  if (key === lastPageviewKey) return undefined;

  lastPageviewKey = key;
  return payload;
}

/** Test seam: the hook's memory of the last page reported. */
export function resetPageviewState(): void {
  lastPageviewKey = null;
}

/** Report a custom event. Does nothing when no tracker is present, which covers server rendering, a build
 * with no website id, and a visitor whose browser blocked the script. */
export function trackEvent<K extends AnalyticsEventName>(name: K, data: AnalyticsEvents[K]): void {
  if (typeof window === "undefined") return;
  const tracker = window.umami;
  if (!tracker || typeof tracker.track !== "function") return;

  try {
    tracker.track(name, data);
  } catch {
    // A beacon that could not be sent must never break the interaction that triggered it.
  }
}

/** Hostname for the tracker's data-domains, or undefined to leave the attribute off. Restricting
 * collection to the canonical host keeps a local build from reporting into the site's stats. */
export function trackedDomain(siteUrl: string): string | undefined {
  let hostname: string;
  try {
    hostname = new URL(siteUrl).hostname;
  } catch {
    return undefined;
  }
  if (hostname === "" || hostname === "localhost" || hostname === "127.0.0.1") return undefined;
  return hostname;
}
