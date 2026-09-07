# Robinhood Chain Gas Explorer — Spec

One-page, auto-updating site that explains Robinhood Chain's multi-constraint gas pricer and shows it working: live per-block numbers plus 1h / 24h / 30d / all-time history, sourced from the public RPC.

Everything in the "Verified facts" sections below was measured live against `https://rpc.mainnet.chain.robinhood.com` on 2026-09-06. Re-verify before shipping; the chain owner changes parameters regularly.

---

## 1. Goals

- Explain the mechanics: why fees can sit at the floor for weeks and then ratchet up for 11 days straight, why there are no tips, where the fees go.
- Show the mechanics: live decomposition of the base fee into its per-constraint contributions, backlogs draining and filling in real time, and the same series over 1h / 24h / 30d / all-time.
- Real-time first: the page updates every block-ish (~1 s) without a reload, driven by the viewer's browser polling the public RPC directly.
- Zero-cost infra: public RPC only, static hosting, one tiny collector for history.
- Be honest about what is measured vs. reconstructed (see §7).

Non-goals: wallet integration, per-transaction fee estimation UI, anything needing a paid RPC.

---

## 2. Verified facts about the chain

| Item | Value | How verified |
|---|---|---|
| Chain ID | 4663 (`0x1237`) | `eth_chainId` |
| Stack | Arbitrum Nitro `v3.11.4-rc.3`, ArbOS **61** (`ArbSys.arbOSVersion()` = 116 = 55 + 61) | `web3_clientVersion`, precompile |
| Gas token | ETH | docs |
| Block cadence | ~10.5 blocks/s (~0.9 M blocks/day), many blocks share a timestamp (second granularity) | block numbers vs. launch date |
| Head block | ~55.8 M | `eth_blockNumber` |
| Min base fee | **0.02 gwei** (set at block 174,150; genesis default was 0.1 gwei) | `getMinimumGasPrice`, OwnerActs |
| Block gas limit / max tx gas | 32 M / 32 M | `getMaxBlockGasLimit`, `getMaxTxGasLimit` |
| Pricing model in use | **Single-gas multi-constraint** (`getGasPricingConstraints` non-empty; `getMultiGasPricingConstraints` empty) | precompile |
| Live constraints | `[60 M gas/s, 15 s]` and `[40 M gas/s, 86 400 s]` | `getGasPricingConstraints` |
| Legacy params (dormant) | speed limit 7 M, inertia 102, tolerance 10, `getGasBacklog()` = 0 | precompile |
| Tips | `getCollectTips()` = false; `eth_maxPriorityFeePerGas` = 0; FCFS sequencer | precompile, docs |
| L1 price per unit | ~0.0024 gwei (`getL1BaseFeeEstimate` = 2,369,608 wei); receipt `gasUsedForL1` remains the authoritative poster-gas charge | precompile, receipt |
| Fee accounts | infra (floor part): `0x5a2B80a9…9BE7` (402 ETH); network (congestion part): `0xbC5C3a7A…F067` (10,706 ETH); L1 reward: `0x8F516B99…82ea`; batch posters: sequencer alias + `0xDaa52608…87F4` | `ArbOwnerPublic`, `ArbAggregator`, `eth_getBalance` |
| Chain owner | `0x2A153c6A…5C09` (single owner) | `getAllChainOwners` |

### 2.1 Verified facts about the public RPC

