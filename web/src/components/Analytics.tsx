"use client";

import Script from "next/script";
import { ANALYTICS_BEFORE_SEND, ANALYTICS_HOST_URL, ANALYTICS_SCRIPT_PATH, ANALYTICS_WEBSITE_ID, beforeSend, trackedDomain } from "@/lib/analytics";
import { SITE_URL } from "@/lib/seo";

// Assigned at module scope rather than from an effect: next/script injects the tracker once hydration is
// done, and the hook has to be on window before the tracker sends its first pageview.
if (typeof window !== "undefined") {
  window[ANALYTICS_BEFORE_SEND] = beforeSend;
}

/**
 * Loads the Umami tracker, which collects pageviews on its own by hooking history.pushState and
 * replaceState. Renders nothing unless a website id was baked in, so local development and an
 * unconfigured deployment stay clean. Umami sets no cookies and stores no personal data, so this needs
 * no consent banner; data-do-not-track honours browsers that send the signal.
 */
export default function Analytics() {
  if (ANALYTICS_WEBSITE_ID === "") return null;

  return (
    <Script
      src={ANALYTICS_SCRIPT_PATH}
      strategy="afterInteractive"
      data-website-id={ANALYTICS_WEBSITE_ID}
      data-host-url={ANALYTICS_HOST_URL}
      data-domains={trackedDomain(SITE_URL)}
      data-do-not-track="true"
      data-before-send={ANALYTICS_BEFORE_SEND}
    />
  );
}
