import type { MetadataRoute } from "next";
import { CHART_VIEWS } from "@/lib/chartViews";
import { SITE_NETWORKS, absoluteUrl } from "@/lib/seo";

/**
 * /sitemap.xml: the index, and for every network its page, its explainer and one entry per chart. The
 * priorities say what the site is for, with Robinhood Chain first and the comparison chains below.
 * Change frequencies reflect what actually moves.
 */
export default function sitemap(): MetadataRoute.Sitemap {
  const now = new Date();
  const entries: MetadataRoute.Sitemap = [{ url: absoluteUrl("/"), lastModified: now, changeFrequency: "daily", priority: 0.9 }];
  for (const network of SITE_NETWORKS) {
    const rank = network.primary ? { page: 1, explainer: 0.8, chart: 0.7 } : { page: 0.5, explainer: 0.3, chart: 0.3 };
    entries.push({ url: absoluteUrl(`/${network.name}`), lastModified: now, changeFrequency: "hourly", priority: rank.page });
    entries.push({ url: absoluteUrl(`/${network.name}/how-it-works`), lastModified: now, changeFrequency: "monthly", priority: rank.explainer });
    for (const view of CHART_VIEWS) {
      entries.push({ url: absoluteUrl(`/${network.name}/charts/${view.id}`), lastModified: now, changeFrequency: "hourly", priority: rank.chart });
    }
  }
  return entries;
}
