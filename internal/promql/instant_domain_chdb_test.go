//go:build chdb

// chDB proof, against the Prometheus reference engine, that queries whose
// `@` pins, offsets or subquery grids leave ClickHouse's DateTime64(9)
// range are answered in SQL with Prometheus's own answer, in instant and
// range mode.
package promql_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"
	"golang.org/x/tools/txtar"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/spec"
)

const instantDomainMetric = "instant_domain_probe"

// instantDomainDemoMetric is the metric the rejection-parity lane seeds,
// which the nested-subquery trigger query reads.
const instantDomainDemoMetric = "demo_http_requests_total"

// instantDomainSeed holds a sample just before instantDomainEval, one in
// 1811, before the earliest instant a parsed DateTime64 string names, and
// two minutes apart near each edge of the stored range: 2262-04-11 23:40
// and 23:45 (job "edge", for instantDomainMetric and for
// instantDomainDemoMetric), 1677-09-21 00:20 and 00:25 (job "floor"), and
// one at 1677-09-21 00:13 (job "first"), seventeen seconds after the
// earliest stored instant.
const instantDomainSeed = "" +
	"CREATE TABLE otel_metrics_gauge (MetricName String, Attributes Map(String, String), TimeUnix DateTime64(9), Value Float64) " +
	"ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);\n" +
	"CREATE TABLE otel_metrics_sum (MetricName String, Attributes Map(String, String), TimeUnix DateTime64(9), Value Float64) " +
	"ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);\n" +
	"INSERT INTO otel_metrics_gauge VALUES\n" +
	"    ('" + instantDomainMetric + "', map('job', 'api'), toDateTime64('2025-12-31 23:59:00', 9), 2.0),\n" +
	"    ('" + instantDomainMetric + "', map('job', 'old'), fromUnixTimestamp64Nano(toInt64(-5000000060000000000)), 3.0),\n" +
	"    ('" + instantDomainMetric + "', map('job', 'edge'), fromUnixTimestamp64Nano(toInt64(9223371600000000000)), 4.0),\n" +
	"    ('" + instantDomainMetric + "', map('job', 'edge'), fromUnixTimestamp64Nano(toInt64(9223371900000000000)), 6.0),\n" +
	"    ('" + instantDomainMetric + "', map('job', 'floor'), fromUnixTimestamp64Nano(toInt64(-9223371600000000000)), 5.0),\n" +
	"    ('" + instantDomainMetric + "', map('job', 'floor'), fromUnixTimestamp64Nano(toInt64(-9223371300000000000)), 7.0),\n" +
	"    ('" + instantDomainMetric + "', map('job', 'first'), fromUnixTimestamp64Nano(toInt64(-9223372020000000000)), 9.0);\n" +
	"INSERT INTO otel_metrics_sum VALUES\n" +
	"    ('" + instantDomainDemoMetric + "', map('job', 'edge'), fromUnixTimestamp64Nano(toInt64(9223371600000000000)), 4.0),\n" +
	"    ('" + instantDomainDemoMetric + "', map('job', 'edge'), fromUnixTimestamp64Nano(toInt64(9223371900000000000)), 6.0);\n"

var (
	instantDomainEval = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	// instantDomainOldSample is the 1811 sample's timestamp.
	instantDomainOldSample = time.Unix(-5000000060, 0).UTC()
	// Request instants a few minutes inside the int64-nanosecond floor and
	// ceiling, to which the API's request parsing clamps a request.
	instantDomainNearFloor   = time.Date(1677, 9, 21, 0, 20, 0, 0, time.UTC)
	instantDomainNearCeiling = time.Date(2262, 4, 11, 23, 46, 0, 0, time.UTC)
	// instantDomainFirstSample is the job "first" sample's timestamp.
	instantDomainFirstSample = time.Unix(-9223372020, 0).UTC()
)

// instantDomainRangeSteps is how many one-minute steps before the
// evaluation instant a range case starts.
const instantDomainRangeSteps = 2

// instantDomainInstantOnlyCases names the cases asked only as an instant
// query: a range vector, which a range query may not ask, and an unpinned
// window crossing the floor, which a range query evaluates at many
// instants and so is rejected there (TestInstantDomain_RangeStepMargin).
var instantDomainInstantOnlyCases = map[string]bool{
	"root_matrix_near_ceiling":               true,
	"root_matrix_past_ceiling":               true,
	"root_matrix_reads_past_ceiling":         true,
	"root_matrix_read_at_year_one":           true,
	"window_below_floor_at_request":          true,
	"increase_window_below_floor_at_request": true,
}

// instantDomainRow is one series of a case's answer. at maps a step
// instant to the value the series carries there; sampleAt, when set, is
// the instant an instant query reports for a selected sample in place of
// the evaluation instant.
type instantDomainRow struct {
	name     string
	labels   map[string]string
	at       func(step time.Time) float64
	sampleAt time.Time
}

