// A deterministic world per network: synthetic demand replayed through the
// TypeScript pricer at increasing resolution (1h buckets for the oldest
// history, 5 s steps for the last hour, per block for the live tail).
// Everything the api can be asked for is derived from the records this
// produces, so live values, series and blocks always agree with each other.

import {
  addGas,
  baseFeeFromExponent,
  constraintExponentBips,
  legacyAddGas,
  legacyExponentBips,
  legacyStep,
  ONE_IN_BIPS,
  step,
  toLegacyState,
  toState,
  type ConstraintState,
  type LegacyState,
} from "@/lib/pricer";
import type {
  BatchResolution,
  BatchSeries,
  BlockPoint,
  ConstraintSet,
  ConstraintsResponse,
  EthUsd,
  L1Series,
  LiveSnapshot,
  Network,
  NetworkStatus,
  OwnerAction,
  Series,
  SeriesPoint,
  SeriesRange,
} from "@/types";
import type { DemandProfile, MockNetworkDef } from "./defs";

export type WorldRecord = {
  t: number;
  dt: number;
  blocks: number;
  gas: number;
  feeMin: bigint;
  feeMax: bigint;
  /** Sum of block base fees, so the bucket average is weighted by block count. */
  feeSum: bigint;
  feesWei: bigint;
  /** Start-of-block exponent of the record's last block, and its per-constraint split. */
  exponent: number;
  constraintBips: number[];
  backlogs: number[];
  backlogsMax: number[];
  /** Floor in force at the record's last block. */
  minFee: bigint;
  /** Σ gasUsed × minBaseFee per block, and feesWei minus that, exact. */
  floorFeesWei: bigint;
  surplusFeesWei: bigint;
  setId: number;
  replayErrorBips: number;
};

export function isoToUnix(iso: string): number {
  return Math.floor(new Date(iso).getTime() / 1000);
}

export function unixToIso(t: number): string {
  return new Date(t * 1000).toISOString();
}

/** Deterministic hash of (seed, i) to [0, 1). */
export function hash01(seed: number, i: number): number {
  let h = (seed ^ Math.imul(i | 0, 0x9e3779b1)) >>> 0;
  h = Math.imul(h ^ (h >>> 16), 0x85ebca6b) >>> 0;
  h = Math.imul(h ^ (h >>> 13), 0xc2b2ae35) >>> 0;
  h ^= h >>> 16;
  return (h >>> 0) / 4294967296;
}

function alignDown(t: number, unit: number): number {
  return Math.floor(t / unit) * unit;
}

function maxBigInt(a: bigint, b: bigint): bigint {
  return a > b ? a : b;
}

function minBigInt(a: bigint, b: bigint): bigint {
  return a < b ? a : b;
}

type BaselinePoint = { t: number; value: number };

export class Demand {
  private readonly points: BaselinePoint[];
  private readonly profile: DemandProfile;

  constructor(profile: DemandProfile) {
    this.profile = profile;
    this.points = profile.baseline.map(([iso, value]) => ({ t: isoToUnix(iso), value }));
  }

  baseline(t: number): number {
    const pts = this.points;
    if (t <= pts[0].t) return pts[0].value;
    for (let i = 1; i < pts.length; i++) {
      if (t <= pts[i].t) {
        const a = pts[i - 1];
        const b = pts[i];
        const f = (t - a.t) / (b.t - a.t);
        return a.value + (b.value - a.value) * f;
      }
    }
    return pts[pts.length - 1].value;
  }

  /** Average burst gas/s over [t, t + dt], from a deterministic slot schedule. */
  burstAverage(t: number, dt: number): number {
    const { slotSeconds, probability, minDuration, maxDuration, minIntensity, maxIntensity } = this.profile.burst;
    const seed = this.profile.seed;
    const firstSlot = Math.floor((t - maxDuration) / slotSeconds);
    const lastSlot = Math.floor((t + dt) / slotSeconds);
    let sum = 0;
    for (let s = firstSlot; s <= lastSlot; s++) {
      if (hash01(seed, s) >= probability) continue;
      const duration = minDuration + (maxDuration - minDuration) * hash01(seed + 2, s);
      const start = s * slotSeconds + hash01(seed + 1, s) * Math.max(0, slotSeconds - duration);
      const intensity = minIntensity + (maxIntensity - minIntensity) * hash01(seed + 3, s);
      const overlap = Math.min(t + dt, start + duration) - Math.max(t, start);
      if (overlap > 0) sum += overlap * intensity;
    }
    return sum / dt;
  }

