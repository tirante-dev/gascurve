import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "@/lib/api/core";
import { baseFeeFromExponent, constraintExponentBips, contributionsBips, step, toState } from "@/lib/pricer";
import type { BatchSeries, BlockPoint, ConstraintsResponse, L1Series, LiveSnapshot, Network, OwnerAction, Series, ServerMessage, StatusResponse } from "@/types";
import { findMockDef, findMockWorld, MOCK_NETWORKS, MockWebSocket, mockNow, mockRequest, resetMockWorlds } from "./index";
import { Demand, hash01, isoToUnix, MockWorld } from "./world";
import { ROBINHOOD } from "./defs";

// 2026-09-06T07:20:00Z, the SPEC Appendix B snapshot time.
const NOW_MS = 1788679200 * 1000;

/** Narrows an api field the mock records in full; history from before the split migration is checked separately. */
function recorded<T>(value: T | null): T {
  if (value === null) throw new Error("expected a recorded value");
  return value;
}

/** The exact infrastructure destination over blocks. */
function floorWei(blocks: readonly BlockPoint[]): bigint {
  return blocks.reduce((sum, b) => {
    const fee = BigInt(b.baseFee);
    const minFee = BigInt(recorded(b.minBaseFee));
    const floor = fee < minFee ? fee : minFee;
    return sum + BigInt(b.gasUsed - recorded(b.posterGas ?? null)) * floor;
  }, 0n);
}

