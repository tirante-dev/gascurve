# gascurve Helm chart

Deploys the three gascurve components: one collector (single replica, the only RPC client), the API, and the web frontend, plus a pre-install/pre-upgrade migration job.

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
| `database.url` | Rendered into a Secret | `""` |
| `database.existingSecret`, `database.existingSecretKey` | Use an existing Secret instead | `""`, `DB_URL` |
| `migrations.enabled` | Pre-install/upgrade migration hook | `true` |
| `config` | Rendered to `config.yaml` (networks, collector pacing, CORS) | see values.yaml |
| `extraEnv` | Extra env for the Go binaries, e.g. private RPC URLs from a Secret | `[]` |

The web image is built with `NEXT_PUBLIC_API_URL=/api/v1`, so it talks to the API through the same host. Put the API on another host only if you rebuild the image with a different value.
