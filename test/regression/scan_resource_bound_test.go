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
