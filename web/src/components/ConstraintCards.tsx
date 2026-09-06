"use client";

import { useMemo } from "react";
import { useAnimatedBacklogs } from "@/hooks/useAnimatedBacklogs";
import { contributionsBips, legacyExponentBips, toLegacyState } from "@/lib/pricer";
import type { Constraint, LegacyParams, LiveSnapshot } from "@/types";
import { contributionRampStep, seriesColor, sharesOf } from "@/utils/chart";
import { formatDuration, formatGas, formatPercent, formatSecondsOfTarget } from "@/utils/format";
import { Card, Label, Swatch } from "./primitives";

/** A meter whose fill carries magnitude on the sequential ramp; the track is the lightest step. */
export function Gauge({ fraction, step, label, marks = [] }: { fraction: number; step: number; label: string; marks?: number[] }) {
  const pct = Math.max(0, Math.min(1, fraction)) * 100;
  return (
    <div className="relative h-2.5 w-full rounded-sm" style={{ background: "var(--seq-1)" }} role="meter" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(pct)} aria-label={label}>
      <div className="h-full rounded-sm" style={{ width: `${pct}%`, background: `var(--seq-${step})` }} />
      {marks.map((m) => (
        <span key={m} className="absolute top-[-3px] h-4 w-px bg-ink-3" style={{ left: `${Math.min(100, m * 100)}%` }} aria-hidden="true" />
      ))}
    </div>
  );
}

/**
 * Card values derived from the backlogs on screen, so the exponent, the share
 * of x and the ramp colour agree with the animated gauge: integer bips through
 * the pricer, shares from those bips.
 */
export function cardValues(constraints: readonly Constraint[], backlogs: readonly number[]): { bips: number[]; shares: number[]; projected: boolean } {
  const shown = constraints.map((c, i) => ({ target: c.target, window: c.window, backlog: backlogs[i] ?? c.backlog }));
  const bips = contributionsBips(shown);
  const projected = shown.some((s, i) => s.backlog !== constraints[i].backlog);
  return { bips, shares: sharesOf(bips), projected };
}

function ConstraintCard({ c, index, backlog, bips, share, projected }: { c: Constraint; index: number; backlog: number; bips: number; share: number; projected: boolean }) {
  const denominator = c.target * c.window;
  const x = denominator > 0 ? backlog / denominator : 0;
  const scale = Math.max(1, Math.ceil(x));
  const marks = Array.from({ length: scale - 1 }, (_, i) => (i + 1) / scale);
  return (
    <Card>
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <Swatch color={seriesColor(index)} />
          <span className="text-sm font-semibold text-ink">Constraint {index + 1}</span>
        </div>
        <span className="text-xs text-ink-3">{share > 0 ? `${formatPercent(share, 1)} of x` : "no contribution"}</span>
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
          <Label>Backlog{projected ? " (projected)" : ""}</Label>
          <dd className="num mt-0.5 text-ink">{formatGas(backlog)}</dd>
          <dd className="num text-xs text-ink-3">{formatSecondsOfTarget(backlog, c.target)} of target</dd>
        </div>
        <div>
          <Label>
            x{index + 1}
            {projected ? " (projected)" : ""}
          </Label>
          <dd className="num mt-0.5 text-ink">{(bips / 10_000).toFixed(4)}</dd>
          <dd className="num text-xs text-ink-3">backlog / (target × window), {bips.toLocaleString("en-US")} bips</dd>
        </div>
      </dl>
      <div className="mt-4">
        <Gauge fraction={x / scale} step={contributionRampStep(bips)} label={`Constraint ${index + 1} backlog as a fraction of ${scale} window${scale > 1 ? "s" : ""} of target`} marks={marks} />
        <div className="num mt-1 flex justify-between text-[11px] text-ink-3">
          <span>0</span>
          <span>
            {scale} × {formatGas(denominator)} gas
          </span>
        </div>
      </div>
    </Card>
  );
}

function LegacyCard({ legacy, backlog, projected }: { legacy: LegacyParams; backlog: number; projected: boolean }) {
  const tolerance = legacy.tolerance * legacy.speedLimit;
  const inertia = legacy.inertia * legacy.speedLimit;
  const bips = Number(legacyExponentBips(toLegacyState({ ...legacy, backlog })));
  const x = inertia > 0 ? bips / 10_000 : 0;
  const scale = Math.max(2, Math.ceil(backlog / tolerance) + 1);
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
          <dd className="num text-xs text-ink-3">{formatGas(tolerance)} gas free</dd>
        </div>
        <div>
          <Label>Backlog{projected ? " (projected)" : ""}</Label>
          <dd className="num mt-0.5 text-ink">{formatGas(backlog)}</dd>
          <dd className="num text-xs text-ink-3">{formatSecondsOfTarget(backlog, legacy.speedLimit)} of speed limit · x = {x.toFixed(4)}</dd>
        </div>
      </dl>
      <div className="mt-4">
        <Gauge fraction={backlog / (tolerance * scale)} step={contributionRampStep(bips)} label="Legacy backlog against the tolerance threshold" marks={[1 / scale]} />
        <div className="num mt-1 flex justify-between text-[11px] text-ink-3">
          <span>0</span>
          <span>tolerance at {formatGas(tolerance)}</span>
          <span>{formatGas(tolerance * scale)}</span>
        </div>
      </div>
    </Card>
  );
}

const PROJECTION_NOTE = "Between samples the backlogs are a projection: they drain at each target rate and x is recomputed from them, so the numbers, shares and colours stay consistent with the gauges. Every tick snaps back to the sampled state.";

/** One card per constraint, backlogs draining at the target rate between ticks. */
export function ConstraintCards({ snapshot }: { snapshot: LiveSnapshot | null }) {
  const constraints = useMemo<Constraint[]>(() => {
    if (!snapshot) return [];
    if (snapshot.model === "legacy" && snapshot.legacy) {
      return [{ target: snapshot.legacy.speedLimit, window: 0, backlog: snapshot.legacy.backlog, exponentBips: 0 }];
    }
    return snapshot.constraints;
  }, [snapshot]);
  const backlogs = useAnimatedBacklogs(constraints, snapshot?.sampledAt ?? "");
  if (!snapshot) return <p className="text-sm text-ink-2">Waiting for the first sample.</p>;
  if (snapshot.model === "legacy" && snapshot.legacy) {
    const backlog = backlogs[0] ?? snapshot.legacy.backlog;
    return (
      <div>
        <LegacyCard legacy={snapshot.legacy} backlog={backlog} projected={backlog !== snapshot.legacy.backlog} />
        <p className="mt-2 text-xs text-ink-3">{PROJECTION_NOTE}</p>
      </div>
    );
  }
  const values = cardValues(constraints, backlogs);
  const cols = constraints.length >= 4 ? "lg:grid-cols-3" : "lg:grid-cols-2";
  return (
    <div>
      <div className={`grid gap-4 sm:grid-cols-2 ${cols}`}>
        {constraints.map((c, i) => (
          <ConstraintCard key={`${c.target}-${c.window}-${i}`} c={c} index={i} backlog={backlogs[i] ?? c.backlog} bips={values.bips[i]} share={values.shares[i]} projected={values.projected} />
        ))}
      </div>
      <p className="mt-2 text-xs text-ink-3">{PROJECTION_NOTE}</p>
    </div>
  );
}
