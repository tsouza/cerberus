package chplan

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// This file exercises the two IR-level scan-bound invariants — the instant
// windowed-array leaf mark (scan_time_bound.go) and the spans-scan resource
// bound (scan_resource_bound.go). Both are fail-closed safety gates: the
// interesting behaviour is which shapes they REJECT, so every case below
// pins an accept/reject decision rather than merely walking the tree.

const (
	spansTable    = "otel_traces"
	metricsTable  = "otel_metrics_gauge"
	traceIDCol    = "TraceId"
	timestampCol  = "Timestamp"
	parentSpanCol = "ParentSpanId"
)

func spansScan() *Scan { return &Scan{Table: spansTable} }

func windowPred() Expr {
	return &Binary{
		Op:    OpGe,
		Left:  &ColumnRef{Name: timestampCol},
		Right: &FuncCall{Name: "fromUnixTimestamp64Nano", Args: []Expr{&LitInt{V: 1}}},
	}
}

func traceIDInPred() Expr {
	return &InList{
		Left: &ColumnRef{Name: traceIDCol},
		List: []Expr{&LitString{V: "abc"}},
	}
}

func btsPred() Expr {
	return &BoundedTraceScope{
		SpansTable:         spansTable,
		TraceIDColumn:      traceIDCol,
		ParentSpanIDColumn: parentSpanCol,
		TimestampColumn:    timestampCol,
		TraceLimit:         7,
	}
}

// =================================================================
// SpansScanResourceBound — the predicate classifier
// =================================================================

