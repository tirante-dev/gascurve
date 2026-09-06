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
  headAt: string;
  lagSeconds: number;
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
};

export type BlockPoint = {
  number: number;
  ts: number;
  gasUsed: number;
  baseFee: string;
  predictedBaseFee: string;
  backlogs: number[];
  exponentBips: number;
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
  backlogs: number[];
  backlogsMax: number[];
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

export type ConstraintsResponse = { current: ConstraintSet; history: ConstraintSet[] };

export type BatchPoint = {
  t: number;
  batches: number;
  gasSpent: number;
  weiSpent: string;
  l1BaseFeeAvg: string;
  calldataBytes: number;
};

export type BatchSeries = { range: string; resolution: string; points: BatchPoint[] };

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
  headAt: string;
  lagSeconds: number;
  lastSampleAt: string;
  lastError: string | null;
  rateLimitEvents: number;
};

export type StatusResponse = { version: string; networks: NetworkStatus[] };

export type ApiErrorBody = { error: { code: string; message: string } };

// WebSocket messages (section 7).

export type HelloData = { network: Network; snapshot: LiveSnapshot; recentBlocks: BlockPoint[] };

export type ServerMessage =
  | { type: "hello"; data: HelloData }
  | { type: "tick"; data: LiveSnapshot }
  | { type: "blocks"; data: BlockPoint[] }
  | { type: "owner_action"; data: OwnerAction }
  | { type: "ping" };

export type ClientMessage = { type: "pong" } | { type: "subscribe"; network: string };

export type LiveStatus = "connecting" | "open" | "reconnecting" | "polling";
