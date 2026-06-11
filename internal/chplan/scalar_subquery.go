package chplan

// ScalarSubquery is an expression that embeds a plan subtree and reads
// its result as a single scalar value: the emitter renders the wrapped
// node parenthesised — `(<SELECT ...>)` — which ClickHouse treats as a
// scalar subquery (evaluated once, then folded as a constant into the
// enclosing expression; CH caches scalar-subquery results, so the same
// ScalarSubquery referenced repeatedly inside one statement costs one
// evaluation).
//
// Contract on Input:
//
//   - It MUST project exactly ONE column. A multi-column subquery is a
//     tuple in CH's scalar-subquery position and breaks every numeric
//     consumer.
//   - It MUST yield exactly ONE row. CH errors on a scalar subquery
//     returning zero rows ("Scalar subquery returned empty result") or
//     more than one. Lowerings that build ScalarSubquery inputs from
//     data-dependent trees must wrap them in an aggregation that pins
//     the one-row shape (see internal/promql's scalarValuePlan, which
//     emits `if(count() = 1, any(Value), nan)` — PromQL `scalar()`
//     semantics — over a no-GROUP-BY Aggregate that always returns one
//     row).
//
// This is the expression-position sibling of chplan.TopK's KExpr slot
// (a node-typed field consumed at the node level): ScalarSubquery lets
// any Expr slot — Project projections, Aggregate parameters, predicate
// trees — bind a computed scalar without threading node-typed fields
// through every consumer.
//
// Optimizer note: ScalarSubquery is an Expr, so chplan.Walk does NOT
// recurse into Input (Walk's contract covers Node.Children only). The
// embedded subtree is therefore invisible to the rule-based optimizer —
// it executes unoptimized, which is acceptable for the scalar-argument
// shapes that produce it (single-row reductions over small scans).
type ScalarSubquery struct {
	Input Node
}

func (*ScalarSubquery) exprNode() {}

func (s *ScalarSubquery) Equal(other Expr) bool {
	o, ok := other.(*ScalarSubquery)
	if !ok {
		return false
	}
	if s.Input == nil || o.Input == nil {
		return s.Input == o.Input
	}
	return s.Input.Equal(o.Input)
}
