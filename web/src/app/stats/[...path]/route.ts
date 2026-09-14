/**
 * First party proxy to the self hosted Umami instance, relaying GET /stats/script.js and
 * POST /stats/api/send to {UMAMI_URL}. Not under /api, which belongs to the Go API in every deployment
 * that fronts both: a path there would 404 everywhere but `next start`. See docs/ARCHITECTURE.md.
 */

// Only those two paths are relayed. The upstream base is operator set, but an unrestricted path would
// still make this an open proxy into whatever the pod's network can reach, so anything else 404s.
// An unset UMAMI_URL disables analytics: the script 404s at the source and the tracker never initializes.

// Read per request so a redeploy with new env is reflected, and so a beacon is never served from a cache.
export const dynamic = "force-dynamic";

const UPSTREAM_TIMEOUT_MS = 10_000;

/** A tracker beacon is a few hundred bytes. This endpoint is public, so a body is read against a ceiling
 * rather than buffered whole: without one a client could make every replica hold an arbitrary body in
 * memory and forward it upstream. */
const MAX_BEACON_BYTES = 16 * 1024;

/** Relative to UMAMI_URL, matched exactly against the joined route path. */
const SCRIPT_PATH = "script.js";
const COLLECT_PATH = "api/send";

/** Long enough that a repeat visitor does not refetch the tracker, short enough that an Umami upgrade
 * reaches visitors the same day. */
const SCRIPT_CACHE_CONTROL = "public, max-age=3600, stale-while-revalidate=86400";

/**
 * Request headers that are not forwarded; everything else is relayed, because the tracker carries its own
 * x-umami-* headers and Umami derives browser, device and language from user-agent and accept-language,
 * so a whitelist would silently degrade the data on any tracker update. cookie is dropped deliberately:
 * the collection endpoint has no use for this app's cookies. The rest are hop by hop, or are recomputed
 * by fetch for the new request.
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

/** The request body as text, or null when it is longer than `limit`. A declared length over the ceiling is
 * refused outright; the stream is then counted as it is read, since a chunked body declares none. */
async function readBody(request: Request, limit: number): Promise<string | null> {
  const declared = Number(request.headers.get("content-length"));
  if (Number.isFinite(declared) && declared > limit) return null;

  const body = request.body;
  if (body === null) return "";

  const reader = body.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    size += value.byteLength;
    if (size > limit) {
      await reader.cancel();
      return null;
    }
    chunks.push(value);
  }

  const joined = new Uint8Array(size);
  let at = 0;
  for (const chunk of chunks) {
    joined.set(chunk, at);
    at += chunk.byteLength;
  }
  return new TextDecoder().decode(joined);
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

  // Relayed as text rather than as a stream: a stream body would need duplex support that not every
  // runtime offers, and the ceiling above is what makes buffering it safe.
  const body = await readBody(request, MAX_BEACON_BYTES);
  if (body === null) return new Response(null, { status: 413 });

  const upstream = await relay(request, `${base}/${COLLECT_PATH}`, body);
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
