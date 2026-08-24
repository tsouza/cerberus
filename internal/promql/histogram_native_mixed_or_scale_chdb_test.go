//go:build chdb

// chDB-backed proof that MUL / histogram-left DIV over a mixed
// float/histogram `or` (cerberus issue #2449's sixth wrapper family,
// histogram_native_mixed_or_scale.go) actually SCALES the histogram-shaped
// row's own nine Histogram*Column fields at real ClickHouse execution —
// not merely that the emitted plan's Go shape looks right — while the
// float-shaped row's Value scales too, both keyed correctly by the
// [chplan.MixedDiscriminatorColumn] this file's own header explains is
// read on decode, never by this lowering itself.
//
// The seed is deliberately picked so a discriminator-blind bug is
// VISIBLE, not coincidental: a broken lowering that scaled only Value
// (forgetting the nine Histogram*Column fields — exactly the mistake
// [scaleHistogramProjection]'s five-field split guards against for the
// bare-histogram case) would leave the histogram row's Count/Sum/bucket
// at their raw seeded values (3 / 6.0 / 9) instead of the correctly
// scaled ones (9 / 18.0 / 27 for `* 3`, 1 / 2.0 / 3 for `/ 3`) — a
// difference no reasonable rounding could produce by accident.
package promql_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

// scaleWrappedHistMetric is the bare exp-histogram selector — the
// histogram-VALUED arm of the mixed `or` — and scaleWrappedFloatMetric is
// the metric histogram_quantile(0.5, ...) reduces to a FLOAT before the
// `or`, mirroring test/spec/promql's own
// exp_histogram_set_op_or_mixed_*_wrapped.txtar fixtures' float/histogram
// split. Distinct label values (not just distinct metric names) keep the
// `or`'s shadow test from ever treating the two as the same series.
const (
	scaleWrappedHistMetric  = "scale_wrapped_hist_side_exp_hist"
	scaleWrappedFloatMetric = "scale_wrapped_float_side_exp_hist"
)

// scaleWrappedSeed seeds both arms: the histogram side with a single
// positive bucket (Count=3, Sum=6.0, bucket=[9]) chosen so `* 3` and `/ 3`
// each land on clean, exactly-representable float64 values (9/18.0/27 and
// 1/2.0/3), and the float side with the IDENTICAL Count/Sum/bucket layout
// ([1,2,3,4], Count=10, Sum=10.0) test/spec/promql's own
// exp_histogram_set_op_or_mixed_float_left.txtar already pins
// histogram_quantile(0.5, ...) at 6.3496042078727974 for — reused here
// rather than re-derived so this test and that golden can never quietly
// disagree on what the oracle answers for the same input.
var scaleWrappedSeed = "" +
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
	"    ('" + scaleWrappedHistMetric + "', map('series', 'hist'), toDateTime64('2026-01-01 00:00:00', 9), 3, 6.0, 0, 0, 0, [9], 0, []),\n" +
	"    ('" + scaleWrappedFloatMetric + "', map('series', 'float'), toDateTime64('2026-01-01 00:00:00', 9), 10, 10.0, 0, 0, 0, [1, 2, 3, 4], 0, []);\n"

var scaleWrappedEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// scaleWrappedQuantileBaseline is test/spec/promql's own
// exp_histogram_set_op_or_mixed_float_left.txtar oracle-pinned answer for
// histogram_quantile(0.5, ...) over the [1,2,3,4]/Count=10/Sum=10.0 bucket
// layout scaleWrappedSeed's float side reuses byte-for-byte.
const scaleWrappedQuantileBaseline = 6.3496042078727974

