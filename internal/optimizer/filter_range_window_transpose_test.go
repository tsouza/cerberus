package optimizer_test

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// TestFilterRangeWindowTranspose_SeriesKey fires on
// `Filter(RangeWindow(Scan, GROUP BY Attributes), Attributes["job"] = "api")`
// and expects the Filter to land under the RangeWindow.
func TestFilterRangeWindowTranspose_SeriesKey(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_metrics_sum"}
	pred := &chplan.Binary{
		Op: chplan.OpEq,
		Left: &chplan.MapAccess{
			Map: &chplan.ColumnRef{Name: "Attributes"},
			Key: &chplan.LitString{V: "job"},
		},
		Right: &chplan.LitString{V: "api"},
	}
	rw := &chplan.RangeWindow{
		Input:           scan,
		Func:            "rate",
		Range:           5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	input := &chplan.Filter{
		Input:     rw,
		Predicate: pred,
	}

	expected := &chplan.RangeWindow{
		Input: &chplan.Filter{
			Input:     scan,
			Predicate: pred,
		},
		Func:            "rate",
		Range:           5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}

	out := optimizer.New(optimizer.FilterRangeWindowTranspose()).Run(input)
	if !out.Equal(expected) {
		t.Fatalf("FilterRangeWindowTranspose did not fire:\n got: %#v\nwant: %#v", out, expected)
	}
}

// TestFilterRangeWindowTranspose_BlockedByValueColumn leaves the Filter
// above when the predicate touches the windowed value column — that's
// the windowed-function output, not present in the RangeWindow input
// row-shape with the same semantics.
func TestFilterRangeWindowTranspose_BlockedByValueColumn(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_metrics_sum"}
	input := &chplan.Filter{
		Input: &chplan.RangeWindow{
			Input:           scan,
			Func:            "rate",
			Range:           5 * time.Minute,
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
			GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		},
		Predicate: &chplan.Binary{
			Op:    chplan.OpGt,
			Left:  &chplan.ColumnRef{Name: "Value"},
			Right: &chplan.LitFloat{V: 0},
		},
	}

	out := optimizer.New(optimizer.FilterRangeWindowTranspose()).Run(input)
	if !out.Equal(input) {
		t.Fatalf("rule fired despite value-column reference:\n got: %#v", out)
	}
}

// TestFilterRangeWindowTranspose_BlockedByTimestampColumn leaves the
// Filter above when the predicate touches the per-sample timestamp
// column — the RangeWindow synthesises a per-step eval grid, so a
// predicate on `TimeUnix` over the window's output has different
// semantics from one applied to the input rows.
func TestFilterRangeWindowTranspose_BlockedByTimestampColumn(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_metrics_sum"}
	input := &chplan.Filter{
		Input: &chplan.RangeWindow{
			Input:           scan,
			Func:            "rate",
			Range:           5 * time.Minute,
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
			GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		},
		Predicate: &chplan.Binary{
			Op:    chplan.OpGt,
			Left:  &chplan.ColumnRef{Name: "TimeUnix"},
			Right: &chplan.LitInt{V: 0},
		},
	}

	out := optimizer.New(optimizer.FilterRangeWindowTranspose()).Run(input)
	if !out.Equal(input) {
		t.Fatalf("rule fired despite timestamp-column reference:\n got: %#v", out)
	}
}

// TestFilterRangeWindowTranspose_BlockedByMixedPredicate verifies the
// conservative split policy: a predicate that AND-s a safe sub-clause
// (on a series key) with an unsafe sub-clause (on the windowed value)
// is left entirely above the RangeWindow. We don't split AND nodes.
func TestFilterRangeWindowTranspose_BlockedByMixedPredicate(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_metrics_sum"}
	input := &chplan.Filter{
		Input: &chplan.RangeWindow{
			Input:           scan,
			Func:            "rate",
			Range:           5 * time.Minute,
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
			GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		},
		Predicate: &chplan.Binary{
			Op: chplan.OpAnd,
			Left: &chplan.Binary{
				Op: chplan.OpEq,
				Left: &chplan.MapAccess{
					Map: &chplan.ColumnRef{Name: "Attributes"},
					Key: &chplan.LitString{V: "job"},
				},
				Right: &chplan.LitString{V: "api"},
			},
			Right: &chplan.Binary{
				Op:    chplan.OpGt,
				Left:  &chplan.ColumnRef{Name: "Value"},
				Right: &chplan.LitFloat{V: 0},
			},
		},
	}

	out := optimizer.New(optimizer.FilterRangeWindowTranspose()).Run(input)
	if !out.Equal(input) {
		t.Fatalf("rule fired despite mixed predicate:\n got: %#v", out)
	}
}

// TestFilterRangeWindowTranspose_BlockedByEmptyGroupBy declines the
// rewrite for RangeWindows with no per-series identity — there's no
// passthrough surface in that case.
func TestFilterRangeWindowTranspose_BlockedByEmptyGroupBy(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_metrics_sum"}
	input := &chplan.Filter{
		Input: &chplan.RangeWindow{
			Input:           scan,
			Func:            "rate",
			Range:           5 * time.Minute,
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
		},
		Predicate: &chplan.Binary{
			Op: chplan.OpEq,
			Left: &chplan.MapAccess{
				Map: &chplan.ColumnRef{Name: "Attributes"},
				Key: &chplan.LitString{V: "job"},
			},
			Right: &chplan.LitString{V: "api"},
		},
	}

	out := optimizer.New(optimizer.FilterRangeWindowTranspose()).Run(input)
	if !out.Equal(input) {
		t.Fatalf("rule fired despite empty GroupBy:\n got: %#v", out)
	}
}

// TestFilterRangeWindowTranspose_BlockedByComputedGroupKey skips
// RangeWindows whose group keys are not bare ColumnRefs (e.g.
// `GROUP BY substr(Attributes['job'], 1, 3)`). Mirrors the
// FilterAggregateTranspose policy.
func TestFilterRangeWindowTranspose_BlockedByComputedGroupKey(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_metrics_sum"}
	input := &chplan.Filter{
		Input: &chplan.RangeWindow{
			Input:           scan,
			Func:            "rate",
			Range:           5 * time.Minute,
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
			GroupBy: []chplan.Expr{
				&chplan.FuncCall{
					Name: "substr",
					Args: []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}, &chplan.LitInt{V: 1}},
				},
			},
		},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "Attributes"},
			Right: &chplan.LitString{V: "api"},
		},
	}

	out := optimizer.New(optimizer.FilterRangeWindowTranspose()).Run(input)
	if !out.Equal(input) {
		t.Fatalf("rule fired despite computed group key:\n got: %#v", out)
	}
}

