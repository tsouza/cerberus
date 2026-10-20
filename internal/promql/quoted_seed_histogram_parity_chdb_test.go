//go:build chdb

// chDB proof that the Prometheus parity oracle reads the exponential
// histograms of a seed whose CREATE TABLE backtick-quotes its names.
package promql_test

import (
	"fmt"
	"testing"
	"time"
)

const quotedSeedHistMetric = "quoted_seed_exp_hist"

// quotedSeedEvalTS is the round-trip harness's fixed now64 instant, which
// an instant query's root stamps on its rows.
var quotedSeedEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// quotedSeedCreate declares the exponential-histogram table under the
// given table name with every column name backtick-quoted.
func quotedSeedCreate(table string) string {
	return "CREATE OR REPLACE TABLE " + table + " (" +
		"`MetricName` String, `Attributes` Map(String, String), " +
		"`ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', " +
		"`TimeUnix` DateTime64(9), " +
		"`Count` UInt64, `Sum` Float64, `Scale` Int32, `ZeroCount` UInt64, " +
		"`PositiveOffset` Int32, `PositiveBucketCounts` Array(UInt64), " +
		"`NegativeOffset` Int32, `NegativeBucketCounts` Array(UInt64)" +
		") ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n"
}

// quotedSeedRows holds two samples of one series: count 2 and sum 4 at
// 23:55, count 3 and sum 9 at 23:56.
const quotedSeedRows = "INSERT INTO otel_metrics_exponential_histogram " +
	"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
	"    ('" + quotedSeedHistMetric + "', map('job', 'h'), toDateTime64('2025-12-31 23:55:00', 9), 2, 4.0, 0, 0, 0, [2], 0, []),\n" +
	"    ('" + quotedSeedHistMetric + "', map('job', 'h'), toDateTime64('2025-12-31 23:56:00', 9), 3, 9.0, 0, 0, 0, [3], 0, []);\n"

// TestQuotedSeedHistogramParity_ChDB pins cerberus issue #3895: the
// reference engine must see the histogram values of a seed whose CREATE
// TABLE backtick-quotes its column names, and also its table name. Every step, from 23:58:01 to 00:00:01, reads the 23:56
// sample within its 5m lookback, so histogram_count answers 3 and
// histogram_sum answers 9, where a reference handed zeroed histograms
// answers 0.
func TestQuotedSeedHistogramParity_ChDB(t *testing.T) {
	eval := quotedSeedEvalTS
	hist := map[string]string{"job": "h"}
	cases := []struct {
		fn   string
		want float64
	}{
		{"histogram_count", 3},
		{"histogram_sum", 9},
	}
	seeds := []struct{ name, seed string }{
		{"quoted_columns", quotedSeedCreate("otel_metrics_exponential_histogram") + quotedSeedRows},
		{"quoted_table", quotedSeedCreate("`otel_metrics_exponential_histogram`") + quotedSeedRows},
	}
	for _, sd := range seeds {
		for _, tc := range cases {
			query := fmt.Sprintf(`%s(%s)`, tc.fn, quotedSeedHistMetric)
			rows := []instantDomainRow{idRow(hist, idConst(tc.want))}
			t.Run(sd.name+"/"+tc.fn+"/instant", func(t *testing.T) {
				runHandWrittenParity(t, sd.seed, query, eval, rows, false)
			})
			t.Run(sd.name+"/"+tc.fn+"/range", func(t *testing.T) {
				runHandWrittenParity(t, sd.seed, query, eval, rows, true)
			})
		}
	}
}
