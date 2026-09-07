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

- The **collector** is the only process that talks to an RPC. One follower goroutine per enabled network. Its private observability server listens on `collector.metrics_port` (default 9090) and serves `/startup`, `/health`, `/ready` and `/metrics`.
- The **api** never calls an RPC. It reads Postgres and fans live updates out to WebSocket clients using Postgres `LISTEN gascurve_live`.
- The **web** app never calls an RPC. It uses the REST API for history and the WebSocket for live updates, with polling of `/live` as a fallback.
- **migrate** (`cmd/migrate`) applies schema migrations. Runtime binaries only run migrations when `database.run_migrations: true`.
- Both runtime binaries expose Prometheus metrics at `/metrics` (§9). The api serves them on `server.port` next to the REST API; the collector serves them on `collector.metrics_port` next to its health routes.

## 2. Networks

Configured in `config.yaml` (`networks:`), overridable by `NETWORK_<NAME>_*` env vars. Every network is an Arbitrum Nitro chain. The pricer model is discovered at runtime from the precompiles, not configured:

| Network | Chain ID | Model observed 2026-09-06 |
|---|---|---|
| robinhood | 4663 | 2 single-gas constraints |
| robinhood-testnet | 46630 | legacy (no constraints), min fee 0.01 gwei |
| arbitrum-one | 42161 | 6 single-gas constraints (ArbOS 51 "Dia" set) |
| arbitrum-sepolia | 421614 | 6 single-gas constraints |

All four report ArbOS 61 (`ArbSys.arbOSVersion()` = 116).

Per-network optional settings: `calls_per_second` (0 = unlimited, otherwise at least 0.1, for dedicated nodes; the public defaults stay at 4), `ws_url` (subscribe to `newHeads` and sample state at each head instead of polling), `archive` (historical `eth_call` works, so the backfill anchors replay to real backlogs every `collector.backfill_anchor_interval` blocks), `history_epoch` (raise it to rebuild the reconstructed history once, see below). Production runs on dedicated nodes; the public-RPC pacing exists for development and for anyone running the collector without one.

### Multiple endpoints per network