  /** Average total demand in gas/s over [t, t + dt]. */
  average(t: number, dt: number): number {
    const mid = t + dt / 2;
    const diurnal = 1 + this.profile.diurnalAmplitude * Math.sin((2 * Math.PI * mid) / 86_400 - Math.PI / 2);
    const noise = 1 + this.profile.noiseAmplitude * (hash01(this.profile.seed + 7, Math.floor(mid / 3600)) * 2 - 1);
    return Math.max(0, this.baseline(mid) * diurnal * noise + this.burstAverage(t, dt));
  }
}

const RANGE_SPEC: Record<SeriesRange, { seconds: number; resolution: Series["resolution"]; span: number }> = {
  "1h": { seconds: 5, resolution: "5s", span: 3600 },
  "24h": { seconds: 60, resolution: "1m", span: 86_400 },
  "30d": { seconds: 900, resolution: "15m", span: 30 * 86_400 },
  all: { seconds: 3600, resolution: "1h", span: Number.POSITIVE_INFINITY },
};

const BATCH_RESOLUTION: Record<SeriesRange, { seconds: number; resolution: BatchResolution }> = {
  "1h": { seconds: 15, resolution: "batch" },
  "24h": { seconds: 900, resolution: "15m" },
  "30d": { seconds: 3600, resolution: "1h" },
  all: { seconds: 3600, resolution: "1h" },
};

const RING_SIZE = 1000;
const LIVE_TAIL_SECONDS = 120;
/** Beyond this gap the world catches up with 5 s steps instead of per-block ticks. */
const CATCH_UP_THRESHOLD = 600;

export class MockWorld {
  readonly def: MockNetworkDef;
  readonly startAt: number;
  readonly demand: Demand;
  readonly records: WorldRecord[] = [];
  readonly blocks: BlockPoint[] = [];
  /** The next second to simulate. */
  time: number;

  private constraints: ConstraintState[] = [];
  private legacy: LegacyState | null = null;
  private setIndex = -1;
  private minFee = 0n;
  private minFeeIndex = -1;
  private lastFee = 0n;
  private lastExponent = 0;
  private lastContributions: number[] = [];
  private lastReplayError = 0;
  private accumulatedInfra = 0n;
  private accumulatedNetwork = 0n;
  /** Per constraint-set index, an adjustment to the starting backlogs (see `anchor`). */
  private startingBacklogAdjust = new Map<number, bigint[]>();

  constructor(def: MockNetworkDef, now: number) {
    this.def = def;
    this.demand = new Demand(def.demand);
    this.startAt = isoToUnix(def.historyStart);
    this.time = this.startAt;
    this.run(now);
    this.anchor(now);
  }

  private reset(): void {
    this.records.length = 0;
    this.blocks.length = 0;
    this.time = this.startAt;
    this.constraints = [];
    this.legacy = null;
    this.setIndex = -1;
    this.minFee = 0n;
    this.minFeeIndex = -1;
    this.lastFee = 0n;
    this.lastExponent = 0;
    this.lastContributions = [];
    this.lastReplayError = 0;
    this.accumulatedInfra = 0n;
    this.accumulatedNetwork = 0n;
  }

  private run(now: number): void {
    this.reset();
    if (this.def.model === "legacy" && this.def.legacy) {
      this.legacy = toLegacyState({ ...this.def.legacy, backlog: 0 });
    }
    this.applyParamsAt(this.startAt);
    this.buildHistory(now);
    this.advanceTo(now);
  }

  /**
   * Lands anchored constraints exactly on the desired backlog at `now`. Backlog
   * dynamics are linear away from zero, so the difference between the natural
   * end state and the anchor is folded into the starting backlog of the set in
   * force and the history replayed once more; the tiny residual from per-block
   * rounding is then snapped, the way the collector re-anchors to samples.
   */
  private anchor(now: number): void {
    const anchors = this.def.anchorBacklogs;
    if (!anchors || this.legacy || this.setIndex < 0) return;
    const delta = this.constraints.map((c, i) => {
      const target = anchors[i];
      return target === null || target === undefined ? 0n : BigInt(target) - c.backlog;
    });
    if (delta.some((d) => d !== 0n)) {
      this.startingBacklogAdjust.set(this.setIndex, delta);
      this.run(now);
    }
    this.constraints.forEach((c, i) => {
      const target = anchors[i];
      if (target !== null && target !== undefined) c.backlog = BigInt(target);
    });
  }

  // Parameters in force at t: constraint set (resetting backlogs on change) and min fee.

