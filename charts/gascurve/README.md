# gascurve Helm chart

Deploys the three gascurve components: one collector (single replica, the only RPC client), the API, and the web frontend. Schema migrations run as an init container on the collector and api pods.

A database is required whenever the collector or the api is enabled: set exactly one of `database.url` and `database.existingSecret`. `values.schema.json` rejects an install, upgrade or template that sets neither or both, so the default values alone do not install.

`values.schema.json` models the whole `config` tree, not just the chart's own keys, because a value the chart waves through and the binary then rejects only shows up as a crash-looping pod. Intervals must be positive Go durations (`500ms`, `3s`, `720h`), `block_retention` must also be at least `1h` (the widest bucket resolution, since row-backed buckets are rebuilt from the rows inside them and a window has to outlive its rows; the schema cannot compare duration magnitudes, so the binary enforces this one), `calls_per_second` is `0` for a dedicated node or a real budget from `0.1` to `10000`, `ws_url` must be `ws://` or `wss://` while `rpc_url` must be `http://` or `https://`, `eth_usd_source` is `""`, `coinbase`, `coingecko` or an `https://` URL, and chart-owned objects reject keys they do not know, so a field put at the wrong level fails the install instead of being mounted and ignored. Ingress needs a backend: enabling it with both `api.enabled` and `web.enabled` false is refused rather than rendering an Ingress with no paths. An ingress that routes to the API also requires at least one valid address or CIDR in `config.server.trusted_proxies`.

The one rule the schema cannot express is the cross-reference between a network with no `rpc_url` and a `NETWORK_<NAME>_RPC_URL` in the collector's environment; the templates check that, for fallback endpoints too. A literal override with an empty value does not count: `internal/config` ignores an empty environment override, so the schema rejects that shape outright.

```bash
helm install gascurve oci://registry.ahkc.win/gascurve/charts/gascurve \
  --set database.url='postgres://user:pass@postgres:5432/gascurve?sslmode=disable' \
  --set ingress.enabled=true --set ingress.host=gascurve.com \
  --set 'config.server.trusted_proxies[0]=10.244.0.0/16'
```

## Values

| Key | Description | Default |
|---|---|---|
| `image.registry` | Registry prefix for all three images | `registry.ahkc.win/gascurve` |
| `image.tag` | Image tag, defaults to the chart's appVersion | `""` |
| `collector.enabled` | Run the collector (always 1 replica) | `true` |
| `api.replicaCount` / `web.replicaCount` | Replicas | `2` / `2` |
| `ingress.enabled`, `ingress.host`, `ingress.tls` | One host: `/` to web, `/api` to api. Needs at least one of `api.enabled` and `web.enabled` | `false`, `gascurve.com` |
| `config.server.trusted_proxies` | Addresses or CIDRs of reverse proxies allowed to supply the client forwarding chain. Required when ingress and api are enabled | `[]` |
| `api.networkPolicy.enabled` | With chart-managed ingress, restrict API pod ingress to the configured controller peers | `true` |
| `api.networkPolicy.allowedPeers` | Kubernetes NetworkPolicy peers allowed to reach the API. Defaults to the standard ingress-nginx controller labels | ingress-nginx controller |
| `api.networkPolicy.monitoringPeers` | Extra peers admitted for `/metrics`, which shares the API's HTTP port. Required when `metrics.serviceMonitor.enabled` is true and the policy renders | `[]` |
| `collector.resources` | Portable defaults, not a production profile. See Sizing the collector | `100m` / `128Mi` requested |
| `database.url` | Rendered into a chart-managed Secret. Exactly one of `database.url` and `database.existingSecret` is required when the collector or api is enabled | `""` |
| `database.existingSecret`, `database.existingSecretKey` | Use an existing Secret instead of `database.url` | `""`, `DB_URL` |
| `migrations.enabled` | Run `gascurve-migrate up` as an init container on the collector and api pods | `true` |
| `collector.securityContext`, `api.securityContext`, `web.securityContext`, `migrations.securityContext` | Numeric `runAsUser`/`runAsGroup`, must match the image's `USER` | `65532` (Go images), `1001` (web) |
| `config` | Rendered to `config.yaml` (networks, collector pacing, CORS) | see values.yaml |
| `collector.extraEnv`, `api.extraEnv`, `migrations.extraEnv` | Extra env for that container only. Private RPC URLs belong in `collector.extraEnv` | `[]` |
| `extraEnv` | Deprecated alias applied to all three. Kept so existing installs keep working; move entries to the component lists | `[]` |
| `config.collector.metrics_port` | Port for the collector's `/metrics` and health server. `0` disables it, its probes, Service and ServiceMonitor | `9090` |
| `metrics.serviceMonitor.enabled`, `metrics.prometheusRule.enabled` | Prometheus operator objects. See Metrics and alerts | `false`, `false` |

