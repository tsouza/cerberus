package chplan

// BoundedTraceScopeAlias is the ClickHouse alias the top-N trace-id set is
// bound to when the emit chokepoint collapses a tree's repeated
// [BoundedTraceScope] gates into one single-evaluation binding. It is a
// synthetic name in the emitter's `__cerberus_` / `_cerberus_` namespace so
// it cannot collide with an OTel-CH column or with a user-supplied alias.
const BoundedTraceScopeAlias = "_cerberus_trace_scope"

// BindBoundedTraceScope collapses a plan's repeated [BoundedTraceScope]
// gates onto one single-evaluation ClickHouse binding.
//
// Every gate in a structure-tab row source re-derives the SAME self-contained
// top-N subquery (`TraceId IN (SELECT TraceId FROM otel_traces WHERE
// ParentSpanId = ” … GROUP BY TraceId ORDER BY min(Timestamp) DESC LIMIT
// N)`). ClickHouse shares nothing between textually separate occurrences of a
// correlated subquery — and, measured on 25.8, shares nothing between
// references to a non-recursive `WITH x AS (SELECT …)` CTE either, because it
// inlines those. It DOES evaluate a `WITH (SELECT …) AS alias` scalar binding
// exactly once. So the collapse is: stamp [BoundedTraceScope.BindingAlias] on
// every gate, and let the emitter bind the top-N once as an array-valued
// scalar the gates test with `has(...)`.
//
// Two node kinds render that subquery, and both are collapsed together: the
// leaf gates ([BoundedTraceScope] in predicate position) and the bounded
// numbering walk ([NestedSetAnnotate] with a TraceLimit, which renders its
// scope on the recursive CTE's anchor AND on its step). They must agree: the
// numbering and the row source ranging over different trace sets is what
// strands kept rows at the 0/0/0 LEFT-JOIN default, so a walk joins the
// binding only when its scope matches the gates' exactly.
//
// It returns the scope every stamped site shares — the value the caller binds
// — or nil when the tree renders no top-N at all, or when its sites do not all
// agree on the trace set (different table / columns / limit / window), in
// which case one binding could not serve them all and every site keeps its own
// subquery.
//
// A SINGLE site is worth binding. One gate in the plan is not one scan in the
// SQL: the emitter pre-renders a subplan once and REPLAYS that text at every
// splice point (emitter.subqueryFrag), so the structural closure's seed, its
// R-side and its union leg each carry their own copy of the same leaf gate.
// The binding is one evaluation regardless, which makes it neutral in the
// genuinely-rendered-once case and a win in every other.
//
// The stamp is an emit-time RENDERING annotation, not a semantic rewrite: a
// stamped site and an unstamped one select exactly the same traces, because
// the top-N ranking is total (the `TraceId ASC` tie-break) and both forms
// derive it from the same parameters. That is what makes stamping in place
// safe — it is idempotent (the alias is a constant, not a counter), so
// re-emitting the same plan (the sharded-pushdown solver emits K re-anchored
// deep copies) converges on the same SQL.
func BindBoundedTraceScope(root Node) *BoundedTraceScope {
	gates := collectBoundedTraceScopes(root)
	walks := collectBoundedNumberingWalks(root)
	scope := candidateScope(gates, walks)
	if scope == nil {
		return nil
	}
	// Agreement is checked over EVERY site before any of them is stamped.
	// Stamping as we go would leave a refused bind half-applied, and a stamped
	// site with no binding emitted around it renders `has(<alias>, …)` against
	// an alias the statement never defines — SQL ClickHouse rejects outright.
	for _, g := range gates {
		if !scope.SameScope(g) {
			return nil
		}
	}
	for _, w := range walks {
		if !scope.SameScope(numberingWalkScope(w)) {
			return nil
		}
	}
	for _, g := range gates {
		g.BindingAlias = BoundedTraceScopeAlias
	}
	for _, w := range walks {
		w.ScopeBindingAlias = BoundedTraceScopeAlias
	}
	bound := *scope
	bound.BindingAlias = BoundedTraceScopeAlias
	return &bound
}

// candidateScope picks the trace scope every site must agree on: the gates'
// when the tree has any, else the numbering walk's. It returns nil when the
// tree renders no top-N subquery at all.
func candidateScope(gates []*BoundedTraceScope, walks []*NestedSetAnnotate) *BoundedTraceScope {
	switch {
	case len(gates) > 0:
		return gates[0]
	case len(walks) > 0:
		return numberingWalkScope(walks[0])
	default:
		return nil
	}
}

// numberingWalkScope is the trace scope a TraceLimit-bounded
// [NestedSetAnnotate] derives for its own numbering walk — the same top-N
// selection a [BoundedTraceScope] gate carrying these parameters derives, which
// is the identity that lets the two share one binding.
func numberingWalkScope(nsa *NestedSetAnnotate) *BoundedTraceScope {
	return &BoundedTraceScope{
		SpansTable:         nsa.SpansTable,
		TraceIDColumn:      nsa.TraceIDColumn,
		ParentSpanIDColumn: nsa.ParentSpanIDColumn,
		TimestampColumn:    nsa.TimestampColumn,
		TraceLimit:         nsa.TraceLimit,
		WindowStartNano:    nsa.WindowStartNano,
		WindowEndNano:      nsa.WindowEndNano,
	}
}

// collectBoundedNumberingWalks returns every TraceLimit-bounded
// [NestedSetAnnotate] reachable from root. An unbounded walk (TraceLimit 0)
// renders no top-N subquery, so it is not a binding site.
func collectBoundedNumberingWalks(root Node) []*NestedSetAnnotate {
	var out []*NestedSetAnnotate
	Walk(root, func(cur Node) bool {
		if nsa, ok := cur.(*NestedSetAnnotate); ok && nsa.TraceLimit > 0 {
			out = append(out, nsa)
		}
		return true
	})
	return out
}

// collectBoundedTraceScopes returns every [BoundedTraceScope] reachable from
// root in predicate position, including the ones inside plan subtrees that
// hang off an Expr slot ([InSubquery] / [ScalarSubquery]) — the spanset-
// aggregate cohort shape, which [RewriteChildren] cannot see by construction.
//
// The same gate pointer pushed into several leaves is reported once per
// occurrence, which is what the caller counts: occurrences are what the
// emitter renders, so two leaves sharing one pointer still cost two scans.
func collectBoundedTraceScopes(root Node) []*BoundedTraceScope {
	var out []*BoundedTraceScope
	var visitExpr func(Expr)
	visitExpr = func(e Expr) {
		InspectExprNodes(e, func(x Expr) bool {
			if bts, ok := x.(*BoundedTraceScope); ok {
				out = append(out, bts)
			}
			return true
		}, func(n Node) {
			collectNodeExprs(n, visitExpr)
		})
	}
	collectNodeExprs(root, visitExpr)
	return out
}

// collectNodeExprs feeds visit every Expr reachable from n and its children.
//
// It reads the IR through nodeExprs, the single enumeration of the Expr slots
// each Node carries, rather than naming the node types that can hold a gate.
// The traceql stamping pass (pushLeafPredicate) conjoins the gate into leaf
// [Filter].Predicate slots and into the [InSubquery] cohort subplans hanging
// off them, so the enumerated superset finds exactly those today; sharing the
// enumeration means a gate that reaches any other slot is bound rather than
// silently left with its own subquery.
func collectNodeExprs(n Node, visit func(Expr)) {
	Walk(n, func(cur Node) bool {
		nodeExprs(cur, visit)
		return true
	})
}
