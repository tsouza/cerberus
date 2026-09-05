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
StatefulSet / Services. Uses a DISTINCT app.kubernetes.io/name
(`<name>-clickhouse`) rather than the gateway's bare name so the gateway
Service's selector (cerberus.selectorLabels = name + instance) does NOT
over-select the ClickHouse pods. (k8s Service selectors match any pod whose
labels are a superset of the selector, so a CH pod carrying the gateway's
name+instance would otherwise land in the gateway's Endpoints and serve HTTP
404s from ClickHouse to gateway clients.) Component discriminator retained.
*/}}
{{- define "cerberus.clickhouse.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cerberus.name" . }}-clickhouse
app.kubernetes.io/instance: {{ .Release.Name }}
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
app.kubernetes.io/name: {{ include "cerberus.name" . }}-keeper
app.kubernetes.io/instance: {{ .Release.Name }}
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
render its own Secret for static object-store creds: objectStorage is enabled
AND the backend uses static keys (NOT cloud identity) AND inline credentials
were supplied AND no existing credentialsSecret was named (an existing Secret
takes precedence). Always empty in hot-only mode (objectStorage.enabled:
false) — no object-store disk means no object-store credentials of any kind.
*/}}
{{- define "cerberus.clickhouse.createObjectStoreSecret" -}}
{{- $os := .Values.clickhouse.bundled.objectStorage -}}
{{- if not $os.enabled -}}
{{- else if eq $os.backend "s3" -}}
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
Emits nothing for cloud-identity backends (IRSA / managed identity) AND
nothing at all in hot-only mode (objectStorage.enabled: false). Renders a
YAML list (one `- name:` block per credential).
*/}}
{{- define "cerberus.clickhouse.objectStoreEnv" -}}
{{- $os := .Values.clickhouse.bundled.objectStorage -}}
{{- if not $os.enabled -}}
{{- else if eq $os.backend "s3" -}}
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
cerberus.clickhouse.mode — one of "object-store" | "hot-only" | "hot-cold",
derived from hotVolume.enabled x objectStorage.enabled. Also enforces the two
render-time invariants that keep the chart from ever rendering a ClickHouse
with no usable storage tier:
  - hotVolume.enabled=false + objectStorage.enabled=false has NOTHING to put
    data on -> fails, naming both keys.
  - hotVolume.enabled=true + objectStorage.enabled=false (hot-only) with no
    schema.ttl set would fill an unbounded local disk forever with no cold
    tier to relieve it and no retention to cap it -> fails, naming schema.ttl.
Input is the root context.
*/}}
{{- define "cerberus.clickhouse.mode" -}}
{{- $b := .Values.clickhouse.bundled -}}
{{- $hot := $b.hotVolume.enabled -}}
{{- $os := $b.objectStorage.enabled -}}
{{- if and (not $hot) (not $os) -}}
{{- fail "clickhouse.bundled: both hotVolume.enabled and objectStorage.enabled are false — the bundled ClickHouse would have no storage tier at all. Set one of them (or both) to true." -}}
{{- else if and $hot (not $os) -}}
{{- if not .Values.schema.ttl -}}
{{- fail "clickhouse.bundled.hotVolume.enabled is true with objectStorage.enabled false (hot-only mode), but schema.ttl is unset — an unbounded local disk with no cold tier and infinite retention fills forever. Set schema.ttl explicitly." -}}
{{- end -}}
hot-only
{{- else if and $hot $os -}}
hot-cold
{{- else -}}
object-store
{{- end -}}
{{- end }}

{{/*
cerberus.clickhouse.effectivePolicyName — the MergeTree storage_policy name
actually emitted, mirroring cerberus.bundled.apply's "operator override wins"
style. An operator who set storagePolicyName away from the chart's own
shipped default (bwc_object_store) always gets that name back, in every mode.
Left at the shipped default, the effective name is MODE-DERIVED instead:
bwc_object_store (object-store mode, unchanged), bwc_hot_only, or
bwc_hot_cold — distinct names per mode so switching mode against an
already-populated cluster fails ClickHouse's own startup validation loudly
(additive-only storage-policy changes) rather than silently reusing a name.
*/}}
{{- define "cerberus.clickhouse.effectivePolicyName" -}}
{{- $b := .Values.clickhouse.bundled -}}
{{- if ne $b.storagePolicyName "bwc_object_store" -}}
{{- $b.storagePolicyName -}}
{{- else -}}
{{- $mode := include "cerberus.clickhouse.mode" . -}}
{{- if eq $mode "hot-only" -}}bwc_hot_only
{{- else if eq $mode "hot-cold" -}}bwc_hot_cold
{{- else -}}{{ $b.storagePolicyName }}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
cerberus.clickhouse.hotDiskPath — the local-disk path the `bwc_hot_disk`
storage-XML disk points at, and the StatefulSet mount path backing it. The
zero-new-PVC default is a subpath of the ALREADY-mounted `metadata` PVC
(/var/lib/clickhouse/hot/); hotVolume.persistence.enabled instead mounts a
DEDICATED PVC at a distinct top-level path (/var/lib/clickhouse-hot/), so the
two modes are trivially distinguishable in a rendered manifest. Input is the
root context.
*/}}
{{- define "cerberus.clickhouse.hotDiskPath" -}}
{{- if .Values.clickhouse.bundled.hotVolume.persistence.enabled -}}
/var/lib/clickhouse-hot/
{{- else -}}
/var/lib/clickhouse/hot/
{{- end -}}
{{- end }}