  private applyParamsAt(t: number): void {
    const sets = this.def.constraintSets;
    let idx = -1;
    for (let i = 0; i < sets.length; i++) {
      if (isoToUnix(sets[i].effectiveAt) <= t) idx = i;
    }
    if (idx !== this.setIndex && idx >= 0) {
      this.setIndex = idx;
      const adjust = this.startingBacklogAdjust.get(idx);
      this.constraints = toState(
        sets[idx].constraints.map((c) => ({ target: c.target, window: c.window, backlog: c.startingBacklog })),
      );
      if (adjust) {
        this.constraints.forEach((c, i) => {
          const adjusted = c.backlog + (adjust[i] ?? 0n);
          c.backlog = adjusted > 0n ? adjusted : 0n;
        });
      }
    }
    const fees = this.def.minFeeHistory;
    let feeIdx = -1;
    for (let i = 0; i < fees.length; i++) {
      if (isoToUnix(fees[i].at) <= t) feeIdx = i;
    }
    if (feeIdx !== this.minFeeIndex && feeIdx >= 0) {
      this.minFeeIndex = feeIdx;
      this.minFee = fees[feeIdx].wei;
    }
  }

  blockAt(t: number): number {
    return this.def.blockAtHistoryStart + Math.floor((t - this.startAt) * this.def.blocksPerSecond);
  }

  private currentBacklogs(): number[] {
    if (this.legacy) return [Number(this.legacy.backlog)];
    return this.constraints.map((c) => Number(c.backlog));
  }

  private currentSetId(): number {
    return this.legacy ? 0 : this.setIndex + 1;
  }

  /** The fee, exponent and per-constraint split implied by the current backlogs, without draining (the Go legacy model reports one element). */
  private priceCurrent(): { fee: bigint; exponent: number; contributions: number[] } {
    if (this.legacy) {
      const exponent = legacyExponentBips(this.legacy);
      return { fee: baseFeeFromExponent(this.minFee, exponent), exponent: Number(exponent), contributions: [Number(exponent)] };
    }
    const contributions = this.constraints.map((c) => constraintExponentBips(c));
    const exponent = contributions.reduce((sum, c) => sum + c, 0n);
    return { fee: baseFeeFromExponent(this.minFee, exponent), exponent: Number(exponent), contributions: contributions.map((c) => Number(c)) };
  }

  /**
   * Fluid update over `dt` seconds absorbing `gas`: B = max(0, B - target*dt + gas).
   * Exact for constant demand within the step, and unlike "drain, then add a
   * whole step of gas" it does not leave a short-window backlog holding gas
   * that the next, finer step could never have drained.
   */
  private fluidUpdate(dt: bigint, gas: bigint): void {
    if (this.legacy) {
      const next = this.legacy.backlog - dt * this.legacy.speedLimit + gas;
      this.legacy.backlog = next > 0n ? next : 0n;
      return;
    }
    for (const c of this.constraints) {
      const next = c.backlog - dt * c.target + gas;
      c.backlog = next > 0n ? next : 0n;
    }
  }

  /** One exact pricer step for a single block: drain by dt, price, then absorb the block's gas. */
  private blockStep(dt: bigint, gas: bigint): { fee: bigint; exponent: number; contributions: number[] } {
    if (this.legacy) {
      const r = legacyStep(this.legacy, dt, this.minFee);
      legacyAddGas(this.legacy, gas);
      return { fee: r.baseFee, exponent: Number(r.exponent), contributions: [Number(r.exponent)] };
    }
    const r = step(this.constraints, dt, this.minFee);
    addGas(this.constraints, gas);
    return { fee: r.baseFee, exponent: Number(r.exponent), contributions: r.contributions.map((c) => Number(c)) };
  }

  /** A step of many blocks at once, priced from the state at its start and updated as a fluid. */
  private coarseStep(t: number, dt: number): void {
    this.applyParamsAt(t);
    const gas = Math.round(this.demand.average(t, dt) * dt);
    const blocks = this.blockAt(t + dt) - this.blockAt(t);
    const { fee, exponent, contributions } = this.priceCurrent();
    this.fluidUpdate(BigInt(dt), BigInt(gas));
    const backlogs = this.currentBacklogs();
    const feesWei = BigInt(gas) * fee;
    const floorFeesWei = BigInt(gas) * this.minFee;
    this.records.push({
      t,
      dt,
      blocks,
      gas,
      feeMin: fee,
      feeMax: fee,
      feeSum: fee * BigInt(blocks),
      feesWei,
      exponent,
      constraintBips: contributions,
      backlogs,
      backlogsMax: backlogs,
      minFee: this.minFee,
      floorFeesWei,
      surplusFeesWei: feesWei - floorFeesWei,
      setId: this.currentSetId(),
      replayErrorBips: 0,
    });
    this.lastFee = fee;
    this.lastExponent = exponent;
    this.lastContributions = contributions;
    this.lastReplayError = 0;
    this.time = t + dt;
  }

