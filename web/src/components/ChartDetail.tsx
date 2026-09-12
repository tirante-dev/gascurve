"use client";

import Link from "next/link";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { useCallback, useMemo, type ReactNode } from "react";
import { useApi } from "@/hooks/useApi";
import { useNetworkLive } from "@/hooks/useNetworkLive";
import { useRefreshOnOwnerAction, useSeries } from "@/hooks/useSeries";
import { useLiveFrame, type SmoothedLive } from "@/hooks/useSmoothedLive";
import { listNetworks } from "@/lib/api/networks";
import {
  CHART_VIEWS,
  chartBackHref,
  chartHref,
  DETAIL_FRAME_CLASS,
  getChartView,
  resolveChartRange,
  resolveConstraint,
} from "@/lib/chartViews";
import { emptyRangeNote } from "@/lib/gaps";
import type { HeroRange } from "@/lib/hero";
import type { ApiState } from "@/hooks/useApi";
import type { LiveSnapshot, PricerModel, Series, SeriesRange } from "@/types";
import { seriesCount, slotLabel } from "@/utils/chart";
import { findNetwork } from "@/utils/network";
import { MinimizeIcon } from "./ChartActions";
import { SawtoothPanel, shortWindowIndices } from "./ConstraintCards";
import { FeeFlowChart, feeFlowLegend } from "./FeeFlows";
import { HISTORY_OPTIONS } from "./HistoryTabs";
import { L1CostChart, l1CostLegend, useL1Costs } from "./L1Section";
import { HERO_RANGE_OPTIONS, HeroChartPanel, HeroThroughputPanel, RESYNC_COPY, WAITING_COPY } from "./LiveHero";
import { PageHeader } from "./PageHeader";
import { Legend, Section, StatusPill } from "./primitives";
import { RangeTabs, type RangeOption } from "./RangeTabs";
import { BacklogChart, buildSeriesModel, ContributionChart } from "./SeriesCharts";
import { TaylorChart } from "./TaylorChart";
import { ThemeToggle } from "./ThemeToggle";

/** A word inside the chart card: what there is to say when there is no chart to draw. */
function ChartNote({ children }: { children: ReactNode }) {
  return (
    <div className={`flex items-center justify-center rounded-sm bg-chart px-6 text-center text-sm text-ink-2 ${DETAIL_FRAME_CLASS}`} aria-live="polite">
      {children}
    </div>
  );
}

/** The row of every chart the site draws. The one on screen is the current page and is not a link away from itself. */
function ChartTabs({ network, current, range }: { network: string; current?: string; range: string | null }) {
  return (
    <nav aria-label="Charts" className="flex flex-wrap gap-2">
      {CHART_VIEWS.map((view) => {
        const active = view.id === current;
        return (
          <Link
            key={view.id}
            href={chartHref(network, view, { range })}
            aria-current={active ? "page" : undefined}
            className={`rounded-full px-3 py-1 text-sm ${active ? "vw-tab-on bg-accent font-medium text-accent-ink" : "vw-control text-ink-2 hover:text-ink"}`}
          >
            {view.shortTitle}
          </Link>
        );
      })}
    </nav>
  );
}

/** The base fee, enlarged: the live block ring or a bucketed range, the same panel the hero draws. */
function BaseFeeBody({ live, range, series, model }: { live: SmoothedLive; range: HeroRange; series: ApiState<Series>; model: PricerModel }) {
  const frame = useLiveFrame(live.frame);
  const snapshot = live.display;
  if (!snapshot) return <ChartNote>{live.resyncing ? RESYNC_COPY : WAITING_COPY}</ChartNote>;
  return (
    <HeroChartPanel
      readout
      snapshot={snapshot}
      blocks={frame.blocks}
      places={frame.places}
      nowMs={frame.nowMs}
      range={range}
      series={series.data}
      seriesLoading={series.loading}
      seriesError={series.error}
      model={model}
      height={DETAIL_FRAME_CLASS}
    />
  );
}

/**
 * The throughput chart, enlarged: the live per-second view or a bucketed
 * range, the same panel the hero draws under its base fee chart.
 */
function ThroughputBody({ live, range, series, model }: { live: SmoothedLive; range: HeroRange; series: ApiState<Series>; model: PricerModel }) {
  const frame = useLiveFrame(live.frame);
  return (
    <HeroThroughputPanel
      readout
      blocks={frame.blocks}
      places={frame.places}
      nowMs={frame.nowMs}
      range={range}
      series={series.data}
      seriesLoading={series.loading}
      seriesError={series.error}
      model={model}
      height={DETAIL_FRAME_CLASS}
      minWidth={560}
    />
  );
}

