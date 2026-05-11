# Cerberus roadmap — v1.0.0

This document is the public-facing narrative for the path to `v1.0.0`. Status by milestone lives in the [GitHub Project](https://github.com/users/tsouza/projects) — *Cerberus v1.0.0 Roadmap*. Per-PR-level reasoning lives in the PR descriptions themselves; we don't use GitHub Issues.

## At a glance

| Release          | Theme                                                                 | What "done" means                                                                                |
| ---------------- | --------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------ |
| **v1.0.0-RC1**   | Full PromQL / LogQL / TraceQL support + 90% upstream API compatibility | Compliance corpora pass; Grafana sees cerberus as drop-in for Prom / Loki / Tempo                |
| **v1.0.0-RC2**   | Advanced QL features + deferred API surface                            | Subqueries, native-histogram quantiles, structural-chain TraceQL, LogQL `\| unpack`, Loki `tail`… |
| **v1.0.0-RC3**   | Optimizer rewrite + performance + advanced testing                     | Pattern-based rules, MV substitution, shadow-mode differential, fuzz + chaos + perf benchmarks   |
| **v1.0.0**       | Tag the last green RC                                                  | All three RCs stable; public API frozen in `pkg/`                                                |

The existing **3-rule optimizer** (filter-fusion, constant-fold, projection-pushdown) ships unchanged through RC1 and RC2. **No new optimizer work happens before RC3** — its full backlog lives in [`docs/optimizer-research.md`](optimizer-research.md).

---

## RC1 — full QL + 90% API

PRs land in milestone order. Within a milestone, the **first PR adds failing TXTAR / compliance fixtures**; subsequent PRs implement to turn them green. Compliance suites are merge gates, so coverage regressions are impossible.

### M0 — finish the seed

The remaining items from the original seed plan, plus the compliance harness scaffold.

| #   | Theme                       | Outcome                                                                                                          |
| --- | --------------------------- | ---------------------------------------------------------------------------------------------------------------- |
| M0.1 | k3d deploy + Justfile e2e   | `deploy/k3s/`, `deploy/grafana/`, `just e2e-{up,seed,run,down,playwright}`                                        |
| M0.2 | Playwright smoke + workflow | `test/e2e/playwright/`, `.github/workflows/e2e.yml`                                                               |
| M0.3 | AI-agent seed               | `CLAUDE.md`, `AGENTS.md`, three `.claude/skills/`                                                                  |
| M0.4 | Engineering hygiene         | `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SECURITY.md`, `.github/CODEOWNERS`, PR template                          |
| M0.5 | Release plumbing            | `release.yml` + `.goreleaser.yml`; tag `v0.1.0` to validate the path                                              |
| M0.6 | Compliance harness scaffold | `harness/compliance/` with `prometheus/compliance` submodule, Docker Compose, allowlist file, `compliance.yml`     |

**Exit criterion:** `just e2e` green; `v0.1.0` cut; `just compliance` runs end-to-end and produces a JSON report (failing baseline is expected and fine).

### M1 — PromQL → 90%, TDD-driven by `prometheus/compliance`

| #    | Theme                                  | Notes                                                                                                                |
| ---- | -------------------------------------- | -------------------------------------------------------------------------------------------------------------------- |
| M1.1 | Real `RangeWindow` SQL emission        | Port [promshim-clickhouse](https://github.com/BadLiveware/promshim-clickhouse)'s windowed-array idiom. **Blocker.** |
| M1.2 | `BinaryExpr` lowering                  | Arithmetic, comparison (with `bool` modifier), logical                                                              |
| M1.3 | Instant-vector functions               | `abs`, `ceil`, `floor`, `round`, `sqrt`, `ln`, `log2`, `log10`, `exp`, `scalar()`, `vector()`                       |
| M1.4 | Aggregation completeness               | `without (…)`, parameterised (`topk`, `quantile`, `count_values`), `stddev`, `stdvar`, `group`                       |
| M1.5 | `offset` and `@` modifiers             | Pass through to `RangeWindow.Offset` / `At`                                                                          |
| M1.6 | Vector matching                        | `on(…)`, `ignoring(…)`, `group_left`, `group_right` (cardinality edges defer to RC2)                                 |
| M1.7 | Float + empty-result correctness       | `NaN`/`±Inf` JSON encoding, empty groups, counter-reset semantics                                                    |

**Exit criterion:** `prometheus/compliance` ≥ 538/539 queries pass; `expected-failures.json` ≤ 2 entries (matching promshim's documented deviations: topk tie-break + float-mod drift).

### M2 — Prom HTTP API completion

| #    | Endpoint / behaviour                                              |
| ---- | ----------------------------------------------------------------- |
| M2.1 | Real per-step bucketing in `/api/v1/query_range`                   |
| M2.2 | Aggregate result shaping (`grouping.go`)                           |
| M2.3 | `chclient.QueryRaw` + `/api/v1/labels`                             |
| M2.4 | `/api/v1/label/<name>/values`                                      |
| M2.5 | `/api/v1/series`                                                   |
| M2.6 | `/api/v1/metadata`                                                 |
| M2.7 | Validation polish + `X-Prometheus-API-Version` + debug headers     |

**Exit criterion:** Grafana Prom datasource (label picker, value picker, metric hover, `sum by` panels) works end-to-end against cerberus without datasource-specific config.

### M3 — LogQL slice + Loki HTTP API

| #    | Theme                                                              |
| ---- | ------------------------------------------------------------------ |
| M3.1 | `schema.Logs` + stream selectors + line filters (`LineContent`)     |
| M3.2 | Label filters + parsers (`JSONExtract`, `LogfmtExtract`)            |
| M3.3 | Metric form (`rate`, `count_over_time`, `bytes_rate`, `bytes_over_time`) |
| M3.4 | Aggregations (reuses M1.4)                                          |
| M3.5 | Loki HTTP API + derived corpus under `harness/logql-corpus/`        |

**Exit criterion:** Derived LogQL corpus ≥ 90% pass; Grafana Explore log search works end-to-end.

### M4 — TraceQL slice + Tempo HTTP API

| #    | Theme                                                              |
| ---- | ------------------------------------------------------------------ |
| M4.1 | `schema.Traces` + span selectors + attribute matchers (`FieldAccess`) |
| M4.2 | Structural operators `>>` / `>` / `<<` / `<` via `chplan.StructuralJoin` |
| M4.3 | Aggregators (`count`, `sum`, `avg`, `max`, `min`)                  |
| M4.4 | Time filters + `\| select(...)` via `TimeFunc`                      |
| M4.5 | Tempo HTTP API + derived corpus under `harness/traceql-corpus/`    |

**Exit criterion:** Derived TraceQL corpus ≥ 90% pass; Grafana trace search + waterfall both work.

### M5 — RC1 release

| #    | Theme                                                              |
| ---- | ------------------------------------------------------------------ |
| M5.1 | `CHANGELOG.md` with features + RC2 deferrals                       |
| M5.2 | README drops the seed badge; status block reads "RC1"              |
| M5.3 | Tag `v1.0.0-RC1`; `release.yml` cuts multi-arch binaries + image    |
| M5.4 | `docs/compliance.md` documents allowlist + per-QL corpus extension |

---

## RC2 — advanced QL + API features

The remaining ~10% per QL, plus the deferred API endpoints. Each lands as its own PR after RC1 tags.

- **PromQL** — subqueries (`m[1h:5m]`); `histogram_quantile` on native histograms; `predict_linear`, `holt_winters`; `@start()`/`@end()`; exemplar attachment; recording-rule inline expansion; `group_left`/`group_right` cardinality enforcement edge cases.
- **LogQL** — `| unpack`, `| pattern`; advanced `label_format` templating; `bytes_*` precise alignment; `tail` (WebSocket streaming).
- **TraceQL** — multi-hop structural chains with predicates at each hop; `histogram_over_time`; link traversal + span-event queries; root-span filtering in nested conditions.
- **HTTP APIs** — Prom `query_exemplars`, `format_query`, `parse_query`; Loki `tail`, `index/stats`, `index/volume`, `detected_fields`, `patterns`; Tempo `search/recent`, `metrics/query_range`.

**Exit criterion:** the lists above empty out; compliance pass rate stays ≥ RC1 baseline.

---

## RC3 — optimizer + performance + advanced testing

All of [`docs/optimizer-research.md`](optimizer-research.md) lands here. The reading list is the contract.

### Optimizer rewrite

| #    | Item                                                              | Primary reference                                                                                   |
| ---- | ----------------------------------------------------------------- | --------------------------------------------------------------------------------------------------- |
| R3.1 | Pattern-based `Rule` API (Calcite-style match / transform)         | [Apache Calcite `org.apache.calcite.rel.rules`](https://calcite.apache.org/javadocAggregate/)       |
| R3.2 | `FilterProjectTranspose` + `FilterAggregateTranspose` rules        | Same                                                                                                |
| R3.3 | Catalyst-style `Batch` grouping                                    | [Spark `Optimizer.scala`](https://github.com/apache/spark/blob/master/sql/catalyst/src/main/scala/org/apache/spark/sql/catalyst/optimizer/Optimizer.scala) |
| R3.4 | Sort-key-aware filter emission + `PREWHERE` promotion              | [ClickHouse query-optimization guide](https://clickhouse.com/resources/engineering/clickhouse-query-optimisation-definitive-guide) |
| R3.5 | Analyzer vs Optimizer rule split                                   | [DataFusion optimizer crate](https://docs.rs/datafusion-optimizer/latest/datafusion_optimizer/)     |

### Performance features

| #    | Item                                                              | Primary reference                                                                                   |
| ---- | ----------------------------------------------------------------- | --------------------------------------------------------------------------------------------------- |
| R3.6 | Materialised-view substitution for `otel_metrics_*` rollups (cost-model trigger) | [Promscale #152](https://github.com/timescale/promscale/issues/152) + [Jindal VLDB 2018](http://www.vldb.org/pvldb/vol11/p800-jindal.pdf) |
| R3.7 | Late materialisation for wide-column scans (logs `Body`, `ResourceAttributes`) | [Selective Late Materialization, VLDB 2025](http://people.iiis.tsinghua.edu.cn/~huanchen/publications/slm-vldb25.pdf) |
| R3.8 | Filter–RangeWindow transpose                                       | [VictoriaMetrics `metricsql/optimizer.go`](https://github.com/VictoriaMetrics/metricsql/blob/master/optimizer.go) |

### Advanced testing

| #     | Item                                                              | Primary reference                                                                                   |
| ----- | ----------------------------------------------------------------- | --------------------------------------------------------------------------------------------------- |
| R3.9  | Shadow-mode differential testing (prefer / force-native / oracle) | [promshim-clickhouse `harness/compliance/`](https://github.com/BadLiveware/promshim-clickhouse/tree/main/harness/compliance) |
| R3.10 | Port promshim's local Go evaluator                                 | Same — `internal/promshim/local/`                                                                   |
| R3.11 | Fuzz + chaos + perf-benchmark CI                                   | `go-fuzz`, custom chaos harness, perf-benchmark workflow                                            |

**Exit criterion:** golden-fixture SQL shrinks on real plans; `internal/optimizer` mutation score ≥ 70%; MV substitution active; shadow-mode reveals < 5% native-SQL gap.

---

## How we work

- **PR-per-change.** Every change ships as its own PR against `main`. Branch protection requires `ci / check` + `ci / lint`, linear history, no force-push.
- **No GitHub Issues.** Tracking lives in the GitHub Project board; reasoning lives in PR descriptions; backlog narratives live in `docs/*.md` files.
- **Fixture-first.** A milestone's first PR adds *failing* TXTAR / compliance fixtures that capture the contract. Subsequent PRs implement to turn them green. Reviewers can sanity-check intent by reading fixtures before code.
- **Compliance suite is the source of truth.** If a PromQL feature lands but doesn't move the `prometheus/compliance` pass rate, the PR is incomplete.
- **Allowlist hygiene.** Adding an entry to `harness/compliance/expected-failures.json` requires a comment with the upstream rationale; never empty-string.
