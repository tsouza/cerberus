# Helm: bundled ClickHouse — background

This document collects the design rationale, rejected alternatives, incident
history, and validation evidence behind
[`helm-clickhouse.md`](helm-clickhouse.md). It answers "why is it built this
way" rather than "what does it do" — nothing here is required to deploy or
operate the bundled ClickHouse correctly; `helm-clickhouse.md` is
self-sufficient for that.

## Why the mode-toggle loud-failure claim is asserted twice

The `bwc-minio` lane's `mode-toggle` scenario is the standing proof that an
incompatible mode change fails ClickHouse's own startup validation rather
than corrupting data. Before that lane existed the claim was also reproduced
manually against a real ClickHouse + MinIO fed the chart's own rendered
config, so the CI assertion codifies an observed behaviour rather than a
hypothesis.

## Why `sessionAffinity`, and the client-side fix that was rejected

`clickhouse.bundled.replicas > 1` auto-enables a Keeper ensemble and
`ReplicatedMergeTree`. That raises a question independent of storage tiering:
can a single incoming cerberus request's multiple ClickHouse statements — in
particular, every sharded-pushdown time-range fan-out — land on different,
asynchronously-replicating replicas and produce a torn composite result?

The answer was established by reading `internal/chclient`,
`internal/solver/executor.go`, and the vendored `clickhouse-go/v2` driver
directly rather than by assuming the driver's connection-strategy knob
mattered: the bundled chart defaults `CERBERUS_CH_ADDR` to a single-element
address list, which makes `CERBERUS_CH_CONN_OPEN_STRATEGY` mathematically
irrelevant regardless of value — both of the driver's dial strategies resolve
to the same single address.

A client-side fix (pinning one pooled connection per request) was considered
and rejected: the sharded-pushdown solver dispatches its K shard statements
**concurrently** (`errgroup.SetLimit`), a single native ClickHouse connection
serves one query at a time, and no primitive in `clickhouse-go/v2`'s public
API supports pinning multiple concurrent connections to one replica without a
driver fork.

`sessionAffinity: ClientIP` was chosen instead because it closes
cross-replica divergence within a single multi-statement request — including
every sharded-pushdown fan-out — with **zero code changes** and no dependency
on fan-out concurrency.

## Why the `dataShards.count: 1` byte-identity claim is structural, not a diff

`count: 1` byte-identity was originally verified by a real diff against this
chart's pre-#3077 tree across every `ci/*.yaml` fixture. That one-time base
comparison would stop meaning anything once `main` eventually **is** this
code, so the claim was locked in permanently by `chart-render-assert.mjs`'s
own `dataShards.count=1` assertions, which keep catching a future regression
regardless.

## Why the per-shard macro ConfigMap key is aliased, not `subPath`-mounted

`macros-datashard-<i>.xml` is aliased to `macros.xml` via the `config`
ConfigMap volume's own `items[].path` list. A separate `subPath`-mounted
volume was tried first and rejected: mounting one ConfigMap key via `subPath`
into a directory another volume already populates fails on a real cluster.

## Why the interserver secret is required at `count > 1` but not at `count: 1`

A single shard with `replicas > 1` forwards across replicas the same way a
multi-shard deployment does and should set `interserverSecret` too. It is not
*required* there only because requiring it would break an existing
single-shard deployment on upgrade.

## Why each per-shard StatefulSet carries a data-shard discriminator label

Without the `cerberus.io/data-shard: "<i>"` label, every per-shard
StatefulSet would share an IDENTICAL, overlapping pod selector, and multiple
StatefulSet controllers reconciling the same selector would fight over pod
ownership.

## Why the `{shard}`/`{replica}` macro slot is shared deliberately

Under `dataShards.count > 1` with `replicas > 1`, both the classic
`ReplicatedMergeTree` engine expression and the chart's own Keeper
coordination draw on the SAME `{shard}`/`{replica}` macro slot that
`macros-datashard-<i>.xml` populates with a distinct `<shard>` value per data
shard. That is the "intentional convergence" issue #3077's own acceptance
criteria calls out, not an accidental collision.

