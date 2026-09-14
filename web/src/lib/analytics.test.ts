import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ANALYTICS_BEFORE_SEND, ANALYTICS_HOST_URL, ANALYTICS_PROXY_PATH, ANALYTICS_SCRIPT_PATH, beforeSend, resetPageviewState, trackEvent, trackedDomain } from "@/lib/analytics";

beforeEach(() => {
  resetPageviewState();
});

afterEach(() => {
  delete window.umami;
});

describe("analytics paths", () => {
  it("serves the script and the collection endpoint from this origin", () => {
    expect(ANALYTICS_PROXY_PATH).toBe("/api/stats");
    expect(ANALYTICS_SCRIPT_PATH).toBe("/api/stats/script.js");
    // Root relative, so the tracker reports to whichever origin served the page.
    expect(ANALYTICS_HOST_URL.startsWith("/")).toBe(true);
    expect(ANALYTICS_BEFORE_SEND).toBe("gascurveBeforeSend");
  });
});

describe("beforeSend", () => {
  it("reports a pageview the first time a page is seen", () => {
    const payload = { url: "/robinhood/charts/base-fee?range=24h" };
    expect(beforeSend("event", payload)).toBe(payload);
  });

  it("drops the pageview a range or constraint change produces", () => {
    expect(beforeSend("event", { url: "/robinhood/charts/backlogs?range=24h&constraint=0" })).not.toBeUndefined();
    expect(beforeSend("event", { url: "/robinhood/charts/backlogs?range=7d&constraint=0" })).toBeUndefined();
    expect(beforeSend("event", { url: "/robinhood/charts/backlogs?range=7d&constraint=2" })).toBeUndefined();
  });

  it("reports a real navigation, and every other parameter", () => {
    expect(beforeSend("event", { url: "/robinhood?range=24h" })).not.toBeUndefined();
    expect(beforeSend("event", { url: "/arbitrum-one?range=24h" })).not.toBeUndefined();
    expect(beforeSend("event", { url: "/arbitrum-one?range=24h&utm_source=x" })).not.toBeUndefined();
  });

  it("treats an absolute URL as the same page as its path", () => {
    expect(beforeSend("event", { url: "/robinhood/explainer" })).not.toBeUndefined();
    expect(beforeSend("event", { url: "https://gascurve.com/robinhood/explainer" })).toBeUndefined();
  });

  it("passes through anything that is not a pageview", () => {
    const named = { url: "/robinhood", name: "network-switch" };
    expect(beforeSend("event", named)).toBe(named);
    const other = { url: "/robinhood" };
    expect(beforeSend("identify", other)).toBe(other);
    const urlless = {};
    expect(beforeSend("event", urlless)).toBe(urlless);
  });

  it("passes through a URL it cannot parse rather than dropping the beacon", () => {
    const payload = { url: "http://[" };
    expect(beforeSend("event", payload)).toBe(payload);
  });
});

describe("trackEvent", () => {
  it("sends the event when the tracker has initialized", () => {
    const track = vi.fn();
    window.umami = { track };
    trackEvent("network-switch", { from: "robinhood", to: "arbitrum-one" });
    expect(track).toHaveBeenCalledWith("network-switch", { from: "robinhood", to: "arbitrum-one" });
  });

  it("does nothing when the script never loaded or was blocked", () => {
    expect(() => trackEvent("range-change", { chart: "base-fee", range: "7d", previous: "24h" })).not.toThrow();
    window.umami = {} as unknown as { track: (name: string) => void };
    expect(() => trackEvent("range-change", { chart: "base-fee", range: "7d", previous: "24h" })).not.toThrow();
  });

  it("swallows a tracker that throws, so the interaction is not interrupted", () => {
    window.umami = {
      track: () => {
        throw new Error("beacon failed");
      },
    };
    expect(() => trackEvent("constraint-change", { chart: "backlogs", constraint: "1", previous: "0" })).not.toThrow();
  });
});

describe("trackedDomain", () => {
  it("returns the canonical hostname", () => {
    expect(trackedDomain("https://gascurve.com")).toBe("gascurve.com");
    expect(trackedDomain("https://gascurve.com/deep/path")).toBe("gascurve.com");
  });

  it("leaves the attribute off for a local or unparseable origin", () => {
    expect(trackedDomain("http://localhost:3000")).toBeUndefined();
    expect(trackedDomain("http://127.0.0.1:3000")).toBeUndefined();
    expect(trackedDomain("file:///tmp/index.html")).toBeUndefined();
    expect(trackedDomain("")).toBeUndefined();
    expect(trackedDomain("gascurve.com")).toBeUndefined();
  });
});
