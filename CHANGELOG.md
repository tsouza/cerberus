# Changelog

All notable changes to cerberus will be documented in this file. The format roughly follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), with one entry per tagged release.

## [Unreleased]

(Work toward v1.0.0-RC3 lands here. See [`docs/roadmap.md`](docs/roadmap.md) for the milestone backlog.)

### Added

- Sort-key-aware filter emission + `PREWHERE` promotion (RC3 R3.4). The chsql emitter now fuses `Filter(Scan)` into a single `SELECT … FROM <table> [PREWHERE …] WHERE …` and partitions conjuncts into a sort-prefix bucket / skip-index bucket / rest, then promotes cheap predicates that touch no wide column into `PREWHERE` when the projection reads any wide column. ~219 existing TXTAR fixtures across `test/spec/{chsql,promql,logql,traceql,optimizer}` were re-emitted; the diff is a pure structural rewrite (one less subquery layer, predicates reordered by sort-key rank, optional `PREWHERE` split) and the rendered SQL is semantically equivalent. 29 of the re-emitted fixtures now carry a `PREWHERE` clause. New unit tests cover the predicate classifier (`prewhere_test.go`); new `test/spec/codegen/prewhere/` fixtures pin the four codegen-only behaviours (wide-column-excluded, partial-promotion, no-wide-no-promotion, sort-prefix-order).

## [v1.0.0-RC2]

