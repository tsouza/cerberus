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
cerberus.clickhouse.dataShardCount — the number of independent ClickHouse
DATA shards this chart renders (cerberus issue #3077, epic #3074's THIRD,
unrelated sense of "shard" — DISAMBIGUATION: not internal/solver's own
query-time-range "shard", nor this chart's pre-existing `{shard}`/`{replica}`
Keeper-coordination macros consumed by schema.replicated.* — see
internal/chopt/topology.go's terminology table). `1` (the default, and
whenever bundled is disabled) keeps every per-shard `range` below inert, so a
count==1 render stays byte-identical to the chart's pre-#3077 shape — no
range, no rename, nothing new evaluated. Input is the root context.
*/}}
{{- define "cerberus.clickhouse.dataShardCount" -}}
{{- if .Values.clickhouse.bundled.enabled -}}
{{- .Values.clickhouse.bundled.dataShards.count | default 1 -}}
{{- else -}}
1
{{- end -}}
{{- end }}

{{/*
cerberus.clickhouse.fullnameForDataShard — the per-DATA-shard StatefulSet name
(cerberus issue #3077 — see cerberus.clickhouse.dataShardCount's own
disambiguation). Takes (dict "ctx" $ "shard" $i), $i a 0-based shard index.
dataShardCount <= 1 returns the EXACT unsuffixed cerberus.clickhouse.fullname
— today's identity, byte-identical; > 1 suffixes EVERY shard, INCLUDING
index 0, with `-datashard-<i>`, so there is no silent partial-rename case.
*/}}
{{- define "cerberus.clickhouse.fullnameForDataShard" -}}
{{- $ctx := .ctx -}}
{{- $base := include "cerberus.clickhouse.fullname" $ctx -}}
{{- if le (int (include "cerberus.clickhouse.dataShardCount" $ctx)) 1 -}}
{{- $base -}}
{{- else -}}
{{- printf "%s-datashard-%d" $base (int .shard) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end }}

{{/*
cerberus.clickhouse.headlessNameForDataShard — the per-DATA-shard headless
Service name, same shape as cerberus.clickhouse.fullnameForDataShard but based
on cerberus.clickhouse.headlessName. Takes (dict "ctx" $ "shard" $i).
*/}}
{{- define "cerberus.clickhouse.headlessNameForDataShard" -}}
{{- $ctx := .ctx -}}
{{- $base := include "cerberus.clickhouse.headlessName" $ctx -}}
{{- if le (int (include "cerberus.clickhouse.dataShardCount" $ctx)) 1 -}}
{{- $base -}}
{{- else -}}
{{- printf "%s-datashard-%d" $base (int .shard) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end }}

