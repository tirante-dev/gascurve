"use client";

import { memo, useMemo } from "react";
import { CartesianGrid, Line, LineChart, ReferenceLine, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { useLiveFrame, type SmoothedLive } from "@/hooks/useSmoothedLive";
import { AVERAGE_WINDOW_S, isShortWindow, SAWTOOTH_WINDOW_S, sawtoothChart, sawtoothSamples, SHORT_WINDOW_S, targetValues, type LiveValues, type SawtoothSample } from "@/lib/smoothing";
import type { BlockPoint, Constraint, LegacyParams, LiveSnapshot } from "@/types";
import { constraintGauge, contributionRampStep, legacyGauge, seriesColor } from "@/utils/chart";
import { FIXED_WIDTH_CH, formatDrainEquivalence, formatDuration, formatGas, formatGasPerSecond, formatInteger, formatPercent, gasParts, gasPerSecondParts, unbroken } from "@/utils/format";
import { ChartTooltip, type TooltipRow } from "./ChartTooltip";
import { RESYNC_COPY, WAITING_COPY } from "./LiveHero";
import { Card, ChartFrame, Figure, Label, Swatch } from "./primitives";

/** A meter whose fill carries magnitude on the sequential ramp; the track is an inset of the surface. */
export function Gauge({ fraction, step, label, marks = [] }: { fraction: number; step: number; label: string; marks?: number[] }) {
  const pct = (Number.isFinite(fraction) ? Math.max(0, Math.min(1, fraction)) : 0) * 100;
  return (
    <div className="relative h-2.5 w-full rounded-sm bg-track" role="meter" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(pct)} aria-label={label}>
      <div className="h-full rounded-sm" style={{ width: `${pct}%`, background: `var(--seq-${step})` }} />
      {marks.map((m) => (
        <span key={m} className="absolute top-[-3px] h-4 w-px bg-ink-3" style={{ left: `${Math.min(100, m * 100)}%` }} aria-hidden="true" />
      ))}
    </div>
  );
}

/** What a backlog figure is, in one line, wherever one is shown. */
export const BACKLOG_TITLE = "gas above the target rate that has not yet drained";

/** "drains 60 Mgas/s at each second": what the dashed line on the chart is. */
export function drainLabel(target: number): string {
  return `drains ${formatGasPerSecond(target)} at each second`;
}

/** Gas ticks carry their unit ("30 Mgas"), so the axis reserves the width for one. */
const GAS_AXIS_WIDTH = 62;

/** Y ticks of the backlog chart: zero, the midpoint and the top, so the scale reads in gas without crowding a card. */
export function backlogTicks(max: number): number[] {
  return [0, max / 2, max];
}

/** The average and the threshold are drawn in ink, never in a constraint colour: a short window can sit at any index in the set. */
const AVERAGE_COLOR = "var(--ink-2)";
const THRESHOLD_COLOR = "var(--ink-3)";

/** A point's place on the axis, read out: "3.4 s ago", or "now" for the newest block. */
export function secondsAgoLabel(x: number): string {
  return x >= 0 ? "now" : `${Math.abs(x).toFixed(1)} s ago`;
}

/** What a hovered block says: which block it was, what it carried, the backlog it left and the average that includes it. */
export function sawtoothTooltipRows(color: string): TooltipRow[] {
  return [
    { label: "block", value: (r) => formatInteger(Number(r.number)) },
    { label: "gas used", value: (r) => formatGas(Number(r.gasUsed)) },
    { label: "backlog", color, value: (r) => formatGas(Number(r.backlog)) },
    { label: `${AVERAGE_WINDOW_S} s average`, color: AVERAGE_COLOR, value: (r) => formatGas(Number(r.average)) },
  ];
}

/** Tall enough for a labelled threshold and two axes without crowding the card. */
const SAWTOOTH_HEIGHT = 132;

/** X ticks over the sawtooth span, in seconds before now. */
const SAWTOOTH_TICKS = [-SAWTOOTH_WINDOW_S, -10, -5, 0];

