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

echo "== the application configuration is modelled, not waved through"

# A network worth reusing across the malformed-value cases below.
net=(--set database.existingSecret=my-db
  --set 'config.networks[0].name=robinhood'
  --set 'config.networks[0].chain_id=4663'
  --set 'config.networks[0].rpc_url=https://rpc.example'
  --set 'config.networks[0].enabled=true')

# Intervals are Go durations, and a zero interval is as wrong as a word:
# internal/config requires every one of them to be positive.
reject "collector.tick_interval that is not a duration" "tick_interval" \
  --set database.existingSecret=my-db --set config.collector.tick_interval=bananas
reject "collector.tick_interval of zero" "tick_interval" \
  --set database.existingSecret=my-db --set config.collector.tick_interval=0s
reject "collector.eth_usd_max_age that is not a duration" "eth_usd_max_age" \
  --set database.existingSecret=my-db --set config.collector.eth_usd_max_age=forever
reject "network tick_interval that is not a duration" "tick_interval" \
  "${net[@]}" --set 'config.networks[0].tick_interval=soon'

# eth_usd_source is "", coinbase, coingecko or an https URL, and nothing else.
reject "collector.eth_usd_source that is not a string" "eth_usd_source" \
  --set database.existingSecret=my-db --set config.collector.eth_usd_source=17
reject "collector.eth_usd_source with an unsupported scheme" "eth_usd_source" \
  --set database.existingSecret=my-db --set-string config.collector.eth_usd_source=ftp://prices.example

# calls_per_second is 0 (unlimited) or a real budget between 0.1 and 10000.
reject "negative calls_per_second" "calls_per_second" \
  "${net[@]}" --set 'config.networks[0].calls_per_second=-5'
reject "calls_per_second above the maximum" "calls_per_second" \
  "${net[@]}" --set 'config.networks[0].calls_per_second=100000'
# helm --set never produces a float, so the below-minimum case needs a file.
cat > "${work}/tiny-cps.yaml" <<'YAML'
database:
  existingSecret: my-db
config:
  networks:
    - name: robinhood
      chain_id: 4663
      rpc_url: https://rpc.example
      enabled: true
      calls_per_second: 0.01
YAML
reject "calls_per_second below the minimum" "calls_per_second" --values "${work}/tiny-cps.yaml"

# A WebSocket URL is ws:// or wss://; an RPC URL is http:// or https://.
reject "ws_url with an http scheme" "ws_url" \
  "${net[@]}" --set 'config.networks[0].ws_url=https://nope.example'
reject "fallback rpc_url with an unsupported scheme" "rpc_url" \
  "${net[@]}" --set 'config.networks[0].fallbacks[0].rpc_url=ftp://nope.example'

reject "archive that is not a boolean" "archive" \
  "${net[@]}" --set-string 'config.networks[0].archive=notabool'

# Chart-owned objects reject unknown keys, so a typo or a field put at the
# wrong level fails the install instead of being mounted and ignored.
reject "unknown field on a network" "not_a_field" \
  "${net[@]}" --set 'config.networks[0].not_a_field=x'
reject "unknown field under config.collector" "not_a_field" \
  --set database.existingSecret=my-db --set config.collector.not_a_field=x
reject "application config placed under the collector component" "tick_interval" \
  --set database.existingSecret=my-db --set collector.tick_interval=3s

# An empty literal NETWORK_<NAME>_RPC_URL supplies nothing: internal/config
# ignores an empty override, so the network would start with no RPC URL.
reject "empty literal NETWORK_<NAME>_RPC_URL" "extraEnv" \
  --set database.existingSecret=my-db \
  --set 'config.networks[0].name=solo' \
  --set 'config.networks[0].chain_id=99' \
  --set 'config.networks[0].enabled=true' \
  --set 'collector.extraEnv[0].name=NETWORK_SOLO_RPC_URL' \
  --set-string 'collector.extraEnv[0].value='

