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

| Configuration                                           | Status                         | How it is validated                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| ------------------------------------------------------- | ------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Object-store mode, S3 / MinIO, single-node              | **Runtime-proven**             | k3d e2e (`bwc-minio` lane, object-storage scenario): live MinIO, real read/write, placement asserted                                                                                                                                                                                                                                                                                                                                                                                     |
| Hot-only mode, single-node                              | **Runtime-proven**             | k3d e2e (`bwc-minio` lane, hot-only scenario): explicit `schema.ttl`, parts asserted on the local hot disk, no object-store disk/secret/env rendered                                                                                                                                                                                                                                                                                                                                     |
| Hot/cold mode, S3 / MinIO, single-node                  | **Runtime-proven**             | k3d e2e (`bwc-minio` lane, hot-cold scenario): fresh rows land on `hot`; rows aged past `tierAfter` are asserted to move onto `cold`                                                                                                                                                                                                                                                                                                                                                     |
| Mode-toggle migration safety (storage-policy rejection) | Implemented, k3d run pending   | `bwc-minio` lane's new `mode-toggle` scenario (issue #3082): install-with-data → incompatible-mode `helm upgrade` → asserts ClickHouse startup failure is exactly `Unknown storage policy ... (UNKNOWN_POLICY)`. Mechanism manually reproduced against real ClickHouse + MinIO fed the chart's own rendered config; the scenario itself has not yet executed in CI (`bwc-minio` is push/nightly/dispatch-only, never a PR gate) — upgrade to **Runtime-proven** once it goes green there |
| Object-store / hot/cold, S3 on real AWS                 | Render / kubeconform-validated | `deploy/helm/cerberus/ci/bwc-aws-values.yaml` renders; no live-AWS run                                                                                                                                                                                                                                                                                                                                                                                                                   |
| Object-store / hot/cold, GCS (S3-compat HMAC)           | Render / kubeconform-validated | `deploy/helm/cerberus/ci/bwc-gcs-values.yaml` renders; no live-GCS run                                                                                                                                                                                                                                                                                                                                                                                                                   |
| Object-store / hot/cold, Azure Blob                     | Render / kubeconform-validated | `deploy/helm/cerberus/ci/bwc-azure-values.yaml` renders; no live-Azure run                                                                                                                                                                                                                                                                                                                                                                                                               |
| IRSA / GKE / AKS workload identity                      | Render / kubeconform-validated | env / SA annotations render; no live cloud-identity run                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| Multi-replica + Keeper (ReplicatedMergeTree)            | Render / kubeconform-validated | `deploy/helm/cerberus/ci/bwc-replicated-values.yaml` renders; no live multi-node run                                                                                                                                                                                                                                                                                                                                                                                                     |
| Dedicated hot-volume PVC (`hotVolume.persistence`)      | Render / kubeconform-validated | `deploy/helm/cerberus/ci/bwc-hot-cold-values.yaml` renders a dedicated `hot` volumeClaimTemplate; no live multi-node run                                                                                                                                                                                                                                                                                                                                                                 |

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

## What's out of scope

True ClickHouse-cluster data-sharding (macro-driven `Distributed` tables
spread across independently-scaled shards, as opposed to this chart's
single-shard `replicas` knob) is a separate, materially larger body of work,
tracked in [#3074](https://github.com/tsouza/cerberus/issues/3074).
cerberus's query engine has zero `Distributed`-table-engine usage anywhere in
`internal/` today, so shard-aware StatefulSet/macro generation would be
machinery with no consumer until the query-planning side gains shard-routing
support.
