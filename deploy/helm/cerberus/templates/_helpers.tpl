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
cerberus.heads — the ordered split-mode head table. Each row binds the bare
Service name operators reach (prometheus / loki / tempo) to the
CERBERUS_ENABLED_HEADS token the binary parses (prom / loki / tempo). The two
diverge only for Prometheus ("prometheus" Service, "prom" token); keeping the
mapping explicit in ONE place is what lets the per-head partials and the
datasource NOTES iterate without re-deriving it. Returns a YAML list of
{svc, token} dicts (consume with `fromYamlArray`).
*/}}
{{- define "cerberus.heads" -}}
- svc: prometheus
  token: prom
- svc: loki
  token: loki
- svc: tempo
  token: tempo
{{- end }}

{{/*
cerberus.headValues — the resolved per-head `split.<svc>` block, normalised so a
null/absent field falls back to the top-level default. Input is the head's bare
Service name (e.g. "prometheus"); reads .Values.split.<svc> off the ROOT context
via $. Returns a YAML dict {enabled, replicaCount, resources, maxSamples} with
every field populated. Used by the per-head Deployment + Service partials.
*/}}
{{- define "cerberus.headValues" -}}
{{- $ctx := .ctx -}}
{{- $svc := .svc -}}
{{- $split := default (dict) $ctx.Values.split -}}
{{- $head := default (dict) (get $split $svc) -}}
enabled: {{ if hasKey $head "enabled" }}{{ $head.enabled }}{{ else }}true{{ end }}
replicaCount: {{ default $ctx.Values.replicaCount $head.replicaCount }}
{{- $res := default $ctx.Values.resources $head.resources }}
{{- if $res }}
resources:
{{ toYaml $res | indent 2 }}
{{- end }}
{{- /* maxSamples: per-head override else top-level query.maxSamples (may be unset) */ -}}
{{- $ms := $head.maxSamples }}
{{- if kindIs "invalid" $ms }}{{ $ms = $ctx.Values.query.maxSamples }}{{ end }}
{{- if not (kindIs "invalid" $ms) }}
maxSamples: {{ int64 $ms }}
{{- end }}
{{- end }}

{{/*
cerberus.headFullname — <release-fullname>-<svc>, e.g. my-cerberus-prometheus.
Used for the per-head Deployment / ServiceAccount-less workload object name in
split mode. The Service itself is bare-named (see cerberus.headService).
*/}}
{{- define "cerberus.headFullname" -}}
{{- printf "%s-%s" (include "cerberus.fullname" .ctx) .svc | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
cerberus.headSelectorLabels — IMMUTABLE per-head selector. The base selector
plus an app.kubernetes.io/component=<svc> discriminator so the three
Deployments and three Services never cross-select each other's pods. Never add
version / commonLabels here.
*/}}
{{- define "cerberus.headSelectorLabels" -}}
{{ include "cerberus.selectorLabels" .ctx }}
app.kubernetes.io/component: {{ .svc }}
{{- end }}

