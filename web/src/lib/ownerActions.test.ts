import { describe, expect, it } from "vitest";
import type { OwnerAction } from "@/types";
import { latestConstraintAction, latestConstraintBlock, parseConstraintArg, rawConstraintArg, touchesConstraints } from "./ownerActions";

function action(method: string, block: number, args: Record<string, unknown> = {}): OwnerAction {
  return { block, at: "2026-09-06T07:20:00Z", txHash: `0x${block}`, method, selector: "0x00000000", args };
}

describe("parseConstraintArg", () => {
  it("reads the objects the api sends and the tuples older fixtures carry", () => {
    expect(parseConstraintArg({ gasTargetPerSecond: 60_000_000, adjustmentWindowSeconds: 15, startingBacklog: 7 })).toEqual({ target: 60_000_000, window: 15, backlog: 7 });
    expect(parseConstraintArg({ target: 40_000_000, window: 86_400, backlog: 0 })).toEqual({ target: 40_000_000, window: 86_400, backlog: 0 });
    // A decoded object without a starting backlog is a set that starts empty.
    expect(parseConstraintArg({ gasTargetPerSecond: 1, adjustmentWindowSeconds: 2 })).toEqual({ target: 1, window: 2, backlog: 0 });
    expect(parseConstraintArg([60_000_000, 15, 7])).toEqual({ target: 60_000_000, window: 15, backlog: 7 });
    expect(parseConstraintArg(["60000000", "15", "7"])).toEqual({ target: 60_000_000, window: 15, backlog: 7 });
  });
  it("gives up on anything it cannot read, so the caller prints the raw value", () => {
    expect(parseConstraintArg(null)).toBeNull();
    expect(parseConstraintArg("0xdeadbeef")).toBeNull();
    expect(parseConstraintArg([1, 2])).toBeNull();
    expect(parseConstraintArg(["a", "b", "c"])).toBeNull();
    expect(parseConstraintArg({ nothing: 1 })).toBeNull();
    expect(rawConstraintArg("0xdeadbeef")).toBe("0xdeadbeef");
    expect(rawConstraintArg({ a: 1 })).toBe('{"a":1}');
  });
});

describe("owner calls that replace what prices a block", () => {
  it("names the constraint set and the legacy parameters, and nothing else", () => {
    expect(touchesConstraints(action("setGasPricingConstraints", 1))).toBe(true);
    expect(touchesConstraints(action("setSpeedLimit", 1))).toBe(true);
    expect(touchesConstraints(action("setL2GasPricingInertia", 1))).toBe(true);
    expect(touchesConstraints(action("setL2GasBacklogTolerance", 1))).toBe(true);
    expect(touchesConstraints(action("setMinimumL2BaseFee", 1))).toBe(false);
    expect(touchesConstraints(action("setNetworkFeeAccount", 1))).toBe(false);
  });
  it("reports the highest block one of them landed on, and null when none did", () => {
    expect(latestConstraintBlock([])).toBeNull();
    expect(latestConstraintBlock([action("setMinimumL2BaseFee", 90)])).toBeNull();
    // The list is newest first, but the answer is the highest block whatever the order.
    expect(latestConstraintBlock([action("setGasPricingConstraints", 40), action("setMinimumL2BaseFee", 90), action("setGasPricingConstraints", 12)])).toBe(40);
    expect(latestConstraintBlock([{ ...action("setGasPricingConstraints", 0), block: Number.NaN }])).toBeNull();
  });
  it("hands back the call itself, so a notice can name what changed", () => {
    expect(latestConstraintAction([])).toBeNull();
    expect(latestConstraintAction([action("setMinimumL2BaseFee", 90)])).toBeNull();
    expect(latestConstraintAction([action("setSpeedLimit", 12), action("setGasPricingConstraints", 40)])?.method).toBe("setGasPricingConstraints");
  });
});