| Capability | Result |
|---|---|
| Transport | HTTPS JSON-RPC only. **No WebSocket** on any tested host/path, so no `eth_subscribe`. Real-time = polling. |
| CORS | `access-control-allow-origin: *`, preflight cached 600 s → browsers can call it directly. |
| User agent | Cloudflare WAF returns **403** for Python's default UA. Browsers and curl are fine. Collector must send a real UA. |
| Batch requests | Supported. 100 items OK; 250 items → 429. |
| Rate limit | Counts **JSON-RPC calls, not HTTP requests**, per client IP. Two apparent limits: a burst cap (15 parallel batches × 20 = 300 calls in <1 s → 14 of 15 rejected; a single 250-item batch rejected) and a rolling quota of roughly **4,000 calls/min** (100-item header batches every 1.5 s ran fine for ~40 batches, then every batch was rejected). Sustained ~4 calls/s for minutes: never limited. Full-transaction block fetches (`eth_getBlockByNumber(n, true)`) appear to be weighted heavier: a 100-item full-tx batch was rejected outright while 20-item ones passed. Recovery is seconds. No `Retry-After` header. |
| `eth_getBlockReceipts` | Supported by the configured public RPCs. Each receipt includes `gasUsedForL1`; block 1,000,000 has 767 poster gas across two receipts. |
| `eth_feeHistory` | Max **1024** blocks/call (larger counts are silently clamped). `baseFeePerGas` matches headers exactly. **`gasUsedRatio` is not usable** (values like 1.0 for a 402 k-gas block; denominator is not 32 M). No timestamps. |
| Archive state | `eth_call` at historical blocks works only for roughly the last **~1,000–20,000 blocks** (a few minutes); older → `metadata is not found`. So precompile state (backlogs, L1 pricer, balances) **cannot be back-filled**. |
| `eth_getLogs` | Full-range (block 0 → latest) query on a sparse address returned in 2.4 s. Owner-action history is fully recoverable. |
| Sequencer feed | `wss://feed.mainnet.chain.robinhood.com` opens from a browser but streams ~3.6 MB/s of raw L2 messages with no base fee. Rejected as a data source. |

Budget derived from this: **≤ 5 calls/s sustained per IP, ≤ 100 calls per HTTP batch, never fire batches concurrently, exponential back-off on 429 starting at 2 s.**

---

## 3. The mechanics the page must explain

### 3.1 Three-destination fee

`fee = gasUsed × baseFee` remains the total a user pays. Nitro records poster gas as receipt `gasUsedForL1` and computes `computeGas = gasUsed - posterGas`. Infrastructure receives `computeGas × min(baseFee, minBaseFee)`, the network account receives `computeGas × (baseFee - min(baseFee, minBaseFee))`, and the L1 pricer funds pool receives `posterGas × baseFee`. The three destinations sum exactly to the total. No priority fee is collected; higher bids don't reorder anything.

Source of truth: Nitro `arbos/tx_processor.go`, where `FillReceiptInfo` records `posterGas` as `GasUsedForL1` and `EndTxHook` performs the three transfers. Robinhood block 1,000,000 is a nonzero vector: `gasUsed=422716`, `posterGas=767`, `baseFee=20036000`, and `minBaseFee=20000000`. It yields `computeGas=421949`, total `8469537776000` wei, infrastructure `8438980000000` wei, network `15190164000` wei, and L1 poster `15367612000` wei.

### 3.2 Multi-constraint pricer (the core)

Source of truth: `arbos/l2pricing/model.go`, `updatePricingModelSingleConstraints`.

Each constraint `i` has a target `T_i` (gas/s), an adjustment window `W_i` (s), and a backlog `B_i` (gas). Once per block, with `dt` = seconds since the previous block (0 for most blocks):

```
for each constraint i:
    B_i = max(0, B_i - T_i * dt)               # pay off backlog at the target rate
    x_i = B_i / (T_i * W_i)                    # exponent contribution (bips internally)
x = Σ x_i
baseFee = minBaseFee * P4(x)                   # if x > 0, else minBaseFee
after each tx: B_i += gasUsed - gasUsedForL1   # every constraint absorbs compute gas
```

`P4` is `ApproxExpBasisPoints(x, 4)`, which is **not** `e^x`: it is the degree-4 Taylor polynomial `1 + x + x²/2 + x³/6 + x⁴/24`. Live check on 2026-09-06: backlogs gave `x = 3.24`; `P4(3.24) = 19.8`; observed base fee was 19.99× the floor (0.3997 gwei). True `e^3.24` would be 25.6×. Above `x ≈ 2` the fee grows like `x⁴/24`, i.e. polynomially. The page should say this plainly and plot both curves.

Consequences to illustrate:

- **The 15 s constraint (60 M gas/s)** is the spike engine. A 900 M-gas burst (15 s at 60 M/s) adds `x = 1` on its own. It drains at 60 M/s, so it is usually zero within seconds of demand dropping.
- **The 86 400 s constraint (40 M gas/s)** is the ratchet. `T·W = 3.456 T gas`; each extra 1 T gas of backlog adds `x ≈ 0.29`. If average demand exceeds 40 M gas/s the backlog grows *all day*, and it takes a full day below target to shed one day's excess. This is why the base fee climbed for 11 straight days from Aug 24: sustained demand (~37–40 M gas/s) was above the then-target (18 M, then 30 M, then 40 M).
- **`getGasBacklog()` is misleading**: it returns the legacy backlog (0). The real backlogs are the third element of each `getGasPricingConstraints()` triple.
- **Owner resets**: every `setGasPricingConstraints` call replaces the backlogs with the `startingBacklog` values supplied (0 on Aug 20, 7.49 T on Sep 1, 9.99 T on Sep 3). The fee can therefore jump discontinuously at an owner action.
- Multi-gas (per-resource: compute, storage, history…) constraints exist in the precompile (`getMultiGasBaseFee` returns 9 identical values today) but are not configured. Mention as "not active".

### 3.3 Constraint-change history (recovered from `OwnerActs` logs on ArbOwner `0x…70`)

| Block | UTC | Constraints `[gas/s, window s, starting backlog]` |
|---|---|---|
| 28 (genesis config) | 2026-04-30 20:37 | 6 constraints: [60M,9], [41M,52], [29M,329], [20M,2105], [14M,13485], [10M,86400], all backlog 0 |
| 174,150 | 2026-06-24 20:28 | (min base fee 0.1 → 0.02 gwei) |
| 6,322,119 | 2026-07-10 19:12 | [60M,15,0], [15M,86400,1.106T] |
| 18,424,412 | 2026-07-24 20:08 | [60M,15,0], [20M,86400,2.970T] |
| 41,739,400 | 2026-08-20 21:21 | [60M,15,0], [18M,86400,0] |
| 51,865,079 | 2026-09-01 16:33 | [60M,15,0], [30M,86400,7.492T] |
| 53,578,754 | 2026-09-03 17:08 | [60M,15,0], [40M,86400,9.989T] |

Public launch was 2026-07-01, so the genesis 6-constraint set ran for the first 10 days of mainnet. Full decoded log is in `data/owner-actions.json`. The page renders this as a timeline overlaid on the all-time chart, and the collector must watch for new `OwnerActs` events (selector `0xcc0d556a`, and `0xa0188cdb` for min-fee changes) because the constraint set is expected to keep changing.

### 3.4 Where the fees go

For compute gas, `min(baseFee, minBaseFee)` goes to the infrastructure fee account and the remainder goes to the network fee account. Poster gas is paid at `baseFee` to the L1 pricer funds pool. The separate batch-poster reimbursements and L1 reward are L1-pricer mechanics, not part of the two compute destinations. Show all three L2 base-fee destinations live as a stacked bar, and the sampled account balances as counters (balance drops are withdrawals; annotate rather than hide).

### 3.5 L1 pricer (secondary section, collapsible)

`getL1BaseFeeEstimate` (price per L1 gas unit the pricer currently charges), `getL1PricingSurplus`, `getL1FeesAvailable`, `getL1PricingUnitsSinceUpdate`, `getLastL1PricingUpdateTime`, `getL1PricingEquilibrationUnits` (160 M), `getPerBatchGasCharge` (210 k), `getL1RewardRate` (10 wei). Explain: price adapts so collected L1 fees ≈ batch-posting cost; with blobs and brotli the cost per tx is tiny, so the price has converged near zero and the surplus is small (0.00019 ETH). Sampled every minute; no history before the collector started.

### 3.6 Internal transactions: `startBlock` and batch posting reports

Every L2 block's first transaction is an ArbOS internal tx (type `0x6a`, to `0x…0a4b05`):

- **`startBlock(l1BaseFee, l1BlockNumber, l2BlockNumber, timePassed)`**, selector `0x6bf6a42d`. `timePassed` is the `dt` the pricer uses, and it equals the header timestamp delta, so headers alone give exact replay inputs. `l1BaseFee` is reported as **0** by the sequencer on this chain (also visible in the sequencer feed), which is worth a footnote on the page.
- **`batchPostingReportV2(batchTimestamp, batchPosterAddress, batchNumber, batchCalldataLength, batchCalldataNonZeros, batchExtraGas, l1BaseFeeWei)`**, selector `0x9998269e`. One per L1 batch, delivered through the delayed inbox ~10 minutes after the batch lands on Ethereum, in bursts of a dozen or so. Each lives in its own 2-transaction block (startBlock + report), which is how to find them cheaply: filter headers for blocks with exactly two tx hashes, then fetch only those with full transactions. Observed cadence: a batch every 12–24 s, ~5,700/day, batch numbers ~201,900 on 2026-09-06.