The web image is built with `NEXT_PUBLIC_API_URL=/api/v1`, so it talks to the API through the same host. Put the API on another host only if you rebuild the image with a different value.

## Ingress client addresses

The REST token bucket and WebSocket connection cap are both per client address. Kubernetes ingress normally makes the ingress controller pod the API's direct peer, so without proxy configuration every visitor shares the controller's limits. For an ingress deployment that includes the API, set `config.server.trusted_proxies` to the addresses or CIDRs the API pods actually see for the ingress controller and any other trusted proxy hops:

```yaml
config:
  server:
    trusted_proxies:
      - 10.244.0.0/16
```

The example is only a placeholder. Use your cluster's controller or proxy ranges. Include every trusted hop that can appear on the right side of `X-Forwarded-For`. `0.0.0.0/0`, `::/0` and any other `/0` are refused, because either one would let any reachable peer choose its rate-limit identity. Entries are validated against exactly what `net.ParseIP` and `net.ParseCIDR` accept, so a leading zero in an octet (`01.2.3.4`) or a partial IPv6 address (`:::`) is refused here rather than at runtime, where a single unparseable entry discards the whole list and silently restores the shared bucket this setting exists to avoid.

The API ignores `X-Forwarded-For` and `X-Real-IP` unless the direct peer is trusted. It then walks `X-Forwarded-For` from right to left past trusted proxy hops and uses the first untrusted address. This preserves the safe behavior for direct or untrusted traffic while giving each user behind ingress a separate REST bucket and WebSocket connection count.

Trusting a pod CIDR by itself is not an access boundary because other pods may share that range. When chart-managed ingress and the API are enabled, the chart therefore creates an ingress-only NetworkPolicy for the API pods. Its default peer selects the standard ingress-nginx controller:

```yaml
api:
  networkPolicy:
    enabled: true
    allowedPeers:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: ingress-nginx
        podSelector:
          matchLabels:
            app.kubernetes.io/name: ingress-nginx
            app.kubernetes.io/component: controller
```

Change the namespace and pod selectors for another ingress installation. `ipBlock` peers are also accepted when a controller cannot be selected by labels. The policy only restricts ingress to the API pods; database and RPC egress are unchanged. Your cluster must use a networking implementation that enforces Kubernetes NetworkPolicy. Set `api.networkPolicy.enabled=false` only when an equivalent policy outside the chart already prevents direct access to the API Service or pods.

The API serves `/metrics` on the same port as REST and WebSocket, so this layer 4 policy also blocks direct API ServiceMonitor scrapes. A blocked scrape is not quiet: `up` goes to 0 and this chart's own `apiDown` alert pages against a healthy API. The chart therefore refuses to render `metrics.serviceMonitor.enabled: true` alongside this policy until `api.networkPolicy.monitoringPeers` names the Prometheus pods:

```yaml
api:
  networkPolicy:
    monitoringPeers:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: mon
        podSelector:
          matchLabels:
            app.kubernetes.io/name: prometheus
```