/**
 * The short-window backlog over the last fifteen seconds: the raw per-block
 * sawtooth, the 2 s average the card's figure shows, and a dashed line at one
 * second of target, the gas the constraint sheds at every second boundary.
 * Memoised on the samples: they change when blocks arrive, the card
 * re-renders every frame.
 */
export const Sawtooth = memo(function Sawtooth({ samples, color, target, index }: { samples: SawtoothSample[]; color: string; target: number; index: number }) {
  const lastTs = samples.length > 0 ? samples[samples.length - 1].ts : 0;
  const data = useMemo(() => sawtoothChart(samples, lastTs), [samples, lastTs]);
  if (data.length < 2) return <div className="w-full rounded-sm bg-chart" style={{ height: SAWTOOTH_HEIGHT }} aria-hidden="true" />;
  const peak = Math.max(...data.map((d) => d.backlog));
  const max = Math.max(peak, target);
  return (
    <ChartFrame
      height={SAWTOOTH_HEIGHT}
      minWidth={260}
      label={`Constraint ${index + 1} backlog per block over the last ${SAWTOOTH_WINDOW_S} s, ${data.length} blocks, 0 to ${formatGas(max)}, with the ${AVERAGE_WINDOW_S} s average and a dashed threshold at ${formatGas(target)}: it ${drainLabel(target)} boundary`}
    >
      <ResponsiveContainer width="100%" height="100%">
        <LineChart data={data} margin={{ top: 8, right: 8, bottom: 2, left: 0 }}>
          <CartesianGrid vertical={false} />
          <XAxis
            dataKey="x"
            type="number"
            domain={[-SAWTOOTH_WINDOW_S, 0]}
            ticks={SAWTOOTH_TICKS}
            tickFormatter={(v: number) => (v === 0 ? "now" : `${v}s`)}
            tickLine
            axisLine={false}
            height={18}
          />
          <YAxis domain={[0, max]} ticks={backlogTicks(max)} tickFormatter={(v: number) => unbroken(formatGas(v))} tickLine={false} axisLine={false} width={GAS_AXIS_WIDTH} />
          {/* One second of target: the gas the constraint sheds at every second boundary. */}
          <ReferenceLine y={target} stroke={THRESHOLD_COLOR} strokeDasharray="4 3" strokeWidth={1} label={{ value: drainLabel(target), position: "insideTopRight" }} />
          <Tooltip isAnimationActive={false} content={(props) => <ChartTooltip {...props} title={secondsAgoLabel} rows={sawtoothTooltipRows(color)} />} />
          <Line type="linear" dataKey="backlog" stroke={color} strokeWidth={1.25} dot={false} isAnimationActive={false} activeDot={{ r: 2.5 }} />
          <Line type="monotone" dataKey="average" stroke={AVERAGE_COLOR} strokeWidth={1} dot={false} isAnimationActive={false} activeDot={false} />
        </LineChart>
      </ResponsiveContainer>
    </ChartFrame>
  );
});

