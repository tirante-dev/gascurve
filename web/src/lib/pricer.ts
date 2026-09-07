// A TypeScript mirror of nitro's arbos/l2pricing/model.go and of
// internal/pricer/pricer.go, in integer basis points with BigInt so results
// match the Go pricer bit for bit, including the uint64 and int64 saturation.
// Never "simplify" this file to floating point.

import type { Constraint, LegacyParams } from "@/types";

export const ONE_IN_BIPS = 10_000n;
export const MAX_UINT64 = (1n << 64n) - 1n;
export const MAX_INT64 = (1n << 63n) - 1n;
export const MIN_INT64 = -(1n << 63n);

/** Go SaturatingUAdd: a + b for uint64, saturating at MaxUint64. */
export function saturatingUAdd(a: bigint, b: bigint): bigint {
  const sum = a + b;
  return sum > MAX_UINT64 ? MAX_UINT64 : sum;
}

/** Go SaturatingUSub: a - b for uint64, floored at zero. */
export function saturatingUSub(a: bigint, b: bigint): bigint {
  return b >= a ? 0n : a - b;
}

/** Go SaturatingUMul: a * b for uint64, saturating at MaxUint64. */
export function saturatingUMul(a: bigint, b: bigint): bigint {
  if (a === 0n || b === 0n) return 0n;
  const product = a * b;
  return product > MAX_UINT64 ? MAX_UINT64 : product;
}

/** Go saturatingCastToBips: a uint64 into int64 basis points, capped at MaxInt64. */
export function saturatingCastToBips(v: bigint): bigint {
  return v > MAX_INT64 ? MAX_INT64 : v;
}

/** Go NaturalToBips: v * 10_000 with saturating multiplication and cast. */
export function naturalToBips(v: bigint): bigint {
  return saturatingCastToBips(saturatingUMul(v, ONE_IN_BIPS));
}

/** Go SaturatingAddBips: int64 addition saturating at both bounds. */
export function saturatingAddBips(a: bigint, b: bigint): bigint {
  const sum = a + b;
  if (sum > MAX_INT64) return MAX_INT64;
  if (sum < MIN_INT64) return MIN_INT64;
  return sum;
}

/**
 * nitro ApproxExpBasisPoints: the degree `accuracy` Taylor polynomial of e^x,
 * evaluated with saturating unsigned arithmetic and integer division at every
 * step. Negative x takes the reciprocal branch, accuracy 0 is exactly 1.
 *   res = b + x/accuracy
 *   for i = accuracy-1 .. 1: res = b + res*x/(i*b)
 */
export function approxExpBips(x: bigint, accuracy = 4): bigint {
  if (accuracy === 0) return ONE_IN_BIPS;
  const negative = x < 0n;
  // Go negates in int64 and casts to uint64; -MinInt64 wraps to 2^63.
  const input = BigInt.asUintN(64, negative ? -x : x);
  const acc = BigInt(accuracy);
  const b = ONE_IN_BIPS;
  let res = b + input / acc;
  for (let i = acc - 1n; i > 0n; i--) {
    res = saturatingUAdd(b, saturatingUMul(res, input) / (i * b));
  }
  if (negative) return saturatingCastToBips((b * b) / res);
  return saturatingCastToBips(res);
}

export type ConstraintState = { target: bigint; window: bigint; backlog: bigint };

/** One constraint's exponent contribution in bips: NaturalToBips(backlog) / bips(window * target), as Go Step does. */
export function constraintExponentBips(c: ConstraintState): bigint {
  if (c.backlog <= 0n) return 0n;
  const divisor = saturatingUMul(c.window, c.target);
  if (divisor === 0n) return 0n;
  return naturalToBips(c.backlog) / saturatingCastToBips(divisor);
}

/** Sum of per-constraint contributions, each an integer division as in nitro, saturating. */
export function exponentBips(constraints: readonly ConstraintState[]): bigint {
  let total = 0n;
  for (const c of constraints) total = saturatingAddBips(total, constraintExponentBips(c));
  return total;
}

/** Base fee for an exponent: minBaseFee * P4(exponent) / 10000, or the floor when x <= 0. */
export function baseFeeFromExponent(minBaseFee: bigint, exponent: bigint): bigint {
  if (exponent <= 0n) return minBaseFee;
  return (minBaseFee * approxExpBips(exponent)) / ONE_IN_BIPS;
}

/** Multiplier over the floor in bips for an exponent (10000 = 1x). */
export function multiplierBips(exponent: bigint): bigint {
  return exponent <= 0n ? ONE_IN_BIPS : approxExpBips(exponent);
}

/** True e^x, for the comparison plot against P4. */
export function trueExpMultiplier(x: number): number {
  return Math.exp(x);
}