/** One short window's backlog, enlarged, following the frame the way the card does. */
function SawtoothBody({ live, index }: { live: SmoothedLive; index: number | null }) {
  const frame = useLiveFrame(live.frame);
  const snapshot = live.display;
  if (!snapshot) return <ChartNote>{live.resyncing ? RESYNC_COPY : WAITING_COPY}</ChartNote>;
  if (index === null) return <ChartNote>This chain has no constraint with a window short enough to draw a sawtooth for.</ChartNote>;
  return <SawtoothPanel snapshot={snapshot} values={frame.values} blocks={frame.blocks} places={frame.places} nowMs={frame.nowMs} index={index} height={DETAIL_FRAME_CLASS} />;
}

/**
 * The constraints the chart can be pointed at: the short windows of the live
 * set for the sawtooth, and every slot the history has for the backlogs.
 */
function constraintChoices(viewId: string | null, takesConstraint: boolean, snapshot: LiveSnapshot | null, series: Series | null, model: PricerModel): number[] {
  if (!takesConstraint) return [];
  if (viewId === "backlog-sawtooth") return shortWindowIndices(snapshot);
  // The slots the range can actually draw, never the shape of a set no point
  // in it matches: an unusable set would offer switcher slots with nothing
  // behind them.
  return series ? Array.from({ length: seriesCount(series, model) }, (_, i) => i) : [];
}

/** The state a history chart is in before it has buckets to draw. */
function seriesNote(series: ApiState<Series>): string | null {
  if (series.error !== null && series.data === null) return `Could not load history: ${series.error}`;
  if (series.data === null) return series.loading ? "Loading history." : "No history yet.";
  // An empty range says nothing about when indexing began, so the note is the
  // bare one: the api's window carries no first point to name.
  if (series.data.points.length === 0) return emptyRangeNote(null);
  return null;
}

/** What a chart's own page draws: its legend, if it has one, above the chart itself. */
type ChartBody = { legend?: ReactNode; chart: ReactNode };

/**
 * A chart on a page of its own: the same component the network page draws,
 * from the same hooks, in a frame most of the viewport tall. The URL is the
 * state: `?range=` picks the range and `?constraint=` the constraint, so a
 * link to this page is a link to exactly what its sender was looking at.
 */