Those peers are appended to the policy's `from` list and can reach every API route, not only `/metrics`. Keep their addresses outside `config.server.trusted_proxies` so their forwarding headers stay untrusted. If the ranges overlap, or a peer selector cannot single Prometheus out, leave `metrics.serviceMonitor.enabled` off and use an equivalent path-aware policy or a trusted metrics proxy instead. Collector scraping is unaffected because it has a separate metrics port and is not selected by this policy.

`config.server.trusted_proxies` is enforced only for chart-managed ingress, because that is the only proxy this chart can see. An API reached through a Cloudflare Tunnel, a Gateway, or an Ingress owned by something else has exactly the same problem and none of the checks: the tunnel or gateway pod is the API's direct peer, so every visitor shares one bucket and one WebSocket cap. Set the value there too, to the addresses the API pods see for that proxy, and pair it with a policy that stops other pods in the same range from reaching the API. The install notes warn when the API is enabled, the chart renders no Ingress, and the list is empty.

Upgrades of an existing release with both ingress and API enabled must add `config.server.trusted_proxies` before this chart version will render. Confirm the controller peer range and labels first, then apply the Helm upgrade. A wrong CIDR leaves forwarded headers ignored, and wrong NetworkPolicy selectors block ingress traffic. No database migration or data recomputation is involved.

## Health and collector metrics

The collector listens on `config.collector.metrics_port` (9090 by default) inside its pod. Kubernetes uses `/startup` until the database setup is complete and the follower loops launch, `/health` for shallow process liveness, and `/ready` for database connectivity plus a fresh fast loop for every enabled network. A loop counts as failing only after three consecutive errors with no success in between, so a single 429 from a metered public RPC cannot flap the pod out of readiness. RPC or database outages therefore remove readiness but do not cause the singleton collector to restart forever. Setting the port to `0` explicitly disables the server and the chart-managed probes.

`/metrics` exposes Prometheus text for the heartbeat, per-loop success, error and duration, observed and indexed heads, block lag, queued gap count, age and blocks, RPC calls, errors, rate limits and latency, and database latency and errors. The chart's headless collector Service supports Prometheus discovery without exposing the endpoint publicly. It can also be inspected directly:

```bash
kubectl port-forward pod/<collector-pod> 9090:9090
curl http://127.0.0.1:9090/metrics
```

The public API keeps `/health` as shallow process liveness and `/ready` as its database check. `/api/v1/status` reads the collector's durable heartbeat and loop telemetry from PostgreSQL and reports `degraded` with reasons when collection is stale or failing. This upgrade needs no schema migration and no data recomputation.

## RPC URLs and Secrets

The collector is the only component that talks to an RPC, so RPC credentials must reach the collector and nothing else. `collector.extraEnv` is the place for them:

```yaml
collector:
  extraEnv:
    - name: NETWORK_ROBINHOOD_RPC_URL
      valueFrom:
        secretKeyRef:
          name: gascurve-rpc
          key: robinhood
config:
  networks:
    - name: robinhood
      chain_id: 4663
      enabled: true
      # rpc_url omitted on purpose: the Secret above supplies it.
```

`NETWORK_<NAME>_RPC_URL` (name upper-cased, dashes to underscores) overrides `config.networks[].rpc_url`, so the plaintext URL can be left out of the ConfigMap entirely. Every network the collector runs needs a primary URL from one of the two places: the chart refuses to render an enabled network that has neither, with a message naming the network and the environment variable. Disabled networks and installs without the collector are not checked.

The three component lists are separate on purpose. Before this split, one top-level `extraEnv` was copied into the collector, the api, and both migrate init containers, so a private RPC credential also sat in the public-facing api pod. The top-level `extraEnv` still works and still goes to all three, and NOTES prints a warning while it is set; a component entry with the same name replaces the top-level one. Do not put RPC credentials there.

Upgrading from a chart that only had the top-level `extraEnv`: move each entry to the component that needs it, most often

```yaml
# before
extraEnv:
  - name: NETWORK_ROBINHOOD_RPC_URL
    valueFrom:
      secretKeyRef: { name: gascurve-rpc, key: robinhood }

# after
collector:
  extraEnv:
    - name: NETWORK_ROBINHOOD_RPC_URL
      valueFrom:
        secretKeyRef: { name: gascurve-rpc, key: robinhood }
```

