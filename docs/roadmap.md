# Cerberus roadmap — v1.0.0

This document is the public-facing narrative for the path to `v1.0.0`. Status by milestone lives in the [GitHub Project](https://github.com/users/tsouza/projects) — *Cerberus v1.0.0 Roadmap*. Per-PR-level reasoning lives in the PR descriptions themselves; we don't use GitHub Issues.

## At a glance

| Release        | Theme                                                                           | What "done" means                                                                                 |
| -------------- | ------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------- |
| **v1.0.0-RC1** | Full PromQL / LogQL / TraceQL support + 90% upstream API compatibility          | Compatibility corpora pass; Grafana sees cerberus as drop-in for Prom / Loki / Tempo              |
| **v1.0.0-RC2** | Advanced QL features + deferred API surface                                     | Subqueries, native-histogram quantiles, structural-chain TraceQL, LogQL `\| unpack`, Loki `tail`… |
| **v1.0.0-RC3** | Optimizer rewrite + performance + advanced testing                              | Pattern-based rules, MV substitution, shadow-mode differential, fuzz + chaos + perf benchmarks    |
| **v1.0.0-RC4** | Full self-observability                                                         | Cerberus emits its own structured logs (slog), OTel metrics + traces, defaults to the same CH     |
| **v1.0.0-RC5** | 12-factor compatibility + polish                                                | `/readyz`, dev `docker-compose.yml`, env-driven schema overrides, `docs/12factor.md`, fast-start  |
| **v1.0.0-RC6** | Type-safe SQL via go-sqlbuilder (R6.0 evaluation → R6.1–R6.10 port)             | No `fmt.Sprintf`-on-SQL anywhere; typed builder with CH-specific helpers; lint enforcement        |
| **v1.0.0-RC7** | `internal/engine/` ExecutionEngine framework (R7.0 evaluation → R7.1–R7.8 port) | One pipeline owner; handlers under ~150 LoC each; shared format + httperr helpers                 |
| **v1.0.0**     | Tag the last green RC                                                           | All RCs stable; public API frozen in `pkg/`                                                       |

The existing **3-rule optimizer** (filter-fusion, constant-fold, projection-pushdown) ships unchanged through RC1 and RC2. **No new optimizer work happens before RC3** — its full backlog lives in [`docs/optimizer-research.md`](optimizer-research.md).

---

## RC1 — full QL + 90% API

PRs land in milestone order. Within a milestone, the **first PR adds failing TXTAR / compatibility fixtures**; subsequent PRs implement to turn them green. Compatibility suites are merge gates, so coverage regressions are impossible.

### M0 — finish the seed

The remaining items from the original seed plan, plus the compatibility harness scaffold.

| #    | Theme                          | Outcome                                                                                                              |
| ---- | ------------------------------ | -------------------------------------------------------------------------------------------------------------------- |
| M0.1 | k3d deploy + Justfile e2e      | `deploy/k3s/`, `deploy/grafana/`, `just e2e-{up,seed,run,down,playwright}`                                           |
| M0.2 | Playwright smoke + workflow    | `test/e2e/playwright/`, `.github/workflows/e2e.yml` (the `dashboard` job)                                            |
| M0.3 | AI-agent seed                  | `CLAUDE.md`, `AGENTS.md`, three `.claude/skills/`                                                                    |
| M0.4 | Engineering hygiene            | `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SECURITY.md`, `.github/CODEOWNERS`, PR template                            |
| M0.5 | Release plumbing               | `release.yml` + `.goreleaser.yml`; tag `v0.1.0` to validate the path                                                 |
| M0.6 | Compatibility harness scaffold | `harness/compatibility/` with `prometheus/compliance` submodule, Docker Compose, allowlist file, `compatibility.yml` |

**Exit criterion:** `just e2e` green; `v0.1.0` cut; `just compatibility` runs end-to-end and produces a JSON report (failing baseline is expected and fine).

### M1 — PromQL → 90%, TDD-driven by `prometheus/compliance`

| #    | Theme                            | Notes                                                                                                               |
| ---- | -------------------------------- | ------------------------------------------------------------------------------------------------------------------- |
| M1.1 | Real `RangeWindow` SQL emission  | Port [promshim-clickhouse](https://github.com/BadLiveware/promshim-clickhouse)'s windowed-array idiom. **Blocker.** |
| M1.2 | `BinaryExpr` lowering            | Arithmetic, comparison (with `bool` modifier), logical                                                              |
| M1.3 | Instant-vector functions         | `abs`, `ceil`, `floor`, `round`, `sqrt`, `ln`, `log2`, `log10`, `exp`, `scalar()`, `vector()`                       |
| M1.4 | Aggregation completeness         | `without (…)`, parameterised (`topk`, `quantile`, `count_values`), `stddev`, `stdvar`, `group`                      |
| M1.5 | `offset` and `@` modifiers       | Pass through to `RangeWindow.Offset` / `At`                                                                         |
| M1.6 | Vector matching                  | `on(…)`, `ignoring(…)`, `group_left`, `group_right` (cardinality edges defer to RC2)                                |
| M1.7 | Float + empty-result correctness | `NaN`/`±Inf` JSON encoding, empty groups, counter-reset semantics                                                   |

**Exit criterion:** `prometheus/compliance` ≥ 538/539 queries pass; `expected-failures.json` ≤ 2 entries (matching promshim's documented deviations: topk tie-break + float-mod drift).

### M2 — Prom HTTP API completion

| #    | Endpoint / behaviour                                           |
| ---- | -------------------------------------------------------------- |
| M2.1 | Real per-step bucketing in `/api/v1/query_range`               |
| M2.2 | Aggregate result shaping (`grouping.go`)                       |
| M2.3 | `chclient.QueryRaw` + `/api/v1/labels`                         |
| M2.4 | `/api/v1/label/<name>/values`                                  |
| M2.5 | `/api/v1/series`                                               |
| M2.6 | `/api/v1/metadata`                                             |
| M2.7 | Validation polish + `X-Prometheus-API-Version` + debug headers |

**Exit criterion:** Grafana Prom datasource (label picker, value picker, metric hover, `sum by` panels) works end-to-end against cerberus without datasource-specific config.

### M3 — LogQL slice + Loki HTTP API

| #    | Theme                                                                    |
| ---- | ------------------------------------------------------------------------ |
| M3.1 | `schema.Logs` + stream selectors + line filters (`LineContent`)          |
| M3.2 | Label filters + parsers (`JSONExtract`, `LogfmtExtract`)                 |
| M3.3 | Metric form (`rate`, `count_over_time`, `bytes_rate`, `bytes_over_time`) |
| M3.4 | Aggregations (reuses M1.4)                                               |
| M3.5 | Loki HTTP API + derived corpus under `harness/logql-corpus/`             |

**Exit criterion:** Derived LogQL corpus ≥ 90% pass; Grafana Explore log search works end-to-end.

### M4 — TraceQL slice + Tempo HTTP API

| #    | Theme                                                                    |
| ---- | ------------------------------------------------------------------------ |
| M4.1 | `schema.Traces` + span selectors + attribute matchers (`FieldAccess`)    |
| M4.2 | Structural operators `>>` / `>` / `<<` / `<` via `chplan.StructuralJoin` |
| M4.3 | Aggregators (`count`, `sum`, `avg`, `max`, `min`)                        |
| M4.4 | Time filters + `\| select(...)` via `TimeFunc`                           |
| M4.5 | Tempo HTTP API + derived corpus under `harness/traceql-corpus/`          |

**Exit criterion:** Derived TraceQL corpus ≥ 90% pass; Grafana trace search + waterfall both work.

### M5 — RC1 release

| #    | Theme                                                                 |
| ---- | --------------------------------------------------------------------- |
| M5.1 | `CHANGELOG.md` with features + RC2 deferrals                          |
| M5.2 | README drops the seed badge; status block reads "RC1"                 |
| M5.3 | Tag `v1.0.0-RC1`; `release.yml` cuts multi-arch binaries + image      |
| M5.4 | `docs/compatibility.md` documents allowlist + per-QL corpus extension |

---

## RC2 — advanced QL + API features

The remaining ~10% per QL, plus the deferred API endpoints. Each lands as its own PR after RC1 tags.

- **PromQL** — `histogram_quantile` on native histograms; `predict_linear`, `holt_winters`; `@start()`/`@end()`; exemplar attachment; recording-rule inline expansion; `group_left`/`group_right` cardinality enforcement edge cases. Subqueries (`m[1h:5m]`, `max_over_time(rate(m[5m])[1h:5m])`) shipped via P0 4.1–4.11 (RC2) — full plan in [`docs/rc2-p0-4-subqueries.md`](rc2-p0-4-subqueries.md). Subquery over aggregator (`max_over_time(sum by(...) (rate(...))[1h:5m])`) + nested subqueries deferred to RC3.
- **LogQL** — `| unpack`, `| pattern`; advanced `label_format` templating; `bytes_*` precise alignment; `tail` (WebSocket streaming).
- **TraceQL** — `histogram_over_time`; link traversal + span-event queries; root-span filtering in nested conditions. `status = error` / `kind = client` enum statics shipped via P0 6 (RC2). `sum(.attr)` / `avg(.attr)` / `max(.attr)` / `min(.attr)` shipped via P0 7 (RC2) using an `unsafe.Pointer` shim on the Tempo `Aggregate.e` field — the long-term replacement (fork + `Expr()` accessor) is captured in [`docs/upstream-tracking.md`](upstream-tracking.md). Recursive structural chains (`>>` / `<<`) deferred to RC3: needs CH WITH RECURSIVE / bounded-depth UNION SQL that wants CH-integration testing alongside the RC3 optimizer work. Direct parent-child `>` and `<` work today.
- **Self-contained k3s deployment** with `otel-collector` + ClickHouse exporter deferred to RC4 alongside the self-observability work — when cerberus emits its own OTel data it'll round-trip through the same collector pipeline. The synthetic `test/e2e/seed/*.sql` continues to be the canonical fixture surface for spec / unit / E2E determinism.
- **HTTP APIs** — Prom `query_exemplars`, `format_query`, `parse_query`; Loki `tail`, `index/stats`, `index/volume`, `detected_fields`, `patterns`; Tempo `search/recent`, `metrics/query_range`, `search/tags`, `search/tag/<n>/values` (the last two gated on RC6 R6.1 sqlbuilder so the new SQL avoids Sprintf).
- **Self-contained deployment** — replace the synthetic `test/e2e/seed/otel_metrics.sql` with a real OTel pipeline in the k3d stack: OpenTelemetry Collector (contrib image) + ClickHouse exporter that **creates the OTel-CH schema** on startup and collects real k8s telemetry (kubelet/cAdvisor metrics, container logs, collector self-traces, optional sample-app OTLP traces). Cerberus then queries that real data through Grafana for E2E. The TXTAR + synthetic-SQL seeding stays for unit/spec tests where determinism matters more than realism. Unblocks RC4's "cerberus eats its own dogfood" architecture.

**Exit criterion:** the lists above empty out; compatibility pass rate stays ≥ RC1 baseline; `just e2e-up` brings up a stack where data flows from real OTel sources, not synthetic INSERTs.

---

## RC3 — optimizer + performance + advanced testing

All of [`docs/optimizer-research.md`](optimizer-research.md) lands here. The reading list is the contract.

### Optimizer rewrite

| #    | Item                                                        | Primary reference                                                                                                                                          |
| ---- | ----------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| R3.1 | Pattern-based `Rule` API (Calcite-style match / transform)  | [Apache Calcite `org.apache.calcite.rel.rules`](https://calcite.apache.org/javadocAggregate/)                                                              |
| R3.2 | `FilterProjectTranspose` + `FilterAggregateTranspose` rules | Same                                                                                                                                                       |
| R3.3 | Catalyst-style `Batch` grouping                             | [Spark `Optimizer.scala`](https://github.com/apache/spark/blob/master/sql/catalyst/src/main/scala/org/apache/spark/sql/catalyst/optimizer/Optimizer.scala) |
| R3.4 | Sort-key-aware filter emission + `PREWHERE` promotion       | [ClickHouse query-optimization guide](https://clickhouse.com/resources/engineering/clickhouse-query-optimisation-definitive-guide)                         |
| R3.5 | Analyzer vs Optimizer rule split                            | [DataFusion optimizer crate](https://docs.rs/datafusion-optimizer/latest/datafusion_optimizer/)                                                            |

### Performance features

| #    | Item                                                                             | Primary reference                                                                                                                         |
| ---- | -------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------- |
| R3.6 | Materialised-view substitution for `otel_metrics_*` rollups (cost-model trigger) | [Promscale #152](https://github.com/timescale/promscale/issues/152) + [Jindal VLDB 2018](http://www.vldb.org/pvldb/vol11/p800-jindal.pdf) |
| R3.7 | Late materialisation for wide-column scans (logs `Body`, `ResourceAttributes`)   | [Selective Late Materialization, VLDB 2025](http://people.iiis.tsinghua.edu.cn/~huanchen/publications/slm-vldb25.pdf)                     |
| R3.8 | Filter–RangeWindow transpose                                                     | [VictoriaMetrics `metricsql/optimizer.go`](https://github.com/VictoriaMetrics/metricsql/blob/master/optimizer.go)                         |

### Advanced testing

| #     | Item                                                              | Primary reference                                                                                                                  |
| ----- | ----------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------- |
| R3.9  | Shadow-mode differential testing (prefer / force-native / oracle) | [promshim-clickhouse `harness/compatibility/`](https://github.com/BadLiveware/promshim-clickhouse/tree/main/harness/compatibility) |
| R3.10 | Port promshim's local Go evaluator                                | Same — `internal/promshim/local/`                                                                                                  |
| R3.11 | Fuzz + chaos + perf-benchmark CI                                  | `go-fuzz`, custom chaos harness, perf-benchmark workflow                                                                           |

**Exit criterion:** golden-fixture SQL shrinks on real plans; `internal/optimizer` mutation score ≥ 70%; MV substitution active; shadow-mode reveals < 5% native-SQL gap.

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

| #    | Item                                                                                                                                          |
| ---- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| R4.1 | Logging quality pass: consistent slog fields (`req_id`, `ql`, `query`, `sql_len`, `duration_ms`, `error_kind`), text+json formats, level env  |
| R4.2 | `otelhttp.NewHandler` wraps the Prom/Loki/Tempo handlers; each request gets a span                                                            |
| R4.3 | Custom spans around `promql.Lower` / `logql.Lower` / `traceql.Lower` / `optimizer.Default().Run` / `chsql.Emit` / `chclient.Query`            |
| R4.4 | Self-metrics: request count + latency histogram by route + status; CH roundtrip count + duration; plan IR node count                          |
| R4.5 | OTLP exporters: `CERBERUS_OTEL_ENDPOINT` / `_INSECURE` / `_SAMPLER` / `_SERVICE_NAME`; graceful no-op when endpoint unreachable               |
| R4.6 | `deploy/k3s/otel-collector.yaml` + a provisioned `deploy/grafana/dashboards/cerberus-self.json` (cerberus's own metrics rendered by cerberus) |
| R4.7 | `docs/observability.md`                                                                                                                       |

**Exit criterion:** every Prom/Loki/Tempo request emits one span with pipeline stage timings; self-dashboard renders cerberus's own request rate + p99 latency; disabling OTel via `CERBERUS_OTEL_ENDPOINT=""` produces a zero-collector-dependency binary.

---

## RC5 — 12-factor compatibility + polish

Driven by an audit of cerberus against [12factor.net](https://12factor.net/). Most factors pass today; this RC closes the gaps and documents the rest.

### Audit snapshot

| #   | Factor            | Status  | Note                                                                                  |
| --- | ----------------- | ------- | ------------------------------------------------------------------------------------- |
| 1   | Codebase          | PASS    | Single repo + Go module.                                                              |
| 2   | Dependencies      | PASS    | `go.mod` explicit; `replace` directives documented.                                   |
| 3   | Config            | PASS    | Env vars only (`CERBERUS_*`); no flags, no runtime YAML.                              |
| 4   | Backing services  | PASS    | CH swappable via env.                                                                 |
| 5   | Build/release/run | PASS    | goreleaser + Dockerfile + release.yml; immutable artifacts.                           |
| 6   | Processes         | PASS    | Stateless; horizontal scale via Deployment replicas.                                  |
| 7   | Port binding      | PASS    | `:8080` self-binding; env-configurable.                                               |
| 8   | Concurrency       | PASS    | Goroutines + Deployment replicas.                                                     |
| 9   | Disposability     | PASS    | `signal.NotifyContext` + 10s graceful-shutdown deadline.                              |
| 10  | Dev/prod parity   | PARTIAL | Same image runs everywhere; missing a one-command `docker-compose.yml` for local dev. |
| 11  | Logs              | PASS    | `slog` to stderr; no log files; no rotation.                                          |
| 12  | Admin processes   | PASS    | None today; future admin tasks would land as separate one-offs.                       |

### Milestones

| #    | Item                                                                                        |
| ---- | ------------------------------------------------------------------------------------------- |
| R5.1 | `/readyz` distinct from `/healthz`; k8s manifests updated to use readiness vs liveness      |
| R5.2 | Repo-root `docker-compose.yml` for one-command local dev (CH + OTel Collector + cerberus)   |
| R5.3 | Env-driven schema overrides via `CERBERUS_SCHEMA_OVERRIDES_JSON`                            |
| R5.4 | `docs/12factor.md` with file-line citations per factor                                      |
| R5.5 | Startup-speed benchmark: process-start → `/healthz` 200 against a reachable CH; target < 2s |

**Exit criterion:** `docker compose up` at repo root brings the dev stack up in < 30s; `CERBERUS_SCHEMA_OVERRIDES_JSON` honoured; `docs/12factor.md` exists with per-factor evidence; startup benchmark passes < 2s in CI.

---

## RC6 — type-safe SQL via go-sqlbuilder

**Hard rule (non-negotiable, takes effect at RC6 cut):** plain strings and `fmt.Sprintf` are forbidden for ClickHouse SQL generation. Every emitted SQL fragment goes through a typed builder — either [`huandu/go-sqlbuilder`](https://github.com/huandu/go-sqlbuilder) wrapped with a cerberus-internal extension layer, *or* a custom `internal/chsql/builder.go` tailored to chplan IR. The choice is made in R6.0 below. Concatenation and templated identifiers are an injection vector even with parameterised values; the builder tracks identifiers, args, and dialect quoting in one place.

### Why now (and not earlier)

Through RC1 the emitter grew Sprintf-driven for speed: `internal/chsql/emit_node.go`, `emit_expr.go`, `range_window.go`, `vector_join.go`, plus the metadata SQL in `internal/api/prom/metadata.go`. That worked for landing the surface, but every helper is hand-quoted, each subquery is a string-pasted hole, and there's no central ClickHouse dialect handling. RC6 reorganises that into a typed, builder-first emitter so RC3's optimizer changes can compose SQL programmatically (PREWHERE promotion, sort-key-aware predicate ordering, MV substitution) without re-scaffolding the string layer.

### R6.0 — Evaluation phase (prerequisite)

This is a *decision* milestone, not a code milestone. Its single deliverable is a written evaluation in `docs/sql-builder-evaluation.md` with a recommendation on which SQL-construction strategy RC6 will adopt. No emitter code changes here.

**Inputs to evaluate:**

1. **Security analysis (current state).** Inventory every `fmt.Sprintf` / string-concat callsite that produces ClickHouse SQL and classify each by injection-vector risk:
   - Schema-derived identifiers (table + column names): low risk — comes from `internal/schema/` config, not user input. Risk surface is the env override path (`CERBERUS_SCHEMA_OVERRIDES_JSON`, lands in R5.3).
   - Map keys (`Attributes['<key>']`): the key string flows from chplan IR; trace whether the parser/lowering ever surfaces an unfiltered user string into a key position.
   - Regex patterns / literal values: already parameterised via `?` placeholders.
   - Tempo's reflect-driven attribute name extraction (`internal/traceql/select.go`): does the upstream parser bound attribute-name characters?
   - **Output:** a table of callsites × risk-class × current-mitigation, plus a list of any vectors not closed by `?`-placeholders today.

2. **Project impact analysis.**
   - Refactor scope: line count touched by a full port (today: 6 emit files + 3 handlers + `metadata.go` ≈ 2k LoC).
   - Risk surface: subtle SQL semantic changes that pass golden updates but trip CH at runtime (e.g. ordering inside `OR` chains, `PREWHERE` placement, alias quoting).
   - Test coverage protecting the refactor: count of TXTAR fixtures + integration tests as the safety net.
   - Dependency exposure: pulling in `huandu/go-sqlbuilder` adds a new transitive surface; weigh against the current zero-dep emitter.

3. **Benefit analysis.**
   - **Security:** what new vectors does a typed builder close that `?` placeholders don't? Honest answer is likely "few" for cerberus today — the upstream parsers already constrain inputs. Defense-in-depth is real but incremental.
   - **Architecture:** type-safe composition unlocks RC3's optimizer rules (PREWHERE promotion, sort-key reordering, MV substitution, late materialisation). This is the *primary* motivation, not security. Note: RC3 ships **before** RC6 chronologically, so the RC3 emitter work either takes a Sprintf-tax or RC6 jumps the queue. R6.0 evaluates whether to reorder.
   - **Maintainability:** removes hand-quoting bugs (CH backtick rules, CH lambda syntax, parametric aggregates).
   - **Testability:** builder-produced SQL is easier to introspect (per-fragment) than Sprintf-built strings.

4. **Build vs buy decision matrix.** For each axis, score third-party (`huandu/go-sqlbuilder`) vs custom (`internal/chsql.Builder`):

   | Axis                         | Third-party               | Custom                    | Notes                                                                                                                                                     |
   | ---------------------------- | ------------------------- | ------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------- |
   | CH idiom coverage out-of-box | partial                   | full                      | Both need MapAt/MapKeys/Lambda/ParamAgg/PREWHERE helpers; third-party also requires bridging its API to those — ~30–40% of the value is custom either way |
   | Upstream maintenance         | shared                    | ours                      | `huandu/go-sqlbuilder` is actively maintained but small; if it stalls we fork                                                                             |
   | Onboarding                   | docs exist                | we write docs             | Third-party has a larger surface to learn                                                                                                                 |
   | API match to chplan IR       | impedance                 | natural                   | Custom builder can be designed *around* chplan node shapes                                                                                                |
   | Code volume                  | smaller core, larger glue | larger core, smaller glue | Net LoC is similar                                                                                                                                        |
   | Security guarantees          | type system encodes       | we encode them            | Equivalent if we're disciplined                                                                                                                           |

5. **Decision rule.** The evaluation produces ONE of three recommendations:
   - **(a) Use `huandu/go-sqlbuilder` + cerberus extension layer.** Justified if the wrapping cost is materially less than building from scratch and the upstream is healthy enough to depend on.
   - **(b) Build `internal/chsql.Builder` from scratch.** Justified if the impedance mismatch with chplan IR is large enough that wrapping the third-party is *more* work than building tailored, OR if the security-critical surface motivates having a single owned-and-audited builder.
   - **(c) Defer the migration entirely.** Justified only if the security surface analysis (step 1) shows the current parameterised-emitter has zero open vectors AND the optimizer rules in RC3 can ship without typed SQL composition (unlikely but the eval should test the assumption).

**Preliminary read** (for the eval to refute or confirm):

- The injection surface today is narrow — every dynamic value already flows through `?` placeholders; identifier dynamism is bounded to schema config. Security alone wouldn't justify the migration.
- The architectural motivation is real: RC3's optimizer rules need to compose SQL fragments, and Sprintf composition collapses under the weight (PREWHERE clauses, conditional WHERE chains, MV-substituted subtrees).
- ~30–40% of the value (CH-specific helpers — MapAt, MapKeys, Lambda, ParamAgg, PREWHERE) is custom regardless of which library backs it.
- Custom builder *may* be the lower-effort route precisely because chplan IR is well-defined and stable; wrapping a generic builder adds an impedance layer without removing the custom layer.
- If recommendation is **(b) custom**, the API surface should mirror chplan node shapes one-to-one (Scan → ScanBuilder, Filter → FilterBuilder, etc.) so the emitter is a structural transformation, not an interpretation.

**Exit criterion for R6.0:** `docs/sql-builder-evaluation.md` exists, recommends (a) / (b) / (c), and the maintainer (Thiago) has signed off on the choice. R6.1 — the first implementation milestone — is then concretely scoped against that choice (currently written assuming **(a)**; rewrite if **(b)** is chosen).

### Library survey

[`huandu/go-sqlbuilder`](https://github.com/huandu/go-sqlbuilder) covers the dialects we care about: `sqlbuilder.ClickHouse` flavor, `Cond` builders for WHERE/JOIN, `BuilderAs` for nested subqueries, `Args.Add` for placeholder management, `Raw`/`Var` for escape hatches. Subqueries compose by passing builders as `From()` / `Join()` arguments — the engine collects placeholders across the tree.

What `go-sqlbuilder` doesn't model out of the box (the cerberus extension layer):

- **Map column access** — `Attributes['job']`, `mapKeys(Attributes)`, `mapFilter((k,v) -> ..., Attributes)`. Wrap as named helpers (`chsql.MapAt(col, key)`, `chsql.MapKeys(col)`, etc.) returning expression strings via the builder's `Args` API.
- **Array idiom for RangeWindow** — `groupArray((ts, value))`, `arraySort`, `arrayPopBack`, `arrayPopFront`, `arrayMap((p, c) -> ..., a, b)`, `arrayFilter`. Same pattern: typed helpers that compose builder fragments.
- **Parameterised aggregates** — `quantile(0.95)(value)`, `quantiles(0.5, 0.9)(value)`. Builder doesn't know the params/args distinction, so a `chsql.ParamAgg(name, params, args)` helper renders it.
- **DateTime64 literals + interval arithmetic** — `now64(9) - toIntervalNanosecond(N)`, `toDateTime64('2026-...', 9)`. Helpers `chsql.Now64()`, `chsql.SubtractNanos(expr, ns)`, `chsql.DateTime64Lit(t)`.
- **CH lambda syntax** — `(k, v) -> NOT (k IN ('a', 'b'))`. A `chsql.Lambda(params, body)` helper avoids the freestyle string trap.
- **PREWHERE clause** (RC3 sort-key emission) — go-sqlbuilder doesn't natively model PREWHERE; add a `chsql.SelectBuilder` wrapper that supports `.Prewhere(cond...)`.

Custom Flavor extension (`sqlbuilder.NewFlavor` style) might also clean up some quoting if upstream `ClickHouse` flavor's identifier escaping doesn't match our needs (CH backticks vs PG double quotes — needs a small audit).

### Refactor strategy

Fixture-first: every TXTAR golden in `test/spec/{promql,logql,traceql,chsql}/` is the safety net. The refactor is allowed to change SQL formatting (whitespace, alias placement) — fixtures get regenerated on the same commit. Behavioural changes (different operators, predicate order) trigger a fixture diff that should be human-reviewed.

PR sequence:

| #     | Item                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| ----- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| R6.1  | Scaffolding per R6.0 decision: vendor `huandu/go-sqlbuilder` + add `internal/chsql/builder.go` as the CH-flavor wrapper (path **(a)**), *or* build `internal/chsql/builder.go` from scratch (path **(b)**). Either path provides the same helpers — MapAt, MapKeys, MapFilterExcept, Now64, SubtractNanos, DateTime64Lit, Lambda, ParamAgg, and a `SelectBuilder` that supports `.Prewhere(cond...)`. Unit tests pin each helper's output. **No emitter changes yet** — pure scaffolding. |
| R6.2  | Port `emitScan`, `emitFilter`, `emitProject`, `emitLimit` (the simple ones). TXTAR fixtures regenerate; diff is whitespace / quoting only.                                                                                                                                                                                                                                                                                                                                                |
| R6.3  | Port `emitAggregate` + `emitAggFunc` (parameterised aggregates use `ParamAgg`).                                                                                                                                                                                                                                                                                                                                                                                                           |
| R6.4  | Port `emitBinary` + `emitMapAccess` + `emitMapWithoutKeys` + `emitFunc` + `emitLineContent`.                                                                                                                                                                                                                                                                                                                                                                                              |
| R6.5  | Port `emitRangeWindow` (the `arraySort/groupArray` windowed-array idiom). The existing emitter is a 100-line Sprintf chain; this is the highest-value port.                                                                                                                                                                                                                                                                                                                               |
| R6.6  | Port `emitVectorJoin` (per-series argMax + INNER JOIN).                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| R6.7  | Port `internal/api/prom/metadata.go` UNION builders + `unionLabelValuesSQL` + `metricMetaSQL`.                                                                                                                                                                                                                                                                                                                                                                                            |
| R6.8  | Port `internal/api/loki/` and `internal/api/tempo/` SQL helpers (RC2/RC3 may have grown new ones).                                                                                                                                                                                                                                                                                                                                                                                        |
| R6.9  | **Lint enforcement**: add a custom golangci-lint rule (or a `cmd/check-sql/` Go tool wired into `just check-sql`) that scans `internal/chsql/`, `internal/api/`, `internal/optimizer/` for `fmt.Sprintf` calls whose first arg contains `SELECT`, `WHERE`, `FROM`, `INSERT`, etc. Fails the `lint` CI check on regressions.                                                                                                                                                               |
| R6.10 | `CLAUDE.md` hard-rule update: `**No raw SQL strings.**` becomes a top-level non-negotiable; `docs/sql-style.md` documents the helper API and its escape hatches (when `Raw()` is acceptable, when a custom `chsql.SelectBuilder` extension is preferable).                                                                                                                                                                                                                                |

### Open questions to resolve in R6.1 (path **(a)** — third-party)

These apply only if R6.0 picks the third-party route. Path **(b)** custom answers them by design (we own the quoting, args lifecycle, and performance characteristics).

- **Identifier quoting**: does `sqlbuilder.ClickHouse` flavor backtick-quote identifiers? If not, we keep our `quoteIdentCH` helper but feed it through the builder's `Var()` mechanism so escaping stays consistent.
- **`Args` lifecycle**: each builder has its own `Args` accumulator. Nested builders compose via `BuilderAs`; the placeholder positions get re-numbered. Verify with a worst-case nested subquery (e.g. the windowed-array RangeWindow) that the resulting `Build()` output matches what the chclient driver expects.
- **Performance**: builder construction is allocation-heavier than Sprintf. Bench against a 1000-iteration emit loop on the largest fixture; expect a small regression that's irrelevant compared to CH query latency, but record the number so RC3's optimizer benchmarks stay honest.

### RC6 exit criteria

- `grep -RIn 'fmt.Sprintf' internal/chsql/ internal/api/ | grep -E 'SELECT|FROM|WHERE|INSERT'` returns empty (the lint rule from R6.9 enforces this in CI).
- Every TXTAR fixture either matches its prior golden bit-for-bit or has a documented whitespace/aliasing diff in the porting PR's description.
- `internal/chsql/builder_test.go` has ≥ one test per helper in `internal/chsql/builder.go`, exercising the placeholder + arg ordering.
- `docs/sql-style.md` published with the helper catalog and the "when can I use Raw()?" guidance.

---

## RC7 — `internal/engine/` ExecutionEngine framework

By RC1's end, every Prom / Loki / Tempo handler runs the same five-stage pipeline — parse → lower → wrap-projection → optimize → emit → query → format — with per-QL adapters at each stage. The repetition is mechanical (5 callsites today, all with subtle drift between them) and grows by one with every new HTTP route. RC7 lifts the common loop into `internal/engine/` so handlers become thin HTTP shells over a `engine.Query(ctx, lang, q)` call. Like RC6, it starts with an evaluation phase: the framework is only worth building if the audit shows the pattern actually generalises.

### Why now (chronologically after RC6)

- The framework is most valuable *after* RC3's optimizer changes and RC6's typed SQL land — those work products would otherwise have to be duplicated by-handler. By RC7 the shared pipeline has stable shape.
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
   - **Pros:** one instrumentation point (RC4 simplification); one strategy switch (RC3 shadow-mode / fallback hooks); one place to add caching / rate limiting / hedging in future RCs; handlers become testable without stubbing the whole pipeline.
   - **Cons:** abstraction tax — readers chase one extra layer; LoC churn ~1k+ during the port; risk of designing the wrong seam if the per-QL drift turns out to be semantic, not mechanical.
   - **The audit's job:** prove the pattern generalises. If 3+ of the 5 callsites diverge in ways the abstraction can't absorb cleanly, recommend **defer**.

5. **Decision rule.** R7.0 produces one of three recommendations:
   - **(a) Build.** Audit shows the pipeline shape is mechanical across handlers; per-QL drift fits into a `Lang` adapter without ad-hoc escape hatches.
   - **(b) Partial.** Build only the shared-helpers extraction (`internal/api/format/` + `internal/api/httperr/`) without the Engine struct itself. Cheaper, captures most of the duplication, no abstraction tax.
   - **(c) Defer.** Per-QL divergence is large enough that the framework would carry too many escape hatches; revisit at a later RC if the divergence shrinks.

**Preliminary read** (for R7.0's PR to refute or confirm): the pipeline is mechanical across the three QLs today, but RC3 + RC4 + RC6 are likely to *increase* per-handler divergence (timing, OTel spans, builder calls). Doing the Engine refactor *after* those land — at RC7 — means the abstraction is informed by all the divergence axes, not just today's narrow ones. Recommendation likely **(a) Build**, but the eval might find that **(b) Partial** captures 80% of the value at 20% of the risk.

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
- **Fixture-first.** A milestone's first PR adds *failing* TXTAR / compatibility fixtures that capture the contract. Subsequent PRs implement to turn them green. Reviewers can sanity-check intent by reading fixtures before code.
- **Compatibility suite is the source of truth.** If a PromQL feature lands but doesn't move the `prometheus/compliance` pass rate, the PR is incomplete.
- **Allowlist hygiene.** Adding an entry to `harness/compatibility/expected-failures.json` requires a comment with the upstream rationale; never empty-string.
