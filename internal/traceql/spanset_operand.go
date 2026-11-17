// See aggregate.go for the no-reflection / no-pointer-aliasing rule
// covering this file.

package traceql

import (
	"maps"
	"slices"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// spanGranularOperand re-expresses a spanset aggregate (`{ … } |
// count() > 1`) as the SPANS it stands for, wherever something downstream
// reads spans: a spanset-operation operand (`({ … } | count() > 1) &&
// (…)`), a following pipeline stage (lowerPipeline) and a metrics function
// (`{ … } | count() > 1 | rate()`, lowerRoot).
//
// Reference Tempo evaluates a spanset aggregate as an annotation, not a
// reduction: `Aggregate.evaluate` clones the per-trace spanset, sets
// `Spanset.Scalar` and keeps every span, and `ScalarFilter.evaluate`
// then forwards that spanset unchanged when the comparison holds
// (grafana/tempo pkg/traceql/ast_execute.go). Both set operators go on
// to combine SPANS — `OpSpansetUnion` emits `uniqueSpans(lhs, rhs)` —
// a following stage evaluates over the same spanset, and the metrics
// evaluator observes every span of each spanset the pipeline returns
// (`MetricsEvaluator.Do`, pkg/traceql/engine_metrics.go). So an aggregate
// contributes the spans its inner selector matched, restricted to the
// traces whose aggregate passed. It does not contribute one row per trace.
//
// Cerberus's lowerAggregate collapses to one row per trace (GROUP BY
// TraceId). That is right for a top-level `| count() > N` search, where
// the /api/search envelope is per trace, and wrong for every span reader:
// the union emitter matches arms positionally (ClickHouse rejects a
// column-count mismatch with code 258) and keys its identity dedup on
// SpanId, which the grouped row shape does not carry; a following
// aggregate would count per-trace rows rather than spans; and a metrics
// node needs the span timestamp the per-trace row no longer has.
//
// Un-collapsing restores all of them at once: the aggregate becomes the
// span rows it consumed, semi-joined against the spansets it admits. The
// rows are Tempo's spans and the column list is the same `SELECT *` every
// plain selector exposes.
//
// After a by() stage the aggregate evaluates once per group
// (groupPerSpanset) and admits groups rather than traces, so the semi-join
// keys on the (TraceId, group key) pair: `{ } | by(name) | count() > 1`
// keeps the spans of each (trace, name) group with more than one span.
//
// Any other shape — a bare selector, a nested set operation, a structural
// join — is returned untouched.
func spanGranularOperand(n chplan.Node, s schema.Traces) chplan.Node {
	agg := spansetAggregate(n, s)
	if agg == nil {
		return n
	}
	// The cohort subquery keeps the arm's aggregate + any scalar-filter
	// HAVING and projects the group key alone; the row source is an
	// independent clone of the aggregate's input, so the two subtrees stay
	// separately rewritable by the window / trace-limit stamping passes
	// rather than aliasing one node into two positions.
	member := chplan.Expr(&chplan.ColumnRef{Name: s.TraceIDColumn})
	keys := []chplan.Projection{{Expr: &chplan.ColumnRef{Name: aggTraceIDAlias}}}
	if len(agg.GroupBy) == len(spansetGroupByAliases) {
		// The group key sits in a one-element tuple on both sides. A key can
		// be NULL (`by(span.a + 1)` on a span with no `a`), and GROUP BY
		// keeps the NULL-keyed spans as one group, as Tempo keeps its nil
		// group; a NULL top-level IN element never matches, while a NULL
		// inside a nested tuple compares as a value.
		member = &chplan.FuncCall{Fn: chplan.FnTuple, Args: []chplan.Expr{member, nullSafeKey(agg.GroupBy[1])}}
		keys = append(keys, chplan.Projection{Expr: nullSafeKey(&chplan.ColumnRef{Name: groupKeyAlias})})
	}
	return &chplan.Filter{
		Input: chplan.CloneNode(agg.Input),
		Predicate: &chplan.InSubquery{
			Left:     member,
			Subquery: &chplan.Project{Input: cohortAggregate(n, agg, map[string]bool{}), Projections: keys},
		},
	}
}

// nullSafeKey wraps a group key in the one-element tuple spanGranularOperand
// compares it in.
func nullSafeKey(key chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Fn: chplan.FnTuple, Args: []chplan.Expr{key}}
}

// cohortAggregate rebuilds the operand's aggregate (and the scalar-filter
// HAVING Filters above it, when there are any) carrying only the aggregate
// functions those Filters read, which keep accumulates on the way down. The
// cohort projects the group key alone, so every other function in
// lowerAggregate's /api/search envelope — span name, resource attributes,
// trace start/end — would be computed over the whole scan and then thrown
// away. A bare `| count()` operand has no HAVING to satisfy, so its cohort is
// every group the aggregate produces and no function is read.
func cohortAggregate(n chplan.Node, agg *chplan.Aggregate, keep map[string]bool) chplan.Node {
	f, ok := n.(*chplan.Filter)
	if !ok {
		return narrowAggFuncs(agg, keep)
	}
	maps.Copy(keep, havingRefs(f.Predicate))
	return &chplan.Filter{Input: cohortAggregate(f.Input, agg, keep), Predicate: f.Predicate}
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

// spansetAggregate returns the spanset aggregate at the root of n —
// either the bare `| count()` Aggregate or the same node under the
// `| count() > N` scalar-filter Filter (and the `Value IS NOT NULL` Filter
// a nullable aggregate carries, dropNullValueRows) — and nil for every
// other shape. The aggregate groups per trace, or per (trace, group key)
// after a by() stage (groupPerSpanset).
func spansetAggregate(n chplan.Node, s schema.Traces) *chplan.Aggregate {
	for {
		f, ok := n.(*chplan.Filter)
		if !ok {
			break
		}
		n = f.Input
	}
	agg, ok := n.(*chplan.Aggregate)
	if !ok || agg.Input == nil || !groupsPerSpanset(agg, s.TraceIDColumn) {
		return nil
	}
	return agg
}

// spansetGroupByAliases are the GROUP BY aliases of a spanset aggregate after
// a by() stage: the trace and the by() group key. A spanset aggregate with
// no by() before it carries the first alone.
var spansetGroupByAliases = []string{aggTraceIDAlias, groupKeyAlias}

// groupsPerSpanset reports whether agg's GROUP BY is the trace-id column
// under lowerAggregate's alias, optionally followed by a group key under the
// GroupKey alias — the only groupings whose cohort a `TraceId IN (…)` or
// `(TraceId, key) IN (…)` semi-join can reproduce. Both a spanset aggregate
// after by() (groupPerSpanset) and the by() stage's own groupAggregate group
// that way; the semi-join is right for either, though lowerFollowingStage
// reads a by() stage's spans directly. A coalesce() aggregate
// (group_coalesce.go), which groups by span identity, falls through.
func groupsPerSpanset(agg *chplan.Aggregate, traceIDCol string) bool {
	n := len(agg.GroupBy)
	if n == 0 || n > len(spansetGroupByAliases) || !slices.Equal(agg.GroupByAliases, spansetGroupByAliases[:n]) {
		return false
	}
	col, ok := agg.GroupBy[0].(*chplan.ColumnRef)
	return ok && col.Name == traceIDCol
}