export function ChartDetail({ network, chart }: { network: string; chart: string }) {
  const view = getChartView(chart);
  const router = useRouter();
  const pathname = usePathname();
  const searchParams = useSearchParams();
  const rawRange = searchParams.get("range");
  const rawConstraint = searchParams.get("constraint");
  const range = view === null ? null : resolveChartRange(view, rawRange);

  // A socket only for a chart that draws the feed. The registry already says
  // which charts those are, and a page of bucketed history that held a
  // subscription open ran a smoothing loop nobody was reading. Every page
  // names its chain from the REST network list, which the live hello refines
  // when there is one.
  const needsLive = view?.live ?? false;
  const { live, smooth, snapshot } = useNetworkLive(network, needsLive);
  const networks = useApi("networks", useCallback((signal: AbortSignal) => listNetworks({ signal }), []), { refetchMs: 300_000 });
  const info = live.networkInfo ?? (networks.data ? findNetwork(networks.data, network) : undefined) ?? null;
  // Which pricer the history belongs to. The series carries no model of its
  // own; an empty constraint-set list must not be read as legacy.
  const model: PricerModel = info?.model ?? snapshot?.model ?? "unknown";

  // Live is not a range the api serves buckets for, so the base fee on Live
  // asks for none at all.
  const seriesRange: SeriesRange | null = range === null || range === "live" ? null : (range as SeriesRange);
  const series = useSeries(seriesRange === null ? null : network, seriesRange);
  useRefreshOnOwnerAction(live.ownerActions, series.refresh);
  const m = useMemo(() => (series.data ? buildSeriesModel(series.data, model) : null), [series.data, model]);

  const viewId = view === null ? null : view.id;
  // Cheap enough to derive on every render (a handful of indices), and the
  // compiler memoises it with the rest of the component.
  const choices = constraintChoices(viewId, view !== null && view.constraint, snapshot, series.data, model);
  const constraint = resolveConstraint(rawConstraint, choices);

  const l1 = useL1Costs(network, seriesRange ?? "24h", series.data, viewId === "l1");

  /** The URL is the source of truth: a control writes to it and the page follows. */
  const setParam = useCallback(
    (key: string, value: string) => {
      const next = new URLSearchParams(searchParams.toString());
      next.set(key, value);
      router.replace(`${pathname}?${next.toString()}`, { scroll: false });
    },
    [pathname, router, searchParams],
  );

  // The switcher stays to slot numbers: a full constraint label ("C1 · 60
  // Mgas/s · 15 s (set 6)") would run a segmented control off a phone screen.
  // What the chosen slot is is said beside it instead.
  const constraintOptions: RangeOption<string>[] = choices.map((i) => ({ value: String(i), label: `C${i + 1}` }));
  const constraintNote = constraint === null ? null : viewId === "backlogs" && series.data ? slotLabel(series.data, constraint, model) : null;

  const body = ((): ChartBody | null => {
    if (view === null) return null;
    const note = seriesNote(series);
    switch (view.id) {
      case "base-fee":
        return { chart: <BaseFeeBody live={smooth} range={(range ?? "live") as HeroRange} series={series} model={model} /> };
      case "backlog-sawtooth":
        return { chart: <SawtoothBody live={smooth} index={constraint} /> };
      case "taylor":
        return { chart: <TaylorChart snapshot={snapshot} height={DETAIL_FRAME_CLASS} heading={false} /> };
      case "contribution":
        return { legend: m ? <Legend items={m.contributionLegend} /> : null, chart: m && note === null ? <ContributionChart m={m} height={DETAIL_FRAME_CLASS} /> : <ChartNote>{note}</ChartNote> };
      case "gas-per-second":
        // On Live the chart is the block ring, which has no constraint
        // targets on it and so nothing for a legend to name.
        return { legend: m && range !== "live" ? <Legend items={m.gasLegend} /> : null, chart: <ThroughputBody live={smooth} range={(range ?? "live") as HeroRange} series={series} model={model} /> };
      case "backlogs":
        return {
          chart:
            m && note === null && constraint !== null && series.data ? (
              <BacklogChart m={m} index={constraint} label={slotLabel(series.data, constraint, model)} height={DETAIL_FRAME_CLASS} />
            ) : (
              <ChartNote>{note ?? "This range has no constraint slots to draw."}</ChartNote>
            ),
        };
      case "fee-flows":
        return {
          legend: m ? <Legend items={feeFlowLegend(m.points.some((p) => p.unsplitFeesEth !== null))} /> : null,
          chart: m && note === null ? <FeeFlowChart points={m.points} gaps={m.gaps} height={DETAIL_FRAME_CLASS} /> : <ChartNote>{note}</ChartNote>,
        };
      case "l1":
        return {
          legend: <Legend items={l1CostLegend(l1.totals)} />,
          chart:
            l1.rows.length > 0 ? (
              <L1CostChart rows={l1.rows} bucket={l1.bucket} span={l1.span} domain={l1.domain} gaps={l1.gaps} height={DETAIL_FRAME_CLASS} />
            ) : (
              <ChartNote>{l1.batches.error !== null ? `Could not load batches: ${l1.batches.error}` : l1.batches.loading || series.loading ? "Loading batch reports." : "No batch reports in this range."}</ChartNote>
            ),
        };
    }
  })();

  return (
    <div className="mx-auto max-w-page px-4 pb-12 sm:px-6">
      <PageHeader name={network} info={info}>
        <Link href={view === null ? `/${encodeURIComponent(network)}` : chartBackHref(network, view)} className="vw-control inline-flex items-center gap-1.5 px-3 py-1 text-sm text-ink-2 hover:text-ink">
          <MinimizeIcon className="h-3.5 w-3.5" />
          Back to the dashboard
        </Link>
        <ThemeToggle />
      </PageHeader>

      <main>
        {view === null ? (
          <Section id="not-found" title="Chart not found">
            <div className="vw-card p-4 text-sm text-ink-2">
              <p className="mb-4">
                There is no chart called <span className="num text-ink">{chart}</span>. Every chart the site draws is below.
              </p>
              <ChartTabs network={network} range={null} />
            </div>
          </Section>
        ) : (
          <Section
            id={view.section}
            title={view.title}
            aside={
              <div className="flex flex-wrap items-center gap-3">
                {view.range === "hero" ? (
                  <RangeTabs options={HERO_RANGE_OPTIONS} value={(range ?? "live") as HeroRange} onChange={(next) => setParam("range", next)} label="Base fee chart range" loading={series.loading && series.data !== null} />
                ) : null}
                {view.range === "series" ? (
                  <RangeTabs options={HISTORY_OPTIONS} value={(range ?? "24h") as SeriesRange} onChange={(next) => setParam("range", next)} label="History range" loading={series.loading && series.data !== null} />
                ) : null}
                {view.live ? <StatusPill status={live.status} /> : null}
              </div>
            }
          >
            <p className="mb-4 max-w-[65ch] text-sm text-ink-2">{view.description}</p>
            <ChartTabs network={network} current={view.id} range={rawRange} />
            <div className="vw-card mt-4 p-4">
              {constraintOptions.length > 1 || body?.legend ? (
                <div className="mb-3 flex flex-wrap items-center justify-between gap-3">
                  {constraintOptions.length > 1 ? (
                    <div className="flex flex-wrap items-center gap-3">
                      <RangeTabs options={constraintOptions} value={String(constraint)} onChange={(next) => setParam("constraint", next)} label="Constraint" />
                      {constraintNote ? <span className="num text-xs text-ink-3">{constraintNote}</span> : null}
                    </div>
                  ) : (
                    <span />
                  )}
                  {body?.legend}
                </div>
              ) : null}
              {body?.chart}
            </div>
          </Section>
        )}
      </main>
    </div>
  );
}
