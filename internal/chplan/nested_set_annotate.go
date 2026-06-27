package chplan

// Synthetic column names the NestedSetAnnotate node appends to its
// input's row shape. The `__cerberus_` prefix keeps them out of the
// OTel-CH column namespace; consumers (the TraceQL `| select(...)`
// lowering and the Tempo /api/search wrap projection) reference them
// via ColumnRef like any other column.
const (
	// NestedSetLeftColumn carries the span's nested-set left bound
	// (Int64; 0 when the span is not part of a numbered tree).
	NestedSetLeftColumn = "__cerberus_ns_left"
	// NestedSetRightColumn carries the span's nested-set right bound
	// (Int64; 0 when unnumbered).
	NestedSetRightColumn = "__cerberus_ns_right"
	// NestedSetParentColumn carries the parent span's nested-set left
	// bound (Int64; -1 for root spans, 0 when unnumbered) — Tempo's
	// `nestedSetParent` ("the left bound of the parent serves as
	// numeric span ID", tempodb/encoding/vparquet4/nested_set_model.go).
	NestedSetParentColumn = "__cerberus_ns_parent"
)

// NestedSetAnnotate decorates each input span row with the three
// nested-set model values reference Tempo materialises at ingest
// (Span.NestedSetLeft / Span.NestedSetRight / Span.ParentID): a
// depth-first interval numbering of the trace's span tree, counter
// starting at 1 per trace, entry and exit each consuming one position.
//
// The OTel ClickHouse schema stores no nested-set columns, so the
// emitter recomputes the numbering at query time from the
// (TraceId, SpanId, ParentSpanId) adjacency in SpansTable: a recursive
// CTE walks every tree rooted at a root span (ParentSpanId = ”) of
// the traces present in Input (the emitter scopes the walk by a cheap
// plan-derived superset of Input's trace ids so Input's own subquery
// is evaluated only once; extra traces never join back, see
// internal/chsql traceScopeFrag), derives each span's DFS path, and
// converts pre-order rank + depth + subtree size into the exact
// entry/exit bounds Tempo's assignNestedSetModelBoundsAndServiceStats
// produces. See internal/chsql/nested_set_annotate.go for the SQL
// shape and the semantics contract (root parent = -1, disconnected
// spans = 0/0/0, counter continues across multiple roots).
//
// Output schema: every Input column, plus NestedSetLeftColumn /
// NestedSetRightColumn / NestedSetParentColumn (Int64). Input must
// expose TraceIDColumn and SpanIDColumn (the join keys back to the
// numbering); all four schema column names refer to SpansTable's
// canonical columns used by the numbering walk.
type NestedSetAnnotate struct {
	Input Node

	// SpansTable is the span table the numbering walk reads — the same
	// table Input ultimately scans. The walk deliberately ignores the
	// query's time window: reference Tempo numbers the WHOLE trace at
	// ingest, so spans outside the search window still occupy
	// positions.
	SpansTable string

	TraceIDColumn      string
	SpanIDColumn       string
	ParentSpanIDColumn string
	// TimestampColumn orders siblings (ties broken by a deterministic
	// SpanId hash). Reference Tempo's sibling order is ingest order,
	// which the OTel-CH schema does not record; start-time order is
	// the closest observable equivalent and yields a valid nested-set
	// numbering of the same tree either way.
	TimestampColumn string

	// TraceLimit bounds the numbering walk to the N newest traces (by
	// root-span Timestamp, descending, ties by TraceId ascending) — the
	// exact selection /api/search's TruncateSummaries keeps. 0 leaves the
	// walk unbounded (every trace the input touches gets numbered), which
	// is the behaviour for callers that don't return a bounded trace set
	// (metrics pipelines, tests, the property harness).
	//
	// The bound only narrows the recursive numbering's trace universe; the
	// outer LEFT JOIN still drops numbering rows for traces the input never
	// produced, and spans of traces outside the top-N fall back to the
	// 0/0/0 unnumbered values — which the search response discards anyway.
	// It is only ever set when the input plan guarantees each returned
	// trace's root span is in the result (so root-Timestamp ranking equals
	// TruncateSummaries' result-min-Timestamp ranking — see
	// traceql.lowerSelect's inputGuaranteesRootInResult gate); for every
	// other shape it stays 0 and the numbering is byte-identical to today.
	TraceLimit int64

	// WindowStartNano / WindowEndNano (when non-zero, set alongside TraceLimit
	// on the search path) restrict the TraceLimit top-N root ranking to roots
	// whose start time falls in the request window — so the numbering scope is
	// the newest-N roots IN the window, not the newest-N ever. They must match
	// the sibling BoundedTraceScope.Window* (the leaf gate) exactly, because
	// boundedRootScopeFrag emits both and a divergence strands kept rows at the
	// 0/0/0 LEFT-JOIN default. Note this windows the SELECTION of which traces
	// to number, NOT the numbering walk itself, which still reads whole traces
	// (Tempo numbers at ingest regardless of the search window).
	WindowStartNano int64
	WindowEndNano   int64
}

func (*NestedSetAnnotate) planNode() {}

func (n *NestedSetAnnotate) Children() []Node { return []Node{n.Input} }

func (n *NestedSetAnnotate) Equal(other Node) bool {
	o, ok := other.(*NestedSetAnnotate)
	if !ok {
		return false
	}
	if n.SpansTable != o.SpansTable ||
		n.TraceIDColumn != o.TraceIDColumn ||
		n.SpanIDColumn != o.SpanIDColumn ||
		n.ParentSpanIDColumn != o.ParentSpanIDColumn ||
		n.TimestampColumn != o.TimestampColumn ||
		n.TraceLimit != o.TraceLimit ||
		n.WindowStartNano != o.WindowStartNano ||
		n.WindowEndNano != o.WindowEndNano {
		return false
	}
	return n.Input.Equal(o.Input)
}
