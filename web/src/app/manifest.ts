import type { MetadataRoute } from "next";
import { PRIMARY_NETWORK_NAME, SITE_DESCRIPTION, SITE_NAME } from "@/lib/seo";

/**
 * /manifest.webmanifest, so an installed or pinned copy of the site carries
 * the mark and the dark ground rather than a screenshot of the page. The
 * maskable icon is the same mark inside a safe zone, which is what keeps
 * Android from cropping the curve's tip.
 */
export default function manifest(): MetadataRoute.Manifest {
  return {
    name: `${SITE_NAME} · ${PRIMARY_NETWORK_NAME} gas tracker`,
    short_name: SITE_NAME,
    description: SITE_DESCRIPTION,
    start_url: "/",
    display: "standalone",
    background_color: "#0f0326",
    theme_color: "#0f0326",
    icons: [
      { src: "/icon-192.png", sizes: "192x192", type: "image/png", purpose: "any" },
      { src: "/icon-512.png", sizes: "512x512", type: "image/png", purpose: "any" },
      { src: "/icon-maskable-512.png", sizes: "512x512", type: "image/png", purpose: "maskable" },
    ],
  };
}