## Metrics and alerts

Both binaries serve Prometheus metrics at `/metrics` whatever this chart is told: the api on `config.server.port`, next to the REST API and the WebSocket, and the collector on `config.collector.metrics_port` (9090) next to its health routes. Metrics read the registry alone, so a follower stuck on an RPC call or on the database still gets scraped. `docs/ARCHITECTURE.md` §9 lists every series and its labels; no endpoint URL is ever a label value, because endpoint URLs carry keys.

The Ingress does not expose either endpoint: it routes `/api` to the api and everything else to web, so `/metrics` is reachable from inside the cluster only.

Whenever the collector is enabled and `config.collector.metrics_port` is not `0`, the chart renders a headless Service for it (`<release>-collector`, port `metrics`), whether or not the operator objects are on, so a plain `scrape_config` can find it without the Prometheus operator. That Service sets `publishNotReadyAddresses`, so the readiness drop an RPC or database outage causes does not also take the pod out of the scrape: the metrics that explain the outage keep flowing. Setting `metrics_port: 0` takes the collector's server, probes, Service and ServiceMonitor away together. What is opt in is the operator's own objects, which need the `ServiceMonitor` and `PrometheusRule` CRDs:

```bash
helm upgrade gascurve … \
  --set metrics.serviceMonitor.enabled=true \
  --set metrics.serviceMonitor.labels.release=kube-prometheus-stack \
  --set metrics.prometheusRule.enabled=true \
  --set metrics.prometheusRule.labels.release=kube-prometheus-stack
```

Most operators only pick up objects carrying a label their selector asks for. Find yours with:

```bash
kubectl get prometheus -A -o jsonpath='{range .items[*]}{.spec.serviceMonitorSelector}{"\n"}{.spec.ruleSelector}{"\n"}{end}'
```

`metrics.prometheusRule.alertLabels` is added to every alert, for Alertmanager routing (`--set metrics.prometheusRule.alertLabels.team=platform`).

### Alerts

Each alert can be switched off on its own, and its window, threshold and severity changed, under `metrics.prometheusRule.alerts.<name>`. `enabled`, `for` and `severity` are always required. `threshold` is required by the alerts whose expression compares against a number — `collectorLagDedicated`, `collectorLagPublic`, `collectorSampleStale`, `collectorRateLimited` and `apiErrorRate` — because a rule rendered without one is PromQL the operator rejects, and the alert would then be missing rather than misconfigured. The other four compare nothing and refuse a `threshold` outright, so a number put on the wrong alert fails the install instead of being silently ignored.

| Alert | Default | Fires when |
|---|---|---|
| `collectorLagDedicated` | `> 30s` for `5m`, warning | a network on a dedicated endpoint falls behind |
| `collectorLagPublic` | `> 180s` for `15m`, warning | a network on a public RPC falls behind |
| `collectorSampleStale` | no sample for `300s`, held `2m`, critical | the fast loop is not sampling at all |
| `collectorRateLimited` | `> 0.2` events/s for `15m`, warning | an endpoint keeps throttling the collector |
| `collectorEndpointsExhausted` | `5m`, critical | every endpoint of a network is disabled |
| `collectorDown` | `5m`, critical | no collector target is up, whether it failed or was removed |
| `apiDown` | `5m`, critical | no api replica is up, whether they failed or were removed |
| `apiErrorRate` | `> 5%` 5xx for `10m`, warning | the api is failing requests |
| `apiListenerDown` | `3m`, warning | a replica has lost its PostgreSQL LISTEN feed, so its WebSocket clients go stale |
| `databaseUnreachable` | `3m`, critical | `/ready` answers 503 while every listener is ready, which leaves the database ping |