  /** The replay error the collector reports for `number`, in bips, from the block number alone. */
  private replayError(number: number): number {
    return Math.floor(hash01(this.def.demand.seed + 5, number) * 6);
  }

  /** Prices and appends one block: an exact pricer step over `dt`, then its gas absorbed. */
  private mintBlock(number: number, ts: number, dt: bigint, gas: number, anchored: boolean): BlockPoint {
    const { fee, exponent, contributions } = this.blockStep(dt, BigInt(gas));
    const err = this.replayError(number);
    const predicted = (fee * (ONE_IN_BIPS - BigInt(err))) / ONE_IN_BIPS;
    const block: BlockPoint = {
      number,
      ts,
      gasUsed: gas,
      baseFee: fee.toString(),
      predictedBaseFee: predicted.toString(),
      backlogs: this.currentBacklogs(),
      constraintBips: contributions,
      exponentBips: exponent,
      minBaseFee: this.minFee.toString(),
      anchored,
    };
    this.blocks.push(block);
    this.lastFee = fee;
    this.lastExponent = exponent;
    this.lastContributions = contributions;
    this.lastReplayError = err;
    return block;
  }

  /** Adds (`sign` 1n) or removes (`sign` -1n) a block's fees from the sampled account balances. */
  private credit(block: BlockPoint, sign: bigint): void {
    const gas = BigInt(block.gasUsed);
    const fee = BigInt(block.baseFee);
    const minFee = BigInt(block.minBaseFee);
    this.accumulatedInfra += sign * gas * minFee;
    this.accumulatedNetwork += sign * gas * (fee > minFee ? fee - minFee : 0n);
  }

  /**
   * The record of the wall-clock second `t`, from the ring's blocks with that
   * timestamp: sums over the blocks, end-of-second state from the last one,
   * the maximum backlog from `before` (the state the second opened with) and
   * every block.
   */
  private recordFor(t: number, before: number[]): WorldRecord {
    const blocks = this.blocks.filter((b) => b.ts === t);
    const last = blocks[blocks.length - 1];
    let feeMin = 0n;
    let feeMax = 0n;
    let feeSum = 0n;
    let feesWei = 0n;
    let floorFeesWei = 0n;
    let gas = 0;
    let backlogsMax = before;
    blocks.forEach((b, k) => {
      const fee = BigInt(b.baseFee);
      feeMin = k === 0 ? fee : minBigInt(feeMin, fee);
      feeMax = maxBigInt(feeMax, fee);
      feeSum += fee;
      feesWei += BigInt(b.gasUsed) * fee;
      floorFeesWei += BigInt(b.gasUsed) * BigInt(b.minBaseFee);
      gas += b.gasUsed;
      backlogsMax = b.backlogs.map((v, i) => Math.max(v, backlogsMax[i] ?? 0));
    });
    return {
      t,
      dt: 1,
      blocks: blocks.length,
      gas,
      feeMin,
      feeMax,
      feeSum,
      feesWei,
      exponent: last.exponentBips,
      constraintBips: last.constraintBips ?? [],
      backlogs: last.backlogs,
      backlogsMax,
      minFee: BigInt(last.minBaseFee),
      floorFeesWei,
      surplusFeesWei: feesWei - floorFeesWei,
      setId: this.currentSetId(),
      replayErrorBips: this.replayError(last.number),
    };
  }

  /** One wall-clock second with individual blocks, feeding the block ring. */
  private tick(): void {
    const t = this.time;
    this.applyParamsAt(t);
    const total = this.demand.average(t, 1);
    const first = this.blockAt(t);
    const count = Math.max(1, this.blockAt(t + 1) - first);
    const weights: number[] = [];
    let weightSum = 0;
    for (let k = 0; k < count; k++) {
      const w = 0.5 + hash01(this.def.demand.seed + 4, first + k);
      weights.push(w);
      weightSum += w;
    }
    const before = this.currentBacklogs();
    for (let k = 0; k < count; k++) {
      const gas = Math.round((total * weights[k]) / weightSum);
      this.credit(this.mintBlock(first + k, t, k === 0 ? 1n : 0n, gas, k === 0), 1n);
    }
    this.records.push(this.recordFor(t, before));
    if (this.blocks.length > RING_SIZE) this.blocks.splice(0, this.blocks.length - RING_SIZE);
    this.time = t + 1;
  }

  /** Puts the pricer back in the state `block` closed with, so the blocks after it can be replayed. */
  private rewindTo(block: BlockPoint): void {
    if (this.legacy) {
      this.legacy.backlog = BigInt(block.backlogs[0] ?? 0);
      return;
    }
    this.constraints.forEach((c, i) => {
      c.backlog = BigInt(block.backlogs[i] ?? 0);
    });
  }

