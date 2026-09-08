// Shapes mirror docs/ARCHITECTURE.md section 6 (REST) and section 7 (WebSocket).
// Field names are the contract with the Go api: do not rename them here.

export type PricerModel = "constraints" | "legacy" | "unknown";

export type SeriesRange = "1h" | "24h" | "30d" | "all";

export type SeriesResolution = "block" | "5s" | "1m" | "15m" | "1h";

/** Whether a series bucket's aggregates include every block in its span. */
export type SeriesCompleteness = "complete" | "partial" | "unknown";

export type ConstraintSetSource = "genesis" | "owner_action" | "observed";

export type Network = {
  name: string;
  displayName: string;
  chainId: number;
  explorerUrl: string;
  model: PricerModel;
  headBlock: number;
  /** Null until the collector has produced a head. */
  headAt: string | null;
  lagSeconds: number | null;
  enabled: boolean;
};

export type Constraint = {
  target: number;
  window: number;
  backlog: number;
  exponentBips: number;
};

export type ConstraintSetEntry = {
  target: number;
  window: number;
  startingBacklog: number;
};

export type ConstraintSet = {
  id: number;
  effectiveBlock: number;
  effectiveAt: string;
  source: ConstraintSetSource;
  constraints: ConstraintSetEntry[];
};

export type Account = { address: string; balance: string };

/** ETH/USD spot fetched by the collector's slow loop, server side; `at` is when it was fetched. */
export type EthUsd = { price: string; at: string; source: string };

export type LegacyParams = {
  speedLimit: number;
  inertia: number;
  tolerance: number;
  backlog: number;
};

export type Prices = {
  perL2Tx: string;
  perL1CalldataByte: string;
  perL2Storage: string;
  perArbGasBase: string;
  perArbGasCongestion: string;
  perArbGasTotal: string;
};

export type L1State = {
  baseFeeEstimate: string;
  surplus: string;
  feesAvailable: string;
  unitsSinceUpdate: number;
  lastUpdateAt: string;
  equilibrationUnits: number;
  perBatchGasCharge: number;
  rewardRate: number;
};

export type LiveSnapshot = {
  chainId: number;
  sampledAt: string;
  block: { number: number; ts: number; gasUsed: number; /** Null until receipt poster gas is available. Absent only during a rolling api upgrade. */ posterGas?: number | null; baseFee: string; txCount: number };
  baseFee: string;
  minBaseFee: string;
  multiplierBips: number;
  exponentBips: number;
  model: "constraints" | "legacy";
  constraints: Constraint[];
  legacy?: LegacyParams;
  prices: Prices;
  /** Total gas rates retained for API compatibility. */
  gasPerSecond: { s10: number; s60: number };
  /** Receipt-backed Nitro pricer input rates. Absent only during a rolling API upgrade. */
  computeGasPerSecond?: { s10: number | null; s60: number | null };
  l1?: L1State;
  accounts?: { infra: Account; network: Account; l1Reward: Account };
  replayErrorBips: number;
  /** Null when the collector has no price. A price older than ten minutes is stale and the UI falls back to ETH. */
  ethUsd: EthUsd | null;
};

export type BlockPoint = {
  number: number;
  ts: number;
  gasUsed: number;
  /** Receipt-backed L1 poster gas. Absent only during a rolling api upgrade. */
  posterGas?: number | null;
  baseFee: string;
  /**
   * The model's fee for this block, computed while replaying its parent. Null when the parent was not
   * replayed: a cold start, a reorg, or the block after a gap.
   */
  predictedBaseFee: string | null;
  /** End-of-block backlogs (after AddGas). */
  backlogs: number[];
  /**
   * Per-constraint exponents that priced this block; sums to exponentBips. Null with no prediction,
   * and for pricing version 0 rows, history recorded before the breakdown existed.
   */
  constraintBips: number[] | null;
  exponentBips: number;
  /** Floor in force at this block; null for pricing version 0 (history without the breakdown). */
  minBaseFee: string | null;
  anchored: boolean;
};

export type SeriesPoint = {
  t: number;
  blocks: number;
  gasUsed: number;
  /** Sum of receipt gasUsedForL1. Null or absent until historical receipt recomputation. */
  posterGas?: number | null;
  /** Total gas rate retained for API compatibility. */
  gasPerSecond: number;
  /** Nitro pricer input rate. Null until receipt poster gas is available; absent only during a rolling api upgrade. */
  computeGasPerSecond?: number | null;
  /**
   * The lowest and highest compute rate any unit of `Series.spreadSeconds` inside the bucket
   * carried: the band around the average. Null together for a bucket holding a unit whose poster
   * gas is unknown, and absent only during a rolling api upgrade.
   */
  computeGasPerSecondMin?: number | null;
  computeGasPerSecondMax?: number | null;
  /**
   * The share of the bucket the collector indexed when it can be measured.
   * Bounded missing intervals reduce it. Insufficient time bounds make it null.
   * Both gas rates cover the measured span, so rates and averages read normally.
   */
  coverage: number | null;
  /** Complete when all blocks are present, partial when an omission is known, and unknown when the available time bounds cannot locate a missing range. */
  completeness: SeriesCompleteness;
  feesWei: string;
  baseFeeMin: string;
  baseFeeAvg: string;
  baseFeeMax: string;
  exponentBips: number;
  /** Start-of-block values of the bucket's last block; null for pricing version 0 history. */
  constraintBips: number[] | null;
  backlogs: number[];
  backlogsMax: number[];
  /** Floor in force at the bucket's last block; null when any block in the bucket has pricing version 0. */
  minBaseFee: string | null;
  /** Compute gas times min(baseFee, minBaseFee), paid to infrastructure. */
  floorFeesWei: string | null;
  /** Compute congestion fees paid to the network account. */
  surplusFeesWei: string | null;
  /** Poster gas times baseFee, paid to the L1 pricer funds pool. Absent only during a rolling api upgrade. */
  posterFeesWei?: string | null;
  constraintSetId: number;
  replayErrorBips: number;
};

