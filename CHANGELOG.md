# Changelog

All notable changes to cerberus will be documented in this file. The format roughly follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), with one entry per tagged release.

## [v1.0.0-RC1]

The first release candidate on the `v1.0.0` line. This is an early,
experimental cut: the three heads parse → lower → execute and the
differential harnesses gate every merge, but the surface is still
evolving and must be validated against your own corpus before any real
use.

### Added

- TraceQL nested-set intrinsics in `| select(...)` — the projection Grafana Traces Drilldown's "Structure" tab sends (`… | select(status, resource.service.name, name, nestedSetParent, nestedSetLeft, nestedSetRight)`) now works end-to-end instead of 422ing. A new `chplan.NestedSetAnnotate` node recomputes reference Tempo's ingest-time nested-set numbering at query time (recursive CTE over the `(TraceId, SpanId, ParentSpanId)` adjacency; DFS bounds per `assignNestedSetModelBoundsAndServiceStats` semantics: counter from 1, root parent `-1`, disconnected spans `0/0/0`, counter continues across multiple roots; sibling order is `(Timestamp, sipHash64(SpanId))` since OTel-CH does not record Tempo's ingest order). `/api/search` responses now surface user-selected attribute values inside `spanSets[].spans[].attributes` (OTLP `intValue` for nested-set intrinsics, `stringValue` otherwise, lowercased `status` / `kind` enum casing per Tempo's wire encoding) and populate the per-span `name` field when `select(name)` requests it — exactly reference Tempo's placement. Two more latent bugs on the same Drilldown path are fixed: mixed structural/plain `||` arms are now column-aligned (ClickHouse rejected the positional `UNION DISTINCT` with code 258), and structural-join wrap subqueries expose the columns `select()` can read (`StatusCode`, `SpanAttributes`, …) so `{A} >> {B} | select(status)` resolves. The PLAIN-FILTER arm gets the same plumbing: the optimizer's `ProjectionPushdown` expression walker now descends into every child-bearing `chplan.Expr` kind (`FieldAccess` sources, `Subscript`, `Lambda` bodies, the map-carrier nodes, `NestedArrayExists` values — plus the `NestedArrayExists.Column` string carrier), so `{ status = error } | select(span.http.method, resource.service.name)` no longer prunes `SpanAttributes` out of the narrowed scan (ClickHouse error 47 → HTTP 502 on the showcase "select / by / coalesce" panel).

- Sort-key-aware filter emission + `PREWHERE` promotion. The chsql emitter now fuses `Filter(Scan)` into a single `SELECT … FROM <table> [PREWHERE …] WHERE …` and partitions conjuncts into a sort-prefix bucket / skip-index bucket / rest, then promotes cheap predicates that touch no wide column into `PREWHERE` when the projection reads any wide column. ~219 existing TXTAR fixtures across `test/spec/{chsql,promql,logql,traceql,optimizer}` were re-emitted; the diff is a pure structural rewrite (one less subquery layer, predicates reordered by sort-key rank, optional `PREWHERE` split) and the rendered SQL is semantically equivalent. 29 of the re-emitted fixtures now carry a `PREWHERE` clause. New unit tests cover the predicate classifier (`prewhere_test.go`); new `test/spec/codegen/prewhere/` fixtures pin the four codegen-only behaviours (wide-column-excluded, partial-promotion, no-wide-no-promotion, sort-prefix-order).

#### Advanced QL

