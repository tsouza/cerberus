package regression

import (
	"errors"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// This meta-test pins the spans-scan resource-bound invariant at the chplan
// level: chplan.RequireSpansScansBounded must reject a bare (unbounded) spans
// Scan and accept the three legitimate partition-bounded leaf shapes. It is a
// static plan-walk pin — no SQL emission, no chDB — so a regression that
// weakens the classifier (e.g. accepting an attribute predicate as a trace-id
// set, or dropping the bare-scan rejection) trips here directly.

const spansTable = "otel_traces"

func tsWindowPred() chplan.Expr {
	t := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	return &chplan.Binary{
		Op:   chplan.OpGe,
		Left: &chplan.ColumnRef{Name: "Timestamp"},
		Right: &chplan.FuncCall{
			Name: "fromUnixTimestamp64Nano",
			Args: []chplan.Expr{&chplan.LitInt{V: t.UnixNano()}},
		},
	}
}

func TestRequireSpansScansBounded_RejectsBareScan(t *testing.T) {
	plan := &chplan.Scan{Table: spansTable}
	err := chplan.RequireSpansScansBounded(spansTable, plan)
	var v *chplan.ScanResourceBoundViolation
	if !errors.As(err, &v) {
		t.Fatalf("bare spans scan must be a ScanResourceBoundViolation, got %v", err)
	}
}

func TestRequireSpansScansBounded_TableScopedNoop(t *testing.T) {
	plan := &chplan.Scan{Table: spansTable}
	// Empty spans table => the invariant is a no-op (PromQL / metrics path).
	if err := chplan.RequireSpansScansBounded("", plan); err != nil {
		t.Fatalf("empty spans table must be a no-op, got %v", err)
	}
	// A scan of a different table is not under enforcement.
	if err := chplan.RequireSpansScansBounded(spansTable, &chplan.Scan{Table: "otel_metrics_gauge"}); err != nil {
		t.Fatalf("non-spans table must be a no-op, got %v", err)
	}
}

func TestRequireSpansScansBounded_AcceptsBoundedLeaves(t *testing.T) {
	cases := map[string]chplan.Expr{
		"window": tsWindowPred(),
		"traceID-inlist": &chplan.InList{
			Left: &chplan.ColumnRef{Name: "TraceId"},
			List: []chplan.Expr{&chplan.LitString{V: "abc"}, &chplan.LitString{V: "def"}},
		},
		"bounded-trace-scope": &chplan.BoundedTraceScope{
			SpansTable:         spansTable,
			TraceIDColumn:      "TraceId",
			ParentSpanIDColumn: "ParentSpanId",
			TimestampColumn:    "Timestamp",
			TraceLimit:         20,
		},
	}
	for name, pred := range cases {
		t.Run(name, func(t *testing.T) {
			plan := &chplan.Filter{Input: &chplan.Scan{Table: spansTable}, Predicate: pred}
			if err := chplan.RequireSpansScansBounded(spansTable, plan); err != nil {
				t.Fatalf("%s leaf must be accepted, got %v", name, err)
			}
		})
	}
}

func TestRequireSpansScansBounded_AccumulatesConjunctsDownSpine(t *testing.T) {
	// An outer window Filter over an inner attribute Filter over the Scan:
	// the descent must ACCUMULATE conjuncts so the window (on the outer Filter)
	// is recognised at the leaf, not replaced by the inner attribute predicate.
	attrFilter := &chplan.Filter{
		Input: &chplan.Scan{Table: spansTable},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.MapAccess{Map: &chplan.ColumnRef{Name: "SpanAttributes"}, Key: &chplan.LitString{V: "http.method"}},
			Right: &chplan.LitString{V: "GET"},
		},
	}
	windowed := &chplan.Filter{Input: attrFilter, Predicate: tsWindowPred()}
	if err := chplan.RequireSpansScansBounded(spansTable, windowed); err != nil {
		t.Fatalf("outer-window + inner-attr leaf must be accepted via conjunct accumulation, got %v", err)
	}
	// The same nesting WITHOUT the outer window is rejected (attr alone is no bound).
	var v *chplan.ScanResourceBoundViolation
	if !errors.As(chplan.RequireSpansScansBounded(spansTable, attrFilter), &v) {
		t.Fatalf("attribute-only nested filter must be rejected")
	}
}

