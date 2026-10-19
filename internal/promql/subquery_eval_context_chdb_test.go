//go:build chdb

// chDB proof, against the Prometheus reference engine, that the nodes
// below a subquery are evaluated in their own context at every step of
// the subquery's grid: an `@` pin, `@ start()` / `@ end()`, an offset, and
// a nested subquery's own step-aligned grid, in instant and range mode.
package promql_test

import (
	"testing"
	"time"
)

// subqueryContextSeed holds `m{job="api"}` an hour before
// subqueryContextEval and in the minutes before it, and
// `instant_domain_probe{job="api"}` a minute before it.
const subqueryContextSeed = "" +
	"CREATE TABLE otel_metrics_gauge (MetricName String, Attributes Map(String, String), TimeUnix DateTime64(9), Value Float64) " +
	"ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);\n" +
	"INSERT INTO otel_metrics_gauge VALUES\n" +
	"    ('m', map('job', 'api'), toDateTime64('2025-12-31 22:57:30', 9), 8.0),\n" +
	"    ('m', map('job', 'api'), toDateTime64('2025-12-31 22:59:00', 9), 7.0),\n" +
	"    ('m', map('job', 'api'), toDateTime64('2025-12-31 23:57:00', 9), 1.0),\n" +
	"    ('m', map('job', 'api'), toDateTime64('2025-12-31 23:58:00', 9), 2.0),\n" +
	"    ('m', map('job', 'api'), toDateTime64('2025-12-31 23:59:00', 9), 3.0),\n" +
	"    ('" + instantDomainMetric + "', map('job', 'api'), toDateTime64('2025-12-31 23:59:00', 9), 2.0);\n"

// subqueryContextEval is the instant query's evaluation instant and the
// range query's end, 2026-01-01T00:00:01Z.
var subqueryContextEval = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// idGridSeconds is the value of a series that carries, at each step, the
// Unix seconds of the last multiple of unit at or before the step moved
// back by offset, plus delta.
func idGridSeconds(unit, offset, delta time.Duration) func(time.Time) float64 {
	return func(step time.Time) float64 {
		return float64(step.Add(-offset).Truncate(unit).Add(delta).Unix())
	}
}

// TestSubqueryEvalContextParity_ChDB pins cerberus issues #3875, #3876,
// #3877, #3879 and #3880, and their siblings, against the Prometheus
// reference engine, with the answers written out by hand.
func TestSubqueryEvalContextParity_ChDB(t *testing.T) {
	noLabels := map[string]string{}
	api := map[string]string{"job": "api"}
	cases := []struct {
		name  string
		query string
		rows  []instantDomainRow
	}{
		// #3875: a pin below a subquery fixes that node's instant at every
		// step of the subquery's grid.
		{
			"pinned_subquery_in_subquery", `max_over_time(count_over_time(vector(1)[5m:1m] @ 1767200000)[10m:1m])`,
			[]instantDomainRow{idRow(noLabels, idConst(5))},
		},
		{"pinned_selector_in_pinned_subquery", `max_over_time((m @ 1767225540)[5m:1m] @ 1767225600)`, []instantDomainRow{idRow(api, idConst(3))}},
		{"pinned_selector_read_past_its_pin", `max_over_time((m @ 1767226140)[5m:1m] @ 1767226000)`, nil},
		{"pinned_selector_in_subquery", `max_over_time((m @ 1767225540)[5m:1m])`, []instantDomainRow{idRow(api, idConst(3))}},
		{"pinned_window_in_nested_call", `max_over_time(max_over_time(m[5m] @ 1767225480)[5m:1m])`, []instantDomainRow{idRow(api, idConst(2))}},
		{
			"pins_at_both_levels", `max_over_time(max_over_time(timestamp(vector(1))[2m:20s] @ 1767225500)[5m:1m] @ 1767225630)`,
			[]instantDomainRow{idRow(noLabels, idConst(1767225500))},
		},
		// #3876: a nested subquery lays its grid back from each outer step,
		// the outer grid's own multiples, not from the outer pin.
		{
			"nested_grid_at_unaligned_pin", `max_over_time(max_over_time(timestamp(vector(1))[2m:20s])[5m:1m] @ 1767225630)`,
			[]instantDomainRow{idRow(noLabels, idConst(1767225600))},
		},
		{
			"nested_grid_not_dividing_outer_step", `max_over_time(max_over_time(timestamp(vector(1))[2m:45s])[5m:1m] @ 1767000040)`,
			[]instantDomainRow{idRow(noLabels, idConst(1766999970))},
		},
		{
			"three_levels", `max_over_time(max_over_time(max_over_time(timestamp(vector(1))[2m:20s])[5m:1m])[10m:2m])`,
			[]instantDomainRow{idRow(noLabels, idGridSeconds(2*time.Minute, 0, 0))},
		},
		// The single-level sibling: the windows of a pinned subquery over a
		// range function end on the subquery's own grid, so none of them
		// holds the sample at 22:57:30.
		{"window_grid_at_unaligned_pin", `max_over_time(count_over_time(m[30s])[5m:1m] @ 1767221925)`, nil},
		// #3877: an offset at either level moves the instants the level
		// below is evaluated at.
		{
			"nested_under_offset", `max_over_time(max_over_time(timestamp(vector(1))[5m:1m])[3m:1m] offset 1h)`,
			[]instantDomainRow{idRow(noLabels, idGridSeconds(time.Minute, time.Hour, 0))},
		},
		{
			"nested_under_negative_offset", `max_over_time(max_over_time(timestamp(vector(1))[5m:1m])[3m:1m] offset -1h)`,
			[]instantDomainRow{idRow(noLabels, idGridSeconds(time.Minute, -time.Hour, 0))},
		},
		{
			"offsets_at_both_levels", `max_over_time(max_over_time(timestamp(vector(1))[2m:20s] offset 3m)[5m:1m] offset 1h)`,
			[]instantDomainRow{idRow(noLabels, idGridSeconds(time.Minute, time.Hour, -3*time.Minute))},
		},
		{"selector_offset_in_subquery", `max_over_time((m offset 1h)[5m:1m])`, []instantDomainRow{idRow(api, idConst(8))}},
		{"window_offset_in_nested_call", `max_over_time(max_over_time(m[2m] offset 1h)[5m:1m])`, []instantDomainRow{idRow(api, idConst(8))}},
		{"selector_subquery_negative_offset", `max_over_time(m[5m:1m] @ 1767218400 offset -1h)`, []instantDomainRow{idRow(api, idConst(8))}},
		{"aggregate_subquery_negative_offset", `max_over_time(sum(m)[5m:1m] @ 1767218400 offset -1h)`, []instantDomainRow{idRow(noLabels, idConst(8))}},
		// #3879: count_over_time over a nested subquery counts every step
		// of the inner window.
		{"count_nested", `max_over_time(count_over_time(vector(1)[2m:1m])[5m:1m])`, []instantDomainRow{idRow(noLabels, idConst(2))}},
		{"count_nested_45s", `max_over_time(count_over_time(vector(1)[2m:45s])[5m:1m])`, []instantDomainRow{idRow(noLabels, idConst(3))}},
		{
			"count_nested_pinned", `max_over_time(count_over_time(vector(1)[2m:45s])[5m:1m] @ 1767000000)`,
			[]instantDomainRow{idRow(noLabels, idConst(3))},
		},
		{
			"count_nested_parenthesised_pinned", `max_over_time((count_over_time(vector(1)[2m:45s]))[5m:1m] @ 1767000000)`,
			[]instantDomainRow{idRow(noLabels, idConst(3))},
		},
		// #3880: `@ start()` and `@ end()` name the request's instants
		// whatever subquery encloses them.
		{
			"end_pin_in_pinned_subquery", `count_over_time((` + instantDomainMetric + `{job="api"} @ end())[5m:1m] @ 1767000000)`,
			[]instantDomainRow{idRow(api, idConst(5))},
		},
		{"start_pin_in_pinned_subquery", `count_over_time((m @ start())[5m:1m] @ 1767000000)`, []instantDomainRow{idRow(api, idConst(5))}},
		{
			"end_pin_on_nested_subquery", `max_over_time(max_over_time(timestamp(vector(1))[2m:20s] @ end())[5m:1m])`,
			[]instantDomainRow{idRow(noLabels, idConst(float64(subqueryContextEval.Truncate(20*time.Second).Unix())))},
		},
	}
	for _, rangeMode := range []bool{false, true} {
		for _, tc := range cases {
			mode := "instant"
			if rangeMode {
				mode = "range"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				runHandWrittenParity(t, subqueryContextSeed, tc.query, subqueryContextEval, tc.rows, rangeMode)
			})
		}
	}
}

