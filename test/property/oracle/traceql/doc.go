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
// transform the spanset on a per-trace basis.
//
//  1. Spanset filter (`{ <attr> = <value> }`): each span is
//     evaluated against the predicate; surviving spans form the
//     per-trace spanset.
//  2. count() aggregate: returns the number of matching spans IN A
//     TRACE. With a scalar filter (`| count() > N`), every trace
//     whose per-trace count satisfies the predicate is included in
//     the result; traces whose count fails the predicate are
//     dropped. Cerberus's wire surface emits one chclient.Sample
//     per surviving trace (the Aggregate groups by TraceId — see
//     internal/api/tempo/handler.go's isSpansetAggregateShape) so
//     the evaluator mirrors that per-trace shape.
//
// # Wire-shape comparison
//
// Tempo's /api/search response carries the count as `inspectedTraces`
// (which equals `len(res.Samples)` — see internal/api/tempo/handler.go's
// SearchMetrics population). The evaluator therefore reports:
//
//   - Selector-only query: one outcome row per matching span, all
//     stamped with the same empty label set so the framework's
//     comparator counts rows-per-group. The generator stamps each
//     span on its own TraceID so per-trace and per-span counts
//     coincide.
//   - count() query: one outcome row per matching trace whose
//     per-trace count satisfies `count() OP N` (zero rows when no
//     trace's count satisfies the predicate).
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
