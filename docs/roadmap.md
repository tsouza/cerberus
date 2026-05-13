# Cerberus roadmap — v1.0.0

This document is the public-facing narrative for the path to `v1.0.0`. Status by milestone lives in the [GitHub Project](https://github.com/users/tsouza/projects) — _Cerberus v1.0.0 Roadmap_. Per-PR-level reasoning lives in the PR descriptions themselves; we don't use GitHub Issues.

## At a glance

| Release        | Theme                                                                                 | What "done" means                                                                                 |
| -------------- | ------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------- |
| **v1.0.0-RC1** | Full PromQL / LogQL / TraceQL support + 90% upstream API compatibility                | Compatibility corpora pass; Grafana sees cerberus as drop-in for Prom / Loki / Tempo              |
| **v1.0.0-RC2** | Advanced QL features + deferred API surface                                           | Subqueries, native-histogram quantiles, structural-chain TraceQL, LogQL `\| unpack`, Loki `tail`… |
| **v1.0.0-RC3** | Optimizer rewrite + performance + scalability + advanced testing                      | Pattern-based rules, MV substitution, streaming cursor, shadow-mode differential testing          |
| **v1.0.0-RC4** | Full self-observability                                                               | Cerberus emits its own structured logs (slog), OTel metrics + traces, defaults to the same CH     |
| **v1.0.0-RC5** | 12-factor compatibility + scale-out polish                                            | `/readyz` pings CH, admission control, HPA recipe, dev `docker-compose.yml`, schema overrides     |
| **v1.0.0-RC6** | Type-safe SQL via custom `internal/chsql.Builder` (R6.0 evaluation → R6.1–R6.10 port) | No `fmt.Sprintf`-on-SQL anywhere; typed builder with CH-specific helpers; lint enforcement        |
| **v1.0.0-RC7** | `internal/engine/` ExecutionEngine framework (R7.0 evaluation → R7.1–R7.8 port)       | One pipeline owner; handlers under ~150 LoC each; shared format + httperr helpers                 |
| **v1.0.0**     | Tag the last green RC                                                                 | All RCs stable; HTTP wire protocols are the public surface, not a Go API                          |

---

## RC1 — full QL + 90% API shipped at `v1.0.0-RC1`

Closed milestones M0 (seed + compatibility harness scaffold), M1 (PromQL → 90% via `prometheus/compliance`), M2 (Prom HTTP API completion), M3 (LogQL slice + Loki HTTP API), M4 (TraceQL slice + Tempo HTTP API), M5 (`v1.0.0-RC1` tag + `docs/compatibility.md`). See `CHANGELOG.md` § `[v1.0.0-RC1]` and `git log v1.0.0-RC1` for the per-PR breakdown.

The **3-rule optimizer** (filter-fusion, constant-fold, projection-pushdown) ships unchanged through RC1 and RC2. No new optimizer work happens before RC3 — its full backlog lives in [`docs/optimizer-research.md`](optimizer-research.md).

---

## RC2 — advanced QL + API features shipped at `v1.0.0-RC2`

Closed the advanced-QL + deferred-API backlog plus the schema-source-of-truth migration. See `CHANGELOG.md` § `[v1.0.0-RC2]` for the full ~71-PR entry and the GitHub Project's RC2 column for per-item PR refs.

Highlights:

- **PromQL** — subqueries (P0 4.1–4.11), `predict_linear` / `holt_winters` / `@start()` / `@end()`, `histogram_quantile` over classic + native (exp) histograms, `group_left` / `group_right` cardinality edges. Subquery-over-aggregator + nested subqueries are deferred to RC3.
- **LogQL** — `| unpack`, `| pattern`, `| line_format`, `| decolorize`, `| label_format` (with Loki template funcs), `bytes_*` alignment, `/api/v1/tail` WebSocket (bounded send buffer + `ctx.Done()` drop), `/labels`, `/label/.../values`, `/series`, `/detected_fields`, `/patterns`, `/index/stats`, `/index/volume`.
- **TraceQL** — `status = error` / `kind = client` enum statics, `sum / avg / max / min` over inner attributes, link traversal + span-event queries, set ops, `group / coalesce` pipeline elements, `histogram_over_time`, MetricsPipeline lowering, multi-hop + recursive `>>` / `<<` chains via CH `WITH RECURSIVE` CTEs.
- **Tempo HTTP API** — `/api/search/recent`, `/api/search/tags`, `/api/search/tag/<n>/values`, `/api/metrics/query_range`.
- **Self-contained k3s deployment** — `deploy/k3s/otel-collector.yaml` (per-node DaemonSet + gateway Deployment wired to the CH exporter) plus `deploy/k3s/sample-app.yaml` (telemetrygen). E2E now reads real OTel data through Grafana.
- **Tempo fork wired** — `unsafe.Pointer` + `reflect.FieldByName` shims retired against `tsouza/tempo:cerberus-accessors`; `forbidigo` gates regressions. See [`docs/upstream-forks.md`](upstream-forks.md).
- **Schema source-of-truth migration** — OTel-CH exporter schema is now the source via `tsouza/opentelemetry-collector-contrib:cerberus-ddl`; `internal/schema/ddl/` consumes the upstream `sqltemplates` API; auto-create startup hook + e2e + compatibility seeders migrated.

---

## RC3 — optimizer + performance + advanced testing (in flight)

All of [`docs/optimizer-research.md`](optimizer-research.md) lands here. The reading list is the contract.

### Optimizer rewrite

| #    | Item                                                        | Status           | Primary reference                                                                                                                                          |
| ---- | ----------------------------------------------------------- | ---------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| R3.1 | Pattern-based `Rule` API (Calcite-style match / transform)  | shipped via #135 | [Apache Calcite `org.apache.calcite.rel.rules`](https://calcite.apache.org/javadocAggregate/)                                                              |
| R3.2 | `FilterProjectTranspose` + `FilterAggregateTranspose` rules | shipped via #177 | Same                                                                                                                                                       |
| R3.3 | Catalyst-style `Batch` grouping                             | shipped via #PR  | [Spark `Optimizer.scala`](https://github.com/apache/spark/blob/master/sql/catalyst/src/main/scala/org/apache/spark/sql/catalyst/optimizer/Optimizer.scala) |
| R3.4 | Sort-key-aware filter emission + `PREWHERE` promotion       | shipped via #PR  | [ClickHouse query-optimization guide](https://clickhouse.com/resources/engineering/clickhouse-query-optimisation-definitive-guide)                         |
| R3.5 | Analyzer vs Optimizer rule split                            | Todo             | [DataFusion optimizer crate](https://docs.rs/datafusion-optimizer/latest/datafusion_optimizer/)                                                            |

### Performance features

R3.4 / R3.6 / R3.7 / R3.8 are the **CH-roundtrip scalability levers** — they shrink the amount of data CH scans and ships per query, which is where the real wins live. R3.12 is the **process-side** scalability lever — it caps RAM growth on large result sets. PREWHERE promotion (R3.4) plus streaming cursor (R3.12) means a 1M-point query both narrows the scan and never materialises the full result in cerberus RAM.

**No query/plan/SQL caching.** Cerberus is a thin query gateway — caching results or plans turns it into a memoization layer, which (a) hides freshness bugs behind stale results, (b) shifts the correctness burden from CH to cerberus, and (c) duplicates work Grafana / Prometheus / Loki already do client-side. If a deployment wants caching, put it in front of cerberus (e.g. a Grafana datasource cache) — not inside.

| #     | Item                                                                                  | Status           | Primary reference                                                                                                                         |
| ----- | ------------------------------------------------------------------------------------- | ---------------- | ----------------------------------------------------------------------------------------------------------------------------------------- |
| R3.6  | Materialised-view substitution for `otel_metrics_*` rollups (cost-model trigger)      | Todo             | [Promscale #152](https://github.com/timescale/promscale/issues/152) + [Jindal VLDB 2018](http://www.vldb.org/pvldb/vol11/p800-jindal.pdf) |
| R3.7  | Late materialisation for wide-column scans (logs `Body`, `ResourceAttributes`)        | Todo             | [Selective Late Materialization, VLDB 2025](http://people.iiis.tsinghua.edu.cn/~huanchen/publications/slm-vldb25.pdf)                     |
| R3.8  | Filter–RangeWindow transpose                                                          | Todo             | [VictoriaMetrics `metricsql/optimizer.go`](https://github.com/VictoriaMetrics/metricsql/blob/master/optimizer.go)                         |
| R3.12 | Streaming `query_range` matrix response cursor (`chclient.Cursor` over `Sample` rows) | shipped via #175 | Stops handlers from materialising the full matrix; 1M-point query memory drops from O(N) to O(chunk_size). Composes with R3.4.            |

### Advanced testing

| #     | Item                                                              | Status           | Primary reference                                                                                                                  |
| ----- | ----------------------------------------------------------------- | ---------------- | ---------------------------------------------------------------------------------------------------------------------------------- |
| R3.9  | Shadow-mode differential testing (prefer / force-native / oracle) | shipped via #136 | [promshim-clickhouse `harness/compatibility/`](https://github.com/BadLiveware/promshim-clickhouse/tree/main/harness/compatibility) |
| R3.10 | Port promshim's local Go evaluator                                | shipped via #134 | Same — `internal/promshim/local/`                                                                                                  |
| R3.11 | Fuzz + chaos + perf-benchmark CI                                  | shipped via #133 | `go-fuzz`, custom chaos harness, perf-benchmark workflow                                                                           |

**Exit criterion:** golden-fixture SQL shrinks on real plans; `internal/optimizer` mutation score ≥ 70%; MV substitution active; shadow-mode reveals < 5% native-SQL gap; `chclient.Cursor` streams a 1M-row fixture with bounded RSS.

---

## RC4 — full self-observability

Cerberus instruments itself with the Go-ecosystem defacto stack and ships telemetry into the same OTel-CH schema it queries. Eats its own dogfood: a cerberus self-dashboard in Grafana queries cerberus, which queries CH, which holds cerberus's own metrics + logs + traces.

### Stack

| Signal  | Library                                                         | Why                                                                                         |
| ------- | --------------------------------------------------------------- | ------------------------------------------------------------------------------------------- |
| Logging | stdlib `log/slog` (Go 1.21+)                                    | Already wired in `cmd/cerberus/main.go`. The defacto for new Go projects.                   |
| Metrics | OpenTelemetry SDK (`go.opentelemetry.io/otel/sdk/metric`)       | OTLP-native → OTel Collector → CH exporter → same `otel_metrics_*` tables cerberus queries. |
| Traces  | OpenTelemetry SDK + `contrib/instrumentation/net/http/otelhttp` | Auto HTTP spans + manual pipeline spans (parse → lower → optimize → emit → execute).        |

### Milestones

| #    | Item                                                                                                                                                                                                |
| ---- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| R4.1 | Logging quality pass: consistent slog fields (`req_id`, `ql`, `query`, `sql_len`, `duration_ms`, `error_kind`), text+json formats, level env                                                        |
| R4.2 | `otelhttp.NewHandler` wraps the Prom/Loki/Tempo handlers; each request gets a span                                                                                                                  |
| R4.3 | Custom spans around `promql.Lower` / `logql.Lower` / `traceql.Lower` / `optimizer.Default().Run` / `chsql.Emit` / `chclient.Query`                                                                  |
| R4.4 | Self-metrics: request count + latency histogram by route + status; CH roundtrip count + duration; plan IR node count; `cerberus_http_requests_in_flight` gauge per route (HPA-consumable, see R5.7) |
| R4.5 | OTLP exporters: `CERBERUS_OTEL_ENDPOINT` / `_INSECURE` / `_SAMPLER` / `_SERVICE_NAME`; graceful no-op when endpoint unreachable                                                                     |
| R4.6 | Wire cerberus's OTLP export into `deploy/k3s/otel-collector.yaml` (landed in RC2) + provisioned `deploy/grafana/dashboards/cerberus-self.json`                                                      |
| R4.7 | `docs/observability.md`                                                                                                                                                                             |

**Exit criterion:** every Prom/Loki/Tempo request emits one span with pipeline stage timings; self-dashboard renders cerberus's own request rate + p99 latency; disabling OTel via `CERBERUS_OTEL_ENDPOINT=""` produces a zero-collector-dependency binary.

---

## RC5 — 12-factor compatibility + polish

Driven by an audit of cerberus against [12factor.net](https://12factor.net/). Most factors pass today; this RC closes the gaps and documents the rest.

### Audit snapshot

| #  | Factor            | Status  | Note                                                                                  |
| -- | ----------------- | ------- | ------------------------------------------------------------------------------------- |
| 1  | Codebase          | PASS    | Single repo + Go module.                                                              |
| 2  | Dependencies      | PASS    | `go.mod` explicit; `replace` directives documented.                                   |
| 3  | Config            | PASS    | Env vars only (`CERBERUS_*`); no flags, no runtime YAML.                              |
| 4  | Backing services  | PASS    | CH swappable via env.                                                                 |
| 5  | Build/release/run | PASS    | goreleaser + Dockerfile + release.yml; immutable artifacts.                           |
| 6  | Processes         | PASS    | Stateless; horizontal scale via Deployment replicas.                                  |
| 7  | Port binding      | PASS    | `:8080` self-binding; env-configurable.                                               |
| 8  | Concurrency       | PASS    | Goroutines + Deployment replicas.                                                     |
| 9  | Disposability     | PASS    | `signal.NotifyContext` + 10s graceful-shutdown deadline.                              |
| 10 | Dev/prod parity   | PARTIAL | Same image runs everywhere; missing a one-command `docker-compose.yml` for local dev. |
| 11 | Logs              | PASS    | `slog` to stderr; no log files; no rotation.                                          |
| 12 | Admin processes   | PASS    | None today; future admin tasks would land as separate one-offs.                       |

### Milestones

| #    | Item                                                                                                                                                                                                                                              |
| ---- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| R5.1 | `/readyz` distinct from `/healthz`; readiness actually pings CH (`chclient.Ping`) and fails-open within a small TTL cache so the probe doesn't hammer CH; k8s manifests switched to readiness probe                                               |
| R5.2 | Repo-root `docker-compose.yml` for one-command local dev (CH + OTel Collector + cerberus)                                                                                                                                                         |
| R5.3 | Env-driven schema overrides via `CERBERUS_SCHEMA_OVERRIDES_JSON`                                                                                                                                                                                  |
| R5.4 | `docs/12factor.md` with file-line citations per factor                                                                                                                                                                                            |
| R5.5 | Startup-speed benchmark: process-start → `/healthz` 200 against a reachable CH; target < 2s                                                                                                                                                       |
| R5.6 | Per-handler concurrency cap / admission control via `golang.org/x/sync/semaphore`, env-driven (`CERBERUS_MAX_INFLIGHT_PROM` / `_LOKI` / `_TEMPO` / `_TAIL`). Surfaces backpressure as `503` + `Retry-After` instead of CH-side timeouts. ~250 LoC |
| R5.7 | Horizontal scale recipe: example `deploy/k3s/cerberus-hpa.yaml` driven by the R4.4 self-metrics (`cerberus_http_requests_in_flight`), with a short `docs/12factor.md` § on the scale-out story                                                    |

**Exit criterion:** `docker compose up` at repo root brings the dev stack up in < 30s; `CERBERUS_SCHEMA_OVERRIDES_JSON` honoured; `docs/12factor.md` exists with per-factor evidence; startup benchmark passes < 2s in CI; admission-control unit test demonstrates `503 Retry-After` under saturation; example HPA manifest scales replicas from `cerberus_http_requests_in_flight` in a k3d smoke test.

---

## RC6 — type-safe SQL via custom `internal/chsql.Builder`

**Hard rule (non-negotiable, takes effect at RC6 cut):** plain strings and `fmt.Sprintf` are forbidden for ClickHouse SQL generation. Every emitted SQL fragment goes through `internal/chsql.Builder` — a custom CH-flavored builder tailored to chplan IR. (R6.0's [build-vs-buy evaluation](sql-builder-evaluation.md) considered third-party `huandu/go-sqlbuilder` + extension layer vs. the custom path; the custom path won on impedance match to chplan IR and on minimising new dependency surface.) Concatenation and templated identifiers are an injection vector even with parameterised values; the builder tracks identifiers, args, and dialect quoting in one place.

### Why now (and not earlier)

Through RC1 the emitter grew Sprintf-driven for speed: `internal/chsql/emit_node.go`, `emit_expr.go`, `range_window.go`, `vector_join.go`, plus the metadata SQL in `internal/api/prom/metadata.go`. That worked for landing the surface, but every helper is hand-quoted, each subquery is a string-pasted hole, and there's no central ClickHouse dialect handling. RC6 reorganises that into a typed, builder-first emitter so RC3's optimizer changes can compose SQL programmatically (PREWHERE promotion, sort-key-aware predicate ordering, MV substitution) without re-scaffolding the string layer.

### Helper inventory (the CH-specific surface `chsql.Builder` exposes)

These are the CH-flavored helpers R6.1 landed. They sit on top of the same `strings.Builder` + `[]any` args mechanics the current emitter already uses, just exposed as a named public API:

- **Map column access** — `Attributes['job']`, `mapKeys(Attributes)`, `mapFilter((k,v) -> ..., Attributes)`. Named helpers: `chsql.MapAt(col, key)`, `chsql.MapKeys(col)`, `chsql.MapFilterExcept(col, keys...)`. Each emits its CH fragment and appends args to the surrounding builder.
- **Array idiom for RangeWindow** — `groupArray((ts, value))`, `arraySort`, `arrayPopBack`, `arrayPopFront`, `arrayMap((p, c) -> ..., a, b)`, `arrayFilter`. R6.5 landed these as typed fragment helpers.
- **Parameterised aggregates** — `quantile(0.95)(value)`, `quantiles(0.5, 0.9)(value)`. `chsql.ParamAgg(name, params, args)` renders the CH-specific `agg(params)(args)` shape.
- **DateTime64 literals + interval arithmetic** — `now64(9) - toIntervalNanosecond(N)`, `toDateTime64('2026-...', 9)`. Helpers `chsql.Now64()`, `chsql.SubtractNanos(expr, ns)`, `chsql.DateTime64Lit(t)`.
- **CH lambda syntax** — `(k, v) -> NOT (k IN ('a', 'b'))`. A `chsql.Lambda(params, body)` helper avoids the freestyle string trap.
- **PREWHERE clause** (RC3 sort-key emission) — `chsql.QueryBuilder.Prewhere(cond...)` distinguishes PREWHERE from WHERE explicitly so RC3 optimizer rules can promote predicates without re-parsing the SQL.

CH identifier quoting (`writeIdent` — backticks with doubled-backtick escaping) is preserved as a private method on the builder; there is no need for a `NewFlavor`-style abstraction since cerberus only ever emits CH.

### PR sequence

| #     | Item                                                                                                                                                                                                                                                                                                                                                   | Status           |
| ----- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ---------------- |
| R6.0  | Evaluation phase: `docs/sql-builder-evaluation.md` recommends path (b) — build custom `internal/chsql.Builder`.                                                                                                                                                                                                                                        | shipped          |
| R6.1  | Scaffolding: `internal/chsql/builder.go` as a public Builder API plus the CH-specific helpers (`MapAt`, `MapKeys`, `MapFilterExcept`, `Now64`, `SubtractNanos`, `DateTime64Lit`, `Lambda`, `ParamAgg`) and a `QueryBuilder` supporting `.Prewhere(cond...)`. Unit tests pin each helper's output. No emitter changes — pure scaffolding.               | shipped via #131 |
| R6.2  | Port `emitScan`, `emitFilter`, `emitProject`, `emitLimit` (the simple ones).                                                                                                                                                                                                                                                                           | shipped via #138 |
| R6.3  | Port `emitAggregate` + `emitAggFunc` (parameterised aggregates use `chsql.ParamAgg`).                                                                                                                                                                                                                                                                  | shipped via #140 |
| R6.4  | Port `emitBinary` + `emitMapAccess` + `emitMapWithoutKeys` + `emitFunc` + `emitLineContent` (`internal/chsql/emit_expr.go`).                                                                                                                                                                                                                           | shipped via #179 |
| R6.5  | Port `emitRangeWindow` (`internal/chsql/range_window.go` — the `arraySort/groupArray` windowed-array idiom) and `emit_node.go::emitOrderBy`.                                                                                                                                                                                                           | shipped via #182 |
| R6.6  | Port `emitVectorJoin` (per-series argMax + INNER JOIN) **and** the full `internal/chsql/structural_join.go` (both `emitStructuralDirectJoin` and `emitStructuralRecursive`). Adds typed `QueryBuilder.Join(kind, src, on)` + `QueryBuilder.WithRecursive(name, anchor, recursive)` helpers.                                                            | shipped via #181 |
| R6.7  | Port `internal/api/prom/metadata.go` UNION builders + `unionLabelValuesSQL` + `metricMetaSQL`.                                                                                                                                                                                                                                                         | shipped via #180 |
| R6.8  | Audit `internal/api/loki/` and `internal/api/tempo/` SQL helpers and port any remaining Sprintf-on-SQL callsites. Most loki + tempo helpers were already builder-aware by RC2 cut (#141, #150) — R6.8 is the cleanup sweep.                                                                                                                            | Todo             |
| R6.9  | **Lint enforcement**: a `cmd/check-sql/` Go tool wired into `just check-sql` (and the `lint` CI job) that scans `internal/chsql/`, `internal/api/`, `internal/optimizer/` for `fmt.Sprintf` calls whose first arg contains `SELECT`, `WHERE`, `FROM`, `INSERT`, etc., plus the `Builder.WriteSQL("…keyword…")` cosplay shape. Fails CI on regressions. | Todo (in-flight) |
| R6.10 | CLAUDE.md hard-rule promotion: `**No raw SQL strings.**` becomes a top-level non-negotiable; `docs/sql-style.md` documents the helper API and its escape hatches (when `chsql.Raw()` is acceptable, when extending `chsql.QueryBuilder` is preferable).                                                                                                | Todo             |

### Notes for the custom path

Path **(b)** owns its quoting, args lifecycle, and performance characteristics by design — there is no third-party API to bridge. Concrete commitments:

- **Identifier quoting** stays on the existing `quoteIdentCH` / `writeIdent` helpers, lifted into the builder as a private method. CH backtick rules + backtick-doubling for embedded backticks are preserved.
- **Args lifecycle**: the builder owns a `[]any` slice and renders `?` placeholders in the order args are appended. Nested `QueryBuilder` instances flatten their args into the parent on render. R6.1 unit tests verify ordering against a worst-case nested subquery (windowed-array RangeWindow).
- **Performance**: builder construction is allocation-heavier than the current `strings.Builder` direct writes. Bench against a 1000-iteration emit loop on the largest fixture as part of R6.5 (RangeWindow port). Record the number so RC3's optimizer benchmarks stay honest.

### RC6 exit criteria

- `grep -RIn 'fmt.Sprintf' internal/chsql/ internal/api/ | grep -E 'SELECT|FROM|WHERE|INSERT'` returns empty (the R6.9 lint gate enforces this in CI).
- Every TXTAR fixture either matches its prior golden bit-for-bit or has a documented whitespace/aliasing diff in the porting PR's description.
- `internal/chsql/builder_test.go` has ≥ one test per helper in `internal/chsql/builder.go`, exercising the placeholder + arg ordering.
- `docs/sql-style.md` published with the helper catalog and the "when can I use Raw()?" guidance.

---

## RC7 — `internal/engine/` ExecutionEngine framework

By RC1's end, every Prom / Loki / Tempo handler runs the same five-stage pipeline — parse → lower → wrap-projection → optimize → emit → query → format — with per-QL adapters at each stage. The repetition is mechanical (5 callsites today, all with subtle drift between them) and grows by one with every new HTTP route. RC7 lifts the common loop into `internal/engine/` so handlers become thin HTTP shells over a `engine.Query(ctx, lang, q)` call. Like RC6, it starts with an evaluation phase: the framework is only worth building if the audit shows the pattern actually generalises.

### Why now (chronologically after RC6)

- The framework is most valuable _after_ RC3's optimizer changes and RC6's typed SQL land — those work products would otherwise have to be duplicated by-handler. By RC7 the shared pipeline has stable shape.
- RC4 self-observability also benefits: one Engine.Query span covers parse + lower + optimize + emit + query for every QL, rather than three separate sets of instrumentation.
- RC3's shadow-mode differential testing (R3.9) and local-Go evaluator fallback (R3.10) need a place to live; Engine is the natural strategy host.

### R7.0 — Evaluation phase (prerequisite)

Decision milestone. Output: `docs/execution-engine-evaluation.md` with a recommendation on whether to build the framework.

**Inputs to evaluate (audit must be re-run at RC7 cut — code shape will have drifted):**

1. **Pipeline-callsite inventory.** Enumerate every place that runs parse / lower / wrap / optimize / emit / query, with line counts and per-stage divergence:
   - `internal/api/prom/handler.go` `executeInstant` — full pipeline, times CH calls via `timeCH(...)` → `X-Cerberus-CH-Millis` header.
   - `internal/api/prom/metadata.go` `matcherSQL` — partial: parse / lower / optimize / emit only (no execute).
   - `internal/api/loki/handler.go` `execute` — full pipeline; conditional `wrapWithLogSampleProjection` branches on metric-vs-stream; does NOT time CH calls (regression vs Prom).
   - `internal/api/tempo/handler.go` `handleSearch` — full pipeline; does NOT time CH calls.
   - `internal/api/tempo/handler.go` `handleTraceByID` — skips parser, builds plan directly.
   - **Output:** a table of (callsite × stage × divergence × line count). The audit reveals whether the divergences are mechanical (Engine absorbs them) or semantic (per-QL needs survive).

2. **Per-handler duplication.** Code that's already copy-pasted across the three handlers and would collapse under Engine:
   - `canonicalKey`, `toVector`, `toMatrixStepGrid`, `withMetricName`, `parseTime`, `parseDuration` — three copies, nearly identical.
   - `apiError`, `writeJSON`, `writeError`, `respondError` — three copies, only the errorType-string differs.
   - `wrapWithSampleProjection` / `wrapWithLogSampleProjection` — three copies; the "shape the Sample row" responsibility is the same.
   - **Output:** estimated LoC saved by extracting these to `internal/api/format/` (response shaping) + `internal/api/httperr/` (error mapping). Engine wraps the rest.

3. **Engine surface design.** Sketch what the API would look like:

   ```go
   package engine

   type Engine struct {
       Lang      Lang             // per-QL adapter (Parse + ProjectSamples)
       Optimizer *optimizer.Driver
       Client    Querier
       Logger    *slog.Logger
   }

   type Lang interface {
       Name() string                                        // "promql" / "logql" / "traceql"
       Parse(query string) (chplan.Node, Meta, error)        // lowered plan + per-QL hints
       ProjectSamples(node chplan.Node, meta Meta) chplan.Node
   }

   type Meta struct {
       IsMetric    bool   // logql: distinguishes metric-vs-stream output
       IsTraceByID bool   // tempo: signals projection variant
       // ... extensible via untyped extras if needed
   }

   type Result struct {
       Samples       []chclient.Sample
       SQL, Strategy string
       Args          []any
       CHMillis      int64
       PlanNodeCount int
   }

   func (e *Engine) Query(ctx context.Context, q string) (Result, error)
   func (e *Engine) QueryPlan(ctx context.Context, plan chplan.Node) (Result, error)
   ```

   Per-QL adapters live in `internal/engine/lang/{promql,logql,traceql}/`.

4. **Cost/benefit.**
   - **Pros:** one instrumentation point (RC4 simplification); one strategy switch (RC3 shadow-mode / fallback hooks); one place to add rate limiting / hedging in future RCs; handlers become testable without stubbing the whole pipeline.
   - **Cons:** abstraction tax — readers chase one extra layer; LoC churn ~1k+ during the port; risk of designing the wrong seam if the per-QL drift turns out to be semantic, not mechanical.
   - **The audit's job:** prove the pattern generalises. If 3+ of the 5 callsites diverge in ways the abstraction can't absorb cleanly, recommend **defer**.

5. **Decision rule.** R7.0 produces one of three recommendations:
   - **(a) Build.** Audit shows the pipeline shape is mechanical across handlers; per-QL drift fits into a `Lang` adapter without ad-hoc escape hatches.
   - **(b) Partial.** Build only the shared-helpers extraction (`internal/api/format/` + `internal/api/httperr/`) without the Engine struct itself. Cheaper, captures most of the duplication, no abstraction tax.
   - **(c) Defer.** Per-QL divergence is large enough that the framework would carry too many escape hatches; revisit at a later RC if the divergence shrinks.

**Preliminary read** (for R7.0's PR to refute or confirm): the pipeline is mechanical across the three QLs today, but RC3 + RC4 + RC6 are likely to _increase_ per-handler divergence (timing, OTel spans, builder calls). Doing the Engine refactor _after_ those land — at RC7 — means the abstraction is informed by all the divergence axes, not just today's narrow ones. Recommendation likely **(a) Build**, but the eval might find that **(b) Partial** captures 80% of the value at 20% of the risk.

**Exit criterion for R7.0:** `docs/execution-engine-evaluation.md` exists, recommends (a) / (b) / (c), maintainer signs off. R7.1+ scope concretely against the choice.

### Implementation milestones (gated on R7.0)

| #    | Item                                                                                                                                                                                                                                           |
| ---- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| R7.1 | `internal/engine/` package with `Engine` struct + `Lang` interface + `Meta` + `Result` types. Unit tests with a fake Querier. **No handler changes yet.**                                                                                      |
| R7.2 | `internal/engine/lang/promql/` adapter. Port `internal/api/prom/handler.go` to use `engine.Query` and `engine.QueryPlan`; drop `executeInstant`, `Optimizer` field, `wrapWithSampleProjection` from the handler. Tests still pass.             |
| R7.3 | Same for `internal/engine/lang/logql/` + `internal/api/loki/handler.go`. The metric-vs-stream branch becomes a `Meta.IsMetric` flag, set by the parse step.                                                                                    |
| R7.4 | Same for `internal/engine/lang/traceql/` + `internal/api/tempo/handler.go`. `handleTraceByID` becomes `engine.QueryPlan(plan)`.                                                                                                                |
| R7.5 | Extract `internal/api/format/`: `canonicalKey`, `toVector`, `toMatrixStepGrid`, `withMetricName`, `parseTime`, `parseDuration`. Prom + Loki dedupe.                                                                                            |
| R7.6 | Extract `internal/api/httperr/`: `apiError`, `writeJSON`, `writeError`, `respondError`. Each handler keeps just its head's specific error-shape mapping (Prom errorType strings, Tempo's distinct error envelope).                             |
| R7.7 | Engine instrumentation: `Result.CHMillis` + `Result.Strategy` + `Result.PlanNodeCount` exposed via response headers (`X-Cerberus-CH-Millis`, `X-Cerberus-Strategy`, `X-Cerberus-Plan-Nodes`). Loki + Tempo gain CH timing they currently lack. |
| R7.8 | Engine becomes the natural seat for RC3's strategy switch and RC4's OTel hooks. Document the extension points in `docs/engine.md` so RC8+ work plugs in cleanly.                                                                               |

**Exit criterion:** `internal/api/{prom,loki,tempo}/handler.go` are each under ~150 LoC and contain only HTTP wrapping + response shaping; `internal/engine/` carries the pipeline; all three handlers emit `X-Cerberus-CH-Millis`; the existing TXTAR + compatibility + Playwright suites pass without changes (refactor is behavioural-equivalence).

---

## How we work

- **PR-per-change.** Every change ships as its own PR against `main`. Branch protection requires `ci / check` + `ci / lint`, linear history, no force-push.
- **Agent-driven work goes through PRs, not issues.** When the maintainer or an AI assistant is doing the work, the PR description is the source of truth — no shadow issue tracking. The GitHub Project tracks milestone status; backlog narratives live in `docs/*.md`. **External contributors** are welcome to open issues for bug reports, design questions, or feature proposals — issues are enabled.
- **Fixture-first.** A milestone's first PR adds _failing_ TXTAR / compatibility fixtures that capture the contract. Subsequent PRs implement to turn them green. Reviewers can sanity-check intent by reading fixtures before code.
- **Compatibility suite is the source of truth.** If a PromQL feature lands but doesn't move the `prometheus/compliance` pass rate, the PR is incomplete.
- **Allowlist hygiene.** Adding an entry to `harness/compatibility/expected-failures.json` requires a comment with the upstream rationale; never empty-string.
