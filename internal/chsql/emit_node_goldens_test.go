package chsql_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// TestEmitNode_Scan_NoColumns — a Scan with an empty Columns slice
// renders `SELECT * FROM <table>`. The lowering pass relies on this
// default when a head doesn't pin a projection list.
func TestEmitNode_Scan_NoColumns(t *testing.T) {
	t.Parallel()

	sql, args, err := chsql.Emit(context.Background(), &chplan.Scan{Table: "otel_metrics_gauge"})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if want := "SELECT * FROM `otel_metrics_gauge`"; sql != want {
		t.Errorf("SQL = %q; want %q", sql, want)
	}
	if args != nil {
		t.Errorf("Args = %v; want nil", args)
	}
}

// TestEmitNode_Scan_WithColumns — a Scan with Columns renders the
// projection list in order, all backtick-quoted.
func TestEmitNode_Scan_WithColumns(t *testing.T) {
	t.Parallel()

	sql, _, err := chsql.Emit(context.Background(), &chplan.Scan{
		Table:   "otel_metrics_gauge",
		Columns: []string{"TimeUnix", "Value", "Attributes"},
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	want := "SELECT `TimeUnix`, `Value`, `Attributes` FROM `otel_metrics_gauge`"
	if sql != want {
		t.Errorf("SQL = %q; want %q", sql, want)
	}
}

// TestEmitNode_Limit_ZeroCount — a Limit with Count == 0 omits the
// LIMIT clause entirely (zero means "no limit", matching the QueryBuilder
// slot semantics).
func TestEmitNode_Limit_ZeroCount(t *testing.T) {
	t.Parallel()

	sql, _, err := chsql.Emit(context.Background(), &chplan.Limit{
		Input: &chplan.Scan{Table: "otel_logs"},
		Count: 0,
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if strings.Contains(sql, "LIMIT") {
		t.Errorf("Limit{Count:0} emitted LIMIT clause: %q", sql)
	}
}

// TestEmitNode_Limit_NegativeCount — same as zero: no LIMIT clause.
func TestEmitNode_Limit_NegativeCount(t *testing.T) {
	t.Parallel()

	sql, _, err := chsql.Emit(context.Background(), &chplan.Limit{
		Input: &chplan.Scan{Table: "otel_logs"},
		Count: -1,
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if strings.Contains(sql, "LIMIT") {
		t.Errorf("Limit{Count:-1} emitted LIMIT clause: %q", sql)
	}
}

// TestEmitNode_Limit_PositiveCount — the canonical case: a non-zero
// Count wraps the input in `SELECT * FROM (<input>) LIMIT N`.
func TestEmitNode_Limit_PositiveCount(t *testing.T) {
	t.Parallel()

	sql, _, err := chsql.Emit(context.Background(), &chplan.Limit{
		Input: &chplan.Scan{Table: "otel_logs"},
		Count: 1000,
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "LIMIT 1000") {
		t.Errorf("Limit{Count:1000} did not emit LIMIT 1000: %q", sql)
	}
}

// TestEmitNode_OrderBy_NoKeys — OrderBy with no keys is a programmer
// error and must produce ErrUnsupported. Silently dropping the sort
// intent would corrupt query results in subtle ways.
func TestEmitNode_OrderBy_NoKeys(t *testing.T) {
	t.Parallel()

	_, _, err := chsql.Emit(context.Background(), &chplan.OrderBy{
		Input: &chplan.Scan{Table: "otel_logs"},
		Keys:  nil,
	})
	if err == nil {
		t.Fatal("OrderBy with no keys did not error")
	}
	if !errors.Is(err, chsql.ErrUnsupported) {
		t.Errorf("err = %v; want wrapped ErrUnsupported", err)
	}
}

// TestEmitNode_OrderBy_SingleKeyAscIsImplicit — Desc=false renders no
// "ASC" keyword; CH defaults to ascending so the absence of "DESC" is
// the implicit ASC.
func TestEmitNode_OrderBy_SingleKeyAscIsImplicit(t *testing.T) {
	t.Parallel()

	sql, _, err := chsql.Emit(context.Background(), &chplan.OrderBy{
		Input: &chplan.Scan{Table: "otel_logs"},
		Keys: []chplan.OrderKey{
			{Expr: &chplan.ColumnRef{Name: "Timestamp"}, Desc: false},
		},
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "ORDER BY `Timestamp`") {
		t.Errorf("missing ORDER BY clause in %q", sql)
	}
	if strings.Contains(sql, "ASC") {
		t.Errorf("implicit ASC should not render as keyword: %q", sql)
	}
	if strings.Contains(sql, "DESC") {
		t.Errorf("desc=false should not render DESC: %q", sql)
	}
}

// TestEmitNode_OrderBy_DescRendersKeyword — Desc=true renders "DESC"
// after the key expression.
func TestEmitNode_OrderBy_DescRendersKeyword(t *testing.T) {
	t.Parallel()

	sql, _, err := chsql.Emit(context.Background(), &chplan.OrderBy{
		Input: &chplan.Scan{Table: "otel_logs"},
		Keys: []chplan.OrderKey{
			{Expr: &chplan.ColumnRef{Name: "Timestamp"}, Desc: true},
		},
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "ORDER BY `Timestamp` DESC") {
		t.Errorf("missing 'ORDER BY `Timestamp` DESC' in %q", sql)
	}
}

// TestEmitNode_Project_EmptyProjections — a Project with no projection
// expressions renders the input wrapped in `SELECT * FROM (<input>)`.
// The Project slot collapses to a passthrough wrapper.
func TestEmitNode_Project_EmptyProjections(t *testing.T) {
	t.Parallel()

	sql, _, err := chsql.Emit(context.Background(), &chplan.Project{
		Input:       &chplan.Scan{Table: "otel_logs"},
		Projections: nil,
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.HasPrefix(sql, "SELECT * FROM (") {
		t.Errorf("expected SELECT * FROM (...) passthrough; got %q", sql)
	}
}

// TestEmitNode_Project_PreservesProjectionOrder — Projections emit in
// slice order; aliases are backtick-quoted.
func TestEmitNode_Project_PreservesProjectionOrder(t *testing.T) {
	t.Parallel()

	sql, _, err := chsql.Emit(context.Background(), &chplan.Project{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "TimeUnix"}, Alias: "t"},
			{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "v"},
			{Expr: &chplan.ColumnRef{Name: "MetricName"}, Alias: "n"},
		},
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	want := "SELECT `TimeUnix` AS `t`, `Value` AS `v`, `MetricName` AS `n` FROM"
	if !strings.Contains(sql, want) {
		t.Errorf("expected %q in emitted SQL; got %q", want, sql)
	}
}

// TestEmitNode_Filter_PropagatesChildError — when the Filter's Input
// child triggers an error during emit, the Filter emitter must surface
// that error rather than swallowing it.
func TestEmitNode_Filter_PropagatesChildError(t *testing.T) {
	t.Parallel()

	// A nested OrderBy with no keys triggers ErrUnsupported. Wrapping
	// it in a Filter exercises the child-error propagation path.
	plan := &chplan.Filter{
		Input: &chplan.OrderBy{
			Input: &chplan.Scan{Table: "otel_logs"},
			Keys:  nil,
		},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "Body"},
			Right: &chplan.LitString{V: "x"},
		},
	}
	_, _, err := chsql.Emit(context.Background(), plan)
	if err == nil {
		t.Fatal("expected error from nested OrderBy{keys:nil}; got nil")
	}
	if !errors.Is(err, chsql.ErrUnsupported) {
		t.Errorf("err = %v; want wrapped ErrUnsupported", err)
	}
}

// TestEmitNode_Limit_PropagatesChildError — same propagation contract
// for Limit (its emitSubqueryFrag path must surface child errors, not
// produce malformed SQL).
func TestEmitNode_Limit_PropagatesChildError(t *testing.T) {
	t.Parallel()

	plan := &chplan.Limit{
		Input: &chplan.OrderBy{
			Input: &chplan.Scan{Table: "otel_logs"},
			Keys:  nil,
		},
		Count: 10,
	}
	_, _, err := chsql.Emit(context.Background(), plan)
	if err == nil {
		t.Fatal("expected error from nested OrderBy{keys:nil}; got nil")
	}
	if !errors.Is(err, chsql.ErrUnsupported) {
		t.Errorf("err = %v; want wrapped ErrUnsupported", err)
	}
}

// TestEmitNode_Project_PropagatesChildError — same propagation contract
// for Project.
func TestEmitNode_Project_PropagatesChildError(t *testing.T) {
	t.Parallel()

	plan := &chplan.Project{
		Input: &chplan.OrderBy{
			Input: &chplan.Scan{Table: "otel_logs"},
			Keys:  nil,
		},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "v"},
		},
	}
	_, _, err := chsql.Emit(context.Background(), plan)
	if err == nil {
		t.Fatal("expected error from nested OrderBy{keys:nil}; got nil")
	}
	if !errors.Is(err, chsql.ErrUnsupported) {
		t.Errorf("err = %v; want wrapped ErrUnsupported", err)
	}
}

// TestEmitNode_Aggregate_PropagatesChildError — same propagation
// contract for Aggregate.
func TestEmitNode_Aggregate_PropagatesChildError(t *testing.T) {
	t.Parallel()

	plan := &chplan.Aggregate{
		Input: &chplan.OrderBy{
			Input: &chplan.Scan{Table: "otel_metrics_gauge"},
			Keys:  nil,
		},
		AggFuncs: []chplan.AggFunc{
			{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "total"},
		},
	}
	_, _, err := chsql.Emit(context.Background(), plan)
	if err == nil {
		t.Fatal("expected error from nested OrderBy{keys:nil}; got nil")
	}
	if !errors.Is(err, chsql.ErrUnsupported) {
		t.Errorf("err = %v; want wrapped ErrUnsupported", err)
	}
}

// TestEmitNode_SetOperation_Intersect — TraceQL `A && B` lowers to
// `SELECT L.* FROM (<A>) AS L INNER JOIN (<B>) AS R
//
//	ON L.TraceId = R.TraceId AND L.SpanId = R.SpanId`.
//
// Both sides are emitted as subqueries; the ON predicate is composed
// from the configured (TraceID, SpanID) column names. The chsql
// package has no Go-level test for this shape (only the per-head
// traceql lowering test), so this is the first emit-level pin.
func TestEmitNode_SetOperation_Intersect(t *testing.T) {
	t.Parallel()

	plan := &chplan.SetOperation{
		Left:          &chplan.Scan{Table: "otel_traces"},
		Right:         &chplan.Scan{Table: "otel_traces"},
		Op:            chplan.SetIntersect,
		TraceIDColumn: "TraceId",
		SpanIDColumn:  "SpanId",
	}
	sql, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// Verify the structural shape: SELECT L.*, INNER JOIN, ON predicate.
	for _, frag := range []string{
		"SELECT L.*",
		"INNER JOIN",
		"`L`.`TraceId` = `R`.`TraceId`",
		"`L`.`SpanId` = `R`.`SpanId`",
		"AS `L`",
		"AS `R`",
	} {
		if !strings.Contains(sql, frag) {
			t.Errorf("SetIntersect SQL missing fragment %q; got %q", frag, sql)
		}
	}
	if args != nil {
		t.Errorf("Args = %v; want nil (no `?` in this shape)", args)
	}
}

// TestEmitNode_SetOperation_Union — TraceQL `A || B` lowers to
// `(<A>) UNION DISTINCT (<B>)`. Both arms render as parenthesised
// subqueries with the UNION DISTINCT keyword between them.
func TestEmitNode_SetOperation_Union(t *testing.T) {
	t.Parallel()

	plan := &chplan.SetOperation{
		Left:          &chplan.Scan{Table: "otel_traces"},
		Right:         &chplan.Scan{Table: "otel_traces"},
		Op:            chplan.SetUnion,
		TraceIDColumn: "TraceId",
		SpanIDColumn:  "SpanId",
	}
	sql, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "UNION DISTINCT") {
		t.Errorf("SetUnion did not emit UNION DISTINCT: %q", sql)
	}
	// Both arms should be wrapped in parens — UNION DISTINCT is a
	// SELECT-level binary, each arm is a complete SELECT.
	if !strings.Contains(sql, "(SELECT * FROM `otel_traces`)") {
		t.Errorf("SetUnion arms not parenthesised: %q", sql)
	}
	if args != nil {
		t.Errorf("Args = %v; want nil", args)
	}
}

// TestEmitNode_SetOperation_MissingColumns — the SetOperation emitter
// validates that TraceIDColumn / SpanIDColumn are non-empty. Without
// them the JOIN predicate would reference empty identifiers and
// produce a CH syntax error at runtime.
func TestEmitNode_SetOperation_MissingColumns(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		plan *chplan.SetOperation
	}{
		{
			"both columns unset",
			&chplan.SetOperation{
				Left:  &chplan.Scan{Table: "otel_traces"},
				Right: &chplan.Scan{Table: "otel_traces"},
				Op:    chplan.SetIntersect,
			},
		},
		{
			"trace id unset",
			&chplan.SetOperation{
				Left:         &chplan.Scan{Table: "otel_traces"},
				Right:        &chplan.Scan{Table: "otel_traces"},
				Op:           chplan.SetIntersect,
				SpanIDColumn: "SpanId",
			},
		},
		{
			"span id unset",
			&chplan.SetOperation{
				Left:          &chplan.Scan{Table: "otel_traces"},
				Right:         &chplan.Scan{Table: "otel_traces"},
				Op:            chplan.SetIntersect,
				TraceIDColumn: "TraceId",
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := chsql.Emit(context.Background(), tc.plan)
			if err == nil {
				t.Fatalf("SetOperation %s did not error", tc.name)
			}
			if !errors.Is(err, chsql.ErrUnsupported) {
				t.Errorf("err = %v; want wrapped ErrUnsupported", err)
			}
		})
	}
}

// TestEmitNode_SetOperation_UnknownOp — a SetOperation whose Op is
// neither SetIntersect nor SetUnion must surface ErrUnsupported rather
// than producing arbitrary SQL.
func TestEmitNode_SetOperation_UnknownOp(t *testing.T) {
	t.Parallel()

	plan := &chplan.SetOperation{
		Left:          &chplan.Scan{Table: "otel_traces"},
		Right:         &chplan.Scan{Table: "otel_traces"},
		Op:            chplan.SetOp("??"),
		TraceIDColumn: "TraceId",
		SpanIDColumn:  "SpanId",
	}
	_, _, err := chsql.Emit(context.Background(), plan)
	if err == nil {
		t.Fatal("unknown SetOp did not error")
	}
	if !errors.Is(err, chsql.ErrUnsupported) {
		t.Errorf("err = %v; want wrapped ErrUnsupported", err)
	}
}

// TestEmitNode_SetOperation_LeftErrorPropagates — a child error on
// the Left arm surfaces from Emit (catches a regression that swallows
// errors after the first subquery render).
func TestEmitNode_SetOperation_LeftErrorPropagates(t *testing.T) {
	t.Parallel()

	plan := &chplan.SetOperation{
		Left: &chplan.OrderBy{
			Input: &chplan.Scan{Table: "otel_traces"},
			Keys:  nil,
		},
		Right:         &chplan.Scan{Table: "otel_traces"},
		Op:            chplan.SetIntersect,
		TraceIDColumn: "TraceId",
		SpanIDColumn:  "SpanId",
	}
	_, _, err := chsql.Emit(context.Background(), plan)
	if err == nil {
		t.Fatal("expected error from left-arm child; got nil")
	}
	if !errors.Is(err, chsql.ErrUnsupported) {
		t.Errorf("err = %v; want wrapped ErrUnsupported", err)
	}
}

// TestEmitNode_SetOperation_RightErrorPropagates — same for the right arm.
func TestEmitNode_SetOperation_RightErrorPropagates(t *testing.T) {
	t.Parallel()

	plan := &chplan.SetOperation{
		Left: &chplan.Scan{Table: "otel_traces"},
		Right: &chplan.OrderBy{
			Input: &chplan.Scan{Table: "otel_traces"},
			Keys:  nil,
		},
		Op:            chplan.SetIntersect,
		TraceIDColumn: "TraceId",
		SpanIDColumn:  "SpanId",
	}
	_, _, err := chsql.Emit(context.Background(), plan)
	if err == nil {
		t.Fatal("expected error from right-arm child; got nil")
	}
	if !errors.Is(err, chsql.ErrUnsupported) {
		t.Errorf("err = %v; want wrapped ErrUnsupported", err)
	}
}

// TestEmitNode_NestedScans_BindArgsInOrder — a Filter wrapping a Scan
// (the PREWHERE-eligible shape) binds the predicate arg at the WHERE
// position. Pinning this confirms args land at `?` positions across
// the typed Frag emission, not at some other position.
func TestEmitNode_NestedScans_BindArgsInOrder(t *testing.T) {
	t.Parallel()

	plan := &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "MetricName"},
			Right: &chplan.LitString{V: "http_requests_total"},
		},
	}
	_, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if want := []any{"http_requests_total"}; !reflect.DeepEqual(args, want) {
		t.Errorf("Args = %v; want %v", args, want)
	}
}

// TestEmitNode_Filter_NonScanInput — a Filter whose Input is NOT a
// Scan falls back to the historical `SELECT * FROM (<subq>) WHERE …`
// shape (PREWHERE only applies above a Scan).
func TestEmitNode_Filter_NonScanInput(t *testing.T) {
	t.Parallel()

	plan := &chplan.Filter{
		Input: &chplan.Limit{
			Input: &chplan.Scan{Table: "otel_logs"},
			Count: 100,
		},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "Body"},
			Right: &chplan.LitString{V: "x"},
		},
	}
	sql, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// Must NOT contain PREWHERE — that's a Filter-over-Scan exclusive shape.
	if strings.Contains(sql, "PREWHERE") {
		t.Errorf("Filter(Limit(Scan)) emitted PREWHERE — only Filter(Scan) qualifies: %q", sql)
	}
	if !strings.HasPrefix(sql, "SELECT * FROM (") {
		t.Errorf("Filter(Limit) did not wrap subquery: %q", sql)
	}
	if want := []any{"x"}; !reflect.DeepEqual(args, want) {
		t.Errorf("Args = %v; want %v", args, want)
	}
}

