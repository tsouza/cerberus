//go:build chdb

// chDB proof, against the Prometheus reference engine, that rate, increase
// and delta over a native or a classic histogram answer Prometheus's own
// answer when their window reaches past an edge of the range a
// DateTime64(9) timestamp can hold, in instant and range mode.
package promql_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"
	"golang.org/x/tools/txtar"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/spec"
)

const (
	hrEdgeNativeMetric  = "hr_edge_exp_hist"
	hrEdgeClassicMetric = "hr_edge_classic"
)

// The two pins. hrEdgeCeilingPin is 2262-04-11 23:49:00, past the last
// stored instant (23:47:16.854775807), so its 10m window ends past it.
// hrEdgeFloorPin is 1677-09-21 00:20:00, whose 10m window starts before
// the first stored instant (00:12:43.145224192).
const (
	hrEdgeCeilingPin = "9223372140"
	hrEdgeFloorPin   = "-9223371700"
)

// hrEdgeSeed holds, for each metric, two samples of job "edge" five
// minutes apart inside hrEdgeCeilingPin's window (23:40 and 23:45) and two
// of job "floor" two hundred seconds apart inside hrEdgeFloorPin's window
// (00:13:20 and 00:16:40). Each native series goes from Count 2, Sum 3 and
// buckets [1, 1] to Count 4, Sum 7 and buckets [2, 2]; each classic series
// from per-bucket counts [1, 1, 0] to [2, 2, 0] over the bounds [1, 2].
const hrEdgeSeed = "" +
	"CREATE TABLE otel_metrics_exponential_histogram (MetricName String, Attributes Map(String, String), " +
	"ResourceAttributes Map(String, String) DEFAULT map(), ServiceName LowCardinality(String) DEFAULT '', " +
	"TimeUnix DateTime64(9), Count UInt64, Sum Float64, Scale Int32, ZeroCount UInt64, " +
	"PositiveOffset Int32, PositiveBucketCounts Array(UInt64), NegativeOffset Int32, NegativeBucketCounts Array(UInt64), " +
	"AggregationTemporality Int32 DEFAULT 2) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);\n" +
	"CREATE TABLE otel_metrics_histogram (MetricName String, Attributes Map(String, String), " +
	"ResourceAttributes Map(String, String) DEFAULT map(), ServiceName LowCardinality(String) DEFAULT '', " +
	"TimeUnix DateTime64(9), Count UInt64, Sum Float64, BucketCounts Array(UInt64), ExplicitBounds Array(Float64), " +
	"AggregationTemporality Int32 DEFAULT 2) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);\n" +
	"INSERT INTO otel_metrics_exponential_histogram (MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, " +
	"PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
	"    ('" + hrEdgeNativeMetric + "', map('job', 'edge'), fromUnixTimestamp64Nano(toInt64(9223371600000000000)), 2, 3, 0, 0, 0, [1, 1], 0, []),\n" +
	"    ('" + hrEdgeNativeMetric + "', map('job', 'edge'), fromUnixTimestamp64Nano(toInt64(9223371900000000000)), 4, 7, 0, 0, 0, [2, 2], 0, []),\n" +
	"    ('" + hrEdgeNativeMetric + "', map('job', 'floor'), fromUnixTimestamp64Nano(toInt64(-9223372000000000000)), 2, 3, 0, 0, 0, [1, 1], 0, []),\n" +
	"    ('" + hrEdgeNativeMetric + "', map('job', 'floor'), fromUnixTimestamp64Nano(toInt64(-9223371800000000000)), 4, 7, 0, 0, 0, [2, 2], 0, []);\n" +
	"INSERT INTO otel_metrics_histogram (MetricName, Attributes, TimeUnix, Count, Sum, BucketCounts, ExplicitBounds) VALUES\n" +
	"    ('" + hrEdgeClassicMetric + "', map('job', 'edge'), fromUnixTimestamp64Nano(toInt64(9223371600000000000)), 2, 3, [1, 1, 0], [1, 2]),\n" +
	"    ('" + hrEdgeClassicMetric + "', map('job', 'edge'), fromUnixTimestamp64Nano(toInt64(9223371900000000000)), 4, 7, [2, 2, 0], [1, 2]),\n" +
	"    ('" + hrEdgeClassicMetric + "', map('job', 'floor'), fromUnixTimestamp64Nano(toInt64(-9223372000000000000)), 2, 3, [1, 1, 0], [1, 2]),\n" +
	"    ('" + hrEdgeClassicMetric + "', map('job', 'floor'), fromUnixTimestamp64Nano(toInt64(-9223371800000000000)), 4, 7, [2, 2, 0], [1, 2]);\n"

