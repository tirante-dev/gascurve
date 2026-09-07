"use client";

import { memo, useMemo, useState, useSyncExternalStore, type ReactNode } from "react";
import { Area, AreaChart, CartesianGrid, ComposedChart, Line, ReferenceLine, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { useLiveFrame, type SmoothedLive } from "@/hooks/useSmoothedLive";
import { useSeries } from "@/hooks/useSeries";
import { feeChartCaption, feeChartData, feeChartLabel, feeTooltipRows, ownerActionNote, type FeeChartData } from "@/lib/feeChart";
import { heroChartData, heroFeeAxis, heroPointTitle, stableFeeAxis, heroRange, heroRangeOnServer, heroSpan, heroTicks, heroTimeLabel, setHeroRange, subscribeHeroRange, HERO_RANGE_LABELS, HERO_RANGES, type HeroPoint, type HeroRange } from "@/lib/hero";
import { SWAP_GAS, targetValues, TRANSFER_GAS, type LiveValues } from "@/lib/smoothing";
import type { BlockPoint, LiveSnapshot, LiveStatus, PricerModel, Series } from "@/types";
import { FLOOR_COLOR, MARKER_COLOR, rampColor, rampInk, rampStep } from "@/utils/chart";
import {
  FIXED_WIDTH_CH,
  formatDateTime,
  formatEthFixed,
  formatGas,
  formatGasFixed,
  formatGwei,
  formatGweiFixed,
  formatInteger,
  formatMultiplierFixed,
  formatPercent,
  formatSignificant,
  formatTick,
  formatTime,
  formatUsdFixed,
  freshUsdPrice,
  gasPerSecondParts,
  weiToGweiNumber,
} from "@/utils/format";
import { chartView } from "@/lib/chartViews";
import { ChartTooltip, type TooltipRow } from "./ChartTooltip";
import { EnlargeLink } from "./ChartActions";
import { Figure, Label, Stat, StatusPill } from "./primitives";
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
          <AreaChart data={points} margin={{ top: 8, right: 10, bottom: 2, left: 0 }}>
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
  const note = useMemo(() => ownerActionNote(data.markers, data.bucketSeconds), [data.markers, data.bucketSeconds]);
  return (
    <ChartBox label={feeChartLabel(rangeLabel, data.points)} height={height}>
      <ResponsiveContainer width="100%" height="100%">
        <ComposedChart data={data.drawn} margin={{ top: 8, right: 10, bottom: 2, left: 0 }}>
          <CartesianGrid vertical={false} />
          <XAxis dataKey="t" type="number" domain={["dataMin", "dataMax"]} tickFormatter={(t: number) => formatTick(t, data.span)} tickLine axisLine={false} height={18} minTickGap={48} />
          <YAxis scale="log" domain={data.domain} tickFormatter={(v: number) => formatSignificant(v, 2)} tickLine={false} axisLine={false} width={HERO_AXIS_WIDTH} />
          <Tooltip isAnimationActive={false} content={(props) => <ChartTooltip {...props} title={(t) => formatDateTime(t)} rows={rows} note={note} />} />
          <Area type="monotone" dataKey="feeMax" stroke="none" fill="var(--series-1)" fillOpacity={0.12} isAnimationActive={false} activeDot={false} />
          <Area type="monotone" dataKey="feeMin" stroke="none" fill="var(--chart)" fillOpacity={1} isAnimationActive={false} activeDot={false} />
          <Line type="monotone" dataKey="feeAvg" stroke="var(--series-1)" strokeWidth={2} dot={false} isAnimationActive={false} />
          <Line type="stepAfter" dataKey="floor" stroke={FLOOR_COLOR} strokeWidth={1.5} strokeDasharray="4 3" dot={false} isAnimationActive={false} />
          {data.markers.map((m) => (
            <ReferenceLine key={`${m.t}-${m.action.txHash}`} x={m.t} stroke={MARKER_COLOR} strokeWidth={1} strokeDasharray="2 3" />
          ))}
        </ComposedChart>
      </ResponsiveContainer>
    </ChartBox>
  );
});

