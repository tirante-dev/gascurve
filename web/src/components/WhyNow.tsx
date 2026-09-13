"use client";

import { useTicker } from "@/hooks/useTicker";
import { diagnose } from "@/lib/diagnosis";
import { gapModel } from "@/lib/gaps";
import type { ListenerStatus, LiveSnapshot, LiveStatus, NetworkStatus, Series } from "@/types";
import { bipsToMultiplier, formatAgo, formatDateTime, formatDuration, formatGas, formatGasPerSecond, formatGwei, formatPercent, formatSignificant } from "@/utils/format";
import { StatusPill } from "./primitives";

type HealthLevel = "healthy" | "warning" | "unknown";
type HistoryAssessment = { level: HealthLevel; label: string; summary: string };

function healthState(snapshot: LiveSnapshot | null, stale: boolean, status: NetworkStatus | null, listener: ListenerStatus | null, history: HistoryAssessment): { level: HealthLevel; label: string } {
  if (!snapshot) return { level: "warning", label: "No live sample" };
  if (stale) return { level: "warning", label: "Live data stale" };
  if (listener?.ready === false || status?.status === "degraded") return { level: "warning", label: "Data degraded" };
  if (history.level !== "healthy") return { level: history.level, label: history.label };
  if (status?.status === "healthy" && listener?.ready === true) return { level: "healthy", label: "Data healthy" };
  return { level: "unknown", label: "Live sample current" };
}

function DataHealthPill({ snapshot, stale, status, listener, history }: { snapshot: LiveSnapshot | null; stale: boolean; status: NetworkStatus | null; listener: ListenerStatus | null; history: HistoryAssessment }) {
  const health = healthState(snapshot, stale, status, listener, history);
  const dot = health.level === "healthy" ? "bg-good vw-dot-glow" : health.level === "warning" ? "bg-warning" : "bg-ink-3";
  return (
    <span className="vw-control inline-flex items-center gap-2 px-2.5 py-1 text-xs font-medium" role="status" aria-live="polite">
      <span className={`inline-block h-2 w-2 rounded-full ${dot}`} aria-hidden="true" />
      {health.label}
    </span>
  );
}

function assessHistory(series: Series | null, loading: boolean): HistoryAssessment {
  if (!series) return loading ? { level: "unknown", label: "History loading", summary: "24 h history is loading." } : { level: "warning", label: "History unavailable", summary: "24 h history is unavailable." };
  if (series.points.length === 0) return { level: "warning", label: "No indexed history", summary: "No 24 h history is indexed yet." };
  const unknown = series.points.filter((point) => point.completeness === "unknown").length;
  const partial = series.points.filter((point) => point.completeness === "partial").length;
  const unknownCoverage = series.points.filter((point) => point.completeness === "complete" && (point.coverage === null || !Number.isFinite(point.coverage) || point.coverage < 0 || point.coverage > 1)).length;
  const limitedCoverage = series.points.filter((point) => point.completeness === "complete" && point.coverage !== null && Number.isFinite(point.coverage) && point.coverage >= 0 && point.coverage < 1).length;
  const gaps = gapModel(series, series.points).gaps.length;
  if (unknown > 0 || unknownCoverage > 0) {
    const details = [
      unknown > 0 ? `${unknown} history ${unknown === 1 ? "bucket has" : "buckets have"} unknown completeness` : null,
      unknownCoverage > 0 ? `${unknownCoverage} complete ${unknownCoverage === 1 ? "bucket has" : "buckets have"} unknown or invalid coverage` : null,
    ].filter(Boolean);
    return { level: "warning", label: "History uncertain", summary: `${details.join("; ")}.` };
  }
  if (partial > 0 || limitedCoverage > 0 || gaps > 0) {
    const details = [
      partial > 0 ? `${partial} partial ${partial === 1 ? "bucket" : "buckets"}` : null,
      limitedCoverage > 0 ? `${limitedCoverage} ${limitedCoverage === 1 ? "bucket has" : "buckets have"} limited coverage` : null,
      gaps > 0 ? `${gaps} unindexed ${gaps === 1 ? "interval" : "intervals"}` : null,
    ].filter(Boolean);
    return { level: "warning", label: "History partial", summary: `24 h history is incomplete: ${details.join(", ")}.` };
  }
  return { level: "healthy", label: "Data healthy", summary: "24 h history is fully indexed." };
}

