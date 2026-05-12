# Changelog

All notable changes to cerberus will be documented in this file. The format roughly follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), with one entry per tagged release.

## [Unreleased]

### Added

- Five-phase RC roadmap (RC1–RC5) plus RC6 sqlbuilder refactor in `docs/roadmap.md`.

## [v1.0.0-RC1] — Full PromQL / LogQL / TraceQL slice

The first release candidate. PromQL is functionally complete against the `prometheus/compliance` suite (informational baseline tracked); LogQL covers stream selectors, label filters, the metric form, and aggregations; TraceQL covers spanset filters, structural relations, and `count()`. The Prom HTTP head is fully wired into Grafana; Loki's `query` / `query_range` work end-to-end.

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

#### TraceQL (M4.1 – M4.3)

- `schema.Traces` + `chplan.FieldAccess` for dotted-path attribute references; SpansetFilter with intrinsic resolution (`duration`, `name`, `kind`, `status`, `traceID`, `spanID`, `parent`) and scope-prefixed paths (`resource.` → ResourceAttributes, `span.` → SpanAttributes). [#64]
- Direct structural ops `>` (parent of) and `<` (child of) via `chplan.StructuralJoin` rendering an INNER JOIN of two span subqueries on `(TraceId, ParentSpanId)`. [#65]
- `| count() > 0` aggregate + scalar-filter wrapping; reuses the M1.4 `chplan.Aggregate` shape. [#66]

#### Engineering / CI

- Required-status checks: `check`, `lint`, `dashboard` (full-stack k3d + cerberus + Grafana + Playwright smoke). `enforce_admins: true`; `gh pr merge --admin` is forbidden. [#56, #59, #60]
- Compliance harness drops the `pull_request` trigger until M6; runs nightly + on `main` push as informational baseline. [#56]
- RC6 roadmap entry with hard rule: no `fmt.Sprintf` (or string concatenation) for ClickHouse SQL going forward; existing emitter Sprintf is grandfathered until R6.1–R6.10 ports it through `huandu/go-sqlbuilder`. [#57]

### Deferred to RC2

- PromQL: subqueries, `histogram_quantile` over native histograms, `topk` / `bottomk` / `count_values` (output-shape changes).
- LogQL: parser stages (`| json`, `| logfmt`, `| regexp`, `| pattern`); `unwrap`-based ops; `tail`; `index/stats`.
- TraceQL: recursive structural ops `>>` / `<<`, set ops, sibling ops; `sum/avg/max/min` over inner attributes (Tempo's parser keeps the inner expression on an unexported field — needs an upstream accessor); `| select(...)`.
- Loki HTTP: `/labels`, `/label/.../values`, `/series` (gated on RC6 R6.1's sqlbuilder integration so the new SQL is type-safe), stream-aware row decoder, `tail`.
- Tempo HTTP: `/api/search`, `/api/traces/<id>`, `/api/search/tags`, `/api/search/tag/<n>/values`.

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

[Unreleased]: https://github.com/tsouza/cerberus/compare/v1.0.0-RC1...HEAD
[v1.0.0-RC1]: https://github.com/tsouza/cerberus/releases/tag/v1.0.0-RC1
[v0.1.0]: https://github.com/tsouza/cerberus/releases/tag/v0.1.0
