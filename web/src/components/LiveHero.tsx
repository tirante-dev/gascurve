"use client";

import { memo, useMemo, useState, useSyncExternalStore, type ReactNode } from "react";
import { Area, AreaChart, CartesianGrid, ComposedChart, Line, ReferenceLine, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { useLiveFrame, type SmoothedLive } from "@/hooks/useSmoothedLive";
import { useSeries } from "@/hooks/useSeries";
import { bucketNote, feeChartCaption, feeChartData, feeChartLabel, feeTooltipRows, type FeeChartData } from "@/lib/feeChart";
import { emptyRangeNote } from "@/lib/gaps";
import {
  heroChartData,
  heroFeeAxis,
  heroPointTitle,
  heroThroughputData,
  stableFeeAxis,
  heroRange,
  heroRangeOnServer,
  heroSpan,
  heroTicks,
  heroTimeLabel,
  setHeroRange,
  subscribeHeroRange,
  throughputAxis,
  throughputTick,
  HERO_RANGE_LABELS,
  HERO_RANGES,
  type HeroPoint,
  type HeroRange,
  type ThroughputPoint,
} from "@/lib/hero";
import { assignPlaces, NO_PLACES, SWAP_GAS, targetValues, TRANSFER_GAS, type BlockPlaces, type LiveValues } from "@/lib/smoothing";
import type { BlockPoint, EthUsd, LiveSnapshot, LiveStatus, PricerModel, Series } from "@/types";
import { FLOOR_COLOR, MARKER_COLOR } from "@/utils/chart";
import {
  FIXED_WIDTH_CH,
  formatDateTime,
  formatEthFixed,
  formatGas,
  formatGasFixed,
  formatGasPerSecond,
  formatGwei,
  formatGweiFixed,
  formatInteger,
  formatSignificant,
  formatTick,
  formatTime,
  gasPerSecondParts,
  usdMath,
  weiToGweiNumber,
} from "@/utils/format";
import { chartView } from "@/lib/chartViews";
import { ChartTooltip, type TooltipRow } from "./ChartTooltip";
import { ChartReadout, type ReadoutGroup } from "./ChartReadout";
import { EnlargeLink } from "./ChartActions";
import { GapBands, GapNote } from "./ChartGaps";
import { buildSeriesModel, bucketRowTitle, GasPerSecondChart } from "./SeriesCharts";
import { FeeGauge } from "./FeeGauge";
import { Figure, HoverNote, Label, type NoteAlign, Stat, type StatTone, StatusPill, Term, TIME_AXIS_RIGHT } from "./primitives";
import { RangeTabs, type RangeOption } from "./RangeTabs";

export { SWAP_GAS, TRANSFER_GAS };

/**
 * A snapshot older than this many seconds is a stale collector, not a quiet
 * chain: "since last block" would otherwise keep counting up and read as a
 * stall on the chain's side.
 */
export const COLLECTOR_LAG_S = 5;

/** Seconds between the collector's sample and the wall clock `nowMs`; zero for an unparseable timestamp. */
export function sampleAge(sampledAt: string, nowMs: number): number {
  const at = Date.parse(sampledAt);
  return Number.isNaN(at) ? 0 : Math.max(0, (nowMs - at) / 1000);
}

/** The fee line: the magenta accent, the one series the live hero chart draws. */
const FEE_COLOR = "var(--accent)";

/** The hero's range control: the live block ring first, then the ranges the api serves buckets for. */
export const HERO_RANGE_OPTIONS: readonly RangeOption<HeroRange>[] = HERO_RANGES.map((range) => ({ value: range, label: HERO_RANGE_LABELS[range] }));

/** What a hovered block on the live chart says: which block it was, what it priced at, what it carried and when. */
export function heroTooltipRows(): TooltipRow[] {
  return [
    { label: "block", value: (r) => formatInteger(Number(r.number)) },
    { label: "base fee", color: FEE_COLOR, value: (r) => `${formatGweiFixed(Number(r.fee))} gwei` },
    { label: "gas used", value: (r) => formatGas(Number(r.gasUsed)) },
    { label: "time", value: (r) => formatTime(Number(r.ts)) },
  ];
}

/** The height the hero's chart stands at on the network page; the enlarged view passes its own. */
export const HERO_CHART_HEIGHT = "h-[180px] lg:h-[260px]";

/**
 * The box the hero chart draws in, at one height in every state: live,
 * historical, loading, empty and failed all fill the same frame, so switching
 * range moves nothing on the page around it.
 */
export function ChartBox({ label, busy, height = HERO_CHART_HEIGHT, children }: { label: string; busy?: boolean; height?: string; children: ReactNode }) {
  return (
    <div role="figure" aria-label={label} aria-busy={busy}>
      <div className={`w-full rounded-sm bg-chart ${height}`}>{children}</div>
    </div>
  );
}

/** A word inside the chart box: what there is to say when there is no chart to draw. */
function ChartNote({ children }: { children: ReactNode }) {
  return <p className="flex h-full items-center justify-center px-6 text-center text-xs text-ink-3">{children}</p>;
}

/**
 * The placement to draw with: the ring's own where the caller has one (the
 * live section always does, and it is the only one that keeps a block's place
 * fixed across arrivals and evictions), and a placement of these blocks alone
 * for a caller with none.
 */
function usePlaces(blocks: readonly BlockPoint[], places: BlockPlaces | undefined): BlockPlaces {
  return useMemo(() => places ?? assignPlaces(NO_PLACES, blocks), [places, blocks]);
}

/** Both hero chart axes reserve the same width, so the plot starts in the same place at every range. */
const HERO_AXIS_WIDTH = 56;

/**
 * The last two minutes of per-block base fee. One series, so no legend: the
 * heading names it. The floor is a dashed cyan rule because it is a
 * threshold, not a series, and the fill is a flat low-alpha tint of the line
 * colour rather than a gradient, so the mark carries no meaning the data does
 * not. Memoised on its points, which move with the frame clock.
 */
export const HeroChart = memo(function HeroChart({ points, floorGwei, floorText, height }: { points: HeroPoint[]; floorGwei: number; floorText: string; height?: string }) {
  const span = heroSpan();
  const ticks = useMemo(() => heroTicks(span), [span]);
  // The axis from the previous render stands while the data still fits it
  // (hysteresis, see stableFeeAxis); derived state, so it is settled during
  // render rather than one frame late.
  const [axis, setAxis] = useState(() => heroFeeAxis(points, floorGwei));
  const next = stableFeeAxis(axis, points, floorGwei);
  if (next !== axis) setAxis(next);
  const fees = points.map((p) => p.fee);
  const label =
    points.length < 2
      ? "Base fee per block, waiting for blocks"
      : `Base fee per block over the last ${span} seconds, ${points.length} blocks, ${formatGweiFixed(Math.min(...fees))} to ${formatGweiFixed(Math.max(...fees))} gwei, with the floor at ${formatGweiFixed(floorGwei)} gwei`;
  return (
    <ChartBox label={label} busy={points.length < 2} height={height}>
      {points.length < 2 ? (
        <ChartNote>Waiting for blocks.</ChartNote>
      ) : (
        <ResponsiveContainer width="100%" height="100%">
          <AreaChart data={points} margin={{ top: 8, right: TIME_AXIS_RIGHT, bottom: 2, left: 0 }}>
            {/* Horizontal only: the time axis has its own ticks and a vertical grid would compete with the marks. */}
            <CartesianGrid vertical={false} />
            <XAxis
              dataKey="x"
              type="number"
              domain={[-span, 0]}
              ticks={ticks}
              tickFormatter={heroTimeLabel}
              tickLine
              axisLine={false}
              height={18}
            />
            {/* A fixed width and a fixed decimal count per band: the plot never shifts sideways as the fee moves. */}
            <YAxis domain={axis.domain} ticks={axis.ticks} tickFormatter={(v: number) => formatGweiFixed(v)} tickLine={false} axisLine={false} width={HERO_AXIS_WIDTH} />
            <ReferenceLine
              y={floorGwei}
              stroke={FLOOR_COLOR}
              strokeDasharray="4 3"
              strokeWidth={1}
              label={{ value: `floor ${floorText} gwei`, position: "insideBottomRight" }}
            />
            <Tooltip isAnimationActive={false} content={(props) => <ChartTooltip {...props} title={heroPointTitle} rows={heroTooltipRows()} />} />
            <Area type="monotone" dataKey="fee" stroke={FEE_COLOR} strokeWidth={1.5} fill={FEE_COLOR} fillOpacity={0.12} dot={false} activeDot={{ r: 2.5 }} isAnimationActive={false} />
          </AreaChart>
        </ResponsiveContainer>
      )}
    </ChartBox>
  );
});

/**
 * The same base fee over a bucketed range: the average as a line, the min to
 * max of each bucket as a band around it, the floor in force as a stepped
 * dashed rule, and one marker per owner action. Log scale, because a range
 * that spans a congestion event spans two orders of magnitude.
 */
export const HeroHistoryChart = memo(function HeroHistoryChart({ data, rangeLabel, height }: { data: FeeChartData; rangeLabel: string; height?: string }) {
  const rows = useMemo(() => feeTooltipRows(), []);
  const note = useMemo(() => bucketNote(data.markers, data.bucketSeconds), [data.markers, data.bucketSeconds]);
  return (
    <>
      <ChartBox label={feeChartLabel(rangeLabel, data.points)} height={height}>
        <ResponsiveContainer width="100%" height="100%">
          <ComposedChart data={data.drawn} margin={{ top: 8, right: TIME_AXIS_RIGHT, bottom: 2, left: 0 }}>
            <CartesianGrid vertical={false} />
            <GapBands gaps={data.gaps.gaps} />
            {/* The axis is the window that was asked for, so the buckets that
                exist sit where they happened rather than filling the frame. */}
            <XAxis dataKey="t" type="number" domain={[data.gaps.window.from, data.gaps.window.to]} tickFormatter={(t: number) => formatTick(t, data.span)} tickLine axisLine={false} height={18} minTickGap={48} />
            <YAxis scale="log" domain={data.domain} tickFormatter={(v: number) => formatSignificant(v, 2)} tickLine={false} axisLine={false} width={HERO_AXIS_WIDTH} />
            <Tooltip isAnimationActive={false} content={(props) => <ChartTooltip {...props} title={(t) => formatDateTime(t)} rows={rows} note={note} />} />
            <Area type="monotone" dataKey="feeMax" connectNulls={false} stroke="none" fill="var(--series-1)" fillOpacity={0.12} isAnimationActive={false} activeDot={false} />
            <Area type="monotone" dataKey="feeMin" connectNulls={false} stroke="none" fill="var(--chart)" fillOpacity={1} isAnimationActive={false} activeDot={false} />
            <Line type="monotone" dataKey="feeAvg" connectNulls={false} stroke="var(--series-1)" strokeWidth={2} dot={false} isAnimationActive={false} />
            <Line type="stepAfter" dataKey="floor" connectNulls={false} stroke={FLOOR_COLOR} strokeWidth={1.5} strokeDasharray="4 3" dot={false} isAnimationActive={false} />
            {data.markers.map((m) => (
              <ReferenceLine key={`${m.t}-${m.action.txHash}`} x={m.t} stroke={MARKER_COLOR} strokeWidth={1} strokeDasharray="2 3" />
            ))}
          </ComposedChart>
        </ResponsiveContainer>
      </ChartBox>
      <GapNote gaps={data.gaps} />
    </>
  );
});

/** The throughput line: the first series colour, the same one the bucketed view draws its rate in. */
const THROUGHPUT_COLOR = "var(--series-1)";

/** The height the throughput chart stands at under the hero's base fee chart; the enlarged view passes its own. */
export const HERO_THROUGHPUT_HEIGHT = "h-[120px] lg:h-[140px]";

/** What a hovered second on the live throughput chart says: the gas that second carried, in how many blocks, and when it was. */
export function throughputTooltipRows(): TooltipRow[] {
  return [
    // A second the ring has a hole across carries no measurement at all, and
    // "0 gas/s" is a different claim from "nobody can say".
    { label: "compute gas in the second", color: THROUGHPUT_COLOR, value: (r) => (typeof r.gas === "number" ? formatGasPerSecond(r.gas) : "n/a") },
    { label: "blocks", value: (r) => (typeof r.blocks === "number" ? formatInteger(r.blocks) : "n/a") },
    { label: "time", value: (r) => formatTime(Number(r.ts)) },
  ];
}

/** The peak of a live throughput series, treating an unmeasured second as nothing. */
export function throughputPeak(points: readonly ThroughputPoint[]): number {
  return Math.max(0, ...points.map((p) => p.gas ?? 0));
}

/** What names a second on the live charts, for the inspector and the data table. */
const livePointTitle = (row: Record<string, unknown>) => heroPointTitle(Number(row.x));

/**
 * Compute gas per second from the block ring, on the same clock-anchored axis as the
 * base fee above it: each whole second's blocks summed and placed at the
 * second's end. The y axis carries one unit for the whole scale, named in the
 * caption, so its labels are bare figures of the same width and the plot
 * never shifts sideways. Memoised on the points, which move with the frame
 * clock.
 */
export const HeroThroughputChart = memo(function HeroThroughputChart({ points, height = HERO_THROUGHPUT_HEIGHT }: { points: ThroughputPoint[]; height?: string }) {
  const span = heroSpan();
  const ticks = useMemo(() => heroTicks(span), [span]);
  const axis = useMemo(() => throughputAxis(throughputPeak(points)), [points]);
  // Seconds the ring can speak for. A null second is drawn as a break, so it
  // is not one of the seconds the description counts.
  const measured = points.filter((p) => p.gas !== null).length;
  const waitingForBlocks = points.length < 2;
  const label = waitingForBlocks
    ? "Compute gas per second, waiting for blocks"
    : measured < 2
      ? "Compute gas per second, receipt data unavailable"
      : `Compute gas carried per second over the last ${span} seconds, ${measured} seconds of blocks, up to ${formatGasPerSecond(throughputPeak(points))}`;
  return (
    <ChartBox label={label} busy={waitingForBlocks} height={height}>
      {measured < 2 ? (
        <ChartNote>{waitingForBlocks ? "Waiting for blocks." : "Receipt data unavailable."}</ChartNote>
      ) : (
        <ResponsiveContainer width="100%" height="100%">
          <AreaChart data={points} margin={{ top: 8, right: TIME_AXIS_RIGHT, bottom: 2, left: 0 }}>
            <CartesianGrid vertical={false} />
            <XAxis dataKey="x" type="number" domain={[-span, 0]} ticks={ticks} tickFormatter={heroTimeLabel} tickLine axisLine={false} height={18} />
            <YAxis domain={[0, axis.top]} ticks={axis.ticks} tickFormatter={(v: number) => throughputTick(v, axis)} tickLine={false} axisLine={false} width={HERO_AXIS_WIDTH} />
            <Tooltip isAnimationActive={false} content={(props) => <ChartTooltip {...props} title={heroPointTitle} rows={throughputTooltipRows()} />} />
            <Area type="monotone" dataKey="gas" stroke={THROUGHPUT_COLOR} strokeWidth={1.5} fill={THROUGHPUT_COLOR} fillOpacity={0.12} dot={false} activeDot={{ r: 2.5 }} isAnimationActive={false} />
          </AreaChart>
        </ResponsiveContainer>
      )}
    </ChartBox>
  );
});

/**
 * The throughput chart under the hero's base fee, at the hero's own range:
 * compute gas per second from the block ring on Live, and the same figure per bucket
 * against every constraint target in force on a history range. The bucketed
 * view is the history chart itself, not a second implementation of it.
 */
export const HeroThroughputPanel = memo(function HeroThroughputPanel({
  blocks,
  places,
  nowMs,
  range,
  series,
  seriesLoading = false,
  seriesError = null,
  model = "unknown",
  height = HERO_THROUGHPUT_HEIGHT,
  minWidth = 280,
  readout = false,
  action,
}: {
  blocks: BlockPoint[];
  /** The ring's own placement, so this chart puts a block exactly where the fee chart above it does. */
  places?: BlockPlaces;
  nowMs: number;
  range: HeroRange;
  series: Series | null;
  seriesLoading?: boolean;
  seriesError?: string | null;
  model?: PricerModel;
  height?: string;
  minWidth?: number;
  /**
   * Whether to render the keyboard inspector and the data table under the
   * chart. Off in the hero, which stays a chart and its figures; on where
   * the chart is enlarged and there is room to read it point by point.
   */
  readout?: boolean;
  /** The control the caption row carries, the enlarge link on the network page and nothing on the chart's own page. */
  action?: ReactNode;
}) {
  const live = range === "live";
  const placed = usePlaces(blocks, places);
  const points = useMemo(() => (live ? heroThroughputData(blocks, placed, nowMs) : []), [live, blocks, placed, nowMs]);
  const m = useMemo(() => (!live && series ? buildSeriesModel(series, model) : null), [live, series, model]);
  const rangeLabel = HERO_RANGE_LABELS[range];
  const liveUnit = useMemo(() => throughputAxis(throughputPeak(points)).unit, [points]);
  const unit = live ? liveUnit : (m?.gasAxis.unit ?? "Mgas/s");
  // The plain reading first, the measurement after it: what the chart shows a
  // reader who has never met the pricer, then the unit and the span for one who has.
  const caption = live
    ? { lead: `Network load, second by second, over the last ${heroSpan()} s`, detail: `compute gas per second across the chain \u00b7 ${unit}` }
    : { lead: `Network load per bucket against each target in force, ${rangeLabel}`, detail: `compute gas per second \u00b7 ${unit}` };
  return (
    <>
        <div className="flex flex-wrap items-center justify-between gap-2">
          <Caption lead={caption.lead} detail={caption.detail} />
          {action}
        </div>
        {live ? (
          <HeroThroughputChart points={points} height={height} />
        ) : seriesError !== null && series === null ? (
          <ChartBox label={`Compute gas per second over ${rangeLabel}, unavailable`} height={height}>
            <ChartNote>Could not load {rangeLabel}: {seriesError}</ChartNote>
          </ChartBox>
        ) : m === null ? (
          <ChartBox label={`Compute gas per second over ${rangeLabel}, loading`} busy height={height}>
            <ChartNote>{seriesLoading ? `Loading ${rangeLabel}.` : `No history for ${rangeLabel} yet.`}</ChartNote>
          </ChartBox>
        ) : m.points.length === 0 ? (
          <ChartBox label={`Compute gas per second over ${rangeLabel}, nothing indexed`} height={height}>
            <ChartNote>{emptyRangeNote(m.gaps.first)}</ChartNote>
          </ChartBox>
        ) : (
          <GasPerSecondChart m={m} height={height} axisWidth={HERO_AXIS_WIDTH} minWidth={minWidth} />
        )}
        {!readout ? null : live ? (
          <ChartReadout
            points={points}
            groups={THROUGHPUT_READOUT}
            title={livePointTitle}
            heading="Second inspector"
            selectLabel="Select a second to read its values"
            caption="Every whole second of the live throughput chart with its compute gas and block count"
            summary="Compute gas per second, as a table"
            timeLabel="when"
          />
        ) : m !== null && m.points.length > 0 ? (
          <ChartReadout
            points={m.points}
            groups={[{ title: "gas", rows: m.gasRows }]}
            note={m.gasNote}
            title={bucketRowTitle}
            heading="Bucket inspector"
            selectLabel="Select a bucket to read its values"
            caption={`Every bucket of the throughput chart over ${rangeLabel} with its rate and the targets in force`}
            summary={`Compute gas per second over ${rangeLabel}, as a table`}
            timeLabel="bucket"
          />
        ) : null}
    </>
  );
});

/** The rail's six supporting figures are set as instrument readouts: flat on the card in light, inset
 * and lit in dark, which is where the two designs part company. */
const STAT_TONE: StatTone = "readout";

/** A band head in the rail, on the chrome rule the rest of the site breaks sections with. */
function RailBand({ children }: { children: ReactNode }) {
  return (
    <div className="flex items-center gap-3 font-display text-[11px] font-medium uppercase tracking-[0.16em] text-ink-3">
      {children}
      <span className="h-px flex-1" style={{ background: "var(--chrome-rule)" }} aria-hidden="true" />
    </div>
  );
}

/** What the live throughput chart reads out without a pointer. */
const THROUGHPUT_READOUT: ReadoutGroup[] = [{ title: "second", rows: throughputTooltipRows() }];

/**
 * A chart's caption: the plain reading, then the measurement behind it in
 * the quieter ink. Both are one paragraph, so a reader who selects the
 * caption gets the whole of it.
 */
function Caption({ lead, detail }: { lead: string; detail: string }) {
  return (
    <p className="text-xs text-ink-3">
      <span className="text-ink-2">{lead}</span>
      <span> · {detail}</span>
    </p>
  );
}

/** The plain words the hero labels its figures with, each with the precise term a hover away. */
export const HERO_TERMS = {
  baseFee: { label: "Base fee now", lines: ["the price of one unit of gas right now, in gwei", "a gwei is a billionth of an ETH; every transaction pays this much per unit of gas it uses, and there are no tips"] },
  load10: { label: "Network load (10 s)", lines: ["compute gas per second, averaged over the last 10 s", "the gas the chain carried net of L1 poster gas, which is the rate the pricer meters"] },
  load60: { label: "Network load (60 s)", lines: ["compute gas per second, averaged over the last 60 s", "the same rate over a longer window, so a burst and a trend read apart"] },
  send: { label: "Send", lines: ["a 21,000 gas transfer at the base fee now", "the gas a plain ETH transfer uses, so the smallest transaction there is"] },
  swap: { label: "Swap", lines: ["a 150,000 gas swap at the base fee now", "about what a token swap on a DEX uses"] },
} as const;

/**
 * "Since last block" is the chain's own cadence. Once the sample itself is
 * older than COLLECTOR_LAG_S the number would mostly measure the collector,
 * so the stat becomes a warning pill that names the lag instead.
 */
function Freshness({ sinceBlock, age, tone }: { sinceBlock: number; age: number; tone?: StatTone }) {
  if (age <= COLLECTOR_LAG_S) return <Stat label="Since last block" value={<Figure ch={4}>{sinceBlock.toFixed(1)}</Figure>} unit="s" tone={tone} />;
  return (
    <div className={`min-w-0 ${tone === "readout" ? "vw-stat-panel" : ""}`} aria-live="polite">
      <Label>Since last block</Label>
      <div className="mt-1">
        <span className="num inline-flex items-center gap-1.5 rounded-full border border-warning px-2.5 py-1 text-xs font-medium text-warning" title={`The collector's last sample is ${Math.floor(age)} s old; the chain may well be producing blocks`}>
          <span className="inline-block h-2 w-2 rounded-full bg-warning" aria-hidden="true" />
          collector lagging {Math.floor(age)} s
        </span>
      </div>
    </div>
  );
}

/**
 * A gas rate tile: the figure in a reserved box and the unit beside it, so the
 * SI prefix rides on the unit ("Mgas/s") and the number never carries a letter.
 */
export function GasRateTile({ label, gasPerSecond, tone }: { label: ReactNode; gasPerSecond: number | null; tone?: StatTone }) {
  if (gasPerSecond === null) return <Stat label={label} value="n/a" tone={tone} />;
  const parts = gasPerSecondParts(gasPerSecond, true);
  return <Stat label={label} value={<Figure ch={FIXED_WIDTH_CH.gasPerSecond}>{parts.value}</Figure>} unit={parts.unit} tone={tone} />;
}

/**
 * What a transaction of a given size costs, in dollars when the collector has a fresh quote and in ETH
 * when it does not. The multiplication behind it, the quote and its age stay a hover away and are always
 * in the accessible description. `align` is which way the note opens: the right-hand tile of a row has to
 * open leftwards to stay inside the card.
 */
export function CostTile({ label, eth, ethUsd, nowMs, align, tone }: { label: ReactNode; eth: number; ethUsd: EthUsd | null; nowMs: number; align?: NoteAlign; tone?: StatTone }) {
  const math = usdMath(eth, ethUsd, nowMs);
  if (math === null) {
    return <Stat label={label} value={<Figure ch={FIXED_WIDTH_CH.eth}>{formatEthFixed(eth)}</Figure>} unit="ETH" size="sm" tone={tone} />;
  }
  return (
    <Stat
      label={label}
      size="sm"
      tone={tone}
      value={
        <HoverNote lines={[math.line, math.provenance]} description={math.description} align={align}>
          {/* The dollar sign sits outside the reserved box, so a changing digit never shifts it. */}
          <span className="text-ink-2">$</span>
          <Figure ch={FIXED_WIDTH_CH.usd}>{math.usd}</Figure>
        </HoverNote>
      }
    />
  );
}

/**
 * The chart column of the hero: the caption and the chart the chosen range
 * calls for, in one frame at every one of them. The detail route draws the
 * same thing at its own height, so the enlarged base fee is this component
 * and not a second implementation of it.
 */
export const HeroChartPanel = memo(function HeroChartPanel({
  snapshot,
  blocks,
  places,
  nowMs,
  range,
  series,
  seriesLoading = false,
  seriesError = null,
  model = "unknown",
  height,
  readout = false,
}: {
  snapshot: LiveSnapshot;
  blocks: BlockPoint[];
  /** The ring's own placement, assigned once per block; see lib/smoothing. */
  places?: BlockPlaces;
  nowMs: number;
  range: HeroRange;
  series: Series | null;
  seriesLoading?: boolean;
  seriesError?: string | null;
  model?: PricerModel;
  height?: string;
  /** See HeroThroughputPanel: the inspector belongs to the enlarged view. */
  readout?: boolean;
}) {
  const live = range === "live";
  // Against the frame's wall clock: the chart slides every frame, and a block
  // keeps its place. Built only on Live: rebuilding fifteen hundred relative
  // positions every frame for a chart drawing buckets is work nobody sees.
  const placed = usePlaces(blocks, places);
  const points = useMemo(() => (live ? heroChartData(blocks, placed, nowMs) : []), [live, blocks, placed, nowMs]);
  const data = useMemo(() => feeChartData(live ? null : series, model), [live, series, model]);
  const floorGwei = weiToGweiNumber(snapshot.minBaseFee);
  const rangeLabel = HERO_RANGE_LABELS[range];
  const note = useMemo(() => bucketNote(data.markers, data.bucketSeconds), [data.markers, data.bucketSeconds]);
  return (
    <>
        {live ? (
          <Caption lead={`Base fee, block by block, over the last ${heroSpan()} s`} detail={`${formatInteger(points.length)} blocks \u00b7 ${formatGasFixed(snapshot.block.gasUsed)} in block ${formatInteger(snapshot.block.number)}`} />
        ) : (
          <p className="text-xs text-ink-3">{feeChartCaption(rangeLabel, data.points)}</p>
        )}
        {live ? (
          <HeroChart points={points} floorGwei={floorGwei} floorText={formatGwei(snapshot.minBaseFee)} height={height} />
        ) : seriesError !== null && series === null ? (
          <ChartBox label={`Base fee over ${rangeLabel}, unavailable`} height={height}>
            <ChartNote>Could not load {rangeLabel}: {seriesError}</ChartNote>
          </ChartBox>
        ) : series === null ? (
          <ChartBox label={`Base fee over ${rangeLabel}, loading`} busy height={height}>
            <ChartNote>{seriesLoading ? `Loading ${rangeLabel}.` : `No history for ${rangeLabel} yet.`}</ChartNote>
          </ChartBox>
        ) : data.points.length === 0 ? (
          <ChartBox label={feeChartLabel(rangeLabel, data.points)} height={height}>
            <ChartNote>{emptyRangeNote(data.gaps.first)}</ChartNote>
          </ChartBox>
        ) : (
          <HeroHistoryChart data={data} rangeLabel={rangeLabel} height={height} />
        )}
        {/* The same values without a pointer: one block or bucket at a time in
            the inspector, all of them in the table. */}
        {!readout ? null : live ? (
          <ChartReadout
            points={points}
            groups={FEE_READOUT}
            title={livePointTitle}
            heading="Block inspector"
            selectLabel="Select a block to read its values"
            caption="Every block of the live base fee chart with its fee, the gas it carried and its time"
            summary="Base fee per block, as a table"
            timeLabel="when"
          />
        ) : (
          <ChartReadout
            points={data.points}
            groups={BUCKET_FEE_READOUT}
            note={note}
            title={bucketRowTitle}
            heading="Bucket inspector"
            selectLabel="Select a bucket to read its values"
            caption={`Every bucket of the base fee chart over ${rangeLabel} with its average, band, floor and block count`}
            summary={`Base fee over ${rangeLabel}, as a table`}
            timeLabel="bucket"
          />
        )}
    </>
  );
});

/** What the live base fee chart reads out without a pointer. */
const FEE_READOUT: ReadoutGroup[] = [{ title: "block", rows: heroTooltipRows() }];

/** The same for a bucketed range. */
const BUCKET_FEE_READOUT: ReadoutGroup[] = [{ title: "bucket", rows: feeTooltipRows() }];

/** Copy for an empty hero: a reorg took the last state away, or nothing has arrived yet. */
export const RESYNC_COPY = "Resyncing after a reorg.";
export const WAITING_COPY = "Waiting for the first sample.";

/**
 * The live hero, subscribed to the frame store so its figures move every frame
 * while the page around it does not. The chart's range is this browser's own
 * choice: only the chart body follows it, so the socket, the smoothing and
 * every figure on the left keep running at every range.
 */
export function LiveHero({ network, live, status, model }: { network: string; live: SmoothedLive; status: LiveStatus; model: PricerModel }) {
  const frame = useLiveFrame(live.frame);
  // The range is an external store, so the server renders Live and hydration
  // has nothing to reconcile; the stored choice arrives on the next render.
  const range = useSyncExternalStore(subscribeHeroRange, heroRange, heroRangeOnServer);
  const historical = range === "live" ? null : range;
  const series = useSeries(historical === null ? null : network, historical);
  return (
    <LiveHeroView
      network={network}
      snapshot={live.display}
      values={frame.values}
      blocks={frame.blocks}
      places={frame.places}
      nowMs={frame.nowMs}
      status={status}
      resyncing={live.resyncing}
      range={range}
      onRangeChange={setHeroRange}
      series={series.data}
      seriesLoading={series.loading}
      seriesError={series.error}
      model={model}
    />
  );
}

/**
 * The hero with everything it shows as plain props. `snapshot` is the display snapshot, so block number,
 * gas and freshness follow it; `values` are the eased figures, with the sample standing in until the
 * first frame. With no snapshot the hero says which kind of nothing it is.
 */
export function LiveHeroView({
  network,
  snapshot,
  values,
  blocks,
  places,
  nowMs,
  status,
  resyncing = false,
  range = "live",
  onRangeChange,
  series = null,
  seriesLoading = false,
  seriesError = null,
  model = "unknown",
}: {
  network: string;
  snapshot: LiveSnapshot | null;
  values: LiveValues | null;
  blocks: BlockPoint[];
  /** The ring's own placement, so every live chart draws a block in the same spot. */
  places?: BlockPlaces;
  nowMs: number;
  status: LiveStatus;
  resyncing?: boolean;
  range?: HeroRange;
  onRangeChange?: (range: HeroRange) => void;
  series?: Series | null;
  seriesLoading?: boolean;
  seriesError?: string | null;
  model?: PricerModel;
}) {
  // Built here, and before the early return, so the throughput panel's memo
  // holds across the frames: a fresh element every render would defeat it.
  const throughputAction = useMemo(
    () => <EnlargeLink network={network} view={chartView("gas-per-second")} range={range} size="hero" />,
    [network, range],
  );
  if (!snapshot) {
    return (
      <div className="vw-card p-5 text-sm text-ink-2" aria-busy="true">
        <div className="flex items-center justify-between">
          <span>{resyncing ? RESYNC_COPY : WAITING_COPY}</span>
          <StatusPill status={status} />
        </div>
      </div>
    );
  }
  const v = values ?? targetValues(snapshot, blocks, 0);
  const sinceBlock = Math.max(0, nowMs / 1000 - snapshot.block.ts);
  const age = sampleAge(snapshot.sampledAt, nowMs);
  const floorText = formatGwei(snapshot.minBaseFee);
  return (
    <div className="vw-card vw-lit p-5">
      <div className="grid grid-cols-1 gap-6 lg:grid-cols-12">
        <div className="flex flex-col gap-4 lg:col-span-4">
          {/* The gauge carries the figure now: the arc is the instrument, the readout under it is what
              the instrument reads, and nothing but the needle is drawn inside the arc. */}
          <FeeGauge baseFeeGwei={v.baseFeeGwei} floorGwei={floorText} multiplier={v.multiplier} exponent={v.exponent} />

          <RailBand>Chain</RailBand>
          <div className="grid grid-cols-2 gap-x-4 gap-y-5">
            <Stat label="Block" value={formatInteger(snapshot.block.number)} tone={STAT_TONE} />
            <Freshness sinceBlock={sinceBlock} age={age} tone={STAT_TONE} />
            <GasRateTile label={<Term lines={HERO_TERMS.load10.lines}>{HERO_TERMS.load10.label}</Term>} gasPerSecond={v.gasPerSecond10} tone={STAT_TONE} />
            <GasRateTile label={<Term lines={HERO_TERMS.load60.lines} align="end">{HERO_TERMS.load60.label}</Term>} gasPerSecond={v.gasPerSecond60} tone={STAT_TONE} />
          </div>

          <RailBand>What it costs</RailBand>
          <div className="grid grid-cols-2 gap-x-4 gap-y-5">
            {/* No quote, or one older than ten minutes: the tiles read in ETH, as they did before there was a price at all. */}
            <CostTile label={<Term lines={HERO_TERMS.send.lines}>{HERO_TERMS.send.label}</Term>} eth={v.transferEth} ethUsd={snapshot.ethUsd} nowMs={nowMs} tone={STAT_TONE} />
            <CostTile label={<Term lines={HERO_TERMS.swap.lines} align="end">{HERO_TERMS.swap.label}</Term>} eth={v.swapEth} ethUsd={snapshot.ethUsd} nowMs={nowMs} align="end" tone={STAT_TONE} />
          </div>
        </div>

        <div className="flex flex-col gap-3 lg:col-span-8">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <RangeTabs options={HERO_RANGE_OPTIONS} value={range} onChange={onRangeChange ?? (() => undefined)} label="Base fee chart range" loading={range !== "live" && seriesLoading && series !== null} />
            <div className="flex items-center gap-2">
              <StatusPill status={status} />
              <EnlargeLink network={network} view={chartView("base-fee")} range={range} size="hero" />
            </div>
          </div>
          <HeroChartPanel snapshot={snapshot} blocks={blocks} places={places} nowMs={nowMs} range={range} series={series} seriesLoading={seriesLoading} seriesError={seriesError} model={model} />
          {/* What the chain carried, under what it charged for it, on the same
              range and the same axis: the two questions are one question. */}
          <HeroThroughputPanel
            blocks={blocks}
            places={places}
            nowMs={nowMs}
            range={range}
            series={series}
            seriesLoading={seriesLoading}
            seriesError={seriesError}
            model={model}
            action={throughputAction}
          />
        </div>
      </div>
    </div>
  );
}
