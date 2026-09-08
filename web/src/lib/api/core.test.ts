import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { API_BASE_URL, ApiError, buildUrl, request, serverApiBase } from "./core";

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
}

describe("a relative api base", () => {
  // The published image bakes NEXT_PUBLIC_API_URL=/api/v1, because the api
  // and the app share a hostname behind the tunnel. new URL() cannot parse
  // a path on its own, which took every request down with "Failed to
  // construct 'URL': Invalid URL".
  it("builds a path the document resolves, rather than throwing", async () => {
    vi.resetModules();
    vi.stubEnv("NEXT_PUBLIC_API_URL", "/api/v1");
    const core = await import("./core");
    expect(core.API_BASE_URL).toBe("/api/v1");
    expect(core.isRelativeBase()).toBe(true);
    expect(core.buildUrl("/networks")).toBe("/api/v1/networks");
    expect(core.buildUrl("networks/robinhood/series", { range: "24h" })).toBe("/api/v1/networks/robinhood/series?range=24h");
    vi.unstubAllEnvs();
    vi.resetModules();
  });
  it("knows an absolute base from a relative one", async () => {
    const { isRelativeBase } = await import("./core");
    expect(isRelativeBase("http://localhost:8080/api/v1")).toBe(false);
    expect(isRelativeBase("https://gascurve.com/api/v1")).toBe(false);
    expect(isRelativeBase("/api/v1")).toBe(true);
    expect(isRelativeBase("")).toBe(true);
  });
});

describe("buildUrl", () => {
  it("joins the base and drops undefined query values", () => {
    expect(API_BASE_URL).toBe("http://localhost:8080/api/v1");
    expect(buildUrl("/networks")).toBe("http://localhost:8080/api/v1/networks");
    expect(buildUrl("networks/robinhood/series", { range: "1h", step: undefined, limit: 5 })).toBe(
      "http://localhost:8080/api/v1/networks/robinhood/series?range=1h&limit=5",
    );
  });

  it("takes a base of its own, for a caller that cannot use the configured one", () => {
    expect(buildUrl("/networks", undefined, "https://gascurve.com/api/v1")).toBe("https://gascurve.com/api/v1/networks");
  });
});

describe("serverApiBase", () => {
  // A card or any other server render fetches without a document to resolve
  // against, so a page-relative base has to be joined onto the site's origin.
  it("joins a relative base onto the origin and leaves an absolute one alone", () => {
    expect(serverApiBase("https://gascurve.com", "/api/v1")).toBe("https://gascurve.com/api/v1");
    expect(serverApiBase("https://gascurve.com/", "/api/v1")).toBe("https://gascurve.com/api/v1");
    expect(serverApiBase("https://gascurve.com", "http://api:8080/api/v1")).toBe("http://api:8080/api/v1");
  });

  it("prefers an address the server was told it can reach directly", () => {
    expect(serverApiBase("https://gascurve.com", "/api/v1", "http://gascurve-api:8080/api/v1/")).toBe("http://gascurve-api:8080/api/v1");
    for (const unset of [undefined, "", "  "]) {
      expect(serverApiBase("https://gascurve.com", "/api/v1", unset)).toBe("https://gascurve.com/api/v1");
    }
  });
});

