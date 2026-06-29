package chplan

import "fmt"

// This file makes "every Scan of the spans table must be resource-bounded" an
// IR-level invariant, enforced generically at the emit chokepoint rather than
// re-remembered per emitter.
//
// Background. The TraceQL whole-trace drilldown legs (root lookup, nested-set
// numbering, structural closures, metrics compare / exemplars) each read the
// otel_traces span table. Unlike the instant windowed-array leaf — which is
// fail-closed via RangeWindow.InstantScanBounded + the RequireScanTimeBound
// analyzer — these legs had no single invariant forcing their scans to be
// resource-dominated. When the bound lived only in an emit-time helper that
// silently no-ops on a zero window (the maybePush*() family), a spans Scan
// could reach ClickHouse with no partition pruning and groupArray / recursive-
// CTE the whole retention into memory (the Traces Drilldown structure tab
// never loading; the Comparison tab flaking under OOM).
//
// A spans Scan is legitimately bounded in exactly one of three ways:
//
//   - boundWindow (form-a): a request-window Timestamp predicate
//     (`Timestamp >/>=/</<= fromUnixTimestamp64Nano(...)`) that prunes
//     MergeTree partitions. This is what `{ } | count() > N` carries.
//   - boundTraceIDSet (form-b): a finite TraceId membership — a literal
//     InList (root lookup's `TraceId IN (<padded ids>)`) or a
//     BoundedTraceScope conjunct (`TraceId IN (<top-N newest traces>)`).
//   - boundMemoryStreaming: a recursive structural arm whose seed-IN was
//     legitimately dropped to avoid CH error 49 (nested recursive closures).
//     It is bounded by the recursion depth cap + the finite recursive working
//     set, NOT by a partition claim. Only the chsql per-site gate can assert
//     this shape; the IR descent below classifies the partition-bounded forms.
//
// RequireSpansScansBounded performs a generic full-tree descent — it is NOT a
// fixed shape match — finding every *Scan whose Table is the spans table and
// classifying the predicate at its nearest enclosing *Filter. A bare,
// unfiltered spans *Scan classifies as boundNone and is rejected.

// ScanBoundKind classifies how a spans-table Scan's resource consumption is
// dominated. The zero value, boundNone, means "no recognized bound" — an
// unbounded full-table scan.
type ScanBoundKind int

const (
	// boundNone is an unbounded scan: no window predicate, no finite TraceId
	// set, no memory-streaming recursive arm. This is the OOM shape.
	boundNone ScanBoundKind = iota
	// boundWindow is form-a: a request-window Timestamp predicate prunes
	// MergeTree partitions.
	boundWindow
	// boundTraceIDSet is form-b: a finite TraceId membership (literal InList
	// or BoundedTraceScope subquery) restricts the scan to a bounded set of
	// traces.
	boundTraceIDSet
	// boundMemoryStreaming is the recursive structural arm bounded by the
	// recursion depth cap + finite working set (not a partition claim). It is
	// asserted by the chsql per-site gate, not by the IR descent.
	boundMemoryStreaming
)

// String renders a ScanBoundKind for diagnostics.
func (k ScanBoundKind) String() string {
	switch k {
	case boundWindow:
		return "window"
	case boundTraceIDSet:
		return "trace-id-set"
	case boundMemoryStreaming:
		return "memory-streaming"
	default:
		return "none"
	}
}

// ScanBoundCols names the spans-schema columns the inspector recognizes inside
// a spans-scan predicate. When a column name is empty the inspector is lenient
// for that axis — it accepts a structurally-recognizable conjunct regardless of
// the column it constrains. This lets RequireSpansScansBounded run at the emit
// chokepoint (which has no schema) by deriving column names opportunistically
// from any BoundedTraceScope in the tree.
type ScanBoundCols struct {
	TraceID      string
	Timestamp    string
	ParentSpanID string
}

// scanBoundTimeFuncs is the set of CH time-literal constructors a windowed
// Timestamp bound compares against. tsBound / the search-window stamping and
// the inner-scan bound emitters all render request-window bounds as a
// comparison of the Timestamp column against one of these calls, so their
// presence on one side of a comparison is the reliable "this is a request
// window, not an attribute predicate" tell.
var scanBoundTimeFuncs = map[string]struct{}{
	"fromUnixTimestamp64Nano":  {},
	"fromUnixTimestamp64Milli": {},
	"fromUnixTimestamp64Micro": {},
	"fromUnixTimestamp":        {},
}

// SpansScanResourceBound inspects pred — the conjunction at a spans Scan's
// nearest enclosing Filter — and returns the strongest partition bound it
// proves. A nil predicate (a bare unfiltered Scan) is boundNone. It recognizes
// form-b (TraceId set) before form-a (window); boundMemoryStreaming is never
// returned here (it has no IR predicate witness — see the file comment).
func SpansScanResourceBound(pred Expr, cols ScanBoundCols) ScanBoundKind {
	if pred == nil {
		return boundNone
	}
	conjuncts := flattenConjuncts(pred)
	for _, c := range conjuncts {
		if isTraceIDSetConjunct(c, cols) {
			return boundTraceIDSet
		}
	}
	for _, c := range conjuncts {
		if isTimestampWindowConjunct(c, cols) {
			return boundWindow
		}
	}
	return boundNone
}

