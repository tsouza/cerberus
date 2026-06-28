package chplan

// BoundedTraceScope is a predicate-position expression that renders
//
//	<TraceIDColumn> IN (<top-N newest root-bearing traces>)
//
// where the right-hand subquery ranks every root span (ParentSpanId == "")
// in the spans table by start time (min Timestamp) descending, TraceId
// ascending, and keeps the top TraceLimit. It is the SAME set the sibling
// NestedSetAnnotate.TraceLimit numbers (boundedRootScopeFrag emits both), so
// the numbering and the gated row source see an identical trace universe.
//
// The traceql stamping pass (stampNestedSetTraceLimit) ANDs one shared
// BoundedTraceScope into every LEAF Filter of a structure-tab row source so
// the structural recursive closures — seeded via the #77 seed re-render of
// those leaves — scan only the top-N traces instead of the whole window. See
// internal/traceql/search_limit.go (pushLeafPredicate) and
// internal/chsql/nested_set_annotate.go (boundedRootScopeFrag).
//
// WindowStartNano / WindowEndNano (when non-zero) restrict the top-N root
// ranking to roots whose start time falls in the request window, so the
// structure tab ranks the newest-N roots IN the window rather than the
// newest-N ever. Without them, a historical-window search would gate the row
// source to globally-newest roots that fall outside the window — an empty
// result (#1109 GAP-3 / the structure-tab rank-in-window fix). Both bounds must
// match the sibling NestedSetAnnotate.Window* exactly, since boundedRootScopeFrag
// emits both the numbering scope and this leaf gate and they must stay
// byte-identical (a mismatch strands kept rows at the 0/0/0 LEFT-JOIN default).
//
// It is a PURE LEAF: it carries no embedded Node (only the column names + the
// limit + window needed to re-derive the self-contained subquery at emit time),
// so InspectExpr has nothing to recurse into and the optimizer's predicate
// classifier treats it as an opaque, non-cheap conjunct that always stays in
// WHERE (never promoted to PREWHERE, which cannot wrap a subquery). TraceLimit
// is always > 0 when a BoundedTraceScope is present.
type BoundedTraceScope struct {
	SpansTable         string
	TraceIDColumn      string
	ParentSpanIDColumn string
	TimestampColumn    string
	TraceLimit         int64
	WindowStartNano    int64
	WindowEndNano      int64
}

func (*BoundedTraceScope) exprNode() {}

func (b *BoundedTraceScope) Equal(other Expr) bool {
	o, ok := other.(*BoundedTraceScope)
	return ok &&
		b.SpansTable == o.SpansTable &&
		b.TraceIDColumn == o.TraceIDColumn &&
		b.ParentSpanIDColumn == o.ParentSpanIDColumn &&
		b.TimestampColumn == o.TimestampColumn &&
		b.TraceLimit == o.TraceLimit &&
		b.WindowStartNano == o.WindowStartNano &&
		b.WindowEndNano == o.WindowEndNano
}