// TestEmitNode_Limit_OverOrderBy — the canonical Tempo `/api/search`
// shape: Limit(OrderBy(Scan, ts DESC), N). Confirms the two passthrough
// emitters chain cleanly, producing a single SELECT with both clauses
// flattened.
func TestEmitNode_Limit_OverOrderBy(t *testing.T) {
	t.Parallel()

	plan := &chplan.Limit{
		Input: &chplan.OrderBy{
			Input: &chplan.Scan{Table: "otel_traces"},
			Keys: []chplan.OrderKey{
				{Expr: &chplan.ColumnRef{Name: "Timestamp"}, Desc: true},
			},
		},
		Count: 20,
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// Both clauses must appear; LIMIT must come after ORDER BY in any
	// nested wrapping.
	if !strings.Contains(sql, "ORDER BY") || !strings.Contains(sql, "LIMIT 20") {
		t.Errorf("expected both ORDER BY and LIMIT 20 in %q", sql)
	}
	orderIdx := strings.Index(sql, "ORDER BY")
	limitIdx := strings.Index(sql, "LIMIT 20")
	if orderIdx > limitIdx {
		t.Errorf("LIMIT appeared before ORDER BY in %q", sql)
	}
}

// TestEmitNode_OrderBy_MultiKey — two keys in OrderBy emit a
// comma-separated list with per-key direction. ASC keys render
// without keyword; DESC keys render with the trailing keyword.
func TestEmitNode_OrderBy_MultiKey(t *testing.T) {
	t.Parallel()

	plan := &chplan.OrderBy{
		Input: &chplan.Scan{Table: "otel_traces"},
		Keys: []chplan.OrderKey{
			{Expr: &chplan.ColumnRef{Name: "ServiceName"}, Desc: false},
			{Expr: &chplan.ColumnRef{Name: "Timestamp"}, Desc: true},
		},
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "ORDER BY `ServiceName`, `Timestamp` DESC") {
		t.Errorf("composite ORDER BY missing or malformed: %q", sql)
	}
}

// TestEmitNode_Scan_BackticksEmbeddedBackticks — Scan with a table
// name containing a backtick character must double-escape the
// backtick to satisfy CH's identifier-quoting rules. This is a
// table-name surface for the existing Ident-doubling contract.
func TestEmitNode_Scan_BackticksEmbeddedBackticks(t *testing.T) {
	t.Parallel()

	sql, _, err := chsql.Emit(context.Background(), &chplan.Scan{Table: "weird`table"})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if want := "SELECT * FROM `weird``table`"; sql != want {
		t.Errorf("SQL = %q; want %q (backtick must be doubled)", sql, want)
	}
}

// TestEmitNode_PreservesSQLLength — Emit reports a non-zero SQL length
// to its span (we observe this via the rendered byte length here, since
// span attributes aren't directly inspectable). A regression that
// returns an empty SQL string but no error would surface as a zero
// length.
func TestEmitNode_PreservesSQLLength(t *testing.T) {
	t.Parallel()

	sql, _, err := chsql.Emit(context.Background(), &chplan.Scan{Table: "otel_logs"})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if len(sql) == 0 {
		t.Error("Emit returned empty SQL with no error")
	}
}

// Note: the `chplan.Node` interface is sealed by an unexported
// `planNode()` marker, so we can't construct an out-of-package Node
// implementation to exercise the emitter's default-case error path.
// TestEmit_NilNode in emit_negatives_test.go covers the analogous
// "no recognised node type" branch via the nil path.
