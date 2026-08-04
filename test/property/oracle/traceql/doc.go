// Package traceql is the from-scratch TraceQL evaluator that serves
// as the independent specification for cerberus's property-testing
// framework. The Tempo TraceQL parser and cerberus's own
// internal/traceql/ast are both treated as out of scope for this
// package — semantic evaluation lives here, derived from the Grafana
// TraceQL documentation rather than reusing either engine.
//
// # Coverage
//
// The generator (test/property/gen/traceql.go's TraceQLQuery) draws 14
// query shapes, widened from the first sweep's single-selector accept
// set (issue #1471) to a breadth comparable to the PromQL leg's
// drawExpr. This package evaluates every one of them:
//
//   - Attribute matchers: `resource.service.name`, `resource.cluster`,
//     `span.http.method`, with `=`, `!=`, `=~`, `!~`.
//   - Intrinsics: `duration OP Nms` (all five comparison operators),
//     `status OP <ok|error|unset>` (`=`/`!=`), `name = "<value>"`.
//   - Multi-condition filters: `{ A && B }`.
//   - Structural relations: `{ A } > { B }` (immediate child) and
//     `{ A } >> { B }` (descendant at any depth), evaluated by walking
//     the generator's linear parent chains (see TraceQLDataset's doc —
//     the dataset never branches, so "some ancestor matches A" reduces
//     to a chain walk).
//   - Scalar-filter pipelines: `| count() OP N` and
//     `| avg|min|max|sum(duration) OP Nms`, both trace-scoped
//     aggregates.
//   - `| select(...)`: recognized and treated as a no-op on the result
//     row count (select() only adds projected columns — see
//     test/spec/traceql/select_one_attr.txtar).
//
// A shape is added to the generator only once this package can
// evaluate it — never the reverse — so the property test is never
// vacuous for a generated shape.
//
// # Semantics (per the TraceQL spec)
//
// TraceQL evaluates over spans. A spanset is the per-trace bucket of
// spans matching the upstream filter; pipeline operators reduce or
// transform the spanset on a per-trace basis.
//
//  1. Spanset filter (`{ <cond> (&& <cond>)* }`): each span is
//     evaluated against every condition; surviving spans form the
//     per-trace spanset.
//  2. Structural relation (`{A} > {B}` / `{A} >> {B}`): B's spanset is
//     narrowed to spans that are, respectively, an immediate child or
//     any-depth descendant of some span in A's spanset within the same
//     trace.
//  3. count() aggregate: returns the number of matching spans IN A
//     TRACE. With a scalar filter (`| count() > N`), every trace
//     whose per-trace count satisfies the predicate is included in
//     the result; traces whose count fails the predicate are
//     dropped. Cerberus's wire surface emits one chclient.Sample
//     per surviving trace (the Aggregate groups by TraceId — see
//     internal/api/tempo/handler.go's isSpansetAggregateShape) so
//     the evaluator mirrors that per-trace shape. avg|min|max|sum(duration)
//     reduce the same per-trace spanset's Duration values instead of
//     counting them (see test/spec/traceql/avg_duration.txtar).
//
// # Wire-shape comparison
//
// Tempo's /api/search response carries the drained-row count as
// `X-Cerberus-Inspected-Spans` (which equals `len(res.Samples)` — see
// internal/api/tempo/search_metrics.go's SearchMetricsFor). That count
// tracks the query's result row count for every shape above — a plain
// filter, a structural join, an aggregate, or a select() projection —
// so the evaluator reports:
//
//   - Filter / structural / select() query: one outcome row per
//     matching span, all stamped with the same empty label set so the
//     framework's comparator counts rows-per-group. The generator
//     stamps each span on its own (SpanName, Timestamp) pair so
//     per-trace and per-span counts coincide (Tempo's TraceSummary
//     collapse never triggers).
//   - count() / avg|min|max|sum(duration) query: one outcome row per
//     matching trace whose per-trace aggregate satisfies the scalar
//     filter (zero rows when no trace's aggregate satisfies it).
//
// The framework's CompareOutcomes diff is multiset-aware over the
// empty-label group, so the row count is what we compare.
//
// # Entry point
//
// Callers use [Evaluate], which is a pure function: dataset + query
// (string form), returns a property.Outcome.
package traceql
