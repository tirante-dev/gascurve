import type { Constraint, LiveSnapshot, Series, SeriesPoint } from "@/types";

export const LIVE_STALE_AFTER_SECONDS = 5;
export const DIAGNOSIS_STALE_AFTER_MS = LIVE_STALE_AFTER_SECONDS * 1000;
export const RECENT_CHANGE_SECONDS = 3_600;
export const MATCHED_HISTORY_MIN_COVERAGE = 0.98;

export type DominantPressure = { index: number; contributionBips: number; share: number };
export type FeeChange = { percent: number; seconds: number };
export type DemandComparison = {
  rate: number;
  target: number;
  periodSeconds: number;
  coverage: number;
  source: "snapshot" | "history";
};
export type PressureDirection = "building" | "draining" | "steady" | "clear";

export type Diagnosis = {
  stale: boolean;
  dominant: DominantPressure | null;
  change: FeeChange | null;
  demand: DemandComparison | null;
  direction: PressureDirection | null;
};

function resolutionSeconds(series: Series): number | null {
  switch (series.resolution) {
    case "5s":
      return 5;
    case "1m":
      return 60;
    case "15m":
      return 900;
    case "1h":
      return 3_600;
    default:
      return null;
  }
}

function isCompleteRate(point: SeriesPoint): point is SeriesPoint & { computeGasPerSecond: number } {
  return point.completeness === "complete" && point.coverage === 1 && typeof point.computeGasPerSecond === "number" && Number.isFinite(point.computeGasPerSecond) && point.computeGasPerSecond >= 0;
}

export function dominantPressure(constraints: readonly Constraint[]): DominantPressure | null {
  let total = 0;
  let index = -1;
  let contributionBips = 0;
  for (let i = 0; i < constraints.length; i++) {
    const contribution = constraints[i].exponentBips;
    if (!Number.isSafeInteger(contribution) || contribution < 0) return null;
    total += contribution;
    if (contribution > contributionBips) {
      index = i;
      contributionBips = contribution;
    }
  }
  if (index < 0 || total <= 0 || !Number.isSafeInteger(total)) return null;
  return { index, contributionBips, share: contributionBips / total };
}

export function feeChange(snapshot: LiveSnapshot, series: Series | null, seconds = RECENT_CHANGE_SECONDS): FeeChange | null {
  if (!series) return null;
  const step = resolutionSeconds(series);
  if (step === null) return null;
  const target = snapshot.block.ts - seconds;
  const point = series.points.find((candidate) => candidate.t <= target && target < candidate.t + step);
  if (!point || point.completeness !== "complete" || point.coverage !== 1) return null;
  const before = Number(point.baseFeeAvg);
  const current = Number(snapshot.baseFee);
  if (!Number.isFinite(before) || !Number.isFinite(current) || before <= 0 || current < 0) return null;
  return { percent: ((current - before) / before) * 100, seconds };
}

export function historicalRate(series: Series | null, end: number, seconds: number): DemandComparison | null {
  if (!series || seconds <= 0) return null;
  const step = resolutionSeconds(series);
  if (step === null) return null;
  const start = end - seconds;
  let weighted = 0;
  let covered = 0;
  let cursor = start;
  for (const point of series.points) {
    const from = Math.max(start, point.t, cursor);
    const to = Math.min(end, point.t + step);
    if (to <= from) continue;
    cursor = Math.max(cursor, to);
    if (!isCompleteRate(point)) continue;
    const duration = to - from;
    covered += duration;
    weighted += point.computeGasPerSecond * duration;
  }
  const coverage = Math.min(1, covered / seconds);
  if (coverage < MATCHED_HISTORY_MIN_COVERAGE || covered <= 0) return null;
  return { rate: weighted / covered, target: 0, periodSeconds: seconds, coverage, source: "history" };
}

function snapshotRate(snapshot: LiveSnapshot, window: number): DemandComparison | null {
  const rates = snapshot.computeGasPerSecond;
  if (!rates) return null;
  const use10 = Math.abs(window - 10) <= Math.abs(window - 60);
  const rate = use10 ? rates.s10 : rates.s60;
  if (rate === null || !Number.isFinite(rate) || rate < 0) return null;
  return { rate, target: 0, periodSeconds: use10 ? 10 : 60, coverage: 1, source: "snapshot" };
}

export function demandComparison(snapshot: LiveSnapshot, series: Series | null, dominant: DominantPressure | null): DemandComparison | null {
  if (snapshot.model === "legacy") {
    if (!snapshot.legacy || !Number.isFinite(snapshot.legacy.speedLimit) || snapshot.legacy.speedLimit <= 0) return null;
    const measure = snapshotRate(snapshot, 60);
    return measure ? { ...measure, target: snapshot.legacy.speedLimit } : null;
  }
  if (!dominant) return null;
  const constraint = snapshot.constraints[dominant.index];
  if (!constraint || !Number.isFinite(constraint.target) || constraint.target <= 0 || !Number.isFinite(constraint.window) || constraint.window <= 0) return null;
  const measure = constraint.window > 60 ? historicalRate(series, snapshot.block.ts, constraint.window) : snapshotRate(snapshot, constraint.window);
  return measure ? { ...measure, target: constraint.target } : null;
}

export function pressureDirection(snapshot: LiveSnapshot, dominant: DominantPressure | null, demand: DemandComparison | null): PressureDirection | null {
  if (!demand || !Number.isFinite(demand.rate) || !Number.isFinite(demand.target)) return null;
  if (snapshot.model === "legacy") {
    if (!snapshot.legacy) return null;
    if (!Number.isSafeInteger(snapshot.exponentBips) || snapshot.exponentBips < 0) return null;
    if (snapshot.exponentBips === 0 && demand.rate <= demand.target) return "clear";
    if (demand.rate === demand.target) return snapshot.exponentBips === 0 ? "clear" : "steady";
    return demand.rate > demand.target ? "building" : "draining";
  }
  if (!dominant) return demand.rate > demand.target ? "building" : "clear";
  const backlog = snapshot.constraints[dominant.index]?.backlog ?? 0;
  if (!Number.isFinite(backlog) || backlog < 0) return null;
  if (backlog === 0 && demand.rate <= demand.target) return "clear";
  if (demand.rate === demand.target) return "steady";
  return demand.rate > demand.target ? "building" : "draining";
}

export function diagnose(snapshot: LiveSnapshot, series: Series | null, nowMs: number): Diagnosis {
  const sampledAt = Date.parse(snapshot.sampledAt);
  const stale = Number.isNaN(sampledAt) || nowMs - sampledAt > DIAGNOSIS_STALE_AFTER_MS;
  const dominant = snapshot.model === "constraints" ? dominantPressure(snapshot.constraints) : null;
  if (stale) return { stale, dominant: null, change: null, demand: null, direction: null };
  const demand = demandComparison(snapshot, series, dominant);
  return {
    stale,
    dominant,
    change: feeChange(snapshot, series),
    demand,
    direction: pressureDirection(snapshot, dominant, demand),
  };
}