/** Where a unit of gas's fee goes: the floor to the infra account, the rest to the network account. Memoised on the snapshot. */
export const FeeSplitBar = memo(function FeeSplitBar({ snapshot }: { snapshot: LiveSnapshot }) {
  const base = BigInt(snapshot.prices.perArbGasBase);
  const congestion = BigInt(snapshot.prices.perArbGasCongestion);
  const total = base + congestion;
  const floorShare = total > 0n ? Number((base * 10_000n) / total) / 10_000 : 1;
  const congestionShare = 1 - floorShare;
  return (
    <div>
      <Label>Where the fee goes</Label>
      <div className="mt-2 flex h-3 w-full gap-[2px] overflow-hidden rounded-sm" role="img" aria-label={`Floor ${formatPercent(floorShare)} to the infra account, congestion ${formatPercent(congestionShare)} to the network account`}>
        <div style={{ width: `${Math.max(1, floorShare * 100)}%`, background: "var(--seq-2)" }} />
        <div style={{ width: `${Math.max(0, congestionShare * 100)}%`, background: "var(--seq-8)" }} />
      </div>
      <dl className="mt-2 grid grid-cols-2 gap-x-3 text-xs text-ink-2">
        <div>
          <dt className="flex items-center gap-1.5">
            <span className="inline-block h-2.5 w-2.5 rounded-[2px]" style={{ background: "var(--seq-2)" }} aria-hidden="true" />
            floor to infra
          </dt>
          <dd className="num mt-0.5 text-ink">
            {formatGwei(snapshot.prices.perArbGasBase)} gwei · {formatPercent(floorShare, 0)}
          </dd>
        </div>
        <div>
          <dt className="flex items-center gap-1.5">
            <span className="inline-block h-2.5 w-2.5 rounded-[2px]" style={{ background: "var(--seq-8)" }} aria-hidden="true" />
            congestion to network
          </dt>
          <dd className="num mt-0.5 text-ink">
            {formatGwei(snapshot.prices.perArbGasCongestion)} gwei · {formatPercent(congestionShare, 0)}
          </dd>
        </div>
      </dl>
    </div>
  );
});

/**
 * "Since last block" is the chain's own cadence. Once the sample itself is
 * older than COLLECTOR_LAG_S the number would mostly measure the collector,
 * so the stat becomes a warning pill that names the lag instead.
 */
