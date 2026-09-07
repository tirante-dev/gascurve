// Every chart the site draws, in one list. The cards on the network page link
// to the enlarged view of the chart they hold, and the detail route reads the
// same entry to decide which chart to draw, which controls it needs and where
// its back link goes. Kept pure (no JSX, no hooks) so the ids, the hrefs and
// the range and constraint parsing are testable without a DOM.

import { isSeriesRange } from "@/lib/api/series";
import { isHeroRange } from "@/lib/hero";
import type { HeroRange } from "@/lib/hero";
import type { SeriesRange } from "@/types";

/**
 * The section of the network page a chart lives in, which is where its
 * enlarged view links back to. Every one of these is the id of a Section on
 * the network page, so `/robinhood#history` lands on the chart's own row.
 */
export const CHART_SECTIONS = ["live", "pricer", "history", "fees", "l1", "explainer"] as const;

export type ChartSection = (typeof CHART_SECTIONS)[number];

/**
 * Which range control a chart carries. "series" is one of the api's ranges
 * (the history charts); "hero" is the base fee's own control, which adds the
 * live block ring in front of those ranges; "none" is a chart that draws the
 * same thing at every range.
 */
export type ChartRangeKind = "none" | "series" | "hero";

export type ChartViewId = "base-fee" | "backlog-sawtooth" | "contribution" | "gas-per-second" | "backlogs" | "fee-flows" | "l1" | "taylor";

export type ChartView = {
  id: ChartViewId;
  /** The heading the enlarged view wears, and what its link is announced as. */
  title: string;
  /** The word on its tab, short enough for a row of eight at phone widths. */
  shortTitle: string;
  description: string;
  section: ChartSection;
  range: ChartRangeKind;
  /** True when the chart draws the live feed, so the detail route needs the socket. */
  live: boolean;
  /** True when the chart draws one constraint at a time, chosen with ?constraint=. */
  constraint: boolean;
};

/** The range a history chart falls back to, the same one the network page opens on. */
export const DEFAULT_SERIES_RANGE: SeriesRange = "24h";

/** The range the base fee chart falls back to: the live block ring. */
export const DEFAULT_HERO_RANGE: HeroRange = "live";

/**
 * The frame an enlarged chart draws in: most of the viewport's height, with a
 * floor so a short window still reads and a ceiling so a tall one does not
 * outrun the page.
 */
export const DETAIL_FRAME_CLASS = "h-[62vh] min-h-[360px] max-h-[720px]";

export const CHART_VIEWS: readonly ChartView[] = [
  {
    id: "base-fee",
    title: "Base fee",
    shortTitle: "Base fee",
    description: "What the chain charges per unit of gas: every block over the last two minutes on Live, and the bucketed average with its min-to-max band, the floor in force and owner actions over each history range.",
    section: "live",
    range: "hero",
    live: true,
    constraint: false,
  },
  {
    id: "backlog-sawtooth",
    title: "Short-window backlog per block",
    shortTitle: "Backlog sawtooth",
    description: "A short window's backlog after each of the last fifteen seconds of blocks, with the gas the constraint sheds at every second boundary. Nitro pays a backlog down only when the block timestamp advances, so bursts show as sawteeth.",
    section: "pricer",
    range: "none",
    live: true,
    constraint: true,
  },
  {
    id: "contribution",
    title: "Contribution to x per constraint",
    shortTitle: "Contribution",
    description: "How much of the exponent each constraint contributed in every bucket, stacked. A replaced constraint set starts its own series rather than continuing the one before it.",
    section: "history",
    range: "series",
    live: false,
    constraint: false,
  },
  {
    id: "gas-per-second",
    title: "Gas throughput",
    shortTitle: "Gas/s",
    description:
      "What the chain actually carried, per second: every whole second of the block ring on Live, and the rate in each bucket against the target of every constraint in force at the time, drawn as a stepped line per set, over each history range. It sits under the base fee in the hero, on the hero's own range.",
    section: "live",
    range: "hero",
    live: true,
    constraint: false,
  },
  {
    id: "backlogs",
    title: "Backlog per constraint",
    shortTitle: "Backlogs",
    description: "One slot of the constraint set: the gas above its target rate that had not yet drained, bucket by bucket. Each slot has its own scale, and a constraint that was replaced starts a new series.",
    section: "history",
    range: "series",
    live: false,
    constraint: true,
  },
  {
    id: "fee-flows",
    title: "Fees collected per bucket",
    shortTitle: "Fee flows",
    description: "Fees paid in every bucket in ETH, stacked by destination: compute floor to infrastructure, compute congestion to the network account, and poster fees to the L1 pricer pool.",
    section: "fees",
    range: "series",
    live: false,
    constraint: false,
  },
  {
    id: "l1",
    title: "L2 fees against ArbOS-attributed batch cost",
    shortTitle: "L1 costs",
    description: "L2 fees against the version-aware batch-poster spending ArbOS attributes from batchPostingReport internal transactions, on a log scale. This is not an Ethereum receipt total.",
    section: "l1",
    range: "series",
    live: false,
    constraint: false,
  },
  {
    id: "taylor",
    title: "P4(x) against e^x",
    shortTitle: "P4 against e^x",
    description: "The degree-4 Taylor polynomial the chain multiplies the floor by, against the exponential it approximates. Above x of about 2 the fee grows like x⁴/24, a polynomial, not an exponential.",
    section: "explainer",
    range: "none",
    live: true,
    constraint: false,
  },
];

