// Shapes mirror docs/ARCHITECTURE.md section 6 (REST) and section 7 (WebSocket).
// Field names are the contract with the Go api: do not rename them here.

export type PricerModel = "constraints" | "legacy" | "unknown";

export type SeriesRange = "1h" | "24h" | "30d" | "all";

export type SeriesResolution = "block" | "5s" | "1m" | "15m" | "1h";

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
  block: { number: number; ts: number; gasUsed: number; baseFee: string; txCount: number };
  baseFee: string;
  minBaseFee: string;
  multiplierBips: number;
  exponentBips: number;
  model: "constraints" | "legacy";
  constraints: Constraint[];
  legacy?: LegacyParams;
  prices: Prices;
  gasPerSecond: { s10: number; s60: number };
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
  baseFee: string;
  predictedBaseFee: string;
  /** End-of-block backlogs (after AddGas). */
  backlogs: number[];
  /**
   * Start-of-block per-constraint exponent, the values that priced this block;
   * sums to exponentBips. Null only for rows written before migration 000006.
   */
  constraintBips: number[] | null;
  exponentBips: number;
  minBaseFee: string;
  anchored: boolean;
};

export type SeriesPoint = {
  t: number;
  blocks: number;
  gasUsed: number;
  gasPerSecond: number;
  feesWei: string;
  baseFeeMin: string;
  baseFeeAvg: string;
  baseFeeMax: string;
  exponentBips: number;
  /** Start-of-block values of the bucket's last block; null for history written before migration 000006. */
  constraintBips: number[] | null;
  backlogs: number[];
  backlogsMax: number[];
  /** Floor in force at the bucket's last block. */
  minBaseFee: string;
  /** Sum of gasUsed times minBaseFee per block, exact; null for history written before migration 000006. */
  floorFeesWei: string | null;
  /** feesWei minus floorFeesWei, exact; null whenever floorFeesWei is. */
  surplusFeesWei: string | null;
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

export type BatchSeries = { range: string; resolution: BatchResolution; points: BatchPoint[] };

export type L1Point = {
  t: number;
  baseFeeEstimate: string;
  surplus: string;
  feesAvailable: string;
  unitsSinceUpdate: number;
};

export type L1Series = { range: string; points: L1Point[] };

export type NetworkStatus = {
  name: string;
  chainId: number;
  headBlock: number;
  headAt: string | null;
  lagSeconds: number | null;
  lastSampleAt: string | null;
  lastError: string | null;
  rateLimitEvents: number;
  enabled?: boolean;
  last429At?: string | null;
  backfillCursor?: number | null;
  arbosVersion?: number | null;
};

export type StatusResponse = { version: string; networks: NetworkStatus[] };

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
