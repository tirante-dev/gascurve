// Owner actions as the UI reads them: the decoded constraint arguments of a setGasPricingConstraints
// call, and which calls replace the pricing state at all. Shared so the timeline, the chart annotations
// and the smoothing loop cannot disagree about what an action said.

import type { OwnerAction } from "@/types";

export type ConstraintArg = { target: number; window: number; backlog: number };

/**
 * The api decodes setGasPricingConstraints as objects; older fixtures and the raw ABI shape are
 * [target, window, backlog] triples. Null for anything else, so a caller can print the raw value.
 */
export function parseConstraintArg(c: unknown): ConstraintArg | null {
  if (Array.isArray(c) && c.length >= 3) {
    const [target, window, backlog] = c.map(Number);
    return Number.isFinite(target) && Number.isFinite(window) && Number.isFinite(backlog) ? { target, window, backlog } : null;
  }
  if (typeof c === "object" && c !== null) {
    const o = c as Record<string, unknown>;
    const target = Number(o.gasTargetPerSecond ?? o.target);
    const window = Number(o.adjustmentWindowSeconds ?? o.window);
    const backlog = Number(o.startingBacklog ?? o.backlog ?? 0);
    return Number.isFinite(target) && Number.isFinite(window) && Number.isFinite(backlog) ? { target, window, backlog } : null;
  }
  return null;
}

export function rawConstraintArg(c: unknown): string {
  return typeof c === "string" ? c : JSON.stringify(c);
}

/**
 * The owner calls that replace what prices a block. Even one that reinstalls the same parameters
 * counts: setGasPricingConstraints carries a startingBacklog per constraint.
 */
export const PRICING_METHODS: readonly string[] = ["setGasPricingConstraints", "setSpeedLimit", "setL2GasPricingInertia", "setL2GasBacklogTolerance"];

export function touchesConstraints(action: Pick<OwnerAction, "method">): boolean {
  return PRICING_METHODS.includes(action.method);
}

/** The highest block one of those calls landed at, or null. Nothing before it is averaged into a
 * figure that stands for the new definition. */
export function latestConstraintBlock(actions: readonly OwnerAction[]): number | null {
  let highest: number | null = null;
  for (const a of actions) {
    if (!touchesConstraints(a) || !Number.isFinite(a.block)) continue;
    if (highest === null || a.block > highest) highest = a.block;
  }
  return highest;
}
