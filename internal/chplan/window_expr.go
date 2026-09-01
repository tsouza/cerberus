package chplan

// WindowExpr is a windowed-aggregate expression: `<Fn>(<Args>) OVER
// (PARTITION BY <PartitionBy>)`. It computes an aggregate over each
// PARTITION BY group WITHOUT collapsing rows — every row in a group is
// annotated with that group's own aggregate result — unlike a GROUP BY
// Aggregate node, which reduces each group down to one row.
//
// The motivating shape: a ClickHouse aggregate's per-row arguments cannot
// see a SIBLING aggregate's finished result within the same GROUP BY
// pass, so a value one aggregate needs as input to another must be
// resolved in an earlier, independent pass. WindowExpr is that earlier
// pass, evaluated in a non-aggregating Project stage ahead of the
// Aggregate that consumes it. It generalizes past a single group:
// PartitionBy carries the same group-key expression(s) the later
// Aggregate GROUP BYs on, so every row is annotated with ITS OWN group's
// value rather than one value shared by the whole input — unlike
// ScalarSubquery, which is valid only for exactly one group (it always
// resolves to a single scalar). An empty PartitionBy renders `OVER ()`,
// ClickHouse's whole-result-set partition, reproducing ScalarSubquery's
// single-group behavior without a subquery.
//
// Fn is resolved through the same sealed Fn table FuncCall uses
// (internal/chsql/fnresolution.go). Only a plain, non-parameterised
// aggregate name is valid here — a Fn resolving to a chsql fnRender hook
// is rejected by the emitter, mirroring resolveAggFuncName's same
// restriction for chplan.AggFunc.
//
// Optimizer note: like ScalarSubquery, WindowExpr's sub-expressions are
// ordinary Exprs (Args, PartitionBy), so chplan.Walk's Node-only
// traversal does not need a special case for it — InspectExpr descends
// into both slices directly, unlike ScalarSubquery's embedded Node
// subtree.
type WindowExpr struct {
	Fn          Fn
	Args        []Expr
	PartitionBy []Expr
}

func (*WindowExpr) exprNode() {}

func (w *WindowExpr) Equal(other Expr) bool {
	o, ok := other.(*WindowExpr)
	if !ok || w.Fn != o.Fn || len(w.Args) != len(o.Args) || len(w.PartitionBy) != len(o.PartitionBy) {
		return false
	}
	for i := range w.Args {
		if !w.Args[i].Equal(o.Args[i]) {
			return false
		}
	}
	for i := range w.PartitionBy {
		if !w.PartitionBy[i].Equal(o.PartitionBy[i]) {
			return false
		}
	}
	return true
}