  /**
   * Replaces the last `depth` blocks of the ring with a canonical fork, the
   * way a reorg does: the pricer is rewound to the ancestor's end-of-block
   * state, the orphaned block numbers are replayed with different gas, and
   * the records of the seconds they belong to are rebuilt from the ring, so
   * series, balances and the live snapshot agree with the new tail. Returns
   * the ancestor and the canonical blocks, oldest first; null when the ring
   * is too short.
   *
   * Every ring block was minted by `tick`, which records its second as one
   * per-second record and never overlaps a coarse one, so the records from
   * the first forked second on are exactly the ones to rebuild.
   */
  reorg(depth: number, salt = 1): { ancestor: number; blocks: BlockPoint[] } | null {
    if (!Number.isInteger(depth) || depth <= 0 || this.blocks.length <= depth) return null;
    const ancestor = this.blocks[this.blocks.length - 1 - depth];
    const firstTs = this.blocks[this.blocks.length - depth].ts;
    let i = this.records.length;
    while (i > 0 && this.records[i - 1].t >= firstTs) i -= 1;
    const orphaned = this.blocks.splice(this.blocks.length - depth);
    this.records.splice(i);
    for (const b of orphaned) this.credit(b, -1n);
    this.rewindTo(ancestor);
    let prevTs = ancestor.ts;
    for (const b of orphaned) {
      const gas = Math.round(b.gasUsed * (0.5 + hash01(this.def.demand.seed + 11 + salt, b.number)));
      this.credit(this.mintBlock(b.number, b.ts, b.ts > prevTs ? 1n : 0n, gas, b.anchored), 1n);
      prevTs = b.ts;
    }
    for (let t = firstTs; t < this.time; t++) {
      const opened = this.blocks.filter((b) => b.ts < t).pop();
      this.records.push(this.recordFor(t, opened ? opened.backlogs : []));
    }
    return { ancestor: ancestor.number, blocks: this.blocks.slice(-depth) };
  }

  private buildHistory(now: number): void {
    const tailStart = alignDown(now - LIVE_TAIL_SECONDS, 5);
    const boundaries = [
      { end: alignDown(now - 30 * 86_400, 900), dt: 1200 },
      { end: alignDown(now - 86_400, 60), dt: 300 },
      { end: alignDown(now - 3600, 5), dt: 20 },
      { end: tailStart, dt: 5 },
    ];
    for (const { end, dt } of boundaries) {
      while (this.time < end) {
        const stepDt = Math.min(dt, end - this.time);
        this.coarseStep(this.time, stepDt);
      }
    }
  }

  /** Brings the world up to `now` (unix seconds). */
  advanceTo(now: number): void {
    while (this.time < now) {
      if (now - this.time > CATCH_UP_THRESHOLD) {
        this.coarseStep(this.time, Math.min(5, now - CATCH_UP_THRESHOLD - this.time));
      } else {
        this.tick();
      }
    }
  }

  get headBlock(): number {
    return this.blockAt(this.time) - 1;
  }

  network(now: number): Network {
    return {
      name: this.def.name,
      displayName: this.def.displayName,
      chainId: this.def.chainId,
      explorerUrl: this.def.explorerUrl,
      model: this.def.model,
      headBlock: this.headBlock,
      headAt: unixToIso(this.time - 1),
      lagSeconds: Math.max(0, now - (this.time - 1)),
      enabled: true,
    };
  }

  status(now: number): NetworkStatus {
    return {
      name: this.def.name,
      chainId: this.def.chainId,
      headBlock: this.headBlock,
      headAt: unixToIso(this.time - 1),
      lagSeconds: Math.max(0, now - (this.time - 1)),
      lastSampleAt: unixToIso(this.time - 1),
      lastError: null,
      rateLimitEvents: 0,
    };
  }

  private constraintSet(index: number): ConstraintSet {
    const set = this.def.constraintSets[index];
    return {
      id: index + 1,
      effectiveBlock: set.effectiveBlock,
      effectiveAt: set.effectiveAt,
      source: set.source,
      constraints: set.constraints.map((c) => ({ ...c })),
    };
  }