func idConst(v float64) func(time.Time) float64 { return func(time.Time) float64 { return v } }

// idStepSeconds is the value of a series that carries, at each step, that
// step's own Unix seconds: the last grid instant of a 1m-step subquery
// ending on a whole minute.
func idStepSeconds(step time.Time) float64 { return float64(step.Unix()) }

// idFloorRate is Prometheus's rate over the 15m window ending at step of
// the job "floor" samples, 5 at 00:20 and 7 at 00:25: the increase of 2
// over their 300s, extrapolated by half the 300s interval towards the
// window's start, more than 330s away, and by the step's distance past
// 00:25 towards its end, then divided by the window's 900s.
func idFloorRate(step time.Time) float64 {
	const (
		sampled      = 300.0
		toStartLimit = sampled / 2
		increase     = 2.0
		window       = 900.0
	)
	last := time.Unix(-9223371300, 0)
	factor := (sampled + toStartLimit + step.Sub(last).Seconds()) / sampled / window
	return increase * factor
}

// edgeGridRows is the answer of the job "edge" selector over the grid of
// a 10m:1m subquery pinned at 2262-04-11 23:49, which reaches past the
// last stored instant: at each minute from 23:40 the newest sample of its
// lookback, 4 from 23:40 and 6 from 23:45.
func edgeGridRows() []instantDomainRow {
	const (
		gridStart     = 9223371600
		gridPoints    = 10
		firstSampleOf = 5
	)
	rows := make([]instantDomainRow, 0, gridPoints)
	for i := range gridPoints {
		v := 4.0
		if i >= firstSampleOf {
			v = 6
		}
		rows = append(rows, instantDomainRow{
			name: instantDomainMetric, labels: map[string]string{"job": "edge"}, at: idConst(v),
			sampleAt: time.Unix(gridStart, 0).Add(time.Duration(i) * time.Minute).UTC(),
		})
	}
	return rows
}

func idRow(labels map[string]string, at func(time.Time) float64) instantDomainRow {
	return instantDomainRow{labels: labels, at: at}
}

