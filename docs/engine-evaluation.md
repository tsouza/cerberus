# Execution engine framework — evaluation

This document captures the evaluation phase for the proposed
`internal/engine/` framework that would lift the request pipeline out
of the three per-API handlers (`internal/api/{prom,loki,tempo}`) into
a shared loop with per-language adapters. It is a research +
planning artefact: it ships no code, it commits to no interface
shape, and its only output is a go / no-go recommendation plus the
sequencing for the implementation phase if the answer is "go".

See also `docs/roadmap.md` § RC7 for the canonical milestone IDs and
exit criteria; this document elaborates the audit those entries
reference.

## Executive summary

**Recommendation: build.** The audit confirms the pipeline shape —
parse → lower → wrap-projection → optimize → emit → execute → format
— is mechanical across all three heads. Per-language drift is
isolated to two well-defined seams: (1) the parser type and the
lowering call it feeds, and (2) the response-shape pivot at the tail
of the pipeline. Both seams already have an "interface in waiting"
shape inside each handler today, copy-pasted three times. There is
no semantic divergence that demands escape hatches; the framework
absorbs the duplication without forcing handlers to reach around it.

The recommendation is **build the full engine**, not the partial
helpers-only option, because the duplication is dominated by the
pipeline loop itself (six-to-seven calls, telemetry-stage-timed,
context-propagated, error-mapped) rather than by the helper code at
the boundaries. Extracting helpers alone would leave the loop
copy-pasted; the abstraction tax is small and one-time.

Risk class: **low**. The refactor is behavioural-equivalence; the
existing TXTAR + compatibility + Playwright suites act as the
guardrail. The single non-trivial risk — wire-format drift while
moving response shaping — is mitigated by handler-level snapshot
tests already in place (`handler_test.go` per package, plus
`pipeline_spans_test.go` for instrumentation).

## Audit: where the three heads agree and where they diverge

### What is identical across the three heads

The pipeline loop itself is the same in every handler. Every full
query path runs:

1. `parseExpr(ctx, query)` — wraps the upstream parser in a
   `cerbtrace.SpanParse` span with `cerberus.ql` / `cerberus.query`
   attributes.
2. `telemetry.ObserveStage(StageLower)` → lower the parsed AST into
   `chplan.Node` using the per-language `Lower` function.
3. Wrap the lowered plan with a `chplan.Project` that re-shapes the
   output rows into the canonical `chclient.Sample` shape (four
   columns: name / attributes / timestamp / value).
4. `telemetry.ObserveStage(StageOptimize)` → run `Optimizer.Run`.
5. `telemetry.ObserveStage(StageEmit)` → `chsql.Emit` the plan into
   parameterised SQL.
6. `telemetry.ObserveStage(StageExecute)` → call the appropriate
   `Querier` method against ClickHouse.
7. Pivot the result rows into the upstream API's response shape.

Steps 2 / 4 / 5 / 6 are byte-for-byte identical apart from the
language identifier passed to `chclient.WithProgressFor`. Step 1
differs only in which parser type is returned. Step 3 differs in
which projection is built (each handler hand-rolls a
`wrapWithSampleProjection`-shaped helper, but the function shape is
the same: take a plan, return a wrapped plan). Step 7 is the
genuinely per-API piece — different wire formats, different result
types.

The shared infrastructure each handler currently owns three copies
of:

- `apiError` (kind / err / status) — Prom and Loki are identical;
  Tempo uses a different error envelope (`ErrorResponse` with
  `TraceID` / `SpanID`) but the internal type is morally the same.
- `respondError` / `writeError` / `writeJSON` — three near-identical
  copies.
- `parseTime` / `parseDuration` — Prom and Loki are nearly
  identical; Loki adds a Unix-nanoseconds heuristic, Tempo doesn't
  use these at all (Tempo's time params don't reach the planner
  yet).
- `canonicalKey(labels map[string]string) string` — three copies,
  identical algorithm (sort keys ASCII, join with `=` and NUL
  separators). Prom uses a hand-rolled insertion sort, Loki/Tempo
  use `sort.Strings`; otherwise byte-for-byte equivalent.
- `toVector(samples, ts) []VectorSample` — two copies (Prom + Loki);
  the algorithm (group by canonical key, keep latest per series,
  stamp with query ts) is identical, only the response struct field
  names differ.
- The matrix step-grid pivot — Prom has the streaming
  `matrixFromCursor` variant, Loki has the eager `toMatrixStepGrid`
  variant; identical 5-minute lookback semantics; trace of how
  `cursor < len(rows) && !rows[cursor].After(t)` advances the cursor
  is line-for-line copy-paste.