# A fallback endpoint needs an RPC URL too, from the values or from the
# positional NETWORK_<NAME>_FALLBACK_RPC_URLS list.
reject "fallback with no rpc_url and no environment list" "NETWORK_ROBINHOOD_FALLBACK_RPC_URLS" \
  "${net[@]}" --set 'config.networks[0].fallbacks[0].calls_per_second=4'
reject "fallback list too short for the fallbacks configured" "position 1" \
  "${net[@]}" \
  --set 'config.networks[0].fallbacks[0].calls_per_second=4' \
  --set 'config.networks[0].fallbacks[1].calls_per_second=4' \
  --set 'collector.extraEnv[0].name=NETWORK_ROBINHOOD_FALLBACK_RPC_URLS' \
  --set-string 'collector.extraEnv[0].value=https://one.example'

echo "== metrics: the operator objects are opt in"

# Schema: an alert's window is a Prometheus duration, its severity is one of
# three, and an unknown alert name is a typo rather than a new rule.
reject "alert severity that is not a severity" "severity" \
  --set database.existingSecret=my-db --set metrics.prometheusRule.enabled=true \
  --set metrics.prometheusRule.alerts.apiDown.severity=page
reject "alert window that is not a duration" "for" \
  --set database.existingSecret=my-db --set metrics.prometheusRule.enabled=true \
  --set metrics.prometheusRule.alerts.apiDown.for=soon
reject "unknown alert name" "not_an_alert" \
  --set database.existingSecret=my-db --set metrics.prometheusRule.enabled=true \
  --set metrics.prometheusRule.alerts.not_an_alert.enabled=true
reject "scrape interval that is not a duration" "interval" \
  --set database.existingSecret=my-db --set metrics.serviceMonitor.enabled=true \
  --set metrics.serviceMonitor.interval=often
reject "metrics_port above the maximum" "metrics_port" \
  --set database.existingSecret=my-db --set config.collector.metrics_port=70000
reject "unknown field under metrics" "not_a_field" \
  --set database.existingSecret=my-db --set metrics.not_a_field=x

echo "== metrics disabled by default"
if render "metrics-default" "${work}/metrics-off.yaml" --values "${ci}/existing-secret-values.yaml"; then
  lacks "${work}/metrics-off.yaml" 'kind: ServiceMonitor' "metrics-default: rendered a ServiceMonitor although metrics.serviceMonitor.enabled is false"
  lacks "${work}/metrics-off.yaml" 'kind: PrometheusRule' "metrics-default: rendered a PrometheusRule although metrics.prometheusRule.enabled is false"
  # The binaries serve /metrics whatever the operator objects say, so the
  # collector keeps its port and its Service either way.
  has "${work}/metrics-off.yaml" 'name: gascurve-collector' "metrics-default: no collector Service, so nothing can scrape the collector"
  has "${work}/metrics-off.yaml" 'containerPort: 9090' "metrics-default: the collector container does not declare its metrics port"
  ok "no operator objects, but the collector is still scrapable"
fi

