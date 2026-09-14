// Client side interface to the self hosted Umami instance. Both the tracker script and the endpoint it
// reports to are served from this app's own origin by app/api/stats/[...path]/route.ts. See
// docs/ARCHITECTURE.md, section 8, for why.

export const ANALYTICS_PROXY_PATH = "/api/stats";

export const ANALYTICS_SCRIPT_PATH = `${ANALYTICS_PROXY_PATH}/script.js`;

/** data-host-url. Root relative, so the tracker reports to whichever origin served the page and one build
 * works in production, in a preview and on localhost alike. */
export const ANALYTICS_HOST_URL = ANALYTICS_PROXY_PATH;

/** Baked in by Dockerfile.web, since Next inlines NEXT_PUBLIC_ values at build time. Empty is the off
 * switch: no tracker is loaded at all. */
export const ANALYTICS_WEBSITE_ID = process.env.NEXT_PUBLIC_UMAMI_WEBSITE_ID ?? "";

/** Custom events and their properties, which Umami turns into filter facets rather than log lines: keep
 * the values scalar and low cardinality. An empty `previous` is a page that was still on its default. */
interface AnalyticsEvents {
  "network-switch": { from: string; to: string };
  "range-change": { chart: string; range: string; previous: string };
  "constraint-change": { chart: string; constraint: string; previous: string };
}

export type AnalyticsEventName = keyof AnalyticsEvents;

type UmamiTracker = { track: (name: string, data?: Record<string, unknown>) => void };

/** The part of the tracker's payload the hook below reads. `name` is absent on a pageview. */
interface TrackerPayload {
  url?: string;
  name?: string;
}

/** Passed to the tracker as data-before-send. The literal type keeps it from drifting from the Window
 * property declared below. */
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

/** The tracker reports `url` as a path, not an absolute URL, so parsing it needs a base. An absolute one
 * still parses as itself. */
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
 * Drops the pageview that changing a chart's range or constraint would otherwise produce, since both live
 * in the query string and the tracker reports on any URL change. Each switch is recorded as an event
 * instead. Returning undefined cancels the beacon; everything else passes through untouched.
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

/** Hostname for data-domains, or undefined to leave the attribute off, which keeps a local build that was
 * given an id from reporting into the site's statistics. */
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