describe("mock world", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(NOW_MS);
    resetMockWorlds();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("is deterministic and lands Robinhood on the Appendix B backlog", () => {
    const a = findMockWorld("robinhood");
    const b = new MockWorld(ROBINHOOD, mockNow());
    expect(a).toBeDefined();
    if (!a) return;
    const sa = a.snapshot(mockNow());
    const sb = b.snapshot(mockNow());
    expect(sa).toEqual(sb);
    expect(sa.constraints).toHaveLength(2);
    expect(sa.constraints[1].backlog).toBe(11_194_391_810_886);
    expect(sa.constraints[1].exponentBips).toBe(32_391);
    expect(sa.minBaseFee).toBe("20000000");
    expect(sa.model).toBe("constraints");
    expect(sa.legacy).toBeUndefined();
    // The block's fee was priced before the block's gas was absorbed, so it
    // sits at or just below the fee implied by the live backlogs.
    const implied = baseFeeFromExponent(20_000_000n, BigInt(sa.constraints.reduce((s, c) => s + c.exponentBips, 0)));
    expect(BigInt(sa.baseFee)).toBeLessThanOrEqual(implied);
    expect(BigInt(sa.baseFee)).toBeGreaterThan((implied * 95n) / 100n);
    expect(sa.multiplierBips).toBeGreaterThan(150_000);
    expect(contributionsBips(sa.constraints)).toEqual(sa.constraints.map((c) => c.exponentBips));
    expect(a.liveExponentBips()).toBe(sa.constraints[0].exponentBips + sa.constraints[1].exponentBips);
    expect(sa.prices.perArbGasTotal).toBe(sa.baseFee);
    expect(BigInt(sa.prices.perArbGasBase) + BigInt(sa.prices.perArbGasCongestion)).toBe(BigInt(sa.baseFee));
    expect(sa.gasPerSecond.s10).toBeGreaterThan(20_000_000);
    expect(sa.gasPerSecond.s60).toBeGreaterThan(20_000_000);
    expect(sa.computeGasPerSecond?.s10).toBeLessThan(sa.gasPerSecond.s10);
    expect(sa.accounts?.network.address).toBe(ROBINHOOD.accounts.network);
    expect(sa.l1?.equilibrationUnits).toBe(160_000_000);
    expect(sa.sampledAt).toBe("2026-09-06T07:20:00.000Z");
  });

  it("keeps a ring of recent blocks with per-block pricing", () => {
    const world = findMockWorld("4663");
    expect(world).toBeDefined();
    if (!world) return;
    const blocks = world.recentBlocks(120);
    expect(blocks).toHaveLength(120);
    for (let i = 1; i < blocks.length; i++) {
      expect(blocks[i].number).toBe(blocks[i - 1].number + 1);
      expect(blocks[i].ts).toBeGreaterThanOrEqual(blocks[i - 1].ts);
    }
    expect(blocks[blocks.length - 1].ts).toBe(mockNow() - 1);
    expect(blocks.some((b) => b.anchored)).toBe(true);
    expect(blocks[0].backlogs).toHaveLength(2);
    // Every block carries the floor in force and the start-of-block split that priced it.
    const set = ROBINHOOD.constraintSets[ROBINHOOD.constraintSets.length - 1].constraints;
    for (let i = 0; i < blocks.length; i++) {
      const b = blocks[i];
      expect(b.minBaseFee).toBe("20000000");
      expect(b.constraintBips).toHaveLength(2);
      expect(recorded(b.constraintBips).reduce((sum, x) => sum + x, 0)).toBe(b.exponentBips);
      if (i > 0 && b.ts === blocks[i - 1].ts) {
        // Same second, dt = 0: the split is the pricer's view of the previous block's end backlogs, before this block's gas.
        const expected = set.map((c, j) => Number(constraintExponentBips({ target: BigInt(c.target), window: BigInt(c.window), backlog: BigInt(blocks[i - 1].backlogs[j]) })));
        expect(b.constraintBips).toEqual(expected);
      }
    }
    expect(world.headBlock).toBe(blocks[blocks.length - 1].number);
    expect(world.blocksAfter(blocks[blocks.length - 3].number)).toEqual(blocks.slice(-2));
    expect(world.blocksAfter(blocks[blocks.length - 1].number)).toEqual([]);
    expect(world.recentBlocks(0)).toEqual([]);
  });

  it("builds a per-block snapshot that agrees with the live one for the last block and shows the sawtooth before it", () => {
    const world = findMockWorld("robinhood");
    if (!world) throw new Error("no world");
    const now = mockNow();
    const blocks = world.recentBlocks(30);
    const last = blocks[blocks.length - 1];
    const live = world.snapshot(now);
    const perBlock = world.snapshotForBlock(last, now * 1000);
    expect(perBlock).toEqual({ ...live, sampledAt: new Date(now * 1000).toISOString() });
    // Earlier blocks in the same second carry a smaller short-window backlog and their own fee.
    const sameSecond = blocks.filter((b) => b.ts === last.ts);
    expect(sameSecond.length).toBeGreaterThan(5);
    let prev = 0;
    for (const b of sameSecond) {
      const s = world.snapshotForBlock(b, b.ts * 1000 + 100);
      expect(s.block.number).toBe(b.number);
      expect(s.baseFee).toBe(b.baseFee);
      expect(s.exponentBips).toBe(b.exponentBips);
      expect(s.constraints[0].backlog).toBe(b.backlogs[0]);
      expect(s.constraints[0].backlog).toBeGreaterThan(prev);
      prev = s.constraints[0].backlog;
      expect(s.replayErrorBips).toBe(Math.round((Number(BigInt(b.baseFee) - BigInt(b.predictedBaseFee ?? b.baseFee)) * 10_000) / Number(b.baseFee)));
      expect(s.gasPerSecond.s10).toBeLessThanOrEqual(live.gasPerSecond.s10 + 1);
      expect(s.sampledAt).toBe(new Date(b.ts * 1000 + 100).toISOString());
    }
    // The first block of the second drained a whole second of target: its backlog is below the previous second's last.
    const previousSecond = blocks.filter((b) => b.ts === last.ts - 1);
    expect(sameSecond[0].backlogs[0]).toBeLessThan(previousSecond[previousSecond.length - 1].backlogs[0]);
    // The legacy world reports its backlog per block too.
    const testnet = findMockWorld("robinhood-testnet");
    if (!testnet) throw new Error("no world");
    const tb = testnet.recentBlocks(1)[0];
    const ts = testnet.snapshotForBlock(tb, now * 1000);
    expect(ts.legacy?.backlog).toBe(tb.backlogs[0]);
    expect(ts.constraints).toEqual([]);
  });

  it("advances with per-second ticks for short gaps and catches up coarsely for long ones", () => {
    const world = findMockWorld("robinhood");
    if (!world) throw new Error("no world");
    const head = world.headBlock;
    world.advanceTo(mockNow() + 3);
    expect(world.headBlock).toBeGreaterThan(head + 25);
    expect(world.time).toBe(mockNow() + 3);
    const recordsBefore = world.records.length;
    world.advanceTo(mockNow() + 3 + 3600);
    expect(world.time).toBe(mockNow() + 3 + 3600);
    // 3000 s of catch-up in 5 s steps plus 600 per-second ticks.
    expect(world.records.length - recordsBefore).toBe(600 + 600);
    world.advanceTo(0);
    expect(world.time).toBe(mockNow() + 3 + 3600);
  });

  it("produces series at every range with consistent buckets", () => {
    const world = findMockWorld("robinhood");
    if (!world) throw new Error("no world");
    const now = mockNow();

    const hour = world.series("1h", now);
    expect(hour.resolution).toBe("5s");
    expect(hour.points.length).toBeGreaterThan(700);
    expect(hour.points.length).toBeLessThanOrEqual(721);
    expect(hour.constraintSets.map((s) => s.id)).toEqual([6]);
    expect(hour.ownerActions).toEqual([]);
    for (let i = 1; i < hour.points.length; i++) {
      expect(hour.points[i].t - hour.points[i - 1].t).toBe(5);
    }
    const last = hour.points[hour.points.length - 1];
    expect(BigInt(last.baseFeeMin) <= BigInt(last.baseFeeAvg)).toBe(true);
    expect(BigInt(last.baseFeeAvg) <= BigInt(last.baseFeeMax)).toBe(true);
    expect(last.backlogs).toHaveLength(2);
    expect(last.backlogsMax[1]).toBeGreaterThanOrEqual(last.backlogs[1]);
    expect(last.constraintSetId).toBe(6);
    expect(last.gasPerSecond).toBeGreaterThan(0);
    expect(BigInt(last.feesWei)).toBeGreaterThan(0n);
    // The bucket's split is the start-of-block split of its last block, and the fee destinations are exact.
    const lastBlock = world.recentBlocks(1)[0];
    expect(last.constraintBips).toEqual(lastBlock.constraintBips);
    expect(last.minBaseFee).toBe("20000000");
    for (const p of hour.points) {
      expect(recorded(p.constraintBips).reduce((sum, x) => sum + x, 0)).toBe(p.exponentBips);
      expect(BigInt(recorded(p.floorFeesWei)) + BigInt(recorded(p.surplusFeesWei)) + BigInt(recorded(p.posterFeesWei ?? null))).toBe(BigInt(p.feesWei));
      expect(BigInt(recorded(p.floorFeesWei))).toBe(BigInt(p.gasUsed - recorded(p.posterGas ?? null)) * BigInt(recorded(p.minBaseFee)));
      expect(BigInt(recorded(p.surplusFeesWei))).toBeGreaterThanOrEqual(0n);
      expect(recorded(p.posterGas ?? null)).toBeGreaterThan(0);
    }

    const day = world.series("24h", now);
    expect(day.resolution).toBe("1m");
    expect(day.points.length).toBeGreaterThan(1400);
    expect(day.points.length).toBeLessThanOrEqual(1441);
    // No phase-boundary artifacts: the short window never carries a coarse step's gas.
    expect(Math.max(...day.points.map((p) => p.exponentBips))).toBeLessThan(80_000);
    expect(Math.max(...day.points.slice(0, 30).map((p) => p.backlogs[0]))).toBeLessThan(2_500_000_000);

    const month = world.series("30d", now);
    expect(month.resolution).toBe("15m");
    expect(month.points.length).toBeGreaterThan(2800);
    expect(month.constraintSets.map((s) => s.id)).toEqual([3, 4, 5, 6]);
    expect(month.ownerActions.map((a) => a.block)).toEqual([41_739_400, 51_865_079, 53_578_754]);

    // The load band, over the unit the api measures each resolution's spread with. The average sits
    // inside it, and a band that only ever equalled the average would say nothing about the load.
    expect([hour.spreadSeconds, day.spreadSeconds, month.spreadSeconds]).toEqual([1, 1, 60]);
    const banded = month.points.filter((p) => p.computeGasPerSecondMin != null);
    expect(banded.length).toBeGreaterThan(month.points.length - 5);
    // Only a whole bucket carries one: the bucket still filling has an average over its elapsed
    // span and extrema over the units inside it, which are not the same population.
    expect(month.points[month.points.length - 1].computeGasPerSecondMin).toBeNull();
    expect(banded.every((p) => (p.coverage ?? 0) >= 1)).toBe(true);
    for (const p of banded) {
      expect(p.computeGasPerSecondMin).toBeLessThanOrEqual(p.computeGasPerSecond ?? 0);
      expect(p.computeGasPerSecondMax).toBeGreaterThanOrEqual(p.computeGasPerSecond ?? 0);
    }
    expect(banded.some((p) => (p.computeGasPerSecondMax ?? 0) > (p.computeGasPerSecondMin ?? 0))).toBe(true);

    const all = world.series("all", now);
    expect(all.spreadSeconds).toBe(900);
    expect(all.resolution).toBe("1h");
    expect(all.points[0].t).toBe(isoToUnix("2026-07-01T00:00:00Z"));
    expect(all.points.length).toBeGreaterThan(1600);
    expect(all.constraintSets.map((s) => s.id)).toEqual([1, 2, 3, 4, 5, 6]);
    expect(all.ownerActions).toHaveLength(5);
    // The 6-constraint genesis set is in force for the first ten days.
    expect(all.points[0].backlogs).toHaveLength(6);
    expect(all.points[0].constraintSetId).toBe(1);
    expect(all.points[all.points.length - 1].backlogs).toHaveLength(2);
    // What the collector has of each bucket. A whole bucket is whole; the one
    // in progress at the right edge and the one the range's start cuts into
    // carry the share that was indexed, and every sum in them is a sum over
    // that share alone.
    expect(hour.points.every((p) => p.coverage !== null && p.coverage > 0 && p.coverage <= 1)).toBe(true);
    expect(month.points[0].coverage).toBeCloseTo(2 / 3, 6);
    expect(month.points[month.points.length - 1].coverage).toBeCloseTo(1 / 3, 6);
    expect(month.points.slice(1, -1).every((p) => p.coverage === 1)).toBe(true);
    expect(all.points[all.points.length - 1].coverage).toBeLessThan(1);
    // The long-window backlog is reset to the starting value at the Sep 3 owner action.
    const sep3 = isoToUnix("2026-09-03T17:08:00Z");
    const before = month.points.filter((p) => p.t < sep3 - 900).pop();
    const after = month.points.find((p) => p.t >= sep3);
    expect(before && after).toBeTruthy();
    if (before && after) {
      expect(Math.abs(after.backlogs[1] - 9_989_000_000_000)).toBeLessThan(600_000_000_000);
      expect(before.backlogs[1]).not.toBe(after.backlogs[1]);
    }
    // Fees climb over the last days of August as demand exceeds the 18M target.
    const aug25 = all.points.find((p) => p.t >= isoToUnix("2026-08-25T00:00:00Z"));
    const aug31 = all.points.find((p) => p.t >= isoToUnix("2026-08-31T00:00:00Z"));
    expect(aug25 && aug31).toBeTruthy();
    if (aug25 && aug31) expect(BigInt(aug31.baseFeeAvg)).toBeGreaterThan(BigInt(aug25.baseFeeAvg) * 5n);
  });

  it("serves constraints, owner actions, batches and l1 series", () => {
    const world = findMockWorld("robinhood");
    if (!world) throw new Error("no world");
    const now = mockNow();
    const c = world.constraintsResponse();
    expect(c.current?.id).toBe(6);
    expect(c.history).toHaveLength(6);
    expect(c.current?.constraints[1]).toEqual({ target: 40_000_000, window: 86_400, startingBacklog: 9_989_000_000_000 });
    const actions = world.ownerActions();
    expect(actions[0].block).toBe(53_578_754);
    expect(actions[actions.length - 1].method).toBe("setMinimumL2BaseFee");
    expect(actions[0].args).toEqual({ constraints: [{ gasTargetPerSecond: 60_000_000, adjustmentWindowSeconds: 15, startingBacklog: 0 }, { gasTargetPerSecond: 40_000_000, adjustmentWindowSeconds: 86_400, startingBacklog: 9_989_000_000_000 }] });
    expect(actions[0].txHash).toMatch(/^0x[0-9a-f]{64}$/);

    const batches = world.batches("24h", now);
    expect(batches.resolution).toBe("15m");
    expect(batches.points.length).toBeGreaterThanOrEqual(96);
    const p = batches.points[10];
    expect(p.batches).toBeGreaterThan(30);
    expect(BigInt(p.weiSpent)).toBe(BigInt(p.gasSpent) * BigInt(p.l1BaseFeeAvg));
    expect(p.calldataBytes).toBe(p.batches * 137);
    expect(world.batches("1h", now).resolution).toBe("batch");
    expect(world.batches("all", now).points[0].t).toBe(isoToUnix("2026-07-01T00:00:00Z"));

    const l1 = world.l1Series("30d", now);
    expect(l1.range).toBe("30d");
    expect(l1.points.length).toBeGreaterThan(700);
    expect(Number(l1.points[0].baseFeeEstimate)).toBeGreaterThan(2_000_000);
    expect(world.network(now).headBlock).toBe(world.headBlock);
    expect(world.network(now + 5).lagSeconds).toBe(6);
    expect(world.status(now)).toMatchObject({
      lastError: null,
      degraded: false,
      capacity: { saturated: false, at: null },
      holes: { pending: 0, blocks: 0 },
    });
  });

  it("models the six-constraint Arbitrum One set", () => {
    const world = findMockWorld("arbitrum-one");
    if (!world) throw new Error("no world");
    const s = world.snapshot(mockNow());
    expect(s.constraints.map((c) => [c.target, c.window])).toEqual([
      [60_000_000, 9],
      [41_000_000, 52],
      [29_000_000, 329],
      [20_000_000, 2105],
      [14_000_000, 13_485],
      [10_000_000, 86_400],
    ]);
    expect(s.minBaseFee).toBe("10000000");
    const series = world.series("30d", mockNow());
    expect(series.ownerActions).toHaveLength(1);
    expect(series.constraintSets.map((x) => x.id)).toEqual([1, 2]);
    expect(series.points[0].backlogs).toHaveLength(6);
    expect(world.constraintsResponse().history).toHaveLength(2);
  });

  it("quotes a plausible ETH price that refreshes on the minute, and none at all for a network without a feed", () => {
    const world = findMockWorld("robinhood");
    if (!world) throw new Error("no world");
    const now = mockNow();
    const quote = world.snapshot(now).ethUsd;
    if (!quote) throw new Error("no quote");
    // Around 4,200 dollars, never further than a percent from it.
    expect(Number(quote.price)).toBeGreaterThan(4_158);
    expect(Number(quote.price)).toBeLessThan(4_242);
    expect(quote.price).toMatch(/^\d+\.\d{2}$/);
    expect(quote.source).toBe("coingecko");
    // Fetched on the minute, so it ages for up to a minute and then refreshes.
    expect(isoToUnix(quote.at) % 60).toBe(0);
    expect(isoToUnix(quote.at)).toBeLessThanOrEqual(now);
    expect(world.snapshot(now + 30).ethUsd).toEqual(quote);
    const next = world.ethUsd(isoToUnix(quote.at) + 60);
    expect(next?.at).not.toBe(quote.at);
    expect(next?.price).not.toBe(quote.price);
    // The testnet has no price feed at all: the snapshot says so rather than guessing.
    const testnet = findMockWorld("robinhood-testnet");
    if (!testnet) throw new Error("no testnet world");
    expect(testnet.snapshot(now).ethUsd).toBeNull();
    expect(testnet.ethUsd(now)).toBeNull();
  });

  it("models the legacy Robinhood testnet pricer", () => {
    const world = findMockWorld("robinhood-testnet");
    if (!world) throw new Error("no world");
    const now = mockNow();
    const s = world.snapshot(now);
    expect(s.model).toBe("legacy");
    expect(s.constraints).toEqual([]);
    expect(s.legacy).toEqual(expect.objectContaining({ speedLimit: 7_000_000, inertia: 102, tolerance: 10 }));
    expect(s.minBaseFee).toBe("10000000");
    expect(world.liveExponentBips()).toBeGreaterThanOrEqual(0);
    const series = world.series("24h", now);
    expect(series.constraintSets).toEqual([]);
    expect(series.points[0].backlogs).toHaveLength(1);
    expect(series.points[0].constraintSetId).toBe(0);
    // The legacy model reports one split element, as the Go pricer does.
    for (const p of series.points) expect(p.constraintBips).toEqual([p.exponentBips]);
    // The floor changed on July 15: earlier buckets carry the old floor and fee split.
    const all = world.series("all", now);
    const change = isoToUnix("2026-07-15T10:00:00Z");
    const before = all.points.filter((p) => p.t < change - 3600).pop();
    const after = all.points.find((p) => p.t >= change);
    expect(before?.minBaseFee).toBe("100000000");
    expect(after?.minBaseFee).toBe("10000000");
    if (before) expect(BigInt(recorded(before.floorFeesWei))).toBe(BigInt(before.gasUsed - recorded(before.posterGas ?? null)) * 100_000_000n);
    if (after) expect(BigInt(recorded(after.floorFeesWei))).toBe(BigInt(after.gasUsed - recorded(after.posterGas ?? null)) * 10_000_000n);
    for (const p of all.points) expect(BigInt(recorded(p.floorFeesWei)) + BigInt(recorded(p.surplusFeesWei)) + BigInt(recorded(p.posterFeesWei ?? null))).toBe(BigInt(p.feesWei));
    // Bursts push the backlog over tolerance and the fee off the floor at least once a day.
    expect(series.points.some((p) => BigInt(p.baseFeeMax) > 10_000_000n)).toBe(true);
    expect(series.points.some((p) => BigInt(p.baseFeeMin) === 10_000_000n)).toBe(true);
    const c = world.constraintsResponse();
    expect(c.current).toBeNull();
    expect(c.history).toEqual([]);
    expect(world.ownerActions()).toHaveLength(1);
    expect(world.series("all", now).ownerActions).toHaveLength(1);
  });

  it("serves the oldest Robinhood history without a split, a floor or fee destinations, as the api does for pricing version 0 rows", () => {
    const world = findMockWorld("robinhood");
    if (!world) throw new Error("no world");
    const now = mockNow();
    const all = world.series("all", now);
    const cutoff = isoToUnix("2026-07-02T12:00:00Z");
    const early = all.points.filter((p) => p.t < cutoff);
    expect(early).toHaveLength(36);
    for (const p of early) {
      expect(p.constraintBips).toBeNull();
      // The contract makes the floor nullable on exactly the same rows.
      expect(p.minBaseFee).toBeNull();
      expect(p.floorFeesWei).toBeNull();
      expect(p.surplusFeesWei).toBeNull();
      expect(recorded(p.posterGas ?? null)).toBeGreaterThan(0);
      expect(BigInt(recorded(p.posterFeesWei ?? null))).toBeGreaterThan(0n);
      expect(recorded(p.computeGasPerSecond ?? null)).toBeLessThan(p.gasPerSecond);
      // Everything else about the bucket is known.
      expect(p.backlogs).toHaveLength(6);
      expect(p.constraintSetId).toBe(1);
      expect(p.exponentBips).toBeGreaterThanOrEqual(0);
      expect(BigInt(p.feesWei)).toBeGreaterThan(0n);
    }
    for (const p of all.points.filter((p) => p.t >= cutoff)) {
      expect(p.constraintBips).not.toBeNull();
      expect(BigInt(recorded(p.floorFeesWei)) + BigInt(recorded(p.surplusFeesWei)) + BigInt(recorded(p.posterFeesWei ?? null))).toBe(BigInt(p.feesWei));
    }
    // The shorter ranges never reach back that far, and the other networks record everything.
    for (const range of ["1h", "24h", "30d"] as const) {
      for (const p of world.series(range, now).points) expect(p.constraintBips).not.toBeNull();
    }
    const arbitrum = findMockWorld("arbitrum-one");
    if (!arbitrum) throw new Error("no world");
    for (const p of arbitrum.series("all", now).points) expect(p.floorFeesWei).not.toBeNull();
  });

  it("forks the tail on a reorg and keeps the blocks, balances, series and live snapshot consistent with the new chain", () => {
    const world = findMockWorld("robinhood");
    if (!world) throw new Error("no world");
    const now = mockNow();
    const before = world.recentBlocks(20);
    const head = before[before.length - 1].number;
    const infraBefore = BigInt(world.snapshot(now).accounts?.infra.balance ?? "0");
    const hourBefore = world.series("1h", now);
    const fork = world.reorg(3);
    expect(fork).not.toBeNull();
    if (!fork) return;
    expect(fork.ancestor).toBe(head - 3);
    expect(fork.blocks.map((b) => b.number)).toEqual([head - 2, head - 1, head]);
    expect(fork.blocks.map((b) => b.ts)).toEqual(before.slice(-3).map((b) => b.ts));
    expect(fork.blocks.map((b) => b.anchored)).toEqual(before.slice(-3).map((b) => b.anchored));
    // The canonical blocks differ from the orphaned ones; everything before the ancestor is untouched.
    expect(fork.blocks.map((b) => b.gasUsed)).not.toEqual(before.slice(-3).map((b) => b.gasUsed));
    expect(world.recentBlocks(20).slice(0, 17)).toEqual(before.slice(0, 17));
    expect(world.blocksAfter(fork.ancestor)).toEqual(fork.blocks);
    expect(world.headBlock).toBe(head);
    // The fork is priced by the pricer from the ancestor's end-of-block state.
    const ancestor = before[before.length - 4];
    const set = ROBINHOOD.constraintSets[ROBINHOOD.constraintSets.length - 1].constraints;
    const state = toState(set.map((c, j) => ({ target: c.target, window: c.window, backlog: ancestor.backlogs[j] })));
    const priced = step(state, BigInt(fork.blocks[0].ts - ancestor.ts), 20_000_000n);
    expect(fork.blocks[0].constraintBips).toEqual(priced.contributions.map((c) => Number(c)));
    expect(fork.blocks[0].baseFee).toBe(priced.baseFee.toString());
    for (const b of fork.blocks) expect(recorded(b.constraintBips).reduce((sum, x) => sum + x, 0)).toBe(b.exponentBips);
    // The live snapshot is the state after the canonical head.
    const live = world.snapshot(now);
    const last = fork.blocks[2];
    expect(live.block.number).toBe(head);
    expect(live.baseFee).toBe(last.baseFee);
    expect(live.constraints.map((c) => c.backlog)).toEqual(last.backlogs);
    expect(world.snapshotForBlock(last, now * 1000)).toEqual({ ...live, sampledAt: new Date(now * 1000).toISOString() });
    // The balances credit the canonical blocks, not the orphaned ones.
    expect(BigInt(live.accounts?.infra.balance ?? "0")).toBe(infraBefore - floorWei(before.slice(-3)) + floorWei(fork.blocks));
    // The buckets the fork touched are rebuilt from the ring; the rest are as they were.
    const hour = world.series("1h", now);
    expect(hour.points).toHaveLength(hourBefore.points.length);
    expect(hour.points.slice(0, -2)).toEqual(hourBefore.points.slice(0, -2));
    const lastPoint = hour.points[hour.points.length - 1];
    const inBucket = world.recentBlocks(1000).filter((b) => b.ts >= lastPoint.t && b.ts < lastPoint.t + 5);
    expect(inBucket[inBucket.length - 1]).toEqual(last);
    expect(lastPoint.blocks).toBe(inBucket.length);
    expect(lastPoint.gasUsed).toBe(inBucket.reduce((sum, b) => sum + b.gasUsed, 0));
    expect(BigInt(lastPoint.feesWei)).toBe(inBucket.reduce((sum, b) => sum + BigInt(b.gasUsed) * BigInt(b.baseFee), 0n));
    expect(BigInt(recorded(lastPoint.floorFeesWei))).toBe(floorWei(inBucket));
    expect(lastPoint.constraintBips).toEqual(last.constraintBips);
    expect(lastPoint.backlogs).toEqual(last.backlogs);
    expect(lastPoint.backlogsMax[0]).toBeGreaterThanOrEqual(Math.max(...inBucket.map((b) => b.backlogs[0])));
    // The world keeps going from the fork.
    world.advanceTo(now + 1);
    expect(world.blocksAfter(head)[0].number).toBe(head + 1);
    // A fork the ring cannot hold, or a depth that is not a whole number of blocks, is refused.
    expect(world.reorg(0)).toBeNull();
    expect(world.reorg(1.5)).toBeNull();
    expect(world.reorg(world.recentBlocks(5000).length)).toBeNull();
    // The legacy world rewinds its single backlog the same way.
    const testnet = findMockWorld("robinhood-testnet");
    if (!testnet) throw new Error("no world");
    const testnetHead = testnet.headBlock;
    const testnetFork = testnet.reorg(2);
    expect(testnetFork?.blocks.map((b) => b.number)).toEqual([testnetHead - 1, testnetHead]);
    expect(testnet.snapshot(now).legacy?.backlog).toBe(testnetFork?.blocks[1].backlogs[0]);
  });

  it("finds worlds by name or chain id and rejects unknown ones", () => {
    expect(findMockDef("42161")?.name).toBe("arbitrum-one");
    expect(findMockDef("nope")).toBeUndefined();
    expect(findMockWorld("nope")).toBeUndefined();
    expect(MOCK_NETWORKS.map((n) => n.name)).toEqual(["robinhood", "arbitrum-one", "robinhood-testnet"]);
  });
});

