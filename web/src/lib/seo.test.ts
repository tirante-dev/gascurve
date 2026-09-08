import { describe, expect, it } from "vitest";
import { DEFAULT_NETWORK } from "@/hooks/useNetwork";
import { CARD_IMAGE, CARD_SIZE, DEFAULT_SITE_URL, PRIMARY_NETWORK, SITE_DESCRIPTION, SITE_NETWORKS, SITE_TITLE, SITE_URL, absoluteUrl, cardImage, cardPath, networkDisplayName, normalizeSiteUrl, pageMetadata } from "@/lib/seo";

describe("normalizeSiteUrl", () => {
  it("keeps an origin and drops a trailing slash", () => {
    expect(normalizeSiteUrl("https://gascurve.com")).toBe("https://gascurve.com");
    expect(normalizeSiteUrl("https://gascurve.com/")).toBe("https://gascurve.com");
    expect(normalizeSiteUrl("  http://localhost:3000/  ")).toBe("http://localhost:3000");
  });

  it("drops a path, because the app serves every route at the origin root", () => {
    // Next joins metadataBase's pathname onto relative metadata URLs, so a
    // sub-path origin would emit canonicals under it while the routes stay at
    // the root: metadata pointing at URLs that 404. Serving under a sub-path
    // is a basePath change first.
    expect(normalizeSiteUrl("https://example.com/gas/")).toBe("https://example.com");
    expect(normalizeSiteUrl("https://example.com/gas/deep")).toBe("https://example.com");
  });

  it("keeps an explicit port, which a preview deployment needs", () => {
    expect(normalizeSiteUrl("http://localhost:3000")).toBe("http://localhost:3000");
  });

  it("rejects anything that is not an absolute http(s) URL", () => {
    expect(normalizeSiteUrl(undefined)).toBeNull();
    expect(normalizeSiteUrl("")).toBeNull();
    expect(normalizeSiteUrl("   ")).toBeNull();
    expect(normalizeSiteUrl("/api/v1")).toBeNull();
    expect(normalizeSiteUrl("gascurve.com")).toBeNull();
    expect(normalizeSiteUrl("ftp://gascurve.com")).toBeNull();
    expect(normalizeSiteUrl("javascript:alert(1)")).toBeNull();
  });
});

describe("absoluteUrl", () => {
  it("joins a path to the base, with or without its leading slash", () => {
    expect(absoluteUrl("/robinhood", "https://gascurve.com")).toBe("https://gascurve.com/robinhood");
    expect(absoluteUrl("robinhood", "https://gascurve.com")).toBe("https://gascurve.com/robinhood");
  });

  it("defaults to the configured site URL", () => {
    expect(absoluteUrl("/sitemap.xml")).toBe(`${SITE_URL}/sitemap.xml`);
  });
});

describe("SITE_URL", () => {
  it("falls back to the value the published image defaults to", () => {
    // The test environment sets no NEXT_PUBLIC_SITE_URL, so this is the fallback path.
    expect(SITE_URL).toBe(DEFAULT_SITE_URL);
  });
});

describe("pageMetadata", () => {
  const args = { network: "robinhood", title: "Base fee on Robinhood", description: "The base fee.", path: "/robinhood/charts/base-fee" };

  it("carries a canonical link and an absolute card URL when the route is the canonical one", () => {
    const meta = pageMetadata({ ...args, canonical: true });
    expect(meta.alternates?.canonical).toBe("/robinhood/charts/base-fee");
    expect(meta.robots).toBeUndefined();
    expect(meta.openGraph?.url).toBe(`${SITE_URL}/robinhood/charts/base-fee`);
    expect(meta.openGraph?.title).toBe(args.title);
    expect(meta.twitter?.description).toBe(args.description);
  });

  it("carries the network's live card on both objects", () => {
    // Next replaces a whole openGraph or twitter object when a page declares
    // one, so a card left to the opengraph-image file convention would be
    // dropped from exactly the pages people share. Both must name it.
    for (const canonical of [true, false]) {
      const meta = pageMetadata({ ...args, canonical });
      expect(meta.openGraph).toMatchObject({ images: [cardImage("robinhood")] });
      expect(meta.twitter).toMatchObject({ images: [cardImage("robinhood")] });
    }
  });

  it("points a chain id route at that route's own card, not at the name form's", () => {
    const meta = pageMetadata({ ...args, network: "4663", path: "/4663", canonical: false });
    expect(meta.openGraph).toMatchObject({ images: [cardImage("4663")] });
  });

  it("asks not to be indexed, and drops the canonical link, for a duplicate route", () => {
    const meta = pageMetadata({ ...args, canonical: false });
    expect(meta.alternates).toBeUndefined();
    expect(meta.robots).toEqual({ index: false, follow: true });
    // The card still names the page it is on, so a shared link previews correctly.
    expect(meta.openGraph?.url).toBe(`${SITE_URL}/robinhood/charts/base-fee`);
  });
});

describe("cardImage", () => {
  it("points at the network's own card route, at the size every platform asks for", () => {
    expect(cardPath("robinhood")).toBe("/robinhood/card");
    expect(cardImage("robinhood")).toEqual({ url: "/robinhood/card", ...CARD_SIZE, alt: expect.stringContaining("Robinhood Chain") });
  });

  it("escapes a network that is not route-shaped, so the URL cannot be broken by the parameter", () => {
    expect(cardPath("a/b?c")).toBe("/a%2Fb%3Fc/card");
  });

  it("keeps the crafted card for the pages outside a network", () => {
    expect(CARD_IMAGE.url).toBe("/og-card.png");
  });
});

describe("SITE_NETWORKS", () => {
  it("holds the networks config.yaml publishes, as route-shaped names", () => {
    expect(SITE_NETWORKS.map((n) => n.name)).toEqual(["robinhood", "robinhood-testnet", "arbitrum-one", "arbitrum-sepolia"]);
    for (const network of SITE_NETWORKS) expect(network.name).toMatch(/^[a-z0-9-]+$/);
  });

  it("names exactly one primary chain, first, and it is the one the root opens", () => {
    expect(SITE_NETWORKS.filter((n) => n.primary).map((n) => n.name)).toEqual([PRIMARY_NETWORK]);
    expect(SITE_NETWORKS[0]?.name).toBe(PRIMARY_NETWORK);
    // A redirect to a chain the site does not lead with would undercut every
    // title and priority built on PRIMARY_NETWORK.
    expect(PRIMARY_NETWORK).toBe(DEFAULT_NETWORK);
  });
});

describe("networkDisplayName", () => {
  it("uses the name config.yaml gives a known network", () => {
    expect(networkDisplayName("robinhood")).toBe("Robinhood Chain");
    expect(networkDisplayName("robinhood-testnet")).toBe("Robinhood Chain Testnet");
    expect(networkDisplayName("arbitrum-one")).toBe("Arbitrum One");
  });

  it("falls back to the title cased route parameter for one it does not know", () => {
    expect(networkDisplayName("some-new-chain")).toBe("Some New Chain");
    expect(networkDisplayName("4663")).toBe("Chain 4663");
  });
});

describe("site copy", () => {
  it("leads with the chain the site is for", () => {
    expect(SITE_TITLE).toMatch(/^Robinhood Chain gas tracker/);
    expect(SITE_DESCRIPTION).toContain("Robinhood Chain");
  });

  it("keeps the description inside the length a search result shows", () => {
    expect(SITE_DESCRIPTION.length).toBeLessThanOrEqual(160);
  });
});
