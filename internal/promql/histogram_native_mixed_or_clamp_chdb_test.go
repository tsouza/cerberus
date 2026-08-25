//go:build chdb

// chDB-backed proof that the clamp family (clamp_max/clamp_min/clamp)
// directly wrapping a mixed float/histogram `or` (cerberus issue #2587,
// histogram_native_mixed_or_math_fn.go's
// [clampOverMixedExpHistogramSetOp] / [lowerClampOverMixedExpHistogramSetOp])
// actually DROPS the histogram-shaped row and keeps the float-shaped
// row's own clamp bound(s) applied at real ClickHouse execution — not
// merely that the emitted plan's Go shape looks right. Reference
// Prometheus's clamp()/clamp_min()/clamp_max() share funcClamp's
// simpleFloatFunc-style kernel with round()/abs()/ceil()/sqrt(): a
// histogram-valued sample is silently skipped, never computed over,
// exactly the same drop semantics
// TestRoundToNearestOverMixedSetOpOr_ChDB
// (histogram_native_mixed_or_round_to_nearest_chdb_test.go) already
// proves at chDB execution for round()'s 2-arg to_nearest form.
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

// clampWrappedHistMetric is the bare exp-histogram selector — the
// histogram-VALUED arm of the mixed `or` — and clampWrappedFloatMetric
// is the metric histogram_quantile(0.5, ...) reduces to a FLOAT before
// the `or`, mirroring
// histogram_native_mixed_or_round_to_nearest_chdb_test.go's own
// rtnWrappedHistMetric/rtnWrappedFloatMetric split (and its bucket
// layout byte-for-byte, so this test's float baseline is the SAME
// oracle-pinned 6.3496042078727974 quantile rather than a value
// re-derived and liable to drift).
const (
	clampWrappedHistMetric  = "clamp_wrapped_hist_side_exp_hist"
	clampWrappedFloatMetric = "clamp_wrapped_float_side_exp_hist"
)

var clampWrappedSeed = "" +
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
	"    ('" + clampWrappedHistMetric + "', map('series', 'hist'), toDateTime64('2026-01-01 00:00:00', 9), 3, 6.0, 0, 0, 0, [9], 0, []),\n" +
	"    ('" + clampWrappedFloatMetric + "', map('series', 'float'), toDateTime64('2026-01-01 00:00:00', 9), 10, 10.0, 0, 0, 0, [1, 2, 3, 4], 0, []);\n"

var clampWrappedEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// clampWrappedQuantileBaseline is
// histogram_native_mixed_or_round_to_nearest_chdb_test.go's own
// rtnWrappedQuantileBaseline, reused byte-for-byte since this file's
// float-side bucket layout is identical.
const clampWrappedQuantileBaseline = 6.3496042078727974

func TestClampFamilyOverMixedSetOpOr_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, clampWrappedSeed)

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	orExpr := "(histogram_quantile(0.5, " + clampWrappedFloatMetric + ") or " + clampWrappedHistMetric + ")"

	cases := []struct {
		name      string
		query     string
		wantFloat float64
	}{
		// clampWrappedQuantileBaseline (~6.35) exceeds the 5 bound, so
		// clamp_max actually clamps rather than passing the value through
		// unchanged — proving the bound is applied, not just forwarded.
		{
			name:      "clamp_max",
			query:     "clamp_max(" + orExpr + ", 5)",
			wantFloat: 5,
		},
		// clampWrappedQuantileBaseline (~6.35) is below the 7 bound, so
		// clamp_min actually clamps rather than passing the value through
		// unchanged.
		{
			name:      "clamp_min",
			query:     "clamp_min(" + orExpr + ", 7)",
			wantFloat: 7,
		},
		// clampWrappedQuantileBaseline (~6.35) exceeds the [5, 6] band, so
		// the 3-arg form clamps to the upper bound.
		{
			name:      "clamp",
			query:     "clamp(" + orExpr + ", 5, 6)",
			wantFloat: 6,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, clampWrappedEvalTS, clampWrappedEvalTS)
			if err != nil {
				t.Fatalf("LowerAt(%q): %v", tc.query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
				t.Fatalf("lower(%q): plan root publishes %s, want %s (the clamp family drops the histogram row)", tc.query, shape, chplan.SampleRowShape)
			}
			if proj, ok := plan.(*chplan.Project); !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.Project", tc.query, plan)
			} else if _, ok := proj.Input.(*chplan.Filter); !ok {
				t.Fatalf("lower(%q): plan root's input is %T, want *chplan.Filter (narrowing to the float-shaped rows)", tc.query, proj.Input)
			}

			sqlStr, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%q): %v", tc.query, err)
			}

			rows := fixture.queryOverEmitted(t, "`Attributes`['series'] AS series, `Value` AS val", sqlStr, args)
			defer func() { _ = rows.Close() }()

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
				t.Fatalf("%s: no row for series %q, want one (the clamp family keeps the float-shaped row)", tc.query, "float")
			}
			if math.Abs(floatVal-tc.wantFloat) > 1e-9 {
				t.Errorf("%s: float row's Value = %v, want %v (a discriminator-blind bug would leave this at the un-clamped quantile %v)", tc.query, floatVal, tc.wantFloat, clampWrappedQuantileBaseline)
			}

			if histVal, ok := seen["hist"]; ok {
				t.Errorf("%s: got a row for series %q (Value = %v), want none (the clamp family drops every histogram-valued sample, matching reference's simpleFloatFunc-style rule)", tc.query, "hist", histVal)
			}
		})
	}
}