  private l1State(now: number) {
    const l1 = this.def.l1;
    const interval = this.def.batch.intervalSeconds;
    const sinceUpdate = now % interval;
    const wobble = 1 + 0.06 * Math.sin((2 * Math.PI * now) / 5400);
    return {
      baseFeeEstimate: Math.round(l1.baseFeeEstimateWei * wobble).toString(),
      surplus: (l1.surplusWei + BigInt(Math.round(Math.sin(now / 900) * 1e12))).toString(),
      feesAvailable: l1.feesAvailableWei.toString(),
      unitsSinceUpdate: Math.round((sinceUpdate / interval) * this.def.batch.calldataBytes * 16),
      lastUpdateAt: unixToIso(now - sinceUpdate),
      equilibrationUnits: l1.equilibrationUnits,
      perBatchGasCharge: l1.perBatchGasCharge,
      rewardRate: l1.rewardRate,
    };
  }

  /** The snapshot the collector would publish now: the state after the last block of the ring. */
  snapshot(now: number): LiveSnapshot {
    const last = this.blocks[this.blocks.length - 1];
    return this.composeSnapshot({
      block: last,
      fee: this.lastFee,
      backlogs: this.currentBacklogs(),
      exponent: this.lastExponent,
      replayError: this.lastReplayError,
      sampledAt: unixToIso(now),
      now,
    });
  }

  /**
   * The snapshot the collector publishes right after `block`, when it samples
   * every block: the block's own fee, its end-of-block backlogs and the
   * exponent that priced it. Within one wall-clock second only the first
   * block drains the backlogs, so consecutive snapshots show the short
   * window's sawtooth. `sampledAtMs` keeps millisecond precision.
   */
  snapshotForBlock(block: BlockPoint, sampledAtMs: number): LiveSnapshot {
    const fee = BigInt(block.baseFee);
    const predicted = BigInt(block.predictedBaseFee);
    const replayError = fee > 0n ? Math.round((Number(fee - predicted) * 10_000) / Number(fee)) : 0;
    return this.composeSnapshot({
      block,
      fee,
      backlogs: block.backlogs,
      exponent: block.exponentBips,
      replayError: Math.abs(replayError),
      sampledAt: new Date(sampledAtMs).toISOString(),
      now: Math.floor(sampledAtMs / 1000),
    });
  }

  private composeSnapshot(args: { block: BlockPoint; fee: bigint; backlogs: number[]; exponent: number; replayError: number; sampledAt: string; now: number }): LiveSnapshot {
    const { block, fee, backlogs, exponent, replayError, sampledAt, now } = args;
    const minFee = BigInt(block.minBaseFee);
    const congestion = fee > minFee ? fee - minFee : 0n;
    const l1 = this.l1State(now);
    const constraints = this.legacy
      ? []
      : this.constraints.map((c, i) => {
          const backlog = BigInt(backlogs[i] ?? 0);
          return {
            target: Number(c.target),
            window: Number(c.window),
            backlog: Number(backlog),
            exponentBips: Number(constraintExponentBips({ target: c.target, window: c.window, backlog })),
          };
        });
    const accounts = this.def.accounts;
    return {
      chainId: this.def.chainId,
      sampledAt,
      block: {
        number: block.number,
        ts: block.ts,
        gasUsed: block.gasUsed,
        baseFee: block.baseFee,
        txCount: 1 + Math.max(0, Math.round(block.gasUsed / 45_000)),
      },
      baseFee: fee.toString(),
      minBaseFee: minFee.toString(),
      multiplierBips: Number((fee * ONE_IN_BIPS) / minFee),
      exponentBips: exponent,
      model: this.def.model,
      constraints,
      ...(this.legacy
        ? {
            legacy: {
              speedLimit: Number(this.legacy.speedLimit),
              inertia: Number(this.legacy.inertia),
              tolerance: Number(this.legacy.tolerance),
              backlog: backlogs[0] ?? 0,
            },
          }
        : {}),
      prices: {
        perL2Tx: (BigInt(l1.baseFeeEstimate) * 100n).toString(),
        perL1CalldataByte: (BigInt(l1.baseFeeEstimate) * 16n).toString(),
        perL2Storage: (fee * 20_000n).toString(),
        perArbGasBase: minFee.toString(),
        perArbGasCongestion: congestion.toString(),
        perArbGasTotal: fee.toString(),
      },
      gasPerSecond: { s10: this.gasPerSecondUpTo(10, block), s60: this.gasPerSecondUpTo(60, block) },
      l1,
      accounts: {
        infra: { address: accounts.infra, balance: (accounts.infraWei + this.accumulatedInfra).toString() },
        network: { address: accounts.network, balance: (accounts.networkWei + this.accumulatedNetwork).toString() },
        l1Reward: { address: accounts.l1Reward, balance: accounts.l1RewardWei.toString() },
      },
      replayErrorBips: replayError,
      ethUsd: this.ethUsd(now),
    };
  }