// TestInstantDomainParity_ChDB pins the shapes of out-of-domain pins against
// the Prometheus reference engine, with the answers written out by hand.
func TestInstantDomainParity_ChDB(t *testing.T) {
	noLabels := map[string]string{}
	edge := map[string]string{"job": "edge"}
	floor := map[string]string{"job": "floor"}
	first := map[string]string{"job": "first"}
	five := []instantDomainRow{idRow(noLabels, idConst(5))}
	cases := []struct {
		name  string
		query string
		eval  time.Time
		rows  []instantDomainRow
	}{
		// The issue's reference answers: a constant inner is evaluated at
		// the instants Prometheus evaluates the pin at, a stored series is
		// empty there.
		{"year_one_constant", `count_over_time(vector(1)[5m:1m] @ -62135596800)`, instantDomainEval, five},
		{"year_one_selector_subquery", `count_over_time(` + instantDomainMetric + `[5m:1m] @ -62135596800)`, instantDomainEval, nil},
		{"year_one_selector", instantDomainMetric + ` @ -62135596800`, instantDomainEval, nil},
		{"before_int64_floor_constant", `count_over_time(vector(1)[5m:1m] @ -9300000000)`, instantDomainEval, five},
		{"pin_1e17_constant", `count_over_time(vector(1)[5m:1m] @ 1e17)`, instantDomainEval, five},
		{"past_int64_ceiling_constant", `count_over_time(vector(1)[5m:1m] @ 10000000000)`, instantDomainEval, five},
		// A subquery window reaching below the floor, by its range or by
		// an offset.
		{"window_below_floor", `count_over_time(vector(1)[5m:1m] @ -9223372036.854)`, instantDomainEval, five},
		{"window_below_floor_by_offset", `count_over_time(vector(1)[5m:1m] @ -9223372036 offset 1h)`, instantDomainEval, five},
		// The evaluation instant itself as a value: Prometheus's wrapped
		// instant in 1754 for year one, the pin itself past 2262.
		{
			"year_one_timestamp", `max_over_time(timestamp(vector(1))[5m:1m] @ -62135596800)`, instantDomainEval,
			[]instantDomainRow{idRow(noLabels, idConst(-6795364620))},
		},
		{
			"past_ceiling_timestamp", `max_over_time(timestamp(vector(1))[5m:1m] @ 10000000000)`, instantDomainEval,
			[]instantDomainRow{idRow(noLabels, idConst(9999999960))},
		},
		{
			"past_ceiling_parenthesised_timestamp", `max_over_time((timestamp(vector(1)))[5m:1m] @ 10000000000)`, instantDomainEval,
			[]instantDomainRow{idRow(noLabels, idConst(9999999960))},
		},
		{
			"past_ceiling_time", `max_over_time(vector(time())[5m:1m] @ 10000000000)`, instantDomainEval,
			[]instantDomainRow{idRow(noLabels, idConst(9999999960))},
		},
		{
			"past_ceiling_year", `max_over_time(year()[5m:1m] @ 10000000000)`, instantDomainEval,
			[]instantDomainRow{idRow(noLabels, idConst(2286))},
		},
		{
			"past_ceiling_ts_of_max", `ts_of_max_over_time(vector(1)[5m:1m] @ 10000000000)`, instantDomainEval,
			[]instantDomainRow{idRow(noLabels, idConst(9999999960))},
		},
		// Prometheus reports max(t, 0) for the last sample of a float-only
		// window, so a window before 1970 reports 0.
		{
			"year_one_ts_of_last", `ts_of_last_over_time(vector(1)[5m:1m] @ -62135596800)`, instantDomainEval,
			[]instantDomainRow{idRow(noLabels, idConst(0))},
		},
		// Mixes of a constant arm and a read: `or` keeps the constant
		// side, arithmetic answers nothing.
		{"or_keeps_constant_in_subquery", `count_over_time((` + instantDomainMetric + ` or vector(1))[5m:1m] @ -62135596800)`, instantDomainEval, five},
		{
			"or_keeps_constant", instantDomainMetric + ` @ -62135596800 or vector(7)`, instantDomainEval,
			[]instantDomainRow{idRow(noLabels, idConst(7))},
		},
		{"arithmetic_is_empty", `vector(1) * on() (` + instantDomainMetric + ` @ -62135596800)`, instantDomainEval, nil},
		{"aggregated_read_past_ceiling", `sum(rate(` + instantDomainMetric + `[5m] @ 10000000000))`, instantDomainEval, nil},
		// absent answers from the selector's matchers.
		{
			"absent_pin_1e17", `absent(` + instantDomainMetric + `{job="a"} @ 1e17)`, instantDomainEval,
			[]instantDomainRow{idRow(map[string]string{"job": "a"}, idConst(1))},
		},
		{
			"absent_over_time_year_one", `absent_over_time(` + instantDomainMetric + `{job="api"}[5m] @ -62135596800)`, instantDomainEval,
			[]instantDomainRow{idRow(map[string]string{"job": "api"}, idConst(1))},
		},
		// A sample before 1900 is read at its own instant.
		{"selector_before_1900", instantDomainMetric + `{job="old"} @ -5000000000`, instantDomainEval, []instantDomainRow{{
			name: instantDomainMetric, labels: map[string]string{"job": "old"}, at: idConst(3), sampleAt: instantDomainOldSample,
		}}},
		{
			"window_before_1900", `count_over_time(` + instantDomainMetric + `{job="old"}[10m] @ -5000000000)`, instantDomainEval,
			[]instantDomainRow{idRow(map[string]string{"job": "old"}, idConst(1))},
		},
		// A root subquery answers its grid instants, which lie in the
		// range although its window reaches past it.
		{name: "root_matrix_near_ceiling", query: `vector(1)[5m:1m] @ -9223372036.854`, eval: instantDomainEval, rows: []instantDomainRow{
			{labels: noLabels, at: idConst(1), sampleAt: time.UnixMilli(9223371780000).UTC()},
			{labels: noLabels, at: idConst(1), sampleAt: time.UnixMilli(9223371840000).UTC()},
			{labels: noLabels, at: idConst(1), sampleAt: time.UnixMilli(9223371900000).UTC()},
			{labels: noLabels, at: idConst(1), sampleAt: time.UnixMilli(9223371960000).UTC()},
			{labels: noLabels, at: idConst(1), sampleAt: time.UnixMilli(9223372020000).UTC()},
		}},
		// A root subquery whose grid leaves the stored range reports its
		// instants as Unix milliseconds.
		{name: "root_matrix_past_ceiling", query: `vector(1)[3m:1m] @ 10000000000`, eval: instantDomainEval, rows: []instantDomainRow{
			{labels: noLabels, at: idConst(1), sampleAt: time.Unix(9999999840, 0).UTC()},
			{labels: noLabels, at: idConst(1), sampleAt: time.Unix(9999999900, 0).UTC()},
			{labels: noLabels, at: idConst(1), sampleAt: time.Unix(9999999960, 0).UTC()},
		}},
		{name: "root_matrix_reads_past_ceiling", query: instantDomainMetric + `{job="edge"}[10m:1m] @ 9223372140`, eval: instantDomainEval, rows: edgeGridRows()},
		{name: "root_matrix_read_at_year_one", query: instantDomainMetric + `[5m] @ -62135596800`, eval: instantDomainEval},
		// Windows reaching past the stored range read the samples inside
		// it, for every function fixed by the samples a window holds.
		{
			"window_past_ceiling", `count_over_time(` + instantDomainMetric + `{job="edge"}[10m] @ 9223372300)`, instantDomainEval,
			[]instantDomainRow{idRow(edge, idConst(1))},
		},
		{
			"window_past_ceiling_by_offset", `count_over_time(` + instantDomainMetric + `{job="edge"}[10m] @ 9223372400 offset 100s)`, instantDomainEval,
			[]instantDomainRow{idRow(edge, idConst(1))},
		},
		{
			"sum_window_past_ceiling", `sum_over_time(` + instantDomainMetric + `{job="edge"}[10m] @ 9223372100)`, instantDomainEval,
			[]instantDomainRow{idRow(edge, idConst(10))},
		},
		{
			"ts_of_first_window_past_ceiling", `ts_of_first_over_time(` + instantDomainMetric + `{job="edge"}[20m] @ 9223372300)`, instantDomainEval,
			[]instantDomainRow{idRow(edge, idConst(9223371600))},
		},
		{
			"deriv_window_past_ceiling", `deriv(` + instantDomainMetric + `{job="edge"}[20m] @ 9223372300)`, instantDomainEval,
			[]instantDomainRow{idRow(edge, idConst(2.0/300))},
		},
		// rate, increase and delta extrapolate to the window's edges: the
		// last sample sits 240s before the pin, under the 330s threshold,
		// so the 103.145...s past the ceiling count, the first sample 60s
		// after the window's start. 2 over 300s extrapolated to 600s.
		{
			"rate_window_past_ceiling", `rate(` + instantDomainMetric + `{job="edge"}[10m] @ 9223372140)`, instantDomainEval,
			[]instantDomainRow{idRow(edge, idConst(4.0/600))},
		},
		{
			"increase_window_past_ceiling", `increase(` + instantDomainMetric + `{job="edge"}[10m] @ 9223372140)`, instantDomainEval,
			[]instantDomainRow{idRow(edge, idConst(4))},
		},
		{
			"delta_window_past_ceiling", `delta(` + instantDomainMetric + `{job="edge"}[10m] @ 9223372140)`, instantDomainEval,
			[]instantDomainRow{idRow(edge, idConst(4))},
		},
		// Both edges lie past the threshold: each extrapolates 150s.
		{
			"rate_window_past_ceiling_clamped", `rate(` + instantDomainMetric + `{job="edge"}[20m] @ 9223372300)`, instantDomainEval,
			[]instantDomainRow{idRow(edge, idConst(4.0/1200))},
		},
		// The regression line through (23:40, 4) and (23:45, 6), read 60s
		// past the evaluation instant, which the pin leaves where it is.
		// Below the floor the window's start lies 510s before the first
		// sample, past the threshold: 150s, plus 30s to the pin.
		{
			"rate_window_below_floor", `rate(` + instantDomainMetric + `{job="floor"}[14m] @ -9223371270)`, instantDomainNearFloor,
			[]instantDomainRow{idRow(floor, idConst(3.2/840))},
		},
		{
			"increase_window_below_floor_at_request", `increase(` + instantDomainMetric + `{job="floor"}[14m])`, instantDomainNearFloor.Add(5*time.Minute + 30*time.Second),
			[]instantDomainRow{idRow(floor, idConst(3.2))},
		},
		{"absent_over_time_window_past_ceiling", `absent_over_time(` + instantDomainMetric + `{job="edge"}[10m] @ 9223372300)`, instantDomainEval, nil},
		{
			"window_below_floor_reads", `count_over_time(` + instantDomainMetric + `{job="floor"}[20m] @ -9223371200)`, instantDomainNearFloor,
			[]instantDomainRow{idRow(floor, idConst(2))},
		},
		{"selector_past_ceiling", instantDomainMetric + `{job="edge"} @ 9223372096.854`, instantDomainEval, []instantDomainRow{{
			name: instantDomainMetric, labels: edge, at: idConst(6), sampleAt: time.Unix(9223371900, 0).UTC(),
		}}},
		{"absent_selector_past_ceiling", `absent(` + instantDomainMetric + `{job="edge"} @ 9223372096.854)`, instantDomainEval, nil},
		{
			"absent_selector_past_ceiling_stale", `absent(` + instantDomainMetric + `{job="edge"} @ 9223372300)`, instantDomainEval,
			// An instant query reports the cut window's end, which the API
			// replaces with the evaluation instant, as for any `@` pin.
			[]instantDomainRow{{labels: edge, at: idConst(1), sampleAt: time.UnixMilli(9223372036854).UTC()}},
		},
		// The lookback is left-open: a sample at its first millisecond is
		// outside it, one a millisecond later is inside.
		{"selector_past_ceiling_lookback_start", instantDomainMetric + `{job="edge"} @ 9223372200`, instantDomainEval, nil},
		{"selector_past_ceiling_lookback_start_inside", instantDomainMetric + `{job="edge"} @ 9223372199.999`, instantDomainEval, []instantDomainRow{{
			name: instantDomainMetric, labels: edge, at: idConst(6), sampleAt: time.Unix(9223371900, 0).UTC(),
		}}},
		{"selector_past_ceiling_stale", instantDomainMetric + `{job="edge"} @ 9223372300`, instantDomainEval, nil},
		{
			"sum_selector_past_ceiling", `sum(` + instantDomainMetric + ` @ 9223372096.854)`, instantDomainEval,
			[]instantDomainRow{idRow(noLabels, idConst(6))},
		},
		{
			"window_below_floor_at_request", `count_over_time(` + instantDomainMetric + `{job="floor"}[10m])`, instantDomainNearFloor,
			[]instantDomainRow{idRow(floor, idConst(1))},
		},
		// A lookback or window reaching below the earliest stored instant
		// from a request near it, read at one instant or at every step of
		// a range.
		{"selector_lookback_below_floor", instantDomainMetric + `{job="first"}`, instantDomainFirstSample.Add(2 * time.Minute), []instantDomainRow{{
			name: instantDomainMetric, labels: first, at: idConst(9), sampleAt: instantDomainFirstSample,
		}}},
		{
			"timestamp_lookback_below_floor", `timestamp(` + instantDomainMetric + `{job="first"})`, instantDomainFirstSample.Add(2 * time.Minute),
			[]instantDomainRow{{labels: first, at: idConst(-9223372020), sampleAt: instantDomainFirstSample}},
		},
		{
			"ts_of_max_window_below_floor", `ts_of_max_over_time(` + instantDomainMetric + `{job="first"}[10m])`, instantDomainFirstSample.Add(2 * time.Minute),
			[]instantDomainRow{idRow(first, idConst(-9223372020))},
		},
		{
			"windows_below_floor_at_every_step", `count_over_time(` + instantDomainMetric + `{job="floor"}[10m])`, instantDomainNearFloor.Add(2 * time.Minute),
			[]instantDomainRow{idRow(floor, idConst(1))},
		},
		{
			"rate_windows_below_floor_at_every_step", `rate(` + instantDomainMetric + `{job="floor"}[15m])`, instantDomainNearFloor.Add(7 * time.Minute),
			[]instantDomainRow{idRow(floor, idFloorRate)},
		},
		// A subquery grid reaching past an edge that reads the stored
		// samples inside it.
		{
			"subquery_reads_past_ceiling", `count_over_time(` + instantDomainMetric + `{job="edge"}[1h:1m] @ 9223373836.854)`, instantDomainEval,
			[]instantDomainRow{idRow(edge, idConst(10))},
		},
		{
			"subquery_timestamp_past_ceiling", `max_over_time(timestamp(` + instantDomainMetric + `{job="edge"})[1h:1m] @ 9223373836.854)`, instantDomainEval,
			[]instantDomainRow{idRow(edge, idConst(9223371900))},
		},
		{
			"subquery_reads_below_floor", `count_over_time(` + instantDomainMetric + `{job="floor"}[30m:1m] @ -9223371000)`, instantDomainNearFloor,
			[]instantDomainRow{idRow(floor, idConst(10))},
		},
		{
			"mixed_subquery_past_ceiling", `count_over_time((` + instantDomainMetric + `{job="edge"} or vector(1))[10m:1m] @ 9223372300)`, instantDomainEval,
			[]instantDomainRow{idRow(noLabels, idConst(10)), idRow(edge, idConst(8))},
		},
		// Requests near the bounds, where the unpinned grid leaves the
		// range.
		{"near_floor_constant", `count_over_time(vector(1)[5m:1m])`, instantDomainNearFloor, five},
		{
			"near_floor_timestamp", `max_over_time(timestamp(vector(1))[5m:1m])`, instantDomainNearFloor,
			[]instantDomainRow{idRow(noLabels, idStepSeconds)},
		},
		{"near_ceiling_constant", `count_over_time(vector(1)[5m:1m] offset -3m)`, instantDomainNearCeiling, five},
		{
			"near_ceiling_timestamp", `max_over_time(timestamp(vector(1))[5m:1m])`, instantDomainNearCeiling,
			[]instantDomainRow{idRow(noLabels, idStepSeconds)},
		},
	}
	for _, rangeMode := range []bool{false, true} {
		for _, tc := range cases {
			mode := "instant"
			if rangeMode {
				mode = "range"
			}
			if rangeMode && instantDomainInstantOnlyCases[tc.name] {
				continue
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				runHandWrittenParity(t, instantDomainSeed, tc.query, tc.eval, tc.rows, rangeMode)
			})
		}
	}
}

