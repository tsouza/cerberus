# Engine

The query engine — `internal/engine/` — is the shared pipeline that
turns an upstream language query (PromQL / LogQL / TraceQL) into a
ClickHouse result. The three HTTP heads (Prometheus, Loki, Tempo)
each plug in as a `Lang` adapter; for those query routes the engine owns
the parse → optimize → emit → execute loop and the telemetry around it.

## Overview

Cerberus has one shared language-query pipeline, not three. Each query
language has its own parser and its own response shape, but the
work in the middle — lowering to the shared plan IR, optimizing,
emitting ClickHouse SQL, executing against the driver — is
identical. The engine extracts that middle so those routes stay thin:
HTTP dispatch + per-language adapter wiring + response shaping.

```text
   HTTP request
        │
        ▼
   query handler (api/{prom,loki,tempo})
        │
        │  builds Lang adapter
        ▼
   engine.Engine.Query(ctx, lang, query)
        │
        │  parse → wrap-projection → optimize → emit → execute
        ▼
   engine.Result
        │
        ▼
   handler formats Result.Samples into the upstream wire shape
        │
        ▼
   HTTP response
```

The engine package lives at `internal/engine/`; the per-language
adapters live next to (or inside) the head they serve:
`internal/api/prom/lang.go`, `internal/logql/lang.go`,
`internal/api/tempo/lang.go`.