{{/*
cerberus.headLabels — full label set for a per-head object: the common labels
with the gateway-wide component label replaced by the per-head one (so each
head is independently selectable). commonLabels still merge in last.
*/}}
{{- define "cerberus.headLabels" -}}
helm.sh/chart: {{ include "cerberus.chart" .ctx }}
{{ include "cerberus.headSelectorLabels" . }}
{{- if .ctx.Chart.AppVersion }}
app.kubernetes.io/version: {{ .ctx.Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .ctx.Release.Service }}
app.kubernetes.io/part-of: cerberus
{{- with .ctx.Values.commonLabels }}
{{ tpl (toYaml .) .ctx }}
{{- end }}
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
{{- /* ClickHouse connection-pool tuning (CERBERUS_CH_*). Each key is emitted
       only when set, so an unset pool block stays byte-identical to the
       binary's own defaults. */ -}}
{{- with $ch.pool }}
{{- if not (kindIs "invalid" .maxOpenConns) }}
CERBERUS_CH_MAX_OPEN_CONNS: {{ int64 .maxOpenConns | quote }}
{{- end }}
{{- if not (kindIs "invalid" .maxIdleConns) }}
CERBERUS_CH_MAX_IDLE_CONNS: {{ int64 .maxIdleConns | quote }}
{{- end }}
{{- with .connMaxLifetime }}
CERBERUS_CH_CONN_MAX_LIFETIME: {{ . | quote }}
{{- end }}
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
{{- if .Values.requirementsCheck }}
CERBERUS_REQUIREMENTS_CHECK: "true"
{{- end }}
{{- with .Values.chOptimizations }}
CERBERUS_CH_OPTIMIZATIONS: {{ . | quote }}
{{- end }}
{{- /* Query safety limits (CERBERUS_QUERY_* / CERBERUS_CH_QUERY_MAX_MEMORY).
       Emitted only when set so an unset block keeps the binary defaults. */ -}}
{{- with .Values.query }}
{{- if not (kindIs "invalid" .maxSamples) }}
CERBERUS_QUERY_MAX_SAMPLES: {{ int64 .maxSamples | quote }}
{{- end }}
{{- with .timeout }}
CERBERUS_QUERY_TIMEOUT: {{ . | quote }}
{{- end }}
{{- if not (kindIs "invalid" .chMaxMemory) }}
{{- /* Quoted verbatim so a humanized size string (e.g. "2Gi") passes through unchanged; the binary accepts both a raw byte integer and a Kubernetes-style suffixed size. */}}
CERBERUS_CH_QUERY_MAX_MEMORY: {{ .chMaxMemory | quote }}
{{- end }}
{{- end }}
{{- if .Values.debug.pprof }}
CERBERUS_DEBUG_PPROF: "true"
{{- end }}
{{- with .Values.prom }}
{{- if .resourceLabels }}
CERBERUS_PROM_RESOURCE_LABELS: {{ join "," .resourceLabels | quote }}
{{- end }}
{{- end }}
{{- /* Replicated-ClickHouse (HA) schema — typed keys win over the generic
       schema.<KEY> passthrough below. */}}
{{- with .Values.schema.ttl }}
CERBERUS_SCHEMA_TTL: {{ . | quote }}
{{- end }}
{{- with .Values.schema.replicated }}
{{- if .enabled }}
CERBERUS_SCHEMA_DATABASE_REPLICATED: "true"
{{- with .zookeeperPath }}
CERBERUS_SCHEMA_DATABASE_REPLICATED_PATH: {{ . | quote }}
{{- end }}
{{- end }}
{{- end }}
{{- /* storage_policy shorthand -> its own dedicated env key (NOT the generic
       settings join), so it can be folded in PINNED FIRST by cerberus. */}}
{{- with .Values.schema.storagePolicy }}
CERBERUS_SCHEMA_STORAGE_POLICY: {{ . | quote }}
{{- end }}
{{- /* Generic MergeTree SETTINGS map -> CERBERUS_SCHEMA_SETTINGS as a sorted
       k=v,k2=v2 list (sorted so the rendered env is deterministic; cerberus
       preserves the order it receives). */}}
{{- $settings := .Values.schema.settings }}
{{- if $settings }}
{{- $pairs := list }}
{{- range $k := (keys $settings | sortAlpha) }}
{{- $pairs = append $pairs (printf "%s=%v" $k (index $settings $k | toString)) }}
{{- end }}
{{- if $pairs }}
CERBERUS_SCHEMA_SETTINGS: {{ join "," $pairs | quote }}
{{- end }}
{{- end }}
{{- /* Generic schema.<KEY> long-tail passthrough; skip the typed sub-keys
       (ttl / replicated / storagePolicy / settings) handled above so a
       duplicate env key is never emitted into the ConfigMap. */}}
{{- range $k, $v := .Values.schema }}
{{- if not (has $k (list "ttl" "replicated" "storagePolicy" "settings")) }}
CERBERUS_SCHEMA_{{ $k }}: {{ $v | quote }}
{{- end }}
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

{{/*
cerberus.memBytes — parse a Kubernetes memory quantity string into an integer
number of bytes. Handles the binary suffixes (Ki/Mi/Gi/Ti), the decimal SI
suffixes (k/K/M/G/T), and a bare integer (already bytes). Returns the byte
count as a string; an unparseable/empty input returns "" (callers skip on
empty). Input is the raw quantity (e.g. "1536Mi", "4Gi", "2G").
*/}}
{{- define "cerberus.memBytes" -}}
{{- $q := . | toString | trim -}}
{{- if hasSuffix "Ki" $q -}}{{ mulf (trimSuffix "Ki" $q) 1024 | int64 }}
{{- else if hasSuffix "Mi" $q -}}{{ mulf (trimSuffix "Mi" $q) 1048576 | int64 }}
{{- else if hasSuffix "Gi" $q -}}{{ mulf (trimSuffix "Gi" $q) 1073741824 | int64 }}
{{- else if hasSuffix "Ti" $q -}}{{ mulf (trimSuffix "Ti" $q) 1099511627776 | int64 }}
{{- else if hasSuffix "k" $q -}}{{ mulf (trimSuffix "k" $q) 1000 | int64 }}
{{- else if hasSuffix "K" $q -}}{{ mulf (trimSuffix "K" $q) 1000 | int64 }}
{{- else if hasSuffix "M" $q -}}{{ mulf (trimSuffix "M" $q) 1000000 | int64 }}
{{- else if hasSuffix "G" $q -}}{{ mulf (trimSuffix "G" $q) 1000000000 | int64 }}
{{- else if hasSuffix "T" $q -}}{{ mulf (trimSuffix "T" $q) 1000000000000 | int64 }}
{{- else if regexMatch "^[0-9]+$" $q -}}{{ $q | int64 }}
{{- end -}}
{{- end }}

