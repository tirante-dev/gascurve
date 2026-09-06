// A TypeScript mirror of nitro's arbos/l2pricing/model.go, in integer basis
// points with BigInt so results match the Go pricer bit for bit. Never
// "simplify" this file to floating point.

import type { Constraint, LegacyParams } from "@/types";

export const ONE_IN_BIPS = 10_000n;

/**
 * nitro ApproxExpBasisPoints: the degree `accuracy` Taylor polynomial of e^x,
 * evaluated with integer division at every step.
 *   res = b + x/accuracy
 *   for i = accuracy-1 .. 1: res = b + res*x/(i*b)
 */
export function approxExpBips(x: bigint, accuracy = 4): bigint {
  const b = ONE_IN_BIPS;
  const acc = BigInt(accuracy);
  let res = b + x / acc;
  for (let i = accuracy - 1; i >= 1; i--) {
    res = b + (res * x) / (BigInt(i) * b);
  }
  return res;
}

export type ConstraintState = { target: bigint; window: bigint; backlog: bigint };

/** One constraint's exponent contribution in bips: backlog / (window * target). */
export function constraintExponentBips(c: ConstraintState): bigint {
  if (c.backlog <= 0n) return 0n;
  const denominator = c.window * c.target;
  if (denominator <= 0n) return 0n;
  return (c.backlog * ONE_IN_BIPS) / denominator;
}

/** Sum of per-constraint contributions, each an integer division as in nitro. */
export function exponentBips(constraints: readonly ConstraintState[]): bigint {
  let total = 0n;
  for (const c of constraints) total += constraintExponentBips(c);
  return total;
}

/** Base fee for an exponent: minBaseFee * P4(exponent) / 10000, or the floor when x = 0. */
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

/** API constraints (numbers) to pricer state (bigint). */
export function toState(constraints: readonly Pick<Constraint, "target" | "window" | "backlog">[]): ConstraintState[] {
  return constraints.map((c) => ({
    target: BigInt(Math.trunc(c.target)),
    window: BigInt(Math.trunc(c.window)),
    backlog: BigInt(Math.trunc(Math.max(0, c.backlog))),
  }));
}

/**
 * Client-side draining between ticks. Fractional dt is allowed here because
 * this only drives the gauge animation; each tick snaps back to sampled values.
 */
export function stepBacklogs(constraints: readonly Pick<Constraint, "target" | "backlog">[], dtSeconds: number): number[] {
  const dt = Math.max(0, dtSeconds);
  return constraints.map((c) => Math.max(0, c.backlog - c.target * dt));
}

/** Exponent contributions per constraint for a list of API constraints. */
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
    const drained = dtSeconds * c.target;
    c.backlog = c.backlog > drained ? c.backlog - drained : 0n;
    const x = constraintExponentBips(c);
    contributions.push(x);
    exponent += x;
  }
  return { baseFee: baseFeeFromExponent(minBaseFee, exponent), exponent, contributions };
}

/** nitro AddGas: every constraint absorbs every unit of gas. Mutates `constraints`. */
export function addGas(constraints: ConstraintState[], gasUsed: bigint): void {
  for (const c of constraints) c.backlog += gasUsed;
}

export type LegacyState = { speedLimit: bigint; inertia: bigint; tolerance: bigint; backlog: bigint };

export function toLegacyState(legacy: Pick<LegacyParams, "speedLimit" | "inertia" | "tolerance" | "backlog">): LegacyState {
  return {
    speedLimit: BigInt(Math.trunc(legacy.speedLimit)),
    inertia: BigInt(Math.trunc(legacy.inertia)),
    tolerance: BigInt(Math.trunc(legacy.tolerance)),
    backlog: BigInt(Math.trunc(Math.max(0, legacy.backlog))),
  };
}

/** Legacy exponent: bips(backlog - tolerance*speedLimit) / (inertia*speedLimit) when above tolerance. */
export function legacyExponentBips(s: LegacyState): bigint {
  const toleranceGas = s.tolerance * s.speedLimit;
  if (s.backlog <= toleranceGas) return 0n;
  const denominator = s.inertia * s.speedLimit;
  if (denominator <= 0n) return 0n;
  return ((s.backlog - toleranceGas) * ONE_IN_BIPS) / denominator;
}

/** Legacy Step: drain by dt * speedLimit, then price. Mutates `s`. */
export function legacyStep(s: LegacyState, dtSeconds: bigint, minBaseFee: bigint): { baseFee: bigint; exponent: bigint } {
  const drained = dtSeconds * s.speedLimit;
  s.backlog = s.backlog > drained ? s.backlog - drained : 0n;
  const exponent = legacyExponentBips(s);
  return { baseFee: baseFeeFromExponent(minBaseFee, exponent), exponent };
}

export function legacyAddGas(s: LegacyState, gasUsed: bigint): void {
  s.backlog += gasUsed;
}