What the report and its effective ArbOS state give:

- The **batch-posting cost ArbOS attributes to the batch**, not the batch poster's Ethereum receipt total. V1 reports use signed saturating addition of `perBatchGasCharge` and the reported `batchDataGas`. V2 reports start with calldata gas (4/16 per byte), keccak overhead, and two storage writes, then add `batchExtraGas` and the nonnegative effective `perBatchGasCharge`. ArbOS 50+ takes the maximum of that amount and `parentGasFloorPerToken × (calldataLength + 3 × calldataNonZeros + 172) + 21,000`. `l1BaseFeeWei × gasSpent` is the attributed wei cost. Example: 137 calldata bytes, 113 non-zero, extra gas 27,132, a 210 k per-batch charge, parent floor 10, and L1 base fee 0.0735 gwei produce 279,096 gas and 0.000020513556 ETH per batch, about 0.117 ETH/day at 5,700 batches.
- **Ethereum base fee history as seen by the chain**, batch cadence, blob usage proxy (`batchExtraGas`), and the batch poster address.
- The trigger for every L1-pricer update (`UpdateForBatchPosterSpending`), so `getLastL1PricingUpdateTime` jumps can be tied to specific batches.

The collector takes the ArbOS version from each block header and persists the report version, effective per-batch charge, parent floor, and calculation version with the raw report fields. That is enough to recompute an attributed cost after future calculation fixes. A truncated owner scan uses an archive state snapshot at its origin when available. Without one, it uses a current pinned snapshot only across ranges with no intervening parameter setter and refuses to guess across a setter.

Page use: an "L2 fees vs. ArbOS-attributed batch-posting cost" panel, batch cadence sparkline, and the L1 base fee series. Backfill cost: all headers (already planned) plus ~5,700 full-block fetches per day of history.

---

## 4. Page layout (single scrolling page)

1. **Hero / live strip** (updates every ~1 s)
   - Base fee now (gwei) and multiplier over floor; sparkline of last 120 blocks.
   - Block number, seconds since last block, gas in last block, rolling gas/s (10 s and 60 s).
   - Cost of a 21 k-gas transfer and of a ~150 k-gas swap in ETH and USD (USD optional; needs an external price feed → make it a toggle, off by default).
   - "Where the fee goes" mini stacked bar (floor vs. congestion).
2. **The pricer, live**: one card per constraint: target, window, backlog (gas and "seconds of target"), `x_i`, and a fill-gauge that visibly drains at `T_i`/s between polls (client-side interpolation, corrected on each poll). Sum → `x` → `P4(x)` → base fee, drawn as an equation with live numbers.
3. **Explainer**: the formula, the Taylor-vs-exp plot, the "burst vs. sustained" animation (two synthetic demand profiles replayed through the simulator in §6), no-tips/FCFS note.
4. **History**: tab bar `1h · 24h · 30d · All` above a linked chart group:
   - base fee (log scale), with floor line;
   - stacked per-constraint contribution to `x`;
   - gas/s vs. each constraint's target;
   - backlog per constraint;
   - owner actions as vertical markers with tooltips (constraint changes, min-fee change).
   A range brush shares x-axis across the group. Hover shows the exact numbers.
5. **Fee flows**: balances of infra/network accounts over time (from collector samples), plus estimated fees/day computed from `Σ gasUsed × baseFee` per block.
6. **L1 pricer** (collapsed by default).
7. **Data & method** footer: what is live, what is reconstructed, last collector sample time, RPC health (calls/min, last 429).

Visual rule: green-to-red is not a fee scale; use a single sequential ramp for "multiplier over floor". Everything must be legible on dark and light.

---

## 5. Data sources by resolution