Not every upstream endpoint is a language query or returns the canonical
sample row shape. Metadata, discovery, live-tail, and enrichment endpoints
compose typed SQL in `internal/api` and use dedicated client decoders; those
intentional paths are listed under [API-owned SQL paths](#api-owned-sql-paths).

## The Engine + Lang contract

The contract centers on `Engine`, the `Lang` interface, per-query `Meta`,
and the result types.

### `Engine`

```go
type Engine struct {
    // pipeline
    Optimizer       *optimizer.Driver
    Client          Querier
    Settings        SettingsRules
    liveSettings    atomic.Pointer[SettingsRules]
    // optional seams (nil keeps the single-statement path)
    Solver                  *solver.Solver
    QueryObserver           QueryObserver
    RouteMemo               *routememo.Memo
    PerRungAdmission        *PerRungAdmissionLearner
    ScanEstimateAdvisor     *ScanEstimateAdvisor
    CardinalityProbeAdvisor *CardinalityProbeAdvisor
    Actuals                 *actuals.Tracker
    // plan-side bounds (0 disables each)
    MaxQuerySamples                      int64
    MaxEmittedSQLBytes                   int64
    RangeBucketFanoutMaxRows             int64
    RangeLWRFanoutMaxRows                int64
    RateWindowFanoutMaxRows              int64
    RangeBucketFanoutFoldCostMaxUnits    int64
    RangeBucketGridNativeMaxRows         int64
    RangeBucketGridNativeMaxDensityUnits int64
    // DELTA-temporality prefix reconstruction
    DeltaPrefixLookback    time.Duration
    DeltaPrefixReadEnabled bool
}
```

- `Optimizer` runs the rule-based fixpoint driver over the plan
  after the wrap-projection. Required.
- `Client` is the ClickHouse executor. The engine only needs the
  narrow `Querier` interface (`Query(ctx, sql, args...) ([]Sample,
  error)`); when the underlying client also satisfies the optional
  `CursorQuerier` interface, the engine's `QueryCursor` /
  `QueryPlanCursor` entry points open a streaming cursor instead
  of draining rows into a slice. Required.
- `Solver` optionally classifies PromQL plans and executes the sharded
  route; `nil` keeps every request on the single-statement route.
- `Settings` holds plan-gated ClickHouse settings. `liveSettings` is the
  atomic replacement installed by `SetSettings` after capability changes.
- `QueryObserver` optionally records dispatches and outcomes for the
  asynchronous query-log performance corpus.
- `MaxQuerySamples` rejects oversized subquery anchor grids before
  dispatch; `0` disables that plan-side gate. The other `*MaxRows` /
  `*MaxUnits` / `MaxEmittedSQLBytes` fields are the per-carrier resource
  bounds `cmd/cerberus` threads in from configuration; each is a
  plan-side rejection and `0` disables it.
- `RouteMemo` optionally remembers resource-failure routing outcomes and
  can steer a later eligible PromQL request to route B.
- `PerRungAdmission`, `ScanEstimateAdvisor`, `CardinalityProbeAdvisor`
  and `Actuals` are the remaining optional seams — evidence-based
  admission, the advisory `EXPLAIN ESTIMATE` and cardinality pre-flights,
  and the predicted-vs-actual drift tracker. `docs/solver.md` describes
  each; `nil` leaves it inert.
- `DeltaPrefixLookback` / `DeltaPrefixReadEnabled` bound and gate the
  DELTA-temporality prefix-reconstruction scan (`docs/operations.md`).

One Engine instance is constructed per HTTP head in
`cmd/cerberus/main.go` and lives for the lifetime of the process.

### `Lang`

```go
type Lang interface {
    Name() string
    Parse(ctx context.Context, query string) (chplan.Node, Meta, error)
    ProjectSamples(plan chplan.Node, meta Meta) (chplan.Node, error)
}
```

- `Name()` returns a stable identifier — `"promql"`, `"logql"`,
  `"traceql"`. The engine threads it onto progress-context keys
  and telemetry labels.
- `Parse` runs the head's parser — the upstream Apache prometheus
  parser for PromQL, cerberus's in-house Apache reimplementation for
  LogQL / TraceQL — lowers the AST into a
  `chplan` tree, and returns the plan plus a `Meta` value. The
  adapter is also responsible for opening the `parse` / `lower`
  pipeline-stage spans so the trace shape is consistent across
  heads.
- `ProjectSamples` wraps the plan with whatever projection the
  adapter needs so that the executed SQL emits rows in the
  canonical `chclient.Sample` shape — `(MetricName, Attributes,
  TimeUnix, Value)`. Each head's per-shape switch
  (canonical / derived / structural-join) lives in the adapter,
  not in the engine. The shape is *positional*: the Loki
  log-stream projection is `(Line, Attributes, TimeUnix)`, plus a
  trailing `Metadata` column on a schema that carries structured
  metadata. It puts the log line in the first, String-typed column
  (aliased `logql.LogLineColumn`) and carries no numeric column at
  all, because a log stream has no value to report. The cursor
  recognises the shape by that leading alias and binds a scan with
  no float destination; `chclient.DecodeLogRows` then decodes the
  row into the named `chclient.LogRow` the streams pivot consumes.

### `Meta`

```go
type Meta struct {
    IsMetric      bool
    IsTraceByID   bool
    ResponseShape string
    Guards        []Guard
    Extra         map[string]any
}
```

Per-query semantic flags the engine needs but cannot infer from
the plan alone:

- `IsMetric` — the response is matrix / vector shaped. PromQL
  always sets this; LogQL sets it when the parsed expression is a
  metric query (rate, count_over_time, vector aggregations);
  Tempo never sets it.
- `IsTraceByID` — short-circuit for Tempo's `/traces/{id}`
  endpoint. The plan is built by the handler without a parser;
  the engine skips the optimizer pass since a row-by-id fetch has
  no rewrites worth running.
- `ResponseShape` — handler-side pivot key (`"loki-matrix"`,
  `"loki-streams"`, `"tempo-trace"`, `"tempo-metrics-matrix"`,
  `"tempo-metrics-instant"`, and `chclient.ResponseShapeMatrix` =
  `"prom-matrix"` on the PromQL `/query_range` path). The engine does not
  read it; it is threaded through `Result` so the response formatter does
  not have to re-derive it, and `chclient` reads the matrix value off the
  context to confirm caller intent before engaging the columnar decode.
- `Guards` — ordered value-domain checks that the engine emits and
  executes before the main query. The first violation rejects the request.
- `Extra` — adapter-specific bag for per-language knobs that ride
  through `Meta` without bloating the type (the LogQL adapter
  uses it to carry the parsed `syntax.Expr` to the handler).

### `Result`

```go
type Result struct {
    Samples       []chclient.Sample
    SQL           string
    Args          []any
    Strategy      string
    CHMillis      int64
    PlanNodeCount int
    Headers       map[string]string
    Meta          Meta
    Inspected     int64
}
```

- `Samples` is the decoded row stream. Handlers pivot it into the
  upstream wire shape.
- `SQL` + `Args` are surfaced for debug logging.
- `Strategy` is the execution-path label — `"trace-by-id"` for the Tempo
  `/traces/{id}` short-circuit, `"native"` otherwise — the same value the
  `X-Cerberus-Strategy` header carries.
- `CHMillis` is the wall-clock time spent in `Client.Query`,
  exposed through the `X-Cerberus-CH-Millis` response header.
- `PlanNodeCount` is the optimised plan's node count, exposed
  through `X-Cerberus-Plan-Nodes`.
- `Headers` is a bag of extra response headers the engine wants
  the handler to stamp on the response — keeps the engine free
  of `http.ResponseWriter`.
- `Meta` is the same `Meta` the adapter returned, threaded
  through so the response pivot can switch on it.
- `Inspected` is the number of rows drained from ClickHouse on the eager
  path. Streaming callers read the corresponding count from the cursor.

A streaming sibling — `CursorResult` — mirrors this shape but
carries a `chclient.Cursor` instead of a `[]Sample` slice. The
caller is responsible for `cursor.Close()`.

## Request lifecycle

A typical parsed language-query request flows through the following stages:

1. **HTTP dispatch.** The per-API handler (`internal/api/prom`,
   `internal/api/loki`, `internal/api/tempo`) parses the HTTP
   request — URL, query parameters, time window, step.
2. **Adapter construction.** The handler builds a per-request
   `Lang` adapter, passing in any state the parser needs
   (PromQL's evaluation window for `@ start()` / `@ end()`, the
   schema config, …).
3. **Engine entry.** The handler calls one of:
   - `Engine.Query(ctx, lang, queryStr)` — the common case.
   - `Engine.QueryPlan(ctx, lang, plan, meta)` — Tempo's
     `/traces/{id}` path, where the handler builds the lookup
     plan directly and skips the parser.
   - `Engine.QueryCursor(ctx, lang, queryStr)` /
     `Engine.QueryPlanCursor(...)` — streaming variants for
     Prom's `/query_range` matrix pivot.
4. **Inside the engine:**
   1. `lang.Parse` runs the head's parser and lowers to
      `chplan`. Opens `parse` + `lower` spans.
   2. The engine executes any `Meta.Guards` before the main statement.
   3. `lang.ProjectSamples` wraps the plan into the canonical
      `Sample` row shape.
   4. The optimizer runs (skipped when `Meta.IsTraceByID` is
      set). Opens an `optimize` span.
   5. The optional solver classifies the optimized plan and may select
      the sharded route.
   6. `chsql.Emit` materialises the plan into parameterised
      ClickHouse SQL. Opens an `emit` span.
   7. `Client.Query` executes the SQL. Opens an `execute` span;
      records wall-clock time into `Result.CHMillis`.
5. **Result.** The engine returns a `Result` (or `CursorResult`)
   with the decoded samples and the metadata the handler needs
   to format the response.
6. **Response formatting.** The handler pivots
   `Result.Samples` into the upstream wire shape (Prom
   `{vector|matrix}` JSON, Loki `{streams|matrix}` JSON, Tempo
   trace summary JSON) and writes it to the response.

Errors are wrapped per stage (`engine: parse: …`,
`engine: emit: …`, `engine: execute: …`) so callers can
classify them with `errors.Is` / `errors.As`. Adapter-specific
error types — `parseStageError` in the Prom adapter,
`*httperr.Error` in the LogQL adapter — ride through the wrap
so the handler can map them to the right HTTP status without
losing the cause.

## API-owned SQL paths

The engine is the common path when an endpoint starts from a language query
or plan and consumes canonical `Sample` rows. Some upstream APIs need a
different row decoder, combine several generated statements, or perform a
side query around the main result. Those handlers use the same typed `chsql`
surface and `chclient`, but intentionally do not force the work through
`Engine.Query`:

- **Prometheus:** `internal/api/prom/metadata.go` builds the catalog and
  series fan-in queries; `internal/api/prom/exemplars.go` emits the
  endpoint's dedicated exemplar rows.
- **Loki:** under `internal/api/loki`, `labels.go`, `label_values.go`,
  `series.go`, `index_stats.go`, `index_volume.go`, `detected_labels.go`,
  `detected_fields.go`, and `patterns.go` build metadata or sampled-row
  shapes; `tail.go` builds each bounded poll in the live WebSocket loop.
- **Tempo:** under `internal/api/tempo`, `search_tags.go` and
  `search_tag_values.go` build tag discovery; `root_lookup.go` and
  `structural_two_phase.go` emit follow-up or narrow phase plans;
  `metrics_exec.go` emits the optional exemplar side query.

These are API-layer orchestration paths, not alternate query-language
pipelines. Main PromQL, LogQL, and TraceQL query execution still converges on
the engine.

## Pipeline stages in depth

The middle of the pipeline — lower → optimize → emit → schema
resolution — is the part the three heads share. Each stage is
described below. The three heads converge on it: each parses its query
language — PromQL with the upstream Apache prometheus parser, LogQL and
TraceQL with cerberus's own in-house Apache reimplementations — lowers to
the shared IR, and runs the same
optimize step. After optimize, the solver classifies the plan and picks
an execution route — route A (one ClickHouse statement, the default for
the overwhelming majority of traffic) or route B (the sharded-pushdown
solver, for the memory-unbounded anchor-fan-out class). Both routes emit
through the same `chsql` emitter; route B just emits and executes K
re-anchored shards and concatenates them behind one cursor.

```text
   PromQL                LogQL                TraceQL
     │                     │                     │
     ▼                     ▼                     ▼
prometheus/        internal/logql/          internal/traceql/
promql/parser      lsyntax                  ast                ← PromQL: upstream Apache parser; LogQL/TraceQL: in-house Apache parsers
     │                     │                     │
     │      per-QL lowering (head → chplan)      │
     ▼                     ▼                     ▼
 ┌──────────────────────────────────────────────────┐
 │           internal/chplan — shared IR            │   Scan • Filter • Project •
 │  one algebra; the optimiser and the emitter      │   Aggregate • RangeWindow •
 │  don't know which head produced the plan         │   Limit • expression tree
 └──────────────────────────────────────────────────┘
                       │
                       ▼
 ┌──────────────────────────────────────────────────┐
 │          internal/optimizer — rule-based         │   Catalyst-style batches:
 │  Analyzer (semantic) → Once (heuristic) →        │   semantic + heuristic +
 │  FixedPoint (rules that unlock each other)       │   fixpoint rewrites; no cost
 │                                                  │   model (see performance.md)
 └──────────────────────────────────────────────────┘
                       │
                       ▼
        internal/solver — route decision           ← classifies the post-optimize
        (hooks the Optimizer.Run → chsql.Emit seam)   plan; PromQL anchor-fan-out
              │                       │                class routes B, else A
   route A (default)          route B (sharded-pushdown solver)
   one plan, one SQL          re-anchor K copies onto disjoint
              │               anchor slices; emit + execute each
              │                       │
              ▼                       ▼
 ┌─────────────────────────────────────────────────┐
 │           internal/chsql — typed emitter        │   • parameterised, escape-free
 │  QueryBuilder slots + typed Frag constructors;  │   • PREWHERE promotion on Filter(Scan)
 │  closed typed surface — no raw SQL. Route B     │   • sort-key-aware predicate ordering
 │  emits each shard byte-identically to route A.  │   • streaming clickhouse-go/v2 cursor
 └─────────────────────────────────────────────────┘
              │                       │
              ▼                       ▼  K statements, bounded parallelism +
   one ClickHouse statement   connection gate, concatenated behind one
              │               cursor (all-or-nothing wire contract)
              └───────────┬───────────┘
                          ▼
                      ClickHouse
```

### One IR for three languages — `internal/chplan`

A small algebra (`Scan`, `Filter`, `Project`, `Aggregate`,
`RangeWindow`, `Limit` + an expression tree) is the meeting point of
all three heads. The optimiser, the SQL emitter, and the engine work
over this IR; they don't know which head produced the plan. **New
optimisations cost one implementation, not three.**

“Shared” means head-agnostic, not backend-neutral. The algebra also carries
physical ClickHouse capability nodes such as `RangeWindowGridNative` and
`RangeWindowStaleResample`, plus a sealed function vocabulary whose symbols
resolve at the `chsql` boundary.

### A real rule-based optimiser — `internal/optimizer`

Catalyst- and DataFusion-style: rules are grouped into named batches,
each with one of three strategies — `Analyzer` (semantic, must-run,
idempotent — panics on contract violation), `Once` (a single pass), and
`FixedPoint(n)` (rules that unlock each other; iterates until no rule
reports a change or `n` iterations have elapsed). `optimizer.Default()`
builds the driver every head runs; its batches, in execution order, are:

| Batch                              | Strategy   | Rules                                                                                              | What it buys                                                                                                                                                                  |
| ---------------------------------- | ---------- | -------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `analyzer.constant-fold-semantic`  | Analyzer   | `ConstantFoldSemantic`                                                                             | Downstream rules can assume pure-literal subtrees have collapsed to a single `Lit`                                                                                            |
| `analyzer.scan-time-bound`         | Analyzer   | `NormalizeScanTimeBound`, `RequireScanTimeBound`, `RequireScanResourceBound`                       | Establishes and then fail-closes the instant scan time bound and the spans-scan resource bound (see the contracts below)                                                      |
| `optimizer.predicate-pushdown`     | FixedPoint | `ConstantFoldHeuristic`, `FilterFusion`, `FilterAggregateTranspose`, `FilterRangeWindowTranspose`  | Boolean identities (`true AND X → X`) fold, adjacent filters fuse, and filters move below aggregates / range windows so CH skip-indexes can fire on a `Scan`                  |
| `optimizer.projection`             | FixedPoint | `ProjectionPushdown`                                                                               | Late materialisation: the narrowed column set is pushed through `Aggregate` / `RangeWindow` to the inner `Scan`, so wide columns are only read after `LIMIT` cuts the row set |
| `optimizer.set-op-linearize`       | FixedPoint | `FlattenVectorSetOp`                                                                               | Collapses a left-assoc `a or b or c …` / `and` chain into one N-ary `NaryVectorSetOp` so the emitter scans each arm once under a single window pass instead of K nested ones  |

`ConstantFoldHeuristic` opens the predicate-pushdown batch and shares its
fixpoint because `FilterFusion` constructs new `Binary` predicates
(`p1 AND true`) that only exist once the batch is running. Nothing after
that batch constructs a `Binary`, so its fixpoint is also the point past
which no new foldable shape can appear. `test/regression` pins this
table to `Default()`.

`FilterAggregateTranspose` is retained as correctness insurance (0 fires
on the current corpus); every other rule fires on real queries.
`FlattenVectorSetOp` only flattens the associative `or` / `and`
operators — an `unless` chain keeps its binary shape. There is no
`FilterProjectTranspose` and no `MVSubstitution` rule.

The optimiser is gated by termination, decision-pin, rule-interaction,
property, and gremlins (mutation) tests.

#### Scan time-bound contract

The innermost per-sample read of an instant windowed range aggregation
(`rate` / `increase` / `*_over_time` / …) always carries a time
predicate, and that bound is an IR-level property rather than an
emitter-local one.

An instant windowed-array **leaf**
RangeWindow (`OuterRange == 0`, and `Input` is **not** a
`MetricsAggregate` / `MetricsHistogramOverTime` / `MetricsCompare`)
carries `RangeWindow.InstantScanBounded`, established once by
`chplan.AttachInstantScanTimeBounds` (run at the top of `chsql.Emit`) and
by the optimiser's must-run `analyzer.scan-time-bound` batch
(`NormalizeScanTimeBound` establishes, `RequireScanTimeBound`
fail-closes). The flag is the contract object; the predicate text is
rendered byte-identically by the emitters.

Two layers enforce it:

- **Plan-build** — `RequireScanTimeBound` panics (→ HTTP 500 via the
  panic-recovery middleware) if any instant windowed-array leaf reaches
  the end of the analyzer batch unmarked.
- **Emit** — every instant-leaf emit path (`emitWindowedArray` /
  `emitWindowedArrayPairsAnchored` / `emitWindowedArrayExtrapolated` via
  `pushInstantScanBound`, and the `emitRangeWindowOverTimeDirect` instant
  path via `requireInstantScanBound`) refuses to render an unbounded
  innermost scan.

**IR-verified paths** (governed by the contract above): every instant
`rate` / `irate` / `increase` / `delta` / `idelta` / `*_over_time`
(array and direct) / `ts_of_*_over_time` / `quantile_over_time` /
`deriv` / `resets` / `changes` / `holt_winters` / `predict_linear` /
`log_rate` leaf.

**Excluded paths** (bounded at emit time by their own mechanism, *not*
flagged in the IR — by design, not by default):

- Matrix shapes (`OuterRange > 0`) and the `MetricsAggregate` /
  `MetricsHistogramOverTime` / `MetricsCompare` emitters bound via
  `maybePushInnerScanTimeBounds` (gated on `Start && End`).
- `RangeLWR`, `RangeBucketFanout`, and `AbsentOverTime` are separate IR
  node types with their own `maybePushRangeScanTimeBound` / inner-scan
  bounds.
- The ClickHouse-native `timeSeries*ToGrid` family
  (`RangeWindowGridNative`, `RangeWindowStaleResample`) bounds its innermost read
  through the same `maybePushRangeScanTimeBound` helper. The family is
  pinned as a class by
  `internal/chsql/range_window_grid_native_scan_bound_test.go`, whose case
  list is driven by the emitter's own `nativeTSGridFn` registry, and the
  bound is rendered **per operand** of a vector-vector join.

#### Deferred label shaping on the native grid

`RangeWindowGridNative` renders in two levels by default: an aggregate level
that groups per series and computes `timeSeries<Fn>ToGrid`, and an outer
level that ARRAY JOINs the grid against its parallel timestamp axis. The
series key it groups on is the shaped Attributes map — on an OTel schema
that map is not a stored column but a `mapSort`/`mapConcat`/`mapUpdate`
tower over `Attributes`, `ResourceAttributes`, and `ServiceName`, so the
default shape evaluates the tower once per raw sample row.

A node carrying a non-empty `Recollapse` list renders in **three**
levels instead, moving that tower to once per raw series. The innermost
level groups on the RAW columns the tower reads and aggregates under the
`-State` combinator; the middle level computes each deferred expression
into a synthetic `shaped_key_<i>` column, groups on it, and folds the
partial states with `-Merge`; the outer level renames each
`shaped_key_<i>` to the output name it replaces and ARRAY JOINs as
before. The row shape reaching a wrapping `Aggregate` is identical
either way, so nothing downstream branches on which shape was emitted.

`Recollapse` is only populated for range functions whose
`-State`/`-Merge` pair is exact under merged states; every other node
passes an empty list and emits the two-level shape byte for byte.
Lowering owns the eligibility decision (`hoistShaping` in
`internal/promql`), the emitter owns the rendering, and
`docs/clickhouse-optimizations.md` covers the `ts_grid_recollapse`
capability gate.

#### Eval-grid carriers

Several IR nodes materialise an evaluation grid — the
`(Start, End, Step)` triple whose anchors become the emitted
timestamps. Consumers that need the request's outer grid (routing, cost
accounting, telemetry) discover it through the
`chplan.GridCarrier` interface rather than by enumerating node kinds.