{{/*
cerberus.gomemlimitEnv — emit a derived `GOMEMLIMIT` container env entry sized to
a fraction of THIS container's memory limit, so the Go runtime's soft heap limit
tracks the cgroup limit per-container (correct per-head in split, per-pod in
monolith) instead of forcing operators to hand-set one global value via extraEnv.

The byte budget is the limit times gomemlimitHeadroomFactor — headroom below
100% for off-heap allocations (the ClickHouse driver's buffers, goroutine
stacks, cgo) that GOMEMLIMIT does NOT bound. Emitted as a literal byte count
with Go's `B` suffix.

Args: dict "ctx" $ "resources" <resolved per-container resources>.
Renders nothing (so the caller emits no env entry) when:
  - limits.memory is unset (default OFF — no derivation to make), or
  - the operator already set GOMEMLIMIT in .Values.extraEnv (explicit wins).
*/}}
{{- define "cerberus.gomemlimitEnv" -}}
{{- /* gomemlimitHeadroomFactor: fraction of the memory limit handed to the Go
       soft heap limit; the remainder is headroom for off-heap memory GOMEMLIMIT
       cannot bound (CH driver buffers, goroutine stacks, cgo). */ -}}
{{- $gomemlimitHeadroomFactor := 0.8 -}}
{{- $ctx := .ctx -}}
{{- $res := .resources -}}
{{- $userSet := false -}}
{{- range (default (list) $ctx.Values.extraEnv) -}}
{{- if eq (default "" .name) "GOMEMLIMIT" -}}{{ $userSet = true }}{{- end -}}
{{- end -}}
{{- $limit := dig "limits" "memory" "" (default (dict) $res) -}}
{{- if and (not $userSet) $limit -}}
{{- $bytes := include "cerberus.memBytes" $limit -}}
{{- if $bytes -}}
- name: GOMEMLIMIT
  value: {{ printf "%dB" (mulf $bytes $gomemlimitHeadroomFactor | floor | int64) | quote }}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
cerberus.affinity — composes the optional colocateWithClickHouse podAffinity
preset over the operator-supplied .Values.affinity. The preset only INJECTS a
pod-affinity term (preferred/soft by default, required/hard opt-in) targeting
the ClickHouse pods, appending to any podAffinity the operator already
declared; every other affinity field the operator sets is preserved verbatim
(.Values.affinity wins). Renders nothing when neither is set.
*/}}
{{- define "cerberus.affinity" -}}
{{- $affinity := deepCopy (default (dict) .Values.affinity) -}}
{{- $preset := dig "colocateWithClickHouse" (dict) (default (dict) .Values.affinityPresets) -}}
{{- if $preset.enabled -}}
{{- $term := dict "labelSelector" (dict "matchLabels" $preset.podSelector.matchLabels) "topologyKey" $preset.topologyKey -}}
{{- $podAffinity := deepCopy (default (dict) $affinity.podAffinity) -}}
{{- if eq (default "preferred" $preset.mode) "required" -}}
{{- $existing := default (list) $podAffinity.requiredDuringSchedulingIgnoredDuringExecution -}}
{{- $_ := set $podAffinity "requiredDuringSchedulingIgnoredDuringExecution" (append $existing $term) -}}
{{- else -}}
{{- $existing := default (list) $podAffinity.preferredDuringSchedulingIgnoredDuringExecution -}}
{{- $_ := set $podAffinity "preferredDuringSchedulingIgnoredDuringExecution" (append $existing (dict "weight" 100 "podAffinityTerm" $term)) -}}
{{- end -}}
{{- $_ := set $affinity "podAffinity" $podAffinity -}}
{{- end -}}
{{- if $affinity -}}
{{ toYaml $affinity }}
{{- end -}}
{{- end -}}
