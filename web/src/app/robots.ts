import type { MetadataRoute } from "next";
import { SITE_URL, absoluteUrl } from "@/lib/seo";

/**
 * /robots.txt. Everything the site serves is public telemetry, so the whole
 * tree is crawlable; the api is excluded because its JSON is not a page and
 * indexing it would only spend crawl budget.
 */
export default function robots(): MetadataRoute.Robots {
  return {
    rules: [{ userAgent: "*", allow: "/", disallow: "/api/" }],
    sitemap: absoluteUrl("/sitemap.xml"),
    host: SITE_URL,
  };
}