// TestFilterRangeWindowTranspose_NoMatchOnOtherShape leaves Filter(Scan)
// alone.
func TestFilterRangeWindowTranspose_NoMatchOnOtherShape(t *testing.T) {
	t.Parallel()

	input := &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_metrics_sum"},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "MetricName"},
			Right: &chplan.LitString{V: "up"},
		},
	}

	out := optimizer.New(optimizer.FilterRangeWindowTranspose()).Run(input)
	if !out.Equal(input) {
		t.Fatalf("rule fired on Filter(Scan):\n got: %#v", out)
	}
}

// TestFilterRangeWindowTranspose_LogQLShape covers the LogQL-style
// `rate({app="api"}[5m])` lowering where the series identity is a bare
// `ServiceName` column rather than the OTel-CH map. The rule should
// still fire as long as the predicate references only that column.
func TestFilterRangeWindowTranspose_LogQLShape(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_logs"}
	pred := &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.ColumnRef{Name: "ServiceName"},
		Right: &chplan.LitString{V: "api"},
	}
	input := &chplan.Filter{
		Input: &chplan.RangeWindow{
			Input:           scan,
			Func:            "rate",
			Range:           5 * time.Minute,
			TimestampColumn: "Timestamp",
			ValueColumn:     "BodyBytes",
			GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "ServiceName"}},
		},
		Predicate: pred,
	}

	out := optimizer.New(optimizer.FilterRangeWindowTranspose()).Run(input)
	rw, ok := out.(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("expected RangeWindow at root, got %T", out)
	}
	if _, ok := rw.Input.(*chplan.Filter); !ok {
		t.Fatalf("expected Filter under RangeWindow, got %T", rw.Input)
	}
}
