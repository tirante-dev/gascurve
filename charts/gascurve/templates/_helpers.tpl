{{/*
Expand the name of the chart.
*/}}
{{- define "gascurve.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name, truncated to 63 characters.
*/}}
{{- define "gascurve.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "gascurve.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "gascurve.labels" -}}
helm.sh/chart: {{ include "gascurve.chart" . }}
{{ include "gascurve.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "gascurve.selectorLabels" -}}
app.kubernetes.io/name: {{ include "gascurve.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "gascurve.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (printf "%s-sa" (include "gascurve.fullname" .)) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Image reference for a component. Usage: include "gascurve.image" (list . .Values.api.image)
*/}}
{{- define "gascurve.image" -}}
{{- $root := index . 0 -}}
{{- $component := index . 1 -}}
{{- $tag := default $root.Chart.AppVersion $root.Values.image.tag -}}
{{- printf "%s/%s:%s" $root.Values.image.registry $component.repository $tag -}}
{{- end }}

{{- define "gascurve.databaseSecretName" -}}
{{- if .Values.database.existingSecret }}
{{- .Values.database.existingSecret }}
{{- else }}
{{- printf "%s-db" (include "gascurve.fullname" .) }}
{{- end }}
{{- end }}

{{- define "gascurve.databaseSecretKey" -}}
{{- if .Values.database.existingSecret }}
{{- .Values.database.existingSecretKey }}
{{- else }}
{{- "DB_URL" }}
{{- end }}
{{- end }}

{{/*
Extra environment for one component: the deprecated top-level extraEnv (an
alias applied to every Go container, kept so existing installs keep working)
followed by the component's own list. An entry in the component list replaces
the top-level entry of the same name rather than shadowing it, so the
container never gets two env vars with one name.
Usage: include "gascurve.extraEnv" (list . .Values.collector.extraEnv)
*/}}
{{- define "gascurve.extraEnv" -}}
{{- $root := index . 0 -}}
{{- $component := default (list) (index . 1) -}}
{{- $overridden := list -}}
{{- range $component -}}
{{- $overridden = append $overridden .name -}}
{{- end -}}
{{- $merged := list -}}
{{- range default (list) $root.Values.extraEnv -}}
{{- if not (has .name $overridden) -}}
{{- $merged = append $merged . -}}
{{- end -}}
{{- end -}}
{{- range $component -}}
{{- $merged = append $merged . -}}
{{- end -}}
{{- if $merged -}}
{{- toYaml $merged -}}
{{- end -}}
{{- end }}

{{/*
Environment shared by the Go binaries plus that component's extra env.
Usage: include "gascurve.goEnv" (list . .Values.api.extraEnv)
*/}}
{{- define "gascurve.goEnv" -}}
{{- $root := index . 0 -}}
{{- $component := index . 1 -}}
- name: DB_URL
  valueFrom:
    secretKeyRef:
      name: {{ include "gascurve.databaseSecretName" $root }}
      key: {{ include "gascurve.databaseSecretKey" $root }}
- name: CONFIG_PATH
  value: /etc/gascurve/config.yaml
- name: LOG_LEVEL
  value: {{ $root.Values.logLevel | quote }}
- name: PORT
  value: {{ $root.Values.config.server.port | quote }}
{{- with include "gascurve.extraEnv" (list $root $component) }}
{{ . }}
{{- end }}
{{- end }}

{{/*
Every network the collector will actually run needs a primary RPC URL from
somewhere: config.networks[].rpc_url in the ConfigMap, or a
NETWORK_<NAME>_RPC_URL entry in the collector's environment (collector.extraEnv
or the deprecated top-level extraEnv), which is how a private URL is kept in a
Secret. Fallback endpoints work the same way through the positional,
comma-separated NETWORK_<NAME>_FALLBACK_RPC_URLS. values.schema.json cannot
express those cross-references, so they are checked here: the render fails with
a message instead of the collector crash-looping on "rpc_url is required (set
NETWORK_<NAME>_RPC_URL)".

An override only counts when it actually carries a value. internal/config
ignores an empty NETWORK_<NAME>_RPC_URL (applyEnv takes the environment value
only when it is non-empty), so an entry with value: "" supplies nothing; the
schema rejects that shape outright, and this check agrees with it rather than
treating the name alone as evidence. A value that arrives through valueFrom
cannot be read at render time and is taken on trust.

A literal NETWORK_<NAME>_ENABLED value in the same environment overrides the
network's enabled flag, matching applyEnv; a name supplied through valueFrom
cannot be read here, so such a network is checked as configured. Only the
collector talks to an RPC, so web-only and api-only installs never reach this
check.
*/}}
{{- define "gascurve.validateNetworkRPC" -}}
{{- $supplied := list -}}
{{- $literals := dict -}}
{{- range concat (default (list) .Values.extraEnv) (default (list) .Values.collector.extraEnv) -}}
{{- $name := toString .name -}}
{{- if hasKey . "value" -}}
{{- $_ := set $literals $name (toString .value) -}}
{{- if ne (toString .value) "" -}}
{{- $supplied = append $supplied $name -}}
{{- end -}}
{{- else -}}
{{- $supplied = append $supplied $name -}}
{{- end -}}
{{- end -}}
{{- range .Values.config.networks -}}
{{- $network := . -}}
{{- $key := printf "NETWORK_%s" ($network.name | toString | replace "-" "_" | upper) -}}
{{- $enabled := $network.enabled -}}
{{- $override := get $literals (printf "%s_ENABLED" $key) -}}
{{- if has $override (list "1" "t" "T" "true" "TRUE" "True") -}}
{{- $enabled = true -}}
{{- else if has $override (list "0" "f" "F" "false" "FALSE" "False") -}}
{{- $enabled = false -}}
{{- end -}}
{{- if $enabled -}}
{{- if and (not $network.rpc_url) (not (has (printf "%s_RPC_URL" $key) $supplied)) -}}
{{- fail (printf "network %s is enabled but has no rpc_url and no collector.extraEnv entry named %s_RPC_URL with a value. Set config.networks[].rpc_url, or add %s_RPC_URL to collector.extraEnv (valueFrom.secretKeyRef to a Secret holding the private URL), or set enabled: false on that network." $network.name $key $key) -}}
{{- end -}}
{{- $listName := printf "%s_FALLBACK_RPC_URLS" $key -}}
{{- $listLiteral := get $literals $listName -}}
{{- $items := list -}}
{{- if $listLiteral -}}
{{- $items = splitList "," $listLiteral -}}
{{- end -}}
{{- range $i, $fallback := default (list) $network.fallbacks -}}
{{- if not $fallback.rpc_url -}}
{{- if not (has $listName $supplied) -}}
{{- fail (printf "network %s: fallbacks[%d] has no rpc_url and there is no collector.extraEnv entry named %s with a value. Every fallback endpoint of an enabled network needs an RPC URL: set fallbacks[%d].rpc_url, or supply the whole list through %s (comma separated, one item per fallback, position %d for this one), or remove the fallback." $network.name $i $listName $i $listName $i) -}}
{{- else if $listLiteral -}}
{{- if or (le (len $items) $i) (eq (trim (index $items $i)) "") -}}
{{- fail (printf "network %s: fallbacks[%d] has no rpc_url and %s supplies nothing at position %d (it is a comma separated list, one item per fallback, and it currently has %d). Set fallbacks[%d].rpc_url or extend %s." $network.name $i $listName $i (len $items) $i $listName) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Container securityContext for a component: the shared containerSecurityContext
merged with the component's own block (numeric runAsUser/runAsGroup, which
must match the image's USER so runAsNonRoot can be verified by the kubelet).
Usage: include "gascurve.containerSecurityContext" (list . .Values.api.securityContext)
*/}}
{{- define "gascurve.containerSecurityContext" -}}
{{- $root := index . 0 -}}
{{- $component := index . 1 -}}
{{- toYaml (mergeOverwrite (deepCopy $root.Values.containerSecurityContext) (default (dict) $component)) -}}
{{- end }}

{{/*
Annotations that roll the Go pods when chart-managed inputs change. The
database Secret checksum is only emitted when the chart renders that Secret;
an external Secret (database.existingSecret) is not tracked, see README.
*/}}
{{- define "gascurve.goPodAnnotations" -}}
checksum/config: {{ include (print .Template.BasePath "/configmap.yaml") . | sha256sum }}
{{- if and (not .Values.database.existingSecret) .Values.database.url }}
checksum/db-secret: {{ include (print .Template.BasePath "/secret.yaml") . | sha256sum }}
{{- end }}
{{- end }}

{{/*
Migration init container. Runs the migrations embedded in the same image as
the main container, so the schema is never newer or older than the binary
that follows it. golang-migrate takes a Postgres advisory lock, so the
collector and api pods can start at the same time without racing. Its
environment is the shared Go env plus migrations.extraEnv only: RPC
credentials from collector.extraEnv never reach it.
Usage: include "gascurve.migrateInitContainer" (list . .Values.api.image)
*/}}
{{- define "gascurve.migrateInitContainer" -}}
{{- $root := index . 0 -}}
{{- $image := index . 1 -}}
- name: migrate
  image: {{ include "gascurve.image" (list $root $image) | quote }}
  imagePullPolicy: {{ $root.Values.image.pullPolicy }}
  command: ["/app/gascurve-migrate", "up"]
  securityContext:
    {{- include "gascurve.containerSecurityContext" (list $root $root.Values.migrations.securityContext) | nindent 4 }}
  env:
    {{- include "gascurve.goEnv" (list $root $root.Values.migrations.extraEnv) | nindent 4 }}
  volumeMounts:
    - name: config
      mountPath: /etc/gascurve
      readOnly: true
  {{- with $root.Values.migrations.resources }}
  resources:
    {{- toYaml . | nindent 4 }}
  {{- end }}
{{- end }}
