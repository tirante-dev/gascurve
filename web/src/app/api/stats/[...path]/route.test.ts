import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { GET, POST } from "./route";

const UPSTREAM = "http://umami.internal:3000";

function params(...path: string[]) {
  return { params: Promise.resolve({ path }) };
}

beforeEach(() => {
  process.env.UMAMI_URL = UPSTREAM;
});

afterEach(() => {
  delete process.env.UMAMI_URL;
  vi.unstubAllGlobals();
});

describe("GET /api/stats", () => {
  it("relays the tracker script and caches it", async () => {
    let requested: string | undefined;
    vi.stubGlobal("fetch", async (url: string) => {
      requested = url;
      return new Response("!function(){}", { status: 200, headers: { "Content-Type": "text/javascript" } });
    });

    const response = await GET(new Request("https://gascurve.com/api/stats/script.js"), params("script.js"));

    expect(response.status).toBe(200);
    expect(await response.text()).toBe("!function(){}");
    expect(response.headers.get("Cache-Control")).toContain("max-age=3600");
    expect(requested).toBe(`${UPSTREAM}/script.js`);
  });

  it("does not forward the visitor's cookies upstream", async () => {
    let sent: Headers | undefined;
    vi.stubGlobal("fetch", async (_url: string, init: RequestInit) => {
      sent = new Headers(init.headers);
      return new Response("", { status: 200 });
    });

    await GET(new Request("https://gascurve.com/api/stats/script.js", { headers: { cookie: "session=secret", "user-agent": "probe" } }), params("script.js"));

    expect(sent?.has("cookie")).toBe(false);
    expect(sent?.get("user-agent")).toBe("probe");
  });

  it("404s any path other than the script, so this is not an open proxy", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    expect((await GET(new Request("https://gascurve.com/api/stats/api/send"), params("api", "send"))).status).toBe(404);
    expect((await GET(new Request("https://gascurve.com/api/stats/../../etc"), params("..", "..", "etc"))).status).toBe(404);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("404s when no instance is configured", async () => {
    delete process.env.UMAMI_URL;
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    expect((await GET(new Request("https://gascurve.com/api/stats/script.js"), params("script.js"))).status).toBe(404);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("502s when the instance answers with an error or cannot be reached", async () => {
    vi.stubGlobal("fetch", async () => new Response("nope", { status: 500 }));
    expect((await GET(new Request("https://gascurve.com/api/stats/script.js"), params("script.js"))).status).toBe(502);

    vi.stubGlobal("fetch", async () => {
      throw new Error("ECONNREFUSED");
    });
    expect((await GET(new Request("https://gascurve.com/api/stats/script.js"), params("script.js"))).status).toBe(502);
  });
});

describe("POST /api/stats", () => {
  it("relays a beacon and returns what the instance said", async () => {
    let body: string | undefined;
    vi.stubGlobal("fetch", async (url: string, init: RequestInit) => {
      expect(url).toBe(`${UPSTREAM}/api/send`);
      body = init.body as string;
      return new Response("token", { status: 200, headers: { "Content-Type": "text/plain" } });
    });

    const beacon = JSON.stringify({ type: "event", payload: { website: "id", url: "/robinhood" } });
    const response = await POST(new Request("https://gascurve.com/api/stats/api/send", { method: "POST", body: beacon }), params("api", "send"));

    expect(response.status).toBe(200);
    expect(await response.text()).toBe("token");
    expect(response.headers.get("Cache-Control")).toBe("no-store");
    expect(body).toBe(beacon);
  });

  it("404s any path other than the collection endpoint", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    expect((await POST(new Request("https://gascurve.com/api/stats/script.js", { method: "POST" }), params("script.js"))).status).toBe(404);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("502s when the instance cannot be reached, which the tracker ignores", async () => {
    vi.stubGlobal("fetch", async () => {
      throw new Error("ECONNREFUSED");
    });

    const response = await POST(new Request("https://gascurve.com/api/stats/api/send", { method: "POST", body: "{}" }), params("api", "send"));
    expect(response.status).toBe(502);
  });
});