Lag is the one figure that needs two rules. A network with a dedicated endpoint normally sits at 0 to 2 seconds; one followed over a public RPC at its documented 4 calls per second normally sits at 20 to 60 seconds, because a tick costs about five calls and the catch-up gets what is left. A single threshold would either page constantly on the public networks or never fire on the dedicated one. `metrics.prometheusRule.dedicatedNetworks` is a regular expression on the `network` label that splits the fleet; it defaults to `robinhood` and must be widened when more networks move onto dedicated nodes.

Every rule is scoped to both the job and the namespace of this release's targets: `job="<release>-collector"` or `job="<release>-api"` (the job an operator derives from a ServiceMonitor is the Service name) and `namespace="<release namespace>"`. The namespace matters because the same release name in two namespaces produces the same job, and without it a healthy staging install would dilute prod's error rate and mask its endpoint exhaustion. Scraping through a hand-written `scrape_config` with a different job name therefore means no alert fires: either name the job after the Service, or set `metrics.prometheusRule.enabled: false` and write the rules yourself.

Every alert also carries `job` and `namespace` as literal labels, because the expression cannot always be relied on for them: `absent()` takes labels from a bare selector and not from a comparison, `sum()` drops every label, and `min by (network, chain_id)` keeps only those two. Without them two releases would fire alerts Alertmanager cannot tell apart and would merge into one.

The two "not being scraped" alerts are `absent(up{...} == 1)`, not `up{...} == 0`. `up == 0` matches only a target that still exists and failed its scrape, so it goes quiet exactly when service discovery removes the target altogether, which is the outage worth paging for. `absent(up == 1)` fires for both, and for the api it means what its description says: no replica is up, rather than any one replica being down. The same property is why each group is rendered only for a component this release actually scrapes, on the same condition as its ServiceMonitor: the collector group needs `collector.enabled` and a non-zero `config.collector.metrics_port`, the api group needs `api.enabled`. A rule kept for a component that was never deployed would page for ever, where `up == 0` was merely inert.

`collectorDown` and `apiDown` are also the backstop for a component that never starts: an api that cannot reach PostgreSQL during a rollout never binds its port, so `databaseUnreachable`, which counts `/ready` answering 503, cannot see it and `apiDown` is what fires.

## Migrations

Each collector and api pod runs `/app/gascurve-migrate up` in an init container from the same image as its main container, so the schema is always at the version the binary expects. There is no Helm hook, so nothing runs before the ServiceAccount, ConfigMap, and Secret exist, and Argo CD needs no hook or sync-wave configuration. golang-migrate takes a PostgreSQL advisory lock, so pods that start at the same time queue on the lock rather than racing. A failed migration keeps the pod in `Init:Error`; inspect it with `kubectl logs <pod> -c migrate`. Set `migrations.enabled=false` if migrations are applied elsewhere.

What a slow or failed migration means for each component differs:

- **api** (RollingUpdate): the previous ReplicaSet keeps serving until the new pods pass their init container and readiness probe, so the API stays available during the migration and during a failed upgrade.
- **collector** (Recreate, single replica): the old collector is stopped before the new pod starts, and the new pod's migration runs after that, so collection pauses for the duration of the migration and stays paused for as long as a migration fails. This gap is the price of the single-writer rule (two collectors would double the RPC budget and race on the same rows). Keep migrations small, and if one fails roll back with `helm rollback` after repairing the schema state (golang-migrate refuses to run on a dirty version). The collector backfills the blocks it missed once it is running again.

## Rolling pods on Secret changes

The collector and api pod templates carry `checksum/config` and, when the chart renders the database Secret from `database.url`, `checksum/db-secret`, so changing either value rolls the pods on upgrade. Secrets the chart does not manage are not tracked: after rotating `database.existingSecret` or any Secret referenced from `collector.extraEnv`, `api.extraEnv`, `migrations.extraEnv` or the deprecated top-level `extraEnv`, run

```bash
kubectl rollout restart deploy/<release>-collector deploy/<release>-api
```

or use a reloader controller. Containers read Secret-backed environment variables only at startup.

## Sizing the collector

