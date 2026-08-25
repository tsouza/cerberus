//go:build chdb

// chDB-backed proof that `count_values("label", ...)` directly wrapping a
// mixed float/histogram `or` (cerberus issue #2595,
// histogram_native_mixed_or_aggregate_count_values.go's
// [countValuesOverMixedExpHistogramSetOp] /
// [lowerCountValuesOverMixedExpHistogramSetOp]) unions the two
// independently-stringified branches correctly at real ClickHouse
// execution: every seeded series is accounted for exactly once, distinct
// float values collapse to one counted row each (proving the float
// branch's own grouping survives being unioned with the histogram
// branch), and the histogram branch contributes its own distinct row
// rather than colliding with any float-valued row.
package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

const (
	cvMixedHistMetric  = "cv_mixed_hist_side_exp_hist"
	cvMixedFloatMetric = "cv_mixed_float_side_gauge"
)

// cvMixedSeed keys one histogram series ("h1") and three float series:
// "f1"/"f2" share the SAME value (3.0), "f3" carries a distinct value
// (9.0) — proving count_values's own float-side grouping (two series,
// one counted row) survives composition with the histogram branch.
var cvMixedSeed = "" +
	"CREATE OR REPLACE TABLE otel_metrics_exponential_histogram (" +
	"`MetricName` String, `Attributes` Map(String, String), " +
	"`ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', " +
	"`TimeUnix` DateTime64(9), " +
	"`Count` UInt64, `Sum` Float64, `Scale` Int32, `ZeroCount` UInt64, " +
	"`PositiveOffset` Int32, `PositiveBucketCounts` Array(UInt64), " +
	"`NegativeOffset` Int32, `NegativeBucketCounts` Array(UInt64)" +
	") ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n" +
	"INSERT INTO otel_metrics_exponential_histogram " +
	"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
	"    ('" + cvMixedHistMetric + "', map('series', 'h1'), toDateTime64('2026-01-01 00:00:00', 9), 5, 10.0, 0, 0, 0, [5], 0, []);\n" +
	swapGaugeSeedDDL +
	"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
	"    ('" + cvMixedFloatMetric + "', map('series', 'f1'), toDateTime64('2026-01-01 00:00:00', 9), 3.0),\n" +
	"    ('" + cvMixedFloatMetric + "', map('series', 'f2'), toDateTime64('2026-01-01 00:00:00', 9), 3.0),\n" +
	"    ('" + cvMixedFloatMetric + "', map('series', 'f3'), toDateTime64('2026-01-01 00:00:00', 9), 9.0);\n"

var cvMixedEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

func TestCountValuesOverMixedSetOpOr_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, cvMixedSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	query := `count_values("v", ` + cvMixedHistMetric + ` or ` + cvMixedFloatMetric + `)`
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, cvMixedEvalTS, cvMixedEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}

	t.Run("totals every seeded series exactly once", func(t *testing.T) {
		rows := fixture.queryOverEmitted(t, "`Value` AS val", sqlStr, args)
		defer func() { _ = rows.Close() }()
		var total float64
		var rowCount int
		for rows.Next() {
			var val float64
			if err := rows.Scan(&val); err != nil {
				t.Fatalf("scan: %v", err)
			}
			total += val
			rowCount++
		}
		if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		const wantTotal = 4.0 // h1, f1, f2, f3 — every seeded series
		if total != wantTotal {
			t.Fatalf("query %q: sum of counted rows = %v, want %v (a dropped branch would undercount)", query, total, wantTotal)
		}
		// Three distinct values: the histogram's own string, "3" (f1+f2),
		// "9" (f3). A collision between the histogram string and a float
		// string would collapse this to fewer rows; a failure to group the
		// float side by value at all would inflate it.
		const wantRows = 3
		if rowCount != wantRows {
			t.Fatalf("query %q: got %d distinct-value rows, want %d", query, rowCount, wantRows)
		}
	})

	t.Run("the shared float value counts both series that hit it", func(t *testing.T) {
		rows := fixture.queryOverEmitted(t, "`Attributes`['v'] AS v, `Value` AS val", sqlStr, args)
		defer func() { _ = rows.Close() }()
		seen := map[string]float64{}
		for rows.Next() {
			var v string
			var val float64
			if err := rows.Scan(&v, &val); err != nil {
				t.Fatalf("scan: %v", err)
			}
			seen[v] = val
		}
		if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		gotVal, ok := seen["3"]
		if !ok {
			t.Fatalf("query %q: no row for value \"3\", want Value=2 (f1, f2)", query)
		}
		if gotVal != 2 {
			t.Errorf("query %q: value \"3\" row has Value=%v, want 2 (f1 and f2 both hit it)", query, gotVal)
		}
		gotVal9, ok := seen["9"]
		if !ok {
			t.Fatalf("query %q: no row for value \"9\", want Value=1 (f3)", query)
		}
		if gotVal9 != 1 {
			t.Errorf("query %q: value \"9\" row has Value=%v, want 1", query, gotVal9)
		}
	})
}
