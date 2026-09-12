"use client";

import { memo, useCallback, useMemo, useState } from "react";
import { CartesianGrid, Line, LineChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { useApi } from "@/hooks/useApi";
import { getBatches } from "@/lib/api/batches";
import { getL1 } from "@/lib/api/l1";
import type { BatchSeries, L1Series, LiveSnapshot, Series, SeriesRange } from "@/types";
import { chartView } from "@/lib/chartViews";
import { joinCosts, logDomain, resamplePosterFees, spanSeconds, type CostRow } from "@/utils/chart";
import { gapModel, irregularStep, withGapBreaks, NO_GAPS, type GapModel } from "@/lib/gaps";
import { partialBands } from "@/lib/partial";
import { formatDateTime, formatDuration, formatEth, formatGas, formatGwei, formatInteger, formatPercent, formatSignificant, formatTick } from "@/utils/format";
import { EnlargeLink } from "./ChartActions";
import { ChartTooltip } from "./ChartTooltip";
import { GapBands, GapNote, PartialBands, PartialNote } from "./ChartGaps";
import { Card, ChartFrame, Legend, Stat, TIME_AXIS_RIGHT, type ChartHeight } from "./primitives";

// "batch" is exactly one point per posting report (every 12 to 24 s on Robinhood).
// Both reports and poster fees are grouped into 15 s buckets before the join,
// so a bucket that holds two reports counts its poster fees once.
export const RESOLUTION_SECONDS: Record<string, number> = { batch: 15, "1m": 60, "15m": 900, "1h": 3600 };

/** The height the cost chart stands at in the section; the enlarged view passes its own. */
export const L1_CHART_HEIGHT = 220;

/** What the section and the enlarged chart both need: the joined rows and what they add up to. */
export type L1Costs = {
  rows: CostRow[];
  bucket: number;
  span: number;
  domain: [number, number];
  /** The window the batch range asked for, and the stretches of it with no report at all. */
  gaps: GapModel;
  totals: { attributedCostEth: number; costIncomplete: boolean; posterFeesEth: number; posterIncomplete: boolean; count: number; interval: number };
  batches: ReturnType<typeof useApi<BatchSeries>>;
  l1: ReturnType<typeof useApi<L1Series>>;
};

/**
 * Batch posting reports joined to poster fees over the union of their buckets. `enabled`
 * is false while the section is collapsed, so a page that never opens it
 * never asks the api for either series.
 */
export function useL1Costs(network: string, range: SeriesRange, series: Series | null, enabled = true): L1Costs {
  const batchFetcher = useCallback((signal: AbortSignal) => getBatches(network, range, { signal }), [network, range]);
  const l1Fetcher = useCallback((signal: AbortSignal) => getL1(network, range, { signal }), [network, range]);
  const batches = useApi<BatchSeries>(enabled ? `${network}:${range}:batches` : null, batchFetcher);
  const l1 = useApi<L1Series>(enabled ? `${network}:${range}:l1` : null, l1Fetcher);

  const joined = useMemo(() => {
    if (!batches.data || !series) return [];
    const bucket = RESOLUTION_SECONDS[batches.data.resolution] ?? 3600;
    return joinCosts(batches.data.points, resamplePosterFees(series.points, bucket), bucket);
  }, [batches.data, series]);
  const bucket = batches.data ? (RESOLUTION_SECONDS[batches.data.resolution] ?? 3600) : 3600;
  // Reports arrive on their own cadence, so an empty bucket between two of
  // them is ordinary; only a stretch several intervals long is a gap.
  const gaps = useMemo(() => {
    if (!batches.data) return NO_GAPS;
    return gapModel(batches.data, batches.data.points, irregularStep(batches.data.points, bucket));
  }, [batches.data, bucket]);
  const rows = useMemo(
    () => joined.map((row) => (gaps.gaps.some((gap) => row.t >= gap.from && row.t < gap.to) ? { ...row, attributedCostEth: null } : row)),
    [joined, gaps.gaps],
  );
  const span = gaps.window.to > gaps.window.from ? gaps.window.to - gaps.window.from : spanSeconds(rows);
  const totals = useMemo(() => {
    const attributedCostEth = rows.reduce((s, r) => s + (r.attributedCostEth ?? 0), 0);
    const costIncomplete = rows.some((r) => r.attributedCostEth === null);
    const posterFeesEth = rows.reduce((s, r) => s + (r.posterFeesEth ?? 0), 0);
    const posterIncomplete = rows.some((r) => r.posterFeesEth === null || r.completeness !== "complete");
    const count = rows.reduce((s, r) => s + r.batches, 0);
    return { attributedCostEth, costIncomplete, posterFeesEth, posterIncomplete, count, interval: count > 0 && span > 0 ? span / count : 0 };
  }, [rows, span]);
  const domain = useMemo(() => logDomain(rows.flatMap((r) => [r.attributedCostEth ?? 0, r.posterFeesEth ?? 0])), [rows]);
  return { rows, bucket, span, domain, gaps, totals, batches, l1 };
}

/** The legend the cost chart carries: each line with what it came to over the range. */
export function l1CostLegend(totals: L1Costs["totals"]) {
  return [
    { label: `${totals.posterIncomplete ? "Indexed poster fees" : "Poster fees"} ${formatSignificant(totals.posterFeesEth, 3)} ETH`, color: "var(--series-1)", kind: "line" as const },
    { label: `${totals.costIncomplete ? "Indexed ArbOS batch cost" : "ArbOS batch cost"} ${formatSignificant(totals.attributedCostEth, 3)} ETH`, color: "var(--series-2)", kind: "line" as const },
  ];
}

/** Poster fees against ArbOS-attributed batch-poster spending, per bucket. */
export const L1CostChart = memo(function L1CostChart({ rows, bucket, span, domain, gaps = NO_GAPS, height = L1_CHART_HEIGHT }: { rows: CostRow[]; bucket: number; span: number; domain: [number, number]; gaps?: GapModel; height?: ChartHeight }) {
  const window = gaps.window.to > gaps.window.from ? gaps.window : { from: rows[0]?.t ?? 0, to: rows[rows.length - 1]?.t ?? 0 };
  const bands = useMemo(() => partialBands(rows, bucket, window.to), [rows, bucket, window.to]);
  // Recharts keys its selectors off the `data` identity, so the broken rows
  // are built once per join rather than on every render of the page.
  const drawn = useMemo(() => withGapBreaks(rows.map((row) => ({ ...row, plottedPosterFeesEth: row.completeness === "complete" ? row.posterFeesEth : null })), gaps.gaps), [rows, gaps.gaps]);
  return (
    <>
      <ChartFrame height={height} label="Poster fees collected for the L1 pricer and ArbOS-attributed batch-posting cost per bucket on a log scale">
        <ResponsiveContainer width="100%" height="100%">
          <LineChart data={drawn} margin={{ top: 8, right: TIME_AXIS_RIGHT, bottom: 0, left: 0 }}>
            <CartesianGrid vertical={false} />
            <GapBands gaps={gaps.gaps} />
            <PartialBands bands={bands} />
            <XAxis dataKey="t" type="number" domain={[window.from, window.to]} tickFormatter={(t: number) => formatTick(t, span)} tickLine={false} axisLine={false} minTickGap={48} />
            <YAxis scale="log" domain={domain} tickFormatter={(v: number) => formatSignificant(v, 1)} tickLine={false} axisLine={false} width={56} />
            <Tooltip
              isAnimationActive={false}
              content={(props) => (
                <ChartTooltip
                  {...props}
                  title={(t) => formatDateTime(t)}
                  rows={[
                    { label: "poster fees", color: "var(--series-1)", value: (r) => (typeof r.posterFeesEth === "number" ? `${formatSignificant(r.posterFeesEth, 4)} ETH` : "n/a") },
                    { label: "ArbOS-attributed cost", color: "var(--series-2)", value: (r) => (typeof r.attributedCostEth === "number" ? `${formatSignificant(r.attributedCostEth, 4)} ETH` : "n/a") },
                    { label: "batches", value: (r) => formatInteger(Number(r.batches)) },
                    { label: "fee coverage", value: (r) => (typeof r.coverage === "number" ? formatPercent(r.coverage) : "unknown") },
                  ]}
                />
              )}
            />
            <Line type="monotone" dataKey="plottedPosterFeesEth" stroke="var(--series-1)" strokeWidth={2} dot={false} isAnimationActive={false} connectNulls={false} />
            <Line type="monotone" dataKey="attributedCostEth" stroke="var(--series-2)" strokeWidth={2} dot={false} isAnimationActive={false} connectNulls={false} />
          </LineChart>
        </ResponsiveContainer>
      </ChartFrame>
      <GapNote gaps={gaps} />
      <PartialNote bands={bands} />
    </>
  );
});

/** L1 pricer values and ArbOS-attributed batch costs. Collapsed by default. */
export function L1Section({ network, range, snapshot, series }: { network: string; range: SeriesRange; snapshot: LiveSnapshot | null; series: Series | null }) {
  const [open, setOpen] = useState(false);
  const { rows, bucket, span, domain, gaps, totals, batches, l1 } = useL1Costs(network, range, series, open);
  const l1State = snapshot?.l1;
  const [tableOpen, setTableOpen] = useState(false);

  return (
    <details
      className="vw-card"
      open={open}
      onToggle={(e) => setOpen((e.currentTarget as HTMLDetailsElement).open)}
    >
      <summary className="cursor-pointer select-none px-4 py-3 text-sm font-semibold text-ink">
        L1 pricer and attributed batch costs <span className="ml-2 font-normal text-ink-3">{open ? "" : "collapsed"}</span>
      </summary>
      <div className="border-t border-hairline p-4">
        {l1State ? (
          <div className="grid grid-cols-2 gap-x-4 gap-y-4 sm:grid-cols-4">
            <Stat label="Price per L1 gas unit" value={formatGwei(l1State.baseFeeEstimate)} unit="gwei" size="sm" hint="getL1BaseFeeEstimate" />
            <Stat label="Pricing surplus" value={formatEth(l1State.surplus, { unit: false })} unit="ETH" size="sm" hint="getL1PricingSurplus" />
            <Stat label="Fees available" value={formatEth(l1State.feesAvailable, { unit: false })} unit="ETH" size="sm" hint="getL1FeesAvailable" />
            <Stat label="Units since update" value={formatGas(l1State.unitsSinceUpdate)} size="sm" hint={`last update ${formatDateTime(l1State.lastUpdateAt)}`} />
            <Stat label="Equilibration units" value={formatGas(l1State.equilibrationUnits)} size="sm" />
            <Stat label="Per-batch gas charge" value={formatInteger(l1State.perBatchGasCharge)} size="sm" />
            <Stat label="L1 reward rate" value={formatInteger(l1State.rewardRate)} unit="wei/unit" size="sm" />
            <Stat label="Batches in range" value={formatInteger(totals.count)} size="sm" hint={totals.interval > 0 ? `one every ${formatDuration(totals.interval)}` : undefined} />
          </div>
        ) : (
          <p className="text-sm text-ink-2">L1 values arrive with the slow (60 s) sample.</p>
        )}
        <p className="mt-3 max-w-[65ch] text-sm text-ink-2">
          The L1 pricer adapts its per-unit price so poster fees collected into its pool match the spending ArbOS attributes to batch posters. Infrastructure and network compute fees do not fund this mechanism. The attributed amount combines Nitro&apos;s calldata and storage
          accounting, batch extra gas, the effective per-batch charge, and the ArbOS 50+ parent calldata floor. It is not the batch poster&apos;s Ethereum receipt total. With blobs and compression
          the cost per transaction is tiny, so <code className="rounded bg-surface-2 px-1 font-mono text-[0.9em] text-ink">gasUsedForL1</code> rounds to 0 on a normal transaction. Receipt{" "}
          <code className="rounded bg-surface-2 px-1 font-mono text-[0.9em] text-ink">gasUsedForL1</code> is the authoritative poster-gas input for allocating user fees.
        </p>
        <div className="mt-4">
          <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
            <h3 className="text-sm font-semibold text-ink">Poster fees against ArbOS-attributed batch-posting cost (ETH per bucket, log scale)</h3>
            <div className="flex items-center gap-3">
              <Legend items={l1CostLegend(totals)} />
              <EnlargeLink network={network} view={chartView("l1")} range={range} />
            </div>
          </div>
          {batches.error ? <p className="text-sm text-critical">Could not load batches: {batches.error}</p> : null}
          {l1.error ? <p className="text-sm text-critical">Could not load L1 series: {l1.error}</p> : null}
          {rows.length > 0 ? (
            <L1CostChart rows={rows} bucket={bucket} span={span} domain={domain} gaps={gaps} />
          ) : (
            // "No reports" is what the record says: a chain can be indexed for
            // the range and still have posted nothing in it.
            <Card className="text-sm text-ink-2">{batches.loading ? "Loading batch reports." : "No batch reports in this range."}</Card>
          )}
          {rows.length > 0 ? (
            <details className="mt-2 text-xs text-ink-2" onToggle={(e) => setTableOpen((e.currentTarget as HTMLDetailsElement).open)}>
              <summary className="cursor-pointer select-none">Data table ({formatInteger(rows.length)} buckets)</summary>
              {tableOpen ? (
                <div className="mt-2 max-h-[320px] overflow-auto">
                  <table className="num w-full min-w-[620px] text-left">
                    <caption className="sr-only">Poster fees collected for the L1 pricer and ArbOS-attributed batch-posting cost per bucket</caption>
                    <thead className="sticky top-0 bg-surface text-ink-3">
                      <tr>
                        <th scope="col" className="py-1 pr-3 font-medium">bucket</th>
                        <th scope="col" className="py-1 pr-3 font-medium">poster fees (ETH)</th>
                        <th scope="col" className="py-1 pr-3 font-medium">ArbOS-attributed cost (ETH)</th>
                        <th scope="col" className="py-1 pr-3 font-medium">batches</th>
                        <th scope="col" className="py-1 pr-3 font-medium">fee coverage</th>
                      </tr>
                    </thead>
                    <tbody>
                      {rows.map((r) => (
                        <tr key={r.t} className="border-t border-hairline">
                          <th scope="row" className="py-1 pr-3 font-normal">{formatDateTime(r.t)}</th>
                          <td className="py-1 pr-3">{r.posterFeesEth === null ? "n/a" : formatSignificant(r.posterFeesEth, 4)}</td>
                          <td className="py-1 pr-3">{r.attributedCostEth === null ? "n/a" : formatSignificant(r.attributedCostEth, 4)}</td>
                          <td className="py-1 pr-3">{formatInteger(r.batches)}</td>
                          <td className="py-1 pr-3">{r.coverage === null ? "unknown" : `${r.completeness}, ${formatPercent(r.coverage)}`}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              ) : null}
            </details>
          ) : null}
          {l1.data && l1.data.points.length > 0 ? (
            <p className="mt-2 text-xs text-ink-3">
              {formatInteger(l1.data.points.length)} L1 pricer samples in range; price per unit moved between {formatGwei(l1.data.points.reduce((m, p) => (BigInt(p.baseFeeEstimate) < BigInt(m) ? p.baseFeeEstimate : m), l1.data.points[0].baseFeeEstimate))} and{" "}
              {formatGwei(l1.data.points.reduce((m, p) => (BigInt(p.baseFeeEstimate) > BigInt(m) ? p.baseFeeEstimate : m), l1.data.points[0].baseFeeEstimate))} gwei.
            </p>
          ) : null}
        </div>
      </div>
    </details>
  );
}