The advanced-QL surface: PromQL subqueries (P0 4.1–4.11), `predict_linear` / `holt_winters` / `@start()` / `@end()`, `histogram_quantile` over both classic and native (exp) histograms, `group_left` / `group_right` cardinality edges; LogQL `| unpack`, `| pattern`, `| line_format`, `| decolorize`, `| label_format` template stages with Loki template funcs, `bytes_*` alignment, `/api/v1/tail` WebSocket, `/labels`, `/label/.../values`, `/series`, `/detected_fields`, `/patterns`, `/index/stats`, `/index/volume`; TraceQL `status = error` / `kind = client` enum statics, `sum / avg / max / min` over inner attributes, link traversal + span-event queries, set ops, `group / coalesce` pipeline elements, `histogram_over_time`, MetricsPipeline lowering, Tempo `/api/search/recent`, `/api/search/tags`, `/api/search/tag/<n>/values`, `/api/metrics/query_range`. The Tempo `unsafe.Pointer` shim is retired via the [`tsouza/tempo:cerberus-accessors`](https://github.com/tsouza/tempo/tree/cerberus-accessors) fork; the OTel CH Exporter schema is the source of truth via the [`tsouza/opentelemetry-collector-contrib:cerberus-ddl`](https://github.com/tsouza/opentelemetry-collector-contrib/tree/cerberus-ddl) fork (no hand-maintained DDL).

#### PromQL

- Fold scalar-only PromQL in Go for Grafana's health probe (`1+1`-style queries). [#95]
- `chplan.RangeWindow.OuterRange` + `Identity` for subquery emit. [#98]
- Step-grid SQL emission for matrix-shape `RangeWindow`. [#101]
- Wire matrix `RangeWindow` through `chclient.Sample` shape (P0 4.4). [#104]
- Lower subquery over range-vector calls — `max_over_time(rate(m[5m])[1h:5m])` (P0 4.6). [#107]
- Pin optimizer "no mis-rewrite" on matrix `RangeWindow` (P0 4.9). [#109]
- Subquery roadmap + `Lower()` docs (P0 4.11). [#110]
- Subquery E2E + Playwright coverage (P0 4.10). [#111]
- `/api/v1/format_query` + `/api/v1/parse_query` handlers. [#114]
- `/api/v1/query_exemplars` handler. [#137]
- `group_left` / `group_right` cardinality + extra-label edges. [#144]
- `predict_linear`, `holt_winters`, `@start()` / `@end()` modifiers. [#159]
- `histogram_quantile` on classic histograms. [#170]
- `histogram_quantile` on native (exp) histograms. [#171]

#### LogQL

- Point stale "not yet supported" LogQL messages at the implemented stages. [#118]
- `| line_format` + `| decolorize` as Go-side post-process. [#124]
- Handle nil predicate from no-op stages; `line_format` / `decolorize` handler tests. [#127]
- TXTAR fixtures for `| line_format` and `| decolorize`. [#128]
- `| label_format` rename + template stages. [#130]
- Expose Loki template funcs in `| line_format` / `| label_format`. [#132]
- `/index/stats` + `/index/volume` handlers. [#141]
- `| unpack` + `| pattern` parser stages. [#142]
- `/labels`, `/label/values`, `/series`, `/detected_fields`, `/patterns` handlers. [#151]
- `bytes_*` alignment + `/api/v1/tail` WebSocket. [#157]

#### TraceQL

- Lower `status` / `kind` static literals (P0 6). [#96]
- Lower `sum` / `avg` / `max` / `min` over inner attribute (P0 7). [#99]
- `/api/search/recent` + `chplan.OrderBy` IR node. [#123]
- Cover `/api/search/recent` handler edges. [#126]
- `/api/search/tags` + `/api/search/tag/<name>/values`. [#150]
- `MetricsPipeline` lowering — `rate` / `*_over_time` → `Aggregate(Scan(traces))`. [#153]
- Set ops (`&&`, `||`, `~`) + `group` / `coalesce` pipeline elements. [#156]
- `MetricsAggregate` IR + `RangeWindow` over metric-shape input. [#160]
- `/api/metrics/query_range` handler. [#163]
- Link traversal + span-event queries. [#169]
- `histogram_over_time(attr)` lowering + emission. [#173]

#### Schema source-of-truth (plus the `tsouza/opentelemetry-collector-contrib` fork)

- Wire `tsouza/opentelemetry-collector-contrib:cerberus-ddl` via `replace`. [#154]
- Mirror upstream OTel CH Exporter columns (exp_histogram + missing classic histogram fields). [#158]
- Wrap upstream OTel CH Exporter DDL via the `schema/ddl` package. [#161]
- `CERBERUS_AUTO_CREATE_SCHEMA` env-gated startup hook. [#166]
- Refactor `harness/prometheus-compliance` to seed via `schema/ddl` package. [#167]
- Refactor `test/e2e` to seed via `schema/ddl` package + Go fixture inserts. [#168]
- Self-contained `otel-collector` + `sample-app` for real E2E data. [#172]

#### Tooling, CI, repo hygiene

- Daily Dependabot for upstream parsers + auto-merge. [#100]
- Bump Playwright deps. [#102]
- Align markdown tables for MD060 (markdownlint v0.40). [#105]
- Don't advance `:latest` on prereleases + SLSA attestation. [#112]
- Defer P0 3 (k3s otel-collector) + P0 5 (recursive `>>` / `<<`). [#113]
- Align indented table that PR #105 missed. [#117]
- Consolidate lint into one job + add `bodyclose`, `errorlint`. [#119]
- Switch `auto-merge-deps` to `workflow_run` trigger. [#120]
- Bump the github-actions group across 1 directory with 10 updates. [#121]
- SQL-builder evaluation (R6.0) recommends custom builder. [#125]
- Align CLAUDE.md + roadmap with R6.0 custom-builder choice. [#129]
- Scaffold public `chsql.Builder` + `QueryBuilder` (R6.1). [#131]
- Fuzz + perf-benchmark workflow scaffolds. [#133]
- Scaffold local Go PromQL evaluator. [#134]
- Pattern-based optimizer `Rule` API scaffold. [#135]
- Scaffold differential-testing harness. [#136]
- Port `emitScan` / `Filter` / `Project` / `Limit` to Builder (R6.2). [#138]
- Plan to fork upstream Tempo, retire `unsafe.Pointer` shims. [#139]
- Port `emitAggregate` + `emitAggFunc` to Builder (R6.3). [#140]
- Wire `tsouza/tempo:cerberus-accessors` fork via `replace` directive. [#143]
- Run dashboard E2E on merge-to-main only, drop PR trigger. [#145]
- Actually use `QueryBuilder` for R6.2 / R6.3 ports + repo audit. [#146]
- Retire `unsafe.Pointer` + reflect shims via `tsouza/tempo` accessors. [#148]
- Tighten no-raw-SQL rule to forbid `Builder.WriteSQL` clause keywords. [#149]
- `forbidigo` lint forbids `unsafe.Pointer` + `reflect.FieldByName` in `internal/traceql`, `internal/api/tempo`. [#152]
- Plan 6 scalability levers. [#155]
- Add lefthook pre-commit + commit-msg hooks (formatters only; CI owns validation). [#162]
- Drop empty `pkg/` — cerberus is a service, not a library. [#165]

#### Core slice

The seed (PR1–PR7 + admin + v0.1.0) plus M1–M4 (full PromQL / LogQL / TraceQL parsing → lowering → execution) + corpus expansion (TXTAR 122 → 166 fixtures, ~280 new unit-test sub-cases, E2E HTTP 12 → 26, Playwright 10 → 19 scenarios).

#### PromQL (M1.1 – M1.7)

- Real `RangeWindow` SQL emission via the promshim-clickhouse windowed-array idiom (`groupArray` + `arraySort` + `arrayFilter` + `arrayPopBack/Front` for counter-reset deltas). [#40]
- `BinaryExpr` lowering: scalar/vector arithmetic and pow / mod. [#41]
- Instant-vector functions: `abs`, `ceil`, `floor`, `round`, `sqrt`, `exp`, `ln`, `log2`, `log10`, `sgn`. [#42]
- Aggregation completeness: `without (...)` (new `chplan.MapWithoutKeys`), `stddev`, `stdvar`, `group`, parameterised `quantile(phi, ...)`. [#43]
- `offset` and `@` modifiers thread through `RangeWindow.Offset` / `End` plus a `Timestamp <= anchor` predicate on instant-vector queries. [#44]
- Vector matching: default + `on(...)` + `ignoring(...)` via the new `chplan.VectorJoin` (per-series argMax + INNER JOIN). [#45]
- Comparison ops + `bool` modifier (Filter shape vs `toFloat64(...)` Project). [#48]
- Clamp family and 2-arg `round(v, to_nearest)`. [#49]

#### Prom HTTP API (M2.1 – M2.7)

- Real per-step bucketing in `/api/v1/query_range` with 5-min lookback. [#50]
- Aggregate result shaping — Sample-shape Project on top of `chplan.Aggregate` so `sum by (job)` etc. flow through the existing chclient decoder. [#52]
- `/api/v1/labels`, `/api/v1/label/{name}/values`, `/api/v1/series` with UNION ALL across metric tables. [#51]
- `/api/v1/metadata` sourcing `MetricDescription` + `MetricUnit` from each table. [#53]
- `X-Prometheus-API-Version` + `X-Cerberus-CH-Millis` debug headers via a header-stamping middleware that times each CH call into a request-scoped counter; `match[]` selector support on `/labels` and `/label/.../values`. [#54]

#### LogQL (M3.1 – M3.5)

- `schema.Logs` + `chplan.LineContent`; stream selectors (`{job="api"}`) and the line-filter family (`|=` / `!=` / `|~` / `!~`) with chained-filter AND-folding and `or`-disjunction. [#55]
- Label filters (`| label="val"` / `| label=~"r"`); `BinaryLabelFilter` and `LineFilterLabelFilter` share the same `*labels.Matcher`-based lowering helper. [#58]
- Metric form: `rate({...}[5m])`, `count_over_time(...)`, `bytes_rate(...)`, `bytes_over_time(...)`. New `log_rate` emitter binds `range_seconds` via a `?` placeholder rather than Sprintf'ing it inline. [#61]
- Aggregations: `sum(rate(...))`, `avg by (job) (count_over_time(...))`, `sum without (pod) (...)`, with stddev / stdvar / group / quantile parity to PromQL. [#62]
- Loki HTTP `query` + `query_range` handlers; metric queries return Prom-style matrix/vector, log queries return Loki "streams" shape. [#63]

#### TraceQL (M4.1 – M4.5)

- `schema.Traces` + `chplan.FieldAccess` for dotted-path attribute references; SpansetFilter with intrinsic resolution (`duration`, `name`, `kind`, `status`, `statusMessage`, `parent`, scoped `trace:id` / `span:id`) and scope-prefixed paths (`resource.` → ResourceAttributes, `span.` → SpanAttributes). [#64]
- Direct structural ops `>` (parent of) and `<` (child of) via `chplan.StructuralJoin` rendering an INNER JOIN of two span subqueries on `(TraceId, ParentSpanId)`. [#65]
- `| count() > 0` aggregate + scalar-filter wrapping; reuses the M1.4 `chplan.Aggregate` shape. [#66]
- `| select(span.x, resource.y)` projection: reflects out `SelectOperation.attrs` (Tempo keeps it on an unexported field) and emits one column per requested attribute aliased to its TraceQL name. [#70]
- Tempo HTTP API: `/api/echo`, `/api/status/version`, `/api/search?q=<TraceQL>`, `/api/traces/{id}`. trace-by-id skips the parser and builds the chplan tree directly. Tempo's distinct error envelope (`{"traceID":"","spanID":"","error":true,"message":"..."}`) drives Grafana's "trace not found" UI. [#71]

#### Test corpus expansion

- TraceQL TXTAR fixtures grow from 8 to 26 — boolean `||`, regex / not-regex matchers, every intrinsic (`name`, `kind`, `statusMessage`, `parent`, scoped `trace:id` / `span:id`), span-attribute scoping variants, scalar-filter thresholds, and resource-scoped select projection. [#72]
- chsql TXTAR fixtures grow from 15 to 29 — direct tests for every chplan IR node (VectorJoin, StructuralJoin, MapWithoutKeys, LineContent variants, parameterised `quantile`, RangeWindow with `Offset` + LogQL `log_rate`, FieldAccess, FuncCall). [#74]
- Meaningful Grafana Playwright scenarios for all three datasources: LogQL streams + metric, TraceQL search + traceByID, richer PromQL (rate matrix + labels + metric names). Per-signal seed files (`otel_logs.sql`, `otel_traces.sql`). [#76]
- Cerberus-side HTTP integration tests for every shipped surface: Prom rate / labels / label-values, Loki streams + metric, Tempo echo / version / search / trace-by-id (found + not-found). [#77]

#### Engineering / CI

- Required-status checks: `check`, `lint`, `dashboard` (full-stack k3d + cerberus + Grafana + Playwright smoke). `enforce_admins: true`; `gh pr merge --admin` is forbidden. [#56, #59, #60]
- Compatibility harness drops the `pull_request` trigger initially; runs nightly + on `main` push as informational baseline. [#56]
- Hard rule established: no `fmt.Sprintf` (or string concatenation) for ClickHouse SQL going forward; existing emitter Sprintf is grandfathered until the typed-builder port replaces it. [#57]
- SQL-builder evaluation: a written security + impact + build-vs-buy analysis recommends third-party (`huandu/go-sqlbuilder` + cerberus extension layer), custom (`internal/chsql.Builder`), or defer. [#73]
- `internal/engine/` ExecutionEngine framework scoped with the same evaluation-first pattern: audit pipeline divergence across 5 callsites before any code lands; recommendation among (a) Build, (b) Partial — helpers-only extraction, (c) Defer. [#75]

### Changed

- `QueryBuilder.Limit(int64)` and Builder API refactors to thread typed clauses through the chplan emitter (R6.2 / R6.3 audit). [#138, #140, #146]
- Migrate test seeders (`harness/prometheus-compliance`, `test/e2e`) off hand-rolled `*.sql` files onto the upstream-derived `schema/ddl` package. [#167, #168]

### Security

- Bump `apache/thrift` v0.22.0 → v0.23.0 (CVE-2026-41602). [#164]

### Infrastructure

- `tsouza/tempo:cerberus-accessors` fork wired via `replace` to retire the `unsafe.Pointer` shim ([#143]); shim removed in [#148]; `forbidigo` gate added in [#152].
- `tsouza/opentelemetry-collector-contrib:cerberus-ddl` fork wired via `replace` so the OTel CH Exporter DDL is the source of truth (no hand-maintained schema). [#154]
- Lefthook pre-commit + commit-msg hooks (formatters only; CI owns validation). [#162]
- `auto-merge-deps` switched to `workflow_run` trigger; `dashboard` job moved to merge-to-main only. [#120, #145]
- Self-contained k3s deployment: per-node OTel Collector DaemonSet + gateway Deployment + sample-app `telemetrygen` for real E2E data. [#172]

### Documentation

- New **per-function / per-construct coverage matrix** ([`docs/coverage.md`](docs/coverage.md)), the user-facing answer to "does cerberus support the queries my dashboards run?". Every PromQL function / aggregation / operator / modifier, every LogQL stage / aggregation / filter, and every TraceQL intrinsic / metrics-op (228 symbols across the three heads) is listed with an honest support status — Supported, Supported (experimental), Supported (cerberus extension), or Rejected (parity with reference). The tables are generated from the `test/surface-parity/inventory.json` conformance ledger (`scripts/gen-coverage.py`), translating the ledger's machine-readable `parity-accept` / `parity-reject` / `wrong-accept` / `wrong-reject` classes into user-facing support language. Current coverage: 226 of 228 symbols supported, 2 intentional parity rejections (bare `start()` / `end()`), and **zero** wrong-rejections. Linked from the README documentation index.

- **`docs/test-strategy.md` layer map + CI-gate inventory reconciled with reality.** The map is now 12 layers (was understated at 11): added Layer 6d (the function-surface parity ledger — `test/surface-parity/` + `test/rejection-parity/` + `test/inventory/`) and Layer 12 (compute fan-out guards — the static fan-out lint, per-construct scaling harness, cardinality / scale-wall / solver-decision ratchets, and the corpus profiler). The CI-gate table now lists every job that runs — including the previously-omitted `compatibility/promql-surface`, `compatibility/prometheus-forced-route`, `perf-guards`, `perf-profile`, `startup-bench`, and `coverage` lanes — with each one's accurate required-vs-informational status (the eleven required checks are spelled out explicitly).

- **Operations: circuit-breaker blast radius documented.** [`docs/operations.md`](docs/operations.md) now spells out that the ClickHouse circuit breaker is a *single* per-`Client` breaker shared across all three API heads **and** the `/readyz` pinger, so one trip 503s every head and flips `/readyz` red (evicting the pod under Kubernetes) — an all-or-nothing, whole-replica coupling operators must tune against. The per-query wall-clock timeout (`CERBERUS_QUERY_TIMEOUT`) is cross-referenced as the separate bound for slow-but-healthy queries, closing the documented "no query deadline" gap.

- **Configuration file documented.** [`docs/configuration.md`](docs/configuration.md) gains a "Configuration file (optional)" section describing the viper loader's optional `cerberus.yaml` (probed in `.` then `/etc/cerberus`), the env > file > default precedence, and the missing-or-malformed-is-tolerated contract. The stale "there is no YAML file to load" claim in `configuration.md` and `operations.md` is corrected.

- **Native-rate parity prose reconciled** (`operations.md`). The dual-emit ULP-divergence statement now cites the test-enforced bound — at most two cells diverge, each by no more than 1 ULP (`maxDualEmitUlpDivergentCells = 2`) — and the observed pinned-fixture count (8 of 9 cells bit-identical), replacing the unverified "16/18" figure. `performance.md`'s `perf-guards` lane is corrected from "required" to its actual informational status.

### Known gaps captured at the core-slice point

Tracked at the time the core slice landed; the advanced-QL work above
delivers most of these. The remainder are honest gaps still to close:

- PromQL: `topk` / `bottomk` / `count_values` (output-shape changes).
- LogQL: `| json`, `| logfmt`, `| regexp` parser stages; `unwrap`-based ops.
- TraceQL: recursive structural ops `>>` / `<<` and sibling ops.

### Known limitations / experimental notes (RC1 posture)

RC1 is an early, experimental cut. The differential harnesses gate every
merge, but correctness, performance, and operational behaviour are still
being shaken out — **validate cerberus against your own corpus before
pointing anything real at it** (see the README warning). The
maintainer-accepted caveats specific to this candidate:

- **TraceQL conformance is the weakest of the three heads.** PromQL is
  scored against the third-party CNCF / PromLabs
  [PromQL Compliance Tester](https://github.com/prometheus/compliance) and
  LogQL against a real reference Loki seeded from Grafana's own
  `pkg/logql/bench` corpus, but **there is no third-party TraceQL
  conformance suite** to draw on. The TraceQL harness diffs against a real
  reference Tempo, yet its corpus is author-written cerberus-owned TXTAR, so
  its breadth is author-bounded rather than reference-derived. Raising
  TraceQL's confidence is the top post-RC1 improvement item. See
  [`docs/compatibility.md`](docs/compatibility.md#traceql--cerberus-owned-driver)
  for the per-head confidence table and the full reasoning.

- **Per-head circuit-breaker isolation is in place but not fully hardened
  ([#94]).** The single `chclient.Client` holds a registry of breakers —
  one per data-plane head (`prom` / `loki` / `tempo`) plus a dedicated
  `probe` breaker for `/readyz` — so one head tripping OPEN no longer 503s
  the others or evicts the pod (see
  [`docs/operations.md`](docs/operations.md#clickhouse-circuit-breaker) for
  the blast-radius contract). Full isolation and independent-recovery
  hardening is post-RC1 work: after a ClickHouse restart, recovery may
  briefly flap as the per-head HALF-OPEN probes re-converge. This is being
  improved separately and is a known experimental rough edge, not a
  blocker.

- **Not production-ready.** Cerberus remains experimental and under active
  development; the surface is evolving and breaking changes are expected.
  Do not stand it in for a running Prometheus / Loki / Tempo deployment
  without first evaluating it against your own queries and data.

## [v0.1.0] — Seed

First tagged release. Closes the seed series (PR1–PR7 + admin + roadmap):

- Module `github.com/tsouza/cerberus` on `go 1.26.2` with the `replace github.com/hashicorp/memberlist => github.com/grafana/memberlist@…` hygiene fix.
- Shared plan IR (`internal/chplan`), ClickHouse SQL emitter (`internal/chsql`), TXTAR spec runner under `test/spec/`.
- Rule-based optimizer (`internal/optimizer`) with three rules: filter fusion, constant folding, projection pushdown.
- PromQL vertical slice (`internal/promql/lower.go`) covering instant vector selectors, label matchers (eq / ne / regex), range vectors (placeholder SQL), and aggregations (`sum`, `count` with `by(…)`).
- HTTP API surface (`internal/api/prom`) for `/api/v1/query` + `/api/v1/query_range` (range_range returns a single point until full `RangeWindow` lowering lands in M1.1).
- CH client wrapper (`internal/chclient`) over `clickhouse-go/v2` with a testcontainers integration test.
- CI: two-job workflow (`check` + `lint`), commitlint relaxed for Dependabot, markdownlint, mutation testing (gremlins) on a nightly cron.
- Branch protection on `main`: required checks, linear history, no force pushes / deletions.

[v1.0.0-RC1]: https://github.com/tsouza/cerberus/releases/tag/v1.0.0-RC1
[v0.1.0]: https://github.com/tsouza/cerberus/releases/tag/v0.1.0
