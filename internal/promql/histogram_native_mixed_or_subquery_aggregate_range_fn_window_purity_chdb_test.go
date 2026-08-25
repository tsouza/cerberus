//go:build chdb

// chDB-backed proof of the window-purity collision-DROP semantics
// histogram_native_mixed_or_subquery_aggregate_range_fn.go's own doc
// derives from reference Prometheus's extrapolatedRate /
// funcSumOverTime / funcAvgOverTime (tsouza/prometheus's own
// promql/functions.go): a `sum by (series) (...)` output group whose
// window holds a histogram-typed sample at ONE subquery anchor and a
// float-typed sample at ANOTHER — never colliding at the SAME anchor, so
// the EXISTING per-anchor [combineMixedAggregateBranches] drop never
// fires — must still be dropped ENTIRELY by a window-purity-filtered
// FOLD-family function (sum_over_time here), while a type-blind function
// (count_over_time) keeps counting both sightings.
//
// Reproducing that shape cleanly needs the histogram sample and the float
// sample separated by MORE than the default five-minute staleness lookback
// ([subqueryStalenessLookback] / [instantLookback], modifiers.go) — samples
// closer together than that would have one carry FORWARD into the other's
// own subquery anchor, producing a genuine SAME-anchor collision the
// existing per-anchor mechanism would already resolve (by keeping the
// mixed `or`'s syntactic-LHS side and shadowing the other, per the `or`
// shadow rule cerberus issue #2337 already ships) — a different, already-
// covered shape, not this file's own window-level one.
//
// series "x" seeds a histogram sample at 00:01 and a gauge sample at
// 00:11 (a ten-minute gap) inside a thirteen-minute subquery window
// (00:01..00:13, evaluated at 00:13): the histogram sample is visible via
// carry-forward at anchors 00:01-00:05 (it expires once an anchor is five
// minutes past its own timestamp: `ts > anchor - 5m` fails from 00:06
// onward) and the gauge sample at anchors 00:11-00:13 (capped by the
// window's own end) — five histogram sightings, three float sightings,
// zero anchors where both are present at once.
package promql_test

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/schema"
)

const (
	sumAvgPurityExpHistMetric = "sav_purity_exp_hist"
	sumAvgPurityGaugeMetric   = "sav_purity_gauge"
)

var sumAvgPuritySeed = subqHistDDL +
	"INSERT INTO otel_metrics_exponential_histogram " +
	"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
	"    ('" + sumAvgPurityExpHistMetric + "', map('series', 'x'), toDateTime64('2026-01-01 00:01:00', 9), 1, 1.0, 0, 0, 0, [1], 0, []);\n" +
	swapGaugeSeedDDL +
	"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
	"    ('" + sumAvgPurityGaugeMetric + "', map('series', 'x'), toDateTime64('2026-01-01 00:11:00', 9), 99.0);\n"

var sumAvgPurityEvalTS = time.Date(2026, 1, 1, 0, 13, 0, 0, time.UTC)

func sumAvgPurityQuery(fn string) string {
	inner := "sum by (series) ((" + sumAvgPurityExpHistMetric + ") or (" + sumAvgPurityGaugeMetric + "))"
	return fn + "((" + inner + ")[13m:1m])"
}

// TestSumOrAvgMixedOrSubqueryWindowPurity_CountOverTime_ChDB proves
// count_over_time — type-blind, no window-purity filtering — reports
// EVERY sighting of series "x" across the window, summing the five
// histogram-typed anchors and the three float-typed ones: reference's own
// funcCountOverTime never inspects sample type.
func TestSumOrAvgMixedOrSubqueryWindowPurity_CountOverTime_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, sumAvgPuritySeed)
	s := schema.DefaultOTelMetrics()

	sqlStr, args := lowerAndEmit(t, sumAvgPurityQuery("count_over_time"), s, sumAvgPurityEvalTS)
	got := sampleValueRows(t, fixture, sqlStr, args)
	if got["x"] != 8 {
		t.Errorf("count_over_time: series x = %v, want 8 (5 histogram-typed sightings + 3 float-typed, type-blind)", got["x"])
	}
}

// TestSumOrAvgMixedOrSubqueryWindowPurity_SumOverTime_ChDB proves
// sum_over_time — a window-purity-filtered FOLD-family member — DROPS
// series "x" entirely: this is the collision-drop semantics PR
// #2592/#2610 found the naive per-arm-distribute approach could not
// reproduce, now verified against real ClickHouse execution for a group
// whose two types never collide at the same subquery anchor.
func TestSumOrAvgMixedOrSubqueryWindowPurity_SumOverTime_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, sumAvgPuritySeed)
	s := schema.DefaultOTelMetrics()

	sqlStr, args := lowerAndEmit(t, sumAvgPurityQuery("sum_over_time"), s, sumAvgPurityEvalTS)

	histRows := subqHistQueryRows(t, fixture, sqlStr, args)
	for _, r := range histRows {
		if r.series == "x" {
			t.Errorf("sum_over_time: series x has a histogram-arm row %+v, want it DROPPED (window holds both a histogram and a float sample)", r)
		}
	}

	got := sampleValueRows(t, fixture, sqlStr, args)
	if _, present := got["x"]; present {
		t.Errorf("sum_over_time: series x has a float-arm row (value %v), want it DROPPED (window holds both a histogram and a float sample)", got["x"])
	}
}

// TestSumOrAvgMixedOrSubqueryWindowPurity_Rate_ChDB is the identical
// window-purity drop proof for rate, the issue's own second FOLD-family
// evidence shape.
func TestSumOrAvgMixedOrSubqueryWindowPurity_Rate_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, sumAvgPuritySeed)
	s := schema.DefaultOTelMetrics()

	sqlStr, args := lowerAndEmit(t, sumAvgPurityQuery("rate"), s, sumAvgPurityEvalTS)

	histRows := subqHistQueryRows(t, fixture, sqlStr, args)
	for _, r := range histRows {
		if r.series == "x" {
			t.Errorf("rate: series x has a histogram-arm row %+v, want it DROPPED", r)
		}
	}

	got := sampleValueRows(t, fixture, sqlStr, args)
	if _, present := got["x"]; present {
		t.Errorf("rate: series x has a float-arm row (value %v), want it DROPPED", got["x"])
	}
}
