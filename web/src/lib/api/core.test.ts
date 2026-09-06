import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { API_BASE_URL, ApiError, buildUrl, request } from "./core";

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
}

describe("buildUrl", () => {
  it("joins the base and drops undefined query values", () => {
    expect(API_BASE_URL).toBe("http://localhost:8080/api/v1");
    expect(buildUrl("/networks")).toBe("http://localhost:8080/api/v1/networks");
    expect(buildUrl("networks/robinhood/series", { range: "1h", step: undefined, limit: 5 })).toBe(
      "http://localhost:8080/api/v1/networks/robinhood/series?range=1h&limit=5",
    );
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
