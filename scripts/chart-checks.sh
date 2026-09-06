#!/usr/bin/env bash
# Render assertions for charts/gascurve, shared by `make chart-template` and
# .github/workflows/chart-test.yml so the local target and CI can never drift.
# Needs helm on PATH. Run it from anywhere; paths are resolved from the script.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
chart="${repo_root}/charts/gascurve"
ci="${chart}/ci"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

failures=0

fail() {
  if [ -n "${GITHUB_ACTIONS:-}" ]; then
    echo "::error::chart-checks: $*"
  else
    echo "FAIL: $*" >&2
  fi
  failures=$((failures + 1))
}

ok() { echo "  ok: $*"; }

# render <name> <output file> [helm template args...]
# Renders and fails the check if helm rejects the values.
render() {
  local name="$1" out="$2"
  shift 2
  if ! helm template gascurve "${chart}" "$@" > "${out}" 2> "${out}.err"; then
    fail "${name}: helm template was rejected but should have rendered: $(tr '\n' ' ' < "${out}.err")"
    return 1
  fi
}

# reject <name> <expected substring> [helm template args...]
# Fails the check unless helm refuses the values with a message containing the
# expected substring.
reject() {
  local name="$1" expect="$2"
  shift 2
  if helm template gascurve "${chart}" "$@" > "${work}/reject.yaml" 2> "${work}/reject.err"; then
    fail "${name}: helm template succeeded but should have been rejected"
    return 0
  fi
  if ! grep -qF "${expect}" "${work}/reject.err"; then
    fail "${name}: rejected, but the message does not mention '${expect}': $(tr '\n' ' ' < "${work}/reject.err")"
    return 0
  fi
  ok "${name} rejected"
}

# has <file> <pattern> <description>
has() { grep -q -- "$2" "$1" || fail "$3"; }

# lacks <file> <pattern> <description>
lacks() { grep -q -- "$2" "$1" && fail "$3" || true; }

echo "== schema rejections"

# The defaults configure no database, and the collector and api are enabled.
reject "defaults with no database" "database"
# Exactly one database source, never both.
reject "database.url plus database.existingSecret" "database" \
  --set database.url=postgres://x:y@db/gascurve --set database.existingSecret=my-db
# L2: an EnvVar needs a name and exactly one of value and valueFrom.
reject "extraEnv entry without a name" "name" \
  --set database.existingSecret=my-db --set 'api.extraEnv[0].placeholder=x'
reject "extraEnv entry with neither value nor valueFrom" "extraEnv" \
  --set database.existingSecret=my-db --set 'api.extraEnv[0].name=NO_SOURCE'
# M1: an enabled network with neither rpc_url nor a NETWORK_<NAME>_RPC_URL
# entry in the collector's environment.
reject "enabled network without an RPC URL" "no collector.extraEnv entry named NETWORK_SOLO_RPC_URL" \
  --set database.existingSecret=my-db \
  --set 'config.networks[0].name=solo' \
  --set 'config.networks[0].chain_id=99' \
  --set 'config.networks[0].enabled=true'

echo "== database.url renders the chart-managed Secret"
if render "database-url" "${work}/url.yaml" --values "${ci}/database-url-values.yaml"; then
  has "${work}/url.yaml" 'kind: Secret' "database-url: no chart-managed Secret rendered"
  has "${work}/url.yaml" 'checksum/db-secret' "database-url: no checksum/db-secret annotation, Secret changes will not roll the pods"
  has "${work}/url.yaml" 'checksum/config' "database-url: no checksum/config annotation"
  ok "Secret plus checksum annotations"
fi

echo "== database.existingSecret renders no Secret"
if render "existing-secret" "${work}/existing.yaml" --values "${ci}/existing-secret-values.yaml"; then
  lacks "${work}/existing.yaml" 'kind: Secret' "existing-secret: rendered a Secret, but database.existingSecret was set"
  has "${work}/existing.yaml" 'name: my-db' "existing-secret: DB_URL is not read from the existing Secret my-db"
  lacks "${work}/existing.yaml" 'checksum/db-secret' "existing-secret: emitted checksum/db-secret for a Secret the chart does not manage"
  ok "existing Secret referenced, none rendered"
fi

echo "== ingress"
if render "ingress" "${work}/ingress.yaml" --values "${ci}/ingress-values.yaml"; then
  has "${work}/ingress.yaml" 'kind: Ingress' "ingress: no Ingress rendered"
  lacks "${work}/ingress.yaml" 'kind: Secret' "ingress: rendered a Secret, but database.existingSecret was set"
  ok "Ingress rendered"
fi

