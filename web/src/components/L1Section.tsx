"use client";

import { useCallback, useMemo, useState } from "react";
import { CartesianGrid, Line, LineChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { useApi } from "@/hooks/useApi";
import { getBatches } from "@/lib/api/batches";
import { getL1 } from "@/lib/api/l1";
import type { LiveSnapshot, Series, SeriesRange } from "@/types";
import { joinCosts, logDomain, resampleFees, spanSeconds } from "@/utils/chart";
import { formatDateTime, formatDuration, formatEth, formatGas, formatGwei, formatInteger, formatSignificant, formatTick } from "@/utils/format";
import { ChartTooltip } from "./ChartTooltip";
import { Card, ChartFrame, Legend, Stat } from "./primitives";

// "batch" is one point per posting report (every 12 to 24 s on Robinhood); L2 fees are joined on 15 s buckets for it.
const RESOLUTION_SECONDS: Record<string, number> = { batch: 15, "1m": 60, "15m": 900, "1h": 3600 };

/** L1 pricer values and what the chain pays Ethereum against what users pay. Collapsed by default. */
export function L1Section({ network, range, snapshot, series }: { network: string; range: SeriesRange; snapshot: LiveSnapshot | null; series: Series | null }) {
  const [open, setOpen] = useState(false);
  const batchFetcher = useCallback((signal: AbortSignal) => getBatches(network, range, { signal }), [network, range]);
  const l1Fetcher = useCallback((signal: AbortSignal) => getL1(network, range, { signal }), [network, range]);
  const batches = useApi(open ? `${network}:${range}:batches` : null, batchFetcher);
  const l1 = useApi(open ? `${network}:${range}:l1` : null, l1Fetcher);

  const rows = useMemo(() => {
    if (!batches.data || !series) return [];
    const bucket = RESOLUTION_SECONDS[batches.data.resolution] ?? 3600;
    return joinCosts(batches.data.points, resampleFees(series.points, bucket), bucket);
  }, [batches.data, series]);
  const span = spanSeconds(rows);
  const totals = useMemo(() => {
    const l1Eth = rows.reduce((s, r) => s + r.l1Eth, 0);
    const l2Eth = rows.reduce((s, r) => s + r.l2Eth, 0);
    const count = rows.reduce((s, r) => s + r.batches, 0);
    return { l1Eth, l2Eth, count, interval: count > 0 && span > 0 ? span / count : 0 };
  }, [rows, span]);
  const domain = useMemo(() => logDomain(rows.flatMap((r) => [r.l1Eth, r.l2Eth])), [rows]);
  const l1State = snapshot?.l1;

  return (
    <details
      className="rounded-md border border-hairline bg-surface"
      open={open}
      onToggle={(e) => setOpen((e.currentTarget as HTMLDetailsElement).open)}
    >
      <summary className="cursor-pointer select-none px-4 py-3 text-sm font-semibold text-ink">
        L1 pricer and posting costs <span className="ml-2 font-normal text-ink-3">{open ? "" : "collapsed"}</span>
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
          The L1 pricer adapts its per-unit price so that collected L1 fees match batch-posting costs. With blobs and compression the cost per transaction is tiny, so the price has converged
          near zero and <code className="rounded bg-surface-2 px-1 font-mono text-[0.9em] text-ink">gasUsedForL1</code> rounds to 0 on a normal transaction. The chart compares what users paid
          in L2 fees with what the chain paid Ethereum for the same buckets, from <code className="rounded bg-surface-2 px-1 font-mono text-[0.9em] text-ink">batchPostingReport</code> internal
          transactions.
        </p>
        <div className="mt-4">
          <div className="mb-2 flex flex-wrap items-baseline justify-between gap-2">
            <h3 className="text-sm font-semibold text-ink">What users pay against what the chain pays Ethereum (ETH per bucket, log scale)</h3>
            <Legend
              items={[
                { label: `L2 fees ${formatSignificant(totals.l2Eth, 3)} ETH`, color: "var(--series-1)", kind: "line" },
                { label: `L1 posting ${formatSignificant(totals.l1Eth, 3)} ETH`, color: "var(--series-2)", kind: "line" },
              ]}
            />
          </div>
          {batches.error ? <p className="text-sm text-critical">Could not load batches: {batches.error}</p> : null}
          {l1.error ? <p className="text-sm text-critical">Could not load L1 series: {l1.error}</p> : null}
          {rows.length > 0 ? (
            <ChartFrame height={220} label="L2 fees and L1 posting cost per bucket on a log scale">
              <ResponsiveContainer width="100%" height="100%">
                <LineChart data={rows} margin={{ top: 8, right: 12, bottom: 0, left: 0 }}>
                  <CartesianGrid vertical={false} />
                  <XAxis dataKey="t" type="number" domain={["dataMin", "dataMax"]} tickFormatter={(t: number) => formatTick(t, span)} tickLine={false} axisLine={false} minTickGap={48} />
                  <YAxis scale="log" domain={domain} tickFormatter={(v: number) => formatSignificant(v, 1)} tickLine={false} axisLine={false} width={56} />
                  <Tooltip
                    isAnimationActive={false}
                    content={(props) => (
                      <ChartTooltip
                        {...props}
                        title={(t) => formatDateTime(t)}
                        rows={[
                          { label: "L2 fees", color: "var(--series-1)", value: (r) => `${formatSignificant(Number(r.l2Eth), 4)} ETH` },
                          { label: "L1 posting cost", color: "var(--series-2)", value: (r) => `${formatSignificant(Number(r.l1Eth), 4)} ETH` },
                          { label: "batches", value: (r) => formatInteger(Number(r.batches)) },
                        ]}
                      />
                    )}
                  />
                  <Line type="monotone" dataKey="l2Eth" stroke="var(--series-1)" strokeWidth={2} dot={false} isAnimationActive={false} connectNulls />
                  <Line type="monotone" dataKey="l1Eth" stroke="var(--series-2)" strokeWidth={2} dot={false} isAnimationActive={false} connectNulls />
                </LineChart>
              </ResponsiveContainer>
            </ChartFrame>
          ) : (
            <Card className="text-sm text-ink-2">{batches.loading ? "Loading batch reports." : "No batch reports in this range."}</Card>
          )}
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