The advanced-QL + deferred-API release. Closes the RC2 backlog from [`docs/roadmap.md`](docs/roadmap.md) § RC2: PromQL subqueries (P0 4.1–4.11), `predict_linear` / `holt_winters` / `@start()` / `@end()`, `histogram_quantile` over both classic and native (exp) histograms, `group_left` / `group_right` cardinality edges; LogQL `| unpack`, `| pattern`, `| line_format`, `| decolorize`, `| label_format` template stages with Loki template funcs, `bytes_*` alignment, `/api/v1/tail` WebSocket, `/labels`, `/label/.../values`, `/series`, `/detected_fields`, `/patterns`, `/index/stats`, `/index/volume`; TraceQL `status = error` / `kind = client` enum statics, `sum / avg / max / min` over inner attributes, link traversal + span-event queries, set ops, `group / coalesce` pipeline elements, `histogram_over_time`, MetricsPipeline lowering, Tempo `/api/search/recent`, `/api/search/tags`, `/api/search/tag/<n>/values`, `/api/metrics/query_range`. The Tempo `unsafe.Pointer` shim is retired via the [`tsouza/tempo:cerberus-accessors`](https://github.com/tsouza/tempo/tree/cerberus-accessors) fork; the OTel CH Exporter schema is now the source of truth via the [`tsouza/opentelemetry-collector-contrib:cerberus-ddl`](https://github.com/tsouza/opentelemetry-collector-contrib/tree/cerberus-ddl) fork (no more hand-maintained DDL). ~71 PRs merged since `v1.0.0-RC1`.

### Added

#### PromQL (RC2)

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
- `histogram_quantile` on classic histograms (RC2 schema G). [#170]
- `histogram_quantile` on native (exp) histograms (RC2 schema H). [#171]

#### LogQL (RC2)

- Point stale "not yet supported" LogQL messages at RC3. [#118]
- `| line_format` + `| decolorize` as Go-side post-process. [#124]
- Handle nil predicate from no-op stages; `line_format` / `decolorize` handler tests. [#127]
- TXTAR fixtures for `| line_format` and `| decolorize`. [#128]
- `| label_format` rename + template stages. [#130]
- Expose Loki template funcs in `| line_format` / `| label_format`. [#132]
- `/index/stats` + `/index/volume` handlers. [#141]
- `| unpack` + `| pattern` parser stages. [#142]
- `/labels`, `/label/values`, `/series`, `/detected_fields`, `/patterns` handlers. [#151]
- `bytes_*` alignment + `/api/v1/tail` WebSocket. [#157]

#### TraceQL (RC2)

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

#### Schema source-of-truth (RC2 schema A–H, plus the `tsouza/opentelemetry-collector-contrib` fork)

- Wire `tsouza/opentelemetry-collector-contrib:cerberus-ddl` via `replace`. [#154]
- Mirror upstream OTel CH Exporter columns (exp_histogram + missing classic histogram fields). [#158]
- Wrap upstream OTel CH Exporter DDL via the `schema/ddl` package. [#161]
- `CERBERUS_AUTO_CREATE_SCHEMA` env-gated startup hook (RC2 schema D). [#166]
- Refactor `harness/prometheus-compliance` to seed via `schema/ddl` package (RC2 schema F). [#167]
- Refactor `test/e2e` to seed via `schema/ddl` package + Go fixture inserts (RC2 schema E). [#168]
- Self-contained `otel-collector` + `sample-app` for real E2E data (RC2). [#172]

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
- RC6 R6.0 — SQL builder evaluation recommends custom builder. [#125]
- Align CLAUDE.md + roadmap with R6.0 custom-builder choice. [#129]
- Scaffold public `chsql.Builder` + `QueryBuilder` (RC6 R6.1). [#131]
- Fuzz + perf-benchmark workflow scaffolds (RC3 R3.11). [#133]
- Scaffold local Go PromQL evaluator (RC3 R3.10). [#134]
- Pattern-based optimizer `Rule` API scaffold (RC3 R3.1). [#135]
- Scaffold differential-testing harness (RC3 R3.9). [#136]
- Port `emitScan` / `Filter` / `Project` / `Limit` to Builder (RC6 R6.2). [#138]
- Plan to fork upstream Tempo, retire `unsafe.Pointer` shims. [#139]
- Port `emitAggregate` + `emitAggFunc` to Builder (RC6 R6.3). [#140]
- Wire `tsouza/tempo:cerberus-accessors` fork via `replace` directive. [#143]
- Run dashboard E2E on merge-to-main only, drop PR trigger. [#145]
- Actually use `QueryBuilder` for R6.2 / R6.3 ports + repo audit. [#146]
- Retire `unsafe.Pointer` + reflect shims via `tsouza/tempo` accessors. [#148]
- Tighten no-raw-SQL rule to forbid `Builder.WriteSQL` clause keywords. [#149]
- `forbidigo` lint forbids `unsafe.Pointer` + `reflect.FieldByName` in `internal/traceql`, `internal/api/tempo`. [#152]
- Plan 6 scalability levers across RC3 / RC4 / RC5. [#155]
- Add lefthook pre-commit + commit-msg hooks (formatters only; CI owns validation). [#162]
- Drop empty `pkg/` — cerberus is a service, not a library. [#165]

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

## [v1.0.0-RC1]

The seed (PR1–PR7 + admin + v0.1.0) plus M1–M4 (full PromQL / LogQL / TraceQL parsing → lowering → execution) + corpus expansion (TXTAR 122 → 166 fixtures, ~280 new unit-test sub-cases, E2E HTTP 12 → 26, Playwright 10 → 19 scenarios) + RC6 / RC7 plan consolidation.

Six pre-existing cerberus bugs surfaced by the RC1 test-coverage push are tracked on the project board as RC2 deferrals (each with a cross-referenced `t.Skip` / `test.skip` marker in the source): wrap-projection over RangeWindow / StructuralJoin / Filter(Aggregate) column-scope mismatch, and bare-scalar PromQL (`1+1`) not lowering.

### Added

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

#### Test corpus expansion (RC1 prereq)

- TraceQL TXTAR fixtures grow from 8 to 26 — boolean `||`, regex / not-regex matchers, every intrinsic (`name`, `kind`, `statusMessage`, `parent`, scoped `trace:id` / `span:id`), span-attribute scoping variants, scalar-filter thresholds, and resource-scoped select projection. [#72]
- chsql TXTAR fixtures grow from 15 to 29 — direct tests for every chplan IR node (VectorJoin, StructuralJoin, MapWithoutKeys, LineContent variants, parameterised `quantile`, RangeWindow with `Offset` + LogQL `log_rate`, FieldAccess, FuncCall). [#74]
- Meaningful Grafana Playwright scenarios for all three datasources: LogQL streams + metric, TraceQL search + traceByID, richer PromQL (rate matrix + labels + metric names). Per-signal seed files (`otel_logs.sql`, `otel_traces.sql`). [#76]
- Cerberus-side HTTP integration tests for every shipped surface: Prom rate / labels / label-values, Loki streams + metric, Tempo echo / version / search / trace-by-id (found + not-found). [#77]

#### Engineering / CI

- Required-status checks: `check`, `lint`, `dashboard` (full-stack k3d + cerberus + Grafana + Playwright smoke). `enforce_admins: true`; `gh pr merge --admin` is forbidden. [#56, #59, #60]
- Compatibility harness drops the `pull_request` trigger until M6; runs nightly + on `main` push as informational baseline. [#56]
- RC6 roadmap with hard rule: no `fmt.Sprintf` (or string concatenation) for ClickHouse SQL going forward; existing emitter Sprintf is grandfathered until R6.1–R6.10 port it through a typed builder. [#57]
- RC6 R6.0 — SQL-builder evaluation phase prepended to RC6: a written security + impact + build-vs-buy analysis recommends third-party (`huandu/go-sqlbuilder` + cerberus extension layer), custom (`internal/chsql.Builder`), or defer. [#73]
- RC7 — `internal/engine/` ExecutionEngine framework planned with the same R7.0 evaluation-first pattern: audit pipeline divergence across 5 callsites before any code lands; recommendation among (a) Build, (b) Partial — helpers-only extraction, (c) Defer. RC2 narrative gains the self-contained-deployment item (OTel Collector + CH exporter creating schema in k3d). [#75]

### Deferred to RC2

- PromQL: subqueries, `histogram_quantile` over native histograms, `topk` / `bottomk` / `count_values` (output-shape changes).
- LogQL: parser stages (`| json`, `| logfmt`, `| regexp`, `| pattern`); `unwrap`-based ops; `tail`; `index/stats`.
- TraceQL: recursive structural ops `>>` / `<<`, set ops, sibling ops; `status = error` / `kind = client` enum statics (Tempo's typed-static encoding needs Status/Kind enum support in `lowerStatic`); `sum / avg / max / min` over inner attributes (Tempo's `Aggregate.e` is on an unexported field — needs an alternative extraction path).
- Loki HTTP: `/labels`, `/label/.../values`, `/series` (gated on RC6 R6.1's sqlbuilder integration so the new SQL is type-safe), stream-aware row decoder, `tail`.
- Tempo HTTP: `/api/search/tags`, `/api/search/tag/<n>/values` (same RC6 gate); `search/recent`, `metrics/query_range`.
- Self-contained deployment: OTel Collector + CH exporter in k3d creating schemas and collecting real k8s telemetry, replacing synthetic `*.sql` seeding for E2E (synthetic stays for unit / spec tests).

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

[Unreleased]: https://github.com/tsouza/cerberus/compare/v1.0.0-RC2...HEAD
[v1.0.0-RC2]: https://github.com/tsouza/cerberus/releases/tag/v1.0.0-RC2
[v1.0.0-RC1]: https://github.com/tsouza/cerberus/releases/tag/v1.0.0-RC1
[v0.1.0]: https://github.com/tsouza/cerberus/releases/tag/v0.1.0