// TestInstantDomainParity_NestedRegions_ChDB pins, against the Prometheus
// reference engine, subqueries whose region holds a nested subquery or an
// `@ start()` / `@ end()` pin and whose instants leave the DateTime64(9)
// range: past its ceiling, before its floor, and at a pin Prometheus
// evaluates at a wrapped instant.
func TestInstantDomainParity_NestedRegions_ChDB(t *testing.T) {
	noLabels := map[string]string{}
	edge := map[string]string{"job": "edge"}
	api := map[string]string{"job": "api"}
	floor := map[string]string{"job": "floor"}
	five := []instantDomainRow{idRow(noLabels, idConst(5))}
	// afterAPISample is an evaluation instant whose range-mode start, two
	// minutes earlier, still follows the job "api" sample at 23:59.
	afterAPISample := instantDomainEval.Add(90 * time.Second)
	cases := []struct {
		name  string
		query string
		eval  time.Time
		rows  []instantDomainRow
	}{
		// The rejection-parity trigger: a nested grid reading the stored
		// samples at 23:40 and 23:45 from a pin past the ceiling.
		{
			"trigger_nested_reads_past_ceiling", `max_over_time(max_over_time(` + instantDomainDemoMetric + `[5m:1m])[1h:1m] @ 9223373836.854)`, instantDomainEval,
			[]instantDomainRow{idRow(edge, idConst(6))},
		},
		{
			"nested_count_reads_past_ceiling", `sum_over_time(count_over_time(` + instantDomainMetric + `{job="edge"}[5m:1m])[1h:1m] @ 9223373836.854)`, instantDomainEval,
			// From 23:40 to 23:49 the 23:40 or 23:45 sample is in the
			// lookback. The outer points from 23:40 to 23:53 each count
			// those of the ten inner points within their 5m window.
			[]instantDomainRow{idRow(edge, idConst(1+2+3+4+5+5+5+5+5+5+4+3+2+1))},
		},
		{
			"nested_reads_below_floor", `max_over_time(max_over_time(` + instantDomainMetric + `{job="floor"}[5m:1m])[30m:1m] @ -9223371000)`, instantDomainNearFloor,
			[]instantDomainRow{idRow(floor, idConst(7))},
		},
		// The issue's repros: a constant nested grid past the ceiling, and
		// one under a pin Prometheus evaluates in 1754.
		{
			"nested_ts_of_last_past_ceiling", `max_over_time(ts_of_last_over_time(vector(1)[2m:1m])[5m:1m] @ 10000000000)`, instantDomainEval,
			[]instantDomainRow{idRow(noLabels, idConst(9999999960))},
		},
		{
			"parenthesised_nested_ts_of_last_past_ceiling", `max_over_time((ts_of_last_over_time(vector(1)[2m:1m]))[5m:1m] @ 10000000000)`, instantDomainEval,
			[]instantDomainRow{idRow(noLabels, idConst(9999999960))},
		},
		{
			"nested_timestamp_at_wrapped_pin", `max_over_time(max_over_time(timestamp(vector(1))[2m:7s])[5m:1m] @ -62135596800)`, instantDomainEval,
			[]instantDomainRow{idRow(noLabels, idConst(-6795364625))},
		},
		// A pin in 1675 evaluated from 1677: the last 7s multiple at or
		// before the pin, a whole minute. From 2026 Prometheus evaluates
		// it in 2259, at its wrapped instant.
		{
			"nested_timestamp_before_floor", `max_over_time(max_over_time(timestamp(vector(1))[2m:7s])[5m:1m] @ -9300000000)`, instantDomainNearFloor,
			[]instantDomainRow{idRow(noLabels, idConst(-9300000003))},
		},
		{
			"nested_timestamp_before_floor_wrapped", `max_over_time(max_over_time(timestamp(vector(1))[2m:7s])[5m:1m] @ -9300000000)`, instantDomainEval,
			[]instantDomainRow{idRow(noLabels, idConst(9146744040))},
		},
		// A pinned subquery nested in a shifted region evaluates at its own
		// pin.
		{
			"pinned_nested_in_region_past_ceiling", `max_over_time(max_over_time(timestamp(vector(1))[2m:1m] @ 1767225600)[5m:1m] @ 10000000000)`, instantDomainEval,
			[]instantDomainRow{idRow(noLabels, idConst(1767225600))},
		},
		// An unpinned nested grid crossing the floor from a request near it.
		{
			"nested_grid_near_floor", `max_over_time(max_over_time(timestamp(vector(1))[5m:1m])[3m:1m])`, instantDomainNearFloor,
			[]instantDomainRow{idRow(noLabels, idStepSeconds)},
		},
		// Request pins inside a region past the ceiling, below the floor
		// and at a wrapped pin.
		{
			"end_pin_in_grid_past_ceiling", `count_over_time((` + instantDomainMetric + `{job="api"} @ end())[5m:1m] @ 10000000000)`, instantDomainEval,
			[]instantDomainRow{idRow(api, idConst(5))},
		},
		{
			"start_pin_in_grid_past_ceiling", `count_over_time((` + instantDomainMetric + `{job="api"} @ start())[5m:1m] @ 10000000000)`, afterAPISample,
			[]instantDomainRow{idRow(api, idConst(5))},
		},
		{
			"end_pin_in_grid_before_floor", `count_over_time((` + instantDomainMetric + `{job="api"} @ end())[5m:1m] @ -9300000000)`, instantDomainEval,
			[]instantDomainRow{idRow(api, idConst(5))},
		},
		// A request pin in a grid that reads stored samples past the
		// ceiling, which the whole query is shifted to answer.
		{
			"end_pin_in_framed_grid_past_ceiling", `count_over_time((` + instantDomainMetric + `{job="edge"} and on() (` + instantDomainMetric + `{job="api"} @ end()))[1h:1m] @ 9223373836.854)`, instantDomainEval,
			[]instantDomainRow{idRow(edge, idConst(10))},
		},
		{
			"start_pin_in_framed_grid_past_ceiling", `count_over_time((` + instantDomainMetric + `{job="edge"} and on() (` + instantDomainMetric + `{job="api"} @ start()))[1h:1m] @ 9223373836.854)`, afterAPISample,
			[]instantDomainRow{idRow(edge, idConst(10))},
		},
		{"end_pin_in_wrapped_grid", `count_over_time((vector(1) and on() (` + instantDomainMetric + ` @ end()))[5m:1m] @ -62135596800)`, instantDomainEval, five},
		{
			"end_pin_subquery_in_grid_past_ceiling", `max_over_time(max_over_time(timestamp(vector(1))[2m:1m] @ end())[5m:1m] @ 10000000000)`, afterAPISample,
			[]instantDomainRow{idRow(noLabels, idConst(1767225660))},
		},
	}
	for _, rangeMode := range []bool{false, true} {
		for _, tc := range cases {
			mode := "instant"
			if rangeMode {
				mode = "range"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				runHandWrittenParity(t, instantDomainSeed, tc.query, tc.eval, tc.rows, rangeMode)
			})
		}
	}
}