function ConstraintCard({ c, index, backlog, bips, share, samples }: { c: Constraint; index: number; backlog: number; bips: number; share: number; samples: SawtoothSample[] | null }) {
  // The gauge spans whole windows of target; marks are capped so a huge
  // backlog over a tiny window cannot ask for a billion elements.
  const gauge = constraintGauge(c, backlog);
  const { scale, denominator } = gauge;
  const short = samples !== null;
  return (
    <Card>
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <Swatch color={seriesColor(index)} />
          <span className="text-sm font-semibold text-ink">Constraint {index + 1}</span>
        </div>
        <span className="text-xs text-ink-3">
          {share > 0 ? (
            <>
              <Figure ch={5}>{formatPercent(share, 1)}</Figure> of x
            </>
          ) : (
            "no contribution"
          )}
        </span>
      </div>
      <dl className="mt-3 grid grid-cols-2 gap-x-4 gap-y-3">
        <div>
          <Label>Target</Label>
          <dd className="num mt-0.5 text-ink">
            {gasPerSecondParts(c.target).value} <span className="text-xs text-ink-2">{gasPerSecondParts(c.target).unit}</span>
          </dd>
        </div>
        <div>
          <Label>Window</Label>
          <dd className="num mt-0.5 text-ink">{formatDuration(c.window)}</dd>
        </div>
        <div title={BACKLOG_TITLE}>
          <Label>{short ? `Backlog (avg ${AVERAGE_WINDOW_S} s)` : "Backlog"}</Label>
          <dd className="num mt-0.5 text-ink">
            <Figure ch={FIXED_WIDTH_CH.gas}>{gasParts(backlog, true).value}</Figure>{" "}
            <span className="text-xs text-ink-2">{gasParts(backlog, true).unit}</span>
          </dd>
          {/* What the figure means: how long the chain must run at exactly the target to drain it. */}
          <dd className="num text-xs text-ink-3">{formatDrainEquivalence(backlog, c.target)}</dd>
        </div>
        <div>
          <Label>x{index + 1}</Label>
          <dd className="num mt-0.5 text-ink">
            <Figure ch={FIXED_WIDTH_CH.x}>{(bips / 10_000).toFixed(4)}</Figure>
          </dd>
          <dd className="num text-xs text-ink-3">backlog / (target × window), {Math.round(bips).toLocaleString("en-US")} bips</dd>
        </div>
      </dl>
      {samples ? (
        <div className="mt-4">
          <Sawtooth samples={samples} color={seriesColor(index)} target={c.target} index={index} />
          <p className="mt-1 text-[11px] text-ink-3">{drainLabel(c.target)} boundary; bursts show as sawteeth. The thin line is the {AVERAGE_WINDOW_S} s average.</p>
        </div>
      ) : null}
      <div className="mt-4">
        <Gauge fraction={gauge.fraction} step={contributionRampStep(bips)} label={`Constraint ${index + 1} backlog as a fraction of ${formatInteger(scale)} window${scale > 1 ? "s" : ""} of target`} marks={gauge.marks} />
        <div className="num mt-1 flex justify-between text-[11px] text-ink-3">
          <span>0</span>
          <span>
            {formatInteger(scale)} × {formatGas(denominator)}
          </span>
        </div>
      </div>
    </Card>
  );
}

function LegacyCard({ legacy, backlog, bips }: { legacy: LegacyParams; backlog: number; bips: number }) {
  const gauge = legacyGauge(legacy, backlog);
  const x = bips / 10_000;
  return (
    <Card>
      <div className="flex items-center gap-2">
        <Swatch color={seriesColor(0)} />
        <span className="text-sm font-semibold text-ink">Legacy pricer (no constraints configured)</span>
      </div>
      <dl className="mt-3 grid grid-cols-2 gap-x-4 gap-y-3 sm:grid-cols-4">
        <div>
          <Label>Speed limit</Label>
          <dd className="num mt-0.5 text-ink">
            {gasPerSecondParts(legacy.speedLimit).value} <span className="text-xs text-ink-2">{gasPerSecondParts(legacy.speedLimit).unit}</span>
          </dd>
        </div>
        <div>
          <Label>Inertia</Label>
          <dd className="num mt-0.5 text-ink">{legacy.inertia}</dd>
        </div>
        <div>
          <Label>Tolerance</Label>
          <dd className="num mt-0.5 text-ink">{legacy.tolerance}</dd>
          <dd className="num text-xs text-ink-3">{gauge.free > 0 ? `${formatGas(gauge.free)} free` : "no free gas: every unit prices"}</dd>
        </div>
        <div title={BACKLOG_TITLE}>
          <Label>Backlog</Label>
          <dd className="num mt-0.5 text-ink">
            <Figure ch={FIXED_WIDTH_CH.gas}>{gasParts(backlog, true).value}</Figure>{" "}
            <span className="text-xs text-ink-2">{gasParts(backlog, true).unit}</span>
          </dd>
          {/* The legacy pricer drains at the speed limit, so that is the rate the equivalence quotes. */}
          <dd className="num text-xs text-ink-3">{formatDrainEquivalence(backlog, legacy.speedLimit)} · x = {x.toFixed(4)}</dd>
        </div>
      </dl>
      <div className="mt-4">
        <Gauge fraction={gauge.fraction} step={contributionRampStep(bips)} label={gauge.free > 0 ? "Legacy backlog against the tolerance threshold" : "Legacy backlog in units of inertia times speed limit"} marks={gauge.marks} />
        <div className="num mt-1 flex justify-between text-[11px] text-ink-3">
          <span>0</span>
          <span>{gauge.free > 0 ? `tolerance at ${formatGas(gauge.free)}` : gauge.unit > 0 ? `x = 1 at ${formatGas(gauge.unit)}` : "no scale (zero inertia or speed limit)"}</span>
          <span>{gauge.span > 0 ? formatGas(gauge.span) : "n/a"}</span>
        </div>
      </div>
    </Card>
  );
}