function operationalHealth(status: NetworkStatus | null, listener: ListenerStatus | null): string {
  const parts: string[] = [];
  if (listener?.ready === false) parts.push("the API update listener is unavailable");
  if (status?.degradedReasons.length) parts.push(status.degradedReasons.join("; "));
  if (status?.holes.checkpointError) parts.push("missing-history state is unreadable");
  else if (status?.holes.blocks) parts.push(`${status.holes.blocks.toLocaleString("en-US")} blocks are not indexed`);
  if (status?.capacity.checkpointError) parts.push("RPC capacity state is unreadable");
  else if (status?.capacity.saturated) parts.push("RPC capacity is saturated");
  if (status && status.endpoints.length > 0) {
    const active = status.endpoints.find((endpoint) => endpoint.index === status.activeEndpoint);
    parts.push(active ? `source endpoint ${active.index + 1} of ${status.endpoints.length}` : "active endpoint state is inconsistent");
  }
  return parts.length > 0 ? parts.join(". ") + "." : status ? "Collector health checks report no degradation." : "Full collector health is unavailable.";
}

function recentChangeText(percent: number, seconds: number): string {
  const direction = percent > 0 ? "up" : percent < 0 ? "down" : "unchanged";
  if (direction === "unchanged") return `Unchanged from the complete bucket ${formatDuration(seconds)} ago.`;
  return `${direction === "up" ? "Up" : "Down"} ${formatSignificant(Math.abs(percent), 3)}% from the complete bucket ${formatDuration(seconds)} ago.`;
}

function demandDeltaText(rate: number, target: number): string {
  const delta = rate - target;
  if (delta === 0) return "at target";
  return `${formatGasPerSecond(Math.abs(delta))} ${delta > 0 ? "above" : "below"} target`;
}

function directionText(snapshot: LiveSnapshot, direction: ReturnType<typeof diagnose>["direction"], demand: ReturnType<typeof diagnose>["demand"]): string {
  if (!direction || !demand) return "Building or draining is unavailable without fresh, matched demand.";
  const scenario = `Deterministic scenario: assumes the measured ${formatDuration(demand.periodSeconds)} load continues. Not a forecast.`;
  if (snapshot.model === "legacy" && snapshot.exponentBips === 0 && direction === "building") return `The legacy backlog is building toward fee pressure. ${scenario}`;
  if (direction === "clear") return `Fee pressure is clear at the measured rate. ${scenario}`;
  return `At the measured rate, pressure is ${direction}. ${scenario}`;
}

function hasKnownConstraintPressure(snapshot: LiveSnapshot): boolean {
  if (snapshot.model !== "constraints" || snapshot.constraints.length === 0) return false;
  let total = 0;
  for (const constraint of snapshot.constraints) {
    if (!Number.isSafeInteger(constraint.exponentBips) || constraint.exponentBips < 0) return false;
    total += constraint.exponentBips;
    if (!Number.isSafeInteger(total)) return false;
  }
  return true;
}