// runHandWrittenParity lowers query at eval, as an instant query or as a
// range query over the instantDomainRangeSteps one-minute steps before
// eval, runs it in chDB over seed, and checks the answer against both the
// hand-written rows and the Prometheus reference engine.
func runHandWrittenParity(t *testing.T, seed, query string, eval time.Time, rows []instantDomainRow, rangeMode bool) {
	t.Helper()
	const fixturePermissions = 0o600
	start, step, endpoint := eval, time.Duration(0), "/api/v1/query"
	if rangeMode {
		start, step, endpoint = eval.Add(-instantDomainRangeSteps*time.Minute), time.Minute, "/api/v1/query_range"
	}
	expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(query)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), start, eval, step)
	if err != nil {
		t.Fatalf("LowerAtRange(%q): %v", query, err)
	}
	plan = spec.AssertScanTimeBoundAccepts(t, plan)
	sql, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	sql, cells := instantDomainCells(t, seed, sql, args, rows, start, eval, rangeMode)
	expected, err := json.Marshal(cells)
	if err != nil {
		t.Fatal(err)
	}
	archive := &txtar.Archive{Files: []txtar.File{
		{Name: "query.promql", Data: []byte(query + "\n")},
		{Name: "parity", Data: []byte("oracle: prometheus\nendpoint: " + endpoint + "\nscope: full\n")},
		{Name: "seed", Data: []byte(seed)},
		{Name: "expected_rows", Data: expected},
	}}
	path := filepath.Join(t.TempDir(), "hand_written_parity.txtar")
	if err := os.WriteFile(path, txtar.Format(archive), fixturePermissions); err != nil {
		t.Fatal(err)
	}
	fixture, err := spec.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	actual := spec.RunRoundTripSQL(t, fixture, sql, args)
	spec.RunParity(t, fixture, spec.ParityEval{Start: start, End: eval, Step: step}, actual)
}

