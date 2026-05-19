# Cerberus roadmap — v1.0.0

This document is the public-facing narrative for the path to `v1.0.0`. Status by milestone lives in the [GitHub Project](https://github.com/users/tsouza/projects) — _Cerberus v1.0.0 Roadmap_. Per-PR-level reasoning lives in the PR descriptions themselves; we don't use GitHub Issues.

> **GA status:** every RC1–RC8 feature milestone has shipped; the project board's feature columns are drained. Outstanding pre-GA work is the **compatibility lane** (drive `prometheus/compliance` diffs from 46 → ≤ 5) and the **Loki + Tempo compliance harness scaffolding** (Phase 1 of each adoption plan). The maintainer cuts `v1.0.0` from the last green main once those gates close — see [§ GA exit criteria](#ga-exit-criteria).

## At a glance

| Release        | Theme                                                                  | What "done" means                                                                                 |
| -------------- | ---------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------- |
| **v1.0.0-RC1** | Full PromQL / LogQL / TraceQL support + 90% upstream API compatibility | Compatibility corpora pass; Grafana sees cerberus as drop-in for Prom / Loki / Tempo              |
| **v1.0.0-RC2** | Advanced QL features + deferred API surface                            | Subqueries, native-histogram quantiles, structural-chain TraceQL, LogQL `\| unpack`, Loki `tail`… |
| **v1.0.0-RC3** | Optimizer rewrite + performance + scalability + advanced testing       | Pattern-based rules, MV substitution, streaming cursor, shadow-mode differential testing          |
| **v1.0.0-RC4** | Full self-observability                                                | Cerberus emits its own structured logs (slog), OTel metrics + traces, defaults to the same CH     |
| **v1.0.0-RC5** | 12-factor compatibility + scale-out polish                             | `/readyz` pings CH, admission control, HPA recipe, dev `docker-compose.yml`, schema overrides     |
| **v1.0.0-RC6** | Type-safe SQL via custom `internal/chsql.Builder`                      | No `fmt.Sprintf`-on-SQL anywhere; typed builder with CH-specific helpers; closed Frag surface     |
| **v1.0.0-RC7** | `internal/engine/` ExecutionEngine framework                           | One pipeline owner; handlers under ~150 LoC each; shared format + httperr helpers                 |
| **v1.0.0-RC8** | chDB-backed semantic test layer                                        | TXTAR fixtures opt into in-process chDB execution; optimizer property tests; mutation kill lane   |
| **v1.0.0**     | Tag the last green RC                                                  | All RCs stable; HTTP wire protocols are the public surface, not a Go API                          |

---

## RC1 — full QL + 90% API shipped at `v1.0.0-RC1`

Closed milestones M0 (seed + compatibility harness scaffold), M1 (PromQL → 90% via `prometheus/compliance`), M2 (Prom HTTP API completion), M3 (LogQL slice + Loki HTTP API), M4 (TraceQL slice + Tempo HTTP API), M5 (`v1.0.0-RC1` tag + `docs/compatibility.md`). See `CHANGELOG.md` § `[v1.0.0-RC1]` and `git log v1.0.0-RC1` for the per-PR breakdown.

The **3-rule optimizer** (filter-fusion, constant-fold, projection-pushdown) ships unchanged through RC1 and RC2. No new optimizer work happens before RC3 — its full backlog lives in [`docs/optimizer-research.md`](optimizer-research.md).

---

## RC2 — advanced QL + API features shipped at `v1.0.0-RC2`

Closed the advanced-QL + deferred-API backlog plus the schema-source-of-truth migration. See `CHANGELOG.md` § `[v1.0.0-RC2]` for the full ~71-PR entry and the GitHub Project's RC2 column for per-item PR refs.

Highlights:

- **PromQL** — subqueries (P0 4.1–4.11) including nested subqueries through Call / ParenExpr / AggregateExpr intermediaries (`max_over_time(rate(m[5m])[10m:1m])[1h:5m]`, `min_over_time(avg_over_time(max_over_time(rate(m[1m])[5m:30s])[1h:5m])[2h:10m])`), `predict_linear` / `holt_winters` / `@start()` / `@end()`, `histogram_quantile` over classic + native (exp) histograms, `group_left` / `group_right` cardinality edges. Subquery-over-aggregator with `without(...)` or parameterised aggregates (`quantile`, `topk`, `bottomk`, `count_values`) remains unsupported.
- **LogQL** — `| unpack`, `| pattern`, `| line_format`, `| decolorize`, `| label_format` (with Loki template funcs), `| drop` / `| keep` (post-fetch label projection via upstream `log.DropLabels` / `log.KeepLabels`), `bytes_*` alignment, `/api/v1/tail` WebSocket (bounded send buffer + `ctx.Done()` drop), `/labels`, `/label/.../values`, `/series`, `/detected_fields`, `/patterns`, `/index/stats`, `/index/volume`.
- **TraceQL** — `status = error` / `kind = client` enum statics, `sum / avg / max / min` over inner attributes, link traversal + span-event queries, set ops, `group / coalesce` pipeline elements, `histogram_over_time`, MetricsPipeline lowering, multi-hop + recursive `>>` / `<<` chains via CH `WITH RECURSIVE` CTEs.
- **Tempo HTTP API** — `/api/search/recent`, `/api/search/tags`, `/api/search/tag/<n>/values`, `/api/metrics/query_range`.
- **Self-contained k3s deployment** — `test/e2e/k3s/otel-collector.yaml` (per-node DaemonSet + gateway Deployment wired to the CH exporter) plus `test/e2e/k3s/sample-app.yaml` (telemetrygen). E2E now reads real OTel data through Grafana.
- **Tempo fork wired** — `unsafe.Pointer` + `reflect.FieldByName` shims retired against `tsouza/tempo:cerberus-accessors`; `forbidigo` gates regressions. See [`docs/upstream-forks.md`](upstream-forks.md).
- **Schema source-of-truth migration** — OTel-CH exporter schema is now the source via `tsouza/opentelemetry-collector-contrib:cerberus-ddl`; `internal/schema/ddl/` consumes the upstream `sqltemplates` API; auto-create startup hook + e2e + compatibility seeders migrated.

---

## RC3 — optimizer + performance + advanced testing

All of [`docs/optimizer-research.md`](optimizer-research.md) lands here. The reading list is the contract. Every R3.x milestone listed below shipped; remaining pre-GA work for the optimizer is captured in [§ Path to GA — post-RC8 correctness + resilience pass](#path-to-ga--post-rc8-correctness--resilience-pass) and [§ Compatibility lane progress](#compatibility-lane-progress).

### Optimizer rewrite

| #    | Item                                                        | Status           | Primary reference                                                                                                                                          |
| ---- | ----------------------------------------------------------- | ---------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| R3.1 | Pattern-based `Rule` API (Calcite-style match / transform)  | shipped via #135 | [Apache Calcite `org.apache.calcite.rel.rules`](https://calcite.apache.org/javadocAggregate/)                                                              |
| R3.2 | `FilterProjectTranspose` + `FilterAggregateTranspose` rules | shipped via #177 | Same                                                                                                                                                       |
| R3.3 | Catalyst-style `Batch` grouping                             | shipped via #191 | [Spark `Optimizer.scala`](https://github.com/apache/spark/blob/master/sql/catalyst/src/main/scala/org/apache/spark/sql/catalyst/optimizer/Optimizer.scala) |
| R3.4 | Sort-key-aware filter emission + `PREWHERE` promotion       | shipped via #196 | [ClickHouse query-optimization guide](https://clickhouse.com/resources/engineering/clickhouse-query-optimisation-definitive-guide)                         |
| R3.5 | Analyzer vs Optimizer rule split                            | shipped via #194 | [DataFusion optimizer crate](https://docs.rs/datafusion-optimizer/latest/datafusion_optimizer/)                                                            |

### Performance features

R3.4 / R3.6 / R3.7 / R3.8 are the **CH-roundtrip scalability levers** — they shrink the amount of data CH scans and ships per query, which is where the real wins live. R3.12 is the **process-side** scalability lever — it caps RAM growth on large result sets. PREWHERE promotion (R3.4) plus streaming cursor (R3.12) means a 1M-point query both narrows the scan and never materialises the full result in cerberus RAM.

**No query/plan/SQL caching.** Cerberus is a thin query gateway — caching results or plans turns it into a memoization layer, which (a) hides freshness bugs behind stale results, (b) shifts the correctness burden from CH to cerberus, and (c) duplicates work Grafana / Prometheus / Loki already do client-side. If a deployment wants caching, put it in front of cerberus (e.g. a Grafana datasource cache) — not inside.

| #     | Item                                                                                  | Status           | Primary reference                                                                                                                         |
| ----- | ------------------------------------------------------------------------------------- | ---------------- | ----------------------------------------------------------------------------------------------------------------------------------------- |
| R3.6  | Materialised-view substitution for `otel_metrics_*` rollups (cost-model trigger)      | shipped via #201 | [Promscale #152](https://github.com/timescale/promscale/issues/152) + [Jindal VLDB 2018](http://www.vldb.org/pvldb/vol11/p800-jindal.pdf) |
| R3.7  | Late materialisation for wide-column scans (logs `Body`, `ResourceAttributes`)        | shipped via #195 | [Selective Late Materialization, VLDB 2025](http://people.iiis.tsinghua.edu.cn/~huanchen/publications/slm-vldb25.pdf)                     |
| R3.8  | Filter-RangeWindow transpose                                                          | shipped via #192 | [VictoriaMetrics `metricsql/optimizer.go`](https://github.com/VictoriaMetrics/metricsql/blob/master/optimizer.go)                         |
| R3.12 | Streaming `query_range` matrix response cursor (`chclient.Cursor` over `Sample` rows) | shipped via #175 | Stops handlers from materialising the full matrix; 1M-point query memory drops from O(N) to O(chunk_size). Composes with R3.4.            |

### Advanced testing

| #     | Item                                                              | Status           | Primary reference                                                                                                                                  |
| ----- | ----------------------------------------------------------------- | ---------------- | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| R3.9  | Shadow-mode differential testing (prefer / force-native / oracle) | shipped via #136 | [promshim-clickhouse `harness/prometheus-compliance/`](https://github.com/BadLiveware/promshim-clickhouse/tree/main/harness/prometheus-compliance) |
| R3.10 | Port promshim's local Go evaluator                                | shipped via #134 | Same — `internal/promshim/local/`                                                                                                                  |
| R3.11 | Fuzz + chaos + perf-benchmark CI                                  | shipped via #133 | `go-fuzz`, custom chaos harness, perf-benchmark workflow                                                                                           |

**Exit criterion:** golden-fixture SQL shrinks on real plans; `internal/optimizer` mutation score ≥ 70%; MV substitution active; shadow-mode reveals < 5% native-SQL gap; `chclient.Cursor` streams a 1M-row fixture with bounded RSS.

---

## RC4 — full self-observability

Cerberus instruments itself with the Go-ecosystem defacto stack and ships telemetry into the same OTel-CH schema it queries. Eats its own dogfood: a cerberus self-dashboard in Grafana queries cerberus, which queries CH, which holds cerberus's own metrics + logs + traces.

### Stack

| Signal  | Library                                                         | Why                                                                                               |
| ------- | --------------------------------------------------------------- | ------------------------------------------------------------------------------------------------- |
| Logging | stdlib `log/slog` (Go 1.21+)                                    | Already wired in `cmd/cerberus/main.go`. The defacto for new Go projects.                         |
| Metrics | OpenTelemetry SDK (`go.opentelemetry.io/otel/sdk/metric`)       | OTLP-native → OTel Collector → CH exporter → same `otel_metrics_*` tables cerberus queries.       |
| Traces  | OpenTelemetry SDK + `contrib/instrumentation/net/http/otelhttp` | Auto HTTP spans + manual pipeline spans (parse → lower → optimize → emit → execute).              |

### Milestones

| #    | Item                                                                                                                                                                                                |
| ---- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| R4.1 | Logging quality pass: consistent slog fields (`req_id`, `ql`, `query`, `sql_len`, `duration_ms`, `error_kind`), text+json formats, level env                                                        |
| R4.2 | `otelhttp.NewHandler` wraps the Prom/Loki/Tempo handlers; each request gets a span                                                                                                                  |
| R4.3 | Custom spans around `promql.Lower` / `logql.Lower` / `traceql.Lower` / `optimizer.Default().Run` / `chsql.Emit` / `chclient.Query`                                                                  |
| R4.4 | Self-metrics: request count + latency histogram by route + status; CH roundtrip count + duration; plan IR node count; `cerberus_http_requests_in_flight` gauge per route (HPA-consumable, see R5.7) |
| R4.5 | OTLP exporters: `CERBERUS_OTEL_ENDPOINT` / `_INSECURE` / `_SAMPLER` / `_SERVICE_NAME`; graceful no-op when endpoint unreachable                                                                     |
| R4.6 | Wire cerberus's OTLP export into `test/e2e/k3s/otel-collector.yaml` (landed in RC2) + provisioned `test/e2e/grafana/dashboards/cerberus-self.json`                                                  |
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
| R5.7 | Horizontal scale recipe: example `test/e2e/k3s/cerberus-hpa.yaml` driven by the R4.4 self-metrics (`cerberus_http_requests_in_flight`), with a short `docs/12factor.md` § on the scale-out story                                                  |

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
| R6.8  | Audit `internal/api/loki/` and `internal/api/tempo/` SQL helpers and port any remaining Sprintf-on-SQL callsites. Most loki + tempo helpers were already builder-aware by RC2 cut (#141, #150) — R6.8 is the cleanup sweep.                                                                                                                            | shipped          |
| R6.9  | Lint enforcement: a `cmd/check-sql/` Go tool wired into `just check-sql` (and the `lint` CI job) that scans `internal/chsql/`, `internal/api/`, `internal/optimizer/` for `fmt.Sprintf` calls whose first arg contains `SELECT`, `WHERE`, `FROM`, `INSERT`, etc., plus the `Builder.WriteSQL("...keyword...")` cosplay shape. Fails CI on regressions. | shipped via #184 |
| R6.10 | CLAUDE.md hard-rule promotion: `**No raw SQL strings.**` becomes a top-level non-negotiable; `docs/sql-style.md` documents the helper API and its escape hatches (when `chsql.Raw()` is acceptable, when extending `chsql.QueryBuilder` is preferable).                                                                                                | shipped via #193 |
| R6.11 | Typed Frag constructors + port `internal/api/{loki,tempo,prom}` to the typed surface (R6.11a #187, R6.11b #190, R6.11c #188, R6.11d #189). R6.11e closes the milestone: renames `Builder.WriteSQL` → unexported `writeSQL`, deletes `cmd/check-sql/`, retires the Sprintf scanner. Typed API is now the only public emission surface.                  | shipped          |
| R6.12 | Retire the `chsql.Raw` / `chsql.Concat` escape hatches by walking every callsite and porting it to a typed constructor; close the public chsql surface so opaque-SQL bytes never enter Frag composition. Adds the missing typed constructors (`BareIdent`, `InlineLit`, `Array`, `Subscript`, `If`, `Lambda1`, `Subquery`, `PreRenderedSQL`).          | shipped          |

### Notes for the custom path

Path **(b)** owns its quoting, args lifecycle, and performance characteristics by design — there is no third-party API to bridge. Concrete commitments:

- **Identifier quoting** stays on the existing `quoteIdentCH` / `writeIdent` helpers, lifted into the builder as a private method. CH backtick rules + backtick-doubling for embedded backticks are preserved.
- **Args lifecycle**: the builder owns a `[]any` slice and renders `?` placeholders in the order args are appended. Nested `QueryBuilder` instances flatten their args into the parent on render. R6.1 unit tests verify ordering against a worst-case nested subquery (windowed-array RangeWindow).
- **Performance**: builder construction is allocation-heavier than the current `strings.Builder` direct writes. Bench against a 1000-iteration emit loop on the largest fixture as part of R6.5 (RangeWindow port). Record the number so RC3's optimizer benchmarks stay honest.

### RC6 exit criteria

- `grep -RIn 'fmt.Sprintf' internal/chsql/ internal/api/ | grep -E 'SELECT|FROM|WHERE|INSERT'` returns empty (was machine-enforced via the R6.9 lint gate; retired in R6.11e once `Builder.WriteSQL` became unexported and the typed Frag surface made the scanner redundant — reviewer discipline + the typed API are the going-forward enforcement).
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

## RC8 — chDB-backed semantic test layer

The TXTAR text-equality suite catches every change in the emitted SQL but it doesn't catch _semantic_ regressions where the SQL still parses and its result set silently flips. RC8 closes that gap by wiring [chDB](https://github.com/chdb-io/chdb-go) (in-process ClickHouse) into the test layers that benefit from real query execution against deterministic seeds.

| #    | Item                                                                                                                 | Status           |
| ---- | -------------------------------------------------------------------------------------------------------------------- | ---------------- |
| R8.0 | Driver probe: validate `Map(String, String)` scan via `database/sql` + document chdb-go v1.11.0 quirks               | shipped via #221 |
| R8.1 | TXTAR `-- seed --` / `-- expected_rows --` sections + build-tagged `chdb` runner                                     | shipped via #223 |
| R8.2 | chDB-backed `Querier` for handler tests (replaces hand-rolled stubs across `internal/api/{prom,loki,tempo}`)         | shipped via #229 |
| R8.3 | Property test on optimizer rules (`internal/optimizer/property_test.go`) + mutation kill criterion using chDB        | shipped via #233 |

**Exit criterion:** every TXTAR fixture that opts in renders byte-stable SQL **and** the row set it produces against a chDB session matches the pinned `expected_rows`; the optimizer property test runs 100 random plans per CI and finds zero divergence between unoptimized + optimized output; `just mutate-chdb` produces a strictly higher kill score than `just mutate` (semantically equivalent mutants are correctly not killed).

### Post-RC8 test-deepening initiative

After the RC8 cut, the suite was deepened in two waves to ~1000+ additional tests across 12 layers, plus the oracle property framework scaffolding and the gremlins phased rollout. The full layer map (1: parser smoke / 2a: chplan IR snapshots / 2b: lowering edges / 3: chplan IR invariants / 4: optimizer properties / 5: chsql Frag goldens / 6a–c: chDB roundtrip per QL / 7: HTTP conformance / 8: system lifecycle / 9: differential shadow harness / 10: Playwright UX / 11: chaos + goleak / 12: perf benchmarks + alloc regressions), CI gates, gremlins phase table, and oracle property phases all live in [`docs/test-strategy.md`](test-strategy.md). The roadmap continues to track RC-level feature milestones; test-strategy is the standing reference for layer-by-layer coverage.

### Path to GA — post-RC8 correctness + resilience pass

With the test pyramid in place, the from-scratch PromQL oracle (#272, Phase 1 PR 2) surfaced a wave of correctness gaps the byte-equality TXTAR fixtures had been blind to. Each gap landed as its own PR with a property regression seed pinned in `test/property/testdata/rapid/`. The work tightened RC1 / RC2 PromQL coverage, added the missing resilience layers (circuit breaker, goleak), and converted the test suite to a hard "no skip" discipline so RC8 mutation work doesn't paper over silently-disabled tests.

**PromQL correctness — oracle-surfaced gaps (RC1 / RC2 completion):**

- `rate` / `increase` / `delta` drop series on empty windows (#287).
- `histogram_quantile` over `sum by(le)(rate(...))` aggregation shape (#281).
- Tempo metrics matrix `Value` column cast to `Float64` for `histogram_quantile` correctness (#294).
- Subquery body over aggregate / `BinaryExpr` via recursive inner-expression lowering (#295, #319).
- RangeWindow `Value` alias + sum-on-empty regression seed (#310).
- `bool` modifier on V-V comparisons + empty `without()` degenerate (#311).
- `label_replace` / `label_join` via Map rewrite (#312).
- `absent_over_time` / `stdvar_over_time` / `deriv` / `resets` / `changes` range-window emit (#314).
- `time()` / `vector()` + scalar-only binop fold (#316).
- Date functions `year` / `month` / `day_of_month` / `day_of_week` / `days_in_month` / `hour` / `minute` / `timestamp` (#313).
- `topk` / `bottomk` via LIMIT BY (plus `count_values` via Aggregate reshape) (#318); `without(...)` variant via `MapWithoutKeys` LIMIT BY partition (#327).
- `^` (pow) emitted as `pow()` instead of raw CH `^` (which is XOR) (#320).
- `absent(v)` lowered via `count()`-guarded synthesised Sample (#321).
- `quantile` / `quantile_over_time` fold out-of-range φ to ±Inf (#322).
- ±Inf / NaN literals emitted as CH-portable division forms (#328).

**LogQL / TraceQL correctness:**

- LogQL start/end pushed into lowering, tightened Playwright assertions (#290).
- LogQL wire-wrap column refs match RangeWindow emit shape (#315).
- Loki `/tail` stubQuerier race: mutex-guard the shared parallel-subtest fake (#317).
- TraceQL `toFloat64` cast on numeric literal comparison against Map-typed values (#303).
- TraceQL non-aggregate scalar-filter LHS errors instead of panicking (#324).

**Resilience (Layer 11 chaos):**

- `chclient` circuit breaker for CH-disconnect resilience + chaos sweep (#305).
- Cursor-chaos sweep no longer guarded by `testing.Short` (#306).

**Test discipline — no skip:**

- Misc `t.Skip` purge: chaos × 2, lifecycle, shadow experiment (#302).
- Shadow harness `tc.skipReason` dropped — pass or remove (#307).
- e2e startup-bench redundant `RUN_STARTUP_BENCH` env gate dropped (#308).
- `t.Skip` forbidden in test files via CI gate; branch protection updated (#309).

**Shadow-mode + oracle:**

- Shadow mode wires `promshim/local` oracle + flips defaults (#326).
- Property RangeWindow case-mismatch seed pinned as regression check (#296).

**Optimizer + compatibility:**

- Widen `ProjectionPushdown` + `MVSubstitution`; remove non-commutativity skips (#300).
- Compatibility harness: real CH healthcheck (#297), `cerberus --version` wired into docker healthcheck (#298), upstream tester's current CLI + drop `|| true` mask (#301), POST-form handler + curated queries + jq null-vs-empty (#304).

**Tempo API:**

- `/api/metrics/query_range` handler shipped post-RC2 (#291).

**CI / infrastructure:**

- Quality-metric linters + `go-arch-lint` for coupling enforcement (#323).
- Perf-benchmark methodology: n=6 samples, 1s benchtime, parent-of-main baseline (#325).
- Property workflow: unskip + nightly N=500 (#280), restored `TestPromQL_Property_FromScratch` wiring (#284), rapid flags after package list (#285).
- Mutation workflow: trailing `/...` stripped from per-phase scope paths (#283).
- Playwright seed + handler fixes from L10 PR #261 nightly failures (#286).

**Fixture audits (Layer 6 chDB roundtrip rigor):**

- 38 empty-`expected_rows` fixtures audited (#288).
- `normalizeValue` coerces all integer widths (#289).
- 32 tautologically-green PromQL fixtures eliminated (#293).
- 17 LogQL + TraceQL chDB roundtrip lanes audited against silent fails (#299).
- TraceQL multi-attribute `by()` chDB roundtrip fixtures (#282).

**Coverage baseline:** `docs/coverage.md` published as the GA-prep coverage baseline with per-package thresholds (#292).

**GA readiness:** with these PRs merged the per-RC feature columns of the project board are fully drained. Pre-GA work continues on the compatibility lane (see [§ Compatibility lane progress](#compatibility-lane-progress)) and the Loki + Tempo compliance harness scaffolding (Phase 1 vendor + adoption plans; see the same section). [§ GA exit criteria](#ga-exit-criteria) lists every gate that must be green for `v1.0.0`.

---

## Compatibility lane progress

The `prometheus/compliance` differential against reference Prometheus has been the GA truth-source since RC1. Once the PromLabs demo schema relabel landed (#342) the harness started exercising the full Prom 2.x query corpus against cerberus, surfacing every PromQL gap byte-equality TXTAR fixtures missed. The lane has shed diffs in five waves:

| Run                                                          | Unexpected | Diffs       | What changed                                                                                                                                                                                                                                                                                                |
| ------------------------------------------------------------ | ---------- | ----------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Pre-PromLabs-relabel baseline                                | 0          | 19          | Harness seed mismatched Prom's reference schema (`(instance, job, type)`); most diffs hidden behind matcher misses.                                                                                                                                                                                         |
| Post-PromLabs-relabel (#342)                                 | 12         | 387         | Reseeded with PromLabs `demo_*` labels; full Prom 2.x corpus now exercised against cerberus.                                                                                                                                                                                                                |
| Post-V-V `on(...)` step-aligned join (#348)                  | 0          | 273         | Pool-AT closed the 12 unexpected failures (V-V `on()` cardinality + matrix fan-out alignment).                                                                                                                                                                                                              |
| Post-#350 audit baseline (#355)                              | 12         | 183         | Audit run after #350; isolated 12 V-V `on()` cardinality regressions tracked separately and clustered the 183 diffs into 8 buckets in [`docs/compat-residual-audit-25898791664.md`](compat-residual-audit-25898791664.md).                                                                                  |
| Post 8-bucket sweep (#358–#366; see audit doc)               | 0          | 46          | All 8 buckets addressed: `__name__` strip (#359), `(start, end]` window (#358), `extrapolatedRate` correction (#366), `offset` + `label_replace` missing-src (#360), `time()` per-step (#362), `absent_over_time` synth labels (#361), `topk`/`bottomk` per-step (#363), `@<ts>` collapse (#364).           |
| Pool-BG residual sweep (in flight)                           | 0          | ≤ 5 target  | Final cleanup pass on the long tail of leverage-3 diffs after the audit re-ranks against the post-#366 corpus.                                                                                                                                                                                              |

**Target for GA:** 0 unexpected failures + ≤ 5 expected diffs, each with an entry in `compatibility/prometheus/expected-failures.json` carrying an upstream rationale (per the CLAUDE.md allowlist-hygiene rule).

**Loki + Tempo compliance harness scaffolding (Phase 1 in flight):**

- **Tempo** — `compatibility/tempo/upstream/` snapshot of `cmd/tempo-vulture` + `pkg/httpclient` landed via #367 (PR 1 of 4 per [`docs/tempo-compliance-plan.md`](tempo-compliance-plan.md)). Driver + Compose stack + CI follow in PRs 2-4.
- **Loki** — adoption plan [`docs/loki-compliance-plan.md`](loki-compliance-plan.md) shipped in #332; vendor snapshot of `grafana/loki:pkg/logql/bench/` + `TestRemoteStorageEquality` (PR 1 of 6) in flight as Pool-CA.

Both harnesses are explicitly **not** GA blockers — the scaffolding is the GA gate, not pass-rates against them. Cerberus already passes ~1500 lines of LogQL and ~1100 lines of TraceQL TXTAR fixtures (Layers 1, 2a, 6b, 6c, 7) plus property tests with from-scratch oracles (#330, #331). The Loki/Tempo harnesses are truth-sources that mirror the role `prometheus/compliance` plays for PromQL.

---

## GA exit criteria

`v1.0.0` is tagged from the last green main after **every** gate below is satisfied. Each row is observable from CI or a checked-in file — no human-attested sub-criteria.

| Gate                                                                       | State at this roadmap revision                                                                                                                                                                                                                                                                              |
| -------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Required PR checks green** (`check` + `lint` + `forbid-skip`)            | Green on `main`; required-status-check list enforced via branch protection.                                                                                                                                                                                                                                 |
| **Compatibility suite: 0 unexpected failures**                             | **MET** post-#348 (was 12 in earlier baselines).                                                                                                                                                                                                                                                            |
| **Compatibility suite: ≤ 5 expected diffs, each with rationale**           | In flight — current run shows 46 diffs after the 8-bucket sweep; Pool-BG residual sweep targets ≤ 5. Each remaining diff must land in `compatibility/prometheus/expected-failures.json` with an upstream rationale comment.                                                                                 |
| **All RC1–RC8 feature milestones shipped**                                 | **MET** — every R{1..8}.x row in this roadmap is `shipped` / `shipped via #N`.                                                                                                                                                                                                                              |
| **chDB roundtrip lanes (Layers 6a / 6b / 6c) green nightly**               | **MET** — PromQL (63 fixtures), LogQL (39 fixtures), TraceQL (61 fixtures) all opt-in via `-- seed --` / `-- expected_rows --`; nightly `chdb` workflow tracks.                                                                                                                                             |
| **Oracle property tests pass at `N=500`**                                  | **MET** — PromQL (#272/#284), LogQL (#331), TraceQL (#330) all wired into `property.yml`'s nightly run.                                                                                                                                                                                                     |
| **Shadow-mode lane diff < 5% native-SQL gap**                              | **MET** — wired via #136 (scaffold) + #326 (oracle defaults flipped); `shadow-mode` workflow runs on push-to-main + nightly.                                                                                                                                                                                |
| **Mutation kill scores meet phased thresholds**                            | **MET** — phase 1 (chplan @ 90%), phase 2 (chsql @ 85%), phase 3 (optimizer @ 85%), phase 4 (parsers @ 85%), phase 5 (qlcommon @ 75%). Bars raised by Pool-CJ from the initial onboarding floors (80/75/70/65) once nightly kill rates settled in the 90-100% band.                                         |
| **Loki + Tempo compliance harness scaffolding landed**                     | In flight — Tempo PR 1 of 4 merged (#367); Loki PR 1 of 6 in flight as Pool-CA. Scaffolding = vendor + plan committed; pass-rates not part of the GA cut.                                                                                                                                                   |
| **`go-arch-lint` coupling rules green**                                    | **MET** — wired via #323; `lint` job runs it on every PR.                                                                                                                                                                                                                                                   |
| **Self-observability (RC4): one span per request, self-dashboard renders** | **MET** — RC4 closed (#208 + provisioned dashboards).                                                                                                                                                                                                                                                       |
| **12-factor scale-out: `docker compose up` works, HPA recipe smokes**      | **MET** — RC5 closed (R5.1–R5.7; #218 HPA, #219 admission control, #220 docker-compose).                                                                                                                                                                                                                    |
| **No raw SQL strings in `internal/`**                                      | **MET** — RC6 closed; `chsql.Raw` / `chsql.Concat` retired (#207); `Builder.WriteSQL` is unexported; typed Frag surface is the only public emission API.                                                                                                                                                    |
| **Engine framework owns the shared pipeline**                              | **MET** — RC7 closed (#227 / #228 / #230 + #232 headers); each `internal/api/{prom,loki,tempo}/handler.go` is a thin HTTP shell.                                                                                                                                                                            |

---

## How we work

- **PR-per-change.** Every change ships as its own PR against `main`. Branch protection requires `ci / check` + `ci / lint`, linear history, no force-push.
- **Agent-driven work goes through PRs, not issues.** When the maintainer or an AI assistant is doing the work, the PR description is the source of truth — no shadow issue tracking. The GitHub Project tracks milestone status; backlog narratives live in `docs/*.md`. **External contributors** are welcome to open issues for bug reports, design questions, or feature proposals — issues are enabled.
- **Fixture-first.** A milestone's first PR adds _failing_ TXTAR / compatibility fixtures that capture the contract. Subsequent PRs implement to turn them green. Reviewers can sanity-check intent by reading fixtures before code.
- **Compatibility suite is the source of truth.** If a PromQL feature lands but doesn't move the `prometheus/compliance` pass rate, the PR is incomplete.
- **Allowlist hygiene.** Adding an entry to `compatibility/prometheus/expected-failures.json` requires a comment with the upstream rationale; never empty-string.
