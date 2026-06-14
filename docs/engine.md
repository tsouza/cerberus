# Engine

The query engine — `internal/engine/` — is the shared pipeline that
turns an upstream query (PromQL / LogQL / TraceQL) into a
ClickHouse result. The three HTTP heads (Prometheus, Loki, Tempo)
each plug in as a `Lang` adapter; the engine owns the parse →
optimize → emit → execute loop and the telemetry around it.

## Overview

Cerberus has one shared query pipeline, not three. Each query
language has its own parser and its own response shape, but the
work in the middle — lowering to the shared plan IR, optimizing,
emitting ClickHouse SQL, executing against the driver — is
identical. The engine extracts that middle so the heads stay thin:
HTTP dispatch + per-language adapter wiring + response shaping.

```text
   HTTP request
        │
        ▼
   handler (api/{prom,loki,tempo})
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

## The Engine + Lang contract

The engine is two types and one interface.

### `Engine`

```go
type Engine struct {
    Optimizer *optimizer.Driver
    Client    Querier
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

One Engine instance is constructed per HTTP head in
`cmd/cerberus/main.go` and lives for the lifetime of the process.

### `Lang`

```go
type Lang interface {
    Name() string
    Parse(ctx context.Context, query string) (chplan.Node, Meta, error)
    ProjectSamples(plan chplan.Node, meta Meta) chplan.Node
}
```

- `Name()` returns a stable identifier — `"promql"`, `"logql"`,
  `"traceql"`. The engine threads it onto progress-context keys
  and telemetry labels.
- `Parse` runs the upstream parser, lowers the AST into a
  `chplan` tree, and returns the plan plus a `Meta` value. The
  adapter is also responsible for opening the `parse` / `lower`
  pipeline-stage spans so the trace shape is consistent across
  heads.
- `ProjectSamples` wraps the plan with whatever projection the
  adapter needs so that the executed SQL emits rows in the
  canonical `chclient.Sample` shape — `(MetricName, Attributes,
  TimeUnix, Value)`. Each head's per-shape switch
  (canonical / derived / structural-join) lives in the adapter,
  not in the engine.

### `Meta`

```go
type Meta struct {
    IsMetric      bool
    IsTraceByID   bool
    ResponseShape string
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
- `ResponseShape` — handler-side pivot key
  (`"prom-vector"`, `"loki-matrix"`, `"loki-streams"`,
  `"tempo-trace"`, …). The engine does not read it; it is
  threaded through `Result` so the response formatter does not
  have to re-derive it.
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
}
```

- `Samples` is the decoded row stream. Handlers pivot it into the
  upstream wire shape.
- `SQL` + `Args` are surfaced for debug logging.
- `Strategy` is a free-form label for the execution path taken —
  empty in the default path; see [Extension points](#extension-points).
- `CHMillis` is the wall-clock time spent in `Client.Query`,
  exposed through the `X-Cerberus-CH-Millis` response header.
- `PlanNodeCount` is the optimised plan's node count, exposed
  through `X-Cerberus-Plan-Nodes`.
- `Headers` is a bag of extra response headers the engine wants
  the handler to stamp on the response — keeps the engine free
  of `http.ResponseWriter`.
- `Meta` is the same `Meta` the adapter returned, threaded
  through so the response pivot can switch on it.

A streaming sibling — `CursorResult` — mirrors this shape but
carries a `chclient.Cursor` instead of a `[]Sample` slice. The
caller is responsible for `cursor.Close()`.

## Request lifecycle

A typical request flows through the following stages:

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
   1. `lang.Parse` runs the upstream parser and lowers to
      `chplan`. Opens `parse` + `lower` spans.
   2. `lang.ProjectSamples` wraps the plan into the canonical
      `Sample` row shape.
   3. The optimizer runs (skipped when `Meta.IsTraceByID` is
      set). Opens an `optimize` span.
   4. `chsql.Emit` materialises the plan into parameterised
      ClickHouse SQL. Opens an `emit` span.
   5. `Client.Query` executes the SQL. Opens an `execute` span;
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

## Pipeline stages in depth

The middle of the pipeline — lower → optimize → emit → schema
resolution — is the part the three heads share. Each stage is
described below. The three heads converge on it: each parses with its
reference upstream parser, lowers to the shared IR, and runs the same
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
prometheus/        grafana/loki/v3/         grafana/tempo/
promql/parser      pkg/logql/syntax         pkg/traceql        ← reference upstream parsers, imported directly
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
 ┌────────────────────────────────────────────────┐
 │           internal/chsql — typed emitter        │   • parameterised, escape-free
 │  QueryBuilder slots + typed Frag constructors;  │   • PREWHERE promotion on Filter(Scan)
 │  closed typed surface — no raw SQL. Route B      │   • sort-key-aware predicate ordering
 │  emits each shard byte-identically to route A.  │   • streaming clickhouse-go/v2 cursor
 └────────────────────────────────────────────────┘
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

### A real rule-based optimiser — `internal/optimizer`

Catalyst- and DataFusion-style: rules are grouped into batches with
three strategies — `Analyzer` (semantic, must-run, idempotent — panics
on contract violation), `Once` (idempotent heuristics, single pass),
and `FixedPoint(n)` (rules that unlock each other; iterates until no
rule reports a change or `n` iterations have elapsed). The default
pipeline ships:

| Stage                            | Rules                                                                    | What it buys                                                                          |
| -------------------------------- | ------------------------------------------------------------------------ | ------------------------------------------------------------------------------------- |
| Analyzer — pure-literal fold     | `ConstantFoldSemantic`                                                   | Downstream rules can assume pure-literal subtrees have collapsed to a single `Lit`    |
| Once — heuristic fold            | `ConstantFoldHeuristic`                                                  | Boolean identity simplification (`true AND X → X`, `false OR X → X`)                  |
| FixedPoint — predicate pushdown  | `FilterFusion`, `FilterAggregateTranspose`, `FilterRangeWindowTranspose` | Filters move below aggregates / range windows so CH skip-indexes can fire on a `Scan` |
| FixedPoint — projection pushdown | `ProjectionPushdown`                                                     | Late materialisation: wide columns are only resolved after `LIMIT` cuts the row set   |

`FilterAggregateTranspose` is retained as speculative correctness
insurance (0 fires on the current corpus); `FilterRangeWindowTranspose`,
`FilterFusion`, `ConstantFoldHeuristic`, and `ProjectionPushdown` all
fire on real queries. The rule set carries only rules that can fire:
there is no `FilterProjectTranspose` (no lowering emits `Filter(Project)`,
so the rule would never match) and no `MVSubstitution` (the default schema
ships no live rollups, so a substitution rule would be a guaranteed
no-op).

The optimiser is gated by termination, decision-pin, rule-interaction,
property, and gremlins (mutation) tests.

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
layout (`otel_metrics_*`, `otel_logs`, `otel_traces`). The schema
source of truth is the upstream OTel-CH exporter via the
[`tsouza/opentelemetry-collector-contrib:cerberus-ddl`](https://github.com/tsouza/opentelemetry-collector-contrib/tree/cerberus-ddl)
fork — cerberus consumes the same DDL templates the production
exporter emits, so a deployment where the exporter writes and cerberus
reads sees one schema across both sides. A thin YAML override config
supports SigNoz schemas and custom column layouts.

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
   `Engine` (sharing the optimizer driver and ClickHouse client
   with the other heads) and register the handler against its
   URL prefix on the HTTP mux.

The engine itself does not need to change — `Lang` is the
extension point.

## Response headers

The engine populates `Result.CHMillis`, `Result.PlanNodeCount`,
and `Result.Strategy` so the handler can stamp them onto the HTTP
response. The contract is:

| Header                  | Source                  | Meaning                                                                                  |
| ----------------------- | ----------------------- | ---------------------------------------------------------------------------------------- |
| `X-Cerberus-CH-Millis`  | `Result.CHMillis`       | Wall-clock milliseconds spent inside `Client.Query` (the ClickHouse roundtrip).          |
| `X-Cerberus-Plan-Nodes` | `Result.PlanNodeCount`  | Node count of the optimised plan that produced the executed SQL.                         |
| `X-Cerberus-Strategy`   | `Result.Strategy`       | Free-form label identifying the execution path. Empty in the default direct-table path.  |

Handlers stamp these headers from `Result` (or via the chclient
millisecond counter where a per-request middleware is in play).
Tests assert their presence — they are part of the wire contract,
not an internal detail.

## Extension points

The engine has two designed-in extension points beyond the `Lang`
interface.

### Strategy switch

`Result.Strategy` is a free-form label that names the execution
path taken. The default path leaves it empty. An optimizer rule
that rewrites the plan to read from a materialised view, a
pre-aggregated table, or any other alternative storage can stamp
the strategy by setting it on a marker node the engine reads back
after the optimizer pass. This is the seat for future
fallback-evaluator work — adding a new strategy means writing the
rule that detects when it applies and the label that identifies
it, without touching the engine's loop.

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

Span names are the constants in `internal/cerbtrace`. The
stopwatch around each stage is the same `telemetry.ObserveStage`
helper, so the OTel span tree and the cerberus stage-duration
histograms stay aligned. New cross-cutting hooks (request-id
propagation, query-budget enforcement, per-tenant quotas) plug
into the same context — no engine surface change required.

For the full OTel setup — exporters, env vars, dashboards — see
[`docs/observability.md`](observability.md).

## What the engine is not

A short list, because the engine's narrow scope is deliberate:

- **Not a query plan cache.** Plans are recomputed per request.
  The engine has no LRU, no memoisation, no plan store.
- **Not a result cache.** Result rows are streamed straight from
  the ClickHouse driver to the handler. Cerberus is a gateway,
  not a memoisation layer.
- **Not a router.** URL → endpoint dispatch stays in the
  handlers; the engine sees a request only after the handler has
  decided which entry point to call.
- **Not a translator of wire formats.** The handler formats
  `Result.Samples` into the upstream wire shape; the engine never
  touches `http.ResponseWriter`.

These boundaries keep the engine's surface small enough that
adding a new query head — or a new extension point — is a local
change rather than a refactor.
