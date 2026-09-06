"use client";

import { memo, useMemo } from "react";
import { useLiveFrame, type SmoothedLive } from "@/hooks/useSmoothedLive";
import { AVERAGE_WINDOW_S, isShortWindow, SAWTOOTH_WINDOW_S, sawtoothSamples, SHORT_WINDOW_S, targetValues, type LiveValues, type SawtoothSample } from "@/lib/smoothing";
import type { BlockPoint, Constraint, LegacyParams, LiveSnapshot } from "@/types";
import { constraintGauge, contributionRampStep, legacyGauge, seriesColor } from "@/utils/chart";
import { FIXED_WIDTH_CH, formatDuration, formatGas, formatGasFixed, formatInteger, formatPercent, formatSecondsOfTarget } from "@/utils/format";
import { RESYNC_COPY, WAITING_COPY } from "./LiveStrip";
import { Card, Figure, Label, Swatch } from "./primitives";

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

const SAWTOOTH_W = 200;
const SAWTOOTH_H = 32;

/** The polyline points of a sawtooth sparkline: x by block order, y by backlog against the peak. Exported for tests. */
export function sawtoothPoints(samples: readonly SawtoothSample[], width = SAWTOOTH_W, height = SAWTOOTH_H): string {
  const max = Math.max(1, ...samples.map((s) => s.backlog));
  const last = Math.max(1, samples.length - 1);
  return samples.map((s, i) => `${((i / last) * width).toFixed(1)},${(height - 1 - (s.backlog / max) * (height - 2)).toFixed(1)}`).join(" ");
}

/**
 * The raw per-block backlog of a short window over the last fifteen seconds,
 * drawn as a polyline so the sawtooth stays a sawtooth. Memoised on the
 * samples: they change when blocks arrive, the card re-renders every frame.
 */
export const Sawtooth = memo(function Sawtooth({ samples, color }: { samples: SawtoothSample[]; color: string }) {
  if (samples.length < 2) return <div className="h-8 w-full rounded-sm bg-chart" aria-hidden="true" />;
  const peak = Math.max(...samples.map((s) => s.backlog));
  return (
    <svg
      className="h-8 w-full rounded-sm bg-chart"
      viewBox={`0 0 ${SAWTOOTH_W} ${SAWTOOTH_H}`}
      preserveAspectRatio="none"
      role="img"
      aria-label={`Backlog per block over the last ${SAWTOOTH_WINDOW_S} s, ${samples.length} blocks, peak ${formatGas(peak)} gas`}
    >
      <polyline points={sawtoothPoints(samples)} fill="none" stroke={color} strokeWidth={1.5} strokeLinejoin="round" vectorEffect="non-scaling-stroke" />
    </svg>
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
            {formatGas(c.target)} <span className="text-xs text-ink-2">gas/s</span>
          </dd>
        </div>
        <div>
          <Label>Window</Label>
          <dd className="num mt-0.5 text-ink">{formatDuration(c.window)}</dd>
        </div>
        <div>
          <Label>{short ? `Backlog (avg ${AVERAGE_WINDOW_S} s)` : "Backlog"}</Label>
          <dd className="num mt-0.5 text-ink">
            <Figure ch={FIXED_WIDTH_CH.gas}>{formatGasFixed(backlog)}</Figure>
          </dd>
          <dd className="num text-xs text-ink-3">{formatSecondsOfTarget(backlog, c.target)} of target</dd>
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
          <Sawtooth samples={samples} color={seriesColor(index)} />
          <p className="mt-1 text-[11px] text-ink-3">drains {formatGas(c.target)} gas at each second boundary; bursts show as sawteeth</p>
        </div>
      ) : null}
      <div className="mt-4">
        <Gauge fraction={gauge.fraction} step={contributionRampStep(bips)} label={`Constraint ${index + 1} backlog as a fraction of ${formatInteger(scale)} window${scale > 1 ? "s" : ""} of target`} marks={gauge.marks} />
        <div className="num mt-1 flex justify-between text-[11px] text-ink-3">
          <span>0</span>
          <span>
            {formatInteger(scale)} × {formatGas(denominator)} gas
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
            {formatGas(legacy.speedLimit)} <span className="text-xs text-ink-2">gas/s</span>
          </dd>
        </div>
        <div>
          <Label>Inertia</Label>
          <dd className="num mt-0.5 text-ink">{legacy.inertia}</dd>
        </div>
        <div>
          <Label>Tolerance</Label>
          <dd className="num mt-0.5 text-ink">{legacy.tolerance}</dd>
          <dd className="num text-xs text-ink-3">{gauge.free > 0 ? `${formatGas(gauge.free)} gas free` : "no free gas: every unit prices"}</dd>
        </div>
        <div>
          <Label>Backlog</Label>
          <dd className="num mt-0.5 text-ink">
            <Figure ch={FIXED_WIDTH_CH.gas}>{formatGasFixed(backlog)}</Figure>
          </dd>
          <dd className="num text-xs text-ink-3">{formatSecondsOfTarget(backlog, legacy.speedLimit)} of speed limit · x = {x.toFixed(4)}</dd>
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
