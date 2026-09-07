"use client";

import { useRouter } from "next/navigation";
import { useEffect } from "react";
import { DEFAULT_NETWORK, readStoredNetwork } from "@/hooks/useNetwork";

/**
 * Sends a viewer who lands on the root to their last network, or to Robinhood
 * Chain. Split out of the index page so that page can stay a server component
 * and carry its own metadata and its crawlable list of networks, which is what
 * a search engine reads when it does not follow the redirect.
 */
export function NetworkRedirect() {
  const router = useRouter();
  useEffect(() => {
    router.replace(`/${readStoredNetwork() ?? DEFAULT_NETWORK}`);
  }, [router]);
  return null;
}