| Series | 1h | 24h | 30d | All-time | Source |
|---|---|---|---|---|---|
| Base fee per block | every block | 1-min buckets (min/avg/max) | 15-min buckets | 1-h buckets | `eth_feeHistory` (browser for the tail, collector for the rest) |
| Gas used per block, gas/s | every block | 1-min | 15-min | 1-h | `eth_getBlockByNumber(n,false)` batched (collector) |
| Backlog / `x_i` per constraint | every block | 1-min | 15-min | 1-h | **Replay** (§6), anchored to live `getGasPricingConstraints` samples |
| Constraint set | — | — | — | full | `eth_getLogs` on `0x…70` OwnerActs (collector, every 5 min) |
| L1 pricer, balances | 5 s live | 1-min samples | 15-min | 1-h | precompile / `eth_getBalance` (collector only; no backfill possible) |
| L1 batch cost, cadence, Ethereum base fee | per batch | per batch | 15-min | 1-h | `batchPostingReportV2` internal txs in 2-tx blocks (collector; fully backfillable) |

Volumes: 1h ≈ 38 k blocks, 24h ≈ 0.9 M, 30d ≈ 27 M, all-time ≈ 56 M and growing 0.9 M/day.

---

## 6. Architecture

Decision (2026-09-06): **the browser never calls an RPC.** All data, live and historical, comes from the gascurve backend. The collector is the single RPC client per network, so the public endpoint sees one well-paced IP regardless of how many viewers there are.

```
public RPC ──(≤4 calls/s, batched, one IP)──▶ collector (Go) ──▶ PostgreSQL ──▶ api (Go, REST + WebSocket) ──▶ web (Next.js)
```

- **collector**: per-network follower; 1 s tick samples head + precompile state, fetches missing headers in ≤100-item batches, replays the pricer with re-anchoring to sampled backlogs, folds buckets, reads OwnerActs logs and batch posting reports, runs a resumable backfill, emits `NOTIFY gascurve_live`.
- **api**: serves `Series`, `LiveSnapshot`, constraint history and the rest over REST, fans the live snapshot out to WebSocket clients (`/api/v1/ws`).
- **web**: renders the page, subscribes to the WebSocket, polls `/live` as a fallback.

The full contract (schema, endpoints, JSON shapes, WS protocol) lives in [ARCHITECTURE.md](ARCHITECTURE.md). The earlier browser-polls-RPC design is superseded; its rate-limit findings still drive the collector budget.

Repo: `tirante-dev/gascurve`, single repository, Go collector + api, Next.js web, released together.

## 7. Replay: reconstructing per-constraint backlogs

Inputs: per-block `(timestamp, gasUsed)`, receipt `gasUsedForL1`, the constraint set in force (with its starting backlogs) from `owner-actions.json`, the min base fee history.

```
state = startingBacklogs at the last owner action ≤ block
for block in order:
    dt = ts - prevTs
    for i: B_i = max(0, B_i - T_i*dt); x_i = B_i/(T_i*W_i)
    predictedBaseFee = minFee * P4(Σx_i)
    record B_i, x_i, predictedBaseFee, header.baseFee
    for i: B_i += gasUsed - gasUsedForL1      # Nitro compute gas
```

Known error sources, all to be surfaced in the "Data & method" footer:

1. Receipt completeness: the collector validates every block receipt set and refuses to treat missing `gasUsedForL1` as zero. Historical destination accounting remains unknown until the receipt-backed replay replaces that bucket.
2. Owner actions inside a block: the replay uses the log's transaction index and the action transaction's receipt. Gas before the action transaction is added to the old backlogs, then the reset is applied, then the action transaction and later gas are added to the reset backlogs.
3. Old headers without state: the decomposition before the collector's start is a pure replay validated only through base fee agreement. Because the two constraints have very different time constants, an alternative estimator exists: `x_24h ≈ rolling 60-s minimum of x` (the 15-s backlog drains to zero within seconds whenever demand < 60 M gas/s). Use it as a cross-check.
4. Blocks with equal timestamps get `dt = 0`; that is how ArbOS behaves too (`startBlock.timePassed` is the timestamp delta), so no correction and no interpolation is needed as long as per-block headers are used.

---

## 8. Risks and open questions

