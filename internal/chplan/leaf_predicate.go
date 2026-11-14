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
// source and into every per-trace subquery it carries: PushLeafWindow with
// pred as both its row and its per-trace predicate. The /api/search
// BoundedTraceScope gate (`TraceId IN topN`) takes this form, since the gate
// bounds a trace's per-trace subqueries to the same traces as its span rows.
func PushLeafPredicate(n Node, pred Expr) Node {
	return PushLeafWindow(n, pred, pred)
}

// PushLeafWindow ANDs rows into every leaf Filter/Scan of a TraceQL span row
// source, descending the full node spine a TraceQL plan can produce (the union
// / structural / project / limit / nested-set / AGGREGATE spine over
// Filter(Scan) / Scan leaves), and perTrace into every per-trace subquery the
// spine carries: each trace-cohort subquery (CohortSubqueries) of a Filter or
// TraceLookupFilter predicate, and each lookup relation of a
// TraceLookupFilter. A bare Scan is wrapped in a Filter; a Filter(Scan) gets
// rows conjoined; every interior node recurses into all children via the
// exhaustively-tested RewriteChildren. A Node behind an Expr slot is invisible
// to RewriteChildren, so the cohort subqueries are reached explicitly. Inside a
// per-trace subquery every leaf takes perTrace.
//
// rows and perTrace differ for a request window: the span rows a search
// matches are the rows inside the window, while a per-trace subquery reads its
// traces' spans inside the window widened by the configured maximum trace
// duration, so a root span or trace extent outside the window still resolves.
//
// It mutates the Filters it reaches and returns the rewritten root, so the
// caller passes a tree it solely owns. Each predicate is shared, not copied,
// by every leaf it reaches, so every leaf emits identical SQL.
//
// Its callers:
//   - the /api/search stamps in internal/traceql/search_limit.go: the
//     BoundedTraceScope gate (`TraceId IN topN`, through PushLeafPredicate)
//     and the request-window fold (`Timestamp BETWEEN start AND end`, its
//     per-trace subqueries widened) over a compound / structural / nested-set
//     search row source. They run only on span search plans
//     (searchTraceLimit(ctx) > 0), never on a metrics row source;
//   - WindowPerTraceSubqueries, which pushes a metrics RangeWindow's widened
//     input bound into the per-trace subqueries of a metrics pipeline only —
//     each TraceLookupFilter lookup and each cohort subquery — through
//     PushLeafPredicate, never into the metrics row source itself.
//
// The generic recurse descends NestedSetAnnotate.Input (the row source) so a
// `select(nestedSet*)` compound search gets its leaves gated/windowed; the
// numbering CTE the emitter synthesises from NestedSetAnnotate is NOT a chplan
// child, so this leaf pass never reaches it — the traceql
// stampRecursiveScanWindow pass windows that synthetic anchor/step scan
// directly instead.
func PushLeafWindow(n Node, rows, perTrace Expr) Node {
	switch v := n.(type) {
	case *Scan:
		// Leaf: wrap in a Filter carrying the predicate.
		return &Filter{Input: v, Predicate: rows}
	case *Filter:
		// A spanset-aggregate operand carries its trace cohort in an
		// InSubquery predicate — a Node reachable only through an Expr slot,
		// which RewriteChildren cannot see by construction (in_subquery.go).
		// Descend it explicitly, or the cohort aggregation scans full
		// retention behind an inert membership test.
		PushLeafPredicateIntoCohort(v.Predicate, perTrace)
		// A Filter directly on a Scan conjoins (one Filter); otherwise recurse.
		if _, ok := v.Input.(*Scan); ok {
			v.Predicate = conjoinScanPred(v.Predicate, rows)
			return v
		}
		v.Input = PushLeafWindow(v.Input, rows, perTrace)
		return v
	case *TraceLookupFilter:
		// Same Expr-slot blind spot as the Filter arm: a trace-scoped
		// comparison against a literal, OR'd with a row comparison
		// (`{ rootName = name || traceDuration > 1s }`), keeps its per-trace
		// membership subquery in this predicate. Each lookup relation is a
		// per-trace subquery; the row source takes the rows predicate.
		PushLeafPredicateIntoCohort(v.Predicate, perTrace)
		out := *v
		out.Lookups = make([]Node, len(v.Lookups))
		for i, lookup := range v.Lookups {
			out.Lookups[i] = PushLeafPredicate(lookup, perTrace)
		}
		out.Input = PushLeafWindow(v.Input, rows, perTrace)
		return &out
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
			return PushLeafWindow(c, rows, perTrace), true
		})
		return out
	}
}

// PushLeafPredicateIntoCohort ANDs pred into the leaves of every trace-cohort
// subquery predicate carries, so a per-trace subquery — a spanset-aggregate
// operand's cohort, a trace-scoped intrinsic's membership test — is bounded by
// pred: a per-trace window, or the top-N gate the span rows it selects carry.
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