echo "== metrics enabled"
if render "metrics" "${work}/metrics.yaml" --values "${ci}/metrics-values.yaml"; then
  has "${work}/metrics.yaml" 'kind: ServiceMonitor' "metrics: no ServiceMonitor rendered"
  has "${work}/metrics.yaml" 'kind: PrometheusRule' "metrics: no PrometheusRule rendered"
  has "${work}/metrics.yaml" 'name: gascurve-api' "metrics: no api ServiceMonitor"
  has "${work}/metrics.yaml" 'name: gascurve-collector' "metrics: no collector ServiceMonitor"
  has "${work}/metrics.yaml" 'release: kube-prometheus-stack' "metrics: the operator selector labels did not reach the objects"
  has "${work}/metrics.yaml" 'team: platform' "metrics: alertLabels did not reach the alerts"
  # The two lag rules split the fleet on the network label, so a public RPC
  # sitting at 20 to 60 seconds cannot fire the dedicated threshold.
  has "${work}/metrics.yaml" 'network=~..robinhood|robinhood-testnet' "metrics: the dedicated lag rule does not select the dedicated networks"
  has "${work}/metrics.yaml" 'network!~..robinhood|robinhood-testnet' "metrics: the public lag rule does not exclude the dedicated networks"
  has "${work}/metrics.yaml" 'alert: GascurveCollectorSampleStale' "metrics: the stale sample alert is missing"
  has "${work}/metrics.yaml" 'alert: GascurveCollectorEndpointsExhausted' "metrics: the exhausted endpoints alert is missing"
  has "${work}/metrics.yaml" 'alert: GascurveCollectorRateLimited' "metrics: the rate limit alert is missing"
  has "${work}/metrics.yaml" 'alert: GascurveApiDown' "metrics: the api down alert is missing"
  has "${work}/metrics.yaml" 'alert: GascurveApiErrorRate' "metrics: the api error rate alert is missing"
  has "${work}/metrics.yaml" 'alert: GascurveDatabaseUnreachable' "metrics: the database alert is missing"
  # Every rule names this release's job, so two releases in one cluster
  # never alert on each other's metrics.
  if ! awk '/^ *expr: /{ if ($0 !~ /job=/) { print "    " $0; bad = 1 } } END { exit bad }' "${work}/metrics.yaml"; then
    fail "metrics: the rule above is not scoped to this release's job"
  fi
  # An alert switched off leaves no rule behind.
  lacks "${work}/metrics.yaml" 'alert: GascurveCollectorDown' "metrics: rendered an alert that was disabled"
  ok "ServiceMonitors, PrometheusRule and the split lag thresholds"
fi

echo "== metrics_port: 0 takes the collector's server away"
if render "metrics-port-zero" "${work}/metrics-zero.yaml" --values "${ci}/existing-secret-values.yaml" \
  --set config.collector.metrics_port=0 --set metrics.serviceMonitor.enabled=true; then
  lacks "${work}/metrics-zero.yaml" 'containerPort: 9090' "metrics-port-zero: the collector still declares a metrics port"
  lacks "${work}/metrics-zero.yaml" 'port: metrics' "metrics-port-zero: something still scrapes the collector"
  has "${work}/metrics-zero.yaml" 'port: http' "metrics-port-zero: the api ServiceMonitor went away with the collector's"
  ok "collector server, Service and ServiceMonitor all gone, api untouched"
fi

echo "== an ingress with no backend is refused"
reject "ingress enabled with api and web disabled" "/ingress/enabled" \
  --set database.existingSecret=my-db --set api.enabled=false --set web.enabled=false \
  --set ingress.enabled=true --set ingress.host=gascurve.com
# The template refuses it as well, for anyone who skips schema validation.
reject "ingress with no backend, schema validation skipped" "no HTTP paths" \
  --skip-schema-validation \
  --set database.existingSecret=my-db --set api.enabled=false --set web.enabled=false \
  --set ingress.enabled=true --set ingress.host=gascurve.com

echo "== ingress client identity and isolation are explicit"
reject "ingress and api without trusted proxies" "trusted_proxies" \
  --set database.existingSecret=my-db \
  --set ingress.enabled=true --set ingress.host=gascurve.com
reject "ingress and api without trusted proxies, schema validation skipped" "trusted_proxies is empty" \
  --skip-schema-validation \
  --set database.existingSecret=my-db \
  --set ingress.enabled=true --set ingress.host=gascurve.com
reject "malformed trusted proxy CIDR" "trusted_proxies" \
  --set database.existingSecret=my-db \
  --set ingress.enabled=true --set ingress.host=gascurve.com \
  --set 'config.server.trusted_proxies[0]=not-a-cidr'
reject "trusted proxy that trusts every peer" "trusted_proxies" \
  --set database.existingSecret=my-db \
  --set ingress.enabled=true --set ingress.host=gascurve.com \
  --set 'config.server.trusted_proxies[0]=0.0.0.0/0'
