# gascurve Helm chart

Deploys the three gascurve components: one collector (single replica, the only RPC client), the API, and the web frontend. Schema migrations run as an init container on the collector and api pods.

A database is required whenever the collector or the api is enabled: set exactly one of `database.url` and `database.existingSecret`. `values.schema.json` rejects an install, upgrade or template that sets neither or both, so the default values alone do not install.

```bash
helm install gascurve oci://registry.ahkc.win/gascurve/charts/gascurve \
  --set database.url='postgres://user:pass@postgres:5432/gascurve?sslmode=disable' \
  --set ingress.enabled=true --set ingress.host=gascurve.com
```

## Values

| Key | Description | Default |
|---|---|---|
| `image.registry` | Registry prefix for all three images | `registry.ahkc.win/gascurve` |
| `image.tag` | Image tag, defaults to the chart's appVersion | `""` |
| `collector.enabled` | Run the collector (always 1 replica) | `true` |
| `api.replicaCount` / `web.replicaCount` | Replicas | `2` / `2` |
| `ingress.enabled`, `ingress.host`, `ingress.tls` | One host: `/` to web, `/api` to api | `false`, `gascurve.com` |
| `database.url` | Rendered into a chart-managed Secret. Exactly one of `database.url` and `database.existingSecret` is required when the collector or api is enabled | `""` |
| `database.existingSecret`, `database.existingSecretKey` | Use an existing Secret instead of `database.url` | `""`, `DB_URL` |
| `migrations.enabled` | Run `gascurve-migrate up` as an init container on the collector and api pods | `true` |
| `collector.securityContext`, `api.securityContext`, `web.securityContext`, `migrations.securityContext` | Numeric `runAsUser`/`runAsGroup`, must match the image's `USER` | `65532` (Go images), `1001` (web) |
| `config` | Rendered to `config.yaml` (networks, collector pacing, CORS) | see values.yaml |
| `collector.extraEnv`, `api.extraEnv`, `migrations.extraEnv` | Extra env for that container only. Private RPC URLs belong in `collector.extraEnv` | `[]` |
| `extraEnv` | Deprecated alias applied to all three. Kept so existing installs keep working; move entries to the component lists | `[]` |

The web image is built with `NEXT_PUBLIC_API_URL=/api/v1`, so it talks to the API through the same host. Put the API on another host only if you rebuild the image with a different value.

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

## Releasing

The chart is versioned and released separately from the application. release-please keeps two release PRs open on `main`: one for the application (tag `v<version>`, publishes the three images) and one for the chart (tag `gascurve-chart-v<version>`, packages and pushes the chart to the OCI registry). `Chart.yaml` `appVersion` is what a published chart pulls, and it is the version Helm Publish packages and verifies, so the chart tag and the application version it ships are fixed together at tag time.

Keeping `appVersion` current is automatic but has a repository prerequisite:

- When an application release is published, the Release Please workflow opens a `fix(helm): update chart app version to <version>` PR and enables auto-merge on it. Merging that PR is what makes release-please open the next chart release PR.
- **Allow auto-merge must be enabled** in the repository settings (Settings, General, Pull Requests, "Allow auto-merge"). GitHub rejects `gh pr merge --auto` when the repository setting is off, so without it the sync PR is opened and then sits there, and `appVersion` stays behind until someone merges it by hand. The workflow step prints the PR number and the exact recovery command when this happens.
- Auto-merge still waits for required reviews and required status checks. It does not bypass branch protection.

If a publish workflow has to be repaired, both take a `workflow_dispatch`:

- **Docker Publish** requires a published, non-prerelease release for `v<version>` and checks that `.release-please-manifest.json` at that tag agrees. Repairs push only the exact version tag; `latest` and the `<major>.<minor>` alias move only when the version being published is the greatest published stable release, so repairing an old version never rolls `latest` backward.
- **Helm Publish** checks out the chart tag first and packages the `appVersion` recorded there. The `app_version` input is an explicit override and is ignored unless `override_app_version` is also set; a mismatch with the tag is logged as a warning.