// hrEdgeCeilingEval and hrEdgeFloorEval are the instant query's
// evaluation instant and the range query's last step for a case pinned at
// hrEdgeCeilingPin and at hrEdgeFloorPin. The range query starts
// hrEdgeRangeSteps one-minute steps earlier.
var (
	hrEdgeCeilingEval = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	hrEdgeFloorEval   = time.Date(1677, 9, 21, 0, 30, 0, 0, time.UTC)
)

const hrEdgeRangeSteps = 2

// hrEdgeFloatColumns and hrEdgeHistogramColumns are an instant answer's
// columns but its reported instant, which an instant query reports at the
// evaluation instant: a float's, and those a histogram adds to them.
const (
	hrEdgeFloatColumns     = "`MetricName`, `Attributes`, `Value`"
	hrEdgeHistogramColumns = ", `HistogramCount`, `HistogramSum`, `HistogramScale`, `HistogramZeroThreshold`, " +
		"`HistogramZeroCount`, `HistogramPositiveOffset`, `HistogramPositiveBucketCounts`, `HistogramNegativeOffset`, " +
		"`HistogramNegativeBucketCounts`"
)

// hrEdgeTimestampColumn is the position of the reported instant in an
// answer's row.
const hrEdgeTimestampColumn = 2

// hrEdgeWindowSeconds is the length of every case's window, which rate
// divides by.
const hrEdgeWindowSeconds = 600.0

// hrEdgeExtrapolation is the factor Prometheus stretches each window's
// increase by, the same for both pins. Over hrEdgeCeilingPin's window the
// samples lie 300s apart, 60s after its start and 240s before its end,
// none past 1.1 times the 300s interval: (300 + 60 + 240) / 300. Over
// hrEdgeFloorPin's window they lie 200s apart, 300s after its start, past
// 1.1 times the interval and so taken as half of it, and 100s before its
// end: (200 + 100 + 100) / 200. The counter's zero crossing, the first
// Count over its increase times the interval, lies further back than either
// start.
const hrEdgeExtrapolation = 2.0

// hrEdgeAnswer is a case's answer: a native histogram with Count count,
// Sum sum and both positive buckets bucket, or a float value.
type hrEdgeAnswer struct {
	histogram          bool
	count, sum, bucket float64
	value              float64
	// unlabelled marks an answer whose series carries no label.
	unlabelled bool
}

// hrEdgeIncrease is the native histogram increase scaled by factor: the
// window's increase is Count 2, Sum 4 and buckets [1, 1].
func hrEdgeIncrease(factor float64) hrEdgeAnswer {
	return hrEdgeAnswer{histogram: true, count: 2 * factor, sum: 4 * factor, bucket: factor}
}

