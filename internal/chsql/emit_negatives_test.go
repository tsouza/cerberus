package chsql_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// TestEmit_NilNode — calling Emit on a nil node returns ErrUnsupported
// instead of panicking. The handler / optimizer paths can pass a nil
// in pathological cases (e.g., an over-eager optimizer rule replaces
// a node with nil); catching it here is friendlier than crashing.
func TestEmit_NilNode(t *testing.T) {
	t.Parallel()

	_, _, err := chsql.Emit(nil)
	if err == nil {
		t.Fatalf("Emit(nil) returned nil error; expected ErrUnsupported")
	}
	if !errors.Is(err, chsql.ErrUnsupported) {
		t.Errorf("Emit(nil) returned %v; expected wrapped ErrUnsupported", err)
	}
}

// TestEmit_AggregateMissingBoth — an Aggregate with no GroupBy and no
// AggFuncs is meaningless (no output columns); emit must error
// rather than producing `SELECT FROM (...)` which CH would reject.
func TestEmit_AggregateMissingBoth(t *testing.T) {
	t.Parallel()

	plan := &chplan.Aggregate{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
	}
	_, _, err := chsql.Emit(plan)
	if err == nil {
		t.Fatalf("Emit(Aggregate with no GroupBy/AggFuncs) returned nil error")
	}
	if !errors.Is(err, chsql.ErrUnsupported) {
		t.Errorf("expected wrapped ErrUnsupported; got %v", err)
	}
}

// TestEmit_RangeWindowMissingColumns — RangeWindow needs both
// TimestampColumn and ValueColumn set; emit must error early rather
// than producing SQL with empty column references.
func TestEmit_RangeWindowMissingColumns(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		rw   *chplan.RangeWindow
	}{
		{
			"TimestampColumn unset",
			&chplan.RangeWindow{
				Input:       &chplan.Scan{Table: "otel_metrics_sum"},
				Func:        "rate",
				Range:       5 * time.Minute,
				ValueColumn: "Value", // TimestampColumn missing
			},
		},
		{
			"ValueColumn unset",
			&chplan.RangeWindow{
				Input:           &chplan.Scan{Table: "otel_metrics_sum"},
				Func:            "rate",
				Range:           5 * time.Minute,
				TimestampColumn: "TimeUnix", // ValueColumn missing
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := chsql.Emit(tc.rw)
			if err == nil {
				t.Fatalf("Emit(%s) returned nil error", tc.name)
			}
			if !errors.Is(err, chsql.ErrUnsupported) {
				t.Errorf("expected wrapped ErrUnsupported; got %v", err)
			}
		})
	}
}

// TestEmit_RangeWindowUnknownFunc — RangeWindow with a function name
// the emitter doesn't know about must error with a clear message.
func TestEmit_RangeWindowUnknownFunc(t *testing.T) {
	t.Parallel()

	plan := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "wharblgarbl",
		Range:           5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
	}
	_, _, err := chsql.Emit(plan)
	if err == nil {
		t.Fatalf("Emit(RangeWindow with unknown Func) returned nil")
	}
	if !errors.Is(err, chsql.ErrUnsupported) {
		t.Errorf("expected wrapped ErrUnsupported; got %v", err)
	}
}

// TestEmit_StructuralJoinMissingColumns — StructuralJoin needs
// TraceIDColumn / SpanIDColumn / ParentSpanIDColumn set; emit must
// reject early.
func TestEmit_StructuralJoinMissingColumns(t *testing.T) {
	t.Parallel()

	plan := &chplan.StructuralJoin{
		Left:  &chplan.Scan{Table: "otel_traces"},
		Right: &chplan.Scan{Table: "otel_traces"},
		Op:    chplan.StructuralChild,
		// All column names unset.
	}
	_, _, err := chsql.Emit(plan)
	if err == nil {
		t.Fatalf("Emit(StructuralJoin with no columns) returned nil")
	}
	if !errors.Is(err, chsql.ErrUnsupported) {
		t.Errorf("expected wrapped ErrUnsupported; got %v", err)
	}
}

