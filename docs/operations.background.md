# Operations — background

This document collects the design rationale, rejected alternatives, incident
history, and measurements behind [`operations.md`](operations.md). It answers
"why is it built this way" rather than "what does it do" — nothing here is
required to run or operate cerberus correctly; `operations.md` is
self-sufficient for that.

## Why the composed cursor carries its own longer teardown budget

Because the cancel `CloseCursor` holds there is an ANCESTOR of
every per-shard query context, a composed cursor reports its own longer budget
through `chclient.ComposedCursor`: nesting its teardown inside a single
connection's drain budget would fire the ancestor cancel at exactly the moment
the K shard sockets were being released cleanly, destroying all of them. It is
also why the composed teardown itself is not counted — its children each count
their own.

## Why the failure-driven route memo defaults on

It is on by default because it only ever turns a
FAILURE into a slower answer and can never change a result — it is what makes
`auto` mean "start on route A and escalate on real evidence" rather than
"guess up front and never learn".

## How the native-rate path's parity was validated

**Parity.** Validated on the chDB substrate (26.5) by a dual-emit test
(`internal/chsql/range_window_grid_native_chdb_test.go`) that runs the fan-out and
the native path on the same seed and compares decoded float64 grids. The 26.5
substrate is above the 25.9 auto floor, so it already carries the left-open
window fix and the native path exercised here uses the same half-open membership
as PromQL — there is no closed-vs-left-open boundary difference to work around.
The seed keeps samples away from the window edges as belt-and-suspenders; the
test pins the emit shape and the extrapolation arithmetic, while the 25.9 floor
is what guarantees the boundary correctness in production. On the pinned
12-sample ramp 8 of 9 grid cells are bit-identical and
1 diverges by exactly 1 ULP (the native value is the next double up from the
correctly-rounded fan-out value — a sub-observable float-order difference, both
render `0.12`).
The test enforces a tight bound rather than the raw fixture count: **at most two
cells may diverge, each by no more than 1 ULP** (`maxDualEmitUlpDivergentCells
= 2`); any cell off by more than 1 ULP, or a third divergent cell, fails the
test as an arithmetic regression. The maturity label stays experimental because
the path rides ClickHouse's experimental setting, but it has since been
validated against a real (non-chDB) server with that setting enforced — found
result-correct at flat memory — which is why `auto` now selects it on ≥ 25.9
rather than leaving it opt-in.

## Why the ClickHouse 26.5 defect is not worked around in emitted SQL

Cerberus does not work around this in emitted SQL: the shapes that avoid it do
so incidentally, so pinning a golden to one would encode an upstream bug as a
cerberus invariant.

## Why cerberus ships no authentication

That is a deliberate scope decision — the same one the
listener makes about TLS — but it is load-bearing for how you deploy
the process, so it is spelled out here rather than left to be inferred
from the absence of a `CERBERUS_AUTH_*` knob.

## Why the tail budget is sized separately from the Loki request budget

Sizing them separately is what keeps the two from interfering. Were
`/tail` drawn from the Loki request budget, `CERBERUS_ADMIT_LOKI`
concurrent Live-tail sessions would occupy every Loki slot indefinitely
and every subsequent `/query`, `/query_range`, `/labels`, `/series`,
`/patterns` and Drilldown probe on that replica would 503 until a tail
client disconnected. Occupancy would only ratchet one way, and the
symptom — "the Loki datasource is down", on a healthy pod with no
elevated CPU — points nowhere near the cause.

## Why a tail refused by the tail cap closes with a `1013` frame