func TestSpansScanResourceBound_Classification(t *testing.T) {
	attrPred := &Binary{
		Op:    OpEq,
		Left:  &FieldAccess{Source: &ColumnRef{Name: "SpanAttributes"}, Path: "http.method"},
		Right: &LitString{V: "GET"},
	}
	cases := []struct {
		name string
		pred Expr
		cols ScanBoundCols
		want ScanBoundKind
	}{
		{"nil predicate is unbounded", nil, ScanBoundCols{}, boundNone},
		{"attribute equality proves nothing", attrPred, ScanBoundCols{}, boundNone},
		{"window comparison", windowPred(), ScanBoundCols{}, boundWindow},
		{"literal TraceId IN", traceIDInPred(), ScanBoundCols{}, boundTraceIDSet},
		{"BoundedTraceScope", btsPred(), ScanBoundCols{}, boundTraceIDSet},
		{
			"TraceId equality singleton",
			&Binary{Op: OpEq, Left: &ColumnRef{Name: traceIDCol}, Right: &LitString{V: "abc"}},
			ScanBoundCols{TraceID: traceIDCol},
			boundTraceIDSet,
		},
		{
			"reversed TraceId equality",
			&Binary{Op: OpEq, Left: &LitString{V: "abc"}, Right: &ColumnRef{Name: traceIDCol}},
			ScanBoundCols{TraceID: traceIDCol},
			boundTraceIDSet,
		},
		{
			"constant-false wins over everything else in the conjunction",
			&Binary{Op: OpAnd, Left: &LitBool{V: false}, Right: windowPred()},
			ScanBoundCols{},
			boundEmpty,
		},
		{
			"constant-TRUE is not a bound",
			&Binary{Op: OpAnd, Left: &LitBool{V: true}, Right: attrPred},
			ScanBoundCols{},
			boundNone,
		},
		{
			"trace-id set is reported ahead of a window in the same conjunction",
			&Binary{Op: OpAnd, Left: windowPred(), Right: traceIDInPred()},
			ScanBoundCols{},
			boundTraceIDSet,
		},
		{
			"window buried in a nested AND tree is still found",
			&Binary{
				Op:    OpAnd,
				Left:  &Binary{Op: OpAnd, Left: attrPred, Right: windowPred()},
				Right: attrPred,
			},
			ScanBoundCols{},
			boundWindow,
		},
		{
			"NOT IN over TraceId is not a finite set",
			&InList{Left: &ColumnRef{Name: traceIDCol}, List: []Expr{&LitString{V: "a"}}, Negated: true},
			ScanBoundCols{},
			boundNone,
		},
		{
			"IN over a different column is rejected once the column is known",
			&InList{Left: &ColumnRef{Name: "SpanId"}, List: []Expr{&LitString{V: "a"}}},
			ScanBoundCols{TraceID: traceIDCol},
			boundNone,
		},
		{
			"qualified TraceId is not the bare column",
			&InList{Left: &ColumnRef{Name: traceIDCol, Qualifier: "t"}, List: []Expr{&LitString{V: "a"}}},
			ScanBoundCols{TraceID: traceIDCol},
			boundNone,
		},
		{
			"BoundedTraceScope over a different trace column is rejected",
			&BoundedTraceScope{TraceIDColumn: "OtherId", TraceLimit: 3},
			ScanBoundCols{TraceID: traceIDCol},
			boundNone,
		},
		{
			"equality against a non-constant is not a singleton",
			&Binary{Op: OpEq, Left: &ColumnRef{Name: traceIDCol}, Right: &ColumnRef{Name: "SpanId"}},
			ScanBoundCols{TraceID: traceIDCol},
			boundNone,
		},
		{
			"Timestamp compared against a plain literal is not a request window",
			&Binary{Op: OpGe, Left: &ColumnRef{Name: timestampCol}, Right: &LitInt{V: 1}},
			ScanBoundCols{Timestamp: timestampCol},
			boundNone,
		},
		{
			"Timestamp equality is not a window comparison",
			&Binary{
				Op:    OpEq,
				Left:  &ColumnRef{Name: timestampCol},
				Right: &FuncCall{Name: "fromUnixTimestamp64Nano", Args: []Expr{&LitInt{V: 1}}},
			},
			ScanBoundCols{Timestamp: timestampCol},
			boundNone,
		},
		{
			"window with the time literal on the left is still a window",
			&Binary{
				Op:    OpLt,
				Left:  &FuncCall{Name: "fromUnixTimestamp", Args: []Expr{&LitInt{V: 1}}},
				Right: &ColumnRef{Name: timestampCol},
			},
			ScanBoundCols{Timestamp: timestampCol},
			boundWindow,
		},
		{
			"an unrecognised time constructor is not a window",
			&Binary{
				Op:    OpGe,
				Left:  &ColumnRef{Name: timestampCol},
				Right: &FuncCall{Name: "now64", Args: []Expr{&LitInt{V: 9}}},
			},
			ScanBoundCols{Timestamp: timestampCol},
			boundNone,
		},
		{
			"window over a non-Timestamp column is rejected once the column is known",
			&Binary{
				Op:    OpGe,
				Left:  &ColumnRef{Name: "Duration"},
				Right: &FuncCall{Name: "fromUnixTimestamp64Nano", Args: []Expr{&LitInt{V: 1}}},
			},
			ScanBoundCols{Timestamp: timestampCol},
			boundNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SpansScanResourceBound(tc.pred, tc.cols); got != tc.want {
				t.Fatalf("SpansScanResourceBound = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestScanBoundKind_String(t *testing.T) {
	want := map[ScanBoundKind]string{
		boundNone:            "none",
		boundWindow:          "window",
		boundTraceIDSet:      "trace-id-set",
		boundMemoryStreaming: "memory-streaming",
		boundEmpty:           "empty",
	}
	for k, s := range want {
		if got := k.String(); got != s {
			t.Fatalf("ScanBoundKind(%d).String() = %q, want %q", int(k), got, s)
		}
	}
}

func TestTraceIDSetCardinality_ReportsTheSetSize(t *testing.T) {
	cols := ScanBoundCols{TraceID: traceIDCol}
	cases := []struct {
		name string
		expr Expr
		want int64
		ok   bool
	}{
		{"BoundedTraceScope reports its own top-N", btsPred(), 7, true},
		{
			"literal InList reports the list length",
			&InList{Left: &ColumnRef{Name: traceIDCol}, List: []Expr{
				&LitString{V: "a"}, &LitString{V: "b"}, &LitString{V: "c"},
			}},
			3, true,
		},
		{
			"equality is a singleton",
			&Binary{Op: OpEq, Left: &ColumnRef{Name: traceIDCol}, Right: &LitString{V: "a"}},
			1, true,
		},
		{
			"a non-equality comparison is not a set",
			&Binary{Op: OpGt, Left: &ColumnRef{Name: traceIDCol}, Right: &LitString{V: "a"}},
			0, false,
		},
		{"an unrelated expression is not a set", &ColumnRef{Name: traceIDCol}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := TraceIDSetCardinality(tc.expr, cols)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("TraceIDSetCardinality = (%d, %v), want (%d, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// =================================================================
// RequireSpansScansBounded — the tree descent
// =================================================================

func TestRequireSpansScansBounded_RejectsAnUnfilteredSpansScan(t *testing.T) {
	err := RequireSpansScansBounded(spansTable, spansScan())
	var v *ScanResourceBoundViolation
	if !errors.As(err, &v) {
		t.Fatalf("want *ScanResourceBoundViolation, got %v", err)
	}
	if v.Table != spansTable {
		t.Fatalf("violation names table %q, want %q", v.Table, spansTable)
	}
	if !strings.Contains(v.Error(), spansTable) {
		t.Fatalf("error text does not name the table: %s", v.Error())
	}
}

func TestRequireSpansScansBounded_EmptyTableNameIsANoOp(t *testing.T) {
	// PromQL / metrics emission never sets a spans table; the invariant must
	// not fire on those plans even though the tree holds a bare Scan.
	if err := RequireSpansScansBounded("", spansScan()); err != nil {
		t.Fatalf("empty spansTable must be a no-op, got %v", err)
	}
}

func TestRequireSpansScansBounded_NilRootIsANoOp(t *testing.T) {
	if err := RequireSpansScansBounded(spansTable, nil); err != nil {
		t.Fatalf("nil root must be a no-op, got %v", err)
	}
}

func TestRequireSpansScansBounded_OtherTablesAreNotGoverned(t *testing.T) {
	if err := RequireSpansScansBounded(spansTable, &Scan{Table: metricsTable}); err != nil {
		t.Fatalf("a non-spans scan must not be gated, got %v", err)
	}
}

func TestRequireSpansScansBounded_AcceptsEachBoundedForm(t *testing.T) {
	for _, tc := range []struct {
		name string
		pred Expr
	}{
		{"window", windowPred()},
		{"trace-id set", traceIDInPred()},
		{"bounded trace scope", btsPred()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := &Filter{Input: spansScan(), Predicate: tc.pred}
			if err := RequireSpansScansBounded(spansTable, root); err != nil {
				t.Fatalf("bounded scan rejected: %v", err)
			}
		})
	}
}

func TestRequireSpansScansBounded_AccumulatesConjunctsDownTheFilterSpine(t *testing.T) {
	// Filter_window(Filter_attr(Scan)): neither filter alone is decisive at
	// the leaf, but the accumulated conjunction is windowed.
	// A map-attribute equality: unlike a bare `col = literal` it is not a
	// TraceId singleton in either lenient or strict mode.
	attr := &Binary{
		Op:    OpEq,
		Left:  &FieldAccess{Source: &ColumnRef{Name: "SpanAttributes"}, Path: "http.method"},
		Right: &LitString{V: "GET"},
	}
	root := &Filter{
		Predicate: windowPred(),
		Input:     &Filter{Predicate: attr, Input: spansScan()},
	}
	if err := RequireSpansScansBounded(spansTable, root); err != nil {
		t.Fatalf("outer window must reach the leaf: %v", err)
	}
	// Same shape without the outer window is still a violation — proving the
	// accumulation above is what saved it.
	bare := &Filter{Predicate: attr, Input: spansScan()}
	if err := RequireSpansScansBounded(spansTable, bare); err == nil {
		t.Fatal("attribute-only filter must not satisfy the bound")
	}
}

func TestRequireSpansScansBounded_LimitIsAMemoryStreamingBound(t *testing.T) {
	root := &Limit{Count: 20, Input: &Project{Input: spansScan()}}
	if err := RequireSpansScansBounded(spansTable, root); err != nil {
		t.Fatalf("top-N Limit must bound the scan: %v", err)
	}
	// A zero Count is "no limit" and must not be mistaken for a top-N.
	zero := &Limit{Count: 0, Input: &Project{Input: spansScan()}}
	if err := RequireSpansScansBounded(spansTable, zero); err == nil {
		t.Fatal("LIMIT 0 must not count as a bound")
	}
}

func TestRequireSpansScansBounded_LimitDoesNotSurviveAnAggregate(t *testing.T) {
	// The GROUP BY hash table materialises in full before the outer LIMIT
	// applies, so the top-N above it cannot bound the inner scan.
	root := &Limit{Count: 20, Input: &Aggregate{Input: spansScan()}}
	if err := RequireSpansScansBounded(spansTable, root); err == nil {
		t.Fatal("a Limit above an Aggregate must not bound the inner scan")
	}
	// …but a bound established beneath the Aggregate still counts.
	ok := &Limit{Count: 20, Input: &Aggregate{
		Input: &Filter{Predicate: windowPred(), Input: spansScan()},
	}}
	if err := RequireSpansScansBounded(spansTable, ok); err != nil {
		t.Fatalf("window beneath the Aggregate must bound the scan: %v", err)
	}
}

func TestRequireSpansScansBounded_MetricsSubtreesAreOwnedByTheirEmitters(t *testing.T) {
	// The metrics matrix emitters bound their own inner scan at emit time, so
	// their (IR-unbounded) inner must be skipped rather than false-rejected.
	for _, root := range []Node{
		Node(&MetricsAggregate{Inner: spansScan()}),
		Node(&MetricsCompare{Inner: spansScan()}),
		Node(&MetricsHistogramOverTime{Inner: spansScan()}),
	} {
		if err := RequireSpansScansBounded(spansTable, root); err != nil {
			t.Fatalf("%T subtree must be skipped, got %v", root, err)
		}
	}
}

func TestRequireSpansScansBounded_DerivesColumnsFromABoundedTraceScope(t *testing.T) {
	// With a BoundedTraceScope in the tree the classifier learns the real
	// column names, which makes an IN over a *different* column stop counting
	// as a trace-id set. The sibling arm below would pass in lenient mode.
	strict := &Filter{
		Predicate: &Binary{Op: OpAnd, Left: btsPred(), Right: &LitBool{V: true}},
		Input: &Project{Input: &Filter{
			Predicate: &InList{Left: &ColumnRef{Name: "SpanId"}, List: []Expr{&LitString{V: "s"}}},
			Input:     &Scan{Table: spansTable, Columns: []string{"SpanId"}},
		}},
	}
	// The outer BoundedTraceScope conjunct is accumulated down the spine, so
	// the leaf is bounded regardless — assert the derivation itself instead.
	if got := deriveScanBoundCols(strict); got.TraceID != traceIDCol ||
		got.Timestamp != timestampCol || got.ParentSpanID != parentSpanCol {
		t.Fatalf("deriveScanBoundCols = %+v, want the BoundedTraceScope columns", got)
	}
	if got := deriveScanBoundCols(spansScan()); got != (ScanBoundCols{}) {
		t.Fatalf("no BoundedTraceScope must yield empty cols, got %+v", got)
	}
	// Lenient (empty-cols) mode accepts the SpanId IN; strict mode does not.
	spanIDIn := &InList{Left: &ColumnRef{Name: "SpanId"}, List: []Expr{&LitString{V: "s"}}}
	if SpansScanResourceBound(spanIDIn, ScanBoundCols{}) != boundTraceIDSet {
		t.Fatal("lenient mode must accept any bare-column IN")
	}
	if SpansScanResourceBound(spanIDIn, ScanBoundCols{TraceID: traceIDCol}) != boundNone {
		t.Fatal("strict mode must reject an IN over a non-TraceId column")
	}
}

func TestRequireSpansScansBounded_ReportsTheFirstViolationOnly(t *testing.T) {
	// Two unbounded leaves: the descent short-circuits after the first, so
	// the returned error is a single violation naming the spans table.
	root := &Project{Input: &Filter{
		Predicate: &LitBool{V: true},
		Input:     &UnionAll{Inputs: []Node{spansScan(), spansScan()}},
	}}
	err := RequireSpansScansBounded(spansTable, root)
	var v *ScanResourceBoundViolation
	if !errors.As(err, &v) {
		t.Fatalf("want *ScanResourceBoundViolation, got %v", err)
	}
}

// =================================================================
// scan_time_bound.go — the instant windowed-array leaf mark
// =================================================================

func instantWindow(input Node) *RangeWindow {
	return &RangeWindow{Input: input, Func: "rate", Range: time.Minute}
}

func TestIsInstantWindowedLeaf(t *testing.T) {
	cases := []struct {
		name string
		rw   *RangeWindow
		want bool
	}{
		{"instant over a raw scan", instantWindow(spansScan()), true},
		{
			"matrix window is not the instant leaf",
			&RangeWindow{Input: spansScan(), OuterRange: time.Hour},
			false,
		},
		{"over MetricsAggregate", instantWindow(&MetricsAggregate{Inner: spansScan()}), false},
		{"over MetricsCompare", instantWindow(&MetricsCompare{Inner: spansScan()}), false},
		{
			"over MetricsHistogramOverTime",
			instantWindow(&MetricsHistogramOverTime{Inner: spansScan()}),
			false,
		},
		{"over a Filter", instantWindow(&Filter{Input: spansScan(), Predicate: windowPred()}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsInstantWindowedLeaf(tc.rw); got != tc.want {
				t.Fatalf("IsInstantWindowedLeaf = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAttachInstantScanTimeBounds_MarksTheLeafWithoutMutatingTheInput(t *testing.T) {
	rw := instantWindow(spansScan())
	root := Node(&Project{Input: rw})

	out := AttachInstantScanTimeBounds(root)
	if out == root {
		t.Fatal("an unmarked tree must be cloned, not mutated in place")
	}
	if rw.InstantScanBounded {
		t.Fatal("the caller's tree was mutated")
	}
	marked, ok := out.(*Project).Input.(*RangeWindow)
	if !ok {
		t.Fatalf("clone reshaped the tree: %T", out.(*Project).Input)
	}
	if !marked.InstantScanBounded {
		t.Fatal("the instant windowed leaf was not marked")
	}
}

func TestAttachInstantScanTimeBounds_IsIdempotentAndCloneFree(t *testing.T) {
	rw := instantWindow(spansScan())
	rw.InstantScanBounded = true
	root := Node(&Project{Input: rw})
	if out := AttachInstantScanTimeBounds(root); out != root {
		t.Fatal("an already-marked tree must be returned unchanged, with no clone")
	}
}

func TestAttachInstantScanTimeBounds_LeavesNonLeafShapesAlone(t *testing.T) {
	// A matrix window is bounded at emit time, not by this flag: nothing to
	// mark, so the walk must return the same pointer.
	root := Node(&RangeWindow{Input: spansScan(), OuterRange: time.Hour})
	if out := AttachInstantScanTimeBounds(root); out != root {
		t.Fatal("a matrix window must not trigger a clone")
	}
	if AttachInstantScanTimeBounds(nil) != nil {
		t.Fatal("nil root must round-trip as nil")
	}
}

func TestAttachInstantScanTimeBounds_MarksEveryLeafInTheTree(t *testing.T) {
	a, b := instantWindow(spansScan()), instantWindow(spansScan())
	b.InstantScanBounded = true // one already marked, one not
	out := AttachInstantScanTimeBounds(&UnionAll{Inputs: []Node{a, b}})
	for i, c := range out.(*UnionAll).Inputs {
		rw, ok := c.(*RangeWindow)
		if !ok {
			t.Fatalf("input %d reshaped to %T", i, c)
		}
		if !rw.InstantScanBounded {
			t.Fatalf("input %d left unmarked", i)
		}
	}
}

func TestWithInstantScanTimeBound(t *testing.T) {
	rw := instantWindow(spansScan())
	got, changed := WithInstantScanTimeBound(rw)
	if !changed {
		t.Fatal("an unmarked instant leaf must report a change")
	}
	if got == rw {
		t.Fatal("the flag must be set on a copy, never on the caller's node")
	}
	if !got.InstantScanBounded || rw.InstantScanBounded {
		t.Fatalf("flag placement wrong: copy=%v original=%v", got.InstantScanBounded, rw.InstantScanBounded)
	}

	// Idempotent: a second application is a no-op returning the same pointer.
	again, changed := WithInstantScanTimeBound(got)
	if changed || again != got {
		t.Fatalf("second application must be a no-op, got changed=%v same=%v", changed, again == got)
	}

	// A non-leaf shape is never marked.
	matrix := &RangeWindow{Input: spansScan(), OuterRange: time.Hour}
	if out, changed := WithInstantScanTimeBound(matrix); changed || out != matrix {
		t.Fatal("a matrix window must be returned untouched")
	}
}