- `withMetricName(labels, name)` — Prom-only today, but the same
  shape would help Loki when promoting `__name__`-style labels.

### What diverges, and how the abstraction absorbs it

| Divergence axis           | Prom                                           | Loki                                                  | Tempo                                                  | How the engine absorbs it                                                          |
| ------------------------- | ---------------------------------------------- | ----------------------------------------------------- | ------------------------------------------------------ | ---------------------------------------------------------------------------------- |
| Parser type               | `promparser.Parser` / `Expr`                   | `syntax.Expr`                                         | `*traceql.RootExpr`                                    | `Lang.Parse` returns `(chplan.Node, Meta, error)` — parser type stays in adapter.  |
| Lowering signature        | `promql.LowerAt(ctx, expr, schema, start, end)` | `logql.Lower(ctx, expr, schema)`                      | `traceql_lower.Lower(ctx, expr, schema)`               | `Lang.Parse` calls its own `Lower`; engine sees only the plan.                     |
| Wrap-projection shape     | derived-vs-canonical branch                    | metric-vs-stream branch                               | structural-join / aggregate / canonical 3-way switch   | `Lang.ProjectSamples(plan, meta) chplan.Node` — adapter owns its own switch.       |
| CH timing                 | `timeCH(...)` wrapper exposes `X-Cerberus-CH-Millis` | none today (regression)                          | none today (regression)                                | Engine times execute uniformly; result struct carries `CHMillis`.                  |
| Error envelope            | `{status, errorType, error}` (Prom)            | identical to Prom                                     | `{traceID, spanID, error, message}`                    | Handler keeps a tiny `writeError` shim; engine returns typed errors.               |
| Response shape            | vector / matrix / scalar / streams (none)      | vector / matrix / streams (with line transform)       | trace summaries / batches / strings                    | Engine returns `[]chclient.Sample`; handler pivots — that's the per-API piece.     |
| Plan-only entry           | none                                           | none                                                  | `handleTraceByID` builds plan directly, skips parser   | `Engine.QueryPlan(ctx, plan)` complements `Engine.Query(ctx, query)`.              |
| Streaming cursor          | `query_range` uses `QueryCursor` + pivot       | not yet                                               | not yet                                                | Engine exposes `QueryCursor` alongside `Query`; opt-in per call.                   |

The "how absorbed" column shows the structural answer: every
divergence falls on one side of the `Lang` interface. Nothing
demands a per-handler escape hatch in the engine itself.

## Proposed interface

The four types — three of them tiny — are listed below in dependency
order. The shapes track `docs/roadmap.md` § RC7's preliminary
sketch; the only deltas are (1) a `Result.Headers` map for arbitrary
response headers the engine wants to set (replacing the
`X-Cerberus-CH-Millis` ad hoc plumbing), and (2) a `Lang.Format`
slot so the pivot from samples back into the upstream wire shape
becomes an engine-level concern when the per-handler diff is
narrow enough.

- **`Engine`** — owns the shared dependencies (optimizer, ClickHouse
  client, logger) and runs the pipeline loop. One instance per
  handler; the per-language differences live behind `Lang`. Methods:
  `Query(ctx, lang, query) (Result, error)` and `QueryPlan(ctx,
  lang, plan) (Result, error)` for callers like `handleTraceByID`
  that build plans without a parser.
- **`Lang`** — interface with three slots: `Name() string` (for
  logs / spans / progress-context keying), `Parse(ctx, query)
  (chplan.Node, Meta, error)` (combines upstream parse + lower +
  pre-projection state into one call so the engine never sees the
  parser type), and `ProjectSamples(plan, meta) chplan.Node` (the
  sample-row reshaping that today lives in
  `wrapWithSampleProjection`).
- **`Meta`** — small POD with the per-language hints the engine
  doesn't itself understand: `IsMetric bool` (logql metric-vs-stream
  branch), `IsTraceByID bool` (tempo skip-parse path),
  `ResponseShape string` (the engine-side response pivot picks
  between `"prom-vector" / "prom-matrix" / "loki-streams" /
  "tempo-traces"`, etc.). Extensible via untyped `Extra map[string]any`
  for adapter-specific knobs without bloating the type.