// instantDomainCells lays the hand-written answer rows out as the
// expected_rows of the emitted SQL, whose result columns depend on the
// plan's shape: each column takes the series name, its labels, the
// reported instant or the value, by the column's name.
//
// A range plan over a subquery also returns anchor_ts, the instant its
// grid is evaluated at, which differs from the reported TimeUnix by any
// offset and by the shift that moves a grid into ClickHouse's range. No
// client sees it, so the returned SQL, which the round trip runs, leaves
// it out whenever TimeUnix is there to report the instant.
func instantDomainCells(t *testing.T, seed, sql string, args []any, want []instantDomainRow, start, end time.Time, rangeMode bool) (string, [][]any) {
	t.Helper()
	db := spec.OpenChDB(t)
	spec.ApplySeed(t, db, seed)
	q, a := spec.SubstituteNow64(sql, args)
	rows, err := db.Query(q, a...)
	if err != nil {
		t.Fatalf("query emitted SQL: %v", err)
	}
	cols, err := rows.Columns()
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatalf("result columns: %v", err)
	}
	// A root reports an instant no DateTime64(9) holds as Int64 Unix
	// milliseconds.
	millis := false
	if slices.Contains(cols, "TimeUnix") {
		var typ string
		err := db.QueryRow("SELECT toTypeName(`TimeUnix`) FROM ("+q+") LIMIT 1", a...).Scan(&typ)
		millis = err == nil && typ == "Int64"
	}
	if slices.Contains(cols, "anchor_ts") && slices.Contains(cols, "TimeUnix") {
		cols = slices.DeleteFunc(cols, func(c string) bool { return c == "anchor_ts" })
		sql = "SELECT `" + strings.Join(cols, "`, `") + "` FROM (" + sql + ")"
	}
	cells := [][]any{}
	for at := start; !at.After(end); at = at.Add(time.Minute) {
		for _, r := range want {
			ts := at
			if !rangeMode && !r.sampleAt.IsZero() {
				ts = r.sampleAt
			}
			cell := make([]any, 0, len(cols))
			for _, c := range cols {
				switch c {
				case "MetricName":
					cell = append(cell, r.name)
				case "Attributes":
					cell = append(cell, r.labels)
				case "Value":
					cell = append(cell, r.at(at))
				case "TimeUnix":
					if millis {
						cell = append(cell, ts.UnixMilli())
					} else {
						cell = append(cell, ts.Format(time.RFC3339Nano))
					}
				default:
					cell = append(cell, ts.Format(time.RFC3339Nano))
				}
			}
			cells = append(cells, cell)
		}
	}
	return sql, cells
}