A tail refused by the tail cap is closed with a `1013` (`Try Again Later`)
close frame, the same shape
reference Loki answers its own `querier.max-concurrent-tail-requests`
with — every other admitted route refuses at the HTTP upgrade instead
(`503` + `Retry-After: 1`), but a WebSocket client has no way to read an
HTTP header once the handshake has already gone through, so `/tail`'s
rejection has to live at the WebSocket layer to be actionable
(issue [#2048](https://github.com/tsouza/cerberus/issues/2048)).

## Workload scheduling: the direct verification against ClickHouse 26.6

**Verified directly against a real ClickHouse 26.6** (not merely cited from
docs — this repo's session found more than one ClickHouse doc claim not hold
up against the actually-deployed version): `CREATE RESOURCE cpu (MASTER
THREAD, WORKER THREAD)`, `CREATE RESOURCE io_default (READ DISK default,
WRITE DISK default)`, and a `CREATE WORKLOAD` hierarchy with per-child
`weight` all worked exactly as documented, and `system.scheduler` showed the
resulting weighted fair-share nodes live and active for both resources. This
is production-quality, GA machinery on the deployed version, not an
experimental flag.

## Workload scheduling: measurements, ownership, and the S3-backed disk

**Measured under combined load** (real measurement against the local
`clickhouse/clickhouse-server:26.6` compose service, not an estimate): a
synthetic OTel-collector-style ingest loop — continuous small (500-row)
batched `INSERT`s into a MergeTree table seeded to 3M rows / dozens of active
parts — ran for 60s concurrently with 16 parallel heavy `GROUP BY` +
`quantiles()` read queries (`max_threads=8` each), with and without the
recipe above (`max_concurrent_threads=8` on `all`, `default` weight 6 vs
`cerberus_queries` weight 1). With isolation configured, ingest p95/p99
latency improved (~135ms → ~124ms p95, ~160ms → ~149ms p99) and insert
throughput rose slightly (~9.7/s → ~10.1/s), at the cost of ~26% fewer
completed read queries in the same window (476 → 352) — the intended
tradeoff, reads yielding CPU/IO share to protect ingest. The effect size was
modest on this single 8-core dev box with local disk: stock ClickHouse's
separate merge thread pool (above) already absorbs a fair amount of
contention before any scheduling is configured, so the gap only widens under
genuinely saturated CPU/IO — the production case this feature targets (a
shared node under real dashboard-storm + ingest-spike load, or S3-backed
merge IO competing with query S3 reads) is harder to reproduce on a
lightly-loaded local box and is where the isolation is expected to matter
most.

**Ownership: this is a documented operator recipe, not cerberus-provisioned
DDL.** `WORKLOAD` / `RESOURCE` are server objects, and in cerberus's common
deployment shape it is a stateless gateway pointed at a
bring-your-own ClickHouse cluster the operator owns and may already run
other, non-cerberus workloads against — auto-issuing `CREATE
WORKLOAD`/`CREATE RESOURCE` DDL, or `ALTER USER`/`CREATE SETTINGS PROFILE`
against that cluster, from inside cerberus would be a real ownership and
blast-radius overreach (and `merge_workload`/`mutation_workload` affect
EVERY table on the cluster, not just cerberus's own). Note also that a
ClickHouse user provisioned via `CLICKHOUSE_USER`/`CLICKHOUSE_PASSWORD`
env-var (XML `users.xml`) config — as the demo compose stack's `clickhouse`
service is — lives in a read-only access storage from SQL's own
perspective: `ALTER USER ... SETTINGS PROFILE ...` fails with
`ACCESS_STORAGE_READONLY` against it, so a settings-profile-based wiring is
NOT reliable in general. `CERBERUS_CH_QUERY_WORKLOAD` sidesteps this
entirely: it rides the SAME per-query settings-stamping path cerberus
already uses for `ts_grid_range` and the query result cache, needing no
ClickHouse RBAC privilege at all on the connection cerberus uses.

**S3-backed disk contention (cerberus issue #2847).** The measurement above
ran against ClickHouse's local (`default`) disk on a single 8-core dev box.
The recipe's `READ DISK` / `WRITE DISK` clause is disk-agnostic by
construction, so the DDL itself needs no changes for an S3 disk — `CREATE
RESOURCE io_s3 (READ DISK s3, WRITE DISK s3)` against an S3 (or
S3-compatible) disk named `s3` is accepted exactly like `io_default` was.
What was not yet exercised is the *behavioral* question: does the
weight-based fair share still deliver the same protection when the disk
underneath is an S3 bucket with a materially different latency/throughput/
queueing profile than local disk?

**Verified directly against a real ClickHouse 26.6 backed by MinIO**
(`minio/minio`, S3-compatible, run locally in Docker — no cloud AWS S3
needed): same table shape (MergeTree, 3M rows seeded, same synthetic
500-row batched `INSERT` loop and 16 parallel heavy `GROUP BY` +
`quantiles()` reads at `max_threads=8`), same recipe
(`max_concurrent_threads=8` on `all`, `default` weight 6 vs
`cerberus_queries` weight 1), disk name swapped for an S3 disk pointed at
MinIO. `system.scheduler` showed the same weighted fair-share nodes live
for both the `cpu` and `io_s3` resources — the DDL and the scheduler wiring
transfer unchanged.

The measured *effect size* does not transfer unchanged, and is
substantially larger on the S3-backed disk than on local disk. Averaged
over two repeated 60s runs each (individual runs agreed within ~5%):

| Metric (60s combined load)         | Local disk (#2785)    | S3/MinIO (#2847)    |
| ---------------------------------- | --------------------- | ------------------- |
| Ingest p95 latency, no split       | ~135ms                | ~703ms              |
| Ingest p95 latency, with split     | ~124ms (-8%)          | ~602ms (-14.5%)     |
| Ingest p99 latency, no split       | ~160ms                | ~1485ms             |
| Ingest p99 latency, with split     | ~149ms (-7%)          | ~1039ms (-30%)      |
| Ingest throughput, no split        | ~9.7/s                | ~3.1/s              |
| Ingest throughput, with split      | ~10.1/s (+4%)         | ~5.5/s (+80%)       |
| Completed heavy reads, no split    | 476                   | 229                 |
| Completed heavy reads, with split  | 352 (-26%)            | 177 (-23%)          |

The read-side cost of the split (roughly a quarter of heavy-read throughput
given up to protect ingest) transfers cleanly: -26% on local disk, -23% on
S3/MinIO, the same order of magnitude. The ingest-side *protection*, on the
other hand, is far more pronounced on S3: p99 latency improves ~30% (vs
~7% on local disk) and ingest throughput nearly doubles (vs a ~4% bump on
local disk). This is consistent with the mechanism #2785 already
identified: stock ClickHouse's separate merge thread pool absorbs a fair
amount of query/merge contention for free on fast local disk, so scheduling
has modest headroom to add on top of it. S3 PUT/GET latency is an order of
magnitude higher and far more variable than local disk, so the SAME 16
concurrent heavy readers starve ingest's S3 merge/flush traffic much harder
when nothing arbitrates between them — which is exactly the headroom the
`default`-weight-6-vs-`cerberus_queries`-weight-1 split recovers. In short:
**do not assume the local-disk percentages in the table above as a
conservative estimate for an S3-backed deployment** — on S3, workload
scheduling both matters more (bigger ingest win) and costs about the same
(similar read-throughput give-up) as it does on local disk.

## Why the database create needs a bootstrap connection

> **Why the database create needs a bootstrap connection.** ClickHouse rejects
> *every* statement (even `CREATE DATABASE`) on a session whose default database
> doesn't exist — and the configured database (`CERBERUS_CH_DATABASE`) is the
> session default, which is exactly the one that may be missing on a cold
> cluster. So when cerberus creates the database it does so over a one-time
> connection bound to ClickHouse's always-present `default` database; the
> fully-qualified `<db>.<table>` table creates run from there too.

## Why `perShardMemoryBytes` divides by `DataShardCount`

That last sentence is the whole reason the formula divides by
`DataShardCount`: the setting name forwards to every shard UNCHANGED (per the
two source facts above), but its enforcement is **per shard, independently**
— each of the `DataShardCount` shards a fan-out touches gets its OWN,
separate `max_memory_usage` budget at the SAME value, not one budget shared
across them. Sending the un-apportioned `cap` value would let a `K`-way
`kEff` fan-out against `N` data shards use up to `K × N × cap` bytes of
aggregate ClickHouse-side memory before any single query hits its own limit —
exactly the amplification `perShardMemoryBytes` exists to divide back out.
Live-load confirmation that the formula holds under real concurrent
multi-shard traffic is issue #3079's job, not this one's.

The formula's `kEff` term only covers a genuine solver K-shard fan-out.
"Route A" — every data-plane query the solver never splits, `kEff == 1` in
the vocabulary above — still reaches the SAME `Distributed` table and fans
out across the SAME `DataShardCount` nodes; ClickHouse decides that fan-out
inside the `Distributed` engine, not cerberus's own solver, so it happens
regardless of whether anything upstream decided to shard. Cerberus issue #3122
(found live on dispatch run 34020019580: every route-A statement's
`system.query_log` row recorded the RAW, unapportioned cap) closed this gap
by apportioning route A's own base cap by `DataShardCount` ALONE, in
`chclient.Client.querySettings` — the exact `kEff=1` case of the same
formula, sharing its divide-and-floor arithmetic with `executor.go` via
`chclient.ApportionMemoryBytes` so the two sites cannot independently drift.

## The Distributed error taxonomy's third named code

**A note on the issue's own third named code.** The issue's Problem
statement named `SHARD_HAS_NO_REPLICAS` as the third error to cover. A
`search/code` sweep of `github.com/ClickHouse/ClickHouse` found **zero**
matches for that identifier anywhere in the codebase — it does not exist.
The nearest same-shaped name, `SHARD_HAS_NO_CONNECTIONS` (code 297,
`src/Interpreters/Cluster.cpp`), is a cluster-CONFIG parse-time error ("No
cluster elements (shard, node) specified in config"), never raised by a
running query, so using it here would misrepresent a config-loading bug as a
query-time failure. `ALL_CONNECTION_TRIES_FAILED` (already named correctly
elsewhere in the same issue sentence) is the real runtime "a shard has no
usable replica" code, and `ALL_REPLICAS_ARE_STALE` is the real "replica
staleness path" the issue also asked for — both are covered above.
`TOO_MANY_UNAVAILABLE_SHARDS` (code 904) exists too, but is unreachable under
cerberus's own `skip_unavailable_shards=0` pin (it only fires when skipping
is ENABLED and too many shards get skipped), so it is not wired into the
taxonomy.

## The multi-shard subquery execution plan behind ClickHouse#29332

**Concrete subquery/derived-table shapes in `internal/chsql`/`internal/chplan`
issue #3079 should execute against a real multi-shard cluster**, expected
results in parentheses (every shape below must match its single-shard/chDB
answer exactly — that equality, not a specific number, is the pass
criterion):

1. **Priority 1 — native-histogram `histogram_quantile()` over a
   `Distributed` table** (`internal/chplan/histogram_quantile_native.go:39`'s
   `ScalarSubquery`, reached with `enable_analyzer=0` forced by
   `applyNativeHistogramAnalyzerFix`): `histogram_quantile(0.9,
   sum(rate(demo_exp_hist[5m])))` and the plain
   `histogram_quantile(0.9, demo_exp_hist)` selector form, both against a
   `dataShards.count: 2+` cluster with series distributed across shards by
   the sharding key. (Expected: identical quantile to the same query against
   a single-shard/chDB copy of the same data — this is the shape most
   directly analogous to `#29332`'s own reproduction, a `WHERE`/aggregate
   wrapping a `ScalarSubquery` over a `Distributed` table under the legacy
   analyzer.)
2. **`scalar(<vector>)` PromQL queries in general**
   (`internal/promql`'s lowering into `chplan.ScalarSubquery`, e.g.
   `internal/chplan/range_window.go:210`'s range-window scalar argument) —
   under the DEFAULT analyzer (`enable_analyzer=1`), as a baseline-regression
   control proving the new-analyzer fix genuinely holds on a real multi-shard
   cluster and not just on ClickHouse's own single-node test suite. (Expected:
   identical to single-shard.)
3. **TraceQL `/api/search` root-lookup `InSubquery`**
   (`internal/chsql/metrics_compare.go`'s `cohortPred`/`bindRootLookupTraceIDTsEnvelope`,
   `TraceId IN (<subquery>)`) against a `Distributed` spans table, both a
   plain search and a `compare()` query (which nests the InSubquery inside
   an additional bounded root leg). (Expected: identical trace set to
   single-shard.)
4. **`BoundedTraceScope`'s structure-tab top-N gate**
   (`internal/chplan/bounded_trace_scope_bind.go`, another `TraceId IN
   (<subquery>)` shape with an additional row-count bound) against a
   `Distributed` spans table. (Expected: identical bounded trace set.)
5. **TraceQL structural join's recursive CTE**
   (`internal/chsql/structural_join.go`'s `WITH RECURSIVE`, reached by every
   `>`/`<`/`>>`/`<<` structural query) against a `Distributed` spans table —
   not the same predicate-pushdown mechanism `#29332` names, but the other
   large recursive/derived-table shape in the emitter, worth a pass since
   recursive CTEs interacting with `Distributed` fan-out have no ClickHouse
   documentation either way. (Expected: identical structural match set to
   single-shard; see also [Recursive-CTE parallelism
   in operations.md](operations.md#recursive-cte-parallelism--recommend-clickhouse--266-for-trace-structure)
   for an unrelated, already-tracked recursive-CTE caveat.)
6. **`NotInSubquery` gap-detection** (`internal/chsql/absent_over_time.go:129`,
   `absent_over_time()`'s covered-anchor exclusion) against a `Distributed`
   metrics table. (Expected: identical gap set to single-shard.)

Live execution of this plan against a real `dataShards.count: 2` (and,
matching the epic's own `N=4` over-subscription case, `count: 4`) cluster is
issue #3079's job; this sub-issue's scope is limited to writing the plan
down with concrete, emitter-grounded shapes.

Issue #3079 executes shapes 1, 2, 4, and 6 as permanent, unconditional Go
`test/e2e` tests (`test/e2e/e2e_datashard_subquery_test.go`) that run in
**every** `just e2e-run` invocation — the standard single-shard lane, the
`bwc-minio` lane, and the new `datashard` lane below — so the SAME pinned
assertions running byte-identically across lanes is itself the "matches a
single-shard reference run" proof, rather than a separate diff step. Shapes 3
and 5 already had dedicated coverage before this issue
(`TestTempoSearch`/`TestTempoSearch_StructuralChild`,
`test/e2e/e2e_tempo_test.go` / `e2e_tempo_extra_test.go`), which also now run
against the `Distributed` target via the same lane; they were not duplicated.

## Why the compat and migration lanes stay single-shard

The three differential harnesses (`docs/compatibility.md`) each diff
cerberus's translation of a query language against a REAL reference
implementation — Prometheus, Loki, Tempo — none of which has any concept of
a ClickHouse data shard at all. What they exist to catch is query-LANGUAGE
fidelity: does cerberus's PromQL/LogQL/TraceQL lowering produce the same
answer the reference engine would. A `Distributed` table is, by design,
architecturally transparent to a correct query against it — same logical
dataset, same query surface, same expected answer — so a second, N-shard
copy of each harness's reference-comparable ClickHouse target would exercise
no code path these harnesses were built to catch; it would only roughly
double each lane's already-heaviest runtime for a dimension none of the
three reference backends can even express an opinion about.

What multi-shard ACTUALLY puts at risk is ClickHouse's own distributed-query
execution — the `#29332` legacy-analyzer/subquery interaction, the
admission-control fan-out ceiling, the per-shard memory apportionment — and
that risk is real but has nothing to do with any of the three query
languages' own semantics. Issue #3079's `datashard` e2e leg (previous
section) targets it directly, with real `system.query_log` evidence a
three-way reference diff has no way to produce (none of Prometheus, Loki, or
Tempo runs on ClickHouse, so none can observe ClickHouse's own shard
fan-out).

`cerberus migrate`'s own verify step (`docs/migration.md`'s Step 10) replays
real queries against a live cerberus + ClickHouse pair and diffs the
answers against the source Prometheus — a migration-time correctness tool,
not a ClickHouse-topology-aware one. Its code
(`internal/migrate`/`cmd/cerberus`'s migrate command surface) carries zero
`DataShardCount`/`dataShards` awareness anywhere: it issues the same queries
through the same cerberus query engine this issue's `datashard` lane
already validates against a real `Distributed` target, so `cerberus
migrate`'s own correctness is inherited from that validation rather than
needing a parallel one. The topology a migrating operator's ClickHouse
happens to run — single-shard today, or a `dataShards.count > 1` deployment
following this epic's own manual-migration runbook
(`docs/helm-clickhouse.md`) — is the operator's own choice and orthogonal to
what Step 10 checks.

## Why the AggregationTemporality skip index exists

**Why it exists (issue #2458).** A range-mode `rate()`/`increase()` window
over a temporality-bearing counter can split into a native
`timeSeriesRateToGrid` arm (fed only non-DELTA rows) and a fan-out arm (fed
only DELTA rows) — see `NativeRateLowerer.LowerRate` in
`internal/promql/lower_strategy.go`. Both arms scan the SAME base table with
the SAME `MetricName`/`Attributes` predicate, differing only in a trailing
`AggregationTemporality` conjunct. That column is not part of the table's
`ORDER BY` (`MetricName, Attributes, ServiceName, TimeUnix`), so without a
skip index ClickHouse cannot prune a single granule on it and reads every
matching row from BOTH arms — a confirmed, reproducible 2.00x `read_rows`
ratio against table size, measured on real production-shaped data. Real OTel
deployments set `AggregationTemporality` once per exporter configuration, so
a given series' samples land in temporality-homogeneous runs almost always;
the minmax index lets ClickHouse recognize a homogeneous granule and skip it
entirely for whichever arm's predicate does not match, without requiring any
change to the plan shape or the table's `ORDER BY`. The same index also
prunes the ordinary single-arm case: any plain scan carrying an
`AggregationTemporality` predicate (the fan-out emitter's own per-row branch,
or a future consumer) benefits identically.

## Why the curated column-statistics set is what it is

**Why these columns, and why two ALTERs per table.** ServiceName / MetricName
/ SpanName / TraceId are all `String` or `LowCardinality(String)` in the
upstream OTel-CH schema, and ClickHouse rejects `minmax` and `tdigest`
outright on a string-typed column (`Code: 708, ILLEGAL_STATISTICS` —
verified against a live ClickHouse 26.5 server, not merely read off the
docs), so they carry `uniq` only. That is also the semantically right choice
for an equality-filtered identity column: `minmax` exists for RANGE
predicates a string equality never issues. AggregationTemporality (`Int32`)
and SeverityNumber (`UInt8`) are numeric, so they carry `minmax, uniq` in
their OWN ALTER — ClickHouse applies one TYPE list to every column in a
single ADD STATISTICS statement, so a string column and a numeric column
sharing supported types can never share one statement. Duration (`UInt64`)
additionally carries `tdigest`, since it is filtered by RANGE (a latency
threshold) far more than by equality, and only `tdigest` lets the planner
estimate a range predicate's selectivity rather than just its `[min, max]`
bounds. AggregationTemporality is scoped to the SAME sum/histogram pair the
`idx_agg_temporality` skip index above already targets — gauge never carries
the column, and exp_histogram carries it but sits outside every
temporality-aware routing path (see `renderAddTemporalityIndex`'s doc
comment), so statistics there would only tax writes for a column no read
path filters.

## Why the TraceId lookup projection covers logs as well as traces

**Both tables carry it, not traces alone.** The issue's own motivation names
logs<->traces correlation as well as trace-by-id, and
`trace_id_index_probe_chdb_test.go`'s own bar already requires both sides
index-served for `Consistent() == true` — `otel_logs` has no more `TraceId`
locality than `otel_traces` does, so scoping the projection to traces alone
would leave the logs side of every correlation hop on the bloom filter.

## Why materialized attribute columns are `DEFAULT`, not `MATERIALIZED`

**`DEFAULT`, not `MATERIALIZED` — this is the load-bearing design choice.**
A `MATERIALIZED` column is computed once at insert time and frozen; a
`DEFAULT` column's value for a row in a part that PREDATES the ALTER is
instead computed LAZILY, at read time, from that row's own already-stored
`SpanAttributes`. Verified directly against a real ClickHouse 26.6 server
(cerberus issue #2776):

- A fresh `ADD COLUMN ... DEFAULT` reads byte-identical to the map on
  every pre-existing row IMMEDIATELY, with ZERO mutation queued in
  `system.mutations` — `ADD COLUMN` is metadata-only.
- Concurrent `INSERT`s against the table while a later `MATERIALIZE
  COLUMN` mutation is in flight complete without error (reproduced on a
  150M-row table with the mutation genuinely overlapping 11 concurrent
  inserts).
- Across 150,000,012 rows spanning before/during/after a `MATERIALIZE
  COLUMN` backfill, ZERO divergence between the map value and the
  materialized column's value was observed.
- Reading through the materialized column + a `set(0)` skip index costs
  ~143 MiB of `read_bytes` versus ~858 MiB reading the map directly on the
  same query and table (~6x less I/O) — the "MB-not-GB" win the proposing
  issue cites (ClickHouse's own
  [map-performance doc](https://clickhouse.com/docs/knowledgebase/improve-map-performance)
  measures 2.9x cold / 11x warm for the general pattern).

## Why `http.status_code` uses `toInt32OrNull`

`toInt32OrNull`, not the bare `toInt32` cast, for the same DEFAULT-safety
reason `internal/traceql/lower.go`'s `toFloat64OrNull` coercion exists on
the map-read path: `SpanAttributes['http.status_code']` returns `''` for
a span that never carries the key and arbitrary text for a malformed
value, and a bare cast would abort the row's DEFAULT evaluation
("Cannot parse string") the instant one such row exists. `toInt32OrNull`
resolves both cases to `NULL` instead — verified against a real
ClickHouse server (`internal/schema/ddl/trace_materialized_attrs_integration_test.go`'s
`TestApply_NumericMaterializedAttrColumn_LazyDefaultAndGracefulNull`) and
against chDB
(`internal/api/tempo/search_tag_values_numeric_materialized_chdb_test.go`).

## Why `/detected_fields` is out of scope for the Loki catalog

**`/detected_fields` is intentionally out of scope for this feature.**
Unlike `/detected_labels`' label-set shape, `/detected_fields` derives
fields by re-running the query path's own `| logfmt` / `| json` parser-stage
extractions over a row peek — replicating that inside a materialized view
would mean embedding the parser cascade in SQL and maintaining a second
declaration of it, a substantially larger and riskier change than the
label-key catalog above. See cerberus issue
[#2844](https://github.com/tsouza/cerberus/issues/2844) for tracking a
dedicated design pass on that, independent of this feature.

## The Loki label-cardinality catalog's measured cost

**Measured before/after cost** (2M synthetic `otel_logs` rows spread across
a 24h window, `service.name`/`k8s.pod.name`/`deployment.environment.name`/
`k8s.namespace.name`/region attributes, ClickHouse 25.9 in Docker): the
existing per-request path over the full 24h window reads all 2,000,000 rows
(171 MiB) in 325ms; the SAME window's catalog read reads 5 rows (555 B, one
row per label key) in 2–3ms — roughly 400,000x fewer rows and ~130x less
wall-clock time. Even against a cheaper 1-hour window (1.6M rows, 50ms), the
catalog read is still ~17x faster.

## The Tempo tag catalog: verification, scope decisions, and measured cost

**Verified, not assumed:**
`internal/api/tempo/search_tags_filter.go`'s `tagQueryFilter` only resolves a
non-nil filter when `q` is present AND lowers to a real span-row predicate,
so a filtered tag-values lookup provably never reaches the catalog — see
`TestSearchTagValues_WithQFilter_StaysOnLivePath`.

**Event/link scopes (cerberus issue #2850).** Issue #2771 originally scoped
these out: `Events.Attributes`/`Links.Attributes` are
`Array(Map(String, String))` — one map PER EVENT/PER LINK on a span row,
not one map per row — so cataloging them costs an extra
`arrayFlatten(arrayMap(...))` fan-out on top of the explosion the
resource/span arms already pay twice, and issue #2771 was not written to
assume that stayed "cheap". Issue #2850 measured it instead of guessing
(see below) and found the extra cost small enough to include: the honest
scope-down from issue #2771 is superseded here, not kept out of inertia.
Instrumentation scope (`ScopeAttributes`) remains excluded — the upstream
schema carries no such column by default, so a stock deployment has
nothing to catalog there; a custom schema that populates it stays on the
live path for that bucket, and `?scope=none` steps off the catalog fast
path entirely on such a schema (see above) rather than silently omit it.
Service-name keying (a third `(Scope, ServiceName, TagKey)` catalog
dimension) was considered and not pursued: neither `/search/tags` nor
`/search/tag/{name}/values` accepts a service-scoped narrowing parameter
in any request shape this codebase or upstream Tempo's own API defines
today, so a service-keyed catalog would pay service-cardinality× more
rows for zero present read-side consumer — a decision the request shape
itself, not this repository's schedule, would prompt reopening. A
separate, narrower bug this investigation found — `resolveTagName`
silently routing an explicit `instrumentation.x` tag-values lookup to the
auto-scope (resource/span) union instead of the configured
`ScopeAttributesColumn`, on schemas that configure one — is tracked as
cerberus issue #3010; it does not block this feature (the catalog never
served that bucket either way).

**Measured before/after cost** (2,000,000 synthetic `otel_traces` rows
spread across a trailing 1h window, 5 resource-attribute keys + 10
span-attribute keys including two deliberately higher-cardinality tails
[`http.route`, `db.statement`], ClickHouse 25.9 in Docker via
testcontainers): the existing live scan
(`SELECT DISTINCT arrayJoin(mapKeys(ResourceAttributes))` over the same
window) reads 1,991,808 rows (294,022,617 bytes) in 302ms; the catalog read
(`SELECT TagKey FROM tempo_tag_catalog WHERE Scope = 'resource' GROUP BY
TagKey`) reads 15 rows (515 bytes) in 4.6ms — roughly 132,800x fewer rows
and ~65.6x less wall-clock time, verified via `system.query_log`
(`read_rows`/`read_bytes`), not client-side row counting. See
`internal/schema/ddl.TestTempoTagCatalog_MeasuredCost`.

**Event/link refresh cost** (same corpus and methodology, extended with
Events/Links populated on the SAME 2,000,000 rows — 10% of spans carry >=1
exception-shaped event, 3% carry a messaging link; see
`TestTempoTagCatalog_EventsLinks_MeasuredCost` for the exact assumptions):
adding the event+link arms to the refreshable view's own body cost only
~1.08x the existing resource+span refresh's wall-clock time (923ms vs
857ms), despite reading ~2x the rows — both Nested columns are empty for
the large majority of rows, so the extra arms decode far fewer bytes (66MB +
36MB) than the flat-Map arms do (799MB) even though ClickHouse's
`read_rows` accounting counts a full base-table scan for each arm. A
stress-test rerun with an unrealistically dense corpus (every span carries
1-3 events AND 1-2 links) still stayed at 2.01x baseline (1.78s) — a small
fraction of the 5-minute refresh period either way.

## Which compression codecs survived measurement

**Only two of the issue's proposed candidates survived measurement.** Every
candidate was benchmarked — not just reasoned about — against real
production-shaped sample data (`test/perf/nightly/testdata/samples/`,
issue #2411) or, where no real sample exists (span Duration — traces are
outside that sample set's scope), representative synthetic data, via a real
MergeTree engine (chDB), comparing whole-table compressed bytes before/after
the codec swap:

- **Adopted — Logs Body:** `ZSTD(3)` measured ~1.3%-1.8% smaller than the
  upstream `ZSTD(1)` default on a representative leveled-log-line corpus,
  consistently across repeated runs. ZSTD's decode cost is level-independent,
  so this costs write-side CPU only — no read-side query latency impact.
- **Adopted — Span Duration:** `GCD, ZSTD(1)` (no Delta stage) measured a
  real ~3.5% win when real precision is coarser than the column's declared
  nanosecond resolution (the issue's own stated GCD rationale), and was a
  statistical no-op (+0.02%, measurement noise) when precision is genuinely
  fine-grained — GCD finds no common divisor when there isn't one, so it
  costs nothing on a deployment whose real Duration values don't carry the
  coarse-precision pattern this codec targets. Chaining a `Delta` stage
  ahead of ZSTD — either `Delta, ZSTD(1)` alone or the issue's own proposed
  `GCD, Delta, ZSTD(1)` pairing — measured 3%-10% LARGER instead: Duration
  values are independent per-span measurements, not a running sequence, so
  delta-encoding them adds entropy rather than removing it.
- **NOT adopted — metrics TimeUnix / span Timestamp (`DateTime64(9)`):** the
  issue proposed `DoubleDelta, ZSTD(1)`, reasoning that a near-constant
  scrape interval leaves second-order regularity DoubleDelta captures and
  Delta alone misses. Measured against three real metrics tables
  (gauge/sum/histogram) it was 43%-166% LARGER than the current
  `Delta, ZSTD(1)` — a regression: a near-constant Delta stream is already
  maximally redundant (the same interval repeated for most of a series'
  run), which `ZSTD(1)` alone already exploits about as well as physically
  possible (232x-443x compression measured); DoubleDelta's own per-value
  framing bytes break up that redundancy more than they remove. `GCD, Delta,
  ZSTD(1)` measured a small, INCONSISTENT effect across the three tables
  (-3.5% to -6.9% on sum/histogram, +9.2% on gauge) — not a safe blanket win
  across every metrics table sharing one codec declaration. Bare `GCD,
  ZSTD(1)` measured catastrophically worse (+650% to +3515%). TimeUnix and
  Timestamp keep their current `Delta, ZSTD(1)`, unchanged by this issue.
- **NOT adopted — Value (gauge/sum) / Sum (histogram/summary/exp_histogram)
  Float64:** the issue gated Gorilla/FPC adoption on beating `ZSTD(1)` on
  real gauge-shaped data. Measured against the same three tables, BOTH
  regressed — Gorilla 27%-2182% larger, FPC 84%-2662% larger — so neither is
  adopted; Value / Sum keep `ZSTD(1)`, unchanged.

See the codec-tuning PR (cerberus issue #2768) for the full benchmark
transcript and measured numbers.

**NOT adopted — Value / Sum Float64, ALP codec (cerberus issue #2822):**
this third Value/Sum candidate was originally deferred rather than
benchmarked, on the premise that it needed a `>= 26.8` version-floor bump
to resolve a Float32 arithmetic decode-compat risk in ALP's 26.8 release.
That premise was corrected when #2822 closed: no floor bump was needed
(chopt already ships per-feature floors — e.g. `full_text_index` at 26.2 —
well above the 24.8 global `min_clickhouse`), and the cited Float32
decode-compat risk never applied, since every targeted column (Value, Sum)
is Float64, not Float32. Benchmarked directly against the same real
production-shaped samples on live ClickHouse 26.6/26.7/26.8 servers, ALP
measured dramatically worse than even Gorilla/FPC above — the histogram
Sum column compressed to ~99.9% of its uncompressed size (essentially no
compression) versus ZSTD(1)'s ~11x, and the counter Value column to ~19%
of uncompressed versus ZSTD(1)'s ~2.7%, consistently across all three
versions. ALP is not adopted; Value / Sum keep `ZSTD(1)`, unchanged.

## The two column-TTL design questions, resolved against a real server

**Two design questions the proposing issue explicitly left open, both
resolved against a real ClickHouse 25.9 server before implementation:**

- **TTL on an indexed column.** `Body` carries `idx_lower_body`
  (`tokenbf_v1` over `lower(Body)`, and optionally `idx_body_text` — see
  the text-index section below). A `MODIFY COLUMN ... TTL` ALTER on `Body`
  is accepted with the index left completely unchanged in `SHOW CREATE
  TABLE`, and a query filtering through the index after the TTL has fired
  returns the correct (now-empty) result — no drop/recreate of the index
  is needed or performed.
- **TTL on a Nested subcolumn.** ClickHouse's own TTL docs carry only a
  scalar-column example; `Events.Attributes` / `Links.Attributes` are
  Nested members, materialized as ordinary `Array(Map(...))` columns.
  Verified directly: the ALTER is accepted, and after materialization the
  expired row's array is cleared to `[]` independently of a fresh row
  sharing the same part — the other Nested subcolumns (`Events.Timestamp`,
  `Events.Name`, `Links.TraceId`, `Links.SpanId`, `Links.TraceState`) carry
  no TTL and are left at the row's own full retention, since they are small
  bounded-width identifiers a real deployment gets negligible storage
  benefit from expiring early.

## Why the text index takes a second name instead of an in-place type swap

A second name, not an in-place type swap of `idx_lower_body`: ClickHouse
matches `ADD INDEX IF NOT EXISTS` on NAME, not type, so re-running it against
a table that already carries `idx_lower_body` as `tokenbf_v1` is a silent
no-op — it could never install the text index on an upgraded deployment.
Swapping the type in place needs `DROP INDEX` + `ADD INDEX`, which is
destructive (existing `MATERIALIZE`'d granules are discarded, forcing a full
re-backfill) and, since this render-time DDL layer has no live
`system.data_skipping_indexes` read, cannot tell whether `idx_lower_body` is
ALREADY the text type — repeating that drop+add on every boot would be
pure, repeated, backfill-losing churn. Installing a second, non-colliding
name is the only additive, idempotent, crash-safe option available here, the
same reasoning `ADD PROJECTION` / `ADD STATISTICS` / `ADD INDEX` above all
already follow. `GRANULARITY 100000000` reproduces ClickHouse's OWN implicit
default for a `text` index type when no `GRANULARITY` clause is given
(confirmed live via `EXPLAIN indexes=1`, and re-confirmed byte-identical when
stamped explicitly) — `chsql.AlterTableAddIndex` has no omit-GRANULARITY
mode, so this reproduces the default rather than widening that builder's
contract for one index type.

## The measurement that retires `idx_lower_body`

`idx_lower_body`'s pre-#2773 `tokenbf_v1(32768, 3, 0)` shape indexes
`lower(Body)` — the exact column expression the `text_index_line_filter`
prefilter above conjuncts against with `LIKE '%tok%'`. That similarity is
worth stating plainly because it is the one honest reason this repo did not
simply take the "`tokenbf_v1` provides zero benefit" claim on faith: a
bloom-filter index over the SAME expression a new predicate shape targets is
exactly the situation where "no predicate cerberus emits matches this index
type" claims deserve a live check rather than a re-statement.

Live-measured against a real ClickHouse 26.6 server (not chDB — index
pruning is genuine server-planner behavior chDB does not reliably model): a
2,002,000-row logs-shaped table (`idx_lower_body` tokenbf_v1(32768, 3, 0)
GRANULARITY 8, `idx_body_text` text(tokenizer = 'splitByNonAlpha')
GRANULARITY 100000000, 2,000 rows containing the word "peer" clustered into
one narrow time window, matching a realistic incident-log shape) against
`lower(Body) LIKE '%peer%'` — the exact conjunct shape
`text_index_line_filter`'s prefilter emits:

| Indexes present                      | `EXPLAIN indexes=1` Parts/Granules pruned  | rows read | query time |
| ------------------------------------ | ------------------------------------------ | --------- | ---------- |
| neither                              | 24/24, 255/255 (no pruning)                | 2,002,000 | 191 ms     |
| `idx_lower_body` alone               | 24/24, 255/255 (no pruning)                | 2,002,000 | 254 ms     |
| `idx_body_text` alone                | 1/24, 1/255                                | 8,192     | 142 ms     |
| both (the post-#2773 upgraded shape) | 1/24, 1/255 (identical to text-only)       | 8,192     | 182 ms     |

`idx_lower_body` alone is byte-for-byte identical to no index at all for
this predicate — confirmed not to be a broken or misconfigured index by the
same probe: `hasToken(lower(Body), 'peer')` and `lower(Body) = 'peer'`
against the SAME `idx_lower_body`-only table both DO prune (5/24 parts,
35/255 granules) — `tokenbf_v1` works exactly as documented for
token-equality-shaped predicates, it is specifically the `LIKE '%needle%'`
substring shape cerberus's line-filter prefilter emits that it cannot
answer. `idx_body_text` alone already accounts for the FULL pruning benefit
in the "both" row; `idx_lower_body` contributes nothing incremental once
`idx_body_text` exists. This confirms both #2839's own "zero query-time
benefit" claim and this document's "harmless (if pointless) no-op" sentence
above hold up under a real-server check, not just as a re-stated assumption
— `idx_lower_body` is confirmed dead weight on an upgraded deployment: real
write-path bloom-filter-maintenance cost on every insert/merge, for zero
read-path benefit on the one predicate shape it was built to accelerate.

## Refuted ClickHouse 26.6-26.8 text-index claims

**Re-verified ClickHouse 26.6-26.8 claims (cerberus issue #2838) — both
REFUTED / no realized win, confirmed across three live server versions.**
`multiSearchAny` inside the skip-index analyzer and a dedicated posting-list
segment cache were both raised as possible 26.6 extras and originally only
probed on one 26.6.3.62 build. Re-verified against real ClickHouse
26.6.4.55, 26.7.6.57, and 26.8.2.7 servers (Docker `clickhouse/clickhouse-
server`), each seeded with 20M realistic log rows and an `idx_lower_body`
text index:

- **`multiSearchAny` stays out of the skip index on every probed build,
  including 26.8.** `EXPLAIN indexes=1` on `multiSearchAny(lower(Body),
  [...])` produced no `Skip` entry and a full granule scan (`Granules:
  612/612` and `2442/2442` across two differently-sized test tables) on all
  three versions, while `hasAnyTokens` on the identical predicate correctly
  pruned to the matching granules only (and even resolved to a trivial
  index-only count on 26.8). This is the real production consumer LogQL's
  own or-filter chains would hit: chained `|=`/`or` alternates
  (`internal/logql/lower.go`'s `lowerLineFilterChain`) lower to an `OpOr`
  tree of per-alternate `position(Body, ?) > 0` predicates — the exact shape
  a working `multiSearchAny` skip-index collapse would target — and it does
  not fire at any tested version. The original refutation was not a
  26.6.3-specific gap; it holds through 26.8.
- **`use_text_index_postings_cache=1` measured no win** on a chained
  multi-stage AND predicate matching cerberus's own emitted
  `text_index_line_filter` shape (`lower(Body) LIKE '%tok%' AND
  position(Body, ?) > 0`, ANDed across 2-3 stages) run repeatedly against
  the identical predicate — the cache's best case. The difference between
  cache-on and cache-off was small and within run-to-run noise across
  repeated `clickhouse-benchmark` trials on ClickHouse 26.8 (e.g. ~26-27 QPS
  on a 3-stage chain, ~12.4-12.7 QPS on a 2-stage chain, in both directions
  across trials) — no realized throughput or latency win large enough to
  justify stamping it either way.

Neither is relied on by `text_index_line_filter`, and neither warrants a new
`chopt` feature: #2838 closed both findings negative with this evidence.

**`text_index_posting_list_apply_mode=lazy` (cerberus issue #2837) —
measured no win, closed negative.** `lazy` is default-off (`materialize`) on
26.6/26.7 and becomes the server default at 26.8. Benchmarked explicitly
stamping `text_index_posting_list_apply_mode=lazy` (plus
`allow_experimental_text_index_lazy_apply=1` where required) against the
unchanged `materialize` default, on the same 3-stage chained-AND workload,
across repeated trials on both 26.6 and 26.7: `lazy` measured slightly
WORSE than `materialize` on both (26.6: ~22.4 vs ~23.1 QPS averaged across 3
trials; 26.7: ~25.4 vs ~25.8 QPS), never a consistent win. On 26.8, where
`lazy` is already the default, explicitly stamping it is a no-op by
definition — "no chopt feature ships whose stamp merely duplicates what the
server already defaults to." `text_index_density_threshold` (the lever
`lazy` mode uses internally to pick leapfrog-intersection vs brute-force
bitmap) was also swept from `0.01` to `0.9` on 26.8 against the same
workload and moved throughput by less than the run-to-run noise floor
already established by the trials above (~2-4%) — no version default-flip
backs it either, so there is no floor at which to gate a feature even if a
real effect existed. Neither setting warrants a `chopt` feature.

## The measured PK-pruning cost of the DELTA-prefix read path

**PK-pruning cost, re-measured against real production-shaped data.** An
earlier estimate (design discussion for this feature, not previously
committed to this document) against a *synthetic* 200-series/90-day probe
(18,000 rows) found a `MetricName`-only read scanning 71% of the table,
dropping to 34% once `BucketStart` moved to position 2 in `ORDER BY` — the
ordering this table already uses above — and flagged re-measuring against a
real high-cardinality metric as still open before this document claimed a
specific number. That re-measurement:
`test/perf/smoke/testdata/samples/svc_http_requests_total.parquet` — a real,
scrubbed 14-day Sum-metric capture, 18,591,129 raw samples across 31,073
distinct series — loaded into a chDB session and aggregated into this
table's exact shape (`GROUP BY MetricName, Attributes, ResourceAttributes,
ServiceName, toStartOfDay(TimeUnix)`), yielding 53,252 aggregate rows (most
series are active on only 1–2 of the 14 days — this sample's two-window daily
capture pattern, not a data-loading artefact). Against that table, `EXPLAIN
ESTIMATE` for a single-metric, half-window read (`WHERE MetricName = ... AND
BucketStart < <day 8 of 14>`) reads **26,624 of the table's 53,252 rows
(50.0%)** — essentially every series' rows for the included days — to answer
a query whose TRUE per-series contribution is typically 1–2 rows. This
confirms the synthetic finding at real production scale and sharpens it: even
with a real metric's genuine (non-synthetic) cardinality and day-distribution,
**no PK-level pruning exists below `MetricName`** — the series-identity
predicate is a `GROUP BY` key computed from the scan output, never a
`WHERE`-testable column, so cost scales with `date-range × metric-cardinality`
regardless of dataset size. This is still enormously cheaper than the removed
unbounded-retention scan (bounded by day-count regardless of series
cardinality, vs. literally the whole base table's history), but is a real,
named cost for a single-series `rate()`/`increase()` query against a
high-cardinality DELTA metric — not `date-range × 1` — and belongs in
capacity planning for any deployment enabling
`CERBERUS_DELTA_PREFIX_READ_ENABLED` against such a metric.

## Why the two-tier fence is a fixed split, not a per-diff prediction

An earlier, more elaborate
design attempted to route each PR through a *predicted* lane subset based on
its diff's blast radius; a backtest against real PRs found the predictor fell
back to the full lane set on the large majority of them anyway, so it bought
none of its intended savings while adding real selection-logic risk. The
two-tier split replaces that attempt.

## Why the Homebrew tap ships a cask, and why it installs under Linuxbrew

This is wired via the goreleaser `homebrew_casks:` block. A *cask* is the right
vehicle for a pre-built binary — a formula describes something Homebrew builds
from source — and it is not a macOS-only choice, though not for the reason casks
are usually assumed to be Mac-bound. The entire Linux gate is Homebrew's
`check_stanza_os_requirements`, which proceeds only when a cask declares no
top-level `depends_on macos:` **and** every one of its artifacts is supported off
a Mac, and otherwise raises `<cask>: cask requires macOS.`. Both conjuncts
matter: `depends_on macos:` *is* consulted on Linux, contrary to folklore — what
makes it harmless here is that `requires_macos?` is set only by a *top-level*
`depends_on macos:`, so neither the implicit `MacOSRequirement` Homebrew attaches
to every cask nor goreleaser's output (which declares no `depends_on` at all)
trips it. The unsupported artifacts are the sixteen classes in
`MACOS_ONLY_ARTIFACTS` (`app`, `pkg`, `service`, …) plus an `installer` in its
`manual:` form — a scripted `installer` is portable. `supports_linux?` is not the
install-time predicate at all; its only caller is homebrew-cask's own CI matrix
generator, and it reports `false` for this cask even though `brew install` works.
What makes cerberus installable under Linuxbrew is that
its sole artifact is a plain `binary`, and that goreleaser emits `on_linux`
url/sha256 pairs for `linux_amd64` and `linux_arm64` from the same `builds:`
matrix that feeds the darwin ones. Because the release binaries are
neither Apple-signed nor notarised, the cask carries a post-install hook that
strips the `com.apple.quarantine` xattr, without which the first run on macOS
dies with "cerberus is damaged and can't be opened".