func TestRequireSpansScansBounded_LimitTopNIsBounded(t *testing.T) {
	// /search/recent: Limit(OrderBy(Scan)) is a bounded-N top-N (O(N) memory),
	// not a full buffer — the descent recognises the enclosing Limit.
	plan := &chplan.Limit{
		Count: 20,
		Input: &chplan.OrderBy{
			Input: &chplan.Scan{Table: spansTable},
			Keys:  []chplan.OrderKey{{Expr: &chplan.ColumnRef{Name: "Timestamp"}, Desc: true}},
		},
	}
	if err := chplan.RequireSpansScansBounded(spansTable, plan); err != nil {
		t.Fatalf("Limit(OrderBy(Scan)) top-N must be accepted, got %v", err)
	}
}

func TestRequireSpansScansBounded_LimitDoesNotCrossAggregate(t *testing.T) {
	// A top-N Limit does NOT bound a scan beneath an Aggregate (the GROUP BY
	// hash table is materialised in full before the LIMIT applies), so an
	// unwindowed spans scan under Limit(Aggregate(...)) must be rejected — the
	// underLimit recognition stops at the Aggregate boundary.
	plan := &chplan.Limit{
		Count: 20,
		Input: &chplan.Aggregate{
			Input:          &chplan.Scan{Table: spansTable},
			GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "TraceId"}},
			GroupByAliases: []string{"TraceId"},
		},
	}
	var v *chplan.ScanResourceBoundViolation
	if !errors.As(chplan.RequireSpansScansBounded(spansTable, plan), &v) {
		t.Fatalf("Limit(Aggregate(bare Scan)) must be rejected — LIMIT does not bound a GROUP-BY scan")
	}

	// But the same Limit(Aggregate(...)) with a WINDOWED leaf (the
	// boundNewestTraces `| count() > N` shape) is accepted via the window.
	windowed := &chplan.Limit{
		Count: 20,
		Input: &chplan.Aggregate{
			Input: &chplan.Filter{
				Input:     &chplan.Scan{Table: spansTable},
				Predicate: tsWindowPred(),
			},
			GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "TraceId"}},
			GroupByAliases: []string{"TraceId"},
		},
	}
	if err := chplan.RequireSpansScansBounded(spansTable, windowed); err != nil {
		t.Fatalf("Limit(Aggregate(Filter_window(Scan))) must be accepted via the window, got %v", err)
	}
}

func TestRequireSpansScansBounded_SkipsMetricsEmitterInner(t *testing.T) {
	// A metrics-emitter inner scan is bounded at emit time (per-site gate), so
	// the IR descent must skip it rather than false-reject the (IR-unwindowed)
	// inner.
	plan := &chplan.RangeWindow{
		Input: &chplan.MetricsAggregate{
			Op:    chplan.MetricsOpRate,
			Inner: &chplan.Scan{Table: spansTable},
		},
		TimestampColumn: "Timestamp",
	}
	if err := chplan.RequireSpansScansBounded(spansTable, plan); err != nil {
		t.Fatalf("metrics-emitter inner must be skipped by the IR descent, got %v", err)
	}
}