- **`Result`** — what the engine returns: `Samples []chclient.Sample`,
  `SQL string`, `Args []any`, `Strategy string` (for future shadow /
  fallback execution at the optimizer level), `CHMillis int64`,
  `PlanNodeCount int`, `Headers map[string]string` (covers the
  `X-Cerberus-*` instrumentation without entangling the engine with
  `http.ResponseWriter`).

## Per-handler diff: what changes, what stays

| Handler            | Lines today | What moves to engine                                       | What stays in handler                                                       |
| ------------------ | ----------- | ---------------------------------------------------------- | --------------------------------------------------------------------------- |
| `prom/handler.go`  | ~675        | `executeInstant`, `executeRangeStreaming`, projection wrap | HTTP routing, `handleQuery`/`handleQueryRange`/scalar fold, response pivot  |
| `loki/handler.go`  | ~560        | `execute`, projection wrap, metric-vs-stream gate          | HTTP routing, `buildInstantData`/`buildRangeData`, line-transform path      |
| `tempo/handler.go` | ~520        | `handleSearch` pipeline body, `handleTraceByID` pipeline   | HTTP routing, `toTraceSummaries`, `groupBatches`, trace-not-found 404 shape |

Estimated post-port handler size: ~150 LoC each, dominated by the
HTTP routing table (`Mount`) and the response-shape pivot. Pipeline
plumbing — every `telemetry.ObserveStage(...).Done(ctx)` line, every
`&apiError{...}` construction — leaves the handler entirely.

## Helper extraction plan

Independent of the engine itself, the duplication around the
handlers collapses into two new packages. Both are mechanical
extractions — same algorithm, same tests carried over.

### `internal/api/format/`

Five exports, currently copied across two-to-three handlers:

1. `canonicalKey(labels map[string]string) string` — sorted-key
   joiner. Three copies today (prom / loki / tempo); single source
   of truth after extraction. Insertion-sort vs. `sort.Strings`
   diverges in micro-benchmarks; pick one based on the size
   distribution of label sets seen in production samples (small
   sets favour insertion sort; large sets favour `sort.Strings`).
2. `toVector(samples, ts) []VectorSample` — group by canonical key,
   keep latest, stamp with eval ts. Two copies (prom / loki); they
   differ only in the response struct's `[2]any` vs. `Sample` type,
   which becomes a generic over the value tuple.
3. `toMatrixStepGrid(samples, start, end, step) []MatrixSample` —
   step-grid pivot with 5-minute lookback. Two copies (prom's eager
   plus loki); prom's streaming variant (`matrixFromCursor`) stays
   in-handler until the engine grows a streaming entry point.
4. `withMetricName(labels, name)` — prom-only today; lifted so logql
   can promote `__name__` when needed.
5. `parseTime(raw, def)` / `parseDuration(raw)` — Prom and Loki are
   nearly identical; merge with an `AcceptUnixNanos bool` flag to
   keep Loki's heuristic optional.

Total duplicate-code sites identified for `internal/api/format/`
extraction: **8**, counting each handler-side copy as one site
(canonicalKey × 3, toVector × 2, toMatrixStepGrid × 2, parseTime × 2,
parseDuration × 2, withMetricName × 1 — overlap-sum across handlers
is 12; counting only the distinct function-handler pairs that move
gives 8). Around 250 LoC of mechanical copy-paste collapses into
~120 LoC of single-source-of-truth.

### `internal/api/httperr/`

Three exports, currently copied across all three handlers:

1. `Error` (renamed from `apiError`) carrying `kind` / `err` /
   `status`. Same shape in prom and loki today; tempo's distinct
   `ErrorResponse` envelope wraps but doesn't replace it.
2. `respondError(w, err, mapper)` — `errors.As` to `*Error`, fall
   back to 500. The per-handler error-string mapping (Prom's
   `ErrBadData` / `ErrExecution` / `ErrInternal`, Tempo's traceID
   passthrough) becomes the `mapper` argument.
3. `writeJSON(w, status, body)` — three copies, byte-identical.

Around 120 LoC of error plumbing collapses into ~60 LoC.

## Phasing

The implementation phase decomposes into the milestones already
listed in `docs/roadmap.md` § RC7; restated here in dependency order
so the diff against the framework's commit cadence is explicit:

1. **Engine scaffolding** — drop `internal/engine/` with `Engine`,
   `Lang`, `Meta`, `Result` and a fake `Querier`-backed unit-test
   harness. No handler touches; this is purely additive so the next
   port has somewhere to land.