// selectorOffsetSeed holds `m{job="edge"}` at 00:40:00 (4), 00:45:00 (6)
// and 00:47:00 (30) on 2026-01-01.
const selectorOffsetSeed = "" +
	"CREATE TABLE otel_metrics_gauge (MetricName String, Attributes Map(String, String), TimeUnix DateTime64(9), Value Float64) " +
	"ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);\n" +
	"INSERT INTO otel_metrics_gauge VALUES\n" +
	"    ('m', map('job', 'edge'), toDateTime64('2026-01-01 00:40:00', 9), 4.0),\n" +
	"    ('m', map('job', 'edge'), toDateTime64('2026-01-01 00:45:00', 9), 6.0),\n" +
	"    ('m', map('job', 'edge'), toDateTime64('2026-01-01 00:47:00', 9), 30.0);\n"

// idFrom is the value of a series that answers before at every step
// before from, and after from it on.
func idFrom(from time.Time, before, after float64) func(time.Time) float64 {
	return func(step time.Time) float64 {
		if step.Before(from) {
			return before
		}
		return after
	}
}

// TestSubqueryRangeFnSelectorOffsetParity_ChDB pins cerberus issue #3886
// against the Prometheus reference engine: the offset on a range
// function's own selector below a subquery shifts that selector's window
// back from every step of the subquery's grid.
func TestSubqueryRangeFnSelectorOffsetParity_ChDB(t *testing.T) {
	edge := map[string]string{"job": "edge"}
	eval := time.Date(2026, 1, 1, 0, 46, 10, 0, time.UTC)
	cases := []struct {
		name      string
		query     string
		rows      []instantDomainRow
		rangeMode bool
	}{
		{"offset_2m/instant", `max_over_time(sum_over_time(m[10m] offset 2m)[2m:1m])`, []instantDomainRow{idRow(edge, idConst(4))}, false},
		{"offset_2m/range", `max_over_time(sum_over_time(m[10m] offset 2m)[2m:1m])`, []instantDomainRow{idRow(edge, idConst(4))}, true},
		{
			"offset_30s/range", `max_over_time(sum_over_time(m[10m] offset 30s)[2m:1m])`,
			[]instantDomainRow{idRow(edge, idFrom(eval, 4, 10))},
			true,
		},
		{"predict_linear_offset_30s/instant", `max_over_time(predict_linear(m[10m] offset 30s, 60)[2m:1m])`, []instantDomainRow{idRow(edge, idConst(6.8))}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runHandWrittenParity(t, selectorOffsetSeed, tc.query, eval, tc.rows, tc.rangeMode)
		})
	}
}