{{/*
cerberus.clickhouse.macrosKeyForDataShard — the ConfigMap key holding shard $i's
`<macros>` XML (cerberus issue #3077): `macros-datashard-<i>.xml`. This is a
cerberus-CHOSEN ConfigMap key name, not part of ClickHouse's own fixed
schema, so — like every other NEW identifier for this epic's DATA-shard
sense of "shard" — it uses the disambiguating `datashard` compound form
rather than a bare `shard`. Takes a 0-based shard index.
*/}}
{{- define "cerberus.clickhouse.macrosKeyForDataShard" -}}
{{- printf "macros-datashard-%d.xml" (int .) -}}
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
cerberus.clickhouse.selectorLabelsForDataShard / cerberus.clickhouse.labelsForDataShard
— cerberus.clickhouse.selectorLabels/labels PLUS a per-DATA-shard
discriminator label, `cerberus.io/data-shard: "<i>"` (cerberus issue #3077 —
see cerberus.clickhouse.dataShardCount's own disambiguation; a LABEL KEY, so
it uses the same datashard-compound-form discipline as every other NEW
identifier for this sense of "shard"). Every per-shard StatefulSet +
Service pair must select ONLY that shard's own pods — sharing
cerberus.clickhouse.selectorLabels bare across every shard would give every
per-shard StatefulSet an IDENTICAL, overlapping pod selector, and multiple
StatefulSet controllers reconciling the SAME selector fight over pod
ownership. ONLY called from the dataShardCount>1 branch of each template —
the dataShardCount<=1 branch keeps using the bare selectorLabels/labels
unchanged, so that branch's selector/label shape never gains this label,
preserving byte-identical output. Takes (dict "ctx" $ "shard" $i), $i a
0-based shard index.
*/}}
{{- define "cerberus.clickhouse.selectorLabelsForDataShard" -}}
{{ include "cerberus.clickhouse.selectorLabels" .ctx }}
cerberus.io/data-shard: {{ .shard | quote }}
{{- end }}
{{- define "cerberus.clickhouse.labelsForDataShard" -}}
helm.sh/chart: {{ include "cerberus.chart" .ctx }}
{{ include "cerberus.clickhouse.selectorLabelsForDataShard" . }}
{{- if .ctx.Chart.AppVersion }}
app.kubernetes.io/version: {{ .ctx.Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .ctx.Release.Service }}
app.kubernetes.io/part-of: cerberus
{{- with .ctx.Values.commonLabels }}
{{ tpl (toYaml .) $.ctx }}
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
rendered: the explicit keeper.enabled override wins; otherwise Keeper turns
on automatically once replicas > 1 (ReplicatedMergeTree needs coordination)
OR once dataShardCount > 1 (cerberus issue #3077) — ClickHouse's own ON
CLUSTER DDL coordination, which every per-shard `CREATE ... ON CLUSTER`
statement relies on regardless of per-shard replica count, itself needs
Keeper. An explicit `keeper.enabled: false` together with dataShardCount > 1
fails loudly (mirrors cerberus.clickhouse.mode's own invalid-combination
guards) instead of silently rendering a per-shard StatefulSet whose "config"
ConfigMap volume unconditionally requires the cluster.xml /
macros-datashard-<i>.xml keys that configmap-config.yaml only emits when
Keeper is enabled — an unguarded combination previously left pods stuck in
ContainerCreating (a real k3d run surfaced this, not a theoretical concern).
*/}}
{{- define "cerberus.clickhouse.keeperEnabled" -}}
{{- $b := .Values.clickhouse.bundled -}}
{{- $k := $b.keeper -}}
{{- if not (kindIs "invalid" $k.enabled) -}}
{{- if and (not $k.enabled) (gt (int (include "cerberus.clickhouse.dataShardCount" .)) 1) -}}
{{- fail "clickhouse.bundled: keeper.enabled is explicitly false but dataShards.count > 1 — every per-shard StatefulSet requires cluster.xml/macros-datashard-<i>.xml, which are only rendered when Keeper is enabled. Leave keeper.enabled unset (it turns on automatically) or set it to true." -}}
{{- end -}}
{{- if $k.enabled }}true{{ end -}}
{{- else if or (gt (int $b.replicas) 1) (gt (int (include "cerberus.clickhouse.dataShardCount" .)) 1) -}}
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
{{- /* addr -> bundled CH Service, unless the operator changed it from default.
       dataShardCount > 1 (cerberus issue #3077) has NO unsuffixed
       "<fullname>:9000" Service at all — point at shard 0's own per-shard
       ClusterIP Service instead. That single entrypoint is sufficient and
       correct: the Distributed wrapper table (internal/schema/ddl's
       Config.DataShardCount) exists identically on every node of every
       shard (created ON CLUSTER), so a connection landing on any one
       shard's replica already fans a query out across the WHOLE cluster
       internally — cerberus itself only ever needs to reach ONE node. */ -}}
{{- if eq (toJson .Values.clickhouse.addr) (toJson (list "clickhouse:9000")) -}}
{{- if gt (int (include "cerberus.clickhouse.dataShardCount" .)) 1 -}}
{{- $_ := set .Values.clickhouse "addr" (list (printf "%s:9000" (include "cerberus.clickhouse.fullnameForDataShard" (dict "ctx" . "shard" 0)))) -}}
{{- else -}}
{{- $_ := set .Values.clickhouse "addr" (list (printf "%s:9000" (include "cerberus.clickhouse.fullname" .))) -}}
{{- end -}}
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
{{- $dataShardCount := int (include "cerberus.clickhouse.dataShardCount" .) -}}
{{- /* replicas > 1 -> Replicated schema, REUSING cerberus's existing
       schema.replicated env wiring. ONLY when dataShardCount <= 1:
       docs/operations.md's own "Auto-create schema" guidance is explicit
       that a Replicated DATABASE engine and an ON CLUSTER cluster are
       "mutually exclusive — pick one" (a Replicated database replicates its
       OWN DDL; layering ON CLUSTER on top of it is unverified against a
       real cluster and not a combination this chart assumes). dataShardCount
       > 1 (cerberus issue #3077) always needs Cluster set (below), so a
       multi-replica-PER-SHARD deployment gets the classic ON-CLUSTER +
       explicit-engine defaulting instead — see the dataShardCount block
       below. */ -}}
{{- if and (gt (int $b.replicas) 1) (le $dataShardCount 1) -}}
{{- if not .Values.schema.replicated.enabled -}}
{{- $_ := set .Values.schema.replicated "enabled" true -}}
{{- end -}}
{{- if not .Values.schema.replicated.zookeeperPath -}}
{{- $_ := set .Values.schema.replicated "zookeeperPath" (printf "/clickhouse/databases/%s/{shard}/{replica}" .Values.clickhouse.database) -}}
{{- end -}}
{{- end -}}
{{- /* dataShards.count > 1 (cerberus issue #3077): a Distributed-engine
       wrapper table and its local table's ON CLUSTER DDL both need a named
       cluster — default schema.CLUSTER to this chart's own cluster.xml
       <cluster> name (bwc_cluster) unless the operator set one, so
       internal/schema/ddl's Config.Cluster is never empty under
       DataShardCount>1 (it would otherwise fail Config.Validate at boot).
       Also set CERBERUS_CH_DATA_SHARDS via the generic config.<KEY>
       passthrough — dataShards.count is a BUNDLED-chart-only knob with no
       typed schema/clickhouse env of its own, so this is the one place that
       surfaces it to the running binary — so the sharded-pushdown solver's
       admission control (internal/chopt.ClusterTopology.DataShardCount)
       knows the real ClickHouse-side fan-out width; leaving the chart's own
       topology count unreported to cerberus would silently defeat that
       admission-control ceiling.

       replicas > 1 TOGETHER with dataShardCount > 1 (multi-replica PER
       shard) does NOT reuse the Replicated-database mechanism above — see
       that block's own comment on why the two are kept apart. Instead it
       defaults schema.TABLE_ENGINE to the CLASSIC explicit
       `ReplicatedMergeTree(path, '{replica}')` form (unless the operator
       set schema.replicated.enabled OR schema.TABLE_ENGINE themselves),
       using ClickHouse's OWN built-in {database}/{table} macros (no
       cerberus templating needed) alongside the {shard}/{replica} macros
       macros-datashard-<i>.xml gives a DISTINCT <shard> value per data
       shard — the "intentional convergence" cerberus issue #3077's
       acceptance criteria calls out: the SAME macro slot serves both this
       classic form and the single-data-shard Replicated-database form
       above, whichever mechanism a given deployment picks. */ -}}
{{- if gt $dataShardCount 1 -}}
{{- if not .Values.schema.CLUSTER -}}
{{- $_ := set .Values.schema "CLUSTER" "bwc_cluster" -}}
{{- end -}}
{{- if not (hasKey .Values.config "CERBERUS_CH_DATA_SHARDS") -}}
{{- $_ := set .Values.config "CERBERUS_CH_DATA_SHARDS" (toString $dataShardCount) -}}
{{- end -}}
{{- if and (gt (int $b.replicas) 1) (not .Values.schema.replicated.enabled) (not .Values.schema.TABLE_ENGINE) -}}
{{- $_ := set .Values.schema "TABLE_ENGINE" "ReplicatedMergeTree('/clickhouse/tables/{shard}/{database}/{table}', '{replica}')" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}
