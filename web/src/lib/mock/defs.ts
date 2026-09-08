// Network definitions for the mock world. Values come from docs/SPEC.md
// (section 2, section 3.3 and Appendix B) where the spec has them.

import type { ConstraintSetSource, OwnerAction } from "@/types";

export type BurstProfile = {
  slotSeconds: number;
  probability: number;
  minDuration: number;
  maxDuration: number;
  minIntensity: number;
  maxIntensity: number;
};

export type DemandProfile = {
  /** Piecewise linear baseline, [ISO time, gas per second]. */
  baseline: [string, number][];
  diurnalAmplitude: number;
  noiseAmplitude: number;
  burst: BurstProfile;
  seed: number;
};

export type MockConstraintSet = {
  effectiveAt: string;
  effectiveBlock: number;
  source: ConstraintSetSource;
  constraints: { target: number; window: number; startingBacklog: number }[];
};

export type MockLegacyParams = { speedLimit: number; inertia: number; tolerance: number };

/** The ETH/USD quote the collector's slow loop would carry. A network without one serves `ethUsd: null`, the scenario the UI falls back to ETH in. */
export type EthUsdProfile = { basePrice: number; source: string };

export type BatchProfile = {
  intervalSeconds: number;
  calldataBytes: number;
  gasSpent: number;
  l1BaseFeeWei: number;
};

export type MockNetworkDef = {
  name: string;
  displayName: string;
  chainId: number;
  explorerUrl: string;
  model: "constraints" | "legacy";
  /** Start of the recorded history. */
  historyStart: string;
  blockAtHistoryStart: number;
  blocksPerSecond: number;
  minFeeHistory: { at: string; wei: bigint }[];
  /** ArbOS upgrades in the recorded history, oldest first. The version in force before the first entry
   * is that entry's `from`, since the world does not model anything older. */
  arbosHistory: { at: string; version: number }[];
  constraintSets: MockConstraintSet[];
  legacy?: MockLegacyParams;
  ownerActions: OwnerAction[];
  demand: DemandProfile;
  /** Desired backlog per constraint at "now"; null leaves the simulation alone. */
  anchorBacklogs?: (number | null)[];
  /**
   * When the per-constraint split and the fee destinations started being
   * recorded (pricing version 0 on a real deployment). Series buckets before
   * this carry null `constraintBips`, `floorFeesWei` and `surplusFeesWei`,
   * as the api serves for history written before the migration.
   */
  splitRecordedFrom?: string;
  l1: {
    baseFeeEstimateWei: number;
    surplusWei: bigint;
    feesAvailableWei: bigint;
    equilibrationUnits: number;
    perBatchGasCharge: number;
    rewardRate: number;
  };
  accounts: { infra: string; network: string; l1Reward: string; infraWei: bigint; networkWei: bigint; l1RewardWei: bigint };
  batch: BatchProfile;
  /** Omitted for a network whose collector has no price feed: its snapshots carry `ethUsd: null`. */
  ethUsd?: EthUsdProfile;
};

const GWEI = 1_000_000_000n;
const ETH = 1_000_000_000_000_000_000n;

function ethWei(eth: number): bigint {
  return BigInt(Math.round(eth * 1_000_000)) * (ETH / 1_000_000n);
}