## The manual k3d run behind the `count > 1` validation status

Before the `datashard` e2e lane existed, `count > 1` was proven by a REAL
manual k3d run performed while building issue #3077 — not merely a
hypothetical plan:

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
   [`operations.md`](operations.md#clickhouse-cluster-data-shard-topology-distributed-engine-tables))
   were BOTH returned by a single `SELECT ... FROM otel_ddl_test.otel_logs`
   issued against the `Distributed` wrapper from shard 0 — and, in a
   separate hand-built table, a manual `cluster('bwc_cluster', 'otel',
   'test_metric_local')` query from shard 0 likewise returned all 4 rows
   split across both shards' local tables.

The cluster was torn down (`k3d cluster delete`) after the run; nothing from
it persists. This proves the ClickHouse-side DDL/query mechanics genuinely
work on a real multi-node cluster. Since then the `datashard` e2e lane has
taken over as the standing proof.

## Why the object-disk path needs no per-shard macro

`storage.xml` referencing no `{shard}`/`{replica}` macro is safe, not merely
unchanged-by-oversight. ClickHouse's S3/GCS/Azure object-disk implementation
names each part's remote object key with its own UUID-derived component
specifically so multiple independent ClickHouse instances (here, N data
shards, each with its own independent local `metadata` PVC tracking which
remote keys belong to it) can safely share one bucket/prefix without
collision — locating a shard's own parts is a function of that shard's local
metadata database, never of the shared object-store namespace. That is why
`objectStorage.path` stays a single, chart-wide value and nothing needed a
`-datashard-<i>` suffix.

## Why `load_balancing` is pinned, and how issue #3086 got there

Two questions were inherited from issue #3075's storage tiering +
sessionAffinity work when issue #3077 added data shards, and both were
checked against the ACTUAL merged shape rather than assumed. The object-disk
question is above; this is the second.

`sessionAffinity: ClientIP` pins a cerberus pod's connection to ONE replica
within the shard that Service's selector reaches. What it CANNOT reach: once
that one pinned connection issues a query against the `Distributed` wrapper
table, ClickHouse's OWN internal replica-selection logic (the
`load_balancing` setting) picks which replica of EVERY OTHER shard to read
from — a decision made entirely inside ClickHouse, invisible to and
uncoordinated by any k8s Service. Whether two separate statements in the same
cerberus-issued multi-statement request (e.g. a sharded-pushdown time-range
fan-out, now composing with a DATA-shard fan-out) could land on two DIFFERENT
replicas of the SAME remote shard — reopening exactly the divergence risk
sessionAffinity exists to close, just one level removed — was the open
question issue #3086 set out to answer.

**Resolution: closed by an unconditional `internal/chclient` settings pin,
not merely documented as a residual risk.** ClickHouse's own default is
`load_balancing=random` (verified against `src/Core/Settings.cpp` at the
pinned `v25.8.1.5101-lts` tag — NOT `round_robin` as that issue's own problem
statement assumed before the source was actually read), which picks
arbitrarily among a shard's least-erroring replicas on EVERY call — the
DEFAULT itself is what reopens the divergence risk. Per
`src/Common/GetPriorityForLoadBalancing.cpp` (same pinned tag),
`first_or_random` gives priority 0 to the replica at the configured offset
and priority 1 to every other replica, so — absent any recorded connection
errors — EVERY statement against a remote shard's `Distributed` connection
pool deterministically selects that ONE offset-0 replica. The pin mirrors the
issue #3078 `skip_unavailable_shards` /
`fallback_to_stale_replicas_for_distributed_queries` pins.