- **Collector budget on the public RPC**: the limiter counts calls, and exact reconstruction needs one header call plus one receipt call per block. Public endpoints are paced sequentially, so deep recomputation can take substantially longer than wall-clock history. Use a dedicated archive endpoint for a timely full repair, or run a Nitro follower node.
- **Limiter semantics** are inferred, not documented. Log every 429 with timestamp and calls-in-last-10-s so the real window can be fitted from data.
- **Constraint set changes** must be handled without redeploy: the page reads the constraint set from `meta.json` and the live call, renders N cards, and shows a "parameters changed at block …" banner within a minute.
- **ArbOS upgrades** could change `P4` or the model (multi-gas constraints are already in the code). Pin the model to ArbOS version; the collector alerts if `arbOSVersion()` changes.
- **USD pricing** needs an external API; keep optional.
- **Fee-account balances** are a proxy for fees collected; withdrawals break it. Prefer `Σ gasUsed × baseFee` from headers for "fees/day", use balances only as a live counter.

---

## 9. Milestones

1. `shared/pricer.ts` + unit tests against recorded live values. Browser-only page: hero strip, live constraint cards, 1h chart from `eth_feeHistory`, tail fill, back-off. (Ships without a collector; 24h+ tabs disabled.)
2. Collector: follow head, state samples, replay with re-anchoring, 24h rollup, publish to R2. Enable 24h tab.
3. Backfill 30 days of headers + all-time base fee; 30d and All tabs; owner-action timeline; explainer section with Taylor plot and burst-vs-sustained simulation.
4. Fee flows + L1 pricer sections; rate-limit telemetry; polish for mobile.

---

## Appendix A — RPC cheat sheet

Precompiles: ArbGasInfo `0x…6C`, ArbOwnerPublic `0x…6B`, ArbSys `0x…64`, ArbAggregator `0x…6D`, ArbOwner `0x…70` (logs only).

```
ArbGasInfo.getGasPricingConstraints()  → uint64[3][]   [target gas/s, window s, LIVE backlog]
ArbGasInfo.getPricesInWei()            → (perL2Tx, weiPerL1CalldataByte, weiPerL2Storage,
                                           perArbGasBase, perArbGasCongestion, perArbGasTotal)
                                           NB: docs list this tuple in the wrong order; this is nitro's order.
ArbGasInfo.getMinimumGasPrice()        → wei
ArbGasInfo.getL1BaseFeeEstimate()      → wei per L1 gas unit (price the L1 pricer charges)
ArbGasInfo.getL1PricingSurplus()       → int256 wei
ArbGasInfo.getL1FeesAvailable / getL1PricingUnitsSinceUpdate / getLastL1PricingUpdateTime
ArbOwnerPublic.getNetworkFeeAccount / getInfraFeeAccount
ArbSys.arbOSVersion()                  → 55 + ArbOS version
OwnerActs topic0 = 0x3c9e6a772755407311e3b35b3ee56799df8f87395941b3a658eee9e08a67ebda
  method selectors: setGasPricingConstraints 0xcc0d556a, setMinimumL2BaseFee 0xa0188cdb,
  setSpeedLimit 0x4d7a060d, setL1PricePerUnit 0x2b352fae, scheduleArbOSUpgrade 0xe388b381
eth_feeHistory(count ≤ 1024, newest, [])  → baseFeePerGas exact; ignore gasUsedRatio
eth_getBlockByNumber(n, false)            → timestamp, gasUsed, baseFeePerGas, l1BlockNumber
internal tx (type 0x6a) selectors        → startBlock 0x6bf6a42d, batchPostingReportV2 0x9998269e
```

## Appendix B — Live snapshot used for the worked example (2026-09-06 07:20 UTC)

```
constraints      [60,000,000, 15, 3,111,506]  [40,000,000, 86,400, 11,194,391,810,886]
x                0.00346 + 3.2391 = 3.2426
P4(x)            19.79      (e^x would be 25.6)
minBaseFee       0.02 gwei  → predicted 0.3958 gwei, observed 0.3997 gwei
gas/s (recent)   ~37–40 M   (24h target 40 M → backlog roughly flat; 24h backlog ≈ 78 h of target)
fee split        0.02 gwei → infra account, 0.38 gwei → network account (95%)
L1 price/unit    0.0024 gwei, gasUsedForL1 = 0 on a normal 68-byte-calldata tx
```