{{/*
cerberus.clickhouse.storageXML — the ClickHouse storage_configuration config.d
file. Three shapes, selected by cerberus.clickhouse.mode:
  - object-store (default, unchanged): one object-store disk (S3 / GCS-over-S3
    / Azure), a local cache disk fronting it, single-volume policy `main`.
  - hot-only: one local disk, single-volume policy `hot`, no object-store disk
    / secret / credential env at all.
  - hot-cold: BOTH — a `hot` volume (the local disk, listed FIRST so new
    inserts land there) and a `cold` volume (the unchanged object-store
    disk/cache chain), joined by move_factor. The object-store disk block is
    reused verbatim, so this is backend-agnostic by construction.
Static credentials are referenced via `from_env` so they never appear in the
ConfigMap; cloud-identity backends emit use_environment_credentials /
use_managed_identity instead. Input is the root context.
*/}}
{{- define "cerberus.clickhouse.storageXML" -}}
{{- $b := .Values.clickhouse.bundled -}}
{{- $os := $b.objectStorage -}}
{{- $mode := include "cerberus.clickhouse.mode" . -}}
{{- $policy := include "cerberus.clickhouse.effectivePolicyName" . -}}
{{- $cacheBytes := include "cerberus.memBytes" $b.cache.size -}}
{{- $hotPath := include "cerberus.clickhouse.hotDiskPath" . -}}
<clickhouse>
  <storage_configuration>
    <disks>
      {{- if ne $mode "hot-only" }}
      <bwc_object_disk>
        {{- if eq $os.backend "s3" }}
        <type>s3</type>
        {{- if $os.s3.endpoint }}
        {{- /* custom endpoint (MinIO / localstack / non-AWS): always path-style
               — the bucket rides in the URL path: <endpoint>/<bucket>/<path>/. */}}
        <endpoint>{{ printf "%s/%s/%s/" (trimSuffix "/" $os.s3.endpoint) $os.bucket (trimSuffix "/" $os.path) }}</endpoint>
        {{- else if $os.s3.forcePathStyle }}
        {{- /* AWS, path-style (legacy): https://s3.<region>.amazonaws.com/<bucket>/<path>/. */}}
        <endpoint>{{ printf "https://s3.%s.amazonaws.com/%s/%s/" $os.s3.region $os.bucket (trimSuffix "/" $os.path) }}</endpoint>
        {{- else }}
        {{- /* AWS, virtual-hosted (default): https://<bucket>.s3.<region>.amazonaws.com/<path>/. */}}
        <endpoint>{{ printf "https://%s.s3.%s.amazonaws.com/%s/" $os.bucket $os.s3.region (trimSuffix "/" $os.path) }}</endpoint>
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
      {{- end }}
      {{- if ne $mode "object-store" }}
      <bwc_hot_disk>
        <type>local</type>
        <path>{{ $hotPath }}</path>
      </bwc_hot_disk>
      {{- end }}
    </disks>
    <policies>
      {{- if eq $mode "hot-only" }}
      <{{ $policy }}>
        <volumes>
          <hot>
            <disk>bwc_hot_disk</disk>
          </hot>
        </volumes>
      </{{ $policy }}>
      {{- else if eq $mode "hot-cold" }}
      <{{ $policy }}>
        <volumes>
          <hot>
            <disk>bwc_hot_disk</disk>
          </hot>
          <cold>
            <disk>bwc_object_cache</disk>
          </cold>
        </volumes>
        <move_factor>{{ $b.hotVolume.moveFactor }}</move_factor>
      </{{ $policy }}>
      {{- else }}
      <{{ $policy }}>
        <volumes>
          <main>
            <disk>bwc_object_cache</disk>
          </main>
        </volumes>
      </{{ $policy }}>
      {{- end }}
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
{{- /* storage_policy -> the mode-derived bwc policy name, unless the operator
       set one. Also validates/derives the mode as a side effect — this is the
       first cerberus.bundled.apply call site to run, so an invalid
       hotVolume/objectStorage combination (or a missing schema.ttl in
       hot-only mode) fails here rather than reaching the templates that
       render it. */ -}}
{{- $mode := include "cerberus.clickhouse.mode" . -}}
{{- if not .Values.schema.storagePolicy -}}
{{- $_ := set .Values.schema "storagePolicy" (include "cerberus.clickhouse.effectivePolicyName" .) -}}
{{- end -}}
{{- /* Hot/cold mode: default schema.tierVolume=cold and, independently,
       schema.tierAfter=7d — unless the operator set the BASE value
       themselves. Checked against ONLY the base tierVolume/tierAfter keys,
       NEVER against any TIER_AFTER_METRICS/LOGS/TRACES per-signal override
       riding the schema.<KEY> long-tail passthrough — so setting only, say,
       schema.TIER_AFTER_METRICS still leaves this base default in place for
       Logs/Traces. A per-signal override ADDS a customization; it must never
       accidentally suppress the base default for the signals the operator
       didn't touch. */ -}}
{{- if eq $mode "hot-cold" -}}
{{- if not .Values.schema.tierVolume -}}
{{- $_ := set .Values.schema "tierVolume" "cold" -}}
{{- end -}}
{{- if not .Values.schema.tierAfter -}}
{{- $_ := set .Values.schema "tierAfter" "7d" -}}
{{- end -}}
{{- end -}}
{{- /* a fresh bundled CH is empty: auto-create the database + schema. autoCreate
       is tri-state (null/true/false) — only an UNSET (null) toggle is promoted
       to true here, so an operator who explicitly set `autoCreate.schema=false`
       (or database=false) keeps that false even under bundled. Mirrors the
       keeper.enabled:null tri-state above. */ -}}
{{- if kindIs "invalid" .Values.autoCreate.schema -}}{{- $_ := set .Values.autoCreate "schema" true -}}{{- end -}}
{{- if kindIs "invalid" .Values.autoCreate.database -}}{{- $_ := set .Values.autoCreate "database" true -}}{{- end -}}
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
