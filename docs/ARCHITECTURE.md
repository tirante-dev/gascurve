# gascurve architecture

This document is the contract between the Go backend (`cmd/`, `internal/`) and the web app (`web/`). Change it in the same PR as any change to a shape it describes. Background on the chain mechanics is in [SPEC.md](SPEC.md).

## 1. Components

```
public RPC (one per network)
    │  JSON-RPC over HTTPS, batched, paced per network (calls/s budget)
    ▼
collector (cmd/collector)  ──writes──▶  PostgreSQL  ◀──reads──  api (cmd/api)
    │                                      ▲                        │  REST + WebSocket
    └──── NOTIFY gascurve_live ────────────┘                        ▼
                                                           web (Next.js, web/)
```

- The **collector** is the only process that talks to an RPC. One follower goroutine per enabled network.
- The **api** never calls an RPC. It reads Postgres and fans live updates out to WebSocket clients using Postgres `LISTEN gascurve_live`.
- The **web** app never calls an RPC. It uses the REST API for history and the WebSocket for live updates, with polling of `/live` as a fallback.
- **migrate** (`cmd/migrate`) applies schema migrations. Runtime binaries only run migrations when `database.run_migrations: true`.

## 2. Networks

Configured in `config.yaml` (`networks:`), overridable by `NETWORK_<NAME>_*` env vars. Every network is an Arbitrum Nitro chain. The pricer model is discovered at runtime from the precompiles, not configured:

| Network | Chain ID | Model observed 2026-09-06 |
|---|---|---|
| robinhood | 4663 | 2 single-gas constraints |
| robinhood-testnet | 46630 | legacy (no constraints), min fee 0.01 gwei |
| arbitrum-one | 42161 | 6 single-gas constraints (ArbOS 51 "Dia" set) |
| arbitrum-sepolia | 421614 | 6 single-gas constraints |

All four report ArbOS 61 (`ArbSys.arbOSVersion()` = 116).

Per-network optional settings: `calls_per_second` (0 = unlimited, for dedicated nodes; the public defaults stay at 4), `ws_url` (subscribe to `newHeads` and sample state at each head instead of polling), `archive` (historical `eth_call` works, so the backfill anchors replay to real backlogs every `collector.backfill_anchor_interval` blocks). Production runs on dedicated nodes; the public-RPC pacing exists for development and for anyone running the collector without one.

## 3. Pricer model (`internal/pricer`)

Pure functions, no I/O, shared by the collector (replay) and tests. Mirrors `arbos/l2pricing/model.go` in nitro exactly, using integer basis-point math.

```
Bips: int64, OneInBips = 10_000

approxExpBips(x Bips, accuracy=4) Bips:      // nitro ApproxExpBasisPoints
    b = 10_000; res = b + x/accuracy
    for i = accuracy-1 .. 1: res = b + res*x/(i*b)
    return res                                 // = b * (1 + x/b + x²/2b² + x³/6b³ + x⁴/24b⁴)

Constraint{ Target uint64 /*gas/s*/, Window uint64 /*s*/, Backlog uint64 }

Step(state, dt seconds, minBaseFee):
    exponent = 0
    for each constraint c:
        c.Backlog = saturatingSub(c.Backlog, dt*c.Target)
        if c.Backlog > 0: exponent += bips(c.Backlog) / bips(c.Window*c.Target)   // integer division, in bips
    baseFee = exponent > 0 ? minBaseFee * approxExpBips(exponent) / 10_000 : minBaseFee

AddGas(state, gasUsed): for each c: c.Backlog += gasUsed

Legacy (no constraints): speedLimit, inertia, tolerance, backlog
    backlog = saturatingSub(backlog, dt*speedLimit)
    if backlog > tolerance*speedLimit:
        exponent = bips(backlog - tolerance*speedLimit) / (inertia*speedLimit)
        baseFee = minBaseFee * approxExpBips(exponent) / 10_000
    else baseFee = minBaseFee
```

Block processing order (as ArbOS does it): `Step(dt)` at the start of the block, which sets that block's base fee, then `AddGas(gasUsed)` for the block's transactions. `dt` is the header timestamp minus the previous header timestamp (0 for most Nitro blocks).

Replay validation vector (Robinhood, 2026-09-06): constraints `[60e6,15,3_111_506]`, `[40e6,86400,11_194_391_810_886]`, minBaseFee 20_000_000 wei. Exponent 34 + 32_391 = 32_425 bips. Base fee 395_8xx_xxx wei (observed 399_726_000 a few seconds later).

`Exponent` per constraint (`c.Backlog / (c.Window*c.Target)` in bips) is exposed to the API as `exponentBips` so the UI can show each constraint's share.