The chart's defaults (`collector.resources.requests` of `100m` CPU and `128Mi`) are portable defaults: enough for one or two networks at the default 3 second tick once the backfill has caught up, and small enough that the chart installs on a laptop cluster. They are not a production profile, and the difference is not small.

A production collector is a different workload in three ways at once:

- it follows **every** enabled network in one process, so the default four networks are four follower loops, not one;
- on first install each of those replays `collector.backfill_depth` (`720h` by default) as fast as its budget allows, which is the busiest the process ever is and the longest it stays busy;
- a dedicated endpoint (`calls_per_second: 0`) removes the pacer entirely, so the loop runs at whatever rate the node and the database can sustain rather than at four calls per second.

Raise the **requests**, not only the limits. Requests are what the scheduler reserves and what decides the CPU share when a node is contended, so a collector left at `100m` competes for CPU like a sidecar exactly while it is doing the most work, and the backfill stretches out instead of finishing. A reasonable starting point for the default four networks with one unmetered endpoint is `500m` to `1` CPU and `512Mi` of memory requested, with limits at roughly twice that; `charts/gascurve/values.yaml` carries the same numbers as a commented example, and `ci/homelab-values.yaml` shows the single-network variant.

Treat those numbers as a starting point to be replaced by measurements from your own cluster, not as a tested profile: the right values depend on how many networks are enabled, whether their endpoints are metered, and how far behind the collector starts. Watch four things while the first backfill runs and settle the requests afterwards:

- CPU throttling on the collector container (`container_cpu_cfs_throttled_seconds_total`): sustained throttling means the limit is too low;
- resident memory against the request and the limit, so an OOM kill during backfill is caught before it happens;
- backlog depth, how far behind the chain head each follower is, and whether it is shrinking;
- tick latency: a fast loop that takes longer than `tick_interval` is the signal that the process, not the endpoint, has become the bottleneck.

The collector is a single replica by design (it is the only RPC client and the only writer), so it scales vertically only. There is no horizontal option to fall back on.

## Full-chain history

`config.collector.backfill_depth` is how far back the owner-action scan and the resumable backfill reach. It takes a duration (`720h`, the default, is thirty days) or the word `genesis`, which reaches the first block of the chain however old it is:

```yaml
config:
  collector:
    backfill_depth: genesis
```

A zero or negative duration is refused at load, so a stray value cannot start a nine-day replay or silently mean no history at all.

**Storage is not the constraint.** Pruning removes only block rows and raw state samples; buckets are never deleted once written. A whole 57-million-block Arbitrum Nitro chain is on the order of 200 thousand bucket rows across the 1m, 15m and 1h resolutions, roughly 100 to 150 MB.

**Time is.** Two phases, in order:

1. The owner-action scan sweeps `eth_getLogs` from the first block to the head before the backfill starts any segment, because the historical constraint sets and minimum fees are unknown until it finishes. On a 57M-block chain against an unmetered in-cluster node that is tens of minutes, spread over many slow ticks since the scan yields to the live path between chunks. It logs its position every thirty seconds and `/status` reports `slow loop on its first pass: scanning owner actions, N of M blocks` for as long as it runs, so it can be told from a hang. The network is degraded until it completes.
2. The backfill then walks the chain backwards. At the roughly 70 blocks per second an unmetered archive node sustains, 57M blocks is around nine days. `gascurve_collector_backfill_cursor_block` and `gascurve_collector_backfill_floor_block` are what to watch; the gap between them, over time, is the rate.

**Set `archive: true`** on an endpoint that serves historical `eth_call` before asking for genesis. A chain with a genesis `setGasPricingConstraints` set (`robinhood`, `arbitrum-one`, `arbitrum-sepolia`) replays from that set and needs nothing else. A legacy chain (`robinhood-testnet` has no constraint sets at all) has nothing to replay from, so the collector samples the state at block 1 and records it as the scan origin; the replay then starts at block 2, since block 1's own state is what the sample describes. Without an archive endpoint nothing below the earliest recorded constraint set can be priced: that range is reported as missing rather than guessed, and on a legacy chain the depth buys nothing at all.

