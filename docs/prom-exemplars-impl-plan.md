# `/api/v1/query_exemplars` — implementation plan (OTel-CH Exemplars column)

Investigation-only doc. Plans the follow-up PRs that turn cerberus's currently
stubbed PromQL exemplars endpoint (`internal/api/prom/exemplars.go` returns
`{"status":"success","data":[]}`) into a real query that reads the
`Exemplars` Nested column the OTel ClickHouse Exporter writes on its
`otel_metrics_{gauge,sum,histogram,exp_histogram}` tables.

## 1 — Current placeholder shape

The placeholder at `internal/api/prom/exemplars.go` validates inputs but
intentionally returns an empty data array, then documents the gap inline.

**Input validation that already lands today:**

- `query` form param is required (`writeError(StatusBadRequest, ErrBadData)` on
  empty), and is passed through `h.parseExpr(ctx, query)` — the same
  Prometheus parser the `/query` and `/query_range` handlers use. A malformed
  PromQL string therefore returns the standard error envelope before any
  ClickHouse hop.
- `start` and `end` are parsed via `format.ParseTimeProm(...)` with a zero
  default, so missing / unparseable / zero values all surface as
  `"missing or invalid 'start'/'end' parameter"`.
- `end.Before(start)` returns `"'end' must be after 'start'"`.

**Output shape on the success path today:**

```go
writeJSON(w, http.StatusOK, Response{
    Status: "success",
    Data:   []ExemplarSeries{},
})
```

`ExemplarSeries` and `Exemplar` are already declared at the top of
`exemplars.go` with the right JSON tags:

```go
type ExemplarSeries struct {
    SeriesLabels map[string]string `json:"seriesLabels"`
    Exemplars    []Exemplar        `json:"exemplars"`
}

type Exemplar struct {
    Labels    map[string]string `json:"labels"`
    Value     float64           `json:"value"`
    Timestamp float64           `json:"timestamp"`
}
```

