// Owner actions as the UI reads them. Two things every consumer needs: the
// decoded constraint arguments of a setGasPricingConstraints call, and which
// calls replace the pricing state at all. Both live here rather than in a
// component, so the timeline, the chart annotations and the smoothing loop
// read one parser and cannot disagree about what an action said.

import type { OwnerAction } from "@/types";

/** One constraint of a setGasPricingConstraints call, whichever shape it arrived in. */
export type ConstraintArg = { target: number; window: number; backlog: number };

/**
 * The api decodes setGasPricingConstraints as objects
 * ({ gasTargetPerSecond, adjustmentWindowSeconds, startingBacklog }); older
 * fixtures and the raw ABI shape are [target, window, backlog] triples. Null
 * for anything else, so a caller can fall back to printing the raw value
 * rather than "[object Object]".
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

/** A constraint argument that could not be parsed, printed as itself rather than coerced. */
export function rawConstraintArg(c: unknown): string {
  return typeof c === "string" ? c : JSON.stringify(c);
}

/**
 * The owner calls that replace what prices a block: the constraint set itself
 * and the three legacy pricer parameters. A call in this list makes every
 * figure derived from a backlog meaningless, even when it reinstalls the
 * parameters that were already there: `setGasPricingConstraints` carries a
 * `startingBacklog` per constraint, so an identical target and window can
 * still come with a different state.
 */
export const PRICING_METHODS: readonly string[] = ["setGasPricingConstraints", "setSpeedLimit", "setL2GasPricingInertia", "setL2GasBacklogTolerance"];

export function touchesConstraints(action: Pick<OwnerAction, "method">): boolean {
  return PRICING_METHODS.includes(action.method);
}

/**
 * The highest block at which one of those calls landed, or null when none of
 * `actions` is one. The smoothing loop treats that block as the first priced
 * under the new definition, so nothing before it is averaged into a figure
 * that stands for the new one.
 */
export function latestConstraintBlock(actions: readonly OwnerAction[]): number | null {
  let highest: number | null = null;
  for (const a of actions) {
    if (!touchesConstraints(a) || !Number.isFinite(a.block)) continue;
    if (highest === null || a.block > highest) highest = a.block;
  }
  return highest;
}