// TestInstantDomainParity_RangeSpanningTheDomain_ChDB pins, against the
// Prometheus reference engine, a range query whose steps span the whole
// stored range: its first step's lookback reaches below the earliest
// stored instant and its last step sits a minute before the latest one,
// so no one shift of its instants fits, and each half of its steps is
// answered in a frame of its own.
func TestInstantDomainParity_RangeSpanningTheDomain_ChDB(t *testing.T) {
	const fixturePermissions = 0o600
	query := instantDomainMetric + `{job=~"first|edge"}`
	start := instantDomainFirstSample.Add(2 * time.Minute)
	end := instantDomainNearCeiling
	// Ten thousand steps of about 21 days, under Prometheus's limit of
	// 11000 points a series, land the last one exactly at end.
	const steps = 10000
	step := time.Duration((end.UnixMilli()-start.UnixMilli())/steps) * time.Millisecond
	if start.UnixMilli()+steps*step.Milliseconds() != end.UnixMilli() {
		t.Fatalf("%d steps of %s from %s miss %s", steps, step, start, end)
	}
	expr, err := parser.NewParser(parser.Options{}).ParseExpr(query)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), start, end, step)
	if err != nil {
		t.Fatalf("LowerAtRange(%q): %v", query, err)
	}
	plan = spec.AssertScanTimeBoundAccepts(t, plan)
	sql, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	cells := [][]any{
		{instantDomainMetric, map[string]string{"job": "first"}, start.Format(time.RFC3339Nano), 9.0},
		{instantDomainMetric, map[string]string{"job": "edge"}, end.Format(time.RFC3339Nano), 6.0},
	}
	expected, err := json.Marshal(cells)
	if err != nil {
		t.Fatal(err)
	}
	archive := &txtar.Archive{Files: []txtar.File{
		{Name: "query.promql", Data: []byte(query + "\n")},
		{Name: "parity", Data: []byte("oracle: prometheus\nendpoint: /api/v1/query_range\nscope: full\n")},
		{Name: "seed", Data: []byte(instantDomainSeed)},
		{Name: "expected_rows", Data: expected},
	}}
	path := filepath.Join(t.TempDir(), "instant_domain_span.txtar")
	if err := os.WriteFile(path, txtar.Format(archive), fixturePermissions); err != nil {
		t.Fatal(err)
	}
	fixture, err := spec.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	actual := spec.RunRoundTripSQL(t, fixture, sql, args)
	spec.RunParity(t, fixture, spec.ParityEval{Start: start, End: end, Step: step}, actual)
}
