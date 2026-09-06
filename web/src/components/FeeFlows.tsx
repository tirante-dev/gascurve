"use client";

import { useMemo, useState } from "react";
import { Area, AreaChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import type { LiveSnapshot, PricerModel, Series } from "@/types";
import { buildChartPoints, spanSeconds, sumWeiEth } from "@/utils/chart";
import { formatDateTime, formatEth, formatInteger, formatSignificant, formatTick, shortAddress } from "@/utils/format";
import { ChartTooltip } from "./ChartTooltip";
import { Card, ChartFrame, Label, Legend, Stat } from "./primitives";

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

/** Fee totals over the range from the api's exact per-bucket floor and surplus sums. */
export function feeTotals(series: Pick<Series, "points">): { total: number; floorEth: number; surplusEth: number; perDay: number } {
  const total = sumWeiEth(series.points, "feesWei");
  const floorEth = sumWeiEth(series.points, "floorFeesWei");
  const surplusEth = sumWeiEth(series.points, "surplusFeesWei");
  const span = spanSeconds(series.points);
  return { total, floorEth, surplusEth, perDay: span > 0 ? (total / span) * 86_400 : 0 };
}

/** Fee account balances as sampled counters, and fees per bucket from the history split by the floor in force at each block. */
export function FeeFlows({ snapshot, series, explorerUrl, model = "unknown" }: { snapshot: LiveSnapshot | null; series: Series | null; explorerUrl?: string; model?: PricerModel }) {
  const points = useMemo(() => (series ? buildChartPoints(series, model) : []), [series, model]);
  const totals = useMemo(() => (series ? feeTotals(series) : null), [series]);
  const [tableOpen, setTableOpen] = useState(false);
  const span = spanSeconds(points);
  const accounts = snapshot?.accounts;

  return (
    <div className="grid grid-cols-[minmax(0,1fr)] gap-4 lg:grid-cols-[minmax(0,1fr)_minmax(0,1.4fr)]">
      <Card>
        <Label>Fee accounts (sampled balances)</Label>
        {accounts ? (
          <div className="mt-2">
            <AccountRow name="Infra fee account" role="receives the floor part of every fee" address={accounts.infra.address} balance={accounts.infra.balance} explorerUrl={explorerUrl} />
            <AccountRow name="Network fee account" role="receives the congestion part above the floor" address={accounts.network.address} balance={accounts.network.balance} explorerUrl={explorerUrl} />
            <AccountRow name="L1 reward recipient" role="10 wei per L1 unit" address={accounts.l1Reward.address} balance={accounts.l1Reward.balance} explorerUrl={explorerUrl} />
          </div>
        ) : (
          <p className="mt-2 text-sm text-ink-2">Balances are sampled once a minute; none yet.</p>
        )}
        <p className="mt-3 text-xs text-ink-3">
          A balance that drops is a withdrawal, not a refund. Fees per period below come from Σ gasUsed × baseFee over blocks, split by the minimum base fee in force at each block (owner changes to the
          floor are respected), which withdrawals do not affect.
        </p>
      </Card>

      <Card>
        {totals && series ? (
          <div className="grid grid-cols-2 gap-x-4 gap-y-4 sm:grid-cols-4">
            <Stat label={`Fees in ${series.range === "all" ? "all time" : `last ${series.range}`}`} value={formatSignificant(totals.total, 4)} unit="ETH" size="sm" />
            <Stat label="Per day (est.)" value={formatSignificant(totals.perDay, 4)} unit="ETH" size="sm" />
            <Stat label="Floor to infra" value={formatSignificant(totals.floorEth, 3)} unit="ETH" size="sm" />
            <Stat label="Congestion to network" value={formatSignificant(totals.surplusEth, 3)} unit="ETH" size="sm" />
          </div>
        ) : null}
        <div className="mt-4">
          <div className="mb-1 flex flex-wrap items-baseline justify-between gap-2">
            <div className="text-xs text-ink-2">Fees per bucket (ETH), stacked by destination</div>
            <Legend
              items={[
                { label: "floor to infra", color: "var(--seq-2)" },
                { label: "congestion to network", color: "var(--seq-8)" },
              ]}
            />
          </div>
          {points.length > 0 ? (
            <>
              <ChartFrame height={160} minWidth={420} label="Fees collected per bucket in ETH, stacked as the floor part and the congestion part">
                <ResponsiveContainer width="100%" height="100%">
                  <AreaChart data={points} margin={{ top: 8, right: 12, bottom: 0, left: 0 }}>
                    <CartesianGrid vertical={false} />
                    <XAxis dataKey="t" type="number" domain={["dataMin", "dataMax"]} tickFormatter={(t: number) => formatTick(t, span)} tickLine={false} axisLine={false} minTickGap={48} />
                    <YAxis tickFormatter={(v: number) => formatSignificant(v, 2)} tickLine={false} axisLine={false} width={48} />
                    <Tooltip
                      isAnimationActive={false}
                      content={(props) => (
                        <ChartTooltip
                          {...props}
                          title={(t) => formatDateTime(t)}
                          rows={[
                            { label: "fees in bucket", value: (r) => `${formatSignificant(Number(r.feesEth), 4)} ETH` },
                            { label: "floor to infra", color: "var(--seq-2)", kind: "rect", value: (r) => `${formatSignificant(Number(r.floorFeesEth), 4)} ETH` },
                            { label: "congestion to network", color: "var(--seq-8)", kind: "rect", value: (r) => `${formatSignificant(Number(r.surplusFeesEth), 4)} ETH` },
                            { label: "floor in force", value: (r) => `${formatSignificant(Number(r.floor), 3)} gwei` },
                          ]}
                        />
                      )}
                    />
                    <Area type="monotone" dataKey="floorFeesEth" stackId="fees" stroke="var(--seq-2)" strokeWidth={1} fill="var(--seq-2)" fillOpacity={0.6} isAnimationActive={false} activeDot={false} />
                    <Area type="monotone" dataKey="surplusFeesEth" stackId="fees" stroke="var(--seq-8)" strokeWidth={1} fill="var(--seq-8)" fillOpacity={0.5} isAnimationActive={false} activeDot={false} />
                  </AreaChart>
                </ResponsiveContainer>
              </ChartFrame>
              <details className="mt-2 text-xs text-ink-2" onToggle={(e) => setTableOpen((e.currentTarget as HTMLDetailsElement).open)}>
                <summary className="cursor-pointer select-none">Data table ({formatInteger(points.length)} buckets)</summary>
                {tableOpen ? (
                  <div className="mt-2 max-h-[320px] overflow-auto">
                    <table className="num w-full min-w-[520px] text-left">
                      <caption className="sr-only">Fees per bucket split into the floor part and the congestion part</caption>
                      <thead className="sticky top-0 bg-surface text-ink-3">
                        <tr>
                          <th scope="col" className="py-1 pr-3 font-medium">bucket</th>
                          <th scope="col" className="py-1 pr-3 font-medium">fees (ETH)</th>
                          <th scope="col" className="py-1 pr-3 font-medium">floor to infra</th>
                          <th scope="col" className="py-1 pr-3 font-medium">congestion to network</th>
                          <th scope="col" className="py-1 pr-3 font-medium">floor (gwei)</th>
                        </tr>
                      </thead>
                      <tbody>
                        {points.map((p) => (
                          <tr key={p.t} className="border-t border-hairline">
                            <th scope="row" className="py-1 pr-3 font-normal">{formatDateTime(p.t)}</th>
                            <td className="py-1 pr-3">{formatSignificant(p.feesEth, 4)}</td>
                            <td className="py-1 pr-3">{formatSignificant(p.floorFeesEth, 4)}</td>
                            <td className="py-1 pr-3">{formatSignificant(p.surplusFeesEth, 4)}</td>
                            <td className="py-1 pr-3">{formatSignificant(p.floor, 3)}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                ) : null}
              </details>
            </>
          ) : (
            <div className="text-sm text-ink-2">No history loaded.</div>
          )}
        </div>
      </Card>
    </div>
  );
}
