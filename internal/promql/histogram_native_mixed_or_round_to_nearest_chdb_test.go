//go:build chdb

// chDB-backed proof that round()'s 2-arg to_nearest form directly
// wrapping a mixed float/histogram `or` (cerberus issue #2578,
// histogram_native_mixed_or_math_fn.go's
// [roundToNearestOverMixedExpHistogramSetOp] /
// [lowerRoundToNearestOverMixedExpHistogramSetOp]) actually DROPS the
// histogram-shaped row and keeps the float-shaped row's own
// round(Value / to_nearest) * to_nearest at real ClickHouse execution —
// not merely that the emitted plan's Go shape looks right. Reference
// Prometheus's round() shares funcRoundImpl's simpleFloatFunc kernel with
// abs()/ceil()/sqrt() and every other instantFnCH entry: a histogram-
// valued sample is silently skipped, never computed over, exactly the
// same drop semantics TestLower_ExpHistogram_MixedSetOpOr_MathFnWrapped
// (histogram_native_mixed_or_math_fn_test.go) already pins for the 1-arg
// form and TestScaleOverMixedSetOpOr_ChDB
// (histogram_native_mixed_or_scale_chdb_test.go) already proves at chDB
// execution for the SCALE family (which keeps and transforms the
// histogram row instead of dropping it).
package promql_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

// rtnWrappedHistMetric is the bare exp-histogram selector — the
// histogram-VALUED arm of the mixed `or` — and rtnWrappedFloatMetric is
// the metric histogram_quantile(0.5, ...) reduces to a FLOAT before the
// `or`, mirroring histogram_native_mixed_or_scale_chdb_test.go's own
// scaleWrappedHistMetric/scaleWrappedFloatMetric split (and its bucket
// layout byte-for-byte, so this test's float baseline is the SAME
// oracle-pinned 6.3496042078727974 quantile rather than a value
// re-derived and liable to drift).
const (
	rtnWrappedHistMetric  = "rtn_wrapped_hist_side_exp_hist"
	rtnWrappedFloatMetric = "rtn_wrapped_float_side_exp_hist"
)

var rtnWrappedSeed = "" +
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
	"    ('" + rtnWrappedHistMetric + "', map('series', 'hist'), toDateTime64('2026-01-01 00:00:00', 9), 3, 6.0, 0, 0, 0, [9], 0, []),\n" +
	"    ('" + rtnWrappedFloatMetric + "', map('series', 'float'), toDateTime64('2026-01-01 00:00:00', 9), 10, 10.0, 0, 0, 0, [1, 2, 3, 4], 0, []);\n"

var rtnWrappedEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// rtnWrappedQuantileBaseline is histogram_native_mixed_or_scale_chdb_test.go's
// own scaleWrappedQuantileBaseline, reused byte-for-byte since this file's
// float-side bucket layout is identical.
const rtnWrappedQuantileBaseline = 6.3496042078727974

func TestRoundToNearestOverMixedSetOpOr_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, rtnWrappedSeed)

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	query := `round((histogram_quantile(0.5, ` + rtnWrappedFloatMetric + `) or ` + rtnWrappedHistMetric + `), 5)`

	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, rtnWrappedEvalTS, rtnWrappedEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
		t.Fatalf("lower(%q): plan root publishes %s, want %s (round() drops the histogram row)", query, shape, chplan.SampleRowShape)
	}

	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}

	rows := fixture.queryOverEmitted(t, "`Attributes`['series'] AS series, `Value` AS val", sqlStr, args)
	defer func() { _ = rows.Close() }()

	wantFloatVal := math.Round(rtnWrappedQuantileBaseline/5) * 5

	seen := map[string]float64{}
	for rows.Next() {
		var series string
		var val float64
		if err := rows.Scan(&series, &val); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen[series] = val
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	floatVal, ok := seen["float"]
	if !ok {
		t.Fatalf("%s: no row for series %q, want one (round() keeps the float-shaped row)", query, "float")
	}
	if math.Abs(floatVal-wantFloatVal) > 1e-9 {
		t.Errorf("%s: float row's Value = %v, want %v (a discriminator-blind bug would leave this at the un-rounded quantile %v)", query, floatVal, wantFloatVal, rtnWrappedQuantileBaseline)
	}

	if histVal, ok := seen["hist"]; ok {
		t.Errorf("%s: got a row for series %q (Value = %v), want none (round() drops every histogram-valued sample, matching reference's simpleFloatFunc)", query, "hist", histVal)
	}
}