Replay comparison: ArbOS computes the fee in block N's `startBlock` and it applies to header N+1's `baseFeePerGas`; the replay compares `predicted(N)` with header N, so `replayErrorBips` carries at most one block of lag. The EIP-7623 floor is not applied to batch-report `gasSpent`.

## 4. Database (PostgreSQL 16, migrations in `internal/db/migrations`)

All tables are keyed by `chain_id` first. Wei values are `NUMERIC(40,0)`. Gas values are `BIGINT`.

```sql
networks            (chain_id PK, name, display_name, explorer_url, enabled, head_block, head_at, last_sample_at, last_error, updated_at)
blocks              (chain_id, number, ts, gas_used, base_fee NUMERIC, l1_block, tx_count,
                     backlogs BIGINT[], exponent_bips BIGINT, predicted_base_fee NUMERIC, anchored BOOL,
                     PK(chain_id, number))                       -- rolling, pruned after collector.block_retention
buckets             (chain_id, resolution TEXT /* '1m'|'15m'|'1h' */, bucket_start TIMESTAMPTZ,
                     blocks INT, gas_used BIGINT, fees_wei NUMERIC,
                     base_fee_min/avg/max NUMERIC, exponent_end_bips BIGINT,
                     backlogs_end BIGINT[], backlogs_max BIGINT[], constraint_set_id INT,
                     replay_error_bips BIGINT /* max |predicted-actual| in bips over the bucket */,
                     last_block BIGINT /* highest block folded in; the backfill never overwrites *_end fields written by newer blocks */,
                     PK(chain_id, resolution, bucket_start))     -- kept forever
state_samples       (chain_id, sampled_at, block_number, base_fee, min_base_fee, constraints JSONB,
                     legacy JSONB /* {speedLimit,inertia,tolerance,backlog} or null */,
                     prices JSONB /* getPricesInWei tuple */, l1 JSONB /* pricer getters */,
                     accounts JSONB /* {infra:{address,balance}, network:{...}, l1Reward:{...}} or null */,
                     PK(chain_id, sampled_at))                   -- l1/accounts only on slow ticks; pruned after sample_retention
owner_actions       (chain_id, block_number, tx_hash, log_index, ts, method, selector, args JSONB,
                     PK(chain_id, tx_hash, log_index))
constraint_sets     (id SERIAL PK, chain_id, effective_block, effective_at, constraints JSONB, source TEXT /* 'genesis'|'owner_action'|'observed' */)
batch_reports       (chain_id, block_number, batch_number, batch_ts, poster, calldata_len, calldata_nonzero,
                     extra_gas, l1_base_fee NUMERIC, gas_spent BIGINT, wei_spent NUMERIC, PK(chain_id, block_number))
collector_state     (chain_id, key, value TEXT, updated_at, PK(chain_id, key))   -- checkpoints: head, backfill_cursor, owner_log_cursor, batch_scan_cursor
```

`NOTIFY gascurve_live, '<json>'` is issued by the collector after each tick with the `LiveSnapshot` below (payloads stay under 8 kB; `recentBlocks` is not included in the notify payload, the API keeps its own ring buffer from `blocks` rows).

`NOTIFY gascurve_owner_action, '{"chainId": …, "action": OwnerAction}'` is issued when the slow loop stores a new owner action; the API turns it into the WebSocket `owner_action` message.

## 5. Collector (`internal/collector`)

One `Follower` per network, all sharing one `*sqlx.DB`.

