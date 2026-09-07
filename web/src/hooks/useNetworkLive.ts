"use client";

import { useMemo } from "react";
import type { LiveSnapshot, LiveStatus, Network } from "@/types";
import { useLive, type LiveState } from "./useLive";
import { useSmoothedLive, type SmoothedLive } from "./useSmoothedLive";

/**
 * The live feed as every page consumes it: the raw feed, the smoothed view of
 * it, and the display snapshot the two agree on.
 */
export type NetworkLive = {
  live: LiveState;
  smooth: SmoothedLive;
  /** The snapshot at the display cadence; null while the feed has delivered none. */
  snapshot: LiveSnapshot | null;
  status: LiveStatus;
  /** What the server said this network is, once its hello has arrived. */
  networkInfo: Network | null;
};

/**
 * One socket, one smoothing loop, for the page that calls it. The network page
 * and a chart's own page both draw live charts, and both take the feed from
 * here so the two sides of an enlarge link show the same numbers with the same
 * easing and the same reorg handling. Calling it twice on one page would open
 * two sockets, so a page calls it once and passes what it needs down.
 *
 * `enabled` false opens no socket and runs no frame loop: a page drawing only
 * bucketed history has nothing to animate, and a subscription it never reads
 * is work the device pays for and nobody sees. Such a page names its chain
 * from the REST network list instead.
 */
export function useNetworkLive(network: string, enabled = true): NetworkLive {
  const live = useLive(network, enabled);
  // The feed ticks every block; a page follows the smoothed view of it, so
  // everything outside the animating components re-renders at the display
  // cadence at most.
  const smooth = useSmoothedLive(live, enabled);
  return useMemo(() => ({ live, smooth, snapshot: smooth.display, status: live.status, networkInfo: live.networkInfo }), [live, smooth]);
}