**Changing the depth later** no longer needs the database dropped. Widening it (a longer duration, or `genesis`) lowers the floor on the next start, extends the owner scan down to meet the new cutoff and resumes the backfill below what it had already built. A shorter duration is logged and otherwise ignored, because it is also what a window measured back from a growing head looks like and honouring it would hand history back on every restart of an unchanged values file. Every one of those decisions is logged at info, so `kubectl logs` says what a redeploy did.

**Backing out of a walk** is `hold`, the word that says a narrowing deliberately:

```yaml
config:
  collector:
    backfill_depth: hold
```

It keeps every bucket already reconstructed and abandons the rest of the descent, reporting `done` at the floor it reached. The run in flight finishes first, which is what keeps its blocks from being folded a second time later, so a `genesis` walk three days into its nine stops within one `backfill_window` (10000 blocks by default). Where nothing bounds a run, meaning `backfill_window: 0` or a network with no archive endpoint, a run is a whole constraint set and the hold takes effect at the end of it; the log line says how many blocks that leaves. Nothing is deleted and nothing is lost: put a duration or `genesis` back later and the walk resumes below the held floor. On a chain that has reconstructed nothing yet, `hold` holds at the first live block, which is how a deployment asks for live data and no history at all. Note that `backfill_depth` is collector-wide, so `hold` holds every network the collector follows.

**Reclaiming the history below a shorter depth** is a different thing and a destructive one. Buckets are never deleted by pruning or by a narrowed depth, so holding stops the cost without recovering the storage (which, per above, is not the constraint anyway). To actually discard what is below a shorter depth, set that depth and raise the network's `history_epoch`: the buckets the backfill built, the backfill cursor and the owner-scan checkpoints are dropped and rebuilt to the depth now configured. That is a rebuild rather than a trim, so the retained window is walked again, and the only way back is to backfill the discarded range once more. It reaches reconstructed history only. Buckets the live path collected (from the hour of the first live block on) are never deleted by anything, so a collector that has been running for a year keeps a year of them whatever the depth says. Use `history_epoch` for history you want replaced (after giving a network an archive endpoint, say), never to widen the depth.

**Batch response limits.** A from-genesis backfill walks four times more chain than the default window and is correspondingly more likely to meet a range whose headers plus receipts exceed the node's response cap. The collector halves its batch cap when an endpoint refuses a batch as too large and recovers a step per quiet minute, so it finds the working width on its own. On a self-hosted nitro node, raising `--rpc.max-batch-response-size` above the 10 MB default lets it stay at `header_batch_size: 100` (200 items per batch) and is worth doing before starting a walk this long.

## Releasing

The chart is versioned and released separately from the application. release-please keeps two release PRs open on `main`: one for the application (tag `v<version>`, publishes the three images) and one for the chart (tag `gascurve-chart-v<version>`, packages and pushes the chart to the OCI registry). `Chart.yaml` `appVersion` is what a published chart pulls, and it is the version Helm Publish packages and verifies, so the chart tag and the application version it ships are fixed together at tag time.

### The very first release has an order

The chart ships with `appVersion: "0.0.0"`, a placeholder that means "the application has never been released". Merge the three PRs in this order:

1. **the application release PR** (`chore: release <version>`). It tags `v<version>` and Docker Publish pushes the three images.
2. **the appVersion sync PR** (`fix(helm): update chart app version to <version>`), which the Release Please workflow opens automatically once that release is published. It sets `Chart.yaml` `appVersion` to the version just released.
3. **the chart release PR** (`chore: release gascurve-chart <version>`), which release-please opens because of the commit in step 2. It tags `gascurve-chart-v<version>`, and the chart tagged there records a real `appVersion`.

Merging the chart PR first is the one ordering that produces a broken artifact: the chart tag freezes `appVersion: 0.0.0`, and a chart that cannot say which images it deploys is not publishable. Helm Publish refuses that tag by name and prints this ordering along with the recovery command, rather than misreporting it as a missing `v0.0.0` release. To recover without re-tagging, dispatch Helm Publish with `version=<chart version>`, `app_version=<the published application version>` and `override_app_version` checked. Only the first chart release needs this; from the second on, step 2 has already run.

