import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "@/lib/api/core";
import { baseFeeFromExponent, contributionsBips } from "@/lib/pricer";
import type { BlockPoint, ConstraintsResponse, LiveSnapshot, Network, OwnerAction, Series, StatusResponse } from "@/types";
import { findMockDef, findMockWorld, MOCK_NETWORKS, MockWebSocket, mockNow, mockRequest, resetMockWorlds } from "./index";
import { Demand, hash01, isoToUnix, MockWorld } from "./world";
import { ROBINHOOD } from "./defs";

// 2026-09-06T07:20:00Z, the SPEC Appendix B snapshot time.
const NOW_MS = 1788679200 * 1000;

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
    expect(world.headBlock).toBe(blocks[blocks.length - 1].number);
    expect(world.blocksAfter(blocks[blocks.length - 3].number)).toEqual(blocks.slice(-2));
    expect(world.blocksAfter(blocks[blocks.length - 1].number)).toEqual([]);
    expect(world.recentBlocks(0)).toEqual([]);
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

    const all = world.series("all", now);
    expect(all.resolution).toBe("1h");
    expect(all.points[0].t).toBe(isoToUnix("2026-07-01T00:00:00Z"));
    expect(all.points.length).toBeGreaterThan(1600);
    expect(all.constraintSets.map((s) => s.id)).toEqual([1, 2, 3, 4, 5, 6]);
    expect(all.ownerActions).toHaveLength(5);
    // The 6-constraint genesis set is in force for the first ten days.
    expect(all.points[0].backlogs).toHaveLength(6);
    expect(all.points[0].constraintSetId).toBe(1);
    expect(all.points[all.points.length - 1].backlogs).toHaveLength(2);
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
    expect(c.current.id).toBe(6);
    expect(c.history).toHaveLength(6);
    expect(c.current.constraints[1]).toEqual({ target: 40_000_000, window: 86_400, startingBacklog: 9_989_000_000_000 });
    const actions = world.ownerActions();
    expect(actions[0].block).toBe(53_578_754);
    expect(actions[actions.length - 1].method).toBe("setMinimumL2BaseFee");
    expect(actions[0].args).toEqual({ constraints: [[60_000_000, 15, 0], [40_000_000, 86_400, 9_989_000_000_000]] });
    expect(actions[0].txHash).toMatch(/^0x[0-9a-f]{64}$/);

    const batches = world.batches("24h", now);
    expect(batches.resolution).toBe("15m");
    expect(batches.points.length).toBeGreaterThanOrEqual(96);
    const p = batches.points[10];
    expect(p.batches).toBeGreaterThan(30);
    expect(BigInt(p.weiSpent)).toBe(BigInt(p.gasSpent) * BigInt(p.l1BaseFeeAvg));
    expect(p.calldataBytes).toBe(p.batches * 137);
    expect(world.batches("1h", now).resolution).toBe("1m");
    expect(world.batches("all", now).points[0].t).toBe(isoToUnix("2026-07-01T00:00:00Z"));

    const l1 = world.l1Series("30d", now);
    expect(l1.range).toBe("30d");
    expect(l1.points.length).toBeGreaterThan(700);
    expect(Number(l1.points[0].baseFeeEstimate)).toBeGreaterThan(2_000_000);
    expect(world.network(now).headBlock).toBe(world.headBlock);
    expect(world.network(now + 5).lagSeconds).toBe(6);
    expect(world.status(now).lastError).toBeNull();
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
    // Bursts push the backlog over tolerance and the fee off the floor at least once a day.
    expect(series.points.some((p) => BigInt(p.baseFeeMax) > 10_000_000n)).toBe(true);
    expect(series.points.some((p) => BigInt(p.baseFeeMin) === 10_000_000n)).toBe(true);
    const c = world.constraintsResponse();
    expect(c.current.constraints).toEqual([]);
    expect(c.history).toEqual([]);
    expect(world.ownerActions()).toHaveLength(1);
    expect(world.series("all", now).ownerActions).toHaveLength(1);
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

    vi.advanceTimersByTime(1000);
    expect(messages.map((m) => m.type)).toEqual(["hello", "tick", "blocks"]);
    const blocks = messages[2].data as BlockPoint[];
    expect(blocks[0].number).toBe(hello.recentBlocks[hello.recentBlocks.length - 1].number + 1);
    expect(blocks.length).toBeGreaterThanOrEqual(10);

    // Fake timers move the clock, so every second brings another batch of blocks.
    vi.advanceTimersByTime(1000);
    expect(messages.map((m) => m.type)).toEqual(["hello", "tick", "blocks", "tick", "blocks"]);

    socket.send(JSON.stringify({ type: "pong" }));
    socket.send("not json");
    socket.send("42");
    socket.send(JSON.stringify({ type: "subscribe", network: "arbitrum-one" }));
    const second = messages[messages.length - 1];
    expect(second.type).toBe("hello");
    expect((second.data as { network: Network }).network.name).toBe("arbitrum-one");

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
