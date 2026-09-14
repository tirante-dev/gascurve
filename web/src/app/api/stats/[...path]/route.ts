/**
 * First party proxy to the self hosted Umami instance, so the tracker script and the pageviews it reports
 * both travel over this app's own origin:
 *
 *   GET  /api/stats/script.js  ->  {UMAMI_URL}/script.js
 *   POST /api/stats/api/send   ->  {UMAMI_URL}/api/send
 */

// Only those two paths are relayed. The upstream base is operator set, but an unrestricted path would
// still make this an open proxy into whatever the pod's network can reach, so anything else 404s.
// An unset UMAMI_URL disables analytics: the script 404s at the source and the tracker never initializes.

// Read per request so a redeploy with new env is reflected, and so a beacon is never served from a cache.
export const dynamic = "force-dynamic";

const UPSTREAM_TIMEOUT_MS = 10_000;

/** Relative to UMAMI_URL, matched exactly against the joined route path. */
const SCRIPT_PATH = "script.js";
const COLLECT_PATH = "api/send";

/** Long enough that a repeat visitor does not refetch the tracker, short enough that an Umami upgrade
 * reaches visitors the same day. */
const SCRIPT_CACHE_CONTROL = "public, max-age=3600, stale-while-revalidate=86400";

/**
 * Request headers that are not forwarded. Everything else is relayed, because the tracker carries its own
 * x-umami-* headers and Umami derives the browser, device and language from user-agent and
 * accept-language, so a whitelist would silently degrade the data on any tracker update.
 *
 * cookie is dropped deliberately: the collection endpoint has no use for this app's cookies, and
 * forwarding them would hand session state to a service that should only ever see anonymous pageviews.
 * The rest are hop by hop, or are recomputed by fetch for the new request.
 */
const BLOCKED_REQUEST_HEADERS = new Set([
  "cookie",
  "host",
  "connection",
  "content-length",
  "accept-encoding",
  "keep-alive",
  "transfer-encoding",
  "upgrade",
  "te",
  "trailer",
  "proxy-authorization",
  "proxy-connection",
]);

function upstreamBase(): string {
  return (process.env.UMAMI_URL ?? "").replace(/\/+$/, "");
}

function forwardedHeaders(request: Request): Headers {
  const headers = new Headers();
  request.headers.forEach((value, key) => {
    if (!BLOCKED_REQUEST_HEADERS.has(key.toLowerCase())) headers.set(key, value);
  });
  return headers;
}

/** Relay one request upstream with a timeout, cancelling it if the visitor navigates away first. */
async function relay(request: Request, upstreamUrl: string, body: BodyInit | null): Promise<Response | null> {
  const controller = new AbortController();
  const timeoutId = setTimeout(() => controller.abort(), UPSTREAM_TIMEOUT_MS);
  const abortUpstream = () => controller.abort();
  request.signal.addEventListener("abort", abortUpstream);

  try {
    return await fetch(upstreamUrl, {
      method: request.method,
      headers: forwardedHeaders(request),
      body,
      signal: controller.signal,
      // Kept out of Next's fetch cache: a shared response would attribute every visitor's pageview to
      // whoever warmed it.
      cache: "no-store",
    });
  } catch {
    return null;
  } finally {
    clearTimeout(timeoutId);
    request.signal.removeEventListener("abort", abortUpstream);
  }
}

async function resolvePath(params: Promise<{ path: string[] }>): Promise<string> {
  const { path } = await params;
  return (path ?? []).join("/");
}

export async function GET(request: Request, { params }: { params: Promise<{ path: string[] }> }): Promise<Response> {
  const base = upstreamBase();
  if (base === "" || (await resolvePath(params)) !== SCRIPT_PATH) return new Response(null, { status: 404 });

  const upstream = await relay(request, `${base}/${SCRIPT_PATH}`, null);
  if (upstream === null || !upstream.ok) {
    upstream?.body?.cancel();
    return new Response(null, { status: 502 });
  }

  return new Response(await upstream.text(), {
    status: 200,
    headers: {
      "Content-Type": upstream.headers.get("Content-Type") ?? "text/javascript; charset=utf-8",
      "Cache-Control": SCRIPT_CACHE_CONTROL,
    },
  });
}

export async function POST(request: Request, { params }: { params: Promise<{ path: string[] }> }): Promise<Response> {
  const base = upstreamBase();
  if (base === "" || (await resolvePath(params)) !== COLLECT_PATH) return new Response(null, { status: 404 });

  // Buffered rather than streamed: the payload is a few hundred bytes, and a stream body would need duplex
  // support that not every runtime offers.
  const upstream = await relay(request, `${base}/${COLLECT_PATH}`, await request.text());
  // The tracker ignores the response, so an unreachable instance costs the visitor nothing but a status.
  if (upstream === null) return new Response(null, { status: 502 });

  const payload = await upstream.text();
  return new Response(payload === "" ? null : payload, {
    status: upstream.status,
    headers: {
      "Content-Type": upstream.headers.get("Content-Type") ?? "text/plain; charset=utf-8",
      "Cache-Control": "no-store",
    },
  });
}
