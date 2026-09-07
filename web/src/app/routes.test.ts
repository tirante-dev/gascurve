import { describe, expect, it } from "vitest";
import { CHART_VIEWS } from "@/lib/chartViews";
import { PRIMARY_NETWORK, SITE_NETWORKS, SITE_URL } from "@/lib/seo";
import manifest from "./manifest";
import robots from "./robots";
import sitemap from "./sitemap";

describe("robots.txt", () => {
  it("opens the site to crawlers, keeps them out of the api, and points at the sitemap", () => {
    const result = robots();
    expect(result.rules).toEqual([{ userAgent: "*", allow: "/", disallow: "/api/" }]);
    expect(result.sitemap).toBe(`${SITE_URL}/sitemap.xml`);
    expect(result.host).toBe(SITE_URL);
  });
});

describe("sitemap.xml", () => {
  const entries = sitemap();
  const urls = entries.map((e) => e.url);

  it("lists the index, and every page of every network", () => {
    expect(urls).toContain(`${SITE_URL}/`);
    for (const network of SITE_NETWORKS) {
      expect(urls).toContain(`${SITE_URL}/${network.name}`);
      expect(urls).toContain(`${SITE_URL}/${network.name}/how-it-works`);
      for (const view of CHART_VIEWS) expect(urls).toContain(`${SITE_URL}/${network.name}/charts/${view.id}`);
    }
    expect(urls).toHaveLength(1 + SITE_NETWORKS.length * (2 + CHART_VIEWS.length));
  });

  it("has no duplicates and every URL is absolute", () => {
    expect(new Set(urls).size).toBe(urls.length);
    for (const url of urls) expect(url.startsWith(`${SITE_URL}/`)).toBe(true);
  });

  it("ranks the chain the site is for above every other page", () => {
    const priority = (path: string) => entries.find((e) => e.url === `${SITE_URL}${path}`)?.priority;
    const primary = priority(`/${PRIMARY_NETWORK}`) ?? 0;
    expect(primary).toBe(1);
    for (const network of SITE_NETWORKS.filter((n) => !n.primary)) {
      expect(priority(`/${network.name}`) ?? 1).toBeLessThan(primary);
      expect(priority(`/${network.name}/how-it-works`) ?? 1).toBeLessThan(priority(`/${PRIMARY_NETWORK}/how-it-works`) ?? 0);
    }
  });
});

describe("manifest", () => {
  it("names the chain the site is for and ships an icon at every size an installer wants", () => {
    const result = manifest();
    expect(result.name).toContain("Robinhood Chain");
    expect(result.icons?.map((i) => i.sizes)).toEqual(["192x192", "512x512", "512x512"]);
    // Android crops a non-maskable icon to its own shape, which would clip the
    // curve's tip; the padded variant is what it uses instead.
    expect(result.icons?.some((i) => i.purpose === "maskable")).toBe(true);
  });
});