export function WhyNow({ snapshot, series, seriesLoading, liveStatus, networkStatus, listener = null, now }: { snapshot: LiveSnapshot | null; series: Series | null; seriesLoading: boolean; liveStatus: LiveStatus; networkStatus: NetworkStatus | null; listener?: ListenerStatus | null; now?: number }) {
  const ticked = useTicker(now === undefined ? 1000 : 0);
  const clock = now ?? ticked;
  const diagnosis = snapshot ? diagnose(snapshot, series, clock) : null;
  const sampledAgo = snapshot ? Math.max(0, (clock - Date.parse(snapshot.sampledAt)) / 1000) : null;
  const constraint = diagnosis?.dominant && snapshot ? snapshot.constraints[diagnosis.dominant.index] : null;
  const modelTarget = snapshot?.model === "legacy" ? "legacy speed limit" : diagnosis?.dominant ? `C${diagnosis.dominant.index + 1} target` : "relevant target";
  const direction = diagnosis?.direction;
  const history = assessHistory(series, seriesLoading);
  const constraintPressureKnown = snapshot ? hasKnownConstraintPressure(snapshot) : false;

  return (
    <section id="why-now" className="vw-card vw-lit mb-4 p-4 sm:p-5" aria-labelledby="why-now-title">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <div className="text-[11px] font-medium uppercase tracking-[0.14em] text-label">Answer first</div>
          <h2 id="why-now-title" className="mt-1 text-xl font-bold tracking-tight text-ink">Why now?</h2>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <DataHealthPill snapshot={snapshot} stale={diagnosis?.stale ?? false} status={networkStatus} listener={listener} history={history} />
          <StatusPill status={liveStatus} />
        </div>
      </header>

      {!snapshot || !diagnosis ? (
        <div className="mt-4 text-sm text-ink-2">
          <p>The current fee, its pressure source, recent change, matched demand, and direction will appear after the first live sample.</p>
          <p className="mt-2 text-xs text-ink-3">{operationalHealth(networkStatus, listener)} {history.summary}</p>
        </div>
      ) : (
        <>
          <p className="mt-4 text-base font-semibold text-ink">
            {diagnosis.stale ? "Latest indexed base fee" : "Current base fee"}: <span className="num">{formatGwei(snapshot.baseFee)} gwei</span>, <span className="num">{Number.isSafeInteger(snapshot.multiplierBips) ? bipsToMultiplier(snapshot.multiplierBips) : "unavailable"}</span> the <span className="num">{formatGwei(snapshot.minBaseFee)} gwei</span> floor.
          </p>
          <dl className="mt-4 grid gap-4 text-sm sm:grid-cols-2 lg:grid-cols-4">
            <div>
              <dt className="text-[11px] font-medium uppercase tracking-[0.1em] text-label">Recent change</dt>
              <dd className="mt-1 text-ink-2">{diagnosis.change ? recentChangeText(diagnosis.change.percent, diagnosis.change.seconds) : diagnosis.stale ? "Unavailable while the live sample is stale." : "Unavailable without a complete comparison bucket."}</dd>
            </div>
            <div>
              <dt className="text-[11px] font-medium uppercase tracking-[0.1em] text-label">Pressure source</dt>
              <dd className="mt-1 text-ink-2">
                {diagnosis.stale
                  ? "Pressure source is unavailable while the live sample is stale."
                  : snapshot.model === "legacy"
                  ? !snapshot.legacy
                    ? "Legacy backlog and its fee-pressure contribution are unavailable."
                    : !Number.isSafeInteger(snapshot.exponentBips) || snapshot.exponentBips < 0
                      ? `Legacy backlog ${formatGas(snapshot.legacy.backlog)}. Its fee-pressure contribution is unavailable.`
                      : snapshot.exponentBips === 0
                        ? `Legacy backlog ${formatGas(snapshot.legacy.backlog)}. Current sampled x is zero, so it contributes no fee pressure.`
                        : `Legacy backlog ${formatGas(snapshot.legacy.backlog)} is the sole source of x, so no constraint share applies.`
                  : diagnosis.dominant && constraint
                    ? `C${diagnosis.dominant.index + 1}, the ${formatDuration(constraint.window)} window, contributes ${formatPercent(diagnosis.dominant.share)} of x.`
                    : constraintPressureKnown
                      ? "No constraint currently contributes backlog pressure."
                      : "Constraint pressure is unavailable."}
              </dd>
            </div>
            <div>
              <dt className="text-[11px] font-medium uppercase tracking-[0.1em] text-label">Demand vs target</dt>
              <dd className="mt-1 text-ink-2">
                {diagnosis.demand
                  ? `${formatDuration(diagnosis.demand.periodSeconds)} compute rate ${formatGasPerSecond(diagnosis.demand.rate)} vs ${formatGasPerSecond(diagnosis.demand.target)} ${modelTarget}${diagnosis.demand.coverage < 1 ? `, from ${formatPercent(diagnosis.demand.coverage)} complete coverage` : ""}; ${demandDeltaText(diagnosis.demand.rate, diagnosis.demand.target)}.`
                  : constraint && constraint.window > 60
                    ? `No matching complete ${formatDuration(constraint.window)} rate is available. The 60 s burst rate is not substituted.`
                    : "No matching compute rate is available."}
              </dd>
            </div>
            <div>
              <dt className="text-[11px] font-medium uppercase tracking-[0.1em] text-label">Direction</dt>
              <dd className="mt-1 text-ink-2">
                {directionText(snapshot, direction ?? null, diagnosis.demand)}
              </dd>
            </div>
          </dl>
          <p className="mt-4 border-t border-hairline pt-3 text-xs leading-relaxed text-ink-3">
            As of {formatDateTime(snapshot.sampledAt)} Local ({formatAgo(sampledAgo ?? 0)}). {operationalHealth(networkStatus, listener)} {history.summary}
          </p>
        </>
      )}
    </section>
  );
}
