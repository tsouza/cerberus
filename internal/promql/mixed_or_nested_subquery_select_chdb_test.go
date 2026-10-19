//go:build chdb

// chDB proof, against the Prometheus reference engine, that a float-valued
// select function over a subquery of a float/histogram `or`, itself inside
// another subquery, answers what Prometheus answers.
package promql_test

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// mixedNestedSeed is beforeEpochSeed with its column names unquoted: the
// parity oracle finds a histogram column only under its bare name, and
// reads one it cannot find as zero (cerberus issue #3895), which would
// hand the reference engine empty histograms.
var mixedNestedSeed = strings.ReplaceAll(beforeEpochSeed, "`", "")

// mixedNestedMinute is the beforeEpochOrigin-relative instant of the
// given whole minute.
func mixedNestedMinute(minute int) time.Time {
	return beforeEpochOrigin.Add(time.Duration(minute) * time.Minute)
}

// mixedNestedFirstPoint is the earliest grid instant of the `or` subquery
// [10m:1m] evaluated at step that holds a sample. Over mixedNestedSeed the
// `or` answers a float at minutes -4 to 0 and a histogram at minutes 1 to
// 6, so the window (step-10m, step] starts at minute -4 or nine minutes
// before step, whichever is later.
func mixedNestedFirstPoint(step time.Time) time.Time {
	first := step.Add(-9 * time.Minute)
	if floor := mixedNestedMinute(-4); first.Before(floor) {
		return floor
	}
	return first
}

// mixedNestedLastPoint is the latest grid instant of that window holding
// a sample: step, or minute 6 once the histogram samples go stale.
func mixedNestedLastPoint(step time.Time) time.Time {
	if last := mixedNestedMinute(6); step.After(last) {
		return last
	}
	return step
}

// TestMixedOrNestedSubquerySelectParity_ChDB pins cerberus issue #3892:
// count_over_time, present_over_time, ts_of_first_over_time,
// ts_of_last_over_time, resets and changes over a subquery of
// `sum(hist) or sum(float)`, inside another subquery. The outer [1m:1m]
// subquery holds one anchor, the evaluation step, so max_over_time
// reports the inner function's value there.
//
// Each query is asked over three windows: one holding float and
// histogram samples, one holding only histogram samples and one holding
// only float samples. All samples lie before 1970, so ts_of_last_over_time
// answers 0 over a window holding one kind and the window's last instant
// over a window holding both.
func TestMixedOrNestedSubquerySelectParity_ChDB(t *testing.T) {
	noLabels := map[string]string{}
	mixed := `(sum(` + beforeEpochHistMetric + `) or sum(` + beforeEpochFloatMetric + `))[10m:1m]`
	count := func(step time.Time) float64 {
		return mixedNestedLastPoint(step).Sub(mixedNestedFirstPoint(step)).Minutes() + 1
	}
	tsOfFirst := func(step time.Time) float64 { return float64(mixedNestedFirstPoint(step).Unix()) }
	windows := []struct {
		name string
		eval time.Time
		// kinds is how many sample kinds every window of this case holds.
		kinds int
		// changes and resets are the window's own counts: the float
		// sequence 3, 5, 5, ... changes once, the histogram sequence
		// changes once when it is in the window from minute 1, and a
		// switch of sample kind is both a change and a reset.
		changes, resets float64
	}{
		{"both_kinds", mixedNestedMinute(5), 2, 3, 1},
		{"histogram_only", mixedNestedMinute(13), 1, 0, 0},
		{"float_only", mixedNestedMinute(0), 1, 1, 0},
	}
	for _, w := range windows {
		tsOfLast := idConst(0)
		if w.kinds == 2 {
			tsOfLast = idStepSeconds
		}
		cases := []struct {
			fn    string
			value func(step time.Time) float64
		}{
			{"count_over_time", count},
			{"present_over_time", idConst(1)},
			{"ts_of_first_over_time", tsOfFirst},
			{"ts_of_last_over_time", tsOfLast},
			{"resets", idConst(w.resets)},
			{"changes", idConst(w.changes)},
		}
		for _, tc := range cases {
			query := fmt.Sprintf(`max_over_time(%s(%s)[1m:1m])`, tc.fn, mixed)
			rows := []instantDomainRow{idRow(noLabels, tc.value)}
			t.Run(w.name+"/"+tc.fn+"/instant", func(t *testing.T) {
				runHandWrittenParity(t, mixedNestedSeed, query, w.eval, rows, false)
			})
			t.Run(w.name+"/"+tc.fn+"/range", func(t *testing.T) {
				runHandWrittenParity(t, mixedNestedSeed, query, w.eval, rows, true)
			})
		}
	}
}