func TestRequireSpansScansBounded_AcceptsConstantFalse(t *testing.T) {
	// The trace-scoped / per-event intrinsics OTel-CH does not materialise
	// (rootName / traceDuration / span:childCount / instrumentation.*) lower to
	// a StaticNil constant-false predicate, which ConstantFold collapses to a
	// bare `false`. A `WHERE false` scan reads zero rows — the tightest bound —
	// and must be accepted, not rejected as unbounded.
	bare := &chplan.Filter{
		Input:     &chplan.Scan{Table: spansTable},
		Predicate: &chplan.LitBool{V: false},
	}
	if err := chplan.RequireSpansScansBounded(spansTable, bare); err != nil {
		t.Fatalf("constant-false scan (WHERE false) must be accepted, got %v", err)
	}
	// A `false AND <window>` conjunction (pre-ConstantFold shape) is also empty.
	conj := &chplan.Filter{
		Input: &chplan.Scan{Table: spansTable},
		Predicate: &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  &chplan.LitBool{V: false},
			Right: tsWindowPred(),
		},
	}
	if err := chplan.RequireSpansScansBounded(spansTable, conj); err != nil {
		t.Fatalf("`false AND window` scan must be accepted, got %v", err)
	}
	// But a constant-TRUE predicate reads everything — still unbounded.
	allRows := &chplan.Filter{
		Input:     &chplan.Scan{Table: spansTable},
		Predicate: &chplan.LitBool{V: true},
	}
	var v *chplan.ScanResourceBoundViolation
	if !errors.As(chplan.RequireSpansScansBounded(spansTable, allRows), &v) {
		t.Fatalf("`WHERE true` scan reads full retention and must be rejected")
	}
}

func TestRequireSpansScansBounded_AcceptsTraceIDEquality(t *testing.T) {
	// /traces/{id}: a `TraceId = <id>` singleton is a finite trace set.
	plan := &chplan.Filter{
		Input: &chplan.Scan{Table: spansTable},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "TraceId"},
			Right: &chplan.LitString{V: "abc123"},
		},
	}
	if err := chplan.RequireSpansScansBounded(spansTable, plan); err != nil {
		t.Fatalf("TraceId = <id> singleton must be accepted, got %v", err)
	}
}

func TestRequireSpansScansBounded_RejectsAttributeInList(t *testing.T) {
	// An attribute IN-list (Left is a MapAccess, not a bare TraceId column) is
	// NOT a partition bound — it must not be mistaken for a trace-id set.
	pred := &chplan.InList{
		Left: &chplan.MapAccess{
			Map: &chplan.ColumnRef{Name: "SpanAttributes"},
			Key: &chplan.LitString{V: "http.method"},
		},
		List: []chplan.Expr{&chplan.LitString{V: "GET"}, &chplan.LitString{V: "POST"}},
	}
	plan := &chplan.Filter{Input: &chplan.Scan{Table: spansTable}, Predicate: pred}
	var v *chplan.ScanResourceBoundViolation
	if !errors.As(chplan.RequireSpansScansBounded(spansTable, plan), &v) {
		t.Fatalf("attribute IN-list must NOT count as a trace-id set bound")
	}
}

func TestRequireSpansScansBounded_DescendsNestedPlan(t *testing.T) {
	// A bounded leaf under an Aggregate + Project (the root-lookup shape) is
	// reached by the generic descent.
	bounded := &chplan.Aggregate{
		Input: &chplan.Filter{
			Input: &chplan.Scan{Table: spansTable},
			Predicate: &chplan.InList{
				Left: &chplan.ColumnRef{Name: "TraceId"},
				List: []chplan.Expr{&chplan.LitString{V: "abc"}},
			},
		},
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "TraceId"}},
		GroupByAliases: []string{"TraceId"},
	}
	if err := chplan.RequireSpansScansBounded(spansTable, bounded); err != nil {
		t.Fatalf("bounded scan under Aggregate must be accepted, got %v", err)
	}

	// The same shape with the bound stripped is rejected.
	unbounded := &chplan.Aggregate{
		Input:          &chplan.Scan{Table: spansTable},
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "TraceId"}},
		GroupByAliases: []string{"TraceId"},
	}
	var v *chplan.ScanResourceBoundViolation
	if !errors.As(chplan.RequireSpansScansBounded(spansTable, unbounded), &v) {
		t.Fatalf("unbounded scan under Aggregate must be rejected")
	}
}
