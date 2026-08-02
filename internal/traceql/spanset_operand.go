// See aggregate.go for the no-reflection / no-pointer-aliasing rule
// covering this file.

package traceql

import (
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// spanGranularOperand re-expresses a spanset-aggregate sub-pipeline
// operand (`({ … } | count() > 1)`) as the SPANS it stands for.
//
// Reference Tempo evaluates a spanset aggregate as an annotation, not a
// reduction: `Aggregate.evaluate` clones the per-trace spanset, sets
// `Spanset.Scalar` and keeps every span, and `ScalarFilter.evaluate`
// then forwards that spanset unchanged when the comparison holds
// (grafana/tempo pkg/traceql/ast_execute.go). Both set operators go on
// to combine SPANS — `OpSpansetUnion` emits `uniqueSpans(lhs, rhs)` —
// so an aggregate operand contributes the spans its inner selector
// matched, restricted to the traces whose aggregate passed. It does not
// contribute one row per trace.
//
// Cerberus's lowerAggregate collapses to one row per trace (GROUP BY
// TraceId). That is right for a top-level `| count() > N` search, where
// the /api/search envelope is per trace, and wrong inside a spanset
// operation: the union emitter matches arms positionally (ClickHouse
// rejects a column-count mismatch with code 258) and keys its identity
// dedup on SpanId, which the grouped row shape does not carry. So an
// aggregate arm beside a plain span arm is a user-reachable CH error
// today, and an aggregate arm beside another aggregate arm resolves
// `L.SpanId` against a relation that has none.
//
// Un-collapsing restores both at once: the arm becomes the span rows
// the aggregate consumed, semi-joined against the trace cohort the
// aggregate admits. The rows are Tempo's spans and the column list is
// the same `SELECT *` every plain arm exposes.
//
// Any other operand shape — a bare selector, a nested set operation, a
// structural join, a `| by(…)` grouping — is returned untouched.
func spanGranularOperand(n chplan.Node, s schema.Traces) chplan.Node {
	agg := traceGranularAggregate(n, s)
	if agg == nil {
		return n
	}
	// The cohort subquery keeps the arm's aggregate + any scalar-filter
	// HAVING and projects the group key alone; the row source is an
	// independent clone of the aggregate's input, so the two subtrees stay
	// separately rewritable by the window / trace-limit stamping passes
	// rather than aliasing one node into two positions.
	cohort := &chplan.Project{
		Input:       cohortAggregate(n, agg),
		Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: aggTraceIDAlias}}},
	}
	return &chplan.Filter{
		Input: chplan.CloneNode(agg.Input),
		Predicate: &chplan.InSubquery{
			Left:     &chplan.ColumnRef{Name: s.TraceIDColumn},
			Subquery: cohort,
		},
	}
}

// cohortAggregate rebuilds the operand's aggregate (and the scalar-filter
// HAVING above it, when there is one) carrying only the aggregate functions
// that HAVING actually reads. The cohort projects the group key alone, so
// every other function in lowerAggregate's /api/search envelope — span name,
// resource attributes, trace start/end — would be computed over the whole
// scan and then thrown away.
func cohortAggregate(n chplan.Node, agg *chplan.Aggregate) chplan.Node {
	f, ok := n.(*chplan.Filter)
	if !ok {
		// A bare `| count()` operand has no HAVING to satisfy, so the cohort
		// is every group the aggregate produces and no function is read.
		return narrowAggFuncs(agg, nil)
	}
	return &chplan.Filter{Input: narrowAggFuncs(agg, havingRefs(f.Predicate)), Predicate: f.Predicate}
}

// narrowAggFuncs returns a copy of agg keeping only the aggregate functions
// whose alias is in keep.
func narrowAggFuncs(agg *chplan.Aggregate, keep map[string]bool) *chplan.Aggregate {
	out := *agg
	out.AggFuncs = nil
	for _, fn := range agg.AggFuncs {
		if keep[fn.Alias] {
			out.AggFuncs = append(out.AggFuncs, fn)
		}
	}
	return &out
}

// havingRefs collects the column names a scalar-filter predicate reads. Every
// name an aggregate can supply is one of its aliases, so intersecting this set
// with the AggFuncs is what narrowAggFuncs prunes on.
func havingRefs(e chplan.Expr) map[string]bool {
	refs := map[string]bool{}
	chplan.InspectExpr(e, func(x chplan.Expr) bool {
		if col, ok := x.(*chplan.ColumnRef); ok {
			refs[col.Name] = true
		}
		return true
	})
	return refs
}

// traceGranularAggregate returns the per-trace spanset aggregate at the
// root of n — either the bare `| count()` Aggregate or the same node
// under the `| count() > N` scalar-filter Filter — and nil for every
// other shape.
func traceGranularAggregate(n chplan.Node, s schema.Traces) *chplan.Aggregate {
	if f, ok := n.(*chplan.Filter); ok {
		n = f.Input
	}
	agg, ok := n.(*chplan.Aggregate)
	if !ok || agg.Input == nil || !groupsOnTraceID(agg, s.TraceIDColumn) {
		return nil
	}
	return agg
}

// groupsOnTraceID reports whether agg's GROUP BY is exactly the trace-id
// column under lowerAggregate's alias — the only grouping whose cohort a
// `TraceId IN (…)` semi-join can reproduce. A `| by(…)` grouping
// (group_coalesce.go) or any future multi-key aggregate falls through.
func groupsOnTraceID(agg *chplan.Aggregate, traceIDCol string) bool {
	if len(agg.GroupBy) != 1 || len(agg.GroupByAliases) != 1 {
		return false
	}
	col, ok := agg.GroupBy[0].(*chplan.ColumnRef)
	return ok && col.Name == traceIDCol && agg.GroupByAliases[0] == aggTraceIDAlias
}