func TestScaleOverMixedSetOpOr_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, scaleWrappedSeed)

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	cases := []struct {
		name         string
		query        string
		wantFloatVal float64
		wantCount    float64
		wantSum      float64
		wantBucket1  float64
	}{
		{
			name:         "* 3, histogram-or left, scalar right",
			query:        `(histogram_quantile(0.5, ` + scaleWrappedFloatMetric + `) or ` + scaleWrappedHistMetric + `) * 3`,
			wantFloatVal: scaleWrappedQuantileBaseline * 3,
			wantCount:    9,
			wantSum:      18.0,
			wantBucket1:  27,
		},
		{
			name:         "3 *, scalar left, histogram-or right",
			query:        `3 * (histogram_quantile(0.5, ` + scaleWrappedFloatMetric + `) or ` + scaleWrappedHistMetric + `)`,
			wantFloatVal: scaleWrappedQuantileBaseline * 3,
			wantCount:    9,
			wantSum:      18.0,
			wantBucket1:  27,
		},
		{
			name:         "/ 3, histogram-left DIV scales",
			query:        `(histogram_quantile(0.5, ` + scaleWrappedFloatMetric + `) or ` + scaleWrappedHistMetric + `) / 3`,
			wantFloatVal: scaleWrappedQuantileBaseline / 3,
			wantCount:    1,
			wantSum:      2.0,
			wantBucket1:  3,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, scaleWrappedEvalTS, scaleWrappedEvalTS)
			if err != nil {
				t.Fatalf("LowerAt(%q): %v", tc.query, err)
			}
			sqlStr, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%q): %v", tc.query, err)
			}

			// `[1]` on HistogramPositiveBucketCounts sidesteps chdb-go's
			// Array(Float64) decode the same way this package's other
			// chDB tests sidestep Map — ClickHouse's arrayElement answers
			// the type's default (0) out of bounds rather than erroring,
			// so this reads 0 on the placeholder empty array and the
			// real (scaled) bucket value on the histogram row.
			projection := "`Attributes`['series'] AS series, `_setop_is_histogram` AS disc, " +
				"`Value` AS val, `HistogramCount` AS cnt, `HistogramSum` AS sum, `HistogramPositiveBucketCounts`[1] AS bucket1"
			rows := fixture.queryOverEmitted(t, projection, sqlStr, args)
			defer func() { _ = rows.Close() }()

			seen := map[string]bool{}
			for rows.Next() {
				var series string
				var disc int
				var val, cnt, sum, bucket1 float64
				if err := rows.Scan(&series, &disc, &val, &cnt, &sum, &bucket1); err != nil {
					t.Fatalf("scan: %v", err)
				}
				seen[series] = true
				switch series {
				case "float":
					if disc != 0 {
						t.Errorf("%s: float row's discriminator = %d, want 0", tc.query, disc)
					}
					if math.Abs(val-tc.wantFloatVal) > 1e-9 {
						t.Errorf("%s: float row's Value = %v, want %v (a discriminator-blind bug would leave this unscaled at %v)", tc.query, val, tc.wantFloatVal, scaleWrappedQuantileBaseline)
					}
				case "hist":
					if disc != 1 {
						t.Errorf("%s: histogram row's discriminator = %d, want 1", tc.query, disc)
					}
					if cnt != tc.wantCount {
						t.Errorf("%s: histogram row's HistogramCount = %v, want %v (raw seeded value is 3 — an unscaled answer means the nine Histogram*Column fields were never touched)", tc.query, cnt, tc.wantCount)
					}
					if sum != tc.wantSum {
						t.Errorf("%s: histogram row's HistogramSum = %v, want %v (raw seeded value is 6)", tc.query, sum, tc.wantSum)
					}
					if bucket1 != tc.wantBucket1 {
						t.Errorf("%s: histogram row's HistogramPositiveBucketCounts[1] = %v, want %v (raw seeded value is 9)", tc.query, bucket1, tc.wantBucket1)
					}
				default:
					t.Errorf("%s: unexpected series label %q", tc.query, series)
				}
			}
			if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
				t.Fatalf("rows.Err: %v", err)
			}
			if !seen["float"] || !seen["hist"] {
				t.Fatalf("%s: got rows for series %v, want both \"float\" and \"hist\" present (the mixed `or` must keep both arms)", tc.query, seen)
			}
		})
	}
}
