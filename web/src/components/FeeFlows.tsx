"use client";

import { memo, useMemo, useState, type ReactNode } from "react";
import { useTicker } from "@/hooks/useTicker";
import { Area, AreaChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import type { EthUsd, LiveSnapshot, PricerModel, Series, SeriesCompleteness, SeriesRange } from "@/types";
import { buildChartPoints, spanSeconds, sumKnownWeiEth, sumWeiEth, UNKNOWN_COLOR, UNSPLIT_FEES_LABEL, type ChartPoint } from "@/utils/chart";
import { bucketSeconds, emptyRangeNote, gapModel, withGapBreaks, NO_GAPS, type GapModel } from "@/lib/gaps";
import { completenessOf, isPartialRow, partialBands, partialKinds, partialRowNote, withFeeStack } from "@/lib/partial";
import { formatDateTime, formatEth, formatInteger, formatPercent, formatSignificant, formatTick, shortAddress, usdMath } from "@/utils/format";
import { chartView } from "@/lib/chartViews";
import { formatFloor } from "@/lib/feeChart";
import { EnlargeLink } from "./ChartActions";
import { ChartTooltip, type TooltipRow } from "./ChartTooltip";
import { GapBands, GapNote, PartialBands, PartialHatch, PartialNote } from "./ChartGaps";
import { Card, ChartFrame, HatchPattern, HoverNote, Label, Legend, Stat, TIME_AXIS_RIGHT, type ChartHeight, type NoteAlign } from "./primitives";

const FLOOR_FILL = "var(--seq-2)";
const SURPLUS_FILL = "var(--seq-8)";
const POSTER_FILL = "var(--series-3)";
/**
 * The hatch that fills buckets whose destination split is unavailable:
 * muted ink, never a destination colour, drawn at full strength so the lines
 * clear 3:1 against the chart surface in both themes.
 */
const UNSPLIT_PATTERN_ID = "fee-unsplit-hatch";

/** "0.1234" for a known part, "n/a" for an unavailable split. */
function formatPart(eth: number | null): string {
  return eth === null ? "n/a" : formatSignificant(eth, 4);
}

/** A nullable ETH field of a chart row: never coerced to zero. */
function ethCell(value: unknown): string {
  return typeof value === "number" ? `${formatSignificant(value, 4)} ETH` : "n/a";
}

/** "3 buckets have fees without a recorded destination", singular when it is one. */
export function unsplitNote(count: number): string {
  return `${formatInteger(count)} ${count === 1 ? "bucket has" : "buckets have"} fees without a recorded destination`;
}

function AccountRow({ name, role, address, balance, explorerUrl }: { name: string; role: string; address: string; balance: string; explorerUrl?: string }) {
  const href = explorerUrl ? `${explorerUrl.replace(/\/+$/, "")}/address/${address}` : undefined;
  return (
    <div className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1 border-t border-hairline py-2 first:border-t-0">
      <div className="min-w-0">
        <div className="text-sm text-ink">{name}</div>
        <div className="text-xs text-ink-3">{role}</div>
        {href ? (
          <a className="num text-xs text-accent underline-offset-2 hover:underline" href={href} target="_blank" rel="noreferrer">
            {shortAddress(address)}
          </a>
        ) : (
          <span className="num text-xs text-ink-3">{shortAddress(address)}</span>
        )}
      </div>
      <div className="num text-base text-ink">{formatEth(balance)}</div>
    </div>
  );
}

/**
 * Fee sums over every indexed block in the range. The values remain useful as
 * lower bounds when coverage is partial. The daily rate uses complete buckets
 * and tolerates only the current bucket being in progress. Poster fees are
 * totaled independently from the historical compute split.
 */
export type FeeTotals = {
  total: number;
  floorEth: number;
  surplusEth: number;
  posterEth: number;
  perDay: number | null;
  rateCoverage: number | null;
  splitKnown: number;
  posterKnown: number;
  posterUnknown: number;
  unsplit: number;
  completeness: SeriesCompleteness;
  partialBuckets: number;
  unknownBuckets: number;
  emptyIntervals: number;
};

export function feeTotals(series: Pick<Series, "from" | "to" | "resolution" | "points">): FeeTotals {
  const total = sumWeiEth(series.points, "feesWei");
  const split = series.points.filter((p) => typeof p.floorFeesWei === "string" && typeof p.surplusFeesWei === "string");
  const floor = sumKnownWeiEth(split, "floorFeesWei");
  const surplus = sumKnownWeiEth(split, "surplusFeesWei");
  const poster = sumKnownWeiEth(series.points, "posterFeesWei");
  const gaps = gapModel(series, series.points);
  const width = bucketSeconds(series.resolution, series.points);
  const kinds = partialKinds(series.points, { to: series.to, step: width });
  const complete = series.points.filter((point) => completenessOf(point) === "complete");
  const completeSpan = series.resolution === "block" && complete.length > 0 ? complete[complete.length - 1].t - complete[0].t + 1 : complete.length * width;
  const requestedSpan = Math.max(0, series.to - series.from);
  const partialBuckets = series.points.filter((point) => completenessOf(point) === "partial").length;
  const unknownBuckets = series.points.filter((point) => completenessOf(point) === "unknown").length;
  const emptyIntervals = gaps.gaps.length;
  const completeness: SeriesCompleteness = partialBuckets > 0 || emptyIntervals > 0 ? "partial" : unknownBuckets > 0 ? "unknown" : "complete";
  const rateBlocked = emptyIntervals > 0 || unknownBuckets > 0 || kinds.some((kind) => kind === "leading" || kind === "unknown");
  const rateCoverage = requestedSpan > 0 && completeSpan > 0 ? Math.min(1, completeSpan / requestedSpan) : null;
  return {
    total,
    floorEth: floor.eth,
    surplusEth: surplus.eth,
    posterEth: poster.eth,
    perDay: !rateBlocked && completeSpan > 0 ? (sumWeiEth(complete, "feesWei") / completeSpan) * 86_400 : null,
    rateCoverage,
    splitKnown: split.length,
    posterKnown: series.points.length - poster.unknown,
    posterUnknown: poster.unknown,
    unsplit: series.points.length - split.length,
    completeness,
    partialBuckets,
    unknownBuckets,
    emptyIntervals,
  };
}

/** Why range totals are lower bounds, or null when the range is complete. */
export function incompleteTotalsNote(totals: FeeTotals): string | null {
  if (totals.completeness === "complete") return null;
  const reasons: string[] = [];
  if (totals.partialBuckets > 0) reasons.push(`${formatInteger(totals.partialBuckets)} partially indexed ${totals.partialBuckets === 1 ? "bucket" : "buckets"}`);
  if (totals.unknownBuckets > 0) reasons.push(`${formatInteger(totals.unknownBuckets)} ${totals.unknownBuckets === 1 ? "bucket has" : "buckets have"} unknown completeness`);
  if (totals.emptyIntervals > 0) reasons.push(`${formatInteger(totals.emptyIntervals)} ${totals.emptyIntervals === 1 ? "interval has" : "intervals have"} no indexed buckets`);
  const rate = totals.perDay === null ? "The daily run rate waits for a gap-free set of complete buckets." : `The daily run rate uses complete buckets only (${formatPercent(totals.rateCoverage ?? 0)} of the requested window).`;
  return `Indexed-block sums are lower bounds: ${reasons.join("; ")}. ${rate}`;
}

/** Which edge a stat's note opens from, at each of the two column counts the stats grid has. */
type UsdPlacement = { align: NoteAlign; alignSm: NoteAlign };

/**
 * Where each stat's note opens from. The stats are laid out `grid-cols-2
 * sm:grid-cols-5`, so a stat's column changes with the breakpoint and no one
 * edge keeps a note inside the card at both: narrow, the odd stats are the
 * right-hand column; wide, only the last two sit near the right edge.
 */
const USD_PLACEMENT: readonly UsdPlacement[] = [
  { align: "start", alignSm: "start" },
  { align: "end", alignSm: "start" },
  { align: "start", alignSm: "start" },
  { align: "end", alignSm: "end" },
  { align: "start", alignSm: "end" },
];

/**
 * The dollar line under an ETH total, hovering to the multiplication that
 * produced it and the quote it used, or nothing at all when there is no fresh
 * quote to convert with. `sig` is the significant digits the total above it is
 * drawn to, so the working quotes the figure beside it. `place` is the edge
 * the note opens from; see USD_PLACEMENT.
 */
function usdLine(eth: number, ethUsd: EthUsd | null | undefined, nowMs: number, sig: number, place: UsdPlacement): ReactNode {
  const math = usdMath(eth, ethUsd, nowMs, (v) => formatSignificant(v, sig));
  if (math === null) return undefined;
  return (
    <HoverNote lines={[math.line, math.provenance]} description={math.description} align={place.align} alignSm={place.alignSm}>
      ${math.usd}
    </HoverNote>
  );
}

/** The height the fee chart stands at on the network page; the enlarged view passes its own. */
export const FEE_CHART_HEIGHT = 160;

/** What a hovered bucket says: the fees in it, where they went, and the floor that split them. */
export function feeFlowRows(unsplit: boolean): TooltipRow[] {
  return [
    { label: "fees in bucket", value: (r) => `${formatSignificant(Number(r.feesEth), 4)} ETH`, when: (r) => !isPartialRow(r) },
    // A bucket the collector has not finished holds what it has collected so
    // far, which is a different fact from what the bucket will hold.
    { label: "fees so far", value: (r) => `${formatSignificant(Number(r.feesEth), 4)} ETH`, when: isPartialRow },
    { label: "floor to infra", color: FLOOR_FILL, kind: "rect", value: (r) => ethCell(r.floorFeesEth), when: (r) => typeof r.floorFeesEth === "number" },
    { label: "congestion to network", color: SURPLUS_FILL, kind: "rect", value: (r) => ethCell(r.surplusFeesEth), when: (r) => typeof r.surplusFeesEth === "number" },
    { label: "poster fee to L1 pricer", color: POSTER_FILL, kind: "rect", value: (r) => ethCell(r.posterFeesEth), when: (r) => typeof r.posterFeesEth === "number" },
    ...(unsplit ? [{ label: UNSPLIT_FEES_LABEL, color: UNKNOWN_COLOR, kind: "hatch" as const, value: (r: Record<string, unknown>) => ethCell(r.unsplitFeesEth), when: (r: Record<string, unknown>) => r.unsplitFeesEth !== null }] : []),
    { label: "floor in force", value: (r) => (typeof r.floor === "number" ? `${formatFloor(r.floor)} gwei` : "n/a") },
  ];
}

/**
 * Fees per bucket in ETH, stacked by destination. Buckets whose split
 * is unavailable are hatched rather than assigned to a destination.
 */
export const FeeFlowChart = memo(function FeeFlowChart({ points, gaps = NO_GAPS, height = FEE_CHART_HEIGHT }: { points: ChartPoint[]; gaps?: GapModel; height?: ChartHeight }) {
  // Recharts recomputes every selector and regenerates every stacked path when
  // the `data` identity changes, so the rows are derived once per series and
  // the window, bands and unsplit flag with them.
  const window = useMemo(
    () => (gaps.window.to > gaps.window.from ? gaps.window : { from: points[0]?.t ?? 0, to: (points[points.length - 1]?.t ?? 0) + gaps.step }),
    [gaps.window, gaps.step, points],
  );
  const span = window.to > window.from ? window.to - window.from : spanSeconds(points);
  const unsplit = useMemo(() => points.some((p) => p.unsplitFeesEth !== null), [points]);
  // The stack is a sum over the bucket, so a bucket the collector has only
  // part of would draw as a bucket that collected little. It is hatched
  // instead, and the stack ends at the last whole bucket.
  // Which partial bucket is the one still filling is decided by the window's
  // right edge, not by array position: a range with a trailing gap ends on a
  // bucket indexing stopped part way through, which is not in progress.
  const bands = useMemo(() => partialBands(points, gaps.step, window.to), [points, gaps.step, window.to]);
  // Empty rows inside the holes, so a bucket that was never indexed breaks the
  // stack rather than reading as a bucket that collected nothing.
  const rows = useMemo(() => withGapBreaks(withFeeStack(points), gaps.gaps), [points, gaps.gaps]);
  return (
    <>
      <ChartFrame height={height} minWidth={420} label="Fees collected per bucket in ETH, stacked as infrastructure, network, and L1 poster destinations, hatched where the split is unavailable or the bucket is incomplete">
        <ResponsiveContainer width="100%" height="100%">
          <AreaChart data={rows} margin={{ top: 8, right: TIME_AXIS_RIGHT, bottom: 0, left: 0 }}>
            <defs>
              <HatchPattern id={UNSPLIT_PATTERN_ID} color={UNKNOWN_COLOR} />
              <PartialHatch />
            </defs>
            <CartesianGrid vertical={false} />
            <GapBands gaps={gaps.gaps} />
            <PartialBands bands={bands} />
            <XAxis dataKey="t" type="number" domain={[window.from, window.to]} tickFormatter={(t: number) => formatTick(t, span)} tickLine={false} axisLine={false} minTickGap={48} />
            <YAxis tickFormatter={(v: number) => formatSignificant(v, 2)} tickLine={false} axisLine={false} width={48} />
            <Tooltip isAnimationActive={false} content={(props) => <ChartTooltip {...props} title={(t) => formatDateTime(t)} rows={feeFlowRows(unsplit)} note={(r) => partialRowNote(r, true)} />} />
            <Area type="monotone" dataKey="stackFloorEth" stackId="fees" connectNulls={false} stroke={FLOOR_FILL} strokeWidth={1} fill={FLOOR_FILL} fillOpacity={0.6} isAnimationActive={false} activeDot={false} />
            <Area type="monotone" dataKey="stackSurplusEth" stackId="fees" connectNulls={false} stroke={SURPLUS_FILL} strokeWidth={1} fill={SURPLUS_FILL} fillOpacity={0.5} isAnimationActive={false} activeDot={false} />
            <Area type="monotone" dataKey="stackPosterEth" stackId="fees" connectNulls={false} stroke={POSTER_FILL} strokeWidth={1} fill={POSTER_FILL} fillOpacity={0.55} isAnimationActive={false} activeDot={false} />
            {unsplit ? (
              <Area type="monotone" dataKey="stackUnsplitEth" stackId="fees" connectNulls={false} stroke={UNKNOWN_COLOR} strokeWidth={1} strokeDasharray="4 3" fill={`url(#${UNSPLIT_PATTERN_ID})`} isAnimationActive={false} activeDot={false} />
            ) : null}
          </AreaChart>
        </ResponsiveContainer>
      </ChartFrame>
      <GapNote gaps={gaps} />
      <PartialNote bands={bands} />
    </>
  );
});

/** The legend the fee chart carries, wherever it is drawn. */
export function feeFlowLegend(unsplit: boolean) {
  return [
    { label: "floor to infra", color: FLOOR_FILL },
    { label: "congestion to network", color: SURPLUS_FILL },
    { label: "poster fee to L1 pricer", color: POSTER_FILL },
    // The unknown series is drawn as a hatch, so its swatch is the same hatch:
    // the legend has to carry the pattern, not only the colour.
    ...(unsplit ? [{ label: UNSPLIT_FEES_LABEL, color: UNKNOWN_COLOR, kind: "hatch" as const }] : []),
  ];
}

/** Fee account balances as sampled counters, and fees per bucket from the history split by the floor in force at each block. */
export function FeeFlows({ network, range, snapshot, series, explorerUrl, model = "unknown", nowMs }: { network: string; range: SeriesRange; snapshot: LiveSnapshot | null; series: Series | null; explorerUrl?: string; model?: PricerModel; /** Wall clock the quote's age is measured against, for a caller that fixes it; otherwise this component keeps its own. */ nowMs?: number }) {
  // The clock lives here rather than on the page, so a second of wall time
  // re-renders the quote lines and not every history chart above them.
  const ticked = useTicker(nowMs === undefined ? 1000 : 0);
  const clock = nowMs ?? ticked;
  const points = useMemo(() => (series ? buildChartPoints(series, model) : []), [series, model]);
  const gaps = useMemo(() => (series ? gapModel(series, points) : NO_GAPS), [series, points]);
  // The same rule the live tiles follow: no quote, or one older than ten minutes, and the totals stay in ETH alone.
  const ethUsd = snapshot?.ethUsd;
  const totals = useMemo(() => (series ? feeTotals(series) : null), [series]);
  const [tableOpen, setTableOpen] = useState(false);
  const accounts = snapshot?.accounts;
  const unsplit = (totals?.unsplit ?? 0) > 0;
  const incomplete = totals?.completeness !== "complete";
  const totalsNote = totals === null ? null : incompleteTotalsNote(totals);
  const legend = feeFlowLegend(unsplit);

  return (
    <div className="grid grid-cols-[minmax(0,1fr)] gap-4 lg:grid-cols-[minmax(0,1fr)_minmax(0,1.4fr)]">
      <Card>
        <Label>Fee accounts (sampled balances)</Label>
        {accounts ? (
          <div className="mt-2">
            <AccountRow name="Infra fee account" role="receives the compute-gas floor" address={accounts.infra.address} balance={accounts.infra.balance} explorerUrl={explorerUrl} />
            <AccountRow name="Network fee account" role="receives compute-gas congestion fees" address={accounts.network.address} balance={accounts.network.balance} explorerUrl={explorerUrl} />
            <AccountRow name="L1 reward recipient" role={snapshot?.l1 ? `${formatInteger(snapshot.l1.rewardRate)} wei per L1 unit, paid from the L1 pricer pool` : "paid downstream from the L1 pricer pool"} address={accounts.l1Reward.address} balance={accounts.l1Reward.balance} explorerUrl={explorerUrl} />
          </div>
        ) : (
          <p className="mt-2 text-sm text-ink-2">Balances are sampled once a minute; none yet.</p>
        )}
        <p className="mt-3 text-xs text-ink-3">
          A balance that drops is a withdrawal, not a refund. Total fees remain Σ gasUsed × baseFee. Receipt poster gas funds the L1 pricer pool; only compute gas is split between infrastructure and
          network by the minimum base fee in force at each block.
        </p>
      </Card>

      <Card>
        {totals && series ? (
          <>
            <div className="grid grid-cols-2 gap-x-4 gap-y-4 sm:grid-cols-5">
              <Stat label={`${incomplete ? "Indexed fees" : "Fees"} in ${series.range === "all" ? "all time" : `last ${series.range}`}`} value={formatSignificant(totals.total, 4)} unit="ETH" size="sm" hint={usdLine(totals.total, ethUsd, clock, 4, USD_PLACEMENT[0])} />
              <Stat
                label="Daily run rate"
                value={totals.perDay === null ? "n/a" : formatSignificant(totals.perDay, 4)}
                unit={totals.perDay === null ? undefined : "ETH"}
                size="sm"
                hint={totals.perDay === null ? undefined : <>{totals.rateCoverage === null ? null : <span className="block">{formatPercent(totals.rateCoverage)} complete coverage</span>}{usdLine(totals.perDay, ethUsd, clock, 4, USD_PLACEMENT[1])}</>}
              />
              <Stat label={incomplete ? "Indexed floor to infra" : "Floor to infra"} value={totals.splitKnown > 0 ? formatSignificant(totals.floorEth, 3) : "n/a"} unit={totals.splitKnown > 0 ? "ETH" : undefined} size="sm" hint={totals.splitKnown > 0 ? usdLine(totals.floorEth, ethUsd, clock, 3, USD_PLACEMENT[2]) : undefined} />
              <Stat label={incomplete ? "Indexed congestion" : "Congestion to network"} value={totals.splitKnown > 0 ? formatSignificant(totals.surplusEth, 3) : "n/a"} unit={totals.splitKnown > 0 ? "ETH" : undefined} size="sm" hint={totals.splitKnown > 0 ? usdLine(totals.surplusEth, ethUsd, clock, 3, USD_PLACEMENT[3]) : undefined} />
              <Stat label={incomplete ? "Indexed poster fee" : "Poster fee to L1 pricer"} value={totals.posterKnown > 0 ? formatSignificant(totals.posterEth, 3) : "n/a"} unit={totals.posterKnown > 0 ? "ETH" : undefined} size="sm" hint={totals.posterKnown > 0 ? usdLine(totals.posterEth, ethUsd, clock, 3, USD_PLACEMENT[4]) : undefined} />
            </div>
            {totalsNote ? <p className="mt-2 text-xs text-ink-3">{totalsNote}</p> : null}
            {unsplit ? <p className="mt-2 text-xs text-ink-3">{unsplitNote(totals.unsplit)}. Poster fees remain included when independently recorded; floor and congestion totals leave these buckets out.</p> : null}
            {totals.posterUnknown > 0 ? <p className="mt-2 text-xs text-ink-3">{formatInteger(totals.posterUnknown)} {totals.posterUnknown === 1 ? "bucket has" : "buckets have"} no recorded poster fee and is left out of the poster total.</p> : null}
          </>
        ) : null}
        <div className="mt-4">
          <div className="mb-1 flex flex-wrap items-center justify-between gap-2">
            <div className="text-xs text-ink-2">Fees per bucket (ETH), stacked by destination</div>
            <div className="flex items-center gap-3">
              <Legend items={legend} />
              <EnlargeLink network={network} view={chartView("fee-flows")} range={range} />
            </div>
          </div>
          {points.length > 0 ? (
            <>
              <FeeFlowChart points={points} gaps={gaps} />
              <details className="mt-2 text-xs text-ink-2" onToggle={(e) => setTableOpen((e.currentTarget as HTMLDetailsElement).open)}>
                <summary className="cursor-pointer select-none">Data table ({formatInteger(points.length)} buckets)</summary>
                {tableOpen ? (
                  <div className="mt-2 max-h-[320px] overflow-auto">
                    <table className="num w-full min-w-[720px] text-left">
                      <caption className="sr-only">Fees per bucket split among infrastructure, network, and the L1 pricer</caption>
                      <thead className="sticky top-0 bg-surface text-ink-3">
                        <tr>
                          <th scope="col" className="py-1 pr-3 font-medium">bucket</th>
                          <th scope="col" className="py-1 pr-3 font-medium">fees (ETH)</th>
                          <th scope="col" className="py-1 pr-3 font-medium">floor to infra</th>
                          <th scope="col" className="py-1 pr-3 font-medium">congestion to network</th>
                          <th scope="col" className="py-1 pr-3 font-medium">poster fee to L1 pricer</th>
                          <th scope="col" className="py-1 pr-3 font-medium">destination unavailable</th>
                          <th scope="col" className="py-1 pr-3 font-medium">coverage</th>
                          <th scope="col" className="py-1 pr-3 font-medium">floor (gwei)</th>
                        </tr>
                      </thead>
                      <tbody>
                        {points.map((p) => (
                          <tr key={p.t} className="border-t border-hairline">
                            <th scope="row" className="py-1 pr-3 font-normal">{formatDateTime(p.t)}</th>
                            <td className="py-1 pr-3">{formatSignificant(p.feesEth, 4)}</td>
                            <td className="py-1 pr-3">{formatPart(p.floorFeesEth)}</td>
                            <td className="py-1 pr-3">{formatPart(p.surplusFeesEth)}</td>
                            <td className="py-1 pr-3">{formatPart(p.posterFeesEth)}</td>
                            <td className="py-1 pr-3">{p.unsplitFeesEth === null ? "0" : formatPart(p.unsplitFeesEth)}</td>
                            <td className="py-1 pr-3">{p.coverage === null ? "unknown" : `${p.completeness}, ${formatPercent(p.coverage)}`}</td>
                            <td className="py-1 pr-3">{formatFloor(p.floor)}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                ) : null}
              </details>
            </>
          ) : (
            <div className="text-sm text-ink-2">{series ? emptyRangeNote(gaps.first) : "No history loaded."}</div>
          )}
        </div>
      </Card>
    </div>
  );
}