// TestHistogramRateStorageEdgeParity_ChDB pins cerberus issue #3933 against
// the Prometheus reference engine, with the answers written out by hand.
func TestHistogramRateStorageEdgeParity_ChDB(t *testing.T) {
	const fixturePermissions = 0o600
	rate := hrEdgeExtrapolation / hrEdgeWindowSeconds
	sumRate := hrEdgeIncrease(rate)
	sumRate.unlabelled = true
	subtests := spec.NewSubtestShard(t)
	cases := []struct {
		name   string
		query  string
		eval   time.Time
		job    string
		answer hrEdgeAnswer
	}{
		{"native_rate_ceiling", `rate(` + hrEdgeNativeMetric + `[10m] @ ` + hrEdgeCeilingPin + `)`, hrEdgeCeilingEval, "edge", hrEdgeIncrease(rate)},
		{"native_increase_ceiling", `increase(` + hrEdgeNativeMetric + `[10m] @ ` + hrEdgeCeilingPin + `)`, hrEdgeCeilingEval, "edge", hrEdgeIncrease(hrEdgeExtrapolation)},
		{"native_delta_ceiling", `delta(` + hrEdgeNativeMetric + `[10m] @ ` + hrEdgeCeilingPin + `)`, hrEdgeCeilingEval, "edge", hrEdgeIncrease(hrEdgeExtrapolation)},
		{"native_rate_floor", `rate(` + hrEdgeNativeMetric + `[10m] @ ` + hrEdgeFloorPin + `)`, hrEdgeFloorEval, "floor", hrEdgeIncrease(rate)},
		{"native_increase_floor", `increase(` + hrEdgeNativeMetric + `[10m] @ ` + hrEdgeFloorPin + `)`, hrEdgeFloorEval, "floor", hrEdgeIncrease(hrEdgeExtrapolation)},
		{
			"native_histogram_count_rate_ceiling", `histogram_count(rate(` + hrEdgeNativeMetric + `[10m] @ ` + hrEdgeCeilingPin + `))`, hrEdgeCeilingEval, "edge",
			hrEdgeAnswer{value: 2 * rate},
		},
		{"native_sum_rate_ceiling", `sum(rate(` + hrEdgeNativeMetric + `[10m] @ ` + hrEdgeCeilingPin + `))`, hrEdgeCeilingEval, "", sumRate},
		// The quantile reads the ratios between buckets, with F the
		// window's factor: 0.5 of the native buckets [F, F] ends the first
		// one, (1, 2], at 2; 0.5 of the classic cumulative ladder
		// [F, 2F, 2F] ends the le="1" rung at 1.
		{
			"native_quantile_rate_ceiling", `histogram_quantile(0.5, rate(` + hrEdgeNativeMetric + `[10m] @ ` + hrEdgeCeilingPin + `))`, hrEdgeCeilingEval, "edge",
			hrEdgeAnswer{value: 2},
		},
		{
			"classic_quantile_increase_ceiling", `histogram_quantile(0.5, increase(` + hrEdgeClassicMetric + `_bucket[10m] @ ` + hrEdgeCeilingPin + `))`, hrEdgeCeilingEval, "edge",
			hrEdgeAnswer{value: 1},
		},
		{
			"classic_quantile_rate_floor", `histogram_quantile(0.5, rate(` + hrEdgeClassicMetric + `_bucket[10m] @ ` + hrEdgeFloorPin + `))`, hrEdgeFloorEval, "floor",
			hrEdgeAnswer{value: 1},
		},
	}
	for _, rangeMode := range []bool{false, true} {
		for _, tc := range cases {
			mode := "instant"
			if rangeMode {
				mode = "range"
			}
			subtests.Run(tc.name+"/"+mode, func(t *testing.T) {
				start, step, endpoint := tc.eval, time.Duration(0), "/api/v1/query"
				if rangeMode {
					start, step, endpoint = tc.eval.Add(-hrEdgeRangeSteps*time.Minute), time.Minute, "/api/v1/query_range"
				}
				expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(tc.query)
				if err != nil {
					t.Fatal(err)
				}
				plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), start, tc.eval, step)
				if err != nil {
					t.Fatalf("LowerAtRange(%q): %v", tc.query, err)
				}
				plan = spec.AssertScanTimeBoundAccepts(t, plan)
				sql, args, err := chsql.Emit(context.Background(), plan)
				if err != nil {
					t.Fatal(err)
				}
				labels := map[string]string{"job": tc.job}
				if tc.answer.unlabelled {
					labels = map[string]string{}
				}
				// The answer's own columns: the reported instant only in range
				// mode, where it differs per step.
				cells := [][]any{}
				for at := start; !at.After(tc.eval); at = at.Add(time.Minute) {
					a := tc.answer
					cell := []any{"", labels, at.Format(time.RFC3339), a.value}
					if a.histogram {
						cell = append(cell, a.count, a.sum, 0, 0, 0, 0, []float64{a.bucket, a.bucket}, 0, []float64{})
					}
					if !rangeMode {
						cell = slices.Delete(cell, hrEdgeTimestampColumn, hrEdgeTimestampColumn+1)
					}
					cells = append(cells, cell)
				}
				if !rangeMode {
					columns := hrEdgeFloatColumns
					if tc.answer.histogram {
						columns += hrEdgeHistogramColumns
					}
					sql = "SELECT " + columns + " FROM (" + sql + ")"
				}
				expected, err := json.Marshal(cells)
				if err != nil {
					t.Fatal(err)
				}
				archive := &txtar.Archive{Files: []txtar.File{
					{Name: "query.promql", Data: []byte(tc.query + "\n")},
					{Name: "parity", Data: []byte("oracle: prometheus\nendpoint: " + endpoint + "\nscope: full\n")},
					{Name: "seed", Data: []byte(hrEdgeSeed)},
					{Name: "expected_rows", Data: expected},
				}}
				path := filepath.Join(t.TempDir(), "histogram_rate_storage_edge.txtar")
				if err := os.WriteFile(path, txtar.Format(archive), fixturePermissions); err != nil {
					t.Fatal(err)
				}
				fixture, err := spec.Load(path)
				if err != nil {
					t.Fatal(err)
				}
				actual := spec.RunRoundTripSQL(t, fixture, sql, args)
				spec.RunParity(t, fixture, spec.ParityEval{Start: start, End: tc.eval, Step: step}, actual)
			})
		}
	}
}