describe("request", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllEnvs();
  });

  it("decodes JSON on success", async () => {
    const fetchImpl = vi.fn(async () => jsonResponse([{ name: "robinhood" }]));
    const result = await request<{ name: string }[]>("/networks", { fetchImpl });
    expect(result).toEqual([{ name: "robinhood" }]);
    expect(fetchImpl).toHaveBeenCalledTimes(1);
    const [url, init] = fetchImpl.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe("http://localhost:8080/api/v1/networks");
    expect(init.headers).toEqual({ Accept: "application/json" });
  });

  it("fetches against the base the caller names", async () => {
    const fetchImpl = vi.fn(async () => jsonResponse({}));
    await request("/networks/robinhood/live", { fetchImpl, baseUrl: "https://gascurve.com/api/v1" });
    const [url] = fetchImpl.mock.calls[0] as unknown as [string];
    expect(url).toBe("https://gascurve.com/api/v1/networks/robinhood/live");
  });

  it("throws a typed ApiError on 4xx without retrying", async () => {
    const fetchImpl = vi.fn(async () =>
      jsonResponse({ error: { code: "not_found", message: "no such network" } }, 404),
    );
    const promise = request("/networks/nope", { fetchImpl });
    await expect(promise).rejects.toMatchObject({ name: "ApiError", status: 404, code: "not_found", message: "no such network" });
    expect(fetchImpl).toHaveBeenCalledTimes(1);
  });

  it("falls back to the status text when the error body is not JSON", async () => {
    const fetchImpl = vi.fn(async () => new Response("nope", { status: 400, statusText: "Bad Request" }));
    await expect(request("/x", { fetchImpl })).rejects.toMatchObject({ status: 400, code: "http_400", message: "Bad Request" });
  });

  it("retries 5xx twice with exponential backoff, then gives up", async () => {
    const fetchImpl = vi.fn(async () => jsonResponse({ error: { code: "db", message: "down" } }, 503));
    const promise = request("/networks", { fetchImpl });
    const failure = expect(promise).rejects.toBeInstanceOf(ApiError);
    await vi.advanceTimersByTimeAsync(500);
    expect(fetchImpl).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(1000);
    expect(fetchImpl).toHaveBeenCalledTimes(3);
    await failure;
    expect(fetchImpl).toHaveBeenCalledTimes(3);
  });

  it("retries network errors and succeeds", async () => {
    const fetchImpl = vi
      .fn<typeof fetch>()
      .mockRejectedValueOnce(new TypeError("Failed to fetch"))
      .mockResolvedValueOnce(jsonResponse({ ok: true }));
    const promise = request<{ ok: boolean }>("/networks", { fetchImpl });
    await vi.advanceTimersByTimeAsync(500);
    await expect(promise).resolves.toEqual({ ok: true });
    expect(fetchImpl).toHaveBeenCalledTimes(2);
  });

  it("wraps non-Error rejections", async () => {
    const fetchImpl = vi.fn<typeof fetch>().mockRejectedValue("boom");
    await expect(request("/x", { fetchImpl, retries: 0 })).rejects.toMatchObject({ code: "network_error", message: "network error" });
  });

  it("aborts after the timeout and retries", async () => {
    const fetchImpl = vi.fn<typeof fetch>((_url, init) => {
      return new Promise<Response>((_resolve, reject) => {
        init?.signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")));
      });
    });
    const promise = request("/slow", { fetchImpl, timeoutMs: 100, retries: 1 });
    const failure = expect(promise).rejects.toMatchObject({ code: "network_error" });
    await vi.advanceTimersByTimeAsync(100);
    await vi.advanceTimersByTimeAsync(500);
    await vi.advanceTimersByTimeAsync(100);
    await failure;
    expect(fetchImpl).toHaveBeenCalledTimes(2);
  });

  it("does not retry when the caller aborted", async () => {
    const controller = new AbortController();
    const fetchImpl = vi.fn<typeof fetch>((_url, init) => {
      return new Promise<Response>((_resolve, reject) => {
        init?.signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")));
      });
    });
    const promise = request("/slow", { fetchImpl, signal: controller.signal });
    const failure = expect(promise).rejects.toMatchObject({ name: "AbortError" });
    controller.abort();
    await failure;
    expect(fetchImpl).toHaveBeenCalledTimes(1);
  });

  it("routes to the mock when NEXT_PUBLIC_USE_MOCK_DATA is true", async () => {
    vi.stubEnv("NEXT_PUBLIC_USE_MOCK_DATA", "true");
    const fetchImpl = vi.fn<typeof fetch>();
    const networks = await request<{ name: string }[]>("/networks", { fetchImpl });
    expect(fetchImpl).not.toHaveBeenCalled();
    expect(networks.map((n) => n.name)).toContain("robinhood");
  });
});

describe("ApiError", () => {
  it("marks 5xx and network errors as retryable", () => {
    expect(new ApiError(500, "x", "y").retryable).toBe(true);
    expect(new ApiError(0, "x", "y").retryable).toBe(true);
    expect(new ApiError(404, "x", "y").retryable).toBe(false);
  });
});