echo "== web only"
if render "web-only" "${work}/web.yaml" --values "${ci}/web-only-values.yaml"; then
  has "${work}/web.yaml" 'app.kubernetes.io/component: web' "web-only: no web component rendered"
  lacks "${work}/web.yaml" 'app.kubernetes.io/component: collector' "web-only: rendered the collector although collector.enabled is false"
  lacks "${work}/web.yaml" 'app.kubernetes.io/component: api' "web-only: rendered the api although api.enabled is false"
  lacks "${work}/web.yaml" 'DB_URL' "web-only: the web container was given DB_URL"
  lacks "${work}/web.yaml" 'name: migrate' "web-only: rendered a migrate init container with no Go component"
  ok "web only, and no RPC check although no network has an rpc_url"
fi

echo "== M2: collector.extraEnv reaches only the collector"
if render "secret-rpc" "${work}/rpc-collector.yaml" --values "${ci}/secret-rpc-values.yaml" \
  --show-only templates/collector-deployment.yaml; then
  has "${work}/rpc-collector.yaml" 'NETWORK_ROBINHOOD_RPC_URL' "secret-rpc: the collector did not get NETWORK_ROBINHOOD_RPC_URL"
  has "${work}/rpc-collector.yaml" 'MIGRATE_ONLY_EXAMPLE' "secret-rpc: the collector's migrate init container did not get migrations.extraEnv"
  lacks "${work}/rpc-collector.yaml" 'API_ONLY_EXAMPLE' "secret-rpc: api.extraEnv leaked into the collector pod"
  ok "collector has the RPC Secret"
fi
if render "secret-rpc" "${work}/rpc-api.yaml" --values "${ci}/secret-rpc-values.yaml" \
  --show-only templates/api-deployment.yaml; then
  lacks "${work}/rpc-api.yaml" 'NETWORK_ROBINHOOD_RPC_URL' "secret-rpc: the RPC Secret leaked into the api pod (container or migrate init container)"
  lacks "${work}/rpc-api.yaml" 'NETWORK_ROBINHOOD_WS_URL' "secret-rpc: the RPC WebSocket Secret leaked into the api pod"
  has "${work}/rpc-api.yaml" 'API_ONLY_EXAMPLE' "secret-rpc: the api did not get api.extraEnv"
  has "${work}/rpc-api.yaml" 'MIGRATE_ONLY_EXAMPLE' "secret-rpc: the api's migrate init container did not get migrations.extraEnv"
  ok "api and its migrate init container have no RPC credentials"
fi

echo "== M1: a Secret-backed primary RPC URL is a valid configuration"
if render "secret-rpc" "${work}/rpc-all.yaml" --values "${ci}/secret-rpc-values.yaml"; then
  lacks "${work}/rpc-all.yaml" 'rpc.mainnet.chain.robinhood.com' "secret-rpc: a plaintext primary rpc_url for robinhood reached the ConfigMap"
  ok "no plaintext primary URL in the ConfigMap"
fi

echo "== the deprecated top-level extraEnv still reaches all three containers"
if render "deprecated-extra-env" "${work}/dep-collector.yaml" --values "${ci}/deprecated-extra-env-values.yaml" \
  --show-only templates/collector-deployment.yaml; then
  has "${work}/dep-collector.yaml" 'LEGACY_EXAMPLE' "deprecated-extra-env: the collector did not get the top-level extraEnv"
  ok "collector"
fi
if render "deprecated-extra-env" "${work}/dep-api.yaml" --values "${ci}/deprecated-extra-env-values.yaml" \
  --show-only templates/api-deployment.yaml; then
  has "${work}/dep-api.yaml" 'LEGACY_EXAMPLE' "deprecated-extra-env: the api did not get the top-level extraEnv"
  ok "api and migrate init container"
fi
# NOTES.txt is not a manifest, so helm template cannot show it. A client-side
# dry-run renders it, but Helm 3 still probes the cluster for its version
# first, so these two checks are skipped where no cluster is reachable (CI).
# The env-var behaviour they accompany is asserted above through the manifests.
notes_check() {
  local label="$1" values="$2" mode="$3"
  local out="${work}/${label}-notes.txt"
  if helm install gascurve "${chart}" --dry-run=client --values "${values}" > "${out}" 2>&1; then
    if [ "${mode}" = has ]; then
      has "${out}" 'extraEnv is deprecated' "${label}: NOTES.txt does not warn about the deprecated alias"
      ok "NOTES warns about the deprecated alias"
    else
      lacks "${out}" 'extraEnv is deprecated' "${label}: NOTES.txt warns about the deprecated alias although only the component lists are used"
      ok "NOTES stays quiet for the component lists"
    fi
  elif grep -q 'cluster unreachable' "${out}"; then
    echo "  skip: ${label} NOTES check (no cluster reachable for helm install --dry-run=client)"
  else
    fail "${label}: helm install --dry-run=client failed: $(tr '\n' ' ' < "${out}")"
  fi
}
notes_check "deprecated-extra-env" "${ci}/deprecated-extra-env-values.yaml" has
notes_check "secret-rpc" "${ci}/secret-rpc-values.yaml" lacks

if [ "${failures}" -ne 0 ]; then
  echo "chart-checks: ${failures} check(s) failed" >&2
  exit 1
fi
echo "OK: chart templates"