A network is a list of endpoints: the primary (`rpc_url`, `ws_url`, `archive`, `calls_per_second`) and `fallbacks`, each with the same four fields. Env overrides: `NETWORK_<NAME>_FALLBACK_RPC_URLS` and `NETWORK_<NAME>_FALLBACK_WS_URLS` (comma-separated, positional), `NETWORK_<NAME>_FALLBACK_ARCHIVE` and `NETWORK_<NAME>_FALLBACK_CALLS_PER_SECOND` (comma-separated, positional, optional). Endpoint URLs carrying keys never go in `config.yaml`; they come from the environment (a Secret in the chart's `extraEnv`). `rpc_url` must be `http://` or `https://` and `ws_url` `ws://` or `wss://`, checked at startup; a rejection names the setting and never quotes the value, because the value is a credential. For the same reason no URL ever reaches a log line, `networks.last_error` or `/status`: every error that could carry one (a transport or WebSocket dial failure, a provider body or JSON-RPC message quoting the request) names the endpoint by index instead, with userinfo, path secrets and query strings stripped.

Routing rules:

- **Ordinary calls** (fast tick, catch-up headers, logs, batch-report scan) go to the primary. After a failed request (transport error, HTTP 5xx, HTTP 401 or 403, 429 past its back-off, an `eth_chainId` mismatch, or a refusal to serve a single block of logs) the network fails over to the next endpoint for `collector.failover_cooldown` (default 60 s), then probes the primary again with a single cheap call before returning to it. Failovers are counted in `/status` (`failovers`, `activeEndpoint`).
- **Capability calls** are routed by capability, not by order: the `newHeads` subscription uses the first endpoint that has a `ws_url`; archive anchoring uses the first endpoint with `archive: true`. The public Robinhood RPC has neither, so with a QuickNode fallback the collector polls the public RPC for ordinary work, follows heads over QuickNode's WebSocket, and anchors the backfill against QuickNode's archive state.
- **Per-endpoint pacing and batch size.** Every endpoint has its own token bucket and its own adaptive batch cap: it starts at `header_batch_size` and halves (floor 10) whenever that endpoint answers a batch with 429, recovering by one step per successful minute. QuickNode rejects 100-item batches and accepts 50; the public RPC accepts 100.
- **Policy follows the endpoint that serves the work.** Whether a catch-up gap is skipped and whether the batch-report scan reads every block or only two-transaction ones is the active endpoint's call policy, taken with the endpoint generation (its index and the failover count). A failover partway through re-decides against the endpoint that will serve the rest: an unlimited catch-up that lands on a paced fallback skips and queues the remaining gap instead of spending the public budget on it, and a scan that started unlimited goes back to the prefilter.
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

AddGas(state, computeGas): for each c: c.Backlog += computeGas

Legacy (no constraints): speedLimit, inertia, tolerance, backlog
    backlog = saturatingSub(backlog, dt*speedLimit)
    if backlog > tolerance*speedLimit:
        exponent = bips(backlog - tolerance*speedLimit) / (inertia*speedLimit)
        baseFee = minBaseFee * approxExpBips(exponent) / 10_000
    else baseFee = minBaseFee
```

Block processing order (as ArbOS does it): `Step(dt)` at the start of the block, which sets that block's base fee, then `AddGas(gasUsed - posterGas)` for the block's transactions. `posterGas` is the sum of receipt `gasUsedForL1`. `dt` is the header timestamp minus the previous header timestamp (0 for most Nitro blocks).

Replay validation vector (Robinhood, 2026-09-06): constraints `[60e6,15,3_111_506]`, `[40e6,86400,11_194_391_810_886]`, minBaseFee 20_000_000 wei. Exponent 34 + 32_391 = 32_425 bips. Base fee 395_8xx_xxx wei (observed 399_726_000 a few seconds later).

`Exponent` per constraint (`c.Backlog / (c.Window*c.Target)` in bips) is exposed to the API as `exponentBips` so the UI can show each constraint's share.

Replay comparison: ArbOS computes the fee in block N's `startBlock` and it applies to header N+1's `baseFeePerGas`; the replay compares `predicted(N)` with header N, so `replayErrorBips` carries at most one block of lag. Batch-report `gasSpent` is separate and reproduces Nitro's report-version path, including the effective per-batch charge and the ArbOS 50+ parent calldata floor.

## 4. Database (PostgreSQL 16, migrations in `internal/db/migrations`)

All tables are keyed by `chain_id` first. Wei values are `NUMERIC(40,0)`. Gas values are `BIGINT`. Backlogs are `NUMERIC(20,0)[]`: a backlog is a `uint64` in the pricer and saturates at 2^64-1, which does not fit in a `BIGINT`. A nullable column means unknown, never zero, and `pricing_version` says which rows carry the full pricing breakdown (1) and which are history recorded without it (0).

Production release `v1.0.1` established schema version 1 from `000001_init`. Released migrations are immutable, and every correction uses a new forward migration. CI verifies both empty-schema installation and upgrades from the last production schema. See [MIGRATIONS.md](MIGRATIONS.md) for authoring, release, rollback, and concurrent pull request rules.
Forward migration `000002_state_samples_chain_block` adds the state-sample lookup index, `000003_owner_action_tx_index` adds the owner-action transaction index column, `000004_missing_ranges` adds durable recovery records for skipped history, and `000005_batch_report_attributed_cost` records ArbOS-attributed batch costs. Migration `000006_poster_gas_fee_accounting` adds receipt-backed fee-accounting fields without inventing values for existing history.

```sql
networks            (chain_id PK, name, display_name, explorer_url, enabled, head_block, head_at, last_sample_at, last_error, updated_at)
blocks              (chain_id, number, hash, parent_hash, ts, gas_used, poster_gas BIGINT NULL, base_fee NUMERIC, l1_block, tx_count, pricing_version SMALLINT /* 1 = full breakdown, 0 = unknown history */,
                     backlogs NUMERIC(20,0)[], exponent_bips BIGINT, predicted_base_fee NUMERIC, anchored BOOL,
                     constraint_bips BIGINT[] NULL, min_base_fee NUMERIC NULL /* both null for pricing_version 0 */,
                     PK(chain_id, number))                       -- rolling, pruned after collector.block_retention
buckets             (chain_id, resolution TEXT /* '1m'|'15m'|'1h' */, bucket_start TIMESTAMPTZ,
                     blocks INT, gas_used BIGINT, poster_gas BIGINT NULL, fees_wei NUMERIC, poster_fees_wei NUMERIC NULL,
                     base_fee_min/avg/max NUMERIC, base_fee_sum NUMERIC NULL /* exact sum, null when unknown */,
                     exponent_end_bips BIGINT, pricing_version SMALLINT,
                     backlogs_end NUMERIC(20,0)[], backlogs_max NUMERIC(20,0)[], constraint_set_id INT,
                     constraint_bips_end BIGINT[] NULL, min_base_fee NUMERIC NULL,
                     floor_fees_wei NUMERIC NULL, surplus_fees_wei NUMERIC NULL /* destination fields null when any source block lacks poster gas or pricing data */,
                     replay_error_bips BIGINT /* max |predicted-actual| in bips over the bucket */,
                     last_block BIGINT /* highest block folded in; the backfill never overwrites *_end fields written by newer blocks */,
                     PK(chain_id, resolution, bucket_start))     -- kept forever
state_samples       (chain_id, sampled_at, block_number, base_fee, min_base_fee, constraints JSONB,
                     legacy JSONB /* {speedLimit,inertia,tolerance,backlog} or null */,
                     prices JSONB /* getPricesInWei tuple */, l1 JSONB /* pricer getters */,
                     accounts JSONB /* {infra:{address,balance}, network:{...}, l1Reward:{...}} or null */,
                     PK(chain_id, sampled_at))                   -- l1/accounts only on slow ticks; pruned after sample_retention
owner_actions       (chain_id, block_number, tx_hash, tx_index INT NULL /* null for rows scanned before it was recorded */, log_index, ts, method, selector, args JSONB,
                     PK(chain_id, tx_hash, log_index))
constraint_sets     (id SERIAL PK, chain_id, effective_block, effective_at, constraints JSONB, source TEXT /* 'genesis'|'owner_action'|'observed' */)
batch_reports       (chain_id, block_number, batch_number, batch_ts, poster, calldata_len, calldata_nonzero,
                     extra_gas, l1_base_fee NUMERIC, gas_spent BIGINT and wei_spent NUMERIC /* compatibility columns */,
                     report_version, arbos_version, per_batch_gas_charge, parent_gas_floor_per_token,
                     cost_calculation_version, attributed_gas_spent BIGINT, attributed_wei_spent NUMERIC,
                     PK(chain_id, block_number))
missing_ranges      (chain_id, from_block, to_block, detected_at, lifecycle /* pending|retrying|blocked */, reason,
                     cursor, replay_state JSONB, folded, retry_count, last_attempt_at, next_retry_at, last_error,
                     predecessor_at, successor_at, cursor_at, created_at, updated_at, PK(chain_id, from_block))
collector_state     (chain_id, key, value TEXT, updated_at, PK(chain_id, key))   -- checkpoints: head, live_start, backfill_cursor (with top), owner_log_cursor, owner_scan_through, owner_scan_origin, batch_scan_cursor_v2, endpoints, generation (bumped by every rewind and by a history rebuild; writers that fetched outside the lock re-check it before committing), rpc_capacity, eth_usd, history_epoch, telemetry (collector heartbeat, loop outcomes, head progress and cumulative RPC/database accounting). A legacy holes key is deleted only after its JSON has been imported into missing_ranges successfully
```

### 4.1 Poster-gas migration and historical recomputation

Migration 6 leaves existing `blocks.poster_gas`, `buckets.poster_gas`, and `buckets.poster_fees_wei` null and clears the old two-way destination sums. Existing total `gas_used` and `fees_wei`, pricing fields, and constraint fields stay intact. This is deliberate: zero poster gas is a fact that can only come from receipts.

Stop every old collector before applying migration 6. An old binary can otherwise write the obsolete two-way split back into buckets after the migration clears it. Apply the migration, deploy the API, web, and collector together, perform the replacement reset below while collectors remain stopped, then start the new collectors. During the transition, destination fields are null and the web shows the split as unavailable. The new collector requires `eth_getBlockReceipts` on ordinary endpoints. It validates receipts and records exact destination accounting for every new or replayed block.

Historical repair is a replacement replay, not an additive fold. Before maintenance, record the oldest bucket timestamp the API serves and configure `collector.backfill_depth` to reach at least that timestamp from the current head. Stop collectors, take a database backup, and perform the following per network in one maintenance transaction:

1. Delete that network's retained block rows and all bucket rows.
2. Clear its `head`, `live_start`, `backfill_cursor`, `owner_log_cursor`, `owner_scan_through`, `owner_scan_origin`, and `history_epoch` checkpoints, delete its `missing_ranges` rows, and null its `networks.head_block`, `head_at`, and `last_sample_at` fields.
3. Commit, restart the collector, and let the configured full backfill run from receipts.

The first collector tick seeds a fresh receipt-backed live head. That block becomes the exclusive upper bound of a new full backfill, which reconstructs every older bucket and replays backlogs with compute gas. Do not retain old block rows: the backfill normally begins below the oldest retained row, so keeping them would leave that retained interval unrecomputed. Do not reset the cursor without deleting buckets first, since old additive buckets would be counted twice. Resetting the owner scan checkpoints is required when the configured depth reaches earlier than its prior origin. Keep the API in maintenance mode while buckets are empty if a temporary history gap is unacceptable. Before ending maintenance, confirm `backfill_cursor.done` is true, the expected earliest timestamp is present or represented by a documented missing range, no unexpected missing ranges remain, and every served bucket has non-null `poster_gas` and all three destination sums. Checking only the currently present buckets can report success after the first replacement batch. Receipt fetching adds one metered call per block, so use a dedicated endpoint or allow for the public endpoint's configured pacing.

The down migration deliberately marks all pricing and destination history unknown before dropping poster gas. This prevents an older API from presenting a corrected compute-only pair as a complete two-way split, but it discards recorded minimum fees and constraint bips. Reapplying migration 6 after a rollback therefore requires the same full historical replacement replay. Migration `000006_poster_gas_fee_accounting` depends on migrations 2 through 5 already being present. The data reset must run only after every required schema migration is present.

`NOTIFY gascurve_live, '<json>'` is issued by the collector after each tick with the `LiveSnapshot` below (payloads stay under 8 kB; `recentBlocks` is not included in the notify payload, the API keeps its own ring buffer from `blocks` rows).

`NOTIFY gascurve_owner_action, '{"chainId": …, "action": OwnerAction}'` is issued when the slow loop stores a new owner action; the API turns it into the WebSocket `owner_action` message.

## 5. Collector (`internal/collector`)

One `Follower` per network, all sharing one `*sqlx.DB`. At startup each follower calls `eth_chainId` and refuses to run (logs and marks `networks.last_error`) if it differs from the configured `chain_id`.

Correctness rules the follower must keep:

- **Replay only forward from a known state.** On a fresh database persist the sampled head only and replay the blocks after it; never seed earlier blocks from a later sample. Catch-up ranges are split at owner-action boundaries. After a skipped gap, persist the sampled head only and record the range in `missing_ranges`.
- **A skipped range is queued work, not lost history.** A range whose preceding state is known is filled later by the history loop, newest first. That state comes from the range's own replay checkpoint or from the stored block before it, never from the live sample: replaying a legacy gap from today's parameters would price it with a speed limit, inertia or tolerance that was not in force there, and recorded owner actions can only be applied forward. A range with nothing to replay from (before the owner-scan origin without an archive endpoint, below the backfill depth with no independently known state, or whose anchor row retention has pruned) has lifecycle `blocked` and reason `no state`; it is re-examined when a state appears before it. Ranges are never capped, expired or removed except after every remaining block commits. Decode failures stop recovery and retain the source row.
- **Nothing advances in memory before the commit.** The pricer state is cloned for the replay; head, previous timestamp, state and last result are published only after the transaction commits. After a commit error the follower reloads head and state from the database (the last stored block row carries the backlogs) so a retry is idempotent.
- **Bucket folds are idempotent.** The fold and the head checkpoint commit in one transaction, and a bucket's additive fields are recomputed from the block rows inside the fold window when those rows exist; buckets store `base_fee_sum` (exact) rather than a running average.
- **Reorgs are detected.** Blocks store `hash` and `parent_hash`; a head whose parent hash does not match the stored head triggers a rewind to the common ancestor (blocks, bucket contributions, owner-action and batch cursors) in one transaction. The rewind moves `networks.head_block` and `head_at` down in that same transaction, or nulls them when no block survives, and sets `last_sample_at` from the newest surviving `state_samples` row (null when none survives): a rewind is not a successful sample and must not read as one in `/status` when the replacement sampling then fails.
- **Capability endpoints are verified before use.** Archive calls go through a managed archive pool with its own failover; the `newHeads` subscriber re-resolves its endpoint at every connection attempt and reports back what happened with it, so WebSocket health is tracked per endpoint apart from the HTTP verification and a socket that cannot be dialed, cannot be subscribed to or will not stay up is cooled down while its JSON-RPC keeps serving. A range with no independently known pricer state is recorded as a hole, never replayed from the live model.
- **Minimum base fee history.** The fee in force at a block comes from recorded `setMinimumL2BaseFee` actions, with nitro's genesis default (0.1 gwei, `InitialMinimumBaseFeeWei`) before the first recorded change, never the live value.

Fast loop, every `collector.tick_interval` (3 s by default: a sample costs six calls including the head lookup and receipts), or on every `newHeads` event when `ws_url` is configured (then state calls are made at that head's block number so samples align exactly with headers):

With `ws_url` the follower starts on the timer, pauses timer sampling once the `newHeads` subscription is up, and resumes it the moment the subscription drops (the subscriber reconnects on its own with 1 s to 30 s back-off and a 15 s ping watchdog). Heads arriving mid-tick collapse into one tick at the newest head; catch-up fetches the rest.

1. Resolve the head number first (`eth_blockNumber`), then one JSON-RPC batch at that exact block tag: `eth_getBlockByNumber(n, false)`, `eth_getBlockReceipts(n)`, `getGasPricingConstraints()`, `getPricesInWei()`, `getMinimumGasPrice()`, so a sample never mixes two blocks. If the constraints call reverts or returns empty, also `getGasBacklog()`, `getPricingInertia()`, `getGasBacklogTolerance()`, `getGasAccountingParams()` (legacy model).
2. Fetch headers and block receipts for every block between the stored head and the new head, in batches of `header_batch_size`, respecting the per-network budget. Receipt count, transaction hashes and indexes, block identity, cumulative gas, total gas, and poster gas are validated before a block is accepted. Missing `gasUsedForL1` is an error, never zero. On a paced network (`calls_per_second > 0`) a gap larger than `max_catch_up_batches` × `header_batch_size` blocks is skipped (logged) and the replay restarts from the sampled backlogs; unlimited (dedicated node) networks always fetch every block.
3. Replay each block through the pricer. When the sampled state's block number equals a replayed block, overwrite the backlogs with the sampled values (`anchored = true`) so drift never accumulates.
4. Upsert `blocks`, fold into `buckets` (1m, 15m, 1h), insert `state_samples`, update `networks.head_block`, `NOTIFY`.

Slow loop, every `collector.slow_interval` (60 s): the ETH/USD spot from `collector.eth_usd_source` (default Coinbase's public spot endpoint, no key; disable with an empty value), stored in `collector_state` before it is published in memory (so the quote a tick emits and the one `/live` reads are the same row) and included in every snapshot until it is older than `collector.eth_usd_max_age` (default 10 m); L1 pricer getters (`getL1BaseFeeEstimate`, `getL1PricingSurplus`, `getL1FeesAvailable`, `getL1PricingUnitsSinceUpdate`, `getLastL1PricingUpdateTime`, `getL1PricingEquilibrationUnits`, `getPerBatchGasCharge`, `getL1RewardRate`), fee-account balances (`ArbOwnerPublic.getInfraFeeAccount/getNetworkFeeAccount`, `ArbGasInfo.getL1RewardRecipient`, `eth_getBalance`), `eth_getLogs` on `0x…70` for new `OwnerActs` since the cursor (asked in pieces of at most the width the endpoint has been seen to accept: any refusal halves the width, down to a single block, and probes back up one step per quiet minute; a refusal of a single block is an endpoint failure and the pool tries the next endpoint), batch-report scan of new 2-transaction blocks, pruning.

History loop (one goroutine, the gap filler first and the backfill only when nothing is fillable, so recent charts complete before deep history grows; completion lives in the backfill cursor alone, which answers `Done` without a call, so a rewind that resets it puts the job back to work instead of leaving the deleted history unrebuilt for the life of the process):

Gap filler (resumable, with the cursor on the `missing_ranges` row): takes the newest fillable range, fetches its headers in `header_batch_size` batches through the Bulk lane (sized to the spare budget exactly like the backfill, never below `minBackfillBatch`, never concurrently for one network), verifies that the range links by parent hash to the stored block before it and to the stored block after it, replays the pricer forward from the end-of-block state of `from - 1` splitting at owner-action boundaries the way the catch-up does, and writes the blocks and their buckets exactly as the live path does (rows and rebuilt buckets from the hour of the first live block on, additive folds below it). That state comes from the row's own `replay_state` or, preferred when the block is still stored, from the block itself with its hash cross-checked against the replay state; the shape in force there is the recorded constraint set or the newest `state_samples` row at or below the block, never the live sample. A range below the bucket boundary therefore fills across as many batches as it needs even though it stores no rows of its own. The block at `to_block + 1` carries the real sampled backlogs: the replay ends on it, is anchored to them, and the prediction it produces is written back onto that row, so the error of the whole reconstruction is recorded there. Each commit is one chain transaction with the `generation` re-checked, advances `cursor`, `cursor_at` and `replay_state`, and removes the row only when the range is complete. A failed attempt sets lifecycle `retrying` and persists its count, error, attempt time and next retry time. A rewind that reaches into recovered blocks resets the cursor, replay state and cursor time but keeps `folded`, since additive buckets below the boundary must not count the same blocks twice. The filler always yields to a catch-up in progress. Under sustained lag it yields 29 history turns, then receives one Bulk turn, while the pacer's Fast reserve remains exclusive to head sampling. This preserves fast-loop priority without starving durable recovery forever.

Backfill job (resumable, checkpoint in `collector_state`): walks backwards from the first stored block to `collector.backfill_depth` fetching headers (the owner-action scan starts at the same depth plus a ten-minute margin, sampling the pricing state there from an archive endpoint when one exists, so a shallow development depth never re-indexes a whole chain). Both windows are measured back from the sampled head's own timestamp, never from the host clock, and the depth is capped at a century where the cutoff is computed so that adding the margin cannot overflow the arithmetic: a clock ahead of a stalled chain would otherwise put the cutoff past the head and mark the whole configured depth unavailable for good. A cutoff later than the head is a retryable error and writes no origin. The job replays from the nearest earlier `constraint_sets` row (starting backlogs from the owner action), and writing buckets only. Runs at low priority inside the same budget (it yields whenever the fast loop needs calls). On `archive: true` networks it re-anchors backlogs from historical state every `backfill_anchor_interval` blocks and records the replay error observed just before each anchor.

History rebuild (`history_epoch`, per network): enabling `archive` changes nothing already stored, because the backfill has finished (its cursor answers `Done` without a call), the owner-scan origin is recorded once, and stored buckets are never revisited. Raising the network's `history_epoch` above the `history_epoch` value in `collector_state` runs one rebuild at start, before any loop does work, and records the new value; a pod that restarts on the same number rebuilds nothing, which is what makes it safe to leave in a manifest. In one chain transaction it bumps the generation (so a step that fetched before it discards its work), deletes the buckets starting before the bucket boundary (what the backfill owns; the row-backed ones above it are rebuilt from rows and stay), resets the backfill cursor, clears `owner_scan_origin`, `owner_log_cursor` and `owner_scan_through` so the origin is established again from archive state, moves blocked `no state` ranges back to `pending` so the re-established origin can make them fillable, and restarts any range that had folded into the deleted additive buckets. It never deletes a missing-range record. Block rows, owner actions, constraint sets and state samples are observations and are kept: the rescan re-reads logs it already recorded and writes the same rows. Clearing `owner_scan_through` is what orders the rebuild, since the backfill starts no segment before a scan pass completes again.

Rate limiting: a token bucket per endpoint, `calls_per_second` tokens/s, burst 2× that. Every JSON-RPC call consumes one token, including each item inside a batch. Batches never exceed 100 items, never exceed the endpoint's pacer burst minus the fast reserve on a budgeted endpoint, and are never sent concurrently for the same network. The pacer never spends tokens the bucket does not hold and has two lanes: the head or timer tick's state sample is `Fast` and draws from a reserve of `max(1, rate/4)` tokens per second without queueing; whatever a fast caller needs beyond the reserve queues first come, first served with `Bulk` (everything else), so a demanding tick on a small budget cannot starve the slow loop or backfill. `tick_interval` can be set per network (`NETWORK_<NAME>_TICK_INTERVAL`); dedicated endpoints run at 500 ms, public ones stay at 3 s.

Endpoint throughput must exceed live ingress demand if missing-range recovery is expected to converge. For a constraints chain polled every `T` seconds while the chain produces `B` blocks/s, live demand is approximately `(6 + 2×max(B×T - 1, 0)) / T` calls/s: six calls resolve and sample the head, and every intervening block needs one header and one receipt call. With reliable `newHeads`, the steady requirement is approximately `5×B` calls/s because each pinned sample costs five calls. Legacy pricing adds four calls to every sample. Slow-loop traffic, retries and recovery need headroom above those figures. `/status.capacity` reports the configured budget, the required rate calculated from the latest observed interval, the ten-second observed rate, finite headroom and whether the budget was saturated. `/status.degraded` is true while capacity is saturated, history is missing or a recovery checkpoint cannot be decoded.

Rate limiting is recognised from HTTP 429 and from JSON-RPC errors with codes -32005 or -32007 or a message naming a rate or request limit (QuickNode reports its 50 requests per second as `-32007`); all of these back off with the same exponential schedule (2 s to 60 s), share the endpoint's cooldown, halve the endpoint's batch cap, and drive failover when persistent. Because the burst is twice the rate, a provider with a strict per-second window is configured at half that window (QuickNode: `calls_per_second: 25`), otherwise the one-second peaks trip it and every back-off stalls the history work. All requests send `User-Agent: gascurve/<version>`.

## 6. API (`internal/api`, chi, prefix `/api/v1`)

Conventions: JSON, `Cache-Control` set per endpoint, CORS from `server.cors_origins`, rate limited per IP (the client IP is taken from `X-Forwarded-For` only when the peer is in `server.trusted_proxies`, a list of CIDRs, walking the chain right to left past trusted hops), request ID header. WebSocket connections are capped per IP and globally (`server.ws_max_per_ip`, `server.ws_max_total`) and messages are rate limited per connection. Wei values are decimal strings. Gas, bips, block numbers and unix timestamps are JSON numbers. Timestamps named `*At` are RFC 3339 UTC. Errors: `{ "error": { "code": "not_found", "message": "..." } }`.

`{network}` accepts the name (`robinhood`) or chain id (`4663`).

| Method and path | Purpose |
|---|---|
| `GET /health`, `GET /ready` | shallow process liveness; readiness checks DB and the PostgreSQL notification listener |
| `GET /networks` | `Network[]` |
| `GET /networks/{network}` | `Network` |
| `GET /networks/{network}/live` | `LiveSnapshot` (same as WS `tick`), `Cache-Control: no-store` |
| `GET /networks/{network}/blocks?limit=120` | `BlockPoint[]` newest first, max 1000 |
| `GET /networks/{network}/series?range=1h\|24h\|30d\|all` | `Series` (see below) |
| `GET /networks/{network}/constraints` | `{ current: ConstraintSet \| null, history: ConstraintSet[] }` (`null` whenever the latest state sample's model is not `constraints`, or before any set is known; `history` is still returned) |
| `GET /networks/{network}/owner-actions` | `OwnerAction[]` newest first |
| `GET /networks/{network}/batches?range=…` | `BatchSeries` (ArbOS-attributed batch-posting cost per bucket, not Ethereum receipt totals, plus batch cadence) |
| `GET /networks/{network}/l1?range=…` | `L1Series` (pricer getters over time, from state samples) |
| `GET /status` | `{ version, status, listener: { ready, reconnects, lastError }, networks: [{ ..., degraded, capacity: { configuredCallsPerSecond, requiredCallsPerSecond, observedCallsPerSecond, headroomCallsPerSecond, saturated, at, checkpointError }, holes: { pending, blocks, unfillable, retrying, oldestAgeSeconds, checkpointError, pendingBlocks, oldestPendingAt, oldestPendingAgeSeconds }, status, degradedReasons, collector: { heartbeatAt, heartbeatAgeSeconds, heartbeatStaleAfterSeconds, observedHead, indexedHead, headLagBlocks, loops: { fast, slow, history }, rpc, database } }] }`. Top-level `status` is `healthy` or `degraded`; it degrades when the notification listener or any enabled network is degraded. A network is `healthy`, `degraded` or `disabled`. The API degrades an enabled network when the heartbeat or a loop is missing or stale, a loop's newest outcome is an error, the collector trails its last observed head, queued gaps outlive the history-loop freshness window, or anything the per-network `degraded` flag covers (missing blocks, saturated RPC capacity, an unreadable checkpoint). `listener.lastError` is null while its PostgreSQL LISTEN connection is ready, and `listener.reconnects` counts successful recoveries after startup. Existing head, rate-limit, backfill, ArbOS and endpoint fields remain for compatibility; endpoint URLs are never exposed, in `error` and `wsError` either. `collector` is null until a compatible collector writes its first telemetry checkpoint. Loop entries retain both last success and last error timestamps plus the last duration. RPC accounting includes calls, HTTP requests, errors, rate limits and average HTTP latency. Database accounting includes operations, errors, average latency and last latency. `holes.pending` counts pending and retrying rows, `holes.unfillable` counts blocked rows, `holes.blocks` is the remaining block count including unfillable history, `holes.oldestAgeSeconds` is the age of the oldest range, while `pendingBlocks` and the `oldestPending*` fields cover queued work only. A checkpoint decode failure sets the related `checkpointError` instead of reporting no gaps. |
| `GET /ws?network=…` | WebSocket, see §7 |

Points in `Series`, `BatchSeries` and `L1Series` are always ascending by `t`. Range to resolution: `1h` → per block from `blocks` (a `step` of 5 s is applied server-side if more than 2000 points), `24h` → `1m` buckets, `30d` → `15m`, `all` → `1h`. `/live` returns 404 until the collector has produced a sample. Arrays are never `null` in responses. `L1Series` reaches back at most `collector.sample_retention`.

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
  block: { number: number; ts: number; gasUsed: number; posterGas: number | null; baseFee: string; txCount: number };
  baseFee: string; minBaseFee: string; multiplierBips: number; exponentBips: number;
  model: 'constraints' | 'legacy';
  constraints: Constraint[];               // empty for legacy
  legacy?: { speedLimit: number; inertia: number; tolerance: number; backlog: number };
  prices: { perL2Tx: string; perL1CalldataByte: string; perL2Storage: string;
            perArbGasBase: string; perArbGasCongestion: string; perArbGasTotal: string };
  gasPerSecond: { s10: number; s60: number }; // total gas, retained for API compatibility
  computeGasPerSecond: { s10: number | null; s60: number | null }; // receipt-backed pricer input, null when any source block lacks poster gas
  l1?: { baseFeeEstimate: string; surplus: string; feesAvailable: string; unitsSinceUpdate: number;
         lastUpdateAt: string; equilibrationUnits: number; perBatchGasCharge: number; rewardRate: number };
  accounts?: { infra: Account; network: Account; l1Reward: Account };
  replayErrorBips: number;                 // |predicted - actual| for the latest block
  ethUsd: { price: string; at: string; source: string } | null;   // ETH/USD spot fetched by the collector's slow loop (server side, never the browser), null when unavailable or stale
}
type Account = { address: string; balance: string }

type BlockPoint = {
  number: number; ts: number; gasUsed: number; posterGas: number | null; baseFee: string; predictedBaseFee: string;
  backlogs: number[];        // end-of-block backlogs (after AddGas)
  constraintBips: number[] | null;  // start-of-block per-constraint exponent, the values that priced this block; sums to exponentBips; null only for pricing version 0 rows (history recorded before the breakdown existed)
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
  blocks: number; gasUsed: number; posterGas: number | null; gasPerSecond: number; computeGasPerSecond: number | null; feesWei: string;
  // gasPerSecond retains the total-gas API value. computeGasPerSecond is the pricer input rate and is null when receipt poster gas is unavailable for any source block.
  // minBaseFee follows pricing_version. The destination split is independently unknown until both pricing and receipt inputs are available.
  coverage: number | null;                 // share of the bucket the collector indexed when the time span is measurable. Bounded missing intervals reduce it; it is null when missing-range time bounds are insufficient. Both gas rates are usable only when coverage is positive and non-null. Sums (gasUsed, feesWei, blocks) always include indexed blocks only
  completeness: 'complete' | 'partial' | 'unknown'; // complete means every block is indexed; partial means the API can place a missing range in this bucket or the bucket is still in progress, even when equal block timestamps leave coverage at 1; unknown means a missing range lacks enough time bounds to decide whether it overlaps this bucket
  baseFeeMin: string; baseFeeAvg: string; baseFeeMax: string;
  exponentBips: number; constraintBips: number[] | null;   // start-of-block values of the bucket's last block; null for pricing version 0 history
  backlogs: number[]; backlogsMax: number[];
  minBaseFee: string | null;                        // floor in force at the bucket's last block; null when any block in the bucket has pricing version 0
  floorFeesWei: string | null; surplusFeesWei: string | null; posterFeesWei: string | null; // computeGas × min(baseFee,minBaseFee), compute congestion, and posterGas × baseFee. When known, the three sum to feesWei
  constraintSetId: number; replayErrorBips: number;
}

type OwnerAction = {
  block: number; at: string; txHash: string; method: string; selector: string;
  args: Record<string, unknown>;           // decoded when the selector is known, else { raw: '0x…' }. setGasPricingConstraints: { constraints: [{ gasTargetPerSecond, adjustmentWindowSeconds, startingBacklog }] }; setMinimumL2BaseFee: { priceInWei: string }
}

type BatchSeries = { range: string; resolution: 'batch' | '1m' | '15m' | '1h'; from: number; to: number; /* window as in Series */ /* 'batch' = exactly one point per report, never grouped, used for 1h; gasSpent and weiSpent are ArbOS-attributed values, not receipt totals */ points: { t: number; batches: number; gasSpent: number; weiSpent: string; l1BaseFeeAvg: string; calldataBytes: number }[] }
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

lib/pq owns the notification connection and reconnects it on its own, re-issuing every LISTEN and marking the gap with a reconnect notification. The hub reconciles from that marker: it refreshes stored blocks, rebuilds a newer live snapshot and delivers missed owner actions before resuming normal fan-out. The API adds only the health lib/pq reports through its event callback, so a replica whose feed is down answers `/ready` with 503 and leaves the Service, even though its query pool is healthy and its REST answers would have been correct.

Nothing replaces a failed lib/pq listener, because a failed one cannot be observed. lib/pq closes its notification channel in exactly one place, `listenerMain`, after `listenerConnLoop` returns, and both of that loop's returns are guarded by `l.closed()`, which only `Close` sets: a closed channel means this process closed it. If one ever closes anyway, `Hub.Run` returns an error and the API exits rather than serving WebSocket pings from a hub that can never deliver another update, which recovers through a restart instead of swapping a listener underneath live clients.

`/ready` therefore answers 503 for two reasons, the database ping and the listener, which is why `gascurve_api_listener_ready` exists: `GascurveDatabaseUnreachable` subtracts the listener case so the two page separately.

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
src/types/               the shapes above, less the response fields no component reads
src/utils/               formatting (gwei, gas, durations), bips math
src/lib/seo.ts           site metadata: canonical origin, titles, the social card, the network list the sitemap uses
```

`src/types/` narrows the response shapes to what the client renders rather than restating them. `/status` carries fields meant for an operator that no component reads: `holes`, `activeEndpoint`, `failovers`, `endpoints` and `listener` are all absent from `StatusResponse`. Add one when something on the page starts using it, so a type that grows records a real dependency instead of churning four test fixtures for a field nothing reads.

Every chart card carries an enlarge control linking to `/{network}/charts/{chart}`, where `chart` is one of the registry ids in `src/lib/chartViews.ts`: `base-fee`, `backlog-sawtooth`, `contribution`, `gas-per-second`, `backlogs`, `fee-flows`, `l1`, `taylor`. The enlarged page draws the same component with the same hooks at a taller frame, keeps the range in `?range=` and the constraint slot in `?constraint=`, and offers tabs across every chart plus a link back to the section it came from.

Units in copy: gas carries an SI prefix on the unit, never on the number (`11.2 Tgas`, `60 Mgas/s`, `812,345 gas` below one million). Figures that animate use fixed decimal counts per band so neighbouring elements never shift. USD figures (from `ethUsd`) are shown by default; hovering one gives the working (`ETH amount × $price/ETH = $figure`) and the quote behind it (source and age), and the same facts are in the accessible description.

Environment: `NEXT_PUBLIC_API_URL` (default `http://localhost:8080/api/v1`), `NEXT_PUBLIC_WS_URL` (derived from the API URL when unset), `NEXT_PUBLIC_SITE_URL` (canonical origin, default `https://gascurve.com`).

### Search metadata and icons

The site is a Robinhood Chain gas tracker first; the other networks are carried for comparison, and that ordering is what `src/lib/seo.ts` encodes. `PRIMARY_NETWORK` names the chain the default title, the description, the social card and the index page lead with, and `SITE_NETWORKS` mirrors the `networks` block of `config.yaml`, marking that one primary. The list is duplicated there rather than fetched because the sitemap, the server rendered titles and the index page's crawlable fallback are all built on the server, where `NEXT_PUBLIC_API_URL` is a path on the site's own origin and cannot be fetched. **Add a network to `SITE_NETWORKS` whenever one is added to `config.yaml`.**

`NEXT_PUBLIC_SITE_URL` is the origin every canonical link, sitemap entry and card URL resolves against, and is baked at build time by `Dockerfile.web`. Every route under `/[network]` builds its metadata through `pageMetadata`, which sets the title, description, social card and either a canonical link or, for the duplicate that a chain id route such as `/4663` serves, `noindex, follow`. The card image is named explicitly in both the `openGraph` and `twitter` objects rather than dropped in as an `opengraph-image` file: a page that declares its own `openGraph` replaces the whole object, so a card left to the file convention would be present on the index and missing from every network page.

Generated routes: `/robots.txt` (`app/robots.ts`), `/sitemap.xml` (`app/sitemap.ts`, the index plus each network's page, explainer and one entry per chart, weighted so the primary chain ranks above the rest) and `/manifest.webmanifest` (`app/manifest.ts`).

Icons: `app/icon.svg` is the source of truth for the mark, a base fee curve lifting off its floor with the live block as the bright tip, in the dark palette's magenta and cyan. `app/favicon.ico` (16/32/48), `app/apple-icon.png` (180) and `public/icon-192.png`, `public/icon-512.png`, `public/icon-maskable-512.png` are the same geometry on the same 64 unit grid; regenerate them together if the mark changes. `public/og-card.png` is the 1200x630 social card.

Coverage gate (90% lines) applies to `src/lib/**`, `src/hooks/**`, `src/utils/**`. Components are tested where behaviour is non-trivial.

## 9. Metrics (`internal/metrics`, Prometheus)

Both binaries serve the exposition format at `/metrics`, from `internal/metrics` and the `prometheus/client_golang` registry: the api on `server.port` (alongside the REST API and the WebSocket, outside the request timeout and not counted by its own request instruments), the collector on `collector.metrics_port` (default 9090, `METRICS_PORT`; `0` disables it) next to `/startup`, `/health` and `/ready`. Both registries also carry the standard Go runtime and process collectors (`go_*`, `process_*`).

Instruments are written from points the code already reaches and are never read back by the application, so a scrape reads the registry alone: a follower stuck on an RPC call or on the database still answers one. Committed-head gauges move only after their transaction commits. Separate observed-head and loop-outcome gauges expose work in progress and failures without claiming it was committed.

**No endpoint URL is ever a label value.** Endpoints are identified by their index in the network's endpoint list, exactly as `/status` reports them; `internal/nitro` scrubs URLs from every error it returns for the same reason.

Every per-network collector series carries `network` (the configured name) and `chain_id` (decimal), and the per-endpoint ones add `endpoint` (the index). Heartbeat and database series describe the process and carry no network label.

| Series | Type | Extra labels | Meaning |
|---|---|---|---|
| `gascurve_collector_heartbeat_timestamp_seconds` | gauge | none | unix time of the latest process heartbeat |
| `gascurve_collector_database_operations_total` | counter | none | PostgreSQL driver operations |
| `gascurve_collector_database_errors_total` | counter | none | failed PostgreSQL driver operations |
| `gascurve_collector_database_latency_seconds` | gauge | none | average PostgreSQL operation latency since process start |
| `gascurve_collector_database_last_latency_seconds` | gauge | none | latency of the latest PostgreSQL operation |
| `gascurve_collector_head_block` | gauge | | number of the newest committed block |
| `gascurve_collector_head_lag_seconds` | gauge | | age of that block, the figure `/status` reports as `lagSeconds` |
| `gascurve_collector_observed_head_block` | gauge | | number of the newest head observed by the fast loop |
| `gascurve_collector_head_lag_blocks` | gauge | | blocks between the observed and committed heads |
| `gascurve_collector_last_sample_timestamp_seconds` | gauge | | unix time of the last successful state sample |
| `gascurve_collector_tick_duration_seconds` | histogram | | wall time of one fast tick, failed ticks included |
| `gascurve_collector_loop_last_success_timestamp_seconds` | gauge | `loop` (`fast`, `slow`, `history`) | unix time of the loop's last success |
| `gascurve_collector_loop_last_error_timestamp_seconds` | gauge | `loop` (`fast`, `slow`, `history`) | unix time of the loop's last error |
| `gascurve_collector_loop_duration_seconds` | histogram | `loop` (`fast`, `slow`, `history`) | wall time of each loop iteration, failures included |
| `gascurve_collector_rpc_calls_total` | counter | `class` (`fast`, `bulk`) | JSON-RPC calls sent, one per item inside a batch, by pacer lane |
| `gascurve_collector_rpc_requests_total` | counter | | HTTP requests sent to JSON-RPC endpoints, retries included |
| `gascurve_collector_rpc_errors_total` | counter | | failed HTTP attempts and item-level JSON-RPC errors |
| `gascurve_collector_rpc_latency_seconds` | gauge | | average HTTP round-trip latency since process start, excluding pacer waits |
| `gascurve_collector_rate_limit_events_total` | counter | | times any endpoint of this network reported throttling |
| `gascurve_collector_endpoint_rate_limit_events_total` | counter | `endpoint` | the same, per endpoint |
| `gascurve_collector_active_endpoint` | gauge | | index of the endpoint ordinary calls go to |
| `gascurve_collector_endpoint_failovers_total` | counter | | times the pool moved to another endpoint |
| `gascurve_collector_endpoint_disabled` | gauge | `endpoint` | 1 while that endpoint is disabled |
| `gascurve_collector_endpoint_ws_cooling` | gauge | `endpoint` | 1 while that endpoint's WebSocket is cooled down |
| `gascurve_collector_backfill_cursor_block` | gauge | | block the active backfill segment has replayed to |
| `gascurve_collector_backfill_floor_block` | gauge | | oldest block `collector.backfill_depth` reaches |
| `gascurve_collector_backfill_blocks_remaining` | gauge | | blocks between them, over every segment still to come |
| `gascurve_collector_backfill_done` | gauge | | 1 once the cursor reports the depth rebuilt |
| `gascurve_collector_holes_pending` | gauge | | ranges queued for the gap filler |
| `gascurve_collector_holes_blocks` | gauge | | blocks not indexed, queued and unfillable together |
| `gascurve_collector_holes_unfillable` | gauge | | ranges nothing can be replayed into |
| `gascurve_collector_holes_pending_blocks` | gauge | | blocks missing across queued, fillable ranges |
| `gascurve_collector_holes_oldest_age_seconds` | gauge | | age of the oldest queued range, zero when none is queued |
| `gascurve_collector_holes_filled_total` | counter | | ranges the gap filler has completed |
| `gascurve_collector_catch_up_gaps_skipped_total` | counter | | catch-up gaps skipped for exceeding the call budget |

The `holes_*` gauges match the pending, blocks and unfillable fields `/status` serves as `holes`; status also reports retrying ranges, age, queued-work totals and checkpoint decode failures. The endpoint gauges match the state it serves as `endpoints`.

The api's series carry no network label: it serves every network from one process.

| Series | Type | Labels | Meaning |
|---|---|---|---|
| `gascurve_api_requests_total` | counter | `route`, `method`, `status` | requests served |
| `gascurve_api_request_duration_seconds` | histogram | `route`, `method` | time to serve one request |
| `gascurve_api_ws_clients` | gauge | | WebSocket clients currently subscribed |
| `gascurve_api_ws_frames_sent_total` | counter | | frames written to clients, pings included |
| `gascurve_api_ws_clients_dropped_total` | counter | | clients closed for a full outbound queue |
| `gascurve_api_listener_ready` | gauge | | 1 while the PostgreSQL notification listener is connected and subscribed |
| `gascurve_api_listener_reconnects_total` | counter | | notification listener recoveries since the process started |

`route` is the chi route pattern (`/api/v1/networks/{network}/series`), resolved after the handler returned, never the request path: a network name or a block number must not become a series of its own. A request no route claims is labeled with the wildcard chi matched (`/api/v1/*`), or `unmatched` when it never reached the router. `method` is bounded the same way: an HTTP method is an arbitrary token that net/http accepts and the router answers 405 to, so anything outside the nine real methods is labeled `other`. `/metrics` is not counted (it is the scrape itself) and neither is a WebSocket that upgraded, whose duration is the life of the socket rather than request latency; a handshake the server refused is counted like any other answer, so sustained WebSocket errors still reach the error rate. The scrape is also exempt from the per-IP rate limit: where a proxy makes ordinary traffic and Prometheus share one peer address, a throttled `/metrics` reads as a dead api.

The chart renders a `ServiceMonitor` per deployable and a `PrometheusRule` (both off by default, both needing the Prometheus operator's CRDs) plus a headless Service for the collector, which has no Service of its own otherwise. Every alert is scoped to the job and namespace of that release's targets (the job an operator derives from a ServiceMonitor is the Service name, which two namespaces running a release of the same name would share), so no release alerts on another's metrics. The two "not being scraped" alerts are `absent(up{...} == 1)` rather than `up{...} == 0`: the latter matches only a target that still exists and failed, and goes quiet exactly when service discovery removes it. Because `absent` fires when no target exists at all, a group is emitted only for a component this release scrapes, on the same condition as its ServiceMonitor, and every alert carries `job` and `namespace` as literal labels, which `absent`, `sum` and `min by ()` would otherwise leave off the alert that fires. Thresholds and alert windows are values; see `charts/gascurve/README.md`.

## 10. Versioning, images and delivery

One version for the whole repo, managed by release-please from the Go component; `web/package.json` is bumped as an extra file. Three images: `gascurve-collector`, `gascurve-api`, `gascurve-web`, each built for `linux/amd64` and `linux/arm64`. The Helm chart under `charts/gascurve` is versioned separately and records the application version it ships as `Chart.yaml` `appVersion`.

Two tag namespaces, two workflows, and they must not be confused: `v<version>` is an application release and is handled by Docker Publish; `gascurve-chart-v<version>` is a chart release and is handled by Helm Publish. Both refuse to act on a bare Git tag: the version must have a published, non-prerelease GitHub release, and `.release-please-manifest.json` at that tag must record the same version, so a tag moved onto an unrelated commit cannot be published.

**Nothing is tagged before it is scanned.** Docker Publish pushes each architecture by digest, with no tag of any kind, scans that exact digest with Trivy at HIGH and CRITICAL, and only then joins the two scanned digests into the exact version tag. The order matters because the tag cannot be taken back: the registry carries an immutable-tag rule on exact `MAJOR.MINOR.PATCH` tags, so an image that turns out to be vulnerable after it has been tagged can only be superseded, never replaced. The pre-merge Docker Build job in CI runs the same gate at the same severity on the pull request, so a change that would be unpublishable is caught before it is merged rather than after a release has been cut.

Immutability also decides what a rerun does. Neither publish workflow has an overwrite mode, because there is nothing an overwrite could do except fail against the registry rule. Instead each checks the registry first: a missing artifact is built and pushed, an artifact that is already there and already correct (an image: both platforms, matching `org.opencontainers.image.version` and `revision`; a chart: matching name, version and `appVersion`) is an idempotent success, and an artifact that is there but is something else is a hard failure that needs a new patch release. A rerun after a partial failure therefore finishes exactly the artifacts that never made it.

The moving tags (`latest` and `<major>.<minor>`) are excluded from the immutable-tag rule and are updated separately, from the digest the exact version tag resolves to, and only when the version being published is the greatest published stable release. Repairing or back-filling an older version never rolls `latest` backward, and a rerun whose only job was to finish an interrupted publish still fixes up aliases that run never wrote.

Chart releases are downstream of application releases: Helm Publish packages the `appVersion` recorded at the chart tag (never "the newest application release", which would substitute a newer application into an older chart) and refuses to publish until all three images for that `appVersion` exist in the registry. On the very first release that makes the merge order load-bearing: application release PR, then the appVersion sync PR it opens, then the chart release PR. See the Releasing section of `charts/gascurve/README.md`.
