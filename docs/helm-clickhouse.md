# Helm: bundled ClickHouse

`clickhouse.bundled.enabled` (`deploy/helm/cerberus/templates/clickhouse/`) renders a
self-contained ClickHouse StatefulSet — plus its Services, plus a Keeper
ensemble once `bundled.replicas > 1` — and defaults cerberus to point at it.
This is the **data tier** and is orthogonal to `mode` (monolith / split) — the
gateway topology is unchanged. With the default `clickhouse.bundled.enabled:
false` the chart renders byte-for-byte as if this whole block did not exist.

Two independent toggles pick the storage tier — **both default `true`**, so
**hot/cold is the chart's default storage mode**, not an opt-in:

- `clickhouse.bundled.objectStorage.enabled` (default `true`) — an S3 / GCS /
  Azure object-store disk fronted by a local read-through cache.
- `clickhouse.bundled.hotVolume.enabled` (default `true`) — a genuine
  local-disk "hot" tier new parts land on directly (not a read-through cache
  of object-resident data).

## The four-cell matrix

| `hotVolume.enabled`  | `objectStorage.enabled`  | Mode                     | `storage_policy`    | Result                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| -------------------- | ------------------------ | ------------------------ | ------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `true` (default)     | `true` (default)         | **Hot/cold**             | `bwc_hot_cold`      | The chart's DEFAULT. Two-volume policy: `hot` (local disk, listed first so new inserts land there) + `cold` (the object-store disk/cache chain). Parts age off `hot` onto `cold` via a `TTL ... TO VOLUME` clause driven by `schema.tierVolume`/`tierAfter*` once they cross that age; `move_factor` (`hotVolume.moveFactor`, default `0.2`) is a separate, free-space-triggered backstop move, independent of that age-based one. Backend-agnostic by construction — the object-store disk block is reused verbatim from object-store mode below. |
| `false`              | `true`                   | **Object-store**         | `bwc_object_store`  | The chart's ONLY mode before #3075, still fully supported — set `hotVolume.enabled: false` explicitly to get it. One object-store disk fronted by a local cache disk, single-volume policy. Every write round-trips through the cache to object storage. **Set this explicitly to preserve current behavior across an upgrade** — see below.                                                                                                                                                                                                       |
| `true`               | `false`                  | **Hot-only**             | `bwc_hot_only`      | Pure local-disk ClickHouse, no object-store dependency at all — no object disk, no Secret, no credential env. Requires `schema.ttl` to be set explicitly (see below).                                                                                                                                                                                                                                                                                                                                                                              |
| `false`              | `false`                  | *(invalid)*              | —                   | **Fails the render**, naming both `hotVolume.enabled` and `objectStorage.enabled` — a bundled ClickHouse with no storage tier at all is never silently rendered.                                                                                                                                                                                                                                                                                                                                                                                   |

The policy name is mode-derived automatically; an explicit
`clickhouse.bundled.storagePolicyName` always overrides it, in every mode.

## Upgrading into the hot/cold default

**This chart's bundled ClickHouse changed its DEFAULT storage mode** from
single-volume object-storage-only to hot/cold (#3075). That is a genuine
storage-layout change, not just a values default: a fresh install still just
works, but an **existing** `clickhouse.bundled.enabled: true` deployment that
upgrades to a chart version carrying this change — without itself pinning
`hotVolume.enabled` — silently asks for a `bwc_hot_cold` policy where its
ClickHouse only has `bwc_object_store` on disk.

ClickHouse's own startup validation catches this loudly rather than
corrupting anything: each mode's `storage_policy` has a distinct name (see the
mode-toggling section below), and ClickHouse refuses to add a policy that
doesn't match what's already provisioned — the pod fails to start with an
"unknown storage policy" — style error, not silent data loss. Still, that is a
scary and avoidable surprise for an operator who didn't expect a default
change. `templates/NOTES.txt` renders a prominent warning on every `helm
upgrade` of a `bundled.enabled: true` release that resolves to hot/cold mode,
naming the fix.

