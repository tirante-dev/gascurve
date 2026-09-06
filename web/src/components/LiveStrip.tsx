"use client";

import { useMemo } from "react";
import { Area, AreaChart, ResponsiveContainer, YAxis } from "recharts";
import { useTicker } from "@/hooks/useTicker";
import type { BlockPoint, LiveSnapshot, LiveStatus } from "@/types";
import { rampColor, rampInk, rampStep } from "@/utils/chart";
import { bipsToMultiplier, costWei, formatEth, formatGas, formatGwei, formatInteger, formatPercent, weiToGweiNumber } from "@/utils/format";
import { Label, Stat, StatusPill } from "./primitives";

export const TRANSFER_GAS = 21_000;
export const SWAP_GAS = 150_000;

/** Last-120-blocks sparkline of the base fee. One series, so no legend. */
export function Sparkline({ blocks }: { blocks: BlockPoint[] }) {
  const data = useMemo(() => blocks.map((b) => ({ n: b.number, fee: weiToGweiNumber(b.baseFee) })), [blocks]);
  if (data.length < 2) return <div className="h-12 w-full rounded bg-surface-2" aria-hidden="true" />;
  const last = data[data.length - 1];
  const min = Math.min(...data.map((d) => d.fee));
  const max = Math.max(...data.map((d) => d.fee));
  return (
    <div className="h-12 w-full" role="img" aria-label={`Base fee over the last ${data.length} blocks, from ${formatGwei(blocks[0].baseFee)} to ${formatGwei(blocks[blocks.length - 1].baseFee)} gwei`}>
      <ResponsiveContainer width="100%" height="100%">
        <AreaChart data={data} margin={{ top: 2, right: 2, bottom: 2, left: 2 }}>
          <YAxis hide domain={[min === max ? min * 0.9 : min, max === min ? max * 1.1 : max]} />
          <Area
            type="monotone"
            dataKey="fee"
            stroke="var(--accent)"
            strokeWidth={1.5}
            fill="var(--accent)"
            fillOpacity={0.1}
            isAnimationActive={false}
            dot={false}
            activeDot={false}
          />
        </AreaChart>
      </ResponsiveContainer>
      <span className="sr-only">Latest block {last.n}</span>
    </div>
  );
}

/** Where a unit of gas's fee goes: the floor to the infra account, the rest to the network account. */
export function FeeSplitBar({ snapshot }: { snapshot: LiveSnapshot }) {
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
}

export function LiveStrip({ snapshot, recentBlocks, status }: { snapshot: LiveSnapshot | null; recentBlocks: BlockPoint[]; status: LiveStatus }) {
  const now = useTicker(250);
  if (!snapshot) {
    return (
      <div className="rounded-md border border-hairline bg-surface p-5 text-sm text-ink-2" aria-busy="true">
        <div className="flex items-center justify-between">
          <span>Waiting for the first sample.</span>
          <StatusPill status={status} />
        </div>
      </div>
    );
  }
  const step = rampStep(snapshot.multiplierBips);
  const sinceBlock = Math.max(0, now / 1000 - snapshot.block.ts);
  return (
    <div className="rounded-md border border-hairline bg-surface p-5">
      <div className="grid gap-6 lg:grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)_minmax(0,1fr)]">
        <div className="flex flex-col gap-4">
          <div className="flex flex-wrap items-end gap-x-6 gap-y-3">
            <div>
              <Label>Base fee now</Label>
              <div className="num mt-1 text-5xl leading-none tracking-tight text-ink sm:text-6xl">
                {formatGwei(snapshot.baseFee)}
                <span className="ml-1.5 text-lg font-normal text-ink-2">gwei</span>
              </div>
            </div>
            <div
              className="num rounded-md px-3 py-2 text-2xl leading-none"
              style={{ background: rampColor(snapshot.multiplierBips), color: rampInk(step) }}
              title={`Multiplier over the ${formatGwei(snapshot.minBaseFee)} gwei floor`}
            >
              {bipsToMultiplier(snapshot.multiplierBips)}
              <div className="mt-1 text-[11px] font-medium uppercase tracking-[0.08em] opacity-80">over floor</div>
            </div>
          </div>
          <Sparkline blocks={recentBlocks} />
          <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-ink-3">
            <span>
              last {recentBlocks.length} blocks · floor {formatGwei(snapshot.minBaseFee)} gwei
            </span>
            <StatusPill status={status} />
          </div>
        </div>

        <div className="grid grid-cols-2 gap-x-4 gap-y-5 sm:grid-cols-3 lg:grid-cols-2">
          <Stat label="Block" value={formatInteger(snapshot.block.number)} />
          <Stat label="Since last block" value={sinceBlock.toFixed(1)} unit="s" />
          <Stat label="Gas in last block" value={formatGas(snapshot.block.gasUsed)} hint={`${formatInteger(snapshot.block.txCount)} tx`} />
          <Stat label="Gas/s (10 s)" value={formatGas(snapshot.gasPerSecond.s10)} />
          <Stat label="Gas/s (60 s)" value={formatGas(snapshot.gasPerSecond.s60)} />
          <Stat label="Exponent x" value={(snapshot.exponentBips / 10_000).toFixed(4)} />
        </div>

        <div className="flex flex-col gap-5">
          <div className="grid grid-cols-2 gap-4">
            <Stat label="21k transfer" value={formatEth(costWei(TRANSFER_GAS, snapshot.baseFee), { unit: false })} unit="ETH" size="sm" />
            <Stat label="150k swap" value={formatEth(costWei(SWAP_GAS, snapshot.baseFee), { unit: false })} unit="ETH" size="sm" />
          </div>
          <FeeSplitBar snapshot={snapshot} />
        </div>
      </div>
    </div>
  );
}
