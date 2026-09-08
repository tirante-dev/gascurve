// Fetch wrapper for the gascurve api: base URL from the environment, a 10 s
// timeout per attempt, and up to two retries on 5xx or network errors with
// exponential backoff. Every endpoint module goes through `request`.

import type { ApiErrorBody } from "@/types";

const DEFAULT_API_URL = "http://localhost:8080/api/v1";

export const API_BASE_URL = (process.env.NEXT_PUBLIC_API_URL ?? DEFAULT_API_URL).replace(/\/+$/, "");

export const DEFAULT_TIMEOUT_MS = 10_000;
export const MAX_RETRIES = 2;
export const RETRY_BASE_DELAY_MS = 500;

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;

  constructor(status: number, code: string, message: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
  }

  get retryable(): boolean {
    return this.status >= 500 || this.status === 0;
  }
}

export type QueryValue = string | number | boolean | undefined;

export type RequestOptions = {
  query?: Record<string, QueryValue>;
  /** The api base to fetch against, for a caller that cannot use the configured one. See serverApiBase. */
  baseUrl?: string;
  signal?: AbortSignal;
  timeoutMs?: number;
  retries?: number;
  fetchImpl?: typeof fetch;
};

function isApiErrorBody(value: unknown): value is ApiErrorBody {
  if (typeof value !== "object" || value === null) return false;
  const error = (value as { error?: unknown }).error;
  if (typeof error !== "object" || error === null) return false;
  const { code, message } = error as { code?: unknown; message?: unknown };
  return typeof code === "string" && typeof message === "string";
}

/**
 * True when the configured base is a path on the page's own origin rather
 * than an absolute URL. The published image bakes NEXT_PUBLIC_API_URL as
 * /api/v1, because the api and the app are served from one hostname behind
 * the tunnel, and a path is not something `new URL` can parse on its own.
 */
export function isRelativeBase(base: string = API_BASE_URL): boolean {
  return !/^[a-z][a-z0-9+.-]*:/i.test(base);
}

/**
 * The api base a fetch made on the server can use. A page-relative base is resolved against the document
 * by a browser and against nothing at all by `fetch` in Node, so a server render joins it onto `origin`:
 * the request goes out through the ingress and back. GASCURVE_SERVER_API_URL names a shorter route the
 * server can take instead. See "The live card" in docs/ARCHITECTURE.md.
 */
export function serverApiBase(origin: string, base: string = API_BASE_URL, override = process.env.GASCURVE_SERVER_API_URL): string {
  const direct = absoluteHttpBase(override);
  if (direct !== null) return direct;
  return isRelativeBase(base) ? origin.replace(/\/+$/, "") + base : base;
}

/**
 * `value` as an absolute http(s) base with no trailing slash, or null for anything this path could not
 * use. An override that is not one is dropped rather than obeyed: the public route it falls back to works
 * everywhere. Rejected along with a relative path and a foreign scheme: credentials, which `fetch` itself
 * refuses, and a query or fragment, which buildUrl would append the endpoint inside of.
 */
function absoluteHttpBase(value: string | undefined): string | null {
  const text = value?.trim();
  if (text === undefined || text === "") return null;
  let url: URL;
  try {
    url = new URL(text);
  } catch {
    return null;
  }
  if (url.protocol !== "http:" && url.protocol !== "https:") return null;
  if (url.username !== "" || url.password !== "" || url.search !== "" || url.hash !== "") return null;
  // Rebuilt from the parsed URL rather than handed back as written: a bare "?" or "#" leaves search and
  // hash empty but survives in the text, and buildUrl would then append the endpoint after it.
  return (url.origin + url.pathname).replace(/\/+$/, "");
}

/**
 * The absolute or origin-relative URL of an api path, with the query
 * appended. Built by string rather than through `new URL`, which throws on
 * a relative base; `fetch` resolves a leading-slash URL against the
 * document, which is exactly what a same-origin deployment wants.
 */
export function buildUrl(path: string, query?: Record<string, QueryValue>, base: string = API_BASE_URL): string {
  const url = base + (path.startsWith("/") ? path : `/${path}`);
  const params = new URLSearchParams();
  if (query) {
    for (const [key, value] of Object.entries(query)) {
      if (value !== undefined) params.set(key, String(value));
    }
  }
  const search = params.toString();
  return search === "" ? url : `${url}?${search}`;
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function attempt<T>(url: string, options: RequestOptions): Promise<T> {
  const fetchImpl = options.fetchImpl ?? fetch;
  const controller = new AbortController();
  const timeoutMs = options.timeoutMs ?? DEFAULT_TIMEOUT_MS;
  const timer = setTimeout(() => controller.abort(), timeoutMs);
  const onOuterAbort = () => controller.abort();
  options.signal?.addEventListener("abort", onOuterAbort);
  try {
    let response: Response;
    try {
      response = await fetchImpl(url, {
        signal: controller.signal,
        headers: { Accept: "application/json" },
      });
    } catch (err) {
      if (options.signal?.aborted) throw err;
      const message = err instanceof Error ? err.message : "network error";
      throw new ApiError(0, "network_error", message);
    }
    if (!response.ok) {
      let code = `http_${response.status}`;
      let message = response.statusText || `HTTP ${response.status}`;
      try {
        const body: unknown = await response.json();
        if (isApiErrorBody(body)) {
          code = body.error.code;
          message = body.error.message;
        }
      } catch {
        // Non-JSON error bodies keep the HTTP status text.
      }
      throw new ApiError(response.status, code, message);
    }
    return (await response.json()) as T;
  } finally {
    clearTimeout(timer);
    options.signal?.removeEventListener("abort", onOuterAbort);
  }
}

/** Fetch `path` under the api base URL and decode JSON, retrying transient failures. */
export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  if (process.env.NEXT_PUBLIC_USE_MOCK_DATA === "true") {
    const { mockRequest } = await import("@/lib/mock");
    return mockRequest<T>(path, options.query);
  }
  const url = buildUrl(path, options.query, options.baseUrl ?? API_BASE_URL);
  const retries = options.retries ?? MAX_RETRIES;
  let lastError: unknown;
  for (let i = 0; i <= retries; i++) {
    try {
      return await attempt<T>(url, options);
    } catch (err) {
      lastError = err;
      if (options.signal?.aborted) throw err;
      const retryable = err instanceof ApiError && err.retryable;
      if (!retryable || i === retries) throw err;
      await sleep(RETRY_BASE_DELAY_MS * 2 ** i);
    }
  }
  throw lastError;
}