`Step > 0` is the only range-vs-instant discriminator a consumer may
branch on; a consumer never enumerates node kinds.

The carrier set is closed in both directions.
`internal/chplan/grid_carrier.go` holds a compile-time list proving
every registered carrier implements the interface, and the completeness
ratchet in `grid_carrier_completeness_test.go` parses the package's own
source and fails when a struct declares the grid-field signature
without being registered. There is no allow-list: a new grid-bearing
node either joins the contract or turns that test red.

### Typed SQL — `internal/chsql`

Every emitted byte goes through a typed builder. Query shapes compose
through `QueryBuilder` slots (`.Select` / `.From` / `.Where` /
`.GroupBy` / `.OrderBy` / `.Limit` / `.Prewhere` / `.Join` /
`.WithRecursive`); expressions compose through typed `Frag`
constructors (`Eq`, `And`, `Or`, `Paren`, `Cast`, `In`, `Like`, `Add`,
`Call`, `Array`, `Subscript`, `If`, `Lambda1`, `Subquery`,
`BareIdent`, `InlineLit`, …). **External packages cannot produce raw
SQL by construction** — the typed Frag surface is closed, and adding a
new shape means adding a new typed constructor.

The emitter is also CH-native rather than ANSI-ish:

- **`PREWHERE` promotion** fuses `Filter(Scan)` into a single
  `SELECT … FROM <table> [PREWHERE …] WHERE …`, partitions conjuncts
  into a sort-prefix bucket / skip-index bucket / rest, and promotes
  cheap predicates that touch no wide column into `PREWHERE` when the
  projection reads any wide column.
