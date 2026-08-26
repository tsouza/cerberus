//go:build chdb

// chDB-backed proof that histogram_quantile(phi, rate(<exp-histogram
// selector>[range])) applies rate()'s per-second division as part of the
// SAME boundary-extrapolation factor reference Prometheus multiplies the
// whole window-reduced histogram by, rather than skipping it because the
// quantile's own rank walk only reads RATIOS between buckets (which a
// uniform scalar factor cancels out of in EXACT arithmetic).
//
// Root cause: [expHistogramWindowStage] (histogram_quantile_native_window.go)
// built its histogramWindowInputs without setting perSecond, unlike its two
// siblings — classicBucketWindowStage (the classic-histogram quantile path)
// and expHistogramValuedWindowFold (the exp-histogram-VALUED `rate()` output
// path) both set perSecond = shape.windowRange.Seconds() for `rate`. Skipping
// it left this path folding rate() as if it were increase(): the window's
// boundary-extrapolation factor scaled Count/Sum/every bucket by the SAME
// number either way, so every ratio between buckets came out identical in
// EXACT arithmetic — invisible everywhere except at an EXACT rank tie, where
// reference Prometheus's own pipeline divides by the range BEFORE the single
// multiplication that scales the whole histogram, and that division's
// rounding is what breaks the tie. cerberus's undivided (and so exactly
// representable) factor kept a tie reference itself breaks, landing the rank
// walk on the ZERO bucket (answering 0) where reference lands on a real
// negative bucket (answering a large negative number).
//
// This is the property test's TestPromQL_Property_NativeHistogram failure
// from CI run https://github.com/tsouza/cerberus/actions/runs/32920763250:
// `histogram_quantile(0.5, sum(rate(request_latency_exp_hist{instance="a"}[5m])))`
// over two samples fifteen seconds apart, the second of which resets the
// bucket layout entirely (count=9→18, an unrelated bucket shape) and carries
// a NEGATIVE Sum (valid OTel exponential-histogram data — nothing stops Sum
// from being negative when the distribution is dominated by negative-domain
// buckets). Reference Prometheus answers -4.000000000000003; cerberus
// answered 0.
package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestHistogramQuantileNativeRate_ChDB_AppliesPerSecondExtrapolationFactor
// pins the exact CI-observed dataset and query. The two samples are 15s
// apart; the eval timestamp sits 185s after the last one, well inside the
// [5m] window, so both durationToStart/durationToEnd clamp to the same
// half-average-gap boundary extrapolation on both sides. The second sample
// resets the counter (its bucket layout shares no absolute index with the
// first's), so the window folds down to the second sample's own distribution
// scaled by the boundary-extrapolation factor.
func TestHistogramQuantileNativeRate_ChDB_AppliesPerSecondExtrapolationFactor(t *testing.T) {
	const metric = "hq_native_rate_extrapolation_exp_hist"
	// A local DDL rather than the shared subqHistDDL: rate()/increase()
	// read schema.Metrics.AggregationTemporalityColumn (needsTemporalityAgg),
	// which subqHistDDL's column set doesn't carry — no other file in this
	// package exercises a native-histogram rate()/increase() window, which
	// is how this bug went unexercised at this layer until the property
	// test's random sweep found it.
	ddl := "CREATE OR REPLACE TABLE otel_metrics_exponential_histogram (" +
		"`MetricName` String, `Attributes` Map(String, String), " +
		"`ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', " +
		"`TimeUnix` DateTime64(9), " +
		"`Count` UInt64, `Sum` Float64, `Scale` Int32, `ZeroCount` UInt64, " +
		"`PositiveOffset` Int32, `PositiveBucketCounts` Array(UInt64), " +
		"`NegativeOffset` Int32, `NegativeBucketCounts` Array(UInt64), " +
		"`AggregationTemporality` Int32 DEFAULT 2" +
		") ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n"
	seed := ddl +
		"INSERT INTO otel_metrics_exponential_histogram " +
		"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		"    ('" + metric + "', map('instance', 'a'), toDateTime64('2026-05-13 12:00:00', 9), 9, 7.625, -1, 0, -2, [2, 3, 3], -2, [1]),\n" +
		"    ('" + metric + "', map('instance', 'a'), toDateTime64('2026-05-13 12:00:15', 9), 18, -122, -1, 1, 0, [3, 5], 1, [5, 4]);\n"
	fixture := newChDBFixture(t, seed)

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	evalTS := time.Date(2026, 5, 13, 12, 3, 20, 0, time.UTC)

	query := "histogram_quantile(0.5, rate(" + metric + `{instance="a"}` + "[5m]))"
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, evalTS, evalTS)
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
	if rows.Next() {
		t.Fatal("query returned more than one row, want exactly one series")
	}

	// This lands EXACTLY on the rank tie the missing per-second division
	// perturbs: reference Prometheus's own division-before-multiplication
	// rounding is what decides which bucket the walk stops on, so this is
	// an exact float64 comparison, not a tolerance — see the file doc for
	// the full derivation and the reference-Prometheus verification.
	const want = -4.000000000000003
	if got != want {
		t.Fatalf("histogram_quantile(0.5, rate(...)) = %v, want exactly %v.\n"+
			"got 0 here means expHistogramWindowStage stopped threading rate's "+
			"per-second division into the boundary-extrapolation factor, so the "+
			"rank walk's exact tie no longer breaks the way reference Prometheus's "+
			"own floating-point pipeline breaks it.", got, want)
	}
}
