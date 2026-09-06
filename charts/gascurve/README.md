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
| `extraEnv` | Extra env for the Go binaries, e.g. private RPC URLs from a Secret | `[]` |

The web image is built with `NEXT_PUBLIC_API_URL=/api/v1`, so it talks to the API through the same host. Put the API on another host only if you rebuild the image with a different value.

## Migrations

Each collector and api pod runs `/app/gascurve-migrate up` in an init container from the same image as its main container, so the schema is always at the version the binary expects. There is no Helm hook, so nothing runs before the ServiceAccount, ConfigMap, and Secret exist, and Argo CD needs no hook or sync-wave configuration. golang-migrate takes a PostgreSQL advisory lock, so pods that start at the same time queue on the lock rather than racing. A failed migration keeps the pod in `Init:Error`; inspect it with `kubectl logs <pod> -c migrate`. Set `migrations.enabled=false` if migrations are applied elsewhere.

What a slow or failed migration means for each component differs:

- **api** (RollingUpdate): the previous ReplicaSet keeps serving until the new pods pass their init container and readiness probe, so the API stays available during the migration and during a failed upgrade.
- **collector** (Recreate, single replica): the old collector is stopped before the new pod starts, and the new pod's migration runs after that, so collection pauses for the duration of the migration and stays paused for as long as a migration fails. This gap is the price of the single-writer rule (two collectors would double the RPC budget and race on the same rows). Keep migrations small, and if one fails roll back with `helm rollback` after repairing the schema state (golang-migrate refuses to run on a dirty version). The collector backfills the blocks it missed once it is running again.

## Rolling pods on Secret changes

The collector and api pod templates carry `checksum/config` and, when the chart renders the database Secret from `database.url`, `checksum/db-secret`, so changing either value rolls the pods on upgrade. Secrets the chart does not manage are not tracked: after rotating `database.existingSecret` or any Secret referenced from `extraEnv`, run

```bash
kubectl rollout restart deploy/<release>-collector deploy/<release>-api
```

or use a reloader controller. Containers read Secret-backed environment variables only at startup.
