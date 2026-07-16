package chplan

// InSubquery is a set-membership predicate: `<Left> IN (<Subquery>)`, where
// Subquery is a full plan subtree rather than a literal list (see InList for
// the flat-literal sibling). It exists so a lowering or emitter can inject an
// exact membership predicate — e.g. "TraceId IN (<the already-filtered span
// cohort>)" — directly into a Filter that sits BELOW an Aggregate's GROUP BY,
// so ClickHouse can push the predicate all the way down into the underlying
// MergeTree scan. Wrapping a `WHERE <col> IN (...)` around an ALREADY
// aggregated relation (i.e. wrapping the Aggregate's output) does not get
// that pushdown — CH does not push a predicate back down through GROUP BY —
// so it prunes nothing at the scan; see metrics_compare.go's RootLookup
// TraceId-membership rewrite in internal/chsql for the motivating case.
//
// Optimizer note: InSubquery is an Expr, so chplan.Walk does NOT recurse
// into Subquery — Walk only traverses Node.Children, and an Expr field
// (including any Node embedded inside one) is invisible to it by
// construction. This mirrors ScalarSubquery's exclusion from optimizer
// visibility; see scalar_subquery.go for the full rationale. InspectExpr /
// InspectExprNodes still reach it explicitly, exactly like ScalarSubquery.
type InSubquery struct {
	Left     Expr
	Subquery Node
}

func (*InSubquery) exprNode() {}

func (i *InSubquery) Equal(other Expr) bool {
	o, ok := other.(*InSubquery)
	if !ok {
		return false
	}
	if (i.Left == nil) != (o.Left == nil) {
		return false
	}
	if i.Left != nil && !i.Left.Equal(o.Left) {
		return false
	}
	if (i.Subquery == nil) != (o.Subquery == nil) {
		return false
	}
	if i.Subquery == nil {
		return true
	}
	return i.Subquery.Equal(o.Subquery)
}