// flattenConjuncts splits an AND-tree into its leaf conjuncts. A non-AND
// expression is a single-element conjunction.
func flattenConjuncts(e Expr) []Expr {
	b, ok := e.(*Binary)
	if !ok || b.Op != OpAnd {
		return []Expr{e}
	}
	return append(flattenConjuncts(b.Left), flattenConjuncts(b.Right)...)
}

// isTraceIDSetConjunct reports whether c proves a finite TraceId membership:
//   - a BoundedTraceScope (self-identifying top-N subquery);
//   - a non-negated InList over the bare TraceId column (root lookup);
//   - a `TraceId = <literal>` equality (the /traces/{id} singleton set).
//
// When cols.TraceID is empty the column checks accept any bare (unqualified)
// ColumnRef — over a spans scan that is an id column, never an attribute
// predicate (attribute INs / equalities carry a MapAccess / FieldAccess left,
// not a bare ColumnRef).
func isTraceIDSetConjunct(c Expr, cols ScanBoundCols) bool {
	switch v := c.(type) {
	case *BoundedTraceScope:
		return cols.TraceID == "" || v.TraceIDColumn == cols.TraceID
	case *InList:
		if v.Negated {
			return false
		}
		return isTraceIDCol(v.Left, cols)
	case *Binary:
		if v.Op != OpEq {
			return false
		}
		return (isTraceIDCol(v.Left, cols) && isConstExpr(v.Right)) ||
			(isTraceIDCol(v.Right, cols) && isConstExpr(v.Left))
	}
	return false
}

// isTraceIDCol reports whether e is the bare (unqualified) TraceId column.
func isTraceIDCol(e Expr, cols ScanBoundCols) bool {
	ref, ok := e.(*ColumnRef)
	if !ok || ref.Qualifier != "" {
		return false
	}
	return cols.TraceID == "" || ref.Name == cols.TraceID
}

// isConstExpr reports whether e is a constant literal — the right-hand side of
// a `TraceId = <id>` singleton bound.
func isConstExpr(e Expr) bool {
	switch e.(type) {
	case *LitString, *InlineString, *LitInt, *LitFloat, *LitBool:
		return true
	}
	return false
}

// isTimestampWindowConjunct reports whether c is a request-window Timestamp
// comparison: a Lt/Le/Gt/Ge Binary with the Timestamp column on one side and a
// CH time-literal constructor (scanBoundTimeFuncs) on the other. When
// cols.Timestamp is empty any bare ColumnRef satisfies the column side.
func isTimestampWindowConjunct(c Expr, cols ScanBoundCols) bool {
	b, ok := c.(*Binary)
	if !ok {
		return false
	}
	switch b.Op {
	case OpLt, OpLe, OpGt, OpGe:
	default:
		return false
	}
	return (isTimestampCol(b.Left, cols) && isTimeLiteralCall(b.Right)) ||
		(isTimestampCol(b.Right, cols) && isTimeLiteralCall(b.Left))
}

// isTimestampCol reports whether e is the bare Timestamp column (matching
// cols.Timestamp, or any bare unqualified column when cols.Timestamp is empty).
func isTimestampCol(e Expr, cols ScanBoundCols) bool {
	ref, ok := e.(*ColumnRef)
	if !ok || ref.Qualifier != "" {
		return false
	}
	return cols.Timestamp == "" || ref.Name == cols.Timestamp
}

// isTimeLiteralCall reports whether e is a CH time-literal constructor call
// (the right-hand side of a request-window bound).
func isTimeLiteralCall(e Expr) bool {
	f, ok := e.(*FuncCall)
	if !ok {
		return false
	}
	_, known := scanBoundTimeFuncs[f.Name]
	return known
}

// ScanResourceBoundViolation is the typed error RequireSpansScansBounded
// returns when an otel_traces spans Scan would reach emit with no recognized
// resource bound (boundNone). It is returned (not panicked) so the chsql.Emit
// chokepoint can surface it as an ordinary emit error; the HTTP layer maps it
// to a 5xx rather than serving a plan that would melt ClickHouse with a
// full-retention scan.
type ScanResourceBoundViolation struct {
	// Table is the spans table whose Scan was unbounded.
	Table string
}

func (e *ScanResourceBoundViolation) Error() string {
	return fmt.Sprintf(
		"chplan: spans Scan of %q reaches emit with no resource bound — "+
			"every otel_traces scan must be partition-pruned (a request-window Timestamp "+
			"predicate or a finite TraceId set) or memory-streaming bounded (a recursive "+
			"structural arm). This is the traces-drilldown unbounded-scan bug class; the "+
			"innermost scan would read full retention. Lowering must stamp a window / "+
			"BoundedTraceScope on the leaf, or the emit site must route through fromSpansScan",
		e.Table,
	)
}

