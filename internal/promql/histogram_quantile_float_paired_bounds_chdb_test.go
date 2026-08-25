//go:build chdb

// chDB-backed proof that lowerHistogramQuantileClassicFloat
// (histogram_quantile_float.go) pairs ExplicitBounds and BucketCounts
// index-for-index after filtering out non-finite `le` bounds, rather than
// filtering only the bounds side and leaving the ladder unfiltered — a
// pre-release audit finding (the float-domain sibling of #2495's
// array-domain fix, histogram_quantile.go's
// classicBucketRowCumulativeExpr/classicBucketUnionBoundsExpr).
//
// A real OTLP classic-histogram row can carry a malformed non-finite
// entry INSIDE its ExplicitBounds array (not just the synthetic trailing
// +Inf overflow position) — this file seeds exactly that shape and
// verifies the query answers the arithmetically-correct interpolated
// quantile rather than silently misaligning the two arrays.
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

// hqFloatPairedBoundsMetric is the BARE metric name — OTel-CH classic
// histograms are stored under the bare name with parallel
// BucketCounts/ExplicitBounds arrays (no `le` label per row); the query
// below names it with the conventional Prometheus `_bucket` suffix, which
// histogramQuantileMatcherPredicate strips before matching MetricName.
const hqFloatPairedBoundsMetric = "hq_float_paired_bounds_probe"

// hqFloatPairedBoundsSeed seeds ONE classic-histogram row whose storage
// ExplicitBounds carries a malformed leading "-inf" entry ahead of the
// three genuine finite bounds [1, 5, 10] — real, if unusual, OTLP data —
// alongside the conventional implicit +Inf overflow BucketCounts always
// carries as its trailing (unpaired) entry.
//
// Per-bucket (non-cumulative) BucketCounts = [100, 2, 3, 4, 1]:
//
//	bound -inf: 100 observations (the malformed rung — deliberately huge,
//	            so any leak into the real ladder is impossible to miss)
//	bound    1:   2 observations
//	bound    5:   3 observations
//	bound   10:   4 observations
//	+Inf overflow: 1 observation
//
// wrapHistogramBucketFanout's cumulative-per-le reshape (histogram_bucket.go)
// synthesises the wire per-`le` CUMULATIVE values this test's query
// reads: le="-inf"->100, le="1"->102, le="5"->105, le="10"->109,
// le="+Inf"->110.
//
// topk(K, ...) is the aggregation-with-no-fold-entry that routes
// histogram_quantile through lowerHistogramQuantileClassicFloat (see that
// function's own doc comment) rather than the array-domain merge path. K
// is 5 — the total number of synthesised per-`le` rows the bucket
// fan-out produces here — so every rung survives topk's ranking: each
// `le` value is a DISTINCT series (topk ranks/selects across the fanned-
// out per-le rows, not within one already-grouped series), and picking a
// smaller K would silently keep only the highest-VALUE rungs (always the
// tail, since the wire representation is cumulative) rather than
// exercising the ladder this test targets.
var hqFloatPairedBoundsSeed = "" +
	"CREATE OR REPLACE TABLE otel_metrics_histogram (" +
	"MetricName String, Attributes Map(String, String), " +
	"ResourceAttributes Map(String, String) DEFAULT map(), ServiceName LowCardinality(String) DEFAULT '', " +
	"TimeUnix DateTime64(9), BucketCounts Array(UInt64), ExplicitBounds Array(Float64), " +
	"AggregationTemporality Int32 DEFAULT 2" +
	") ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);\n" +
	"INSERT INTO otel_metrics_histogram (MetricName, Attributes, TimeUnix, BucketCounts, ExplicitBounds) VALUES\n" +
	"    ('" + hqFloatPairedBoundsMetric + "', map('service', 'checkout'), toDateTime64('2026-01-01 00:00:00', 9), " +
	"[100, 2, 3, 4, 1], [-inf, 1.0, 5.0, 10.0]);\n"

var hqFloatPairedBoundsEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// TestHistogramQuantileClassicFloat_ChDB_PairedBoundsAndLadder pins the
// finding: pre-fix, ExplicitBounds correctly dropped BOTH non-finite
// entries (the malformed "-inf" AND the trailing "+Inf") down to [1,5,10]
// (length 3), but BucketCounts stayed the full unfiltered sorted ladder
// [100,102,105,109,110] (length 5) — misaligned by 2 instead of the
// schema's required 1. Post-fix, BucketCounts drops the malformed rung's
// OWN cumulative count in lockstep, landing at [102,105,109,110] (length
// 4 = 3+1), and the query answers the true interpolated quantile:
//
//	rank = phi * total = 0.9 * 110 = 99
//	first bucket whose count >= 99: bound=1, count=102 (i=0)
//	result = 0 + (1-0)*(99/102) ≈ 0.970588
func TestHistogramQuantileClassicFloat_ChDB_PairedBoundsAndLadder(t *testing.T) {
	fixture := newChDBFixture(t, hqFloatPairedBoundsSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	const query = "histogram_quantile(0.9, topk(5, " + hqFloatPairedBoundsMetric + "_bucket))"
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, hqFloatPairedBoundsEvalTS, hqFloatPairedBoundsEvalTS)
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

	if math.IsNaN(got) {
		t.Fatal("query answered NaN — the paired-length guard should still interpolate over the three genuine finite bounds, not collapse the ladder")
	}

	const want = 99.0 / 102.0 // see doc comment above for the derivation
	const tolerance = 1e-6
	if math.Abs(got-want) > tolerance {
		t.Fatalf(
			"Value = %v, want %v (±%v) — the pre-fix misaligned pairing answers 0.99 for this exact seed (verified: reverting the fix reproduces it), so a value near THAT would mean ExplicitBounds/BucketCounts desynced again",
			got, want, tolerance,
		)
	}
}