**The fix: pin `clickhouse.bundled.hotVolume.enabled: false` in your own
values BEFORE upgrading past the chart version that introduced this default**,
if you are relying on (or unsure whether you're relying on) the previous
single-volume, object-storage-only behavior. That one line reproduces the
exact pre-#3075 chart behavior indefinitely, regardless of any future default
change. A genuinely fresh hot/cold install needs no action — the default is
exactly what you want.

**Hot-only requires `schema.ttl`.** A bounded local disk with no cold tier and
unset `schema.ttl` (infinite retention) would fill unboundedly, so the chart
**fails the render** in hot-only mode unless `schema.ttl` is explicitly set —
no silent default retention is guessed on the operator's behalf.

**Hot/cold auto-tiering.** In hot/cold mode, `cerberus.bundled.apply` defaults
`schema.tierVolume=cold` and, independently, `schema.tierAfter=7d` — but only
when the operator hasn't set that BASE value themselves. A per-signal override
(`schema: { TIER_AFTER_METRICS: "3d" }`) only customizes that one signal; it
does not suppress the base `tierAfter=7d` default that still covers the
signals the operator did not touch. See
[Hot/cold storage tiering](operations.md#hotcold-storage-tiering) for what
the emitted `TTL ... TO VOLUME` clause actually does.

## The local hot volume: capacity risk

The default hot volume needs **no new PVC** — it's a subpath of the existing
`metadata` PVC (already sized for CH system metadata plus the object-store
cache). That default's real benefit is skipping the object-store round trip on
writes, **not** delivering materially faster storage: it shares `metadata`'s
storage class and I/O budget with CH's own system metadata and the cache disk.

Two capacity risks follow, and the chart surfaces rather than hides them:

1. The hot volume is a **third, effectively unbounded consumer** of
   `clickhouse.bundled.persistence.size`, alongside CH's system metadata and
   the object-store cache.
2. Unlike the object-store disk (zero-copy replicated), a local disk is
   **not** replicated between StatefulSet pods — under
   `clickhouse.bundled.replicas > 1` each replica fills its own `metadata` PVC
   independently, so the consumption in (1) **multiplies by the replica
   count**.

`templates/NOTES.txt` renders a warning naming both, whenever `hotVolume.
enabled` is on without a dedicated `hotVolume.persistence` (and additionally
names the replica multiplier once `bundled.replicas > 1`). For any non-toy
deployment, set `clickhouse.bundled.hotVolume.persistence.enabled: true` — a
dedicated PVC, ideally on a faster StorageClass — instead of relying on the
zero-new-PVC default.

## Mode toggling is an initial-install-only operation

Each mode's `storage_policy` gets a **distinct name**
(`bwc_object_store` / `bwc_hot_only` / `bwc_hot_cold`). That is deliberate:
ClickHouse permits only *additive* storage-policy changes on a running server
— it never allows renaming or removing a volume already in use. So toggling
`hotVolume.enabled` or `objectStorage.enabled` against an **already-populated**
cluster fails ClickHouse's own startup validation loudly (the new policy name
doesn't exist yet on disk, or the old one still does) rather than silently
corrupting data.

This is supported at **initial install only**. Migrating a populated cluster
between modes is an out-of-band operation — e.g. a blue-green cutover via
ClickHouse's `remote()` table function or `INSERT SELECT` into a
freshly-installed release in the new mode — not something the chart automates.
This is exactly the mechanism behind the ["Upgrading into the hot/cold
default"](#upgrading-into-the-hotcold-default) hazard above: it fails loudly
for the same additive-only-policy reason, it just now happens by DEFAULT
rather than only on a deliberate mode change.

This loud-failure claim is codified as a repeatable `bwc-minio` e2e check,
not just asserted in a comment: the lane's `mode-toggle` scenario installs
and seeds a real object-storage cluster, `helm upgrade`s it into hot-cold
mode without pinning `hotVolume.enabled: false`, and asserts the bundled
ClickHouse pod's own startup validation refuses with `Unknown storage policy
... (UNKNOWN_POLICY)` — never a silent success, a generic non-ready pod, or a
plain timeout. It has also been reproduced manually against a real
ClickHouse + MinIO fed the chart's own rendered config. See the
[Support / validation matrix](#support--validation-matrix) below for its
exact status pending this leg's first `bwc-minio` CI run.

## Multi-replica consistency

`clickhouse.bundled.replicas > 1` auto-enables a Keeper ensemble and
`ReplicatedMergeTree`. That raises a question independent of storage tiering:
can a single incoming cerberus request's multiple ClickHouse statements — in
particular, every sharded-pushdown time-range fan-out — land on different,
asynchronously-replicating replicas and produce a torn composite result?

Verified directly against `internal/chclient`, `internal/solver/executor.go`,
and the vendored `clickhouse-go/v2` driver: the bundled chart defaults
`CERBERUS_CH_ADDR` to a **single-element** address list (the ClusterIP Service
name), which makes `CERBERUS_CH_CONN_OPEN_STRATEGY` mathematically irrelevant
regardless of value — both of the driver's dial strategies resolve to the same
single address. The actual determinant of which backend pod a connection
reaches is **kube-proxy's per-new-TCP-connection Service DNAT**, which no
client-side setting controls.

A client-side fix (pinning one pooled connection per request) was considered
and rejected: the sharded-pushdown solver dispatches its K shard statements
**concurrently** (`errgroup.SetLimit`), a single native ClickHouse connection
serves one query at a time, and no primitive in `clickhouse-go/v2`'s public
API supports pinning multiple concurrent connections to one replica without a
driver fork.

The chosen fix instead adds `sessionAffinity: ClientIP` (default on) to the
bundled ClickHouse's ClusterIP Service, with a configurable affinity window
(`clickhouse.bundled.service.sessionAffinityTimeoutSeconds`, default `10800`
seconds = 3 hours): every new connection a given cerberus pod opens routes to
the **same** replica for the affinity window, closing cross-replica divergence
within a single multi-statement request — including every sharded-pushdown
fan-out — with **zero code changes** and no dependency on fan-out concurrency.
Set `clickhouse.bundled.service.sessionAffinity: "None"` to opt back out to
plain kube-proxy per-connection routing.

**What this does NOT claim.** `sessionAffinity` does not eliminate temporal
read-skew from concurrent ingestion during a request — ClickHouse has no
cross-statement consistent-snapshot mechanism. That residual risk is
pre-existing to the sharded-pushdown solver against *any* target, replicated
or not; it is not introduced or solved by this mechanism. `CERBERUS_CH_
CONN_OPEN_STRATEGY` remains unexposed by the chart and untouched (binary
default `in_order`); see
[`configuration.md`](configuration.md#dependency-matrix) for the one case
where `round_robin` actually does anything — a manually configured multi-host
`CERBERUS_CH_ADDR` outside the bundled chart's default.

## Support / validation matrix

Backends and modes differ in how far they have been exercised. Treat anything
below "runtime-proven" as needing a real-cloud validation pass against your
own bucket/credentials before production use:

| Configuration                                                                                    | Status                         | How it is validated                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| ------------------------------------------------------------------------------------------------ | ------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Object-store mode, S3 / MinIO, single-node                                                       | **Runtime-proven**             | k3d e2e (`bwc-minio` lane, object-storage scenario): live MinIO, real read/write, placement asserted                                                                                                                                                                                                                                                                                                                                                                                     |
| Hot-only mode, single-node                                                                       | **Runtime-proven**             | k3d e2e (`bwc-minio` lane, hot-only scenario): explicit `schema.ttl`, parts asserted on the local hot disk, no object-store disk/secret/env rendered                                                                                                                                                                                                                                                                                                                                     |
| Hot/cold mode, S3 / MinIO, single-node                                                           | **Runtime-proven**             | k3d e2e (`bwc-minio` lane, hot-cold scenario): fresh rows land on `hot`; rows aged past `tierAfter` are asserted to move onto `cold`                                                                                                                                                                                                                                                                                                                                                     |
| Mode-toggle migration safety (storage-policy rejection)                                          | Implemented, k3d run pending   | `bwc-minio` lane's new `mode-toggle` scenario (issue #3082): install-with-data → incompatible-mode `helm upgrade` → asserts ClickHouse startup failure is exactly `Unknown storage policy ... (UNKNOWN_POLICY)`. Mechanism manually reproduced against real ClickHouse + MinIO fed the chart's own rendered config; the scenario itself has not yet executed in CI (`bwc-minio` is push/nightly/dispatch-only, never a PR gate) — upgrade to **Runtime-proven** once it goes green there |
| Object-store / hot/cold, S3 on real AWS                                                          | Render / kubeconform-validated | `deploy/helm/cerberus/ci/bwc-aws-values.yaml` renders; no live-AWS run                                                                                                                                                                                                                                                                                                                                                                                                                   |
| Object-store / hot/cold, GCS (S3-compat HMAC)                                                    | Render / kubeconform-validated | `deploy/helm/cerberus/ci/bwc-gcs-values.yaml` renders; no live-GCS run                                                                                                                                                                                                                                                                                                                                                                                                                   |
| Object-store / hot/cold, Azure Blob                                                              | Render / kubeconform-validated | `deploy/helm/cerberus/ci/bwc-azure-values.yaml` renders; no live-Azure run                                                                                                                                                                                                                                                                                                                                                                                                               |
| IRSA / GKE / AKS workload identity                                                               | Render / kubeconform-validated | env / SA annotations render; no live cloud-identity run                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| Multi-replica + Keeper (ReplicatedMergeTree)                                                     | Render / kubeconform-validated | `deploy/helm/cerberus/ci/bwc-replicated-values.yaml` renders; no live multi-node run                                                                                                                                                                                                                                                                                                                                                                                                     |
| Dedicated hot-volume PVC (`hotVolume.persistence`)                                               | Render / kubeconform-validated | `deploy/helm/cerberus/ci/bwc-hot-cold-values.yaml` renders a dedicated `hot` volumeClaimTemplate; no live multi-node run                                                                                                                                                                                                                                                                                                                                                                 |
| `dataShards.count: 2` topology (manual k3d, single-replica-per-shard, S3/MinIO)                  | **Infrastructure-validated**   | Manual k3d run: `CREATE TABLE ... ON CLUSTER bwc_cluster` succeeded on both shards; a manual `cluster('bwc_cluster', ...)` query returned correctly merged rows from both. NOT a `just e2e` lane yet, and NOT query-correctness-supported under concurrent solver load — see [#3079](https://github.com/tsouza/cerberus/issues/3079)                                                                                                                                                     |
| `dataShards.count > 1` + `replicas > 1` (multi-replica per shard, classic `ReplicatedMergeTree`) | Render / kubeconform-validated | `chart-render-assert.mjs`'s replicated+dataShards section renders; no live multi-shard-multi-replica run                                                                                                                                                                                                                                                                                                                                                                                 |

Only S3/MinIO single-node, in every one of the three storage modes, is proven
end to end on the CI substrate today (the k3d e2e brings up real MinIO and a
real ClickHouse and writes / reads / tiers through the real disks). The
mode-toggle migration-safety mechanism is implemented and manually reproduced
against real ClickHouse + MinIO outside CI, but its own `bwc-minio` leg has
not yet run — see that row above for the exact status. Every remaining row is
rendered and schema-validated only; the XML wiring is correct by construction
but the cloud round-trip has not been exercised in CI.

## Pre-requisites the chart does NOT handle for you

- **The bucket / container MUST be pre-created.** ClickHouse object disks (S3
  *and* Azure) do not create the bucket/container — point `objectStorage.bucket`
  (or `azure.container`) at one that already exists, or the disk fails on first
  write. (Hot-only mode has no bucket at all — this doesn't apply.)
- **GCS** is reached over its S3-compatible (interop) endpoint with HMAC keys; a
  region/location hint on the bucket that matches your workload's region avoids
  cross-region egress. GCS rejects multi-object delete, so the chart already
  emits `<support_batch_delete>false</support_batch_delete>`.
- **S3 addressing** follows `s3.forcePathStyle`: a custom `endpoint`
  (MinIO/localstack) is always path-style; on real AWS, `false` (default) builds
  a virtual-hosted endpoint (`https://<bucket>.s3.<region>.amazonaws.com/`) and
  `true` builds the legacy path-style form.

## ClickHouse cluster DATA-shard topology (`dataShards.count`)

`clickhouse.bundled.dataShards.count` (cerberus issue
[#3077](https://github.com/tsouza/cerberus/issues/3077), part of epic
[#3074](https://github.com/tsouza/cerberus/issues/3074)) is a **different**
axis than everything above: `replicas` picks how many identical COPIES of the
same dataset exist; `dataShards.count` picks how many independent DATA
PARTITIONS the dataset is split across, fanned out via a `Distributed`-engine
table (`internal/schema/ddl`'s `Config.DataShardCount`, see
[`operations.md`](operations.md#clickhouse-cluster-data-shard-topology-distributed-tables-cerberus-issue-3077)
for the DDL side). It is also unrelated to this chart's own
`{shard}`/`{replica}` Keeper-coordination macros (`schema.replicated.
zookeeperPath`) and to `internal/solver`'s own query-time-range "shard" — see
`internal/chopt/topology.go`'s terminology table for the full picture.

**`count: 1` (default) renders byte-identical to today's chart** — same
StatefulSet/Service names, same `cluster.xml` shape — verified by a real diff
against this chart's pre-#3077 tree across every `ci/*.yaml` fixture, locked
in permanently by `chart-render-assert.mjs`'s own dataShards.count=1
assertions (a one-time base comparison would stop meaning anything once
main eventually **is** this code; the structural assertions keep catching a
future regression regardless).

**`count > 1` renders every shard, INCLUDING index 0**, via a `range` —
`<fullname>-datashard-<i>` StatefulSets, `<headlessName>-datashard-<i>`
headless Services, a `<fullname>-datashard-<i>` ClusterIP Service, each with
its own `macros-datashard-<i>.xml` ConfigMap key — aliased to `macros.xml`
via the `config` ConfigMap volume's own `items[].path` list (a separate
`subPath`-mounted volume was tried first and rejected: mounting one
ConfigMap key via `subPath` into a directory another volume already
populates fails on a real cluster) — replacing today's single hardcoded
`<shard>01</shard>` literal — never a silent partial rename. `remote_servers.xml` (in the shared, cluster-global
`cluster.xml` key) lists every shard's every replica identically on every
pod. Keeper auto-enables from `dataShards.count > 1` alone, independent of
`replicas`: ClickHouse's own `ON CLUSTER` DDL-coordination mechanism (which
every per-shard `CREATE ... ON CLUSTER` statement relies on) needs Keeper
regardless of per-shard replica count. Each per-shard StatefulSet/Service
pair carries a `cerberus.io/data-shard: "<i>"` discriminator label — without
it, every per-shard StatefulSet would share an IDENTICAL, overlapping pod
selector, and multiple StatefulSet controllers reconciling the same
selector would fight over pod ownership.

`CERBERUS_CH_ADDR` defaults to **shard 0's own** per-shard ClusterIP Service
once `count > 1` (the unsuffixed Service no longer exists). A single
connection landing on any one shard's replica is sufficient: the
`Distributed` wrapper table exists identically on every node of every shard
(created `ON CLUSTER`), so that one connection already fans a query out
across the WHOLE cluster internally.

**`replicas > 1` (multi-replica PER shard) together with `dataShards.count >
1`** does NOT reuse the plain `replicas > 1` Replicated-database default
above — [`operations.md`](operations.md#auto-create-schema-single-node-vs-clustered)
calls a Replicated-database engine and an `ON CLUSTER` cluster "mutually
exclusive — pick one." Instead, this combination defaults
`schema.TABLE_ENGINE` to the CLASSIC explicit
`ReplicatedMergeTree('/clickhouse/tables/{shard}/{database}/{table}',
'{replica}')` form (ClickHouse's own built-in `{database}`/`{table}` macros,
no cerberus templating needed) — unless the operator has already set
`schema.replicated.enabled` or `schema.TABLE_ENGINE` themselves, which always
wins. Either way, BOTH mechanisms draw on the SAME `{shard}`/`{replica}`
macro slot `macros-datashard-<i>.xml` populates with a DISTINCT `<shard>`
value per data shard — the "intentional convergence" issue #3077's own
acceptance criteria calls out — and
`internal/schema/ddl`'s `TestDataShardCount_ReplicatedCombination` pins that
the single-data-shard Replicated-database form (an operator's own explicit
choice, or the `replicas == 1` default path) renders correctly alongside
`DataShardCount`.

### `1 -> N` is a MANUAL-MIGRATION-ONLY operation

Bumping `dataShards.count` from `1` to `N > 1` on an ALREADY-populated
deployment is **not** a supported in-place `helm upgrade` — there is no
automatic migration path, and the chart does not attempt one. Renaming even
shard 0's StatefulSet (`<fullname>` -> `<fullname>-datashard-0`) changes the
identity Kubernetes derives `volumeClaimTemplates`-backed PVC names from: the
new StatefulSet schedules its pods against brand-new, EMPTY PVCs (`metadata`,
and, if `hotVolume.persistence.enabled`, the dedicated hot-volume PVC too)
while every PVC that already existed — and the data on it — sits stranded,
orphaned under the OLD, now-unreferenced name.

There is no runbook this chart can automate here: a genuine shard split
means physically repartitioning the existing dataset across the new shard
count, which is an operator-driven, out-of-band data migration (e.g. a
blue-green cutover through `remote()`/`INSERT SELECT` into a freshly
provisioned `count: N` release), not a manifest change. Do not bump
`dataShards.count` on a live, populated deployment without such a plan; a
fresh install at the target `count` has no such hazard.

### Infrastructure-validated only, not yet query-correctness-supported

`count > 1` has been proven at the render/kubeconform layer (every
`ci/*.yaml` fixture plus the dedicated dataShards assertions in
`chart-render-assert.mjs`) plus a REAL manual k3d run performed while
building issue #3077 — not merely a hypothetical plan:

1. A k3d cluster (`k3d cluster create`, single k3s node) with the chart's
   `clickhouse.bundled.enabled=true`, `hotVolume.enabled=true`,
   `objectStorage.enabled=false` (hot-only, no MinIO dependency),
   `dataShards.count=2` rendered and `kubectl apply`'d directly (no cerberus
   image build needed for this DDL-focused proof). Both
   `rn-cerberus-clickhouse-datashard-{0,1}-0` pods and the 3-node Keeper
   ensemble reached `Running`/`1/1 Ready`.
2. `system.macros` on each pod confirmed the per-shard macro split:
   shard 0's pod carries `shard=01`, shard 1's `shard=02`, both
   `cluster=bwc_cluster`; `system.clusters` showed both shards' single
   replica each under `bwc_cluster`.
3. `internal/schema/ddl.RenderAll`'s ACTUAL generated statements (invoked
   directly against `Config{Database: "otel_ddl_test", Cluster:
   "bwc_cluster", DataShardCount: 2}`, the Logs signal) — the real
   production code path, not a hand-written approximation — executed
   without error via `CREATE TABLE ... ON CLUSTER bwc_cluster` against
   **both** shards: `otel_logs_local` (plain `MergeTree()`, since this run
   used `bundled.replicas: 1`), its curated Body-codec `ALTER`, and the
   `otel_logs` `Distributed` wrapper.
4. A row inserted directly into shard 0's `otel_logs_local` and a different
   row inserted directly into shard 1's `otel_logs_local` (the recommended
   direct-to-local write pattern — see
   [`operations.md`](operations.md#clickhouse-cluster-data-shard-topology-distributed-tables-cerberus-issue-3077))
   were BOTH returned by a single `SELECT ... FROM otel_ddl_test.otel_logs`
   issued against the `Distributed` wrapper from shard 0 — and, in a
   separate hand-built table, a manual `cluster('bwc_cluster', 'otel',
   'test_metric_local')` query from shard 0 likewise returned all 4 rows
   split across both shards' local tables.

The cluster was torn down (`k3d cluster delete`) after the run; nothing from
it persists. This proves the ClickHouse-side DDL/query mechanics genuinely
work on a real multi-node cluster. It does **not** prove cerberus's own
compiled binary running this path end-to-end inside the cluster (that needs
a built cerberus image, which is the heavier `just e2e`-style lane), it has
**not** been validated under concurrent solver-driven load, and every
compat/e2e harness in this repository remains single-shard-only. Do not run
`count > 1` in production until the e2e-hardening sub-issue
([#3079](https://github.com/tsouza/cerberus/issues/3079)) closes.

### #3075 compatibility: object-disk path is shard-agnostic; sessionAffinity gains a new gap

Two questions #3077 inherited from #3075's now-merged storage tiering +
sessionAffinity work, checked against the ACTUAL merged shape rather than
assumed:

- **The object-disk path template needs no per-shard macro.** `storage.xml`
  (`cerberus.clickhouse.storageXML`) references no `{shard}`/`{replica}`
  macro anywhere, and is mounted from the SAME cluster-global ConfigMap key
  on every pod of every shard regardless of `dataShards.count` — verified by
  reading the merged template directly. This is safe, not merely
  unchanged-by-oversight: ClickHouse's S3/GCS/Azure object-disk implementation
  names each part's remote object key with its own UUID-derived component
  specifically so multiple independent ClickHouse instances (here, N data
  shards, each with its own independent local `metadata` PVC tracking which
  remote keys belong to it) can safely share one bucket/prefix without
  collision — locating a shard's own parts is a function of that shard's
  local metadata database, never of the shared object-store namespace. So
  `objectStorage.path` stays a single, chart-wide value; nothing here needed
  a `-datashard-<i>` suffix.
- **sessionAffinity's consistency guarantee gains a new gap once a
  `Distributed` fan-out is in play — recorded as an explicit open follow-up,
  not silently assumed solved.** `sessionAffinity: ClientIP` (see
  ["Multi-replica consistency"](#multi-replica-consistency) above) pins a
  cerberus pod's connection to ONE replica **within the shard that
  Service's selector reaches** — under `dataShards.count > 1` each per-shard
  Service pair still provides that guarantee for ITS OWN shard, so a
  multi-statement request that only ever touches shard 0 (e.g. because
  `CERBERUS_CH_ADDR` defaults there) still gets the same closed
  cross-replica-divergence property it always did. What sessionAffinity
  CANNOT reach: once that one pinned connection issues a query against the
  `Distributed` wrapper table, ClickHouse's OWN internal replica-selection
  logic (`load_balancing`, default `round_robin` per query) picks which
  replica of EVERY OTHER shard to read from — a decision made entirely
  inside ClickHouse, invisible to and uncoordinated by any k8s Service.
  Whether two separate statements in the same cerberus-issued multi-statement
  request (e.g. a sharded-pushdown time-range fan-out, now composing with a
  DATA-shard fan-out) can land on two DIFFERENT replicas of the SAME remote
  shard — reopening exactly the divergence risk sessionAffinity exists to
  close, just one level removed — is open. Tracked as
  [#3086](https://github.com/tsouza/cerberus/issues/3086), scoped to the
  settings-verification / e2e-hardening sub-issues (#3078/#3079) that can
  actually observe real cross-shard replica selection under load.

## What's out of scope

Declaring `dataShards.count > 1` "supported" for production query
correctness, and verifying `internal/chopt`'s per-query settings against the
newly-real `Distributed` table, are explicitly later sub-issues of epic #3074
(#3078 settings verification, #3079 e2e hardening) — not this chart's own
scope. `internal/schema/ddl`'s `Config.DataShardCount` also does not yet wire
the local/`Distributed` split for the opt-in DELTA-prefix / downsample-tier /
Loki-label-catalog / Tempo-tag-catalog auxiliary tables (each introduces its
own separately-named table + materialized view); combining `DataShardCount >
1` with any of those is rejected at config-validation time rather than
silently under-provisioning one of them.
