package chsql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// TestEmittedDefaultOutputNamesMatchRowType renders every node whose
// RowType() falls back to a default output name when its alias field is
// empty — the Tempo metrics family and a matrix RangeWindow consuming its
// own anchor — with those fields EMPTY, and asserts each defaulted name
// the RowType publishes is an alias the emitted statement actually
// produces. RowType() and the emitter share the constants in
// chplan/output_names.go; this pins that the emitter still reads them
// where it defaults, so a schema-on-Node consumer acting on RowType()
// (a union arm alignment, a wrapping Aggregate's GROUP BY) is acting on
// the statement and not on a stale mirror of it.
func TestEmittedDefaultOutputNamesMatchRowType(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	inner := metricsTimestampTestScan("otel_traces", "Timestamp")
	window := func(input chplan.Node) *chplan.RangeWindow {
		return &chplan.RangeWindow{
			Input: input, Start: start, End: start.Add(time.Minute),
			Step: time.Minute, Range: time.Minute, TimestampColumn: "Timestamp",
		}
	}
	plans := map[string]chplan.Node{
		"MetricsAggregate instant multi-quantile (g0, __phi__)": &chplan.MetricsAggregate{
			Op:         chplan.MetricsOpQuantileOverTime,
			Attr:       &chplan.ColumnRef{Name: "Duration"},
			Quantiles:  []float64{0.5, 0.9},
			GroupBy:    []chplan.Expr{&chplan.ColumnRef{Name: "SpanName"}},
			ValueAlias: "Value",
			Inner:      inner,
		},
		"MetricsAggregate matrix quantile (g0, __bucket)": window(&chplan.MetricsAggregate{
			Op:         chplan.MetricsOpQuantileOverTime,
			Attr:       &chplan.ColumnRef{Name: "Duration"},
			Quantiles:  []float64{0.5},
			GroupBy:    []chplan.Expr{&chplan.ColumnRef{Name: "SpanName"}},
			ValueAlias: "Value",
			Inner:      inner,
		}),
		"MetricsHistogramOverTime instant (__bucket, Value)": &chplan.MetricsHistogramOverTime{
			Attr: &chplan.ColumnRef{Name: "Duration"}, Inner: inner,
		},
		"MetricsHistogramOverTime matrix (__bucket, Value)": window(&chplan.MetricsHistogramOverTime{
			Attr: &chplan.ColumnRef{Name: "Duration"}, Inner: inner,
		}),
		"MetricsCompare (is_selection, attr, val, Value)": compareNode(),
		"RangeWindow over its own anchor (TimeUnix)": &chplan.RangeWindow{
			Input:           rangeWindowPublicTestScan("otel_metrics_gauge"),
			Func:            "sum_over_time",
			Range:           time.Minute,
			Start:           start,
			End:             start.Add(time.Minute),
			Step:            time.Minute,
			OuterRange:      time.Minute,
			TimestampColumn: chplan.RangeWindowAnchorColumn,
			ValueColumn:     "Value",
			GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		},
	}
	for name, plan := range plans {
		t.Run(name, func(t *testing.T) {
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			rowType := plan.RowType()
			if rowType.Open || len(rowType.Columns) == 0 {
				t.Fatalf("RowType is open or empty: %#v", rowType)
			}
			for _, column := range rowType.Columns {
				if column.Name == "" {
					t.Fatalf("RowType carries an unnamed column: %#v", rowType)
				}
				if !strings.Contains(sql, " AS `"+column.Name+"`") && !strings.Contains(sql, " AS "+column.Name) {
					t.Errorf("RowType column %q is never aliased in the emitted statement:\n%s", column.Name, sql)
				}
			}
		})
	}
}
