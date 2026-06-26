{{/*
Bundled-ClickHouse ("bwc") helpers. Everything here is consumed ONLY by the
templates/clickhouse/* objects (all gated behind clickhouse.bundled.enabled) and
by cerberus.bundled.apply (the defaulting that wires cerberus at the bundled
data tier). None of it renders anything when bundled is disabled.
*/}}

{{/*
cerberus.clickhouse.fullname — the bundled ClickHouse ClusterIP Service +
StatefulSet name: <release-fullname>-clickhouse.
*/}}
{{- define "cerberus.clickhouse.fullname" -}}
{{- printf "%s-clickhouse" (include "cerberus.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
cerberus.clickhouse.headlessName — the headless Service that gives the
StatefulSet pods stable per-replica DNS.
*/}}
{{- define "cerberus.clickhouse.headlessName" -}}
{{- printf "%s-clickhouse-headless" (include "cerberus.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
cerberus.keeper.fullname / cerberus.keeper.headlessName — the Keeper ensemble
StatefulSet + its headless Service (only rendered when keeper is enabled).
*/}}
{{- define "cerberus.keeper.fullname" -}}
{{- printf "%s-keeper" (include "cerberus.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- define "cerberus.keeper.headlessName" -}}
{{- printf "%s-keeper-headless" (include "cerberus.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
cerberus.clickhouse.selectorLabels — IMMUTABLE selector for the bundled CH
StatefulSet / Services. Base selector + a clickhouse component discriminator.
*/}}
{{- define "cerberus.clickhouse.selectorLabels" -}}
{{ include "cerberus.selectorLabels" . }}
app.kubernetes.io/component: clickhouse
{{- end }}

{{/*
cerberus.clickhouse.labels — full label set for a bundled CH object (common
labels with the gateway component replaced by `clickhouse`).
*/}}
{{- define "cerberus.clickhouse.labels" -}}
helm.sh/chart: {{ include "cerberus.chart" . }}
{{ include "cerberus.clickhouse.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: cerberus
{{- with .Values.commonLabels }}
{{ tpl (toYaml .) $ }}
{{- end }}
{{- end }}

{{/*
cerberus.keeper.selectorLabels / cerberus.keeper.labels — same shape, with the
clickhouse-keeper component.
*/}}
{{- define "cerberus.keeper.selectorLabels" -}}
{{ include "cerberus.selectorLabels" . }}
app.kubernetes.io/component: clickhouse-keeper
{{- end }}
{{- define "cerberus.keeper.labels" -}}
helm.sh/chart: {{ include "cerberus.chart" . }}
{{ include "cerberus.keeper.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: cerberus
{{- with .Values.commonLabels }}
{{ tpl (toYaml .) $ }}
{{- end }}
{{- end }}

{{/*
cerberus.clickhouse.serviceAccountName — name of the SA the CH pods run under.
*/}}
{{- define "cerberus.clickhouse.serviceAccountName" -}}
{{- if .Values.clickhouse.bundled.serviceAccount.create -}}
{{- include "cerberus.clickhouse.fullname" . -}}
{{- else -}}
default
{{- end -}}
{{- end }}

{{/*
cerberus.clickhouse.keeperEnabled — "true" when the Keeper ensemble should be
rendered: the explicit keeper.enabled override wins; otherwise Keeper turns on
automatically once replicas > 1 (ReplicatedMergeTree needs coordination).
*/}}
{{- define "cerberus.clickhouse.keeperEnabled" -}}
{{- $b := .Values.clickhouse.bundled -}}
{{- $k := $b.keeper -}}
{{- if not (kindIs "invalid" $k.enabled) -}}
{{- if $k.enabled }}true{{ end -}}
{{- else if gt (int $b.replicas) 1 -}}
true
{{- end -}}
{{- end }}

{{/*
cerberus.clickhouse.objectStoreSecretName — the chart-managed Secret holding
static object-store credentials.
*/}}
{{- define "cerberus.clickhouse.objectStoreSecretName" -}}
{{- printf "%s-object-store" (include "cerberus.clickhouse.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
cerberus.clickhouse.createObjectStoreSecret — "true" when the chart should
render its own Secret for static object-store creds: the backend uses static
keys (NOT cloud identity) AND inline credentials were supplied AND no existing
credentialsSecret was named (an existing Secret takes precedence).
*/}}
{{- define "cerberus.clickhouse.createObjectStoreSecret" -}}
{{- $os := .Values.clickhouse.bundled.objectStorage -}}
{{- if eq $os.backend "s3" -}}
{{- if and (not $os.s3.useEnvironmentCredentials) (not $os.s3.credentialsSecret) $os.s3.accessKeyId -}}true{{- end -}}
{{- else if eq $os.backend "gcs" -}}
{{- if and (not $os.gcs.credentialsSecret) $os.gcs.accessKeyId -}}true{{- end -}}
{{- else if eq $os.backend "azure" -}}
{{- if and (not $os.azure.useManagedIdentity) (not $os.azure.credentialsSecret) $os.azure.accountName -}}true{{- end -}}
{{- end -}}
{{- end }}

{{/*
cerberus.clickhouse.objectStoreEnv — the ClickHouse container `env:` entries that
feed the storage XML's `from_env` references. Pulls static credentials from the
existing credentialsSecret when named, else from the chart-managed Secret.
Emits nothing for cloud-identity backends (IRSA / managed identity). Renders a
YAML list (one `- name:` block per credential).
*/}}
{{- define "cerberus.clickhouse.objectStoreEnv" -}}
{{- $os := .Values.clickhouse.bundled.objectStorage -}}
{{- if eq $os.backend "s3" -}}
{{- if not $os.s3.useEnvironmentCredentials -}}
{{- $secret := default (include "cerberus.clickhouse.objectStoreSecretName" .) $os.s3.credentialsSecret -}}
- name: S3_ACCESS_KEY_ID
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: access-key-id
- name: S3_SECRET_ACCESS_KEY
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: secret-access-key
{{- end -}}
{{- else if eq $os.backend "gcs" -}}
{{- $secret := default (include "cerberus.clickhouse.objectStoreSecretName" .) $os.gcs.credentialsSecret -}}
- name: GCS_ACCESS_KEY_ID
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: access-key-id
- name: GCS_SECRET_ACCESS_KEY
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: secret-access-key
{{- else if eq $os.backend "azure" -}}
{{- if not $os.azure.useManagedIdentity -}}
{{- $secret := default (include "cerberus.clickhouse.objectStoreSecretName" .) $os.azure.credentialsSecret -}}
- name: AZURE_ACCOUNT_NAME
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: account-name
- name: AZURE_ACCOUNT_KEY
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: account-key
{{- end -}}
{{- end -}}
{{- end }}

{{/*
cerberus.clickhouse.storageXML — the ClickHouse storage_configuration config.d
file: one object-store disk (S3 / GCS-over-S3 / Azure), a local cache disk
fronting it, and a single-volume policy (storagePolicyName) selecting the cache
disk. Static credentials are referenced via `from_env` so they never appear in
the ConfigMap; cloud-identity backends emit use_environment_credentials /
use_managed_identity instead. Input is the root context.
*/}}
{{- define "cerberus.clickhouse.storageXML" -}}
{{- $b := .Values.clickhouse.bundled -}}
{{- $os := $b.objectStorage -}}
{{- $policy := $b.storagePolicyName -}}
{{- $cacheBytes := include "cerberus.memBytes" $b.cache.size -}}
<clickhouse>
  <storage_configuration>
    <disks>
      <bwc_object_disk>
        {{- if eq $os.backend "s3" }}
        <type>s3</type>
        {{- if $os.s3.endpoint }}
        <endpoint>{{ printf "%s/%s/%s/" (trimSuffix "/" $os.s3.endpoint) $os.bucket (trimSuffix "/" $os.path) }}</endpoint>
        {{- else }}
        <endpoint>{{ printf "https://s3.%s.amazonaws.com/%s/%s/" $os.s3.region $os.bucket (trimSuffix "/" $os.path) }}</endpoint>
        {{- end }}
        {{- with $os.s3.region }}
        <region>{{ . }}</region>
        {{- end }}
        {{- if $os.s3.useEnvironmentCredentials }}
        <use_environment_credentials>true</use_environment_credentials>
        {{- else }}
        <access_key_id from_env="S3_ACCESS_KEY_ID" />
        <secret_access_key from_env="S3_SECRET_ACCESS_KEY" />
        {{- end }}
        {{- if $os.s3.forcePathStyle }}
        <!-- path-style addressing: bucket carried in the URL path -->
        {{- end }}
        {{- else if eq $os.backend "gcs" }}
        <type>s3</type>
        <endpoint>{{ printf "https://storage.googleapis.com/%s/%s/" $os.bucket (trimSuffix "/" $os.path) }}</endpoint>
        <access_key_id from_env="GCS_ACCESS_KEY_ID" />
        <secret_access_key from_env="GCS_SECRET_ACCESS_KEY" />
        <!-- GCS S3 API rejects multi-object delete -->
        <support_batch_delete>false</support_batch_delete>
        {{- else if eq $os.backend "azure" }}
        <type>azure_blob_storage</type>
        <storage_account_url>{{ $os.azure.storageAccountUrl }}</storage_account_url>
        <container_name>{{ $os.azure.container }}</container_name>
        {{- if $os.azure.useManagedIdentity }}
        <use_managed_identity>true</use_managed_identity>
        {{- else }}
        <account_name from_env="AZURE_ACCOUNT_NAME" />
        <account_key from_env="AZURE_ACCOUNT_KEY" />
        {{- end }}
        {{- end }}
      </bwc_object_disk>
      <bwc_object_cache>
        <type>cache</type>
        <disk>bwc_object_disk</disk>
        <path>/var/lib/clickhouse/object_store_cache/</path>
        {{- with $cacheBytes }}
        <max_size>{{ . }}</max_size>
        {{- end }}
      </bwc_object_cache>
    </disks>
    <policies>
      <{{ $policy }}>
        <volumes>
          <main>
            <disk>bwc_object_cache</disk>
          </main>
        </volumes>
      </{{ $policy }}>
    </policies>
  </storage_configuration>
</clickhouse>
{{- end }}

{{/*
cerberus.bundled.apply — DEFAULTING. When clickhouse.bundled.enabled, mutate
.Values in place so the rest of the chart (the cerberus env ConfigMap in
particular) points at the bundled data tier. Operator overrides win: a value the
operator changed from the chart default is left untouched. A no-op when bundled
is disabled, so non-bundled renders are byte-identical.
*/}}
{{- define "cerberus.bundled.apply" -}}
{{- $b := default (dict) .Values.clickhouse.bundled -}}
{{- if $b.enabled -}}
{{- /* addr -> bundled CH Service, unless the operator changed it from default */ -}}
{{- if eq (toJson .Values.clickhouse.addr) (toJson (list "clickhouse:9000")) -}}
{{- $_ := set .Values.clickhouse "addr" (list (printf "%s:9000" (include "cerberus.clickhouse.fullname" .))) -}}
{{- end -}}
{{- /* storage_policy -> bwc policy, unless the operator set one */ -}}
{{- if not .Values.schema.storagePolicy -}}
{{- $_ := set .Values.schema "storagePolicy" $b.storagePolicyName -}}
{{- end -}}
{{- /* a fresh bundled CH is empty: auto-create the database + schema. (Override
       an individual toggle via config.CERBERUS_AUTO_CREATE_* if you provision
       externally — config env wins over the typed default in the ConfigMap.) */ -}}
{{- if not .Values.autoCreate.schema -}}{{- $_ := set .Values.autoCreate "schema" true -}}{{- end -}}
{{- if not .Values.autoCreate.database -}}{{- $_ := set .Values.autoCreate "database" true -}}{{- end -}}
{{- /* replicas > 1 -> Replicated schema, REUSING cerberus's existing
       schema.replicated env wiring. */ -}}
{{- if gt (int $b.replicas) 1 -}}
{{- if not .Values.schema.replicated.enabled -}}
{{- $_ := set .Values.schema.replicated "enabled" true -}}
{{- end -}}
{{- if not .Values.schema.replicated.zookeeperPath -}}
{{- $_ := set .Values.schema.replicated "zookeeperPath" (printf "/clickhouse/databases/%s/{shard}/{replica}" .Values.clickhouse.database) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}