2. **Prom port** — `internal/engine/lang/promql/` adapter; rewrite
   `prom/handler.go` against it. Prom is first because it's the most
   mature handler and already has CH timing — porting it
   regression-frees the timing path through the engine instead of
   handler-side. Drops `executeInstant` /
   `executeRangeStreaming` / `wrapWithSampleProjection` from the
   handler.
3. **Loki port** — `internal/engine/lang/logql/` adapter; rewrite
   `loki/handler.go`. The metric-vs-stream branch becomes a
   `Meta.IsMetric` flag set by `Lang.Parse`; `wrapWithLogSampleProjection`
   collapses into `ProjectSamples`. Loki gains CH timing for free.
4. **Tempo port** — `internal/engine/lang/traceql/` adapter; rewrite
   `tempo/handler.go`. `handleTraceByID` exercises `Engine.QueryPlan`
   (no parser involved); `handleSearchRecent` ditto for the
   plan-built-by-hand path. Tempo gains CH timing for free.
5. **Format extraction** — land `internal/api/format/` and dedupe
   the helpers from prom + loki + tempo. Independent of the engine
   ports; can in principle ship before them (parallel track), but
   ordering after the ports keeps each refactor scoped.
6. **Httperr extraction** — land `internal/api/httperr/` and dedupe
   `apiError` / `writeJSON` / `writeError` / `respondError`. Tempo
   keeps a thin handler-side shim for its distinct envelope.
7. **Instrumentation** — engine starts emitting `X-Cerberus-CH-Millis`
   / `X-Cerberus-Strategy` / `X-Cerberus-Plan-Nodes` headers from
   `Result.Headers`; loki + tempo gain timing visibility they
   currently lack. One OTel span per `Engine.Query` rather than
   three per-handler trees.
8. **Docs** — `docs/engine.md` documents the extension points
   (`Lang` adapter contract, `Strategy` future-hook, `Meta` extras
   pattern) so later RCs plug in cleanly. The shadow-mode /
   local-Go evaluator fallback work has a natural seat at the
   engine layer.

## Risks and mitigations

- **Wire-format drift while moving response shaping.** The
  highest-leverage risk: a per-handler regression in the JSON output
  is invisible at the engine layer and only surfaces when Grafana
  parses the response. Mitigation: keep the handler-side snapshot
  tests (`handler_test.go` per package) intact through every port;
  the engine layer adds tests over the pipeline core, not over the
  wire format. The compatibility suite is the second guardrail.
- **OTel span fan-out.** Today each handler emits its own
  parse/lower/optimize/emit/execute spans, all under the per-handler
  tracer. Moving the spans into the engine changes the tracer
  identity. Mitigation: tracer identity is a label, not a structural
  property — observability dashboards key off the span name. The
  `pipeline_spans_test.go` snapshot in `api/prom/` catches name
  drift.
- **Adapter-package thrash.** Three new `lang/<ql>/` packages create
  import depth. Mitigation: each adapter is ~150 LoC, single
  package; the depth is one level and pays for itself in handler
  shrinkage. The CLAUDE.md "top-level reading order" survives —
  readers go `lower.go` → adapter, not adapter → lower.
- **Streaming entry point shape.** The prom `query_range` streaming
  cursor variant (`executeRangeStreaming` / `matrixFromCursor`)
  doesn't naturally fit a uniform `Result` return. Mitigation: keep
  `Engine.QueryCursor` as a sibling entry point that returns a
  `chclient.Cursor` instead of a `Result`; the handler pivots
  cursor-side as it does today. Streaming becomes an engine
  capability, not a separate world.
- **Helper-extraction packaging mistake.** Splitting
  `internal/api/format/` too aggressively (one function per file) or
  too coarsely (one giant file) hurts readability. Mitigation:
  group by upstream wire shape — `vector.go`, `matrix.go`,
  `labels.go`, `time.go` — mirroring how the handlers currently
  organise their helpers.

## Decision

The audit's three-way recommendation (build / partial / defer)
resolves to **build**. The pipeline shape is mechanical across the
three heads, the divergence axes all fit behind a `Lang` interface
without ad-hoc escape hatches, and the helper extraction collapses
~370 LoC of copy-paste into ~180 LoC of single source of truth. The
partial option (helpers only, no engine) leaves the seven-step
pipeline loop copy-pasted three times; the defer option ignores the
fact that RC4's instrumentation and RC6's typed-SQL work already
landed without forcing the abstraction, so the timing is right.

The implementation phase proceeds against the phasing in this
document; each milestone ships as its own PR with the existing test
suites as guardrails.