function Freshness({ sinceBlock, age }: { sinceBlock: number; age: number }) {
  if (age <= COLLECTOR_LAG_S) return <Stat label="Since last block" value={<Figure ch={4}>{sinceBlock.toFixed(1)}</Figure>} unit="s" />;
  return (
    <div className="min-w-0" aria-live="polite">
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
export function GasRateTile({ label, gasPerSecond }: { label: string; gasPerSecond: number }) {
  const parts = gasPerSecondParts(gasPerSecond, true);
  return <Stat label={label} value={<Figure ch={FIXED_WIDTH_CH.gasPerSecond}>{parts.value}</Figure>} unit={parts.unit} />;
}

/**
 * What a transaction of a given size costs, in dollars when the collector has
 * a fresh quote and in ETH when it does not. The dollar figure is the primary
 * one because it is the one people hold in their heads; the ETH amount stays
 * a hover away and is always in the accessible description, so nothing is
 * only available to a pointer.
 */
export function CostTile({ label, eth, usdPerEth }: { label: string; eth: number; usdPerEth: number | null }) {
  if (usdPerEth === null) {
    return <Stat label={label} value={<Figure ch={FIXED_WIDTH_CH.eth}>{formatEthFixed(eth)}</Figure>} unit="ETH" size="sm" />;
  }
  const ethText = `${formatEthFixed(eth)} ETH`;
  const usd = formatUsdFixed(eth * usdPerEth);
  return (
    <Stat
      label={label}
      size="sm"
      value={
        <span title={ethText}>
          {/* The dollar sign sits outside the reserved box, so a changing digit never shifts it. */}
          <span aria-hidden="true">
            <span className="text-ink-2">$</span>
            <Figure ch={FIXED_WIDTH_CH.usd}>{usd}</Figure>
          </span>
          <span className="sr-only">{`${usd} US dollars, ${ethText}, at ${formatUsdFixed(usdPerEth)} dollars per ETH`}</span>
        </span>
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
export function HeroChartPanel({
  snapshot,
  blocks,
  nowMs,
  range,
  series,
  seriesLoading = false,
  seriesError = null,
  model = "unknown",
  height,
}: {
  snapshot: LiveSnapshot;
  blocks: BlockPoint[];
  nowMs: number;
  range: HeroRange;
  series: Series | null;
  seriesLoading?: boolean;
  seriesError?: string | null;
  model?: PricerModel;
  height?: string;
}) {
  // Against the frame's wall clock: the chart slides every frame, and a block keeps its place.
  const points = useMemo(() => heroChartData(blocks, nowMs), [blocks, nowMs]);
  const data = useMemo(() => feeChartData(range === "live" ? null : series, model), [range, series, model]);
  const floorGwei = weiToGweiNumber(snapshot.minBaseFee);
  const rangeLabel = HERO_RANGE_LABELS[range];
  const caption =
    range === "live"
      ? `Base fee per block \u00b7 last ${points.length} blocks \u00b7 ${formatGasFixed(snapshot.block.gasUsed)} in block ${formatInteger(snapshot.block.number)}`
      : feeChartCaption(rangeLabel, data.points);
  return (
    <>
      <p className="text-xs text-ink-3">{caption}</p>
      {range === "live" ? (
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
          <ChartNote>No buckets in {rangeLabel} yet.</ChartNote>
        </ChartBox>
      ) : (
        <HeroHistoryChart data={data} rangeLabel={rangeLabel} height={height} />
      )}
    </>
  );
}

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
 * The hero with everything it shows as plain props: the figures on the left,
 * the chosen range of base fee on the right. `snapshot` is the display
 * snapshot (the render cadence), so block number, gas in the block and the
 * freshness follow it; `values` are the eased figures, with the sample
 * standing in until the first frame has produced them. With no snapshot the
 * hero says which kind of nothing it is: a reorg that took the last canonical
 * state away, or a feed that has not delivered one yet.
 */
export function LiveHeroView({
  network,
  snapshot,
  values,
  blocks,
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
  const multiplierBips = v.multiplier * 10_000;
  const step = rampStep(multiplierBips);
  const sinceBlock = Math.max(0, nowMs / 1000 - snapshot.block.ts);
  const age = sampleAge(snapshot.sampledAt, nowMs);
  // No quote, or one older than ten minutes: the tiles read in ETH, as they did before there was a price at all.
  const usdPerEth = freshUsdPrice(snapshot.ethUsd, nowMs);
  return (
    <div className="vw-card p-5">
      <div className="grid grid-cols-1 gap-6 lg:grid-cols-12">
        <div className="flex flex-col gap-4 lg:col-span-4">
          <div className="flex flex-wrap items-end gap-x-6 gap-y-3">
            <div>
              <Label>Base fee now</Label>
              <div className="num mt-1 text-4xl leading-none tracking-tight text-ink sm:text-5xl">
                <Figure ch={FIXED_WIDTH_CH.gwei} className="vw-hero">
                  {formatGweiFixed(v.baseFeeGwei)}
                </Figure>
                <span className="ml-1.5 text-lg font-normal text-ink-2">gwei</span>
              </div>
            </div>
            <div
              className="num vw-tile rounded-md px-3 py-2 text-2xl leading-none"
              style={{ background: rampColor(multiplierBips), color: rampInk(step) }}
              title={`Multiplier over the ${formatGwei(snapshot.minBaseFee)} gwei floor`}
            >
              <Figure ch={FIXED_WIDTH_CH.multiplier}>{formatMultiplierFixed(v.multiplier)}</Figure>×
              <div className="mt-1 text-[11px] font-medium uppercase tracking-[0.08em] opacity-80">over floor</div>
            </div>
          </div>
          <div className="num text-xs text-ink-3">
            floor {formatGwei(snapshot.minBaseFee)} gwei · x <Figure ch={FIXED_WIDTH_CH.x}>{v.exponent.toFixed(4)}</Figure>
          </div>

          <div className="grid grid-cols-2 gap-x-4 gap-y-5">
            <Stat label="Block" value={formatInteger(snapshot.block.number)} />
            <Freshness sinceBlock={sinceBlock} age={age} />
            <GasRateTile label="Gas/s (10 s)" gasPerSecond={v.gasPerSecond10} />
            <GasRateTile label="Gas/s (60 s)" gasPerSecond={v.gasPerSecond60} />
            <CostTile label="21k transfer" eth={v.transferEth} usdPerEth={usdPerEth} />
            <CostTile label="150k swap" eth={v.swapEth} usdPerEth={usdPerEth} />
          </div>

          <FeeSplitBar snapshot={snapshot} />
        </div>

        <div className="flex flex-col gap-3 lg:col-span-8">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <RangeTabs options={HERO_RANGE_OPTIONS} value={range} onChange={onRangeChange ?? (() => undefined)} label="Base fee chart range" loading={range !== "live" && seriesLoading && series !== null} />
            <div className="flex items-center gap-2">
              <StatusPill status={status} />
              <EnlargeLink network={network} view={chartView("base-fee")} range={range} size="hero" />
            </div>
          </div>
          <HeroChartPanel snapshot={snapshot} blocks={blocks} nowMs={nowMs} range={range} series={series} seriesLoading={seriesLoading} seriesError={seriesError} model={model} />
        </div>
      </div>
    </div>
  );
}
