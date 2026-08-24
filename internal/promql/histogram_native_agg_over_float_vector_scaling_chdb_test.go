//go:build chdb

// chDB-backed proof for cerberus issue #2540: sum()/avg() wrapping the
// exp-histogram/float-vector SCALING join shape
// (expHistogramFloatVectorScalingBinop, cerberus issues #2339/#2342/
// #2537) now execute correctly against real ClickHouse data — each
// histogram series is scaled by its joined float row FIRST, and only
// THEN merged across series by the aggregation — not merely lower
// without erroring.
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

// aggScaleSumHistMetric/aggScaleSumFloatMetric back
// TestSumOverFloatVectorScalingBinop_ChDB: TWO histogram series sharing
// job="api" but distinguished by "az" (outside the on(job) match key,
// so group_left() must keep BOTH — group_left() preserves the "many"
// side's own full label set, unlike plain on()/ignoring() one-to-one
// matching, which reduces to the match key), joined many-to-one against
// ONE float row via group_left().
const (
	aggScaleSumHistMetric  = "agg_scale_sum_hist_exp_hist"
	aggScaleSumFloatMetric = "agg_scale_sum_float_gauge"
)

var aggScaleEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

var aggScaleSumSeed = "" +
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
	"    ('" + aggScaleSumHistMetric + "', map('job', 'api', 'az', 'us'), toDateTime64('2026-01-01 00:00:00', 9), 3, 6.0, 0, 0, 0, [9], 0, []),\n" +
	"    ('" + aggScaleSumHistMetric + "', map('job', 'api', 'az', 'eu'), toDateTime64('2026-01-01 00:00:00', 9), 5, 10.0, 0, 0, 0, [20], 0, []);\n" +
	swapGaugeSeedDDL +
	"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
	"    ('" + aggScaleSumFloatMetric + "', map('job', 'api'), toDateTime64('2026-01-01 00:00:00', 9), 2.0);\n"

// TestSumOverFloatVectorScalingBinop_ChDB proves
// `sum(hist * on(job) group_left() float)` scales each histogram series
// by the SAME joined float value (2.0) BEFORE merging across series:
// hist(3,6.0,[9])*2 + hist(5,10.0,[20])*2 = (6,12.0,[18]) + (10,20.0,[40])
// = (16,32.0,[58]) — not the unscaled merge (8,16.0,[29]), which is what
// a bug that dropped the scale before merging would produce instead.
func TestSumOverFloatVectorScalingBinop_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, aggScaleSumSeed)

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	query := "sum(" + aggScaleSumHistMetric + " * on(job) group_left() " + aggScaleSumFloatMetric + ")"
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, aggScaleEvalTS, aggScaleEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	if _, ok := plan.(*chplan.HistogramProjection); !ok {
		t.Fatalf("LowerAt(%q): plan root is %T, want *chplan.HistogramProjection", query, plan)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}

	projection := "length(mapKeys(`Attributes`)) AS numLabels, " +
		"`HistogramCount` AS cnt, `HistogramSum` AS sum, `HistogramPositiveBucketCounts`[1] AS bucket1"
	rows := fixture.queryOverEmitted(t, projection, sqlStr, args)
	defer func() { _ = rows.Close() }()

	n := 0
	for rows.Next() {
		n++
		var numLabels int
		var cnt, sum, bucket1 float64
		if err := rows.Scan(&numLabels, &cnt, &sum, &bucket1); err != nil {
			t.Fatalf("scan: %v", err)
		}
		// sum() with no by/without drops every label — the merged row
		// speaks for the whole group, not one series.
		if numLabels != 0 {
			t.Errorf("numLabels = %d, want 0 (sum() with no by/without drops every label)", numLabels)
		}
		const wantCount, wantSum, wantBucket1 = 16, 32.0, 58
		if math.Abs(cnt-wantCount) > 1e-9 {
			t.Errorf("HistogramCount = %v, want %v", cnt, wantCount)
		}
		if math.Abs(sum-wantSum) > 1e-9 {
			t.Errorf("HistogramSum = %v, want %v", sum, wantSum)
		}
		if math.Abs(bucket1-wantBucket1) > 1e-9 {
			t.Errorf("HistogramPositiveBucketCounts[1] = %v, want %v", bucket1, wantBucket1)
		}
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if n != 1 {
		t.Fatalf("got %d rows, want exactly 1 (both scaled series merge into one group)", n)
	}
}

// aggScaleAvgHistMetric/aggScaleAvgFloatMetric back
// TestAvgOverFloatVectorScalingBinop_ChDB — kept distinct from the sum()
// scenario's metrics so the two seeds never collide in the
// process-shared chDB session (see fixture_chdb_test.go's own package
// doc).
const (
	aggScaleAvgHistMetric  = "agg_scale_avg_hist_exp_hist"
	aggScaleAvgFloatMetric = "agg_scale_avg_float_gauge"
)

var aggScaleAvgSeed = "" +
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
	"    ('" + aggScaleAvgHistMetric + "', map('job', 'api', 'az', 'us'), toDateTime64('2026-01-01 00:00:00', 9), 3, 6.0, 0, 0, 0, [9], 0, []),\n" +
	"    ('" + aggScaleAvgHistMetric + "', map('job', 'api', 'az', 'eu'), toDateTime64('2026-01-01 00:00:00', 9), 5, 10.0, 0, 0, 0, [20], 0, []);\n" +
	swapGaugeSeedDDL +
	"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
	"    ('" + aggScaleAvgFloatMetric + "', map('job', 'api'), toDateTime64('2026-01-01 00:00:00', 9), 2.0);\n"

// TestAvgOverFloatVectorScalingBinop_ChDB proves `avg(hist * on(job)
// group_left() float)` scales each series by the joined float value
// FIRST, merges across series SECOND, and only THEN divides by the
// group's member count (2): the sum() scenario's own (16,32.0,[58])
// merge divided by 2 = (8,16.0,[29]).
func TestAvgOverFloatVectorScalingBinop_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, aggScaleAvgSeed)

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	query := "avg(" + aggScaleAvgHistMetric + " * on(job) group_left() " + aggScaleAvgFloatMetric + ")"
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, aggScaleEvalTS, aggScaleEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	if _, ok := plan.(*chplan.HistogramProjection); !ok {
		t.Fatalf("LowerAt(%q): plan root is %T, want *chplan.HistogramProjection", query, plan)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}

	projection := "`HistogramCount` AS cnt, `HistogramSum` AS sum, `HistogramPositiveBucketCounts`[1] AS bucket1"
	rows := fixture.queryOverEmitted(t, projection, sqlStr, args)
	defer func() { _ = rows.Close() }()

	n := 0
	for rows.Next() {
		n++
		var cnt, sum, bucket1 float64
		if err := rows.Scan(&cnt, &sum, &bucket1); err != nil {
			t.Fatalf("scan: %v", err)
		}
		const wantCount, wantSum, wantBucket1 = 8, 16.0, 29
		if math.Abs(cnt-wantCount) > 1e-9 {
			t.Errorf("HistogramCount = %v, want %v", cnt, wantCount)
		}
		if math.Abs(sum-wantSum) > 1e-9 {
			t.Errorf("HistogramSum = %v, want %v", sum, wantSum)
		}
		if math.Abs(bucket1-wantBucket1) > 1e-9 {
			t.Errorf("HistogramPositiveBucketCounts[1] = %v, want %v", bucket1, wantBucket1)
		}
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if n != 1 {
		t.Fatalf("got %d rows, want exactly 1", n)
	}
}