  /**
   * The ETH/USD quote the collector's slow loop would hold: refetched on the
   * minute and quoted with the minute it was fetched in, so the UI sees a
   * price that ages for up to a minute and then refreshes. Null for a network
   * defined without a price feed.
   */
  ethUsd(now: number): EthUsd | null {
    const profile = this.def.ethUsd;
    if (!profile) return null;
    const minute = alignDown(now, 60);
    // A slow deterministic walk of about a percent either way, so the figure moves without ever looking implausible.
    const drift = 1 + (hash01(this.def.chainId, minute / 60) - 0.5) * 0.02;
    return { price: (profile.basePrice * drift).toFixed(2), at: unixToIso(minute), source: profile.source };
  }

  /** Average gas per second over the `windowSeconds` timestamp seconds ending with the last block. */
  gasPerSecond(windowSeconds: number): number {
    return this.gasPerSecondUpTo(windowSeconds, this.blocks[this.blocks.length - 1]);
  }

  /** The same window ending with `last`, ignoring blocks after it, so a per-block snapshot sees the rate as of that block. */
  private gasPerSecondUpTo(windowSeconds: number, last: BlockPoint): number {
    const from = last.ts + 1 - windowSeconds;
    let gas = 0;
    for (let i = this.blocks.length - 1; i >= 0; i--) {
      const b = this.blocks[i];
      if (b.number > last.number) continue;
      if (b.ts < from) break;
      gas += b.gasUsed;
    }
    return Math.round(gas / windowSeconds);
  }

  recentBlocks(limit: number): BlockPoint[] {
    const n = Math.max(0, Math.min(limit, this.blocks.length));
    return this.blocks.slice(this.blocks.length - n);
  }

  /** Blocks with a number greater than `after`, oldest first. */
  blocksAfter(after: number): BlockPoint[] {
    const out: BlockPoint[] = [];
    for (let i = this.blocks.length - 1; i >= 0; i--) {
      if (this.blocks[i].number <= after) break;
      out.push(this.blocks[i]);
    }
    return out.reverse();
  }

  /** Constraint sets in force at any time in [from, to]. */
  constraintSetsFor(from: number, to: number): ConstraintSet[] {
    const sets = this.def.constraintSets;
    const out: ConstraintSet[] = [];
    let lastBefore = -1;
    for (let i = 0; i < sets.length; i++) {
      const at = isoToUnix(sets[i].effectiveAt);
      if (at <= from) lastBefore = i;
      else if (at <= to) out.push(this.constraintSet(i));
    }
    if (lastBefore >= 0) out.unshift(this.constraintSet(lastBefore));
    return out;
  }

  ownerActionsFor(from: number, to: number): OwnerAction[] {
    return this.def.ownerActions
      .filter((a) => {
        const at = isoToUnix(a.at);
        return at >= from && at <= to;
      })
      .sort((a, b) => a.block - b.block);
  }

  ownerActions(): OwnerAction[] {
    return [...this.def.ownerActions].sort((a, b) => b.block - a.block);
  }

  constraintsResponse(): ConstraintsResponse {
    const sets = this.def.constraintSets;
    if (sets.length === 0) {
      // Legacy chains have no constraint set; the API returns null.
      return { current: null, history: [] };
    }
    const history = sets.map((_, i) => this.constraintSet(i));
    return { current: history[history.length - 1], history };
  }