reject "trusted proxy that trusts every peer, schema validation skipped" "trusts every reachable peer" \
  --skip-schema-validation \
  --set database.existingSecret=my-db \
  --set ingress.enabled=true --set ingress.host=gascurve.com \
  --set 'config.server.trusted_proxies[0]=::/0'
reject "trusted proxy octet with a leading zero" "trusted_proxies" \
  --set database.existingSecret=my-db \
  --set ingress.enabled=true --set ingress.host=gascurve.com \
  --set 'config.server.trusted_proxies[0]=01.2.3.4'
reject "trusted proxy that is not a whole IPv6 address" "trusted_proxies" \
  --set database.existingSecret=my-db \
  --set ingress.enabled=true --set ingress.host=gascurve.com \
  --set 'config.server.trusted_proxies[0]=:::'
reject "NetworkPolicy selector using In with no values" "allowedPeers" \
  --set database.existingSecret=my-db \
  --set ingress.enabled=true --set ingress.host=gascurve.com \
  --set 'config.server.trusted_proxies[0]=10.244.0.0/16' \
  --set-json 'api.networkPolicy.allowedPeers=[{"podSelector":{"matchExpressions":[{"key":"role","operator":"In"}]}}]'
reject "NetworkPolicy selector using Exists with values" "allowedPeers" \
  --set database.existingSecret=my-db \
  --set ingress.enabled=true --set ingress.host=gascurve.com \
  --set 'config.server.trusted_proxies[0]=10.244.0.0/16' \
  --set-json 'api.networkPolicy.allowedPeers=[{"podSelector":{"matchExpressions":[{"key":"role","operator":"Exists","values":["proxy"]}]}}]'
reject "empty ingress controller NetworkPolicy peers" "allowedPeers" \
  --set database.existingSecret=my-db \
  --set ingress.enabled=true --set ingress.host=gascurve.com \
  --set 'config.server.trusted_proxies[0]=10.244.0.0/16' \
  --set-json 'api.networkPolicy.allowedPeers=[]'

reject "API ServiceMonitor blocked by the chart's own NetworkPolicy" "monitoringPeers" \
  --set database.existingSecret=my-db \
  --set ingress.enabled=true --set ingress.host=gascurve.com \
  --set 'config.server.trusted_proxies[0]=10.244.0.0/16' \
  --set metrics.serviceMonitor.enabled=true
reject "API ServiceMonitor blocked by the chart's own NetworkPolicy, schema validation skipped" "apiDown" \
  --skip-schema-validation \
  --set database.existingSecret=my-db \
  --set ingress.enabled=true --set ingress.host=gascurve.com \
  --set 'config.server.trusted_proxies[0]=10.244.0.0/16' \
  --set metrics.serviceMonitor.enabled=true

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
  has "${work}/ingress.yaml" 'trusted_proxies:' "ingress: trusted proxy configuration did not reach the ConfigMap"
  has "${work}/ingress.yaml" '10.244.0.0/16' "ingress: trusted proxy CIDR did not reach the ConfigMap"
  has "${work}/ingress.yaml" 'kind: NetworkPolicy' "ingress: no API NetworkPolicy rendered"
  has "${work}/ingress.yaml" 'kubernetes.io/metadata.name: ingress-nginx' "ingress: NetworkPolicy does not select the ingress-nginx namespace"
  has "${work}/ingress.yaml" 'app.kubernetes.io/component: controller' "ingress: NetworkPolicy does not select ingress controller pods"
  has "${work}/ingress.yaml" 'port: http' "ingress: NetworkPolicy does not limit access to the API HTTP port"
  lacks "${work}/ingress.yaml" 'kind: Secret' "ingress: rendered a Secret, but database.existingSecret was set"
  ok "Ingress, trusted proxies and API NetworkPolicy rendered"
fi

