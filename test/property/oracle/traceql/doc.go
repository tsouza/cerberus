// Package traceql is the from-scratch TraceQL evaluator that serves
// as the independent specification for cerberus's property-testing
// framework. The Tempo TraceQL parser is treated as a string-format
// parser only; semantic evaluation lives in-tree, derived from the
// Grafana TraceQL documentation rather than reusing Tempo's engine.
//
// # MVP coverage (Phase 1)
//
//   - SpansetFilter: `{ resource.service.name = "<value>" }` —
//     attribute-equality matcher against ResourceAttributes['service.name'].
//   - Scalar filter pipeline: `| count() OP N` where OP ∈
//     {>, >=, <, <=, =} and N is a non-negative integer literal.
//
// # Semantics (per the TraceQL spec)
//
// TraceQL evaluates over spans. A spanset is the per-trace bucket of
// spans matching the upstream filter; pipeline operators reduce or
// transform the spanset.
//
//  1. Spanset filter (`{ <attr> = <value> }`): each span is
//     evaluated against the predicate; surviving spans form the
//     spanset. Per-trace grouping is implicit in Tempo's engine but
//     irrelevant for the count() aggregation today — the evaluator
//     keeps a flat span list because count() across all matching
//     spans equals count() per-trace summed across traces (each of
//     the generator's spans lives in its own trace).
//  2. count() aggregate: returns the number of spans in the spanset.
//     With a scalar filter (`| count() > N`), the trace is included
//     in the result only when the predicate holds. Cerberus's wire
//     surface returns the count value as a single sample (one row
//     in the search response) when the predicate is satisfied,
//     zero rows when it isn't — the evaluator mirrors that shape.
//
// # Wire-shape comparison
//
// Tempo's /api/search response carries the count as `inspectedTraces`
// (which equals `len(res.Samples)` — see internal/api/tempo/handler.go's
// SearchMetrics population). The evaluator therefore reports:
//
//   - Selector-only query: one outcome row per matching span, all
//     stamped with the same empty label set so the framework's
//     comparator counts rows-per-group.
//   - count() query that passes: ONE outcome row (count emitted as a
//     single trace summary in cerberus's response).
//   - count() query that fails: ZERO outcome rows.
//
// The framework's CompareOutcomes diff is multiset-aware over the
// empty-label group, so the row count is what we compare. Tempo's
// /api/search collapses multiple spans with the same (SpanName,
// Timestamp) into one trace summary — the generator avoids that
// collapse by stamping a unique suffix on each span name, so
// `len(samples) == len(traces)` on the cerberus side.
//
// # Entry point
//
// Callers use [Evaluate], which is a pure function: dataset + query
// (string form), returns a property.Outcome.
package traceql
