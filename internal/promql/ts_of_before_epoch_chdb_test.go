//go:build chdb

// chDB proof, against the Prometheus reference engine, that the ts_of_*
// family reports Prometheus's own value for windows whose samples lie
// before the Unix epoch: ts_of_last_over_time answers max(tf, th) with
// an absent kind's timestamp defaulting to 0, so a window holding only
// one kind of sample, all before 1970, answers 0; ts_of_first_over_time's
// absent kind defaults to the largest int64 and never wins.
package promql_test

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	beforeEpochHistMetric  = "before_epoch_exp_hist"
	beforeEpochFloatMetric = "before_epoch_gauge"
)

// beforeEpochOrigin is the instant every sample and evaluation of this
// file is placed relative to, ten years before the Unix epoch.
var beforeEpochOrigin = time.Date(1960, 1, 1, 0, 0, 0, 0, time.UTC)

// beforeEpochSeed holds two histogram samples one and two minutes after
// beforeEpochOrigin, and two float samples four and three minutes before
// it, so a subquery over `sum(hist) or sum(float)` sees float rows at
// its earlier steps and histogram rows at its later ones.
var beforeEpochSeed = "" +
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
	"    ('" + beforeEpochHistMetric + "', map('job', 'h'), toDateTime64('1960-01-01 00:01:00', 9), 2, 4.0, 0, 0, 0, [2], 0, []),\n" +
	"    ('" + beforeEpochHistMetric + "', map('job', 'h'), toDateTime64('1960-01-01 00:02:00', 9), 3, 9.0, 0, 0, 0, [3], 0, []);\n" +
	swapGaugeSeedDDL +
	"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
	"    ('" + beforeEpochFloatMetric + "', map('job', 'f'), toDateTime64('1959-12-31 23:56:00', 9), 3.0),\n" +
	"    ('" + beforeEpochFloatMetric + "', map('job', 'f'), toDateTime64('1959-12-31 23:57:00', 9), 5.0);\n"

// beforeEpochSeconds is the Unix seconds of the instant offset after
// beforeEpochOrigin.
func beforeEpochSeconds(offset time.Duration) float64 {
	return float64(beforeEpochOrigin.Add(offset).Unix())
}

// TestTsOfBeforeEpochParity_ChDB pins cerberus issue #3881. Each query is
// asked three ways: as an instant query and as a range query, both pinned
// with `@` to five minutes after beforeEpochOrigin, and as an unpinned
// range query whose steps end there, which evaluates every step's own
// window.
func TestTsOfBeforeEpochParity_ChDB(t *testing.T) {
	at := beforeEpochOrigin.Add(5 * time.Minute)
	pin := fmt.Sprintf(" @ %d", at.Unix())
	hist := map[string]string{"job": "h"}
	float := map[string]string{"job": "f"}
	noLabels := map[string]string{}
	mixed := `(sum(` + beforeEpochHistMetric + `) or sum(` + beforeEpochFloatMetric + `))`
	// Each case's value is a function of the instant Prometheus evaluates
	// the window at: the pin, or the step of an unpinned range query.
	cases := []struct {
		name   string
		query  string
		labels map[string]string
		value  func(instant time.Time) float64
	}{
		// A window holding only histogram samples answers 0 for
		// ts_of_last and the earliest sample's timestamp for ts_of_first.
		{"hist_ts_of_last", `ts_of_last_over_time(` + beforeEpochHistMetric + `[10m]%s)`, hist, idConst(0)},
		{"hist_ts_of_first", `ts_of_first_over_time(` + beforeEpochHistMetric + `[10m]%s)`, hist, idConst(beforeEpochSeconds(time.Minute))},
		{"hist_subquery_ts_of_last", `ts_of_last_over_time(` + beforeEpochHistMetric + `[10m:1m]%s)`, hist, idConst(0)},
		{"hist_subquery_ts_of_first", `ts_of_first_over_time(` + beforeEpochHistMetric + `[10m:1m]%s)`, hist, idConst(beforeEpochSeconds(time.Minute))},
		// A subquery over a subquery, and ts_of under a subquery over a
		// subquery.
		{"hist_nested_subquery_ts_of_last", `ts_of_last_over_time(last_over_time(` + beforeEpochHistMetric + `[1m:1m])[10m:1m]%s)`, hist, idConst(0)},
		{"hist_ts_of_last_in_subquery", `max_over_time(ts_of_last_over_time(` + beforeEpochHistMetric + `[10m:1m])[1m:1m]%s)`, hist, idConst(0)},
		// The float path, as a guard.
		{"float_ts_of_last", `ts_of_last_over_time(` + beforeEpochFloatMetric + `[10m]%s)`, float, idConst(0)},
		{"float_ts_of_first", `ts_of_first_over_time(` + beforeEpochFloatMetric + `[10m]%s)`, float, idConst(beforeEpochSeconds(-4 * time.Minute))},
		// A mixed window holding both kinds answers the later of its last
		// float and last histogram timestamps, the window's own last grid
		// instant, which lies before 1970.
		{"mixed_ts_of_last", `ts_of_last_over_time(` + mixed + `[10m:1m]%s)`, noLabels, idStepSeconds},
		{"mixed_ts_of_first", `ts_of_first_over_time(` + mixed + `[10m:1m]%s)`, noLabels, idConst(beforeEpochSeconds(-4 * time.Minute))},
		{"mixed_nested_subquery_ts_of_last", `ts_of_last_over_time(last_over_time(` + mixed + `[1m:1m])[10m:1m]%s)`, noLabels, idStepSeconds},
		// A mixed relation whose window holds only histogram rows answers
		// 0, as a histogram-only window does.
		{"mixed_hist_only_window_ts_of_last", `ts_of_last_over_time(` + mixed + `[2m:1m]%s)`, noLabels, idConst(0)},
	}
	for _, tc := range cases {
		pinned := []instantDomainRow{idRow(tc.labels, func(time.Time) float64 { return tc.value(at) })}
		// An instant query over a pinned subquery reports the pin as the
		// sample's instant, as for any `@` pin; the API replaces it with
		// the evaluation instant.
		if strings.Contains(tc.query, ":1m]") {
			pinned[0].sampleAt = at
		}
		t.Run(tc.name+"/instant", func(t *testing.T) {
			runHandWrittenParity(t, beforeEpochSeed, fmt.Sprintf(tc.query, pin), instantDomainEval, pinned, false)
		})
		t.Run(tc.name+"/range_pinned", func(t *testing.T) {
			runHandWrittenParity(t, beforeEpochSeed, fmt.Sprintf(tc.query, pin), instantDomainEval, pinned, true)
		})
		t.Run(tc.name+"/range", func(t *testing.T) {
			runHandWrittenParity(t, beforeEpochSeed, fmt.Sprintf(tc.query, ""), at, []instantDomainRow{idRow(tc.labels, tc.value)}, true)
		})
	}
}