- **`WITH RECURSIVE`** for label-set / trace-graph traversal.
- **Streaming `clickhouse-go/v2` cursor** — bounded RSS, no row buffer
  on the hot path; the engine's `QueryCursor` opens a streaming
  cursor when the underlying client implements `CursorQuerier`.

### Schema — drop-in OTel

Defaults to the
[OpenTelemetry ClickHouse Exporter](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/exporter/clickhouseexporter)
layout (`otel_metrics_*`, `otel_logs`, `otel_traces`). The DDL templates
come from the
[`tsouza/opentelemetry-collector-contrib:cerberus-ddl`](https://github.com/tsouza/opentelemetry-collector-contrib/tree/cerberus-ddl)
fork of the exporter. Runtime YAML and environment
overrides cover the five metrics table names, the logs and spans table names,
the traces timestamp-lookup toggle, and the Prometheus resource-label list.
They do not provide a SigNoz preset or arbitrary column-name mapping.

## Adding a new query head

To add a fourth query head, three pieces are needed:

1. **Implement the `Lang` interface.** Put the parser type, the
   lowering function, and the per-language wrap-projection
   behind one struct that satisfies
   `Name() / Parse() / ProjectSamples()`. Follow the existing
   adapters as templates:
   - `internal/api/prom/lang.go` — PromQL.
   - `internal/logql/lang.go` — LogQL.
   - `internal/api/tempo/lang.go` — TraceQL.

   The adapter is responsible for opening its own `parse` and
   `lower` spans (via `cerbtrace.SpanParse` / `SpanLower`) so
   the trace shape stays consistent.
2. **Write a handler.** The handler owns HTTP routing, request
   parsing, adapter construction, and the call into
   `Engine.Query` or `Engine.QueryPlan`. It also formats the
   returned `Result.Samples` into the upstream wire shape. Mirror
   the shape of `internal/api/prom/handler.go` or
   `internal/api/loki/handler.go`.
3. **Wire it in `cmd/cerberus/main.go`.** Construct the head's
   `Engine` — its own `optimizer.Default()` driver, the ClickHouse
   client shared with the other heads — and register the handler
   against its URL prefix on the HTTP mux.

The engine itself does not need to change — `Lang` is the
extension point.

## Response headers

The engine populates `Result.CHMillis`, `Result.PlanNodeCount`,
and `Result.Headers` so the handler can stamp them onto the HTTP
response. The contract is:

| Header                      | Source                                | Meaning                                                                                                                                                                                                                                                                          |
| --------------------------- | ------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `X-Cerberus-CH-Millis`      | `Result.CHMillis`                     | Wall-clock milliseconds spent inside `Client.Query` (the ClickHouse roundtrip).                                                                                                                                                                                                  |
| `X-Cerberus-Plan-Nodes`     | `Result.PlanNodeCount`                | Node count of the optimised plan that produced the executed SQL.                                                                                                                                                                                                                 |
| `X-Cerberus-Strategy`       | `Result.Headers[HeaderStrategy]`      | Execution-family label computed by `strategyFor(meta)`: `native` or `trace-by-id`.                                                                                                                                                                                               |
| `X-Cerberus-Route-Decision` | `Result.Headers[HeaderRouteDecision]` | Stamped only when a `Solver` is wired and classified the plan (PromQL head); omitted otherwise. `<strategy>;reason=<reason>` — `route-a;reason=…` on a non-route, `sharded-timeslice;k=<K>;reason=…` on a route. Observational: never changes the body or `X-Cerberus-Strategy`. |

Handlers stamp these headers from `Result` (or via the chclient
millisecond counter where a per-request middleware is in play).
Tests assert their presence — they are part of the wire contract,
not an internal detail.

Individual heads add their own headers where an upstream wire format
has no field for a number cerberus needs to report. The Tempo head's
`/api/search` stamps one:

| Header                       | Source                   | Meaning                                                                             |
| ---------------------------- | ------------------------ | ----------------------------------------------------------------------------------- |
| `X-Cerberus-Inspected-Spans` | `tempo.SearchMetricsFor` | Span ROWS drained from ClickHouse to answer the search — the resource-bound signal. |

`SearchMetrics.InspectedTraces` in the response body counts distinct
**traces** (upstream Tempo's semantics); the header counts span rows.
The gRPC `StreamingQuerier.Search` RPC reports the span count on an
identically-named trailer.

## Extension points

Beyond `Lang`, the engine has seven optional runtime seams, each a
pointer or interface field whose `nil` preserves the ordinary
single-statement path: `Solver` (route classification and sharded
execution), `RouteMemo` (failure-driven route selection),
`PerRungAdmission` (evidence-based refinement of the solver's per-rung
admission), `ScanEstimateAdvisor` and `CardinalityProbeAdvisor` (advisory
pre-flights that feed the solver), `Actuals` (predicted-vs-actual drift
tracking), and `QueryObserver` (dispatch and outcome events for the
performance corpus). `Settings` plus `SetSettings` apply plan-gated
ClickHouse settings and permit an atomic capability refresh.

### OTel hooks

The engine takes `context.Context` end-to-end and emits a
pipeline-stage span at each boundary:

```text
parent HTTP span
└─ parse        (opened by Lang.Parse)
└─ lower        (opened by Lang.Parse)
└─ optimize     (engine; skipped when Meta.IsTraceByID)
└─ emit         (engine)
└─ execute      (engine; closed on Client.Query return — or on
                 Cursor.Close() for the streaming path)
```

Span names are the constants in `internal/cerbtrace`. The stopwatch
around each stage is the same `telemetry.ObserveStage` helper, taking
the language alongside the stage
(`telemetry.ObserveStage(telemetry.StageEmit, lang.Name())`), so the
OTel span tree and the cerberus stage-duration histograms stay aligned.
Cross-cutting hooks (request-id propagation, query-budget enforcement,
per-tenant quotas) plug into the same context.

For the full OTel setup — exporters, env vars, dashboards — see
[`observability.md`](observability.md).

## What the engine is not

- **Not a query plan cache.** Plans are recomputed per request.
  The engine has no LRU, no memoisation, no plan store.
- **Not a result cache.** Cursor routes stream rows to the handler; eager
  routes drain rows into the per-request `Result.Samples` slice. Neither
  retains results beyond that request.
- **Not a router.** URL → endpoint dispatch stays in the
  handlers; the engine sees a request only after the handler has
  decided which entry point to call.
- **Not the owner of every ClickHouse statement.** It owns the shared
  language-query pipeline. Endpoint-specific metadata, discovery, live-tail,
  and enrichment statements remain in the API packages listed above.
- **Not a translator of wire formats.** The handler formats
  engine results or dedicated endpoint rows into the upstream wire shape;
  the engine never touches `http.ResponseWriter`.
- **Not a streaming subquery reducer.** A PromQL subquery
  `<reducer>_over_time(<inner>[range:step])` materialises
  `range/step + 1` anchor rows per series before collapsing them.
  `requireSubquerySampleBudget` (`internal/engine/anchor_budget.go`)
  measures one series' anchor grid against `Config.MaxQuerySamples` and
  returns the same Prom-shaped 422 upstream Prometheus returns once a
  subquery would load more than `query.max-samples` into memory.

---

For the rationale behind these choices — alternatives considered, incidents,
measurements — see [engine.background.md](engine.background.md).
