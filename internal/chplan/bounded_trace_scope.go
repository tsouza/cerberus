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

	// BindingAlias, when non-empty, names a single-evaluation ClickHouse
	// scalar binding that already holds the top-N trace-id array, so this
	// gate renders as `has(<BindingAlias>, <TraceIDColumn>)` instead of
	// re-deriving the whole top-N subquery here. It is stamped by
	// [BindBoundedTraceScope] at the emit chokepoint — never by a lowering
	// or an optimizer rule — and selects exactly the same trace set as the
	// unbound form, so a partially-stamped tree is a partial optimisation,
	// not a semantic difference. See internal/chsql/emit.go for the
	// `WITH (SELECT groupArray(...) FROM (<top-N>)) AS <alias>` binding the
	// alias resolves against.
	BindingAlias string
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
		b.WindowEndNano == o.WindowEndNano &&
		b.BindingAlias == o.BindingAlias
}

// SameScope reports whether b and o select the same trace set, ignoring
// which rendering form each one carries (BindingAlias). It is what
// [BindBoundedTraceScope] uses to decide that every gate in a tree may
// share one binding: the gates must agree on the table, the columns, the
// limit and the window — everything the top-N subquery is derived from.
func (b *BoundedTraceScope) SameScope(o *BoundedTraceScope) bool {
	return b.SpansTable == o.SpansTable &&
		b.TraceIDColumn == o.TraceIDColumn &&
		b.ParentSpanIDColumn == o.ParentSpanIDColumn &&
		b.TimestampColumn == o.TimestampColumn &&
		b.TraceLimit == o.TraceLimit &&
		b.WindowStartNano == o.WindowStartNano &&
		b.WindowEndNano == o.WindowEndNano
}
