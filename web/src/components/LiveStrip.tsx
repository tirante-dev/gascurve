"use client";

import { memo, useMemo } from "react";
import { Area, AreaChart, ResponsiveContainer, YAxis } from "recharts";
import { useLiveFrame, type SmoothedLive } from "@/hooks/useSmoothedLive";
import { SWAP_GAS, targetValues, TRANSFER_GAS, type LiveValues } from "@/lib/smoothing";
import type { BlockPoint, LiveSnapshot, LiveStatus } from "@/types";
import { rampColor, rampInk, rampStep } from "@/utils/chart";
import {
  FIXED_WIDTH_CH,
  formatEthFixed,
  formatGasFixed,
  formatGasPerSecondFixed,
  formatGwei,
  formatGweiFixed,
  formatInteger,
  formatMultiplierFixed,
  formatPercent,
  weiToGweiNumber,
} from "@/utils/format";
import { Figure, Label, Stat, StatusPill } from "./primitives";

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

/**
 * Sparkline of the base fee over the block ring. One series, so no legend.
 * Memoised: the strip re-renders every frame while its figures move, the
 * ring changes only when blocks arrive.
 */
export const Sparkline = memo(function Sparkline({ blocks }: { blocks: BlockPoint[] }) {
  const data = useMemo(() => blocks.map((b) => ({ n: b.number, fee: weiToGweiNumber(b.baseFee) })), [blocks]);
  if (data.length < 2) return <div className="h-12 w-full rounded bg-chart" aria-hidden="true" />;
  const last = data[data.length - 1];
  const min = Math.min(...data.map((d) => d.fee));
  const max = Math.max(...data.map((d) => d.fee));
  return (
    <div className="h-12 w-full rounded-sm bg-chart" role="img" aria-label={`Base fee over the last ${data.length} blocks, from ${formatGwei(blocks[0].baseFee)} to ${formatGwei(blocks[blocks.length - 1].baseFee)} gwei`}>
      <ResponsiveContainer width="100%" height="100%">
        <AreaChart data={data} margin={{ top: 2, right: 2, bottom: 2, left: 2 }}>
          <YAxis hide domain={[min === max ? min * 0.9 : min, max === min ? max * 1.1 : max]} />
          <Area
            type="monotone"
            dataKey="fee"
            stroke="var(--accent)"
            strokeWidth={1.5}
            fill="var(--accent)"
            fillOpacity={0.12}
            isAnimationActive={false}
            dot={false}
            activeDot={false}
          />
        </AreaChart>
      </ResponsiveContainer>
      <span className="sr-only">Latest block {last.n}</span>
    </div>
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

/** Copy for an empty strip: a reorg took the last state away, or nothing has arrived yet. */
export const RESYNC_COPY = "Resyncing after a reorg.";
export const WAITING_COPY = "Waiting for the first sample.";

/** The live strip, subscribed to the frame store so its figures move every frame while the page around it does not. */
export function LiveStrip({ live, status }: { live: SmoothedLive; status: LiveStatus }) {
  const frame = useLiveFrame(live.frame);
  return <LiveStripView snapshot={live.display} values={frame.values} blocks={frame.blocks} nowMs={frame.nowMs} status={status} resyncing={live.resyncing} />;
}

/**
 * The strip with everything it shows as plain props. `snapshot` is the
 * display snapshot (the render cadence): block number, gas in the block and
 * the freshness follow it. `values` are the eased figures; until the first
 * frame has produced them the sample stands in. With no snapshot the strip
 * says which kind of nothing it is: a reorg that took the last canonical
 * state away, or a feed that has not delivered one yet.
 */
export function LiveStripView({ snapshot, values, blocks, nowMs, status, resyncing = false }: { snapshot: LiveSnapshot | null; values: LiveValues | null; blocks: BlockPoint[]; nowMs: number; status: LiveStatus; resyncing?: boolean }) {
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
  return (
    <div className="vw-card p-5">
      <div className="grid grid-cols-[minmax(0,1fr)] gap-6 lg:grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)_minmax(0,1fr)]">
        <div className="flex flex-col gap-4">
          <div className="flex flex-wrap items-end gap-x-6 gap-y-3">
            <div>
              <Label>Base fee now</Label>
              <div className="num mt-1 text-5xl leading-none tracking-tight text-ink sm:text-6xl">
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
          <Sparkline blocks={blocks} />
          <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-ink-3">
            <span>
              last {blocks.length} blocks · floor {formatGwei(snapshot.minBaseFee)} gwei
            </span>
            <StatusPill status={status} />
          </div>
        </div>

        <div className="grid grid-cols-2 gap-x-4 gap-y-5 sm:grid-cols-3 lg:grid-cols-2">
          <Stat label="Block" value={formatInteger(snapshot.block.number)} />
          <Freshness sinceBlock={sinceBlock} age={age} />
          <Stat label="Gas in last block" value={<Figure ch={FIXED_WIDTH_CH.gas}>{formatGasFixed(snapshot.block.gasUsed)}</Figure>} hint={`${formatInteger(snapshot.block.txCount)} tx`} />
          <Stat label="Gas/s (10 s)" value={<Figure ch={FIXED_WIDTH_CH.gasPerSecond}>{formatGasPerSecondFixed(v.gasPerSecond10)}</Figure>} unit="M" />
          <Stat label="Gas/s (60 s)" value={<Figure ch={FIXED_WIDTH_CH.gasPerSecond}>{formatGasPerSecondFixed(v.gasPerSecond60)}</Figure>} unit="M" />
          <Stat label="Exponent x" value={<Figure ch={FIXED_WIDTH_CH.x}>{v.exponent.toFixed(4)}</Figure>} />
        </div>

        <div className="flex flex-col gap-5">
          <div className="grid grid-cols-2 gap-4">
            <Stat label="21k transfer" value={<Figure ch={FIXED_WIDTH_CH.eth}>{formatEthFixed(v.transferEth)}</Figure>} unit="ETH" size="sm" />
            <Stat label="150k swap" value={<Figure ch={FIXED_WIDTH_CH.eth}>{formatEthFixed(v.swapEth)}</Figure>} unit="ETH" size="sm" />
          </div>
          <FeeSplitBar snapshot={snapshot} />
        </div>
      </div>
    </div>
  );
}
