package chplan

// This file owns the ONE enumeration of the Expr slots each concrete Node
// carries, and the deep Node traversal built on top of it.
//
// Background: [Walk] follows [Node.Children] only. A plan subtree that hangs
// off an Expr slot — [ScalarSubquery].Input, [InSubquery].Subquery — is
// invisible to it, because those subtrees are reachable only through a
// node's Expr fields, which Children() deliberately does not report. Any
// caller that asks "does this plan contain node kind X anywhere" over [Walk]
// therefore answers NO for a plan whose only X sits inside a scalar
// subquery. When the answer gates a ClickHouse setting or a resource bound,
// that silent miss is a production failure, not a missed optimisation.
//
// [WalkDeep] is that total traversal. It is deliberately a SEPARATE entry
// point from [Walk]: widening [Walk] itself would change what every
// optimizer rule, emitter and baseline sees.

// nodeExprs feeds visit every Expr n carries directly, in a stable order.
// It does NOT recurse — neither into n's children nor into the visited
// Exprs' own sub-expressions; [InspectExpr] is the Expr-side recursion and
// [WalkDeep] composes the two.
//
// EXHAUSTIVENESS CONTRACT: every concrete Node type that carries an
// Expr-bearing field MUST have an arm here. A node whose Expr slots are not
// enumerated hides every plan subtree nested inside them, which is exactly
// the blind spot [WalkDeep] exists to close. TestNodeExprs_Exhaustive
// derives the Expr-bearing fields of every Node type by reflection and
// fails when this switch does not surface one of them, so the enumeration
// cannot fall behind the IR.
func nodeExprs(n Node, visit func(Expr)) {
	switch v := n.(type) {
	case *Scan, *OneRow, *StepGrid, *NestedSetAnnotate, *SearchTraceLimit,
		*Limit, *AbsentOverTime, *RangeLWR, *RangeWindowResample,
		*MetricsSecondStage, *InfoJoin, *UnionAll, *NaryVectorSetOp,
		*CrossJoin, *StructuralJoin, *VectorJoin, *VectorSetOp, *SetOperation:
		// Nodes with no Expr-typed field. Their plan subtrees are entirely
		// reachable through Children().
	case *Filter:
		visitExpr(v.Predicate, visit)
	case *Project:
		visitProjections(v.Projections, visit)
		visitProjections(v.Replacements, visit)
	case *Aggregate:
		visitExprs(v.GroupBy, visit)
		visitAggFuncs(v.AggFuncs, visit)
		visitExpr(v.Having, visit)
	case *RangeWindow:
		visitExprs(v.GroupBy, visit)
		visitExprs(v.ScalarExprs, visit)
	case *RangeWindowNative:
		visitExprs(v.GroupBy, visit)
		visitProjections(v.Recollapse, visit)
	case *OrderBy:
		for _, k := range v.Keys {
			visitExpr(k.Expr, visit)
		}
	case *TopK:
		visitExprs(v.By, visit)
		visitExpr(v.SortExpr, visit)
	case *RangeBucketFanout:
		visitExprs(v.GroupBy, visit)
		visitAggFuncs(v.AggFuncs, visit)
	case *HistogramQuantile:
		visitExpr(v.PhiExpr, visit)
		visitExprs(v.GroupBy, visit)
	case *HistogramQuantileNative:
		visitExpr(v.PhiExpr, visit)
		visitExprs(v.GroupBy, visit)
	case *HistogramProjection:
		visitExprs(v.GroupBy, visit)
	case *MetricsAggregate:
		visitExpr(v.Attr, visit)
		visitExprs(v.GroupBy, visit)
	case *MetricsHistogramOverTime:
		visitExpr(v.Attr, visit)
		visitExprs(v.GroupBy, visit)
	case *MetricsCompare:
		visitExpr(v.Selection, visit)
		visitExpr(v.Pairs, visit)
	}
}

// visitExpr feeds a single optional Expr slot to visit, skipping the nil
// slots the IR uses to mean "absent" (Aggregate.Having, TopK.SortExpr, ...).
func visitExpr(e Expr, visit func(Expr)) {
	if e != nil {
		visit(e)
	}
}

// visitExprs feeds every non-nil entry of an []Expr slot to visit.
func visitExprs(es []Expr, visit func(Expr)) {
	for _, e := range es {
		visitExpr(e, visit)
	}
}

// visitProjections feeds the Expr of every entry of a []Projection slot to
// visit.
func visitProjections(ps []Projection, visit func(Expr)) {
	for _, p := range ps {
		visitExpr(p.Expr, visit)
	}
}

// visitAggFuncs feeds the parameter and argument Exprs of every entry of an
// []AggFunc slot to visit.
func visitAggFuncs(fns []AggFunc, visit func(Expr)) {
	for _, fn := range fns {
		visitExprs(fn.Params, visit)
		visitExprs(fn.Args, visit)
	}
}

// InspectNodeExprs feeds visit every Expr that n carries in one of its own
// Expr slots. It is the exported read side of the single Expr-slot
// enumeration, for callers outside chplan that sweep a plan's expressions
// without wanting a whole [WalkDeep]. Like nodeExprs it does not recurse:
// pair it with [InspectExpr] / [InspectExprNodes] to reach sub-expressions
// and the plan subtrees nested inside them.
func InspectNodeExprs(n Node, visit func(Expr)) {
	if n == nil {
		return
	}
	nodeExprs(n, visit)
}

// WalkDeep visits n and every Node reachable from it in depth-first
// pre-order, following BOTH [Node.Children] and the plan subtrees embedded
// in Expr slots ([ScalarSubquery].Input, [InSubquery].Subquery, at any
// depth inside any of the node's own expressions). The visit function
// returns false to skip a node's subtree — its children AND its
// Expr-embedded subtrees alike.
//
// Use WalkDeep wherever missing a node kind is a CORRECTNESS or
// RESOURCE-BOUND failure: an experimental-function setting that must be
// stamped on the dispatch, a sample budget that must count an anchor grid,
// a memory bound that must fire. [Walk] remains the right traversal for
// callers that reason about the row-flow spine alone.
func WalkDeep(n Node, visit func(Node) bool) {
	if n == nil || !visit(n) {
		return
	}
	for _, c := range n.Children() {
		WalkDeep(c, visit)
	}
	nodeExprs(n, func(e Expr) {
		InspectExprNodes(e, func(Expr) bool { return true }, func(sub Node) {
			WalkDeep(sub, visit)
		})
	})
}