if render "ingress-peer-expressions" "${work}/ingress-peer-expressions.yaml" \
  --values "${ci}/ingress-values.yaml" \
  --set-json 'api.networkPolicy.allowedPeers=[{"namespaceSelector":{"matchExpressions":[{"key":"kubernetes.io/metadata.name","operator":"In","values":["ingress-nginx"]}]},"podSelector":{"matchExpressions":[{"key":"app.kubernetes.io/component","operator":"Exists"}]}}]'; then
  has "${work}/ingress-peer-expressions.yaml" 'operator: In' "ingress-peer-expressions: the In selector did not reach the NetworkPolicy"
  has "${work}/ingress-peer-expressions.yaml" 'operator: Exists' "ingress-peer-expressions: the Exists selector did not reach the NetworkPolicy"
  ok "well formed matchExpressions peers render"
fi

if render "ingress-scraped" "${work}/ingress-scraped.yaml" \
  --values "${ci}/ingress-values.yaml" --set metrics.serviceMonitor.enabled=true \
  --set-json 'api.networkPolicy.monitoringPeers=[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"mon"}},"podSelector":{"matchLabels":{"app.kubernetes.io/name":"prometheus"}}}]'; then
  has "${work}/ingress-scraped.yaml" 'kind: ServiceMonitor' "ingress-scraped: no ServiceMonitor rendered"
  has "${work}/ingress-scraped.yaml" 'kubernetes.io/metadata.name: mon' "ingress-scraped: NetworkPolicy does not admit the monitoring peer"
  ok "a declared monitoring peer reaches the API through its NetworkPolicy"
fi

if render "ingress-policy-disabled" "${work}/ingress-policy-disabled.yaml" \
  --values "${ci}/ingress-values.yaml" --set api.networkPolicy.enabled=false; then
  has "${work}/ingress-policy-disabled.yaml" 'kind: Ingress' "ingress-policy-disabled: no Ingress rendered"
  lacks "${work}/ingress-policy-disabled.yaml" 'kind: NetworkPolicy' "ingress-policy-disabled: API NetworkPolicy rendered although its chart control is disabled"
  ok "API NetworkPolicy control can defer to an equivalent external policy"
fi

if render "web-only-ingress" "${work}/web-only-ingress.yaml" \
  --set database.existingSecret=my-db --set api.enabled=false \
  --set ingress.enabled=true --set ingress.host=gascurve.com; then
  has "${work}/web-only-ingress.yaml" 'kind: Ingress' "web-only-ingress: no Ingress rendered"
  lacks "${work}/web-only-ingress.yaml" 'kind: NetworkPolicy' "web-only-ingress: rendered an API NetworkPolicy with no API"
  ok "web-only ingress needs neither API proxy trust nor an API NetworkPolicy"
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
echo "== the homelab configuration still renders"
if render "homelab" "${work}/homelab-collector.yaml" --values "${ci}/homelab-values.yaml" \
  --show-only templates/collector-deployment.yaml; then
  has "${work}/homelab-collector.yaml" 'NETWORK_ROBINHOOD_RPC_URL' "homelab: the collector did not get the Secret-backed primary RPC URL"
  has "${work}/homelab-collector.yaml" 'NETWORK_ROBINHOOD_WS_URL' "homelab: the collector did not get the Secret-backed WebSocket URL"
  has "${work}/homelab-collector.yaml" 'ETH_USD_SOURCE' "homelab: the collector did not get the empty ETH_USD_SOURCE that disables the price fetch"
  ok "collector environment"
