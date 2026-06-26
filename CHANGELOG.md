# Changelog

All notable changes to cerberus will be documented in this file. The format roughly follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), with one entry per tagged release.

## [Unreleased]

## [v1.7.2] — 2026-06-26

### Fixed

- **chsql:** dedup duplicate-timestamp samples in row-path rate/increase/delta (#1092)

## [v1.7.1] — 2026-06-26

### Fixed

- **tempo:** bound windowless tag/attribute discovery to recent window (#1089)
- **prom:** push request window into no-match metadata discovery scans (#1088)

## [v1.7.0] — 2026-06-25

### Added

- **api:** add /info metadata + optimization fingerprint endpoint (#1082)
- **schema:** configurable storage_policy + MergeTree settings on auto-created tables (#1081)

### Fixed

- **chclient:** re-key query_id on columnar row-path fallback to avoid CH 216 (#1086)
- **release:** maintenance preflight waits for CI to finish instead of snapshotting (#1083)
- **traceql:** push time bound into compare scan + root leg, not above the join (#1080)
- **chopt:** probe CH version over the default connection so a missing otel DB no longer pins 24.8 (#1079)

## [v1.6.1] — 2026-06-25

### Added

- **chopt:** composable auto token in CERBERUS_CH_OPTIMIZATIONS (#1076)
- **optcorpus:** capture cerberus-side rejections in the router corpus (#1065)
- **routerrules:** detection-fidelity benchmark + degradation sweep (#1063)
- **routerrules:** concrete 5-rule ruleset + harness + chDB-parity CI fix (#1062)
- **routerrules:** generic router-rules catalog + offline analysis engine (#1060)

### Fixed

- **release:** maintenance preflight excludes de-gated informational lanes (#1077)
- **routerrules:** gate route_a_memory_near_cap on the configured cap, not the corpus p95 (#1066)
- **e2e:** reconcile Traces-Drilldown init-race 400 when both bodies are lost (#1070)
- **routerrules:** wrap integer CH columns to match Go scan types (strict clickhouse-go) (#1064)
- **traceql:** bound compare() query memory to avoid 2GiB ClickHouse OOM (#1059)

### CI

- **lint:** exclude .claude worktrees from golangci-lint scan (#1071)
- **release:** auto-retire the out-of-window release line on a new minor (active EOL) (#1072)

### Documentation

- theme-aware README hero via <picture> (#1074)
- **readme:** branded hero banner + regrouped badges (#1073)
- forbid "pre-existing" as an escape hatch for leaving bugs unfixed (#1069)
- **release:** define the maintenance support-window / EOL policy (latest 3 minor lines) (#1068)
- **coverage:** reframe PromQL start()/end() rejection as permanent parity (#1061)

## [v1.6.0] — 2026-06-25

### Added

- **optcorpus:** capture cerberus-side rejections in the router corpus (#1065)
- **routerrules:** detection-fidelity benchmark + degradation sweep (#1063)
- **routerrules:** concrete 5-rule ruleset + harness + chDB-parity CI fix (#1062)
- **routerrules:** generic router-rules catalog + offline analysis engine (#1060)

### Fixed

- **routerrules:** gate route_a_memory_near_cap on the configured cap, not the corpus p95 (#1066)
- **e2e:** reconcile Traces-Drilldown init-race 400 when both bodies are lost (#1070)
- **routerrules:** wrap integer CH columns to match Go scan types (strict clickhouse-go) (#1064)
- **traceql:** bound compare() query memory to avoid 2GiB ClickHouse OOM (#1059)

### CI

- **lint:** exclude .claude worktrees from golangci-lint scan (#1071)
- **release:** auto-retire the out-of-window release line on a new minor (active EOL) (#1072)

### Documentation

- theme-aware README hero via <picture> (#1074)
- **readme:** branded hero banner + regrouped badges (#1073)
- forbid "pre-existing" as an escape hatch for leaving bugs unfixed (#1069)
- **release:** define the maintenance support-window / EOL policy (latest 3 minor lines) (#1068)
- **coverage:** reframe PromQL start()/end() rejection as permanent parity (#1061)

## [v1.5.0] — 2026-06-24

### Added

- **optcorpus:** record routing decision + cost-grid for route A/B calibration (stage 0) (#1053)

### Fixed

- remediate verified audit findings (#1046)
- **promql:** pass TimeUnix through scalar-wrapped rate arm in range joins (#1045)
- **api:** harden Loki/Tempo HTTP surface against POST + DoS vectors (#1049)
- **chart:** per-head PDB + auto-derived GOMEMLIMIT for split mode (#1040)
- **e2e:** re-provision Grafana after split-mode datasource rewrite (#1037)

### Performance

- **chsql:** push inner-scan time bound on range query lowerings (#1048)

### CI

- **release:** support maintenance-line (release/X.Y.x) hotfix publishing (#1054)
- **pr-label:** self-healing backfill + shared mapping (#1051)
- **mutation:** skip gremlins matrix on non-release PRs (aggregator passes through) (#1052)
- **release:** publish on merge of a validated release PR, not on raw tag (#1044)
- **release:** make opening a release PR label-triggered (#1039)
- **e2e:** isolate split_isolation in its own dashboard shard (#1038)
- **e2e:** run the FULL matrix (split + crawl) on release PRs (#1036)

## [v1.4.0] — 2026-06-22

### Added

- **chart:** split mode — isolated per-head deployments (no proxy) (#1031)
- **api:** O(output) drain counter + falsifiable bounds-drain regression harness (#1030)
- **config:** add CERBERUS_ENABLED_HEADS per-head toggle (#1029)

### Fixed

- **test:** thread a time window into the TraceQL property harness (#1032)
- **config:** default-on per-query sample budget at 5M (#1028)
- **tempo:** bound /api/search drain with SQL trace-limit + window pushdown (#1027)

## [v1.3.0] — 2026-06-19

### Added

- **config:** accept humanized byte sizes (2Gi) for memory caps, BWC-preserving (#1017)

### Fixed

- **telemetry:** apply CERBERUS_LOG_LEVEL to the OTLP slog bridge (stop debug leaking to otel_logs) (#1018)

### CI

- **release:** make release tooling backport-aware (maintenance lines + :latest guard) (#1019)

## [v1.2.0] — 2026-06-19

### Added

- **chclient:** surface ch-go telemetry on cerberus's own telemetry (#1007)

### Fixed

- **loki:** tail drops overflow rows when a poll window exceeds the limit (#1011)
- **release:** gate strictly on main HEAD fully settled green (#1008)

### Documentation

- collapse native-upstream roadmap into a single note (uses-today + positioning) (#1014)
- **roadmap:** mark 5A external-table push DEFERRED — no qualifying call-site (#1010)
- native-CH roadmap execution addendum (code-now + ambitious chase) (#1009)

### Dependencies

- bump `github.com/ClickHouse/ch-go` 0.71 → 0.72, which exposes the client-side query telemetry surfaced in #1007 (#1005)

## [v1.1.0] — 2026-06-19

### Added

- **ci:** manual prepare-release workflow + generator (#1003)
- **chopt:** generate opt feature table from registry + drift gate (#998)
- **config:** generate docs/configuration.md from viper config + CI drift gate (#1000)
- **promql:** adopt native timeSeriesChangesToGrid/ResetsToGrid (25.9) (#990)
- **chclient:** columnar query_range matrix decode via ch-go (flag-gated) (#983)

### Fixed

- **e2e:** gate showcase probes on seed-fixture signal to kill unless-panel flake (#993)
- **e2e:** stop the false-positive DiskPressure breadcrumb mislabelling (#992)
- **test:** serialize chDB engine access to stop SIGABRT in result.Free (#984)

### Performance

- **solver:** share immutable off-spine in plan slicing (copy-on-write) (#988)

### Changed

- **chopt:** adopt columnar decode as a CH_OPTIMIZATIONS feature, drop standalone env (#989)
- **promql:** pure polymorphic range-lowering dispatch (no nil-check) (#986)

### CI

- **forbid-skip:** assert-from-source doc-count gate; fix forbid-skip 6->5 + layer 12->13 drift (#997)
- add internal/external link + doc-to-code reference gates (#999)
- **clickhouse:** central versions.yaml SoT + version-sync gate (#995)
- **e2e:** cache Playwright chromium across e2e shards (#994)
- **forbid-skip:** drop the wording-tests vocabulary scan, keep the five behavioural checks (#991)
- **e2e:** free runner disk before the stack to stop DiskPressure evictions (#981)

### Documentation

- **changelog:** restructure pre-1.1.0 — backfill v1.0.1/v1.0.2, stage [Unreleased] (#1004)
- fix configuration.md anchor links broken by generator (#1000) (#1001)
- sync docs to code ahead of v1.1.0 (feature table, COW, dead links, counts) (#996)
- native-ClickHouse roadmap + staged timeSeriesIncreaseToGrid contribution (#982)

## [v1.0.2] — 2026-06-18

### Added

- **ClickHouse-optimization suite + auto-picker.** A cohesive optimization
  layer driven by two knobs: `CERBERUS_CH_OPTIMIZATIONS` (`auto` | `off` |
  comma-separated feature ids, default `auto`) and
  `CERBERUS_CH_OPTIMIZATIONS_MODE` (`permissive` | `enforcing`, default
  `permissive`). At startup cerberus probes `SELECT version()` once and resolves
  an immutable enabled-set: under `auto` it enables every **stable** feature the
  server supports and never an experimental one; an explicit list honours the
  mode for unsupported features (WARN+skip vs FATAL) and a typo'd id is always
  fatal. The seeded registry: `aggregation_in_order` (24.8, stable, auto-enabled —
  stamps `optimize_aggregation_in_order=1` on sort-key-prefix GROUP BY plans),
  `condition_cache` (25.3, stable — stamps `use_query_condition_cache=1` on
  predicate-stable read paths), and `ts_grid_range` (25.6, experimental,
  explicit-only). Everything is version-safe: a feature whose floor exceeds the
  connected server is simply not enabled, so cerberus keeps emitting its
  24.8-safe SQL. See [`docs/clickhouse-optimizations.md`](docs/clickhouse-optimizations.md).
- **Async `system.query_log` performance-corpus reconciler**
  (`CERBERUS_CH_OPT_CORPUS_ENABLED`, off by default; `CERBERUS_CH_OPT_CORPUS_INTERVAL`,
  `CERBERUS_CH_OPT_CORPUS_SINK_PATH`). A bounded background reconciler joins
  recently-dispatched cerberus query_ids back to `system.query_log` for their
  server-side cost (read rows/bytes, duration, memory, ProfileEvents) and
  appends `(shape-id, opts, timings)` tuples to a durable JSONL sink an operator
  can mine. Production-only (chDB has no `system.query_log`); errors are logged,
  never fatal. The dispatch seam is non-blocking and O(1) (a single buffered
  channel send into a fixed-size circular ring), so it never serializes the
  prom/loki/tempo heads or taxes the data plane; the `system.query_log` scan is
  resource-capped (`max_execution_time`, `max_threads=1`, low `priority`,
  row/byte read limits) so it cannot starve data-plane queries.

### Deprecated

- **`CERBERUS_EXPERIMENTAL_TS_GRID_RANGE`** is soft-deprecated in favour of
  `CERBERUS_CH_OPTIMIZATIONS` (list `ts_grid_range`). It keeps working — it is
  re-routed through the optimization resolver (under `auto`: explicit `true`
  force-enables, `false` force-disables, unset has no effect; any explicit
  `CERBERUS_CH_OPTIMIZATIONS` choice — a list **or** the `off` kill-switch —
  overrides the legacy flag, so `off` stays absolute) — and emits a one-time
  startup deprecation warning.

### Changed

- **Per-query instrumentation** (query_id / `log_comment` shape id) and a
  ClickHouse settings map, plus the `aggregation_in_order` optimization. (#978)
- `histogram_quantile` phi-domain handling (+/-Inf out of range) and
  `vector(scalar)` vector-typing fixes. (#974)

## [v1.0.1] — 2026-06-18

### Added

- **Publishable cerberus Helm chart + OCI release pipeline**, exposing the full
  `CERBERUS_*` config surface, prod-HA typed values with ClickHouse co-location,
  and a chart-validate / kubeconform / helm-docs drift gate. (#962, #968)

### Fixed

- Restore integer per-head admission caps with bool aliases. (#973)
- `on()`/`ignoring()` one-to-one binop leaking non-matching labels; vector-join
  dropping operand `MetricName`/`TimeUnix` (code-47). (#971)
- Uniform boolean parsing (1/0/true/false) across all `CERBERUS_*` env vars.

## [v1.0.0] — 2026-06-17

First general-availability release. Cerberus is a drop-in Prometheus /
Loki / Tempo HTTP gateway for ClickHouse: each head parses with its
reference upstream parser, lowers to a shared plan IR, runs a rule-based
optimizer, and emits parameterised ClickHouse SQL — so Grafana, alerting,
and CLI tooling see three normal datasources speaking unmodified PromQL /
LogQL / TraceQL.

**Wire-format API stability.** The three upstream HTTP surfaces cerberus
serves are the 1.0 compatibility contract and follow semantic versioning
from here. The query languages are the upstream parsers' own, so they
track upstream. The `CERBERUS_*` configuration surface is stable;
additive changes only within 1.x.

This is a young, actively-developed project: a confident 1.0 because the
behaviour is held to reference engines by differential harnesses on every
merge — not because every edge is explored. Two areas carry honestly lower
confidence and are called out below.

### Capabilities at 1.0

- **PromQL** scored against the third-party CNCF / PromLabs
  [PromQL Compliance Tester](https://github.com/prometheus/compliance) —
  574/574 cases passing against a real Prometheus, no allow-list — plus
  subqueries, `histogram_quantile` over classic and native histograms,
  `predict_linear` / `holt_winters`, `@start()` / `@end()`,
  `group_left` / `group_right`, and the full instant + range-query surface.
- **LogQL** diffed against a real Loki on Grafana's own `pkg/logql/bench`
  corpus: pipeline stages (`| json` / `| logfmt` / `| pattern` / `| unpack`
  / `| line_format` / `| label_format` / …), metric queries, structured
  metadata, and the `/labels` / `/series` / `/index/stats` metadata surface.
- **TraceQL** diffed against a real Tempo: structural operators, nested-set
  intrinsics in `| select(...)`, set ops, `group` / `coalesce`, the metrics
  pipeline, and the `/api/search` + tag-discovery surface.
- **Coverage** ([`docs/coverage.md`](docs/coverage.md)): 226 of 228
  catalogued symbols supported across the three heads, 2 intentional
  parity rejections (bare `start()` / `end()`), **zero** wrong-rejections.
- **OpenTelemetry-native** schema (the `clickhouseexporter` table shape),
  with resource attributes projected as Prometheus labels and the
  `CERBERUS_SCHEMA_*` overrides for non-default layouts.
- **Operations**: `ReplicatedMergeTree` / `Replicated`-database schema
  bootstrap (`CERBERUS_AUTO_CREATE_SCHEMA`), per-head ClickHouse circuit
  breakers, OTLP self-telemetry export, `/readyz` / `/healthz` probes, and
  the full ClickHouse connection surface (TLS, timeouts, pool sizing).
- **Performance**: single-pass prefix-sum range aggregation, sharded
  pushdown solver, PREWHERE promotion + late materialisation, metadata
  fan-in batching, and an optional experimental native-rate path
  (`CERBERUS_EXPERIMENTAL_TS_GRID_RANGE`, off by default) — all held
  against regression by the compute-fan-out perf-guard ratchets.

### Changed since the v1.0.0-rc series

- **Metadata endpoints scan the full `[start,end]` window.** `/api/v1/series`,
  `/labels`, and `/label/<name>/values` enumerate every series/label/value
  with any sample in the requested window instead of an instant staleness
  window at `now`, fixing intermittent empty results for late-arriving
  (delta-temporality) data.
- **Instant range-vector queries anchor to `time=T`**, not ClickHouse
  wall-clock, closing an intermittent empty-window class.
- **GCP / cloud metric-name translation**: slash-containing OTel names,
  `histogram_quantile` over `sum_over_time` of delta-histogram buckets,
  aggregated-range ÷ scalar, and standalone `_sum` / `_count`-suffixed
  gauges all resolve.
- **Resource attributes as Prometheus labels** (env / namespace / pod /
  cluster) with bounded query-time memory.
- **Replicated schema**: emit explicit bare `ReplicatedMergeTree` under a
  `Replicated` database; cold-cluster boot creates the database itself
  instead of fatally exiting.
- **Full ClickHouse connection configuration** surface exposed (TLS,
  read timeout, pool limits).

### Known limitations (honest at 1.0)

- **TraceQL conformance is the lightest of the three heads.** There is no
  third-party TraceQL conformance suite; its corpus is cerberus-owned
  author-written TXTAR diffed against a real Tempo, so its breadth is
  author-bounded rather than reference-derived. Raising TraceQL's
  confidence is the top post-1.0 item. See
  [`docs/compatibility.md`](docs/compatibility.md).
- **Cerberus is a query gateway, not a store.** It runs no ingestion and
  caches nothing (only the `/readyz` TTL); bring your own ClickHouse and
  OTel pipeline.
- **Per-head circuit-breaker recovery may briefly flap** as HALF-OPEN
  probes re-converge after a ClickHouse restart ([#94]).

## [v1.0.0-rc.1]

The first published release candidate on the `v1.0.0` line — the core
slice plus the advanced-QL surface. This was an early cut: the three
heads parse → lower → execute and the differential harnesses gate every
merge, but the surface was still evolving. The `rc.2` → `rc.9`
prereleases that followed (replicated-schema bootstrap, resource-attribute
labels, the perf collapses, and the GCP / metadata query-translation
fixes) are summarised under [v1.0.0] above and listed individually on the
[releases page](https://github.com/tsouza/cerberus/releases).

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

## v0.1.0 — Seed (pre-release history, not tagged)

The seed series (PR1–PR7 + admin + roadmap) that predates the published
`v1.0.0-rc.*` tags:

- Module `github.com/tsouza/cerberus` on `go 1.26.2` with the `replace github.com/hashicorp/memberlist => github.com/grafana/memberlist@…` hygiene fix.
- Shared plan IR (`internal/chplan`), ClickHouse SQL emitter (`internal/chsql`), TXTAR spec runner under `test/spec/`.
- Rule-based optimizer (`internal/optimizer`) with three rules: filter fusion, constant folding, projection pushdown.
- PromQL vertical slice (`internal/promql/lower.go`) covering instant vector selectors, label matchers (eq / ne / regex), range vectors (placeholder SQL), and aggregations (`sum`, `count` with `by(…)`).
- HTTP API surface (`internal/api/prom`) for `/api/v1/query` + `/api/v1/query_range` (range_range returns a single point until full `RangeWindow` lowering lands in M1.1).
- CH client wrapper (`internal/chclient`) over `clickhouse-go/v2` with a testcontainers integration test.
- CI: two-job workflow (`check` + `lint`), commitlint relaxed for Dependabot, markdownlint, mutation testing (gremlins) on a nightly cron.
- Branch protection on `main`: required checks, linear history, no force pushes / deletions.

[v1.0.0]: https://github.com/tsouza/cerberus/releases/tag/v1.0.0
[v1.0.0-rc.1]: https://github.com/tsouza/cerberus/releases/tag/v1.0.0-rc.1