// RequireSpansScansBounded performs a generic full-tree descent over root,
// finding every *Scan whose Table equals spansTable and verifying the predicate
// at its nearest enclosing *Filter classifies as a recognized partition bound.
// A spans Scan with no enclosing filter predicate — or a predicate that proves
// no bound — yields a ScanResourceBoundViolation.
//
// spansTable == "" (PromQL / metrics emission, where the caller never sets a
// spans table on the emit context) makes this a pure no-op: the invariant is
// table-scoped to the TraceQL spans table. The descent only sees Node-tree
// scans; the emitter-synthetic recursive scans (structural closures, nested-set
// numbering) are not chplan *Scans and are gated independently by the chsql
// per-site fromSpansScan helper.
func RequireSpansScansBounded(spansTable string, root Node) error {
	if spansTable == "" || root == nil {
		return nil
	}
	cols := deriveScanBoundCols(root)
	var firstErr error
	var descend func(n Node, pred Expr, underLimit bool)
	descend = func(n Node, pred Expr, underLimit bool) {
		if firstErr != nil || n == nil {
			return
		}
		switch v := n.(type) {
		case *MetricsAggregate, *MetricsCompare, *MetricsHistogramOverTime:
			// The metrics matrix emitters bound their own inner spans scan at
			// emit time (maybePushInnerScanTimeBounds), enforced fail-closed by
			// the per-site chsql requireInnerSpansScanBound gate. Their inner
			// carries no window in the IR, so classifying it here would
			// false-reject a correct prod query — skip the subtree and let the
			// per-site gate own it.
			return
		case *Filter:
			// Accumulate conjuncts down the spine so an outer-window +
			// inner-attribute leaf (Filter_window(Filter_attr(Scan))) is still
			// recognised as windowed.
			descend(v.Input, conjoinScanPred(pred, v.Predicate), underLimit)
		case *Limit:
			// A `LIMIT N` (Count > 0) bounds the output to a top-N: over an
			// ORDER BY / Filter / Project chain it is a bounded-N priority queue
			// (O(N) memory), the /search/recent shape. The scan still reads
			// partitions but cannot buffer full retention into memory — a
			// memory-streaming bound. It does NOT survive an intervening
			// Aggregate (handled below).
			descend(v.Input, pred, underLimit || v.Count > 0)
		case *Aggregate:
			// A GROUP BY hash table is materialised in full before an outer
			// LIMIT applies, so a top-N Limit above an Aggregate does NOT bound
			// the inner scan — drop the underLimit context when crossing it.
			// The scan-level window / trace-id bound, if any, lives in the
			// Aggregate's input and re-accumulates below (e.g. boundNewestTraces'
			// `| count() > N` leaf is bounded by its own stampSearchWindow,
			// not the outer top-N Limit).
			for _, c := range n.Children() {
				descend(c, pred, false)
			}
		case *Scan:
			if v.Table != spansTable {
				return
			}
			if SpansScanResourceBound(pred, cols) != boundNone || underLimit {
				return
			}
			firstErr = &ScanResourceBoundViolation{Table: spansTable}
		default:
			// Propagate the accumulated filter predicate + limit context down
			// through non-Filter nodes so a Scan beneath an intervening
			// Project / Aggregate still sees its governing bounds.
			for _, c := range n.Children() {
				descend(c, pred, underLimit)
			}
		}
	}
	descend(root, nil, false)
	return firstErr
}

// conjoinScanPred ANDs two predicates, dropping a nil arm.
func conjoinScanPred(a, b Expr) Expr {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	default:
		return &Binary{Op: OpAnd, Left: a, Right: b}
	}
}

// deriveScanBoundCols opportunistically lifts the spans column names from the
// first BoundedTraceScope anywhere in root (predicate position included), so
// the classifier can tighten its column matching even though the emit
// chokepoint has no schema. When no BoundedTraceScope is present the returned
// cols are empty and the classifier runs in its lenient, structurally-strict
// mode.
func deriveScanBoundCols(root Node) ScanBoundCols {
	var cols ScanBoundCols
	Walk(root, func(n Node) bool {
		if cols.TraceID != "" {
			return false
		}
		f, ok := n.(*Filter)
		if !ok {
			return true
		}
		InspectExpr(f.Predicate, func(e Expr) bool {
			if bts, ok := e.(*BoundedTraceScope); ok && cols.TraceID == "" {
				cols = ScanBoundCols{
					TraceID:      bts.TraceIDColumn,
					Timestamp:    bts.TimestampColumn,
					ParentSpanID: bts.ParentSpanIDColumn,
				}
			}
			return true
		})
		return true
	})
	return cols
}