// TestEmit_ColumnRefQualifier — a ColumnRef with a non-empty Qualifier
// renders `<qualifier>.<name>` (both backtick-quoted). Needed for
// referencing the right-hand side of a StructuralJoin, whose emit
// shape is `SELECT R.* FROM (L) JOIN (R) ON …` — outer projections
// must address those columns through the `R` alias.
func TestEmit_ColumnRefQualifier(t *testing.T) {
	t.Parallel()

	plan := &chplan.Project{
		Input: &chplan.Scan{Table: "otel_traces"},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Qualifier: "R", Name: "SpanName"}, Alias: "MetricName"},
			{Expr: &chplan.ColumnRef{Name: "Timestamp"}, Alias: "TimeUnix"},
		},
	}
	sql, _, err := chsql.Emit(plan)
	if err != nil {
		t.Fatalf("Emit returned unexpected error: %v", err)
	}
	want := "`R`.`SpanName`"
	if !strings.Contains(sql, want) {
		t.Errorf("emitted SQL missing qualified column ref:\n  got  %q\n  want it to contain %q", sql, want)
	}
	wantBare := "`Timestamp`"
	if !strings.Contains(sql, wantBare) {
		t.Errorf("emitted SQL missing bare column ref:\n  got  %q\n  want it to contain %q", sql, wantBare)
	}
}

// TestEmit_StructuralJoinRecursiveEmits — `>>` (descendant) and `<<`
// (ancestor) lower to a CH `WITH RECURSIVE` CTE that walks the parent
// chain inside the span table. Confirm both ops emit a recursive CTE
// header so a regression that silently falls back to the direct
// INNER-JOIN shape is caught early. (Byte-exact SQL is locked in by
// the `structural_join_descendant` / `_ancestor` txtar fixtures.)
func TestEmit_StructuralJoinRecursiveEmits(t *testing.T) {
	t.Parallel()

	for _, op := range []chplan.StructuralOp{chplan.StructuralDescendant, chplan.StructuralAncestor} {
		t.Run(string(op), func(t *testing.T) {
			t.Parallel()
			plan := &chplan.StructuralJoin{
				Left:               &chplan.Scan{Table: "otel_traces"},
				Right:              &chplan.Scan{Table: "otel_traces"},
				Op:                 op,
				TraceIDColumn:      "TraceId",
				SpanIDColumn:       "SpanId",
				ParentSpanIDColumn: "ParentSpanId",
			}
			sql, _, err := chsql.Emit(plan)
			if err != nil {
				t.Fatalf("Emit(StructuralJoin %s) unexpected error: %v", op, err)
			}
			if !strings.Contains(sql, "WITH RECURSIVE _struct_closure") {
				t.Errorf("Emit(StructuralJoin %s) did not render a WITH RECURSIVE CTE; got %q",
					op, sql)
			}
		})
	}
}

// TestEmit_StructuralJoinRecursiveBoundedDepth — MaxDepth > 0 caps the
// recursive walk via a `WHERE c._depth < N` predicate inside the
// recursive step. MaxDepth == 0 (default) is unbounded and emits no
// WHERE clause inside the CTE step.
func TestEmit_StructuralJoinRecursiveBoundedDepth(t *testing.T) {
	t.Parallel()

	plan := &chplan.StructuralJoin{
		Left:               &chplan.Scan{Table: "otel_traces"},
		Right:              &chplan.Scan{Table: "otel_traces"},
		Op:                 chplan.StructuralDescendant,
		TraceIDColumn:      "TraceId",
		SpanIDColumn:       "SpanId",
		ParentSpanIDColumn: "ParentSpanId",
		MaxDepth:           5,
	}
	sql, _, err := chsql.Emit(plan)
	if err != nil {
		t.Fatalf("Emit returned unexpected error: %v", err)
	}
	if !strings.Contains(sql, "WHERE c._depth < 5") {
		t.Errorf("Emit did not render depth cap; got %q", sql)
	}
}
