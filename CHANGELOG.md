# Changelog

All notable changes to cerberus will be documented in this file. The format roughly follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), with one entry per tagged release.

## [Unreleased] — towards v1.0.0-RC1

The seed (PR1–PR7 + admin + v0.1.0) plus M1–M4 + corpus expansion + Playwright + E2E HTTP tests + RC6 / RC7 plan consolidation. All RC1 work is now in flight or merged; the tag is the only step left.

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

[Unreleased]: https://github.com/tsouza/cerberus/compare/v0.1.0...HEAD
[v0.1.0]: https://github.com/tsouza/cerberus/releases/tag/v0.1.0
