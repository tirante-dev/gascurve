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
Environment shared by the Go binaries.
*/}}
{{- define "gascurve.goEnv" -}}
- name: DB_URL
  valueFrom:
    secretKeyRef:
      name: {{ include "gascurve.databaseSecretName" . }}
      key: {{ include "gascurve.databaseSecretKey" . }}
- name: CONFIG_PATH
  value: /etc/gascurve/config.yaml
- name: LOG_LEVEL
  value: {{ .Values.logLevel | quote }}
- name: PORT
  value: {{ .Values.config.server.port | quote }}
{{- with .Values.extraEnv }}
{{ toYaml . }}
{{- end }}
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
collector and api pods can start at the same time without racing.
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
    {{- include "gascurve.goEnv" $root | nindent 4 }}
  volumeMounts:
    - name: config
      mountPath: /etc/gascurve
      readOnly: true
  {{- with $root.Values.migrations.resources }}
  resources:
    {{- toYaml . | nindent 4 }}
  {{- end }}
{{- end }}