The selection state (`PoolWithFailoverBase::Pool::error_count`) lives on the
ClickHouse SERVER process cerberus's own sessionAffinity already pins to, not
on any per-client state, so the resulting guarantee is actually STRONGER than
sessionAffinity's own: every statement from every cerberus pod converges on
the same physical replica per remote shard, cluster-wide, for as long as that
replica stays healthy — not merely "for the lifetime of one client's affinity
window." `internal/chclient/distributed_query_settings.go`'s own doc comment
carries the full citation chain, including why `first_or_random` was chosen
over the equally deterministic `in_order`.

**The read-concentration trade-off is PERMANENT, not a bug to keep chasing.**
Concentrating every cerberus read against a remote shard's `Distributed`
connection on that shard's ONE offset-0 replica is the same
correctness-over-throughput trade cerberus already makes for the issue #3078
pins, applied one level further down the replica-selection stack. An operator
who needs read-scaling across a shard's replicas overrides `load_balancing`
via a server-side settings profile; cerberus's own default never silently
reverts to unpredictable divergence risk instead.

## Why the router-calibration corpus emits its own engine

The corpus never reuses `schema.TABLE_ENGINE`'s own expression. It reads that
knob as a declaration and emits its own engine, because the expression's
Keeper path belongs to the signal tables and its engine family was chosen for
them — a `ReplacingMergeTree` pinned there would dedupe corpus rows by their
sort key. The bare `ReplicatedMergeTree` form needs no path of its own: the
server derives one per table from `default_replica_path`.

## Why the `Distributed`-wrapper gap for the corpus is a boundary, not a defect

Under `dataShards.count > 1` the corpus gets no `Distributed` wrapper, so
rows never leave the shard they were written on — the same
local/`Distributed` split that is base-signal-tables-only. It costs nothing
on the query path, which never reads the corpus, and within each shard's
replica set the corpus does replicate.

## Why the auxiliary-feature carve-out exists

The verification of `internal/chopt`'s per-query settings against a real
`Distributed` table and the runtime proof of `dataShards.count > 1` under
concurrent load were delivered by epic #3074's later sub-issues, not by the
chart itself. The four opt-in auxiliary features each introduce a
separately-named table plus its own materialized view, and none of them is
wired for the local/`Distributed` split — so combining them with
`DataShardCount > 1` is rejected at config-validation time rather than
silently under-provisioning one of those tables.

## Why the ClickHouse pods carry their own shutdown budget

The ClickHouse StatefulSets had no `terminationGracePeriodSeconds`, so
Kubernetes gave them its 30-second default, while ClickHouse 26.8 raised its
own `shutdown_wait_unfinished` default from 5 to 120 seconds, per ClickHouse
PR 110838. Upgrading the bundled image alone would have let the kubelet SIGKILL
a server still inside its own shutdown. Measured against 26.6.8.7 and
26.8.10.6 in Docker: with `shutdown_wait_unfinished_queries: 1`, a 15-second
query started before SIGTERM returned its full result and the server exited 0
after 14.7 s; a 40-second query under a 30-second wait was cut at 30.2 s, the
server still exiting 0. With the flag at its default `0`, the same query was
cancelled at SIGTERM (`QUERY_WAS_CANCELLED`) and the server exited in about
3 s. An idle client connection held shutdown for 5 s on 26.6 and about 8–11 s
on 26.8 before the server closed it itself.

Draining rather than cancelling is the chosen behaviour because a rolling
update is routine: a cancelled query surfaces to a Grafana user as an error,
while a drained one finishes. The wait follows cerberus's own query timeout
(`max_execution_time` on every data-plane query), the longest any cerberus
query can run, so a drain never has to cut one. A first version rendered a
literal 120 beside the binary's 2-minute default, and raising `query.timeout`
left the wait behind; deriving it from the same values, with the binary
default held equal by `TestChartQueryTimeoutDefaultMatchesBinary`, removes the
second copy. An explicit shorter wait is refused rather than accepted, since
it can only mean a rollout that cancels queries cerberus still admits.

## Why the format pin is guarded by the image tag

