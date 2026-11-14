package chplan

import "time"

// TimestampBound builds `<tsCol> <op> fromUnixTimestamp64Nano(<t.UnixNano()>)`
// — a nanosecond-precision comparison against a DateTime64(9) timestamp column,
// the precision the OTel-CH traces schema stores. It is the one constructor of
// the window conjuncts PushLeafPredicate carries into a TraceQL plan's leaves:
// the search request window and the metrics RangeWindow input bound
// (RangeWindowInputBound) both build theirs here.
func TimestampBound(op BinaryOp, t time.Time, tsCol string) Expr {
	return &Binary{
		Op:   op,
		Left: &ColumnRef{Name: tsCol},
		Right: &FuncCall{
			Fn:   FnFromUnixNanos,
			Args: []Expr{&LitInt{V: t.UnixNano()}},
		},
	}
}

// PushLeafPredicate ANDs pred into every leaf Filter/Scan of a TraceQL span row
// source, descending the full node spine a TraceQL plan can produce (the union
// / structural / project / limit / nested-set / AGGREGATE spine over
// Filter(Scan) / Scan leaves). A bare Scan is wrapped in a Filter; a
// Filter(Scan) gets pred conjoined; every interior node recurses into all
// children via the exhaustively-tested RewriteChildren. A Filter or
// TraceLookupFilter predicate's trace-cohort subqueries (CohortSubqueries) take
// pred too, because a Node behind an Expr slot is invisible to
// RewriteChildren.
//
// It mutates the Filters it reaches and returns the rewritten root, so the
// caller passes a tree it solely owns. pred is shared, not copied, by every
// leaf it reaches, so every leaf emits identical SQL.
//
// Its callers:
//   - the /api/search stamps in internal/traceql/search_limit.go: the
//     BoundedTraceScope gate (`TraceId IN topN`) and the request-window fold
//     (`Timestamp BETWEEN start AND end`) over a compound / structural /
//     nested-set search row source, and the request window over the per-trace
//     lookups of a plain-search TraceLookupFilter. They run only on span search
//     plans (searchTraceLimit(ctx) > 0), never on a metrics row source;
//   - WindowPerTraceSubqueries, which pushes a metrics RangeWindow's input
//     bound into the per-trace subqueries of a metrics pipeline only — each
//     TraceLookupFilter lookup and each cohort subquery — never into the
//     metrics row source itself.
//
// The generic recurse descends NestedSetAnnotate.Input (the row source) so a
// `select(nestedSet*)` compound search gets its leaves gated/windowed; the
// numbering CTE the emitter synthesises from NestedSetAnnotate is NOT a chplan
// child, so this leaf pass never reaches it — the traceql
// stampRecursiveScanWindow pass windows that synthetic anchor/step scan
// directly instead.
func PushLeafPredicate(n Node, pred Expr) Node {
	switch v := n.(type) {
	case *Scan:
		// Leaf: wrap in a Filter carrying the predicate.
		return &Filter{Input: v, Predicate: pred}
	case *Filter:
		// A spanset-aggregate operand carries its trace cohort in an
		// InSubquery predicate — a Node reachable only through an Expr slot,
		// which RewriteChildren cannot see by construction (in_subquery.go).
		// Descend it explicitly, or the cohort aggregation scans full
		// retention behind an inert membership test.
		PushLeafPredicateIntoCohort(v.Predicate, pred)
		// A Filter directly on a Scan conjoins (one Filter); otherwise recurse.
		if _, ok := v.Input.(*Scan); ok {
			v.Predicate = conjoinScanPred(v.Predicate, pred)
			return v
		}
		v.Input = PushLeafPredicate(v.Input, pred)
		return v
	case *TraceLookupFilter:
		// Same Expr-slot blind spot as the Filter arm: a trace-scoped
		// comparison against a literal, OR'd with a row comparison
		// (`{ rootName = name || traceDuration > 1s }`), keeps its per-trace
		// membership subquery in this predicate. The children — the row
		// source and every lookup relation — take the generic recurse.
		PushLeafPredicateIntoCohort(v.Predicate, pred)
		out, _ := RewriteChildren(v, func(c Node) (Node, bool) {
			return PushLeafPredicate(c, pred), true
		})
		return out
	case *SearchTraceLimit:
		// Already fully windowed: the search lowering builds this node with
		// the window already folded onto its plain-search leaves, before its
		// compound-shape window fold runs. Recursing here would double-fold
		// the predicate (TestSearchWindow_PlainNotDoubleFolded). This is an
		// explicit "already handled", not a silent drop — every OTHER node
		// still recurses.
		return n
	default:
		// Every interior node recurses into ALL its children via the
		// exhaustively-tested generic rewrite, so no node type — Aggregate
		// (a `| count() > N` search), joins, unions, or a future addition —
		// can hit a silent default that drops the predicate onto an all-time
		// scan.
		out, _ := RewriteChildren(n, func(c Node) (Node, bool) {
			return PushLeafPredicate(c, pred), true
		})
		return out
	}
}

// PushLeafPredicateIntoCohort ANDs pred into the leaves of every trace-cohort
// subquery predicate carries, so a per-trace subquery — a spanset-aggregate
// operand's cohort, a trace-scoped intrinsic's membership test — is bounded by
// the same window / top-N gate as the span rows it selects.
func PushLeafPredicateIntoCohort(predicate, pred Expr) {
	for _, in := range CohortSubqueries(predicate) {
		in.Subquery = PushLeafPredicate(in.Subquery, pred)
	}
}

// CohortSubqueries collects every InSubquery reachable from e — the per-trace
// membership test a spanset-aggregate operand or a trace-scoped intrinsic
// compared with a literal carries. The stamping walks recurse through the
// collected Subquery nodes because a Node behind an Expr slot is invisible to
// RewriteChildren by construction; InspectExpr is the maintained, exhaustive
// Expr walk, so a cohort nested under any future Expr shape is still found.
func CohortSubqueries(e Expr) []*InSubquery {
	var out []*InSubquery
	InspectExpr(e, func(x Expr) bool {
		if in, ok := x.(*InSubquery); ok && in.Subquery != nil {
			out = append(out, in)
		}
		return true
	})
	return out
}