This matches the upstream Prom contract verbatim
(<https://prometheus.io/docs/prometheus/latest/querying/api/#querying-exemplars>):

```json
{
  "status": "success",
  "data": [
    {
      "seriesLabels": {"__name__": "metric", ...},
      "exemplars": [
        {"labels": {"trace_id": "...", "span_id": "..."},
         "value": 1.23, "timestamp": 1234567890.123}
      ]
    }
  ]
}
```

— so the only gap is the empty-array short-circuit. The wire-shape work is
already done; the implementation is purely "produce non-empty data".

**Gaps the implementation closes:**

1. The handler does not yet extract the `*parser.VectorSelector` from the
   parsed expression — it discards the AST after the well-formed check. The
   real path needs the matcher list and the metric-name matcher (`__name__`)
   to pick the target table.
2. There is no SQL emitter for a Nested-column `arrayJoin` shape; the
   existing `internal/chsql/exemplars.go` emits the Tempo `otel_traces`
   exemplars (one representative span per (series, bucket) anchor), which is
   a different schema and a different shape — see § 4 for the contrast.
3. The handler does not route per-metric-family. PromQL exemplars must
   target sum, gauge, histogram, OR exp_histogram tables; the cerberus
   `Metrics.TableFor(name)` heuristic only returns `SumTable` vs `GaugeTable`
   today and lacks histogram-bucket awareness for the exemplars case.

## 2 — OTel-CH exemplar column schema

The OTel ClickHouse Exporter writes exemplars on four of the five metrics
tables. Source: the upstream DDL templates at
`<sqltemplates>/metrics_{gauge,sum,histogram,exp_histogram}_table.sql` in
the `tsouza/opentelemetry-collector-contrib:cerberus-ddl` fork.

**Tables that carry exemplars:**

| Table                       | Per-sample value columns                                            | Exemplars column |
| --------------------------- | ------------------------------------------------------------------- | ---------------- |
| `otel_metrics_gauge`        | `Value Float64`                                                     | `Exemplars Nested(...)` |
| `otel_metrics_sum`          | `Value Float64`                                                     | `Exemplars Nested(...)` |
| `otel_metrics_histogram`    | `Count UInt64`, `Sum Float64`, `BucketCounts Array(UInt64)`         | `Exemplars Nested(...)` |
| `otel_metrics_exp_histogram`| `Count UInt64`, `Sum Float64`, `PositiveBucketCounts Array(UInt64)` | `Exemplars Nested(...)` |
| `otel_metrics_summary`      | `Count UInt64`, `Sum Float64`, `ValueAtQuantiles Nested(...)`       | **(none)**       |

The summary table does NOT have an `Exemplars` column upstream; PromQL queries
that name a summary-shaped metric should return an empty `data:[]` for the
exemplars path. The handler routing logic must guard against this.

**Exemplars Nested layout (identical across the four carrying tables):**

```text
Exemplars Nested (
    FilteredAttributes Map(LowCardinality(String), String),
    TimeUnix DateTime64(9),
    Value Float64,
    SpanId String,
    TraceId String
) CODEC(ZSTD(1))
```

ClickHouse's Nested encoding stores each sub-field as a parallel array — at
read time the columns surface as `Exemplars.FilteredAttributes`,
`Exemplars.TimeUnix`, `Exemplars.Value`, `Exemplars.SpanId`,
`Exemplars.TraceId`, each an `Array(...)` matching the per-row exemplar
count. This is the standard Nested shape; cerberus already encodes the
analogous `Events Nested(...)` / `Links Nested(...)` columns on
`otel_traces` (see § 4 for the precedent).

The Nested-vs-`Array(Tuple(...))` question is settled: the upstream DDL
fork ships them as Nested (the templates above are the source of truth), so
the SQL emitter targets the Nested form. If a deployment ever runs with a
`flatten_nested = 0` schema variant (where Nested surfaces as a single
`Array(Tuple(...))` column instead), the emitted SQL would need to swap
`arrayJoin(Exemplars.Value)` for `arrayJoin(Exemplars).2` / similar tuple
element access. PR A includes a comment on this fallback but does not
implement it; we trust the schema.

**Already wired in cerberus:**

`schema.Metrics.ExemplarsColumn` is already declared and defaulted to
`"Exemplars"` by `DefaultOTelMetrics()`
(`internal/schema/otel.go:131,318`). The wide-column list in
`internal/chsql/tableshape.go:140` already lists `metrics.ExemplarsColumn`
as a "wide" column on each metrics table (so late-materialisation and
predicate-classifier rules push expensive Exemplars reads to the
post-PREWHERE phase). No schema-package change is needed.

## 3 — SQL query template

For a request `/api/v1/query_exemplars?query=<metric{matchers}>&start=<t1>&end=<t2>`,
the handler resolves the metric-family target table, then emits the
following SQL shape (filled with parameter placeholders / typed Frags by the
real chsql emitter):

```sql
SELECT
    `MetricName`                                              AS metric_name,
    `Attributes`                                              AS attrs,
    `ServiceName`                                             AS service_name,
    arrayJoin(arrayZip(
        `Exemplars.TimeUnix`,
        `Exemplars.Value`,
        `Exemplars.TraceId`,
        `Exemplars.SpanId`,
        `Exemplars.FilteredAttributes`
    ))                                                        AS exemplar
FROM `<exemplars_table>`           -- gauge / sum / histogram / exp_histogram
WHERE `MetricName` = {metric_name:String}
  AND <label-matcher predicates>
  AND `TimeUnix` >= toDateTime64({start_unix:Float64}, 9)
  AND `TimeUnix` <= toDateTime64({end_unix:Float64}, 9)
  AND length(`Exemplars.TimeUnix`) > 0   -- skip rows without exemplars
ORDER BY metric_name, attrs, exemplar.1
```

`arrayZip(...)` packs the five parallel arrays into one Array(Tuple) so the
outer `arrayJoin` fans out one row per exemplar. `exemplar.1` ... `exemplar.5`
project the tuple components in row form. Equivalent shape using positional
`arrayJoin`-with-index, which lets us read the parallel arrays without a
materialised tuple:

```sql
WITH
    `Exemplars.TimeUnix` AS ts,
    `Exemplars.Value` AS val,
    `Exemplars.TraceId` AS tid,
    `Exemplars.SpanId` AS sid,
    `Exemplars.FilteredAttributes` AS attrs_arr,
    arrayJoin(arrayEnumerate(ts)) AS i
SELECT
    `MetricName` AS metric_name,
    `Attributes` AS series_attrs,
    `ServiceName` AS service_name,
    ts[i] AS exemplar_ts,
    val[i] AS exemplar_value,
    tid[i] AS exemplar_trace_id,
    sid[i] AS exemplar_span_id,
    attrs_arr[i] AS exemplar_attrs
FROM `<exemplars_table>`
WHERE `MetricName` = {metric_name:String}
  AND <label-matcher predicates>
  AND `TimeUnix` >= toDateTime64({start_unix:Float64}, 9)
  AND `TimeUnix` <= toDateTime64({end_unix:Float64}, 9)
  AND length(`Exemplars.TimeUnix`) > 0
ORDER BY metric_name, series_attrs, exemplar_ts
```

The second variant is preferred — it sidesteps `arrayZip` materialising a
fat tuple (the FilteredAttributes Map(...) inside a Tuple is awkward for
the CH planner) and projects each component as its own column, matching the
positional binding cerberus's `chclient.Cursor` already uses elsewhere.

**Row → wire mapping.** Once the cursor decodes the rows, the handler:

1. Groups by `(metric_name, series_attrs, service_name)` — that tuple is the
   `seriesLabels` key. The `__name__` label is `metric_name`; `service.name`
   is the dedicated service column; remaining series labels are the entries
   of the `Attributes` map.
2. Per group emits one `ExemplarSeries`. Inside each series the per-row
   exemplar columns become one `Exemplar`:
   - `Labels` is `exemplar_attrs` (a `Map(String,String)`) MERGED with the
     reserved `trace_id` / `span_id` keys derived from `exemplar_trace_id`
     and `exemplar_span_id`. The reserved-key precedence matches upstream
     Prom: caller-supplied attrs win, but trace/span IDs are auto-added if
     missing.
   - `Value` is `toFloat64(exemplar_value)`.
   - `Timestamp` is `toFloat64(toUnixTimestamp64Nano(exemplar_ts)) / 1e9` —
     unix seconds with fractional nanos (matches Prom's number-not-string
     exemplar JSON contract).

**Parameter binding.** The four `{...}` placeholders use cerberus's typed
parameter API (`InlineLit` for the metric name, the time bounds via
`Call("toDateTime64", InlineLit(...), InlineLit(int64(9)))`), so the
emitted SQL is fully parameterised — no string concatenation.

## 4 — chplan + chsql lowering shape

The query is structurally the simplest of the metrics queries: a per-table
scan with predicates, no time bucketing, no aggregation, no LWR. It reuses
the existing chplan IR almost entirely.

**chplan side — re-uses existing nodes, no new ones.**

```text
Project[
    metric_name, series_attrs, service_name,
    exemplar_ts, exemplar_value, exemplar_trace_id, exemplar_span_id, exemplar_attrs
] (
    Filter[
        And(
            And(
                And(
                    matcher_predicates,              // from VectorSelector.LabelMatchers
                    TimeUnix >= start,
                ),
                TimeUnix <= end,
            ),
            length(Exemplars.TimeUnix) > 0,          // non-empty exemplars guard
        ),
    ] (
        Scan[
            Table: <exemplars_table>,
            Columns: [
                MetricName, Attributes, ServiceName, TimeUnix,
                Exemplars.TimeUnix, Exemplars.Value,
                Exemplars.TraceId, Exemplars.SpanId, Exemplars.FilteredAttributes,
            ],
        ],
    ),
)
```

`buildPredicate(v.LabelMatchers, s)` from `internal/promql/lower.go:147` is
reused as-is — same matcher → predicate translation the
`/query`/`/query_range` paths use. The time-bound and non-empty-exemplars
predicates are appended via the existing `chplan.Binary{Op: chplan.OpAnd}`
fold. No chplan node additions.

**chsql side — one new emitter, one new Frag.**

The arrayJoin-of-Nested shape is not currently expressed by an existing
emitter. The plan adds:

- A new emitter function `EmitQueryExemplars(ctx, plan, schema) (sql,
  args, error)` (or as a method on `*emitter`) that handles the
  scan-and-Nested-arrayJoin shape end-to-end. Layer-1 (`emit.go`) routes
  to it when the plan root is the exemplars Project shape; alternatively
  it lives behind a dedicated entrypoint the handler calls directly,
  bypassing the generic root-dispatch — see § 5 PR A for the choice.
- One typed-Frag constructor for the positional Nested-array project. The
  existing `Call("arrayJoin", ...)` + `Subscript(arr, i)` combination
  already covers it; the new helper is a thin wrapper that takes a list
  of Nested sub-field names and emits the `WITH ts AS ...,
  arrayJoin(arrayEnumerate(ts)) AS i ... SELECT ts[i], val[i], ...` shape
  via the existing typed API. The contract is **no raw SQL strings** —
  the helper composes via `WithClause` / `Subscript` / `Call`, not
  string interpolation.

**Existing chsql infrastructure that is leveraged unchanged:**

- `chsql.QueryBuilder` slots (`Select` / `From` / `Where` / `OrderBy`) for
  the outer shape.
- `Col(name)` / `Ident(name)` for column references; `Lit(v)` for the
  metric-name and time literals; `InlineLit(...)` for the `toDateTime64`
  precision arg.
- `Call("arrayJoin", ...)`, `Call("arrayEnumerate", ...)`,
  `Call("length", ...)`, `Call("toFloat64", ...)`,
  `Call("toUnixTimestamp64Nano", ...)`, `Call("toDateTime64", ...)`.
- `Subscript(arr, i)` for the `array[index]` shape.
- `And` / `Or` / `Eq` / `Like` for predicate composition (already used by
  `buildPredicate`).
- `WithClause(alias, frag)` to bind the parallel-array names so the SELECT
  list reads cleanly. (If `WithClause` is not yet exposed, the equivalent is
  a derived-table subquery — both compile identically in CH; see the
  builder.go `WithRecursive` precedent.)

**Why not reuse `internal/chsql/exemplars.go::EmitMetricsExemplars`?**

The Tempo exemplars path operates on `otel_traces` (one row per span)
and emits one representative exemplar per (series, bucket) anchor via
`argMax` over the span timestamp. The PromQL exemplars endpoint operates on
the metrics tables (one row per metric data-point, each carrying an
Exemplars Nested column whose array length is the per-data-point exemplar
count) and emits EVERY exemplar in the requested window. The shapes do not
overlap. They share only the wire contract — the `{traceID, spanID, value,
timestamp, labels}` per-exemplar tuple — and the wire envelope is the same
shape Tempo's
`internal/api/tempo/metrics_query_range.go::attachExemplars` produces.

## 5 — Implementation PR breakdown

### PR A — chsql exemplars emitter for the Nested arrayJoin shape

Adds `internal/chsql/query_exemplars.go` exposing
`EmitQueryExemplars(ctx, exemplarsTable, predicate, schema) (sql, args,
error)`. The signature takes the resolved table name (not a chplan.Node) +
the matcher predicate (a `chplan.Expr` already built by `buildPredicate`) +
the schema struct (for column names: `MetricName`, `Attributes`,
`ServiceName`, `ExemplarsColumn` and its Nested sub-fields, `TimestampColumn`).
Returns the SQL string + `[]any` parameter values.

Scope:

- Build the `WITH ts AS Exemplars.TimeUnix, ...` parallel-array bindings
  via the typed builder.
- Build the outer `SELECT MetricName, Attributes, ServiceName, ts[i], val[i],
  tid[i], sid[i], attrs_arr[i] FROM <table> WHERE <predicate> AND TimeUnix
  >= ... AND TimeUnix <= ... AND length(ts) > 0`.
- Unit tests for the SQL shape with golden output covering all four metrics
  tables (gauge / sum / histogram / exp_histogram).
- Negative tests: missing `ExemplarsColumn` schema field returns
  `ErrUnsupported`; explicit `summary` table returns `ErrUnsupported`
  (cerberus refuses to emit an exemplars SQL against a table without an
  Exemplars column).
- Layer-2a TXTAR fixture under `test/spec/promql/exemplars_*.txtar` covering
  the `-- sql --` snapshot (no chDB roundtrip yet; that lands in PR C).

PR A is intentionally narrow: chsql only. No handler changes. The unit test
exercises the emitter directly via the typed Frags and a stub
`schema.Metrics`.

### PR B — handler wire-up

Updates `internal/api/prom/exemplars.go` to:

1. Keep the existing input validation untouched (already correct).
2. After parsing, walk the AST and require it to be a bare `VectorSelector`
   (or a `ParenExpr` wrapping one). Anything more complex returns
   `ErrBadData` — upstream Prometheus also restricts the `query` parameter
   on this endpoint to a single VectorSelector. (Confirmed against the
   reference `prometheus/web/api/v1/api.go::queryExemplars` handler.)
3. Extract `metricName = metricNameFromMatchers(v.LabelMatchers)` and
   resolve the target table via a new `schema.Metrics.ExemplarsTableFor(name)`
   helper. The helper extends the existing `TableFor` routing to:
   - `_bucket` / `_count` / `_sum` suffix → `HistogramTable`
     (those are histogram synthetic-series labels, not the sum table).
   - `_total` suffix → `SumTable`.
   - The exp-histogram suffix (`Metrics.ExpHistogramSuffix`, default
     `_exp_hist`) → `ExpHistogramTable`.
   - No suffix → `GaugeTable`.
   - Summary metrics (no naming convention; the schema would need a
     `SummaryMetricNames []string` allow-list, OR the handler simply
     returns empty for summaries by treating them as gauges and emitting
     against the gauge table — see § 7 Open question 2).
4. Build the matcher predicate via the existing
   `buildPredicate(v.LabelMatchers, s)`.
5. Call `chsql.EmitQueryExemplars(ctx, table, predicate, start, end,
   h.Schema)`.
6. Run the SQL via `h.Client.QueryExemplars(ctx, sql, args)` — a new method
   on the `Querier` interface that returns a typed row slice (or a generic
   cursor that the handler decodes into ExemplarSeries).
7. Group the rows by `(metric_name, attrs, service_name)`, project each
   group into one `ExemplarSeries`, emit per-row Exemplars with the
   `trace_id` / `span_id` reserved-key merge described in § 3.
8. Return the envelope via the existing `writeJSON(..., Response{Status:
   "success", Data: <series slice>})` path.

Scope:

- The new `Querier.QueryExemplars` cursor — implemented over
  `chclient.Client.Query` since the row shape is fixed (eight columns:
  metric_name String, attrs Map(String,String), service_name String,
  ts DateTime64(9), value Float64, trace_id String, span_id String,
  attrs Map(String,String)).
- A unit test on the handler against a stub `Querier` that returns canned
  rows. Asserts the wire envelope (`ExemplarSeries` shape, grouping by
  series labels, the `__name__` / `service.name` precedence, the
  `trace_id` / `span_id` reserved-key merge).

PR B does not touch chsql; it consumes PR A's emitter as a black box.

### PR C — TXTAR fixture + chDB roundtrip with seed exemplars

Adds `test/spec/promql/query_exemplars_basic.txtar` with `-- seed --`,
`-- input --`, `-- sql --`, `-- chplan --`, and `-- expected_rows --`
sections (the standard TXTAR slots covered by the existing chDB runner).
The seed populates `otel_metrics_sum` and `otel_metrics_histogram` with a
handful of rows each, each row carrying 1-3 exemplars in the Nested column.
The `-- input --` is the wire-level URL
`/api/v1/query_exemplars?query=<...>&start=<...>&end=<...>`; `-- expected_rows --`
asserts the response JSON envelope verbatim.

Scope:

- One fixture for `sum` (`my_counter_total{service="checkout"}`), one for
  `histogram` (`my_histogram_bucket{service="checkout"}`), and one negative
  case (summary metric → empty `data:[]`).
- Conformance assertion: response JSON parses against the upstream
  Prom-API expectations (use the existing fixture-runner's JSON-shape
  comparator from `test/spec/runner_chdb.go`).
- Integration with the seed loader: the `-- seed --` section needs to emit
  `INSERT INTO otel_metrics_sum (..., Exemplars.TimeUnix, Exemplars.Value,
  ...) VALUES (..., [t1, t2], [v1, v2], ...)` rows — the Nested-column
  insert shape. The chDB runner already handles Nested inserts for
  `otel_logs.LogAttributes`; the metrics tables follow the same convention.

PR C does not change production code; it is fixture + asserts.

## 6 — Test plan

- **PR A — typed-builder unit tests on the SQL shape.**
  - Golden SQL for each of the four exemplars-carrying tables (gauge, sum,
    histogram, exp_histogram). Assert the WHERE clause includes the
    metric-name equality, the matcher predicates, the time bounds, and the
    `length(Exemplars.TimeUnix) > 0` guard.
  - Negative: summary table is rejected with `ErrUnsupported`.
  - Negative: empty matcher list (no `__name__`) is rejected upstream by
    the handler — chsql is not tested for that branch; the emitter
    assumes a metric name was resolved.
- **PR B — handler tests against a stub Querier.**
  - Single series with two exemplars → one `ExemplarSeries` with two
    `Exemplar` entries; `trace_id` / `span_id` populated from the row.
  - Two series with one exemplar each → two `ExemplarSeries`, each with
    one `Exemplar`.
  - Empty result set → `"data":[]` not `"data":null`.
  - Bad PromQL → `ErrBadData` with the existing error envelope.
  - Non-VectorSelector query (e.g. `sum(rate(metric[5m]))`) → `ErrBadData`
    with a structured "single VectorSelector required" message.
  - Bad time bounds → existing time-validation paths.
  - Summary metric → `"data":[]` (handler short-circuits before chsql).
- **PR C — chDB roundtrip.**
  - Seed → SQL → response: the seeded exemplars surface in the response in
    timestamp order.
  - Conformance against the Prom wire shape: `seriesLabels` carries
    `__name__`, `service.name` if present, and any per-series Attributes
    keys; `exemplars[].labels` carries `trace_id` + `span_id` plus any
    per-exemplar FilteredAttributes keys.
  - JSON envelope: `status: "success"`, `data` is a top-level array (not
    `data.exemplars` or any wrapper) — matches the existing placeholder's
    shape.
- **Layer-3a property test (optional follow-up).** The exemplars endpoint
  is structurally simple enough to oracle: feed a random metric name +
  random exemplar count per row to the chDB runner, then assert the
  returned exemplar count equals the sum of per-row counts. Not in scope
  for PRs A-C; tracked as a follow-up under
  [`docs/test-strategy.md`](test-strategy.md) Phase 3 (oracle-vs-impl
  comparator additions).
- **Layer-7a Prom-compliance harness.** The
  `compatibility/prometheus/expected-failures.json` already lists the
  exemplars-related queries; PR C closes those entries (or downgrades them
  to "passes" once the implementation lands). Confirm by re-running
  `just compatibility` locally and updating the failure list in the same PR.

## 7 — Open questions and risks

- **Per-row vs per-metric joins.** The query in § 3 fans out exemplars
  one-per-row with `arrayJoin(arrayEnumerate(...))`, then groups in
  application code. The ALTERNATIVE is to project the parallel arrays as
  arrays and group in SQL via `groupArray(arrayZip(...))`, returning one
  row per series with the exemplars list nested. The former is simpler to
  decode (cerberus's cursor already binds flat rows) and the latter saves
  network round-trip volume by collapsing series grouping. PR A picks the
  former (per-row); PR C measures volume and decides whether to swap.
  Both compile identically against the same chsql Frag set, so the swap
  is a single emitter constant change.

- **Summary table behaviour.** The OTel-CH summary table has no Exemplars
  column. The handler must either (a) refuse summary-shaped queries with
  `ErrBadData` ("metric type does not support exemplars") or (b) silently
  return an empty `data:[]` for summary metrics, matching Prom's behaviour
  for metric types without exemplars. PR B defaults to (b) — silently
  empty — and documents the behaviour in the handler doc-comment, since
  empty-array is exactly what the upstream Prom client expects when no
  exemplars exist on a metric.

- **Large exemplar arrays.** A high-throughput histogram can carry dozens
  of exemplars per data-point and millions of data-points in a 1-hour
  window. The `arrayJoin(arrayEnumerate(...))` plan fans out
  per-data-point × per-exemplar, so the cardinality is the cross product.
  Mitigations:
  1. Apply a `SETTINGS max_result_rows = N` cap inside the emitter (default
     ~10k matching Grafana's exemplars panel display limit).
  2. ORDER BY exemplar timestamp DESC + LIMIT N to surface the most-recent
     exemplars when the cap trips.
  3. Make the cap configurable via env (`CERBERUS_PROM_EXEMPLAR_LIMIT`,
     default 10000) — same pattern as the existing limit knobs in
     `internal/config/`. PR A wires the cap; PR B exposes the env knob.

- **Gauge support.** PromQL exemplars are most commonly associated with
  histograms (the OTel SDK auto-attaches exemplars to histogram
  observations). Gauge exemplars are rarer and may be empty on most
  deployments; cerberus still queries the gauge table for non-suffixed
  metric names (`TableFor` heuristic) so the query path is correct, just
  often yields empty results. No additional handling needed; the
  `length(Exemplars.TimeUnix) > 0` predicate skips empty-Exemplar rows
  before fanout.

- **`__name__` matcher requirement.** The PromQL spec says
  `query_exemplars` requires the query to name a specific metric (via the
  `__name__` matcher); a query with only label matchers (`{job="api"}`)
  is invalid. PR B validates `metricNameFromMatchers(v.LabelMatchers) != ""`
  and returns `ErrBadData` if empty. This matches the upstream Prom
  handler's behaviour (`prometheus/web/api/v1/api.go::queryExemplars`
  errors with "metric name is required").

- **Nested-vs-flat schema variant.** The DDL templates ship Nested with
  the default `flatten_nested = 1`. If a deployment runs with
  `flatten_nested = 0`, the Exemplars column surfaces as a single
  `Array(Tuple(...))` instead of parallel arrays. The emitted SQL in § 3
  targets the Nested form; the alternative would emit
  `arrayJoin(Exemplars).2` (tuple element access) instead of
  `Exemplars.TimeUnix[i]`. PR A documents this in the emitter
  doc-comment but does not switch on schema variant — cerberus's schema
  layer assumes the upstream-default Nested form everywhere. A deployment
  running `flatten_nested = 0` would also break the existing
  `Events Nested(...)` reads in `internal/api/tempo/`, so this is a
  whole-codebase invariant, not an exemplars-specific risk.

- **Reserved-key precedence in `Exemplar.Labels`.** The OTel-CH
  `Exemplars.FilteredAttributes` is the attribute set the SDK records on
  each exemplar; the reserved Prom wire-format keys `trace_id` and
  `span_id` are populated from `Exemplars.TraceId` / `Exemplars.SpanId`.
  If the FilteredAttributes map already carries a `trace_id` or `span_id`
  key (unusual but legal in OTel), the merge precedence is:
  - The Exemplar-row TraceId / SpanId columns ALWAYS win — the OTel-CH
    exporter writes them from the OTel `SpanContext`, not from the
    attribute set, so they are authoritative.
  - Empty TraceId / SpanId columns are dropped (do not emit empty
    `"trace_id":""` keys to the wire). This matches Prom's behaviour for
    exemplars without trace linkage.

  PR B implements the merge in the row → ExemplarSeries projection;
  unit tests exercise both branches (FilteredAttributes carries a
  collision, and TraceId is empty).
</content>
</invoke>