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

Per-network optional settings: `calls_per_second` (0 = unlimited, otherwise at least 0.1, for dedicated nodes; the public defaults stay at 4), `ws_url` (subscribe to `newHeads` and sample state at each head instead of polling), `archive` (historical `eth_call` works, so the backfill anchors replay to real backlogs every `collector.backfill_anchor_interval` blocks). Production runs on dedicated nodes; the public-RPC pacing exists for development and for anyone running the collector without one.

### Multiple endpoints per network

A network is a list of endpoints: the primary (`rpc_url`, `ws_url`, `archive`, `calls_per_second`) and `fallbacks`, each with the same four fields. Env overrides: `NETWORK_<NAME>_FALLBACK_RPC_URLS` and `NETWORK_<NAME>_FALLBACK_WS_URLS` (comma-separated, positional), `NETWORK_<NAME>_FALLBACK_ARCHIVE` and `NETWORK_<NAME>_FALLBACK_CALLS_PER_SECOND` (comma-separated, positional, optional). Endpoint URLs carrying keys never go in `config.yaml`; they come from the environment (a Secret in the chart's `extraEnv`).

Routing rules:

- **Ordinary calls** (fast tick, catch-up headers, logs, batch-report scan) go to the primary. After a failed request (transport error, HTTP 5xx, 429 past its back-off, or an `eth_chainId` mismatch) the network fails over to the next endpoint for `collector.failover_cooldown` (default 60 s), then probes the primary again with a single cheap call before returning to it. Failovers are counted in `/status` (`failovers`, `activeEndpoint`).
- **Capability calls** are routed by capability, not by order: the `newHeads` subscription uses the first endpoint that has a `ws_url`; archive anchoring uses the first endpoint with `archive: true`. The public Robinhood RPC has neither, so with a QuickNode fallback the collector polls the public RPC for ordinary work, follows heads over QuickNode's WebSocket, and anchors the backfill against QuickNode's archive state.
- **Per-endpoint pacing and batch size.** Every endpoint has its own token bucket and its own adaptive batch cap: it starts at `header_batch_size` and halves (floor 10) whenever that endpoint answers a batch with 429, recovering by one step per successful minute. QuickNode rejects 100-item batches and accepts 50; the public RPC accepts 100.
- Every endpoint must report the configured `chain_id`; one that does not is disabled with an error in `/status`.

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
blocks              (chain_id, number, hash, parent_hash, ts, gas_used, base_fee NUMERIC, l1_block, tx_count, pricing_version SMALLINT /* 1 = full breakdown, 0 = unknown history */,
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
collector_state     (chain_id, key, value TEXT, updated_at, PK(chain_id, key))   -- checkpoints: head, live_start, holes (`[{from,to,at,next,reason}]`: `next` is the gap filler's progress cursor, `reason` is set only on a range nothing can be replayed into), backfill_cursor (with top), owner_log_cursor, owner_scan_through, owner_scan_origin, batch_scan_cursor, endpoints, generation (bumped by every rewind; writers that fetched outside the lock re-check it before committing), eth_usd
```

`NOTIFY gascurve_live, '<json>'` is issued by the collector after each tick with the `LiveSnapshot` below (payloads stay under 8 kB; `recentBlocks` is not included in the notify payload, the API keeps its own ring buffer from `blocks` rows).

`NOTIFY gascurve_owner_action, '{"chainId": …, "action": OwnerAction}'` is issued when the slow loop stores a new owner action; the API turns it into the WebSocket `owner_action` message.

## 5. Collector (`internal/collector`)

One `Follower` per network, all sharing one `*sqlx.DB`. At startup each follower calls `eth_chainId` and refuses to run (logs and marks `networks.last_error`) if it differs from the configured `chain_id`.

Correctness rules the follower must keep:

- **Replay only forward from a known state.** On a fresh database persist the sampled head only and replay the blocks after it; never seed earlier blocks from a later sample. Catch-up ranges are split at owner-action boundaries. After a skipped gap, persist the sampled head only and record the hole in `collector_state`.
- **A skipped range is queued work, not lost history.** A hole whose preceding block is stored with the full pricing state is filled later by the history loop, newest first. Only a range with nothing to replay from (before the owner-scan origin without an archive endpoint, or below the backfill depth with no independently known state) is recorded as permanently un-indexed, with `reason: "no state"`, and is re-examined only when a state appears before it.
- **Nothing advances in memory before the commit.** The pricer state is cloned for the replay; head, previous timestamp, state and last result are published only after the transaction commits. After a commit error the follower reloads head and state from the database (the last stored block row carries the backlogs) so a retry is idempotent.
- **Bucket folds are idempotent.** The fold and the head checkpoint commit in one transaction, and a bucket's additive fields are recomputed from the block rows inside the fold window when those rows exist; buckets store `base_fee_sum` (exact) rather than a running average.
- **Reorgs are detected.** Blocks store `hash` and `parent_hash`; a head whose parent hash does not match the stored head triggers a rewind to the common ancestor (blocks, bucket contributions, owner-action and batch cursors) in one transaction.
- **Capability endpoints are verified before use.** Archive calls go through a managed archive pool with its own failover; the `newHeads` subscriber re-resolves its endpoint at every connection attempt. A range with no independently known pricer state is recorded as a hole, never replayed from the live model.
- **Minimum base fee history.** The fee in force at a block comes from recorded `setMinimumL2BaseFee` actions, with nitro's genesis default (0.1 gwei, `InitialMinimumBaseFeeWei`) before the first recorded change, never the live value.

Fast loop, every `collector.tick_interval` (3 s by default: a sample costs five calls, so at a public budget of four per second a 1 s tick starved the header catch-up and skipped gaps), or on every `newHeads` event when `ws_url` is configured (then state calls are made at that head's block number so samples align exactly with headers):

With `ws_url` the follower starts on the timer, pauses timer sampling once the `newHeads` subscription is up, and resumes it the moment the subscription drops (the subscriber reconnects on its own with 1 s to 30 s back-off and a 15 s ping watchdog). Heads arriving mid-tick collapse into one tick at the newest head; catch-up fetches the rest.

1. Resolve the head number first (`eth_blockNumber`), then one JSON-RPC batch at that exact block tag: `eth_getBlockByNumber(n, false)`, `getGasPricingConstraints()`, `getPricesInWei()`, `getMinimumGasPrice()`, so a sample never mixes two blocks. If the constraints call reverts or returns empty, also `getGasBacklog()`, `getPricingInertia()`, `getGasBacklogTolerance()`, `getGasAccountingParams()` (legacy model).
2. Fetch headers for every block between the stored head and the new head, in batches of `header_batch_size`, respecting the per-network budget. On a paced network (`calls_per_second > 0`) a gap larger than `max_catch_up_batches` × `header_batch_size` blocks is skipped (logged) and the replay restarts from the sampled backlogs, because a 4 calls/s budget cannot follow a ~10 blocks/s chain block by block; unlimited (dedicated node) networks always fetch every block.
3. Replay each block through the pricer. When the sampled state's block number equals a replayed block, overwrite the backlogs with the sampled values (`anchored = true`) so drift never accumulates.
4. Upsert `blocks`, fold into `buckets` (1m, 15m, 1h), insert `state_samples`, update `networks.head_block`, `NOTIFY`.

Slow loop, every `collector.slow_interval` (60 s): the ETH/USD spot from `collector.eth_usd_source` (default Coinbase's public spot endpoint, no key; disable with an empty value), stored in `collector_state` and included in every snapshot until it is older than `collector.eth_usd_max_age` (default 10 m); L1 pricer getters (`getL1BaseFeeEstimate`, `getL1PricingSurplus`, `getL1FeesAvailable`, `getL1PricingUnitsSinceUpdate`, `getLastL1PricingUpdateTime`, `getL1PricingEquilibrationUnits`, `getPerBatchGasCharge`, `getL1RewardRate`), fee-account balances (`ArbOwnerPublic.getInfraFeeAccount/getNetworkFeeAccount`, `ArbGasInfo.getL1RewardRecipient`, `eth_getBalance`), `eth_getLogs` on `0x…70` for new `OwnerActs` since the cursor, batch-report scan of new 2-transaction blocks, pruning.

History loop (one goroutine, the gap filler first and the backfill only when nothing is fillable, so recent charts complete before deep history grows):

Gap filler (resumable, the cursor lives on the hole entry): takes the newest fillable hole, fetches its headers in `header_batch_size` batches through the Bulk lane (sized to the spare budget exactly like the backfill, never below `minBackfillBatch`, never concurrently for one network), verifies that the range links by parent hash to the stored block before it and to the stored block after it, replays the pricer forward from the stored end-of-block state of `from - 1` splitting at owner-action boundaries the way the catch-up does, and writes the blocks and their buckets exactly as the live path does (rows and rebuilt buckets from the hour of the first live block on, additive folds below it). The stored block at `to + 1` carries the real sampled backlogs: the replay ends on it, is anchored to them, and the prediction it produces is written back onto that row, so the error of the whole reconstruction is recorded there. Each commit is one chain transaction with the `generation` re-checked, moves the hole's `next` cursor and removes the entry when the range is complete; the filler yields whenever the fast loop is catching up. A rewind that reaches into what a filler wrote resets that hole's cursor to the start of its range. On a 4 calls/s public endpoint the filler only gets what the fast tick and the slow loop leave spare, so filling is best effort and deliberately newest first.

Backfill job (resumable, checkpoint in `collector_state`): walks backwards from the first stored block to `collector.backfill_depth` fetching headers (the owner-action scan starts at the same depth plus a ten-minute margin, sampling the pricing state there from an archive endpoint when one exists, so a shallow development depth never re-indexes a whole chain), replaying from the nearest earlier `constraint_sets` row (starting backlogs from the owner action), and writing buckets only. Runs at low priority inside the same budget (it yields whenever the fast loop needs calls). On `archive: true` networks it re-anchors backlogs from historical state every `backfill_anchor_interval` blocks and records the replay error observed just before each anchor.

Rate limiting: a token bucket per endpoint, `calls_per_second` tokens/s, burst 2× that. Every JSON-RPC call consumes one token, including each item inside a batch. Batches never exceed 100 items, never exceed the endpoint's pacer burst minus the fast reserve on a budgeted endpoint, and are never sent concurrently for the same network. The pacer never spends tokens the bucket does not hold and has two lanes: the head or timer tick's state sample is `Fast` and draws from a reserve of `max(1, rate/4)` tokens per second without queueing; whatever a fast caller needs beyond the reserve queues first come, first served with `Bulk` (everything else), so a demanding tick on a small budget can never starve the slow loop or the backfill. `tick_interval` can be set per network (`NETWORK_<NAME>_TICK_INTERVAL`); dedicated endpoints run at 500 ms, public ones stay at 3 s. Rate limiting is recognised from HTTP 429 and from JSON-RPC errors with codes -32005 or -32007 or a message naming a rate or request limit (QuickNode reports its 50 requests per second as `-32007`); all of these back off with the same exponential schedule (2 s to 60 s), share the endpoint's cooldown, halve the endpoint's batch cap, and drive failover when persistent. Because the burst is twice the rate, a provider with a strict per-second window is configured at half that window (QuickNode: `calls_per_second: 25`), otherwise the one-second peaks trip it and every back-off stalls the history work. All requests send `User-Agent: gascurve/<version>`.

## 6. API (`internal/api`, chi, prefix `/api/v1`)

Conventions: JSON, `Cache-Control` set per endpoint, CORS from `server.cors_origins`, rate limited per IP (the client IP is taken from `X-Forwarded-For` only when the peer is in `server.trusted_proxies`, a list of CIDRs, walking the chain right to left past trusted hops), request ID header. WebSocket connections are capped per IP and globally (`server.ws_max_per_ip`, `server.ws_max_total`) and messages are rate limited per connection. Wei values are decimal strings. Gas, bips, block numbers and unix timestamps are JSON numbers. Timestamps named `*At` are RFC 3339 UTC. Errors: `{ "error": { "code": "not_found", "message": "..." } }`.

`{network}` accepts the name (`robinhood`) or chain id (`4663`).

| Method and path | Purpose |
|---|---|
| `GET /health`, `GET /ready` | liveness; readiness checks DB |
| `GET /networks` | `Network[]` |
| `GET /networks/{network}` | `Network` |
| `GET /networks/{network}/live` | `LiveSnapshot` (same as WS `tick`), `Cache-Control: no-store` |
| `GET /networks/{network}/blocks?limit=120` | `BlockPoint[]` newest first, max 1000 |
| `GET /networks/{network}/series?range=1h\|24h\|30d\|all` | `Series` (see below) |
| `GET /networks/{network}/constraints` | `{ current: ConstraintSet \| null, history: ConstraintSet[] }` (`null` whenever the latest state sample's model is not `constraints`, or before any set is known; `history` is still returned) |
| `GET /networks/{network}/owner-actions` | `OwnerAction[]` newest first |
| `GET /networks/{network}/batches?range=…` | `BatchSeries` (L1 cost per bucket, batch cadence) |
| `GET /networks/{network}/l1?range=…` | `L1Series` (pricer getters over time, from state samples) |
| `GET /status` | `{ version, networks: [{ name, chainId, enabled, headBlock, headAt, lagSeconds, lastSampleAt, lastError, rateLimitEvents, last429At, backfillCursor, arbosVersion, holes: { pending, blocks, unfillable }, activeEndpoint, failovers, endpoints: [{ index, ws, archive, disabled, error: string | null }] }] }` (`headAt`, `lagSeconds`, `lastSampleAt`, `last429At`, `backfillCursor`, `arbosVersion` are nullable; `endpoints` is `[]` when unknown; endpoint URLs are never exposed. `holes.pending` counts the ranges queued for the gap filler, `holes.unfillable` the ones nothing can be replayed into, and `holes.blocks` how many blocks are still not indexed across both) |
| `GET /ws?network=…` | WebSocket, see §7 |

Range to resolution: `1h` → per block from `blocks` (a `step` of 5 s is applied server-side if more than 2000 points), `24h` → `1m` buckets, `30d` → `15m`, `all` → `1h`. `/live` returns 404 until the collector has produced a sample. Arrays are never `null` in responses. `L1Series` reaches back at most `collector.sample_retention`.

```ts
type Network = {
  name: string; displayName: string; chainId: number; explorerUrl: string;
  model: 'constraints' | 'legacy' | 'unknown';
  headBlock: number; headAt: string | null; lagSeconds: number | null; enabled: boolean;   // null until the collector has produced a head
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
  ethUsd: { price: string; at: string; source: string } | null;   // ETH/USD spot fetched by the collector's slow loop (server side, never the browser), null when unavailable or stale
}
type Account = { address: string; balance: string }

type BlockPoint = {
  number: number; ts: number; gasUsed: number; baseFee: string; predictedBaseFee: string;
  backlogs: number[];        // end-of-block backlogs (after AddGas)
  constraintBips: number[] | null;  // start-of-block per-constraint exponent, the values that priced this block; sums to exponentBips; null only for rows written before migration 000006
  exponentBips: number; minBaseFee: string | null; anchored: boolean;   // minBaseFee null for pricing version 0 (history without the breakdown)
}

type Series = {
  range: '1h' | '24h' | '30d' | 'all'; resolution: 'block' | '5s' | '1m' | '15m' | '1h';
  from: number; to: number;   // the requested window in unix seconds, whatever the points cover; charts draw the whole window and show what is missing as not indexed. For 'all', from is the first indexed point (equal to to when nothing is indexed)
  constraintSets: ConstraintSet[];         // sets that were in force during the range, for markers and card layout
  ownerActions: OwnerAction[];             // within the range, for chart markers
  points: SeriesPoint[];
}
type SeriesPoint = {
  t: number;                               // unix seconds, bucket start
  blocks: number; gasUsed: number; gasPerSecond: number; feesWei: string;
  coverage: number;                        // share of the bucket the collector indexed (1 = whole); gasPerSecond is the rate over that covered span, so the bucket in progress and the first one after the collector started read as rates, not as fractions of a bucket. Sums (gasUsed, feesWei, blocks) are over the covered span only
  baseFeeMin: string; baseFeeAvg: string; baseFeeMax: string;
  exponentBips: number; constraintBips: number[] | null;   // start-of-block values of the bucket's last block; null for pre-000006 history
  backlogs: number[]; backlogsMax: number[];
  minBaseFee: string | null;                        // floor in force at the bucket's last block; null when any block in the bucket has pricing version 0
  floorFeesWei: string | null; surplusFeesWei: string | null;     // Σ gasUsed × minBaseFee and feesWei minus that, per block, exact; null when any block in the bucket has pricing version 0
  constraintSetId: number; replayErrorBips: number;
}

type OwnerAction = {
  block: number; at: string; txHash: string; method: string; selector: string;
  args: Record<string, unknown>;           // decoded when the selector is known, else { raw: '0x…' }. setGasPricingConstraints: { constraints: [{ gasTargetPerSecond, adjustmentWindowSeconds, startingBacklog }] }; setMinimumL2BaseFee: { priceInWei: string }
}

type BatchSeries = { range: string; resolution: 'batch' | '1m' | '15m' | '1h'; from: number; to: number; /* window as in Series */ /* 'batch' = exactly one point per report, never grouped, used for 1h */ points: { t: number; batches: number; gasSpent: number; weiSpent: string; l1BaseFeeAvg: string; calldataBytes: number }[] }
type L1Series = { range: string; from: number; to: number; /* window as in Series */ points: { t: number; baseFeeEstimate: string; surplus: string; feesAvailable: string; unitsSinceUpdate: number }[] }
```

## 7. WebSocket (`/api/v1/ws?network=robinhood`)

Server to client, one JSON object per message:

```ts
{ type: 'hello', data: { network: Network; snapshot: LiveSnapshot | null; recentBlocks: BlockPoint[] } }   // on connect; snapshot null until the collector has sampled; recentBlocks is the newest 1200 blocks (two minutes of a ten blocks per second chain, the hero chart window)
{ type: 'error', error: { code: string; message: string } }   // e.g. unknown subscribe target; the socket stays open
{ type: 'reorg', data: { chainId: number; ancestor: number; blocks: BlockPoint[] } }   // sent before the next tick: drop every block above ancestor, append blocks (canonical, oldest first)
{ type: 'tick',  data: LiveSnapshot }                    // every collector tick, ~1/s
{ type: 'blocks', data: BlockPoint[] }                   // new blocks since the previous message, oldest first
{ type: 'owner_action', data: OwnerAction }              // when the collector sees a new one
{ type: 'ping' }                                         // every 30 s; client replies { type: 'pong' }
```

Client to server: `{ type: 'pong' }` and `{ type: 'subscribe', network: string }` to switch networks on the same socket. The server closes idle sockets that miss two pings. The web client reconnects with exponential back-off (1 s to 30 s) and falls back to polling `/live` every 2 s while disconnected.

## 8. Web (`web/`, Next.js App Router, TypeScript strict, Tailwind, Recharts, Vitest)

```
src/app/                 layout, page (redirect to default network), [network]/page, [network]/how-it-works/page,
                         [network]/charts/[chart]/page (one chart enlarged; ?range= and ?constraint= deep-link the view)
src/components/          PageHeader, LiveHero (live figures plus the base fee chart with a range control: Live 2 min from the block ring,
                         or 1h/24h/30d/all from /series), ConstraintCards, PricerEquation, HowItWorks (Explainer + TaylorChart), HistoryTabs,
                         SeriesCharts (constraint-level history), FeeFlows, L1Section, OwnerActionTimeline, NetworkSwitcher, DataFooter
src/hooks/               useLive (WS + fallback), useSmoothedLive (250 ms render cadence, tweened values, one rAF loop), useSeries, useNetwork
src/lib/api/             core.ts (fetch with timeout and retry), networks.ts, series.ts, live.ts, ws.ts
src/lib/pricer.ts        approxExpBips and helpers in TS, unit-tested against the same vectors as Go
src/types/               the shapes above
src/utils/               formatting (gwei, gas, durations), bips math
```

Every chart card carries an enlarge control linking to `/{network}/charts/{chart}`, where `chart` is one of the registry ids in `src/lib/chartViews.ts`: `base-fee`, `backlog-sawtooth`, `contribution`, `gas-per-second`, `backlogs`, `fee-flows`, `l1`, `taylor`. The enlarged page draws the same component with the same hooks at a taller frame, keeps the range in `?range=` and the constraint slot in `?constraint=`, and offers tabs across every chart plus a link back to the section it came from.

Units in copy: gas carries an SI prefix on the unit, never on the number (`11.2 Tgas`, `60 Mgas/s`, `812,345 gas` below one million). Figures that animate use fixed decimal counts per band so neighbouring elements never shift. USD figures (from `ethUsd`) are shown by default with the ETH amount on hover.

Environment: `NEXT_PUBLIC_API_URL` (default `http://localhost:8080/api/v1`), `NEXT_PUBLIC_WS_URL` (derived from the API URL when unset), `NEXT_PUBLIC_SITE_URL`.

Coverage gate (90% lines) applies to `src/lib/**`, `src/hooks/**`, `src/utils/**`. Components are tested where behaviour is non-trivial.

## 9. Versioning and images

One version for the whole repo, managed by release-please from the Go component; `web/package.json` is bumped as an extra file. Three images: `gascurve-collector`, `gascurve-api`, `gascurve-web`.