The pin arrives as a new chart default, so an existing release whose bundled
image predates 26.6 (25.8, a 26.3 LTS, an Altinity build) would receive it on
its next `helm upgrade`: the config checksum changes, every ClickHouse pod
restarts, and each exits at startup with `UNKNOWN_SETTING` (exit 115, measured
on 26.5.7.64 and 25.3.14.14) — an outage on one replica, a stuck rollout on
several. Failing the render instead stops the upgrade before anything
restarts. Dropping the setting silently for an older tag was rejected: the
same map carries an operator's explicit settings, which Helm cannot tell apart
from the default, and silently discarding an explicit setting is worse than a
render error that names it. A tag with no version is rendered unchecked
because refusing it would break every digest-pinned release, the practice
most worth keeping.

The server's work after the last query — stopping background pools, flushing
system logs and in-memory buffers — took under a second on an idle server.
`overheadSeconds: 30` is headroom for the same work on a loaded server over an
object store, not a measured cost; it is a named value so an operator can
tune it without touching the wait.

The existing top-level `terminationGracePeriodSeconds` was not reused: it is
the cerberus process's grace, sized for cerberus's own 10-second shutdown
context, and tying the two would either kill ClickHouse early or hold every
cerberus rollout for two and a half minutes. Keeper stops in seconds and has
no query drain, so it keeps the Kubernetes default rather than inheriting a
budget sized for queries.

## Why the chart pins `packed_skip_index_max_bytes`

The upgrade contract was drafted around text indexes, where ClickHouse #111803
added `text_index_serialization_version` (the issue called it
`text_index_version`). Measured across 26.3.33, 26.5.7, 26.6.8, 26.7.13 and
26.8.10 with cerberus's own schema, on shared volumes and in a two-replica
mixed-version cluster:

- 26.6 and 26.7 write text indexes as `v1_with_codec`, 26.8 as
  `v2_with_positions`. 26.6, 26.7 and 26.8 read all three versions; 26.3 and
  26.5 read only `v0_initial` and fail with `Unsupported version of sparse
  index (N)`. On the bundled path, which never shipped a server between 26.2
  and 26.5 (it went from 25.8, which has no text index, to 26.6), no text-index
  pin is needed.
- 26.8 also packs every skip index smaller than 1 MiB into one
  `skp_idx.packed` archive per part whenever it merges or mutates a part
  (`packed_skip_index_max_bytes`, default `1048576`; inserted parts were not
  packed). 26.7 reads the archive; 26.6 fails with `UNKNOWN_FORMAT_VERSION:
  Unknown format (1) of packed data`, on reads and on replica fetches. In the
  mixed-version cluster, the 26.6 replica could not fetch the part the 26.8
  replica produced by `MATERIALIZE INDEX`, so the mutation never completed on
  it. Every cerberus table carries small `minmax` / `bloom_filter` /
  `tokenbf_v1` indices, so this affects every signal, not only logs.

26.6 and 26.7 already default the setting to `0`, so the pin changes nothing
on the bundled line and only takes effect once a newer image is deployed. It
lives in the default of the existing `settings` pass-through rather than in
the template so that it is visible, overridable, and advanced by the same
mechanism an operator already uses for MergeTree settings. The cost of the pin
is the object count packing saves on object storage; the cost of not pinning is
a rollback that cannot read its own data.

The profile-level `compatibility` setting, which ClickHouse documents for
rolling upgrades, also turned packing off for merges run from a client
session with that profile, but it resets every query-setting default newer
than the chosen version as well, and whether background merges honour it was
not established; the MergeTree setting has neither problem.

## The detach-on-startup behaviour behind the rollback table

Rolling 26.8 back to 26.6 after a `MATERIALIZE INDEX` mutation wrote a packed
part, with the unmutated part still on disk, 26.6 detached the new part as
broken at startup and served the older one: no read error, but the mutation's
result silently gone. After `OPTIMIZE ... FINAL` (no covered part left), 26.6
kept the packed part active and failed every read of it. Both outcomes are why
the rollback procedure rewrites under the pin before the older image starts.