const ROBINHOOD_CONSTRAINT_HISTORY: MockConstraintSet[] = [
  {
    effectiveAt: "2026-04-30T20:37:00Z",
    effectiveBlock: 28,
    source: "genesis",
    constraints: [
      { target: 60_000_000, window: 9, startingBacklog: 0 },
      { target: 41_000_000, window: 52, startingBacklog: 0 },
      { target: 29_000_000, window: 329, startingBacklog: 0 },
      { target: 20_000_000, window: 2_105, startingBacklog: 0 },
      { target: 14_000_000, window: 13_485, startingBacklog: 0 },
      { target: 10_000_000, window: 86_400, startingBacklog: 0 },
    ],
  },
  {
    effectiveAt: "2026-07-10T19:12:00Z",
    effectiveBlock: 6_322_119,
    source: "owner_action",
    constraints: [
      { target: 60_000_000, window: 15, startingBacklog: 0 },
      { target: 15_000_000, window: 86_400, startingBacklog: 1_106_000_000_000 },
    ],
  },
  {
    effectiveAt: "2026-07-24T20:08:00Z",
    effectiveBlock: 18_424_412,
    source: "owner_action",
    constraints: [
      { target: 60_000_000, window: 15, startingBacklog: 0 },
      { target: 20_000_000, window: 86_400, startingBacklog: 2_970_000_000_000 },
    ],
  },
  {
    effectiveAt: "2026-08-20T21:21:00Z",
    effectiveBlock: 41_739_400,
    source: "owner_action",
    constraints: [
      { target: 60_000_000, window: 15, startingBacklog: 0 },
      { target: 18_000_000, window: 86_400, startingBacklog: 0 },
    ],
  },
  {
    effectiveAt: "2026-09-01T16:33:00Z",
    effectiveBlock: 51_865_079,
    source: "owner_action",
    constraints: [
      { target: 60_000_000, window: 15, startingBacklog: 0 },
      { target: 30_000_000, window: 86_400, startingBacklog: 7_492_000_000_000 },
    ],
  },
  {
    effectiveAt: "2026-09-03T17:08:00Z",
    effectiveBlock: 53_578_754,
    source: "owner_action",
    constraints: [
      { target: 60_000_000, window: 15, startingBacklog: 0 },
      { target: 40_000_000, window: 86_400, startingBacklog: 9_989_000_000_000 },
    ],
  },
];

function txHash(seed: number): string {
  let out = "0x";
  let h = seed >>> 0;
  for (let i = 0; i < 64; i++) {
    h = (Math.imul(h, 1664525) + 1013904223) >>> 0;
    out += (h >>> 28).toString(16);
  }
  return out;
}

function constraintAction(set: MockConstraintSet, seed: number): OwnerAction {
  return {
    block: set.effectiveBlock,
    at: set.effectiveAt,
    txHash: txHash(seed),
    method: "setGasPricingConstraints",
    selector: "0xcc0d556a",
    args: { constraints: set.constraints.map((c) => ({ gasTargetPerSecond: c.target, adjustmentWindowSeconds: c.window, startingBacklog: c.startingBacklog })) },
  };
}

const ROBINHOOD_OWNER_ACTIONS: OwnerAction[] = [
  {
    block: 174_150,
    at: "2026-06-24T20:28:00Z",
    txHash: txHash(11),
    method: "setMinimumL2BaseFee",
    selector: "0xa0188cdb",
    args: { priceInWei: "20000000" },
  },
  ...ROBINHOOD_CONSTRAINT_HISTORY.slice(1).map((set, i) => constraintAction(set, 100 + i)),
];