describe("demand", () => {
  it("interpolates the baseline and averages bursts", () => {
    const demand = new Demand(ROBINHOOD.demand);
    expect(demand.baseline(isoToUnix("2026-06-01T00:00:00Z"))).toBe(4_000_000);
    expect(demand.baseline(isoToUnix("2026-07-05T12:00:00Z"))).toBeCloseTo(6_000_000, -3);
    expect(demand.baseline(isoToUnix("2027-01-01T00:00:00Z"))).toBe(44_000_000);
    const t = isoToUnix("2026-09-06T07:00:00Z");
    let withBurst = 0;
    for (let i = 0; i < 200; i++) {
      if (demand.burstAverage(t + i * 240, 240) > 0) withBurst += 1;
    }
    expect(withBurst).toBeGreaterThan(5);
    expect(withBurst).toBeLessThan(80);
    expect(demand.average(t, 60)).toBeGreaterThan(20_000_000);
    expect(hash01(1, 2)).toBeGreaterThanOrEqual(0);
    expect(hash01(1, 2)).toBeLessThan(1);
    expect(hash01(1, 2)).toBe(hash01(1, 2));
    expect(hash01(1, 2)).not.toBe(hash01(1, 3));
  });
});

describe("mockRequest", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(NOW_MS);
    resetMockWorlds();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("routes every documented path", async () => {
    const networks = await mockRequest<Network[]>("/networks");
    expect(networks.map((n) => n.name)).toEqual(["robinhood", "arbitrum-one", "robinhood-testnet"]);
    expect(networks[0].chainId).toBe(4663);
    expect((await mockRequest<Network>("networks/4663")).name).toBe("robinhood");
    const live = await mockRequest<LiveSnapshot>("/networks/robinhood/live");
    expect(live.chainId).toBe(4663);
    const blocks = await mockRequest<BlockPoint[]>("/networks/robinhood/blocks", { limit: 10 });
    expect(blocks).toHaveLength(10);
    expect(blocks[0].number).toBeGreaterThan(blocks[1].number);
    expect(await mockRequest<BlockPoint[]>("/networks/robinhood/blocks", { limit: "x" })).toHaveLength(120);
    expect(await mockRequest<BlockPoint[]>("/networks/robinhood/blocks", { limit: 5000 })).toHaveLength(1000);
    const series = await mockRequest<Series>("/networks/robinhood/series", { range: "24h" });
    expect(series.range).toBe("24h");
    expect((await mockRequest<Series>("/networks/robinhood/series")).range).toBe("1h");
    const constraints = await mockRequest<ConstraintsResponse>("/networks/robinhood/constraints");
    expect(constraints.history).toHaveLength(6);
    const actions = await mockRequest<OwnerAction[]>("/networks/robinhood/owner-actions");
    expect(actions).toHaveLength(6);
    expect((await mockRequest<{ range: string }>("/networks/robinhood/batches", { range: "30d" })).range).toBe("30d");
    expect((await mockRequest<{ range: string }>("/networks/robinhood/l1", { range: "1h" })).range).toBe("1h");
    const status = await mockRequest<StatusResponse>("/status");
    expect(status.networks).toHaveLength(3);
    expect(status.version).toBe("0.0.0-mock");
    expect(await mockRequest("/health")).toEqual({ ok: true });
  });

  it("answers with the window it was asked for, so a chart can span it", async () => {
    const now = Math.floor(NOW_MS / 1000);
    const day = await mockRequest<Series>("/networks/robinhood/series", { range: "24h" });
    expect(day.to).toBe(now);
    expect(day.from).toBe(now - 86_400);
    expect(day.points[0].t).toBeGreaterThanOrEqual(day.from);
    // "all" reaches back to the first indexed bucket, not to a fixed span.
    const all = await mockRequest<Series>("/networks/robinhood/series", { range: "all" });
    expect(all.from).toBe(all.points[0].t);
    expect(all.to).toBe(now);
    // The batch and L1 series carry the same window.
    const batches = await mockRequest<BatchSeries>("/networks/robinhood/batches", { range: "1h" });
    expect(batches.from).toBe(now - 3600);
    expect(batches.to).toBe(now);
    const allBatches = await mockRequest<BatchSeries>("/networks/robinhood/batches", { range: "all" });
    expect(allBatches.from).toBe(allBatches.points[0].t);
    const l1 = await mockRequest<L1Series>("/networks/robinhood/l1", { range: "24h" });
    expect(l1.from).toBe(now - 86_400);
    expect(l1.to).toBe(now);
    const allL1 = await mockRequest<L1Series>("/networks/robinhood/l1", { range: "all" });
    expect(allL1.from).toBe(allL1.points[0].t);
  });

  it("returns typed errors", async () => {
    await expect(mockRequest("/networks/nope/live")).rejects.toMatchObject({ status: 404, code: "not_found" });
    await expect(mockRequest("/networks/robinhood/series", { range: "2h" })).rejects.toBeInstanceOf(ApiError);
    await expect(mockRequest("/networks/robinhood/nothing")).rejects.toMatchObject({ status: 404 });
    await expect(mockRequest("/whatever")).rejects.toMatchObject({ status: 404 });
    await expect(mockRequest("/networks/robinhood/live/extra")).rejects.toMatchObject({ status: 404 });
  });
});

