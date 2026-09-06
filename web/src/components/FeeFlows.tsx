"use client";

import { useMemo } from "react";
import { Area, AreaChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import type { LiveSnapshot, Series } from "@/types";
import { buildChartPoints, spanSeconds, sumFeesEth } from "@/utils/chart";
import { formatDateTime, formatEth, formatSignificant, formatTick, shortAddress, weiToEthNumber } from "@/utils/format";
import { ChartTooltip } from "./ChartTooltip";
import { Card, ChartFrame, Label, Stat } from "./primitives";

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

/** Fee account balances as sampled counters, and fees per bucket from the history. */
export function FeeFlows({ snapshot, series, explorerUrl }: { snapshot: LiveSnapshot | null; series: Series | null; explorerUrl?: string }) {
  const points = useMemo(() => (series ? buildChartPoints(series) : []), [series]);
  const totals = useMemo(() => {
    if (!series || !snapshot) return null;
    const total = sumFeesEth(series.points);
    const minFee = BigInt(snapshot.minBaseFee);
    let floorWei = 0n;
    for (const p of series.points) {
      const fees = BigInt(p.feesWei);
      const floorPart = BigInt(p.gasUsed) * minFee;
      floorWei += floorPart < fees ? floorPart : fees;
    }
    const floorEth = weiToEthNumber(floorWei);
    const span = spanSeconds(series.points);
    return { total, floorEth, congestionEth: Math.max(0, total - floorEth), perDay: span > 0 ? (total / span) * 86_400 : 0 };
  }, [series, snapshot]);
  const span = spanSeconds(points);
  const accounts = snapshot?.accounts;

  return (
    <div className="grid gap-4 lg:grid-cols-[minmax(0,1fr)_minmax(0,1.4fr)]">
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
        <p className="mt-3 text-xs text-ink-3">A balance that drops is a withdrawal, not a refund. Fees per period below come from Σ gasUsed × baseFee over blocks, which withdrawals do not affect.</p>
      </Card>

      <Card>
        {totals && series ? (
          <div className="grid grid-cols-2 gap-x-4 gap-y-4 sm:grid-cols-4">
            <Stat label={`Fees in ${series.range === "all" ? "all time" : `last ${series.range}`}`} value={formatSignificant(totals.total, 4)} unit="ETH" size="sm" />
            <Stat label="Per day (est.)" value={formatSignificant(totals.perDay, 4)} unit="ETH" size="sm" />
            <Stat label="Floor to infra" value={formatSignificant(totals.floorEth, 3)} unit="ETH" size="sm" />
            <Stat label="Congestion to network" value={formatSignificant(totals.congestionEth, 3)} unit="ETH" size="sm" />
          </div>
        ) : null}
        <div className="mt-4">
          <div className="mb-1 text-xs text-ink-2">Fees per bucket (ETH)</div>
          {points.length > 0 ? (
            <ChartFrame height={160} minWidth={420} label="Fees collected per bucket in ETH">
              <ResponsiveContainer width="100%" height="100%">
                <AreaChart data={points} margin={{ top: 8, right: 12, bottom: 0, left: 0 }}>
                  <CartesianGrid vertical={false} />
                  <XAxis dataKey="t" type="number" domain={["dataMin", "dataMax"]} tickFormatter={(t: number) => formatTick(t, span)} tickLine={false} axisLine={false} minTickGap={48} />
                  <YAxis tickFormatter={(v: number) => formatSignificant(v, 2)} tickLine={false} axisLine={false} width={48} />
                  <Tooltip
                    isAnimationActive={false}
                    content={(props) => (
                      <ChartTooltip {...props} title={(t) => formatDateTime(t)} rows={[{ label: "fees in bucket", color: "var(--series-1)", value: (r) => `${formatSignificant(Number(r.feesEth), 4)} ETH` }]} />
                    )}
                  />
                  <Area type="monotone" dataKey="feesEth" stroke="var(--series-1)" strokeWidth={2} fill="var(--series-1)" fillOpacity={0.1} isAnimationActive={false} activeDot={false} />
                </AreaChart>
              </ResponsiveContainer>
            </ChartFrame>
          ) : (
            <div className="text-sm text-ink-2">No history loaded.</div>
          )}
        </div>
      </Card>
    </div>
  );
}