Every release after the first follows the same order without anyone thinking about it, because step 2 is what causes step 3 to exist.

### Keeping appVersion current

Automatic, with one repository prerequisite:

- When an application release is published, the Release Please workflow opens the `fix(helm): update chart app version to <version>` PR and enables auto-merge on it. Merging that PR is what makes release-please open the next chart release PR.
- **Allow auto-merge must be enabled** in the repository settings (Settings, General, Pull Requests, "Allow auto-merge"). GitHub rejects `gh pr merge --auto` when the repository setting is off, so without it the sync PR is opened and then sits there, and `appVersion` stays behind until someone merges it by hand. The workflow step prints the PR number and the exact recovery command when this happens.
- Auto-merge still waits for required reviews and required status checks. It does not bypass branch protection.

### The release token

release-please needs a token other than `GITHUB_TOKEN`, so that the release PR gets CI runs and the published release triggers the publish workflows. Either form needs three permissions, all read/write: **Contents**, **Pull requests** and **Issues**.

Issues write is the one that is easy to miss. release-please manages its own `autorelease:*` labels, label management counts as an Issues permission, and without it the very first run in a repository that has no such labels yet fails while creating them.

- **Preferred: a GitHub App** (`RELEASE_APP_ID` + `RELEASE_APP_PRIVATE_KEY`), installed on this repository only. The workflow narrows its token request to exactly those three permissions, but narrowing cannot add anything: the App installation itself must have been granted Contents, Pull requests and Issues read/write by the repository owner, which is done in the App's installation settings and not from this repository.
- **Fallback: `RELEASE_PLEASE_TOKEN`**, a fine-grained personal access token scoped to this repository only, with Contents read/write, Pull requests read/write and Issues read/write, and nothing else. Do not use a classic PAT.

The chart appVersion sync job only pushes a branch and opens a PR, so it stays on Contents and Pull requests and needs no Issues permission.

### Published artifacts are immutable

The registry carries an immutable-tag rule on exact `MAJOR.MINOR.PATCH` tags, for images and for chart versions alike. A published version cannot be replaced, only superseded, so neither publish workflow has an `overwrite` input: there is nothing an overwrite could do except fail against that rule.

That makes reruns safe rather than dangerous. Both workflows check what is already in the registry before doing anything:

- if the exact artifact is missing, they build, scan and push it, so a rerun after a partial failure finishes only the images or the chart that never made it;
- if it is present and is what this release should be (an image: both platforms, the same `org.opencontainers.image.version` and `revision`; a chart: the same name, version and `appVersion`), the run reports success and does nothing;
- if it is present but is something else, the run fails and says so. That is not repairable in place. Publish a new patch version.

Moving tags are handled separately, on every run that gets that far, so a rerun whose only job was to finish an interrupted publish still fixes up aliases the interrupted run never wrote. `latest` and `<major>.<minor>` are excluded from the immutable-tag rule and are repointed from the digest the exact tag resolves to, and only when the version being published is the greatest published stable release: repairing or back-filling an older version never rolls `latest` backward.

### Repairing a publish

Both workflows take a `workflow_dispatch`:

- **Docker Publish** requires a published, non-prerelease release for `v<version>` and checks that `.release-please-manifest.json` at that tag agrees. Each architecture is pushed by digest with no tag, scanned with Trivy at HIGH and CRITICAL, and only then joined into the exact version tag, so nothing that fails the scan can ever be pulled by version.
- **Helm Publish** requires a published, non-prerelease release for `gascurve-chart-v<version>`, checks that `.release-please-manifest.json` at that tag records the same chart version, checks out the chart tag and packages the `appVersion` recorded there. All three images for that `appVersion` must already exist in the registry. The `app_version` input is an explicit override and is ignored unless `override_app_version` is also set; a mismatch with the tag is logged as a warning.