describe("MockWebSocket", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(NOW_MS);
    resetMockWorlds();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("speaks the section 7 protocol", () => {
    const socket = new MockWebSocket("ws://localhost:8080/api/v1/ws?network=robinhood");
    const messages: { type: string; data?: unknown }[] = [];
    const opened = vi.fn();
    socket.onopen = opened;
    socket.onmessage = (ev) => messages.push(JSON.parse(String(ev.data)));
    expect(socket.readyState).toBe(0);
    expect(() => socket.send("{}")).toThrow();
    vi.advanceTimersByTime(30);
    expect(opened).toHaveBeenCalled();
    expect(socket.readyState).toBe(1);
    expect(messages[0].type).toBe("hello");
    const hello = messages[0].data as { network: Network; recentBlocks: BlockPoint[] };
    expect(hello.network.name).toBe("robinhood");
    expect(hello.recentBlocks).toHaveLength(120);

    // The first second's blocks arrive one at a time, each with its own tick, spread over the following second.
    vi.advanceTimersByTime(1000);
    expect(messages.map((m) => m.type)).toEqual(["hello", "tick", "blocks"]);
    const firstBlocks = messages[2].data as BlockPoint[];
    expect(firstBlocks).toHaveLength(1);
    expect(firstBlocks[0].number).toBe(hello.recentBlocks[hello.recentBlocks.length - 1].number + 1);
    expect((messages[1].data as LiveSnapshot).block.number).toBe(firstBlocks[0].number);
    vi.advanceTimersByTime(999);
    const pairs = (messages.length - 1) / 2;
    expect(pairs).toBeGreaterThanOrEqual(10);
    for (let i = 1; i < messages.length; i += 2) {
      expect(messages[i].type).toBe("tick");
      expect(messages[i + 1].type).toBe("blocks");
      const tick = messages[i].data as LiveSnapshot;
      const blocks = messages[i + 1].data as BlockPoint[];
      expect(blocks).toHaveLength(1);
      expect(blocks[0].number).toBe(firstBlocks[0].number + (i - 1) / 2);
      expect(tick.block.number).toBe(blocks[0].number);
      expect(tick.constraints[0].backlog).toBe(blocks[0].backlogs[0]);
      if (i > 1) {
        const prev = messages[i - 2].data as LiveSnapshot;
        expect(Date.parse(tick.sampledAt)).toBeGreaterThan(Date.parse(prev.sampledAt));
        // Blocks in one second share a timestamp and only add gas; the drain lands at the boundary.
        if (tick.block.ts === prev.block.ts) expect(tick.constraints[0].backlog).toBeGreaterThan(prev.constraints[0].backlog);
        else expect(tick.constraints[0].backlog).toBeLessThan(prev.constraints[0].backlog);
      }
    }
    // The second second: its first block goes out at once, the sawtooth resets at the boundary.
    vi.advanceTimersByTime(1);
    const boundary = messages[messages.length - 2].data as LiveSnapshot;
    const lastOfFirst = messages[messages.length - 4].data as LiveSnapshot;
    expect(boundary.block.ts).toBe(lastOfFirst.block.ts + 1);
    expect(boundary.constraints[0].backlog).toBeLessThan(lastOfFirst.constraints[0].backlog);
    // A second that brings no new block (the clock did not move on) still gets a heartbeat tick.
    vi.advanceTimersByTime(999);
    const beforeHeartbeat = messages.length;
    vi.setSystemTime(Date.now() - 1000);
    vi.advanceTimersByTime(1);
    expect(messages.length).toBe(beforeHeartbeat + 1);
    expect(messages[messages.length - 1].type).toBe("tick");
    vi.setSystemTime(Date.now() + 1000);
    // A pending block timer of the old network never leaks past a subscribe.
    const seen = messages.length;

    socket.send(JSON.stringify({ type: "pong" }));
    socket.send("not json");
    socket.send("42");
    socket.send(JSON.stringify({ type: "subscribe", network: "arbitrum-one" }));
    expect(messages).toHaveLength(seen + 1);
    const second = messages[messages.length - 1];
    expect(second.type).toBe("hello");
    expect((second.data as { network: Network }).network.name).toBe("arbitrum-one");
    vi.advanceTimersByTime(500);
    for (const m of messages.slice(seen + 1)) expect((m.data as LiveSnapshot | BlockPoint[]) instanceof Array || (m.data as LiveSnapshot).chainId === 42161).toBe(true);

    vi.advanceTimersByTime(29_000);
    expect(messages.some((m) => m.type === "ping")).toBe(true);

    const closed = vi.fn();
    socket.onclose = closed;
    socket.close();
    expect(socket.readyState).toBe(3);
    expect(closed).toHaveBeenCalledTimes(1);
    socket.close();
    expect(closed).toHaveBeenCalledTimes(1);
    const before = messages.length;
    vi.advanceTimersByTime(5000);
    expect(messages.length).toBe(before);
  });

  it("drives a reorg with the scenario helper: queued blocks go out first, then the reorg, then ticks continue from the fork", () => {
    const socket = new MockWebSocket("ws://localhost:8080/api/v1/ws?network=robinhood");
    const messages: ServerMessage[] = [];
    socket.onmessage = (ev) => messages.push(JSON.parse(String(ev.data)));
    // Not open yet: nothing to fork.
    expect(socket.reorg(3)).toBeNull();
    vi.advanceTimersByTime(30);
    vi.advanceTimersByTime(1000);
    vi.advanceTimersByTime(300);
    const blocksOf = (from: number) => messages.slice(from).filter((m): m is Extract<ServerMessage, { type: "blocks" }> => m.type === "blocks").flatMap((m) => m.data);
    const publishedBefore = blocksOf(0).length;
    const world = findMockWorld("robinhood");
    if (!world) throw new Error("no world");
    const head = world.headBlock;
    expect(blocksOf(0)[publishedBefore - 1].number).toBeLessThan(head);
    const data = socket.reorg(3);
    expect(data).not.toBeNull();
    if (!data) return;
    expect(data.chainId).toBe(4663);
    expect(data.ancestor).toBe(head - 3);
    expect(data.blocks.map((b) => b.number)).toEqual([head - 2, head - 1, head]);
    expect(data.blocks).toEqual(world.blocksAfter(data.ancestor));
    // Every block still queued for the second went out before the reorg, so the client's chain reaches the head the fork replaces.
    const last = messages[messages.length - 1];
    expect(last.type).toBe("reorg");
    if (last.type === "reorg") expect(last.data).toEqual(data);
    const published = blocksOf(0);
    expect(published.length).toBeGreaterThan(publishedBefore);
    expect(published[published.length - 1].number).toBe(head);
    // Nothing queued survives the flush, and the next second continues from the fork.
    const seen = messages.length;
    vi.advanceTimersByTime(699);
    expect(messages).toHaveLength(seen);
    vi.advanceTimersByTime(1);
    expect(messages[messages.length - 2].type).toBe("tick");
    const next = blocksOf(seen);
    expect(next[0].number).toBe(head + 1);
    // A fork the ring cannot hold sends no reorg (the queued blocks still go out first); a closed socket sends nothing at all.
    const before = messages.length;
    expect(socket.reorg(100_000)).toBeNull();
    expect(messages.slice(before).every((m) => m.type === "tick" || m.type === "blocks")).toBe(true);
    socket.close();
    const closed = messages.length;
    expect(socket.reorg(3)).toBeNull();
    expect(messages).toHaveLength(closed);
  });

  it("closes on an unknown network and defaults to robinhood", () => {
    const bad = new MockWebSocket("ws://localhost/ws?network=nope");
    const closed = vi.fn();
    bad.onclose = closed;
    vi.advanceTimersByTime(30);
    expect(closed).toHaveBeenCalled();
    expect(bad.readyState).toBe(3);

    const plain = new MockWebSocket("/ws");
    const messages: { type: string; data?: { network?: Network } }[] = [];
    plain.onmessage = (ev) => messages.push(JSON.parse(String(ev.data)));
    vi.advanceTimersByTime(30);
    expect(messages[0].data?.network?.name).toBe("robinhood");
    plain.send(JSON.stringify({ type: "subscribe", network: "nope" }));
    expect(plain.readyState).toBe(3);

    const closedEarly = new MockWebSocket("/ws");
    closedEarly.close();
    vi.advanceTimersByTime(100);
    expect(closedEarly.readyState).toBe(3);
  });
});
