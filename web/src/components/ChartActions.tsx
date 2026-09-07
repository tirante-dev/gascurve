"use client";

import Link from "next/link";
import { chartHref, type ChartLinkParams, type ChartView } from "@/lib/chartViews";

/**
 * The two corner marks of the enlarge control, drawn here rather than pulled
 * from an icon package: two glyphs are not worth a dependency, and drawing
 * them inline keeps them on `currentColor` with the rest of the control.
 */
function MaximizeIcon({ className }: { className: string }) {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" className={className} aria-hidden="true">
      <polyline points="15 3 21 3 21 9" />
      <polyline points="9 21 3 21 3 15" />
      <line x1="21" y1="3" x2="14" y2="10" />
      <line x1="3" y1="21" x2="10" y2="14" />
    </svg>
  );
}

export function MinimizeIcon({ className }: { className: string }) {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" className={className} aria-hidden="true">
      <polyline points="4 14 10 14 10 20" />
      <polyline points="20 10 14 10 14 4" />
      <line x1="14" y1="10" x2="21" y2="3" />
      <line x1="3" y1="21" x2="10" y2="14" />
    </svg>
  );
}

/** What the enlarge control is called, wherever it appears. */
export const ENLARGE_TITLE = "Enlarge chart";

/** The two footprints the control comes in: one for a card header, a denser one for the hero's control row. */
const SIZES = {
  card: { box: "h-7 w-7", icon: "h-3.5 w-3.5" },
  hero: { box: "h-6 w-6", icon: "h-3 w-3" },
} as const;

export type EnlargeSize = keyof typeof SIZES;

/**
 * The control every chart card header carries: a link to that chart's own
 * page, at the range and constraint the card is showing. A link and not a
 * button, so it opens in a new tab, can be copied, and works before the
 * client has hydrated. `flex-none` and a fixed box, so a header's title keeps
 * the whole of the remaining width instead of being pushed onto a second line
 * at phone widths.
 */
export function EnlargeLink({
  network,
  view,
  range,
  constraint,
  size = "card",
  label,
  className = "",
}: ChartLinkParams & {
  network: string;
  view: ChartView;
  size?: EnlargeSize;
  /** What the link is announced as, when the card names something narrower than the chart does (one backlog slot, say). */
  label?: string;
  className?: string;
}) {
  const box = SIZES[size];
  return (
    <Link
      href={chartHref(network, view, { range, constraint })}
      aria-label={`Open ${label ?? view.title} enlarged`}
      title={ENLARGE_TITLE}
      className={`vw-control inline-flex flex-none items-center justify-center text-ink-2 hover:text-ink ${box.box} ${className}`}
    >
      <MaximizeIcon className={box.icon} />
    </Link>
  );
}
