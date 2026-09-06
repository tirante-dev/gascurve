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