const MOTION_NOTE = "Figures ease toward each sample over about 300 ms. Long windows keep draining at their target rate between samples, and x and the shares are recomputed from each frame's backlogs, so the numbers, shares and gauges stay consistent. An owner action that changes the constraints snaps everything: the old figures no longer mean anything under the new definition.";
const SAWTOOTH_NOTE = `Windows of ${formatDuration(SHORT_WINDOW_S)} or less are shown as a ${AVERAGE_WINDOW_S} s average: nitro pays a backlog down only when the block timestamp advances, so within one second every block adds gas and the whole second's drain lands at once. The raw sparkline shows that sawtooth.`;

/** One card per constraint, subscribed to the frame store: the figures move every frame, the page around them does not. */
export function ConstraintCards({ live }: { live: SmoothedLive }) {
  const frame = useLiveFrame(live.frame);
  return <ConstraintCardsView snapshot={live.display} values={frame.values} blocks={frame.blocks} resyncing={live.resyncing} />;
}

/** The cards with everything they show as plain props; until the first frame has eased values the sample stands in. */
export function ConstraintCardsView({ snapshot, values, blocks, resyncing = false }: { snapshot: LiveSnapshot | null; values: LiveValues | null; blocks: BlockPoint[]; resyncing?: boolean }) {
  const samples = useMemo(() => {
    if (!snapshot || snapshot.model === "legacy") return [];
    return snapshot.constraints.map((c, i) => (isShortWindow(c.window) ? sawtoothSamples(blocks, i, snapshot.block.ts) : null));
  }, [snapshot, blocks]);
  if (!snapshot) return <p className="text-sm text-ink-2">{resyncing ? RESYNC_COPY : WAITING_COPY}</p>;
  const v = values ?? targetValues(snapshot, blocks, 0);
  if (snapshot.model === "legacy" && snapshot.legacy) {
    return (
      <div>
        <LegacyCard legacy={snapshot.legacy} backlog={v.backlogs[0] ?? snapshot.legacy.backlog} bips={v.bips[0] ?? 0} />
        <p className="mt-2 text-xs text-ink-3">{MOTION_NOTE}</p>
      </div>
    );
  }
  const constraints = snapshot.constraints;
  const cols = constraints.length >= 4 ? "lg:grid-cols-3" : "lg:grid-cols-2";
  const anyShort = samples.some((s) => s !== null);
  return (
    <div>
      <div className={`grid gap-4 sm:grid-cols-2 ${cols}`}>
        {constraints.map((c, i) => (
          <ConstraintCard key={`${c.target}-${c.window}-${i}`} c={c} index={i} backlog={v.backlogs[i] ?? c.backlog} bips={v.bips[i] ?? c.exponentBips} share={v.shares[i] ?? 0} samples={samples[i] ?? null} />
        ))}
      </div>
      <p className="mt-2 text-xs text-ink-3">
        {MOTION_NOTE}
        {anyShort ? ` ${SAWTOOTH_NOTE}` : ""}
      </p>
    </div>
  );
}
