# Router-rules catalog — background

This document collects the design rationale, rejected alternatives, incident
history, and academic grounding behind [`router-rules.md`](router-rules.md). It
answers "why is the catalog built this way" rather than "what does it do" —
nothing here is required to run the analysis, read a finding, or add a rule;
`router-rules.md` is self-sufficient for that.

## Why generic drivers + dynamic params (and not a tuned ruleset)

A router-rules catalog that shipped concrete thresholds — "OOM above 4 GiB",
"shard above 8× fan-out" — would encode **one** deployment's cost surface and
quietly mis-fire on every other. The catalog instead ships only rule
*structure* and *named parameter references*; every threshold, watermark, cap,
and percentile cutoff is a named parameter resolved per-deployment at runtime:

- **`config`** — a number the operator sets in their own deployment config
  (e.g. `router_rules.watermark_percentile`, or reuses an existing knob like
  `query.max_memory_bytes`). This is the only place an operator number enters.
- **`config_scaled`** — the product of two config params (`ref` × `scale_by`),
  e.g. `memory_near_cap = memory_near_cap_fraction × query.max_memory_bytes`.
  Lets a rule gate on a tunable fraction of the deployment's **actual**
  configured cap (rather than the corpus p95, which ~5% of any healthy
  population trivially exceeds), still without a number in the catalog.
- **`corpus_percentile` / `corpus_agg`** — learned from the deployment's own
  corpus (a per-shape `read_rows` tail, a per-language `cumulative_d`
  watermark). Self-relative, so a shape is judged against its own history.
- **`corpus_count_ratio`** — a deployment-wide scalar (`countIf(num scope) /
  countIf(den scope)`) surfaced as message context, never a gate.

The result: a small audit surface (the base `catalog/catalog.yaml` plus one
`catalog/rules/<rule_id>.yaml` per rule) with a
no-numbers invariant enforced three independent ways (a condition AST with no
number-literal node, a load-time validator, and the `TestEmbeddedCatalogHasNoNumbers`
guard). The same rule fires correctly whether a deployment's pain is broad
PromQL aggregations, high-cardinality TraceQL `compare()`, or LogQL line scans —
because the *number* that decides "pathological" comes from that deployment's
own data, not the catalog.

## Why the naive wrong-rejection rule was dropped

The original analysis dropped a naive *wrong-rejection* rule (flagging
`exit_status=rejected` against a parity oracle) because judging it needs a
rejection-parity oracle the corpus alone lacks. `cerberus_side_rejection_pressure`
ships the buildable form instead: it fires on the cerberus-side rejection
*cluster* (gated by `min_support`) and surfaces the deployment-wide rejection
share as message context (`{cerberus_reject_ratio}`, a `corpus_count_ratio`
scalar) — context, never a gate, so no inline tolerance number is needed.

## Why `anchor-grid-indivisible` is excluded from the reason gate

`anchor-grid-indivisible` is the case that shows why the membership is stated as
a list and not as "declined on cost". It IS a cost verdict, but the plan cleared
every threshold before the anchor-grid gate turned it away, so lowering a
threshold cannot change its outcome either — and slicing it would cost more, not
less. Reading it as a threshold's doing would advise exactly the wrong lever.

## Why the group key is rendered as a decimal, not widened to a float

Widening the column to a `float64` first — the obvious way to share one
numeric path across every column — rounds every value at or above 2^53 and
collapses distinct hot shapes into one class, so the backends disagree about what
a class even is while every individual rule still lowers from the same AST. A
fixture cannot detect that below 2^53, where the two renderings agree; the
benchmark's one hash-grouped class therefore sits at
`17000000000000000001`/`...002`, adjacent values above 2^63, and the parity lane
compares exact keys over the range a real hash occupies.

## Why parity-on-chDB is not enough: the strict-scan integration lane

The `chdb`-tagged parity test executes the CH path's SQL, but against chDB
(libchdb behind chdb-go's Parquet `database/sql` driver), which **leniently
coerces** result-column types into whatever Go destination a `Scan` supplies — a
`UInt64` `count()` or an integer-typed `quantileExact` lands happily in a
`*float64`. Production cerberus does not use chDB: the offline analysis talks to
a real ClickHouse over the native protocol via `clickhouse-go/v2`, whose `Scan`
is **strict** — a column type that doesn't match the destination is a hard error
(`converting … to … is unsupported`, code 47) the operator sees as a 502. This
is exactly the class of bug `#1064` was: the corpus SELECTs returned integer CH
types but the cursor scanned them into `*float64`/`*int64`; every chDB parity
test stayed green while the read path 502'd against real ClickHouse. The fix
wraps every integer-returning aggregate in `toFloat64(…)`/`toInt64(…)`
([`source_ch.go`](../internal/routerrules/source_ch.go)) so the wire type
matches the scan destination.

Because the chDB parity lane is structurally blind to that class, and because
compose-smoke / e2e drive the data plane (never the offline corpus reconciler),
the real-CH integration lanes [`router-rules.md`](router-rules.md) enumerates
exist.

## Academic references

The detector design draws on the database-systems literature on cost-based
optimization, cardinality-estimation error, memory-aware admission control, and
self-driving / continuously-tuned systems:

1. P. G. Selinger, M. M. Astrahan, D. D. Chamberlin, R. A. Lorie, T. G. Price.
   *Access Path Selection in a Relational Database Management System.* ACM
   SIGMOD 1979, pp. 23–34. — cost-based optimization and the divergence between
   predicted and observed cost from stale catalog statistics. (Grounds the
   whole corpus-vs-decision premise: a router decision made on estimated cost is
   audited against realized cost.)
2. V. Leis, A. Gubichev, A. Mirchev, P. Boncz, A. Kemper, T. Neumann. *How Good
   Are Query Optimizers, Really?* PVLDB 9(3), 2015, pp. 204–215. — cardinality
   estimation is the dominant source of optimizer cost error. (Grounds
   `read_rows`/`cumulative_d` as the realized-cardinality signals to mine.)
3. Y. Wu, et al. *Robust Query-Driven Cardinality Estimation under Changing
   Workloads.* PVLDB 16, 2023. — estimator drift under shifting workloads; act
   on outcome-changing errors, recomputed per window. (Grounds the
   `corpus_percentile` watermarks recomputed over the `--since` window.)
4. *LearnedWMP: Workload Memory Prediction Using Distribution of Query
   Templates.* arXiv:2401.12103, 2024; with Microsoft SQL Server memory-grant /
   `RESOURCE_SEMAPHORE` admission-control documentation. — OOM/spill as a
   dominant failure mode cured by admission control + rewrite/cap rather than
   more parallelism. (Grounds `failure_cluster_by_reason`, `route_b_still_failing`,
   `cerberus_side_rejection_pressure`.)
5. A. Pavlo, et al. *Self-Driving Database Management Systems.* CIDR 2017; L. Ma,
   D. Van Aken, et al. *Query-based Workload Forecasting for Self-Driving DBMS.*
   SIGMOD 2018 (OtterTune). — continuous, bidirectional tuning against the live
   workload. (Grounds the per-deployment parameter-resolution model and the
   route-B-regret rule.)
6. *(Boundary, cited for the out-of-scope rationale.)* R. Avnur, J. M.
   Hellerstein. *Eddies: Continuously Adaptive Query Processing.* ACM SIGMOD
   2000. — mid-query, per-tuple reoptimization yields no post-hoc corpus signal;
   the router is a coarse-grained admission decision, so eddy-style adaptivity
   is explicitly out of scope for this catalog.