fi
if render "homelab" "${work}/homelab-config.yaml" --values "${ci}/homelab-values.yaml" \
  --show-only templates/configmap.yaml; then
  has "${work}/homelab-config.yaml" 'metrics_port: 9090' "homelab: the collector metrics port did not reach config.yaml"
  has "${work}/homelab-config.yaml" 'tick_interval: 500ms' "homelab: tick_interval 500ms did not reach config.yaml"
  has "${work}/homelab-config.yaml" 'calls_per_second: 25' "homelab: the 25 calls per second budget did not reach config.yaml"
  has "${work}/homelab-config.yaml" 'backfill_depth: 720h' "homelab: backfill_depth 720h did not reach config.yaml"
  has "${work}/homelab-config.yaml" 'archive: true' "homelab: the archive primary did not reach config.yaml"
  has "${work}/homelab-config.yaml" 'ws://nitro-rpc' "homelab: the in-cluster ws:// fallback did not reach config.yaml"
  has "${work}/homelab-config.yaml" 'calls_per_second: 0$' "homelab: the unlimited in-cluster fallback did not reach config.yaml"
  has "${work}/homelab-config.yaml" 'calls_per_second: 4$' "homelab: the public fallback at 4 calls per second did not reach config.yaml"
  # The public endpoint is the second fallback, so it belongs in the
  # ConfigMap. The keyed primary does not, and it has no rpc_url at all here:
  # it arrives as NETWORK_ROBINHOOD_RPC_URL from the Secret, asserted above.
  has "${work}/homelab-config.yaml" 'rpc.mainnet.chain.robinhood.com' "homelab: the public fallback URL did not reach config.yaml"
  ok "config.yaml"
fi
if render "homelab" "${work}/homelab-api.yaml" --values "${ci}/homelab-values.yaml" \
  --show-only templates/api-deployment.yaml; then
  lacks "${work}/homelab-api.yaml" 'NETWORK_ROBINHOOD_RPC_URL' "homelab: the RPC Secret leaked into the api pod"
  lacks "${work}/homelab-api.yaml" 'NETWORK_ROBINHOOD_WS_URL' "homelab: the WebSocket Secret leaked into the api pod"
  ok "api pod has no RPC credentials"
fi

# NOTES.txt is not a manifest, so helm template cannot show it. A client-side
# dry-run renders it, but Helm 3 still probes the cluster for its version
# first, so these two checks are skipped where no cluster is reachable (CI).
# The env-var behaviour they accompany is asserted above through the manifests.
# notes_check <label> <values file> <has|lacks> <pattern> <what the pattern warns about>
notes_check() {
  local label="$1" values="$2" mode="$3" pattern="$4" what="$5"
  local out="${work}/${label}-notes.txt"
  if helm install gascurve "${chart}" --dry-run=client --values "${values}" > "${out}" 2>&1; then
    if [ "${mode}" = has ]; then
      has "${out}" "${pattern}" "${label}: NOTES.txt does not warn about ${what}"
      ok "NOTES warns about ${what}"
    else
      lacks "${out}" "${pattern}" "${label}: NOTES.txt warns about ${what} when it should not"
      ok "NOTES stays quiet about ${what}"
    fi
  elif grep -q 'cluster unreachable' "${out}"; then
    echo "  skip: ${label} NOTES check (no cluster reachable for helm install --dry-run=client)"
  else
    fail "${label}: helm install --dry-run=client failed: $(tr '\n' ' ' < "${out}")"
  fi
}
notes_check "deprecated-extra-env" "${ci}/deprecated-extra-env-values.yaml" has \
  'extraEnv is deprecated' "the deprecated alias"
notes_check "secret-rpc" "${ci}/secret-rpc-values.yaml" lacks \
  'extraEnv is deprecated' "the deprecated alias"
# An api with no chart-managed Ingress may still sit behind a tunnel or an
# Ingress this chart does not own, which the render checks cannot see.
notes_check "no-ingress-proxy-trust" "${ci}/secret-rpc-values.yaml" has \
  'config.server.trusted_proxies is empty' "an api that may be fronted by a proxy the chart cannot see"
notes_check "ingress-proxy-trust" "${ci}/ingress-values.yaml" lacks \
  'config.server.trusted_proxies is empty' "an api that may be fronted by a proxy the chart cannot see"

if [ "${failures}" -ne 0 ]; then
  echo "chart-checks: ${failures} check(s) failed" >&2
  exit 1
fi
echo "OK: chart templates"