/** P4(x) evaluated through the integer pricer, as a float multiplier. */
export function p4Multiplier(x: number): number {
  const bips = BigInt(Math.round(x * 10_000));
  return Number(multiplierBips(bips)) / 10_000;
}

/** A JSON number into a uint64: truncated, floored at zero, capped at MaxUint64. */
export function toUint64(value: number): bigint {
  if (!Number.isFinite(value) || value <= 0) return 0n;
  const v = BigInt(Math.trunc(value));
  return v > MAX_UINT64 ? MAX_UINT64 : v;
}

/** API constraints (numbers) to pricer state (bigint). */
export function toState(constraints: readonly Pick<Constraint, "target" | "window" | "backlog">[]): ConstraintState[] {
  return constraints.map((c) => ({ target: toUint64(c.target), window: toUint64(c.window), backlog: toUint64(c.backlog) }));
}

/**
 * Client-side draining between ticks. Fractional dt is allowed here because
 * this only drives the gauge animation; each tick snaps back to sampled values.
 */
export function stepBacklogs(constraints: readonly Pick<Constraint, "target" | "backlog">[], dtSeconds: number): number[] {
  const dt = Math.max(0, dtSeconds);
  return constraints.map((c) => Math.max(0, c.backlog - c.target * dt));
}

/** Exponent contributions per constraint (integer bips) for a list of API constraints. */
export function contributionsBips(constraints: readonly Pick<Constraint, "target" | "window" | "backlog">[]): number[] {
  return toState(constraints).map((c) => Number(constraintExponentBips(c)));
}

/**
 * nitro Step: drain every backlog by dt * target, then compute the exponent and fee.
 * Mutates `constraints`. Returns the fee that applies to the block being opened.
 */
export function step(
  constraints: ConstraintState[],
  dtSeconds: bigint,
  minBaseFee: bigint,
): { baseFee: bigint; exponent: bigint; contributions: bigint[] } {
  const contributions: bigint[] = [];
  let exponent = 0n;
  for (const c of constraints) {
    c.backlog = saturatingUSub(c.backlog, saturatingUMul(dtSeconds, c.target));
    const x = constraintExponentBips(c);
    contributions.push(x);
    exponent = saturatingAddBips(exponent, x);
  }
  return { baseFee: baseFeeFromExponent(minBaseFee, exponent), exponent, contributions };
}

/** nitro AddGas: every constraint absorbs every unit of gas, saturating. Mutates `constraints`. */
export function addGas(constraints: ConstraintState[], gasUsed: bigint): void {
  for (const c of constraints) c.backlog = saturatingUAdd(c.backlog, gasUsed);
}

export type LegacyState = { speedLimit: bigint; inertia: bigint; tolerance: bigint; backlog: bigint };

export function toLegacyState(legacy: Pick<LegacyParams, "speedLimit" | "inertia" | "tolerance" | "backlog">): LegacyState {
  return {
    speedLimit: toUint64(legacy.speedLimit),
    inertia: toUint64(legacy.inertia),
    tolerance: toUint64(legacy.tolerance),
    backlog: toUint64(legacy.backlog),
  };
}

/**
 * Legacy exponent: bips(backlog - tolerance*speedLimit) / (inertia*speedLimit)
 * when above tolerance. Mirrors nitro's updatePricingModelLegacy and the Go
 * pricer exactly: the tolerance threshold is a plain uint64 multiply that
 * wraps on overflow, the excess is cast to bips saturating, and the inertia
 * denominator is a saturating multiply cast to bips saturating. A zero
 * denominator (nitro would panic) yields no exponent.
 */
export function legacyExponentBips(s: LegacyState): bigint {
  const threshold = BigInt.asUintN(64, s.tolerance * s.speedLimit);
  if (s.backlog <= threshold) return 0n;
  const inertia = saturatingCastToBips(saturatingUMul(s.inertia, s.speedLimit));
  if (inertia <= 0n) return 0n;
  return naturalToBips(s.backlog - threshold) / inertia;
}

/** Legacy Step: drain by dt * speedLimit, then price. Mutates `s`. */
export function legacyStep(s: LegacyState, dtSeconds: bigint, minBaseFee: bigint): { baseFee: bigint; exponent: bigint } {
  s.backlog = saturatingUSub(s.backlog, saturatingUMul(dtSeconds, s.speedLimit));
  const exponent = legacyExponentBips(s);
  return { baseFee: baseFeeFromExponent(minBaseFee, exponent), exponent };
}

export function legacyAddGas(s: LegacyState, gasUsed: bigint): void {
  s.backlog = saturatingUAdd(s.backlog, gasUsed);
}