  series(range: SeriesRange, now: number): Series {
    const spec = RANGE_SPEC[range];
    const from = range === "all" ? this.startAt : now - spec.span;
    const buckets = new Map<number, WorldRecord & { duration: number }>();
    for (let i = this.records.length - 1; i >= 0; i--) {
      const r = this.records[i];
      if (r.t < from) break;
      const start = alignDown(r.t, spec.seconds);
      const acc = buckets.get(start);
      if (!acc) {
        buckets.set(start, { ...r, t: start, duration: r.dt, backlogs: [...r.backlogs], backlogsMax: [...r.backlogsMax], constraintBips: [...r.constraintBips] });
        continue;
      }
      // Records are visited newest first, so the first one seen carries the
      // end-of-bucket state (backlogs, constraintBips, minFee); sums accumulate.
      acc.duration += r.dt;
      acc.blocks += r.blocks;
      acc.gas += r.gas;
      acc.feeMin = minBigInt(acc.feeMin, r.feeMin);
      acc.feeMax = maxBigInt(acc.feeMax, r.feeMax);
      acc.feeSum += r.feeSum;
      acc.feesWei += r.feesWei;
      acc.floorFeesWei += r.floorFeesWei;
      acc.surplusFeesWei += r.surplusFeesWei;
      acc.backlogsMax = acc.backlogsMax.map((v, j) => Math.max(v, r.backlogsMax[j] ?? 0));
      acc.replayErrorBips = Math.max(acc.replayErrorBips, r.replayErrorBips);
    }
    // Buckets that predate the split migration are served the way the api
    // serves pre-000006 history: no split, no fee destinations.
    const recordedFrom = this.def.splitRecordedFrom ? isoToUnix(this.def.splitRecordedFrom) : this.startAt;
    const points: SeriesPoint[] = [...buckets.values()]
      .sort((a, b) => a.t - b.t)
      .map((b) => {
        const recorded = b.t >= recordedFrom;
        return {
          t: b.t,
          blocks: b.blocks,
          gasUsed: b.gas,
          gasPerSecond: b.duration > 0 ? Math.round(b.gas / b.duration) : 0,
          // What the collector has of the bucket: a whole one everywhere but
          // at the two ends, where the bucket in progress and the first one
          // after the world began hold only part of their span.
          coverage: Math.min(1, b.duration / spec.seconds),
          feesWei: b.feesWei.toString(),
          baseFeeMin: b.feeMin.toString(),
          baseFeeAvg: (b.blocks > 0 ? b.feeSum / BigInt(b.blocks) : b.feeMin).toString(),
          baseFeeMax: b.feeMax.toString(),
          exponentBips: b.exponent,
          constraintBips: recorded ? b.constraintBips : null,
          backlogs: b.backlogs,
          backlogsMax: b.backlogsMax,
          minBaseFee: b.minFee.toString(),
          floorFeesWei: recorded ? b.floorFeesWei.toString() : null,
          surplusFeesWei: recorded ? b.surplusFeesWei.toString() : null,
          constraintSetId: b.setId,
          replayErrorBips: b.replayErrorBips,
        };
      });
    return {
      range,
      resolution: spec.resolution,
      // The window that was asked for, so a chart spans it whatever the
      // collector has indexed of it. `all` reaches back to the first indexed
      // point, and to `now` alone when nothing is indexed.
      from: range === "all" ? (points[0]?.t ?? now) : from,
      to: now,
      constraintSets: this.constraintSetsFor(from, now),
      ownerActions: this.ownerActionsFor(from, now),
      points,
    };
  }

  batches(range: SeriesRange, now: number): BatchSeries {
    const spec = BATCH_RESOLUTION[range];
    const from = range === "all" ? this.startAt : now - RANGE_SPEC[range].span;
    const batch = this.def.batch;
    const seed = this.def.demand.seed + 9;
    const points: BatchSeries["points"] = [];
    for (let t = alignDown(from, spec.seconds); t < now; t += spec.seconds) {
      const slot = Math.floor(t / spec.seconds);
      const covered = Math.min(spec.seconds, now - t);
      const batches = Math.max(0, Math.round((covered / batch.intervalSeconds) * (0.85 + 0.3 * hash01(seed, slot))));
      const feeWobble = 1 + 0.35 * Math.sin((2 * Math.PI * t) / 86_400 + 1) + 0.1 * (hash01(seed + 1, slot) * 2 - 1);
      const l1BaseFee = Math.round(batch.l1BaseFeeWei * feeWobble);
      const gasSpent = Math.round(batches * batch.gasSpent * (0.95 + 0.1 * hash01(seed + 2, slot)));
      points.push({
        t,
        batches,
        gasSpent,
        weiSpent: (BigInt(gasSpent) * BigInt(l1BaseFee)).toString(),
        l1BaseFeeAvg: l1BaseFee.toString(),
        calldataBytes: batches * batch.calldataBytes,
      });
    }
    return { range, resolution: spec.resolution, from: range === "all" ? (points[0]?.t ?? now) : from, to: now, points };
  }

  l1Series(range: SeriesRange, now: number): L1Series {
    const spec = BATCH_RESOLUTION[range];
    const from = range === "all" ? this.startAt : now - RANGE_SPEC[range].span;
    const points: L1Series["points"] = [];
    for (let t = alignDown(from, spec.seconds); t < now; t += spec.seconds) {
      const s = this.l1State(t);
      points.push({
        t,
        baseFeeEstimate: s.baseFeeEstimate,
        surplus: s.surplus,
        feesAvailable: s.feesAvailable,
        unitsSinceUpdate: s.unitsSinceUpdate,
      });
    }
    return { range, from: range === "all" ? (points[0]?.t ?? now) : from, to: now, points };
  }

  /** Exponent from the live backlogs, for tests that check consistency with the pricer. */
  liveExponentBips(): number {
    if (this.legacy) return Number(legacyExponentBips(this.legacy));
    let total = 0n;
    for (const c of this.constraints) total += constraintExponentBips(c);
    return Number(total);
  }
}
