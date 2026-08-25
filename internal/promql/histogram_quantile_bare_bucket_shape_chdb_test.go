//go:build chdb

// chDB-backed proof that lowerHistogramQuantileClassicBare's
// classicBucketFiniteBoundsRestriction (histogram_quantile.go) tolerates
// BOTH shapes ExplicitBounds and BucketCounts can legally take on a
// classic-histogram row, not just the canonical OTel one — a pre-release
// audit finding (#2625): the ORIGINAL fix required BucketCounts to be
// exactly len(ExplicitBounds)+1 via a pairwise arrayFilter, which threw
// `SIZES_OF_ARRAYS_DONT_MATCH` against any row shaped like the corpus's
// own long-standing "no overflow bucket" convention — BucketCounts and
// ExplicitBounds the SAME length, which readSeededClassicHistograms
// (test/spec/parity_chdb.go) documents and reconstructs explicitly, and
// which classicBucketRowCumulativeExpr's `arraySlice(cs, 1, length(bs))`
// (the aggregated/merge path, a few hundred lines above in this same
// file) already tolerated by construction. This test pins BOTH shapes
// against the SAME bounds and a proportionally similar bucket ladder, so
// a future change that reintroduces an equal-length assumption (in
// either direction — over-strict again, or silently dropping the
// overflow count when it IS present) fails here before it reaches chDB
// in CI.
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

// hqBareBucketShapeEqualMetric seeds the EQUAL-length shape: BucketCounts
// and ExplicitBounds both length 3, i.e. "no overflow bucket" — every
// observation falls at or below the highest explicit bound.
const hqBareBucketShapeEqualMetric = "hq_bare_bucket_shape_equal_probe"

// hqBareBucketShapeOverflowMetric seeds the canonical OTel shape:
// BucketCounts one longer than ExplicitBounds, with a genuine non-zero
// overflow count above the highest explicit bound.
const hqBareBucketShapeOverflowMetric = "hq_bare_bucket_shape_overflow_probe"

// Both rows share the same three bounds [1, 5, 10] and the same
// proportional per-bucket counts [2, 3, 4] below them, so the only
// difference between the two answers below is whether an overflow
// bucket exists at all.
var hqBareBucketShapeSeed = "" +
	"CREATE OR REPLACE TABLE otel_metrics_histogram (" +
	"MetricName String, Attributes Map(String, String), " +
	"ResourceAttributes Map(String, String) DEFAULT map(), ServiceName LowCardinality(String) DEFAULT '', " +
	"TimeUnix DateTime64(9), BucketCounts Array(UInt64), ExplicitBounds Array(Float64), " +
	"AggregationTemporality Int32 DEFAULT 2" +
	") ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);\n" +
	"INSERT INTO otel_metrics_histogram (MetricName, Attributes, TimeUnix, BucketCounts, ExplicitBounds) VALUES\n" +
	"    ('" + hqBareBucketShapeEqualMetric + "', map('service', 'checkout'), toDateTime64('2026-01-01 00:00:00', 9), " +
	"[2, 3, 4], [1.0, 5.0, 10.0]),\n" +
	"    ('" + hqBareBucketShapeOverflowMetric + "', map('service', 'checkout'), toDateTime64('2026-01-01 00:00:00', 9), " +
	"[2, 3, 4, 5], [1.0, 5.0, 10.0]);\n"

var hqBareBucketShapeEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// hqBareBucketShapePhi is the single phi both subtests probe at — see
// each test's own doc comment for the rank derivation.
const hqBareBucketShapePhi = "0.9"

func runHistogramQuantileBareBucketShape(t *testing.T, metric string) float64 {
	t.Helper()
	fixture := newChDBFixture(t, hqBareBucketShapeSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	query := "histogram_quantile(" + hqBareBucketShapePhi + ", " + metric + "_bucket)"
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, hqBareBucketShapeEvalTS, hqBareBucketShapeEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}

	rows := fixture.queryOverEmitted(t, "Value AS v", sqlStr, args)
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		t.Fatal("query returned no rows, want exactly one interpolated quantile")
	}
	var got float64
	if err := rows.Scan(&got); err != nil {
		t.Fatalf("scan Value: %v", err)
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return got
}

// TestHistogramQuantileClassicBare_ChDB_EqualLengthBucketShape pins the
// "no overflow bucket" shape: cumulative = [2, 5, 9], total = 9, rank =
// 0.9*9 = 8.1 lands strictly inside the top bucket (5, 10], so the
// answer interpolates: 5 + (10-5)*((8.1-5)/(9-5)) = 8.875. Pre-fix
// (#2625), this shape made classicBucketFiniteBoundsRestriction's
// pairwise arrayFilter throw SIZES_OF_ARRAYS_DONT_MATCH before the query
// ever reached this interpolation.
func TestHistogramQuantileClassicBare_ChDB_EqualLengthBucketShape(t *testing.T) {
	got := runHistogramQuantileBareBucketShape(t, hqBareBucketShapeEqualMetric)

	const want = 8.875
	const tolerance = 1e-9
	if math.Abs(got-want) > tolerance {
		t.Fatalf("Value = %v, want %v (±%v)", got, want, tolerance)
	}
}

// TestHistogramQuantileClassicBare_ChDB_OverflowBucketShape pins the
// canonical OTel shape with a genuine overflow count: cumulative =
// [2, 5, 9, 14], total = 14, rank = 0.9*14 = 12.6 lands in the +Inf
// overflow rung (idx == length(cum) AND length(buckets) != length(bounds)),
// so HistogramQuantile's own overflow clamp answers the highest finite
// bound (10) directly rather than interpolating or extrapolating past
// it — Prometheus's own bucketQuantile convention. This differs from the
// equal-length case above (8.875) purely because of the extra overflow
// count: if a regression silently dropped it, this would fall back to
// the equal-length answer instead.
func TestHistogramQuantileClassicBare_ChDB_OverflowBucketShape(t *testing.T) {
	got := runHistogramQuantileBareBucketShape(t, hqBareBucketShapeOverflowMetric)

	const want = 10.0
	const tolerance = 1e-9
	if math.Abs(got-want) > tolerance {
		t.Fatalf("Value = %v, want %v (±%v)", got, want, tolerance)
	}
}