export type OwnerAction = {
  block: number;
  at: string;
  txHash: string;
  method: string;
  selector: string;
  args: Record<string, unknown>;
};

export type Series = {
  range: SeriesRange;
  resolution: SeriesResolution;
  /**
   * The window the range asked for, in unix seconds: the axis's own extent,
   * not the extent of the buckets. For `all`, `from` is the first indexed
   * point's time, and equals `to` when nothing is indexed at all.
   */
  from: number;
  to: number;
  /**
   * The width of the unit the points' load band is measured over: a second inside a 5s or 1m
   * bucket, a minute inside 15m, a quarter hour inside 1h. Null for the per-block resolution,
   * whose points have no interior, and absent only during a rolling api upgrade.
   */
  spreadSeconds?: number | null;
  constraintSets: ConstraintSet[];
  ownerActions: OwnerAction[];
  points: SeriesPoint[];
};

export type ConstraintsResponse = { current: ConstraintSet | null; history: ConstraintSet[] };

export type BatchPoint = {
  t: number;
  batches: number;
  gasSpent: number;
  weiSpent: string;
  l1BaseFeeAvg: string;
  calldataBytes: number;
};

export type BatchResolution = "batch" | "1m" | "15m" | "1h";

/** `from` and `to` are the requested window, as on Series. */
export type BatchSeries = { range: string; resolution: BatchResolution; from: number; to: number; points: BatchPoint[] };

export type L1Point = {
  t: number;
  baseFeeEstimate: string;
  surplus: string;
  feesAvailable: string;
  unitsSinceUpdate: number;
};

/** `from` and `to` are the requested window, as on Series. */
export type L1Series = { range: string; from: number; to: number; points: L1Point[] };

export type HealthStatus = "healthy" | "degraded" | "disabled";

export type RPCCapacity = {
  configuredCallsPerSecond: number;
  requiredCallsPerSecond: number;
  observedCallsPerSecond: number;
  headroomCallsPerSecond: number | null;
  saturated: boolean;
  at: string | null;
  checkpointError: boolean;
};

export type MissingRangesStatus = {
  pending: number;
  blocks: number;
  unfillable: number;
  retrying: number;
  oldestAgeSeconds: number;
  checkpointError: boolean;
  pendingBlocks: number;
  oldestPendingAt: string | null;
  oldestPendingAgeSeconds: number | null;
};

export type CollectorLoopStatus = {
  lastSuccessAt: string | null;
  lastErrorAt: string | null;
  lastError: string | null;
  lastDurationMs: number;
  staleAfterSeconds: number;
};

export type CollectorTelemetry = {
  heartbeatAt: string | null;
  heartbeatAgeSeconds?: number;
  heartbeatStaleAfterSeconds: number;
  observedHead: number;
  indexedHead: number;
  headLagBlocks: number;
  loops: { fast: CollectorLoopStatus; slow: CollectorLoopStatus; history: CollectorLoopStatus };
  rpc: { calls: number; requests: number; errors: number; callsLast10Seconds: number; rateLimitEvents: number; last429At: string | null; averageLatencyMs: number };
  database: { operations: number; errors: number; averageLatencyMs: number; lastLatencyMs: number };
};

export type EndpointStatus = {
  index: number;
  ws: boolean;
  archive: boolean;
  disabled: boolean;
  error: string | null;
  wsCooling: boolean;
  wsError: string | null;
};

export type ListenerStatus = { ready: boolean; reconnects: number; lastError: string | null };

export type NetworkStatus = {
  name: string;
  chainId: number;
  enabled: boolean;
  headBlock: number;
  headAt: string | null;
  lagSeconds: number | null;
  lastSampleAt: string | null;
  lastError: string | null;
  rateLimitEvents: number;
  last429At: string | null;
  backfillCursor: string | null;
  arbosVersion: string | null;
  degraded: boolean;
  capacity: RPCCapacity;
  holes: MissingRangesStatus;
  status: HealthStatus;
  degradedReasons: string[];
  collector: CollectorTelemetry | null;
  activeEndpoint: number;
  failovers: number;
  endpoints: EndpointStatus[];
};

export type StatusResponse = {
  version: string;
  status: Exclude<HealthStatus, "disabled">;
  listener?: ListenerStatus;
  networks: NetworkStatus[];
};

export type ApiErrorBody = { error: { code: string; message: string } };

// WebSocket messages (section 7).

export type HelloData = { network: Network; snapshot: LiveSnapshot | null; recentBlocks: BlockPoint[] };

/** Sent before the next tick: drop every block above `ancestor`, append `blocks` (canonical, oldest first). */
export type ReorgData = { chainId: number; ancestor: number; blocks: BlockPoint[] };

export type ServerMessage =
  | { type: "hello"; data: HelloData }
  | { type: "reorg"; data: ReorgData }
  | { type: "tick"; data: LiveSnapshot }
  | { type: "blocks"; data: BlockPoint[] }
  | { type: "owner_action"; data: OwnerAction }
  | { type: "error"; error: { code: string; message: string } }
  | { type: "ping" };

export type ClientMessage = { type: "pong" } | { type: "subscribe"; network: string };

export type LiveStatus = "connecting" | "open" | "reconnecting" | "polling";
