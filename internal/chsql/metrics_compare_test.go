package chsql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// compareNode builds a minimal valid MetricsCompare (no root lookup —
// the join shape is covered by the lowering-level tests + TXTAR
// fixtures; this file pins the emitter's own contract).
func compareNode() *chplan.MetricsCompare {
	return &chplan.MetricsCompare{
		Selection: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "StatusCode"},
			Right: &chplan.LitString{V: "Error"},
		},
		TopN: 10,
		Pairs: &chplan.FuncCall{Name: "array", Args: []chplan.Expr{
			&chplan.FuncCall{Name: "tuple", Args: []chplan.Expr{
				&chplan.LitString{V: "name"},
				&chplan.ColumnRef{Name: "SpanName"},
			}},
		}},
		SelAlias:   "is_selection",
		AttrAlias:  "attr",
		ValAlias:   "val",
		ValueAlias: "Value",
		Inner:      &chplan.Scan{Table: "otel_traces"},
	}
}

// TestEmitMetricsCompare_BareShape — bare emission groups by
// (cohort, attr, val) with a deterministic ORDER BY and the Float64
// count reducer.
func TestEmitMetricsCompare_BareShape(t *testing.T) {
	t.Parallel()

	sql, _, err := chsql.Emit(context.Background(), compareNode())
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, want := range []string{
		"arrayJoin(array(tuple(",
		"AS `is_selection`",
		"tupleElement(kv, ?) AS `attr`",
		"tupleElement(kv, ?) AS `val`",
		"toFloat64(count(?)) AS `Value`",
		"GROUP BY `is_selection`, `attr`, `val`",
		"ORDER BY `is_selection`, `attr`, `val`",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("bare SQL missing %q:\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "anchor_ts") {
		t.Errorf("bare SQL must not contain the matrix anchor column:\n%s", sql)
	}
}

// TestEmitRangeWindowCompare_MatrixShape — the RangeWindow wrap adds
// the sample-side anchor fanout, the anchor GROUP BY axis, and the
// (Start - range, End] scan-bound pushdown.
func TestEmitRangeWindowCompare_MatrixShape(t *testing.T) {
	t.Parallel()

	rw := &chplan.RangeWindow{
		Input:           compareNode(),
		Range:           time.Minute,
		Step:            time.Minute,
		Start:           time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
		End:             time.Date(2026, 5, 12, 10, 3, 0, 0, time.UTC),
		TimestampColumn: "Timestamp",
	}
	sql, _, err := chsql.Emit(context.Background(), rw)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, want := range []string{
		"AS `anchor_ts`",
		"GROUP BY `is_selection`, `attr`, `val`, `anchor_ts`",
		"`Timestamp` > toDateTime64('2026-05-12 10:00:00.000000000', 9) - toIntervalNanosecond(60000000000)",
		"`Timestamp` <= toDateTime64('2026-05-12 10:03:00.000000000', 9)",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("matrix SQL missing %q:\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "ORDER BY") {
		t.Errorf("matrix SQL must not pin an ORDER BY (the handler owns series assembly):\n%s", sql)
	}
}

// TestEmitMetricsCompare_ErrorPaths — nil Selection / Pairs / Inner and
// a non-positive matrix Step surface as synchronous emit errors.
func TestEmitMetricsCompare_ErrorPaths(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		node chplan.Node
		want string
	}{
		{"nilSelection", func() chplan.Node { m := compareNode(); m.Selection = nil; return m }(), "Selection is nil"},
		{"nilPairs", func() chplan.Node { m := compareNode(); m.Pairs = nil; return m }(), "Pairs is nil"},
		{"nilInner", func() chplan.Node { m := compareNode(); m.Inner = nil; return m }(), "Inner is nil"},
		{"rootLookupWithoutTraceID", func() chplan.Node {
			m := compareNode()
			m.RootLookup = &chplan.Scan{Table: "otel_traces"}
			m.TraceIDColumn = ""
			return m
		}(), "TraceIDColumn empty"},
		{"matrixZeroStep", &chplan.RangeWindow{
			Input:           compareNode(),
			TimestampColumn: "Timestamp",
		}, "requires Step > 0"},
		{"matrixNoTsColumn", &chplan.RangeWindow{
			Input: compareNode(),
			Step:  time.Minute,
		}, "TimestampColumn unset"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := chsql.Emit(context.Background(), tc.node)
			if err == nil {
				t.Fatalf("Emit should fail for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q missing %q", err, tc.want)
			}
		})
	}
}
