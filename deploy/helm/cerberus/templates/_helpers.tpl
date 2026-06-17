{{/*
Expand the name of the chart.
*/}}
{{- define "cerberus.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this
(by the DNS naming spec). If release name contains chart name it will be used
as a full name.
*/}}
{{- define "cerberus.fullname" -}}
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

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "cerberus.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels. commonLabels are tpl-rendered and merged LAST so operators can
override anything except the selector identity.
*/}}
{{- define "cerberus.labels" -}}
helm.sh/chart: {{ include "cerberus.chart" . }}
{{ include "cerberus.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: gateway
app.kubernetes.io/part-of: cerberus
{{- with .Values.commonLabels }}
{{ tpl (toYaml .) $ }}
{{- end }}
{{- end }}

{{/*
Selector labels — IMMUTABLE. Only name + instance. Never include version or
commonLabels here: the Deployment/PDB/HPA selector must never drift across an
upgrade or the controller orphans its pods.
*/}}
{{- define "cerberus.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cerberus.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
The name of the service account to use.
*/}}
{{- define "cerberus.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "cerberus.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Name of the env ConfigMap.
*/}}
{{- define "cerberus.envConfigMapName" -}}
{{- printf "%s-env" (include "cerberus.fullname" .) }}
{{- end }}

{{/*
Name of the chart-managed Secret (holds an inline ClickHouse password).
*/}}
{{- define "cerberus.secretName" -}}
{{- printf "%s" (include "cerberus.fullname" .) }}
{{- end }}

{{/*
Whether a chart-managed Secret should be rendered: an inline password is set
AND no existingSecret was supplied (existingSecret takes precedence).
*/}}
{{- define "cerberus.createSecret" -}}
{{- if and .Values.clickhouse.password (not .Values.clickhouse.existingSecret) -}}
true
{{- end -}}
{{- end }}

{{/*
Whether the ClickHouse TLS cert files should be volume-mounted from a Secret.
*/}}
{{- define "cerberus.tlsMount" -}}
{{- if and .Values.clickhouse.tls.enabled .Values.clickhouse.tls.existingSecret -}}
true
{{- end -}}
{{- end }}

{{/*
cerberus.nonSecretEnv — the ordered map of every NON-secret env var, lowered
from the typed blocks, then the free-form `config` map (config overrides typed
defaults). Rendered into the env ConfigMap. The ClickHouse password is NEVER
here — it flows via a secretKeyRef in the Deployment.

Returns YAML key: "value" pairs (one per line). Mutually-exclusive ordering is:
typed clickhouse -> otlp -> autoCreate -> admit -> schema -> http/log -> config.
*/}}
{{- define "cerberus.nonSecretEnv" -}}
{{- $ch := .Values.clickhouse -}}
{{- /* ClickHouse connection */ -}}
CERBERUS_CH_ADDR: {{ join "," $ch.addr | quote }}
{{- with $ch.database }}
CERBERUS_CH_DATABASE: {{ . | quote }}
{{- end }}
{{- with $ch.username }}
CERBERUS_CH_USERNAME: {{ . | quote }}
{{- end }}
{{- with $ch.protocol }}
CERBERUS_CH_PROTOCOL: {{ . | quote }}
{{- end }}
{{- with $ch.dialTimeout }}
CERBERUS_CH_DIAL_TIMEOUT: {{ . | quote }}
{{- end }}
{{- if $ch.tls.enabled }}
CERBERUS_CH_TLS_ENABLED: "true"
{{- with $ch.tls.insecureSkipVerify }}
CERBERUS_CH_TLS_INSECURE_SKIP_VERIFY: {{ . | quote }}
{{- end }}
{{- with $ch.tls.serverName }}
CERBERUS_CH_TLS_SERVER_NAME: {{ . | quote }}
{{- end }}
{{- if eq (include "cerberus.tlsMount" .) "true" }}
{{- $dir := "/etc/cerberus/tls" }}
CERBERUS_CH_TLS_CA_FILE: {{ printf "%s/%s" $dir $ch.tls.caFileKey | quote }}
CERBERUS_CH_TLS_CERT_FILE: {{ printf "%s/%s" $dir $ch.tls.certFileKey | quote }}
CERBERUS_CH_TLS_KEY_FILE: {{ printf "%s/%s" $dir $ch.tls.keyFileKey | quote }}
{{- end }}
{{- end }}
{{- with .Values.otlp.endpoint }}
CERBERUS_OTLP_ENDPOINT: {{ . | quote }}
{{- end }}
{{- if .Values.otlp.endpoint }}
CERBERUS_OTLP_INSECURE: {{ .Values.otlp.insecure | quote }}
{{- with .Values.otlp.headers }}
CERBERUS_OTLP_HEADERS: {{ . | quote }}
{{- end }}
{{- with .Values.otlp.exportInterval }}
CERBERUS_OTLP_EXPORT_INTERVAL: {{ . | quote }}
{{- end }}
{{- with .Values.otlp.timeout }}
CERBERUS_OTLP_TIMEOUT: {{ . | quote }}
{{- end }}
{{- end }}
CERBERUS_AUTO_CREATE_SCHEMA: {{ .Values.autoCreate.schema | quote }}
CERBERUS_AUTO_CREATE_DATABASE: {{ .Values.autoCreate.database | quote }}
CERBERUS_ADMIT_PROM: {{ .Values.admit.prom | quote }}
CERBERUS_ADMIT_LOKI: {{ .Values.admit.loki | quote }}
CERBERUS_ADMIT_TEMPO: {{ .Values.admit.tempo | quote }}
CERBERUS_ADMIT_DISABLED: {{ .Values.admit.disabled | quote }}
{{- range $k, $v := .Values.schema }}
CERBERUS_SCHEMA_{{ $k }}: {{ $v | quote }}
{{- end }}
{{- with .Values.http.addr }}
CERBERUS_HTTP_ADDR: {{ . | quote }}
{{- end }}
{{- with .Values.logLevel }}
CERBERUS_LOG_LEVEL: {{ . | quote }}
{{- end }}
{{- with .Values.logFormat }}
CERBERUS_LOG_FORMAT: {{ . | quote }}
{{- end }}
{{- range $k, $v := .Values.config }}
{{ $k }}: {{ $v | quote }}
{{- end }}
{{- end }}

{{/*
cerberus.env — the container `env:` list. Holds ONLY entries that need
valueFrom (the ClickHouse password secretKeyRef) plus the operator's raw
extraEnv (which therefore overrides everything in the ConfigMap, since later
env entries win and envFrom is lowest-precedence). Non-secret pairs live in the
ConfigMap and reach the container via envFrom.
*/}}
{{- define "cerberus.env" -}}
{{- $ch := .Values.clickhouse -}}
{{- if eq (include "cerberus.createSecret" .) "true" }}
- name: CERBERUS_CH_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ include "cerberus.secretName" . }}
      key: {{ $ch.passwordKey }}
{{- else if $ch.existingSecret }}
- name: CERBERUS_CH_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ $ch.existingSecret }}
      key: {{ $ch.passwordKey }}
{{- end }}
{{- with .Values.extraEnv }}
{{ tpl (toYaml .) $ }}
{{- end }}
{{- end }}