export const ROBINHOOD: MockNetworkDef = {
  name: "robinhood",
  displayName: "Robinhood Chain",
  chainId: 4663,
  explorerUrl: "https://explorer.chain.robinhood.com",
  model: "constraints",
  historyStart: "2026-07-01T00:00:00Z",
  blockAtHistoryStart: 0,
  blocksPerSecond: 10.5,
  minFeeHistory: [
    { at: "2026-04-30T20:37:00Z", wei: 100_000_000n },
    { at: "2026-06-24T20:28:00Z", wei: 20_000_000n },
  ],
  // The chain launched on 51 and upgraded before the recorded history begins, so every bucket here is
  // on one version.
  arbosHistory: [
    { at: "2026-04-30T20:37:00Z", version: 51 },
    { at: "2026-06-16T22:19:00Z", version: 61 },
  ],
  constraintSets: ROBINHOOD_CONSTRAINT_HISTORY,
  ownerActions: ROBINHOOD_OWNER_ACTIONS,
  demand: {
    baseline: [
      ["2026-07-01T00:00:00Z", 4_000_000],
      ["2026-07-10T00:00:00Z", 8_000_000],
      ["2026-07-24T00:00:00Z", 12_000_000],
      ["2026-08-20T00:00:00Z", 14_000_000],
      ["2026-08-24T00:00:00Z", 16_000_000],
      ["2026-08-26T00:00:00Z", 27_000_000],
      ["2026-09-01T12:00:00Z", 34_000_000],
      ["2026-09-03T12:00:00Z", 44_000_000],
      ["2026-09-06T00:00:00Z", 44_000_000],
    ],
    diurnalAmplitude: 0.1,
    noiseAmplitude: 0.06,
    burst: {
      slotSeconds: 240,
      probability: 0.12,
      minDuration: 5,
      maxDuration: 20,
      minIntensity: 40_000_000,
      maxIntensity: 110_000_000,
    },
    seed: 4663,
  },
  anchorBacklogs: [null, 11_194_391_810_886],
  // The first day and a half of the recorded history predates the fee-split migration.
  splitRecordedFrom: "2026-07-02T12:00:00Z",
  l1: {
    baseFeeEstimateWei: 2_369_608,
    surplusWei: 190_000_000_000_000n,
    feesAvailableWei: 1_240_000_000_000_000n,
    equilibrationUnits: 160_000_000,
    perBatchGasCharge: 210_000,
    rewardRate: 10,
  },
  accounts: {
    infra: "0x5a2B80a9c3F0f1e3a5d9c7b8e6f4a2d1c0b9a89BE7",
    network: "0xbC5C3a7A1d2e3f4a5b6c7d8e9f0a1b2c3d4e5F067",
    l1Reward: "0x8F516B99a1b2c3d4e5f60718293a4b5c6d7e82ea",
    infraWei: ethWei(402.31),
    networkWei: ethWei(10_706.42),
    l1RewardWei: ethWei(0.3121),
  },
  batch: { intervalSeconds: 18, calldataBytes: 137, gasSpent: 279_096, l1BaseFeeWei: 73_500_000 },
  ethUsd: { basePrice: 4_200, source: "coingecko" },
};

const ARBITRUM_ONE_SETS: MockConstraintSet[] = [
  {
    effectiveAt: "2026-07-01T00:00:00Z",
    effectiveBlock: 372_000_000,
    source: "observed",
    constraints: [
      { target: 60_000_000, window: 9, startingBacklog: 0 },
      { target: 41_000_000, window: 52, startingBacklog: 0 },
      { target: 29_000_000, window: 329, startingBacklog: 0 },
      { target: 20_000_000, window: 2_105, startingBacklog: 0 },
      { target: 14_000_000, window: 13_485, startingBacklog: 0 },
      { target: 7_000_000, window: 86_400, startingBacklog: 0 },
    ],
  },
  {
    effectiveAt: "2026-08-12T15:00:00Z",
    effectiveBlock: 386_400_000,
    source: "owner_action",
    constraints: [
      { target: 60_000_000, window: 9, startingBacklog: 0 },
      { target: 41_000_000, window: 52, startingBacklog: 0 },
      { target: 29_000_000, window: 329, startingBacklog: 0 },
      { target: 20_000_000, window: 2_105, startingBacklog: 0 },
      { target: 14_000_000, window: 13_485, startingBacklog: 0 },
      { target: 10_000_000, window: 86_400, startingBacklog: 0 },
    ],
  },
];

