//go:build chdb

// chDB proof, against the Prometheus reference engine, that a subquery
// whose range and offset together span more than a time.Duration holds
// is answered from the samples in its grid's lookbacks.
package promql_test

import (
	"testing"
	"time"
)

const longOffsetMetric = "long_offset_gauge"

// longOffsetSeed holds two samples a few minutes before two instants of a
// 100d subquery grid: 1970-01-01, the epoch, and 100 days after it.
const longOffsetSeed = "" +
	"CREATE OR REPLACE TABLE otel_metrics_gauge (`MetricName` String, `Attributes` Map(String, String), " +
	"`ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', " +
	"`TimeUnix` DateTime64(9), `Value` Float64) ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n" +
	"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
	"    ('" + longOffsetMetric + "', map('job', 'a'), toDateTime64('1969-12-31 23:58:00', 9), 7.0),\n" +
	"    ('" + longOffsetMetric + "', map('job', 'a'), toDateTime64('1970-04-10 23:59:00', 9), 3.0);\n"

// TestSubqueryLongOffsetParity_ChDB pins cerberus issue #3896. Evaluated
// at 2201-01-01, `[150y:100d] offset 150y` lays its grid over
// (1901, 2051], inside DateTime64(9)'s range, while the 300 years from
// the grid's start to the evaluation instant exceed a time.Duration.
func TestSubqueryLongOffsetParity_ChDB(t *testing.T) {
	eval := time.Date(2201, 1, 1, 0, 0, 0, 0, time.UTC)
	job := map[string]string{"job": "a"}
	cases := []struct {
		name  string
		query string
		value float64
	}{
		{"max_over_time", `max_over_time(` + longOffsetMetric + `[150y:100d] offset 150y)`, 7},
		{"count_over_time", `count_over_time(` + longOffsetMetric + `[150y:100d] offset 150y)`, 2},
		{"min_over_time", `min_over_time(` + longOffsetMetric + `[150y:100d] offset 150y)`, 3},
	}
	for _, tc := range cases {
		rows := []instantDomainRow{idRow(job, idConst(tc.value))}
		t.Run(tc.name+"/instant", func(t *testing.T) {
			runHandWrittenParity(t, longOffsetSeed, tc.query, eval, rows, false)
		})
		t.Run(tc.name+"/range", func(t *testing.T) {
			runHandWrittenParity(t, longOffsetSeed, tc.query, eval, rows, true)
		})
	}
}
