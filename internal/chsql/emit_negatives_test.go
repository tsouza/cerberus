package chsql_test

import (
	"errors"
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

// TestEmit_StructuralJoinRecursiveOpRejected — `>>` (descendant) and
// `<<` (ancestor) are mapped at the lowering layer but the emitter
// doesn't yet produce JOIN-on-prefix SQL. Emit must reject cleanly
// rather than producing broken SQL.
func TestEmit_StructuralJoinRecursiveOpRejected(t *testing.T) {
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
			_, _, err := chsql.Emit(plan)
			if err == nil {
				t.Fatalf("Emit(StructuralJoin %s) returned nil; recursive form should error until RC2", op)
			}
			if !errors.Is(err, chsql.ErrUnsupported) {
				t.Errorf("expected wrapped ErrUnsupported; got %v", err)
			}
		})
	}
}