export function getChartView(id: string | undefined): ChartView | null {
  return CHART_VIEWS.find((view) => view.id === id) ?? null;
}

/**
 * The entry for an id known at the call site, so a card can name the chart it
 * holds without handling a null the type system has already ruled out.
 */
export function chartView(id: ChartViewId): ChartView {
  const view = getChartView(id);
  if (view === null) throw new Error(`no chart view ${id}`);
  return view;
}

/** Where a chart's enlarged view links back to: its own row on the network page. */
export function chartBackHref(network: string, view: ChartView): string {
  return `/${encodeURIComponent(network)}#${view.section}`;
}

/**
 * The range to put in a link to `view`, or null when it takes none or when
 * the range in hand is not one it understands. Moving from the base fee on
 * Live to a history chart carries no range, so that chart opens on its own
 * default rather than on a range it cannot draw.
 */
export function linkRange(view: ChartView, range: string | null | undefined): string | null {
  if (range === null || range === undefined) return null;
  if (view.range === "hero") return isHeroRange(range) ? range : null;
  if (view.range === "series") return isSeriesRange(range) ? range : null;
  return null;
}

export type ChartLinkParams = { range?: string | null; constraint?: number | null };

/** The href of a chart's enlarged view, carrying the range and constraint it applies to. */
export function chartHref(network: string, view: ChartView, params: ChartLinkParams = {}): string {
  const query = new URLSearchParams();
  const range = linkRange(view, params.range);
  if (range !== null) query.set("range", range);
  const constraint = params.constraint;
  if (view.constraint && typeof constraint === "number" && Number.isInteger(constraint) && constraint >= 0) {
    query.set("constraint", String(constraint));
  }
  const search = query.toString();
  return `/${encodeURIComponent(network)}/charts/${view.id}${search === "" ? "" : `?${search}`}`;
}

/** The hero range a `?range=` stands for; Live for anything else, including a missing one. */
export function resolveHeroRange(raw: string | null | undefined): HeroRange {
  return raw !== null && raw !== undefined && isHeroRange(raw) ? raw : DEFAULT_HERO_RANGE;
}

/** The api range a `?range=` stands for; 24h for anything else, Live included. */
export function resolveSeriesRange(raw: string | null | undefined): SeriesRange {
  return raw !== null && raw !== undefined && isSeriesRange(raw) ? raw : DEFAULT_SERIES_RANGE;
}

/**
 * The range the detail route should draw `view` at, as a string: the hero's
 * ranges for the base fee, the api's for a history chart, and nothing at all
 * for a chart that takes no range.
 */
export function resolveChartRange(view: ChartView, raw: string | null | undefined): string | null {
  if (view.range === "hero") return resolveHeroRange(raw);
  if (view.range === "series") return resolveSeriesRange(raw);
  return null;
}

/**
 * The constraint a `?constraint=` picks out of `choices` (the indices the
 * chart can draw), or the first of them when it names none of them. Null when
 * there is nothing to choose from at all.
 */
export function resolveConstraint(raw: string | null | undefined, choices: readonly number[]): number | null {
  if (choices.length === 0) return null;
  const index = raw === null || raw === undefined ? Number.NaN : Number(raw);
  return choices.includes(index) ? index : choices[0];
}