export const ARBITRUM_ONE: MockNetworkDef = {
  name: "arbitrum-one",
  displayName: "Arbitrum One",
  chainId: 42161,
  explorerUrl: "https://arbiscan.io",
  model: "constraints",
  historyStart: "2026-07-01T00:00:00Z",
  blockAtHistoryStart: 372_000_000,
  blocksPerSecond: 4,
  minFeeHistory: [{ at: "2024-01-01T00:00:00Z", wei: 10_000_000n }],
  // The upgrade lands inside the recorded history, so the deeper ranges carry buckets that span it.
  arbosHistory: [
    { at: "2026-01-08T17:00:00Z", version: 51 },
    { at: "2026-08-20T17:00:00Z", version: 61 },
  ],
  constraintSets: ARBITRUM_ONE_SETS,
  ownerActions: [constraintAction(ARBITRUM_ONE_SETS[1], 200)],
  demand: {
    baseline: [
      ["2026-07-01T00:00:00Z", 6_500_000],
      ["2026-08-01T00:00:00Z", 8_000_000],
      ["2026-08-12T00:00:00Z", 9_000_000],
      ["2026-09-06T00:00:00Z", 9_500_000],
    ],
    diurnalAmplitude: 0.22,
    noiseAmplitude: 0.08,
    burst: {
      slotSeconds: 180,
      probability: 0.2,
      minDuration: 5,
      maxDuration: 20,
      minIntensity: 30_000_000,
      maxIntensity: 120_000_000,
    },
    seed: 42161,
  },
  l1: {
    baseFeeEstimateWei: 1_850_000_000,
    surplusWei: -1_200_000_000_000_000_000n,
    feesAvailableWei: 48_000_000_000_000_000_000n,
    equilibrationUnits: 160_000_000,
    perBatchGasCharge: 240_000,
    rewardRate: 10,
  },
  accounts: {
    infra: "0x0000000000000000000000000000000000000000",
    network: "0xEb3B4B9A5a2c1d0e9f8a7b6c5d4e3f2a1b0c9d8e",
    l1Reward: "0x2b1c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c",
    infraWei: 0n,
    networkWei: ethWei(1_234.5),
    l1RewardWei: ethWei(12.02),
  },
  batch: { intervalSeconds: 60, calldataBytes: 102_400, gasSpent: 2_100_000, l1BaseFeeWei: 1_100_000_000 },
  ethUsd: { basePrice: 4_200, source: "coingecko" },
};

export const ROBINHOOD_TESTNET: MockNetworkDef = {
  name: "robinhood-testnet",
  displayName: "Robinhood Testnet",
  chainId: 46630,
  explorerUrl: "https://explorer.testnet.chain.robinhood.com",
  model: "legacy",
  historyStart: "2026-07-01T00:00:00Z",
  blockAtHistoryStart: 12_000_000,
  blocksPerSecond: 4,
  minFeeHistory: [
    { at: "2026-04-01T00:00:00Z", wei: 100_000_000n },
    { at: "2026-07-15T10:00:00Z", wei: 10_000_000n },
  ],
  arbosHistory: [{ at: "2026-04-01T00:00:00Z", version: 40 }],
  constraintSets: [],
  legacy: { speedLimit: 7_000_000, inertia: 102, tolerance: 10 },
  ownerActions: [
    {
      block: 16_838_400,
      at: "2026-07-15T10:00:00Z",
      txHash: txHash(300),
      method: "setMinimumL2BaseFee",
      selector: "0xa0188cdb",
      args: { priceInWei: "10000000" },
    },
  ],
  demand: {
    baseline: [
      ["2026-07-01T00:00:00Z", 900_000],
      ["2026-08-01T00:00:00Z", 1_600_000],
      ["2026-09-06T00:00:00Z", 1_800_000],
    ],
    diurnalAmplitude: 0.3,
    noiseAmplitude: 0.15,
    burst: {
      slotSeconds: 300,
      probability: 0.18,
      minDuration: 10,
      maxDuration: 30,
      minIntensity: 20_000_000,
      maxIntensity: 90_000_000,
    },
    seed: 46630,
  },
  l1: {
    baseFeeEstimateWei: 3_100_000,
    surplusWei: 42_000_000_000_000n,
    feesAvailableWei: 310_000_000_000_000n,
    equilibrationUnits: 160_000_000,
    perBatchGasCharge: 210_000,
    rewardRate: 10,
  },
  accounts: {
    infra: "0x1111111111111111111111111111111111111111",
    network: "0x2222222222222222222222222222222222222222",
    l1Reward: "0x3333333333333333333333333333333333333333",
    infraWei: ethWei(3.2),
    networkWei: ethWei(0.84),
    l1RewardWei: ethWei(0.01),
  },
  batch: { intervalSeconds: 45, calldataBytes: 120, gasSpent: 28_500, l1BaseFeeWei: 12_000_000 },
  // No price feed on the testnet: every snapshot carries ethUsd null, and the UI shows ETH.
};

export const MOCK_NETWORKS: readonly MockNetworkDef[] = [ROBINHOOD, ARBITRUM_ONE, ROBINHOOD_TESTNET];

export const MOCK_GWEI = GWEI;