Fast loop, every `collector.tick_interval` (1 s), or on every `newHeads` event when `ws_url` is configured (then state calls are made at that head's block number so samples align exactly with headers):

1. One JSON-RPC batch: `eth_getBlockByNumber("latest", false)`, `getGasPricingConstraints()`, `getPricesInWei()`, `getMinimumGasPrice()`. If the constraints call reverts or returns empty, also `getGasBacklog()`, `getPricingInertia()`, `getGasBacklogTolerance()`, `getGasAccountingParams()` (legacy model).
2. Fetch headers for every block between the stored head and the new head, in batches of `header_batch_size`, respecting the per-network budget. On a paced network (`calls_per_second > 0`) a gap larger than 10 × `header_batch_size` blocks is skipped (logged) and the replay restarts from the sampled backlogs, because a 4 calls/s budget cannot follow a ~10 blocks/s chain block by block; unlimited (dedicated node) networks always fetch every block.
3. Replay each block through the pricer. When the sampled state's block number equals a replayed block, overwrite the backlogs with the sampled values (`anchored = true`) so drift never accumulates.
4. Upsert `blocks`, fold into `buckets` (1m, 15m, 1h), insert `state_samples`, update `networks.head_block`, `NOTIFY`.

Slow loop, every `collector.slow_interval` (60 s): L1 pricer getters (`getL1BaseFeeEstimate`, `getL1PricingSurplus`, `getL1FeesAvailable`, `getL1PricingUnitsSinceUpdate`, `getLastL1PricingUpdateTime`, `getL1PricingEquilibrationUnits`, `getPerBatchGasCharge`, `getL1RewardRate`), fee-account balances (`ArbOwnerPublic.getInfraFeeAccount/getNetworkFeeAccount`, `ArbGasInfo.getL1RewardRecipient`, `eth_getBalance`), `eth_getLogs` on `0x…70` for new `OwnerActs` since the cursor, batch-report scan of new 2-transaction blocks, pruning.

Backfill job (resumable, checkpoint in `collector_state`): walks backwards from the first stored block to `collector.backfill_depth` fetching headers, replaying from the nearest earlier `constraint_sets` row (starting backlogs from the owner action), and writing buckets only. Runs at low priority inside the same budget (it yields whenever the fast loop needs calls). On `archive: true` networks it re-anchors backlogs from historical state every `backfill_anchor_interval` blocks and records the replay error observed just before each anchor.

Rate limiting: a token bucket per network, `calls_per_second` tokens/s, burst 2× that. Every JSON-RPC call consumes one token, including each item inside a batch. Batches never exceed 100 items and are never sent concurrently for the same network. HTTP 429 or JSON-RPC error code 429: exponential back-off starting at 2 s, capped at 60 s, logged with the calls made in the last 10 s. All requests send `User-Agent: gascurve/<version>`.

## 6. API (`internal/api`, chi, prefix `/api/v1`)

Conventions: JSON, `Cache-Control` set per endpoint, CORS from `server.cors_origins`, rate limited per IP, request ID header. Wei values are decimal strings. Gas, bips, block numbers and unix timestamps are JSON numbers. Timestamps named `*At` are RFC 3339 UTC. Errors: `{ "error": { "code": "not_found", "message": "..." } }`.

`{network}` accepts the name (`robinhood`) or chain id (`4663`).

| Method and path | Purpose |
|---|---|
| `GET /health`, `GET /ready` | liveness; readiness checks DB |
| `GET /networks` | `Network[]` |
| `GET /networks/{network}` | `Network` |
| `GET /networks/{network}/live` | `LiveSnapshot` (same as WS `tick`), `Cache-Control: no-store` |
| `GET /networks/{network}/blocks?limit=120` | `BlockPoint[]` newest first, max 1000 |
| `GET /networks/{network}/series?range=1h\|24h\|30d\|all` | `Series` (see below) |
| `GET /networks/{network}/constraints` | `{ current: ConstraintSet \| null, history: ConstraintSet[] }` (`null` on legacy chains or before any set is known) |
| `GET /networks/{network}/owner-actions` | `OwnerAction[]` newest first |
| `GET /networks/{network}/batches?range=…` | `BatchSeries` (L1 cost per bucket, batch cadence) |
| `GET /networks/{network}/l1?range=…` | `L1Series` (pricer getters over time, from state samples) |
| `GET /status` | `{ version, networks: [{ name, chainId, enabled, headBlock, headAt, lagSeconds, lastSampleAt, lastError, rateLimitEvents, last429At, backfillCursor, arbosVersion }] }` |
| `GET /ws?network=…` | WebSocket, see §7 |

Range to resolution: `1h` → per block from `blocks` (a `step` of 5 s is applied server-side if more than 2000 points), `24h` → `1m` buckets, `30d` → `15m`, `all` → `1h`. `/live` returns 404 until the collector has produced a sample. Arrays are never `null` in responses. `L1Series` reaches back at most `collector.sample_retention`.

```ts
type Network = {
  name: string; displayName: string; chainId: number; explorerUrl: string;
  model: 'constraints' | 'legacy' | 'unknown';
  headBlock: number; headAt: string; lagSeconds: number; enabled: boolean;
}

type Constraint = { target: number; window: number; backlog: number; exponentBips: number }

type ConstraintSet = {
  id: number; effectiveBlock: number; effectiveAt: string; source: 'genesis' | 'owner_action' | 'observed';
  constraints: { target: number; window: number; startingBacklog: number }[];
}

type LiveSnapshot = {
  chainId: number; sampledAt: string;
  block: { number: number; ts: number; gasUsed: number; baseFee: string; txCount: number };
  baseFee: string; minBaseFee: string; multiplierBips: number; exponentBips: number;
  model: 'constraints' | 'legacy';
  constraints: Constraint[];               // empty for legacy
  legacy?: { speedLimit: number; inertia: number; tolerance: number; backlog: number };
  prices: { perL2Tx: string; perL1CalldataByte: string; perL2Storage: string;
            perArbGasBase: string; perArbGasCongestion: string; perArbGasTotal: string };
  gasPerSecond: { s10: number; s60: number };
  l1?: { baseFeeEstimate: string; surplus: string; feesAvailable: string; unitsSinceUpdate: number;
         lastUpdateAt: string; equilibrationUnits: number; perBatchGasCharge: number; rewardRate: number };
  accounts?: { infra: Account; network: Account; l1Reward: Account };
  replayErrorBips: number;                 // |predicted - actual| for the latest block
}
type Account = { address: string; balance: string }

type BlockPoint = {
  number: number; ts: number; gasUsed: number; baseFee: string; predictedBaseFee: string;
  backlogs: number[]; exponentBips: number; anchored: boolean;
}

type Series = {
  range: '1h' | '24h' | '30d' | 'all'; resolution: 'block' | '5s' | '1m' | '15m' | '1h';
  constraintSets: ConstraintSet[];         // sets that were in force during the range, for markers and card layout
  ownerActions: OwnerAction[];             // within the range, for chart markers
  points: SeriesPoint[];
}
type SeriesPoint = {
  t: number;                               // unix seconds, bucket start
  blocks: number; gasUsed: number; gasPerSecond: number; feesWei: string;
  baseFeeMin: string; baseFeeAvg: string; baseFeeMax: string;
  exponentBips: number; backlogs: number[]; backlogsMax: number[];
  constraintSetId: number; replayErrorBips: number;
}

type OwnerAction = {
  block: number; at: string; txHash: string; method: string; selector: string;
  args: Record<string, unknown>;           // decoded when the selector is known, else { raw: '0x…' }
}

type BatchSeries = { range: string; resolution: 'batch' | '1m' | '15m' | '1h'; /* 'batch' = one point per report, used for 1h */ points: { t: number; batches: number; gasSpent: number; weiSpent: string; l1BaseFeeAvg: string; calldataBytes: number }[] }
type L1Series = { range: string; points: { t: number; baseFeeEstimate: string; surplus: string; feesAvailable: string; unitsSinceUpdate: number }[] }
```

## 7. WebSocket (`/api/v1/ws?network=robinhood`)

Server to client, one JSON object per message:

```ts
{ type: 'hello', data: { network: Network; snapshot: LiveSnapshot | null; recentBlocks: BlockPoint[] } }   // on connect; snapshot null until the collector has sampled
{ type: 'error', error: { code: string; message: string } }   // e.g. unknown subscribe target; the socket stays open
{ type: 'tick',  data: LiveSnapshot }                    // every collector tick, ~1/s
{ type: 'blocks', data: BlockPoint[] }                   // new blocks since the previous message, oldest first
{ type: 'owner_action', data: OwnerAction }              // when the collector sees a new one
{ type: 'ping' }                                         // every 30 s; client replies { type: 'pong' }
```

Client to server: `{ type: 'pong' }` and `{ type: 'subscribe', network: string }` to switch networks on the same socket. The server closes idle sockets that miss two pings. The web client reconnects with exponential back-off (1 s to 30 s) and falls back to polling `/live` every 2 s while disconnected.

## 8. Web (`web/`, Next.js App Router, TypeScript strict, Tailwind, Recharts, Vitest)

```
src/app/                 layout, page (redirect to default network), [network]/page
src/components/          LiveStrip, ConstraintCards, PricerEquation, Explainer, HistoryTabs, SeriesCharts,
                         FeeFlows, L1Section, OwnerActionTimeline, NetworkSwitcher, DataFooter
src/hooks/               useLive (WS + fallback), useSeries, useNetwork
src/lib/api/             core.ts (fetch with timeout and retry), networks.ts, series.ts, live.ts, ws.ts
src/lib/pricer.ts        approxExpBips and helpers in TS, unit-tested against the same vectors as Go
src/types/               the shapes above
src/utils/               formatting (gwei, gas, durations), bips math
```

Environment: `NEXT_PUBLIC_API_URL` (default `http://localhost:8080/api/v1`), `NEXT_PUBLIC_WS_URL` (derived from the API URL when unset), `NEXT_PUBLIC_SITE_URL`.

Coverage gate (90% lines) applies to `src/lib/**`, `src/hooks/**`, `src/utils/**`. Components are tested where behaviour is non-trivial.

## 9. Versioning and images

One version for the whole repo, managed by release-please from the Go component; `web/package.json` is bumped as an extra file. Three images: `gascurve-collector`, `gascurve-api`, `gascurve-web`.
