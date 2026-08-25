//go:build chdb

// chDB-backed proof that cerberus issue #2581's `sum`/`avg`-wrapped mixed
// float/histogram `or` subquery composition
// (histogram_native_mixed_or_subquery_aggregate_range_fn.go) actually
// executes correctly against real ClickHouse for the ORDINARY, no-collision
// case: a histogram-only group ("h") and a float-only group ("f") both fold
// correctly through the new eleven-name composition. The window-purity
// collision-DROP semantics get their own dedicated fixture and doc —
// histogram_native_mixed_or_subquery_aggregate_range_fn_window_purity_chdb_test.go
// — because reproducing it cleanly needs samples spaced beyond the default
// 5-minute staleness lookback (subqueryStalenessLookback), which would
// otherwise let one series' carried-forward sample smear across several
// subquery anchors and make this file's own h/f arithmetic much harder to
// hand-verify.
package promql_test

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/schema"
)

const (
	sumAvgMixedExpHistMetric = "sav_mor_exp_hist"
	sumAvgMixedGaugeMetric   = "sav_mor_gauge"
)

// sumAvgMixedOrSeed backs every case in this file: series "h" gets the
// same two-sample histogram history subqSelectHistFixture's own baseline
// uses; series "f" gets two plain gauge samples at the same two anchors on
// a disjoint metric, so the mixed `or` never has to resolve a same-series
// shadow.
var sumAvgMixedOrSeed = subqHistDDL +
	"INSERT INTO otel_metrics_exponential_histogram " +
	"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
	"    ('" + sumAvgMixedExpHistMetric + "', map('series', 'h'), toDateTime64('2026-01-01 00:01:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []),\n" +
	"    ('" + sumAvgMixedExpHistMetric + "', map('series', 'h'), toDateTime64('2026-01-01 00:02:00', 9), 3, 9.0, 0, 0, 0, [12], 0, []);\n" +
	swapGaugeSeedDDL +
	"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
	"    ('" + sumAvgMixedGaugeMetric + "', map('series', 'f'), toDateTime64('2026-01-01 00:01:00', 9), 10.0),\n" +
	"    ('" + sumAvgMixedGaugeMetric + "', map('series', 'f'), toDateTime64('2026-01-01 00:02:00', 9), 20.0);\n"

var sumAvgMixedOrEvalTS = time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC)

// sumAvgMixedOrQuery builds `<fn>((sum by (series) ((h) or (f)))[2m:1m])`
// — the issue's own trigger shape, with this file's own metric names and
// window.
func sumAvgMixedOrQuery(fn string) string {
	inner := "sum by (series) ((" + sumAvgMixedExpHistMetric + ") or (" + sumAvgMixedGaugeMetric + "))"
	return fn + "((" + inner + ")[2m:1m])"
}

// TestSumOrAvgMixedOrSubqueryCountOverTime_ChDB proves count_over_time
// composes over the sum/avg-wrapped mixed-or subquery for both the
// histogram-only and float-only groups.
func TestSumOrAvgMixedOrSubqueryCountOverTime_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, sumAvgMixedOrSeed)
	s := schema.DefaultOTelMetrics()

	sqlStr, args := lowerAndEmit(t, sumAvgMixedOrQuery("count_over_time"), s, sumAvgMixedOrEvalTS)
	got := sampleValueRows(t, fixture, sqlStr, args)
	if got["h"] != 2 {
		t.Errorf("count_over_time: series h = %v, want 2", got["h"])
	}
	if got["f"] != 2 {
		t.Errorf("count_over_time: series f = %v, want 2", got["f"])
	}
}

// TestSumOrAvgMixedOrSubquerySumOverTime_ChDB proves sum_over_time — a
// window-purity-filtered FOLD-family member — folds the histogram-only
// group ("h") through the exact same arithmetic
// subquery_select_histogram_chdb_test.go's own pure-histogram baseline
// pins (Count=5, Sum=13, Bucket1=18 — 2+3, 4+9, 6+12) and the float-only
// group ("f") through an ordinary float sum (10+20=30) — the ordinary,
// no-collision path.
func TestSumOrAvgMixedOrSubquerySumOverTime_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, sumAvgMixedOrSeed)
	s := schema.DefaultOTelMetrics()

	sqlStr, args := lowerAndEmit(t, sumAvgMixedOrQuery("sum_over_time"), s, sumAvgMixedOrEvalTS)

	histRows := subqHistQueryRows(t, fixture, sqlStr, args)
	var gotHist bool
	for _, r := range histRows {
		if r.series == "h" {
			gotHist = true
			if r.cnt != 5 || r.sum != 13.0 || r.bucket1 != 18 {
				t.Errorf("sum_over_time, histogram arm series h = %+v, want Count=5 Sum=13 Bucket1=18 (2+3, 4+9, 6+12)", r)
			}
		}
	}
	if !gotHist {
		t.Fatalf("sum_over_time: no histogram-arm row for series h; got %+v", histRows)
	}

	got := sampleValueRows(t, fixture, sqlStr, args)
	if got["f"] != 30 {
		t.Errorf("sum_over_time, float arm series f = %v, want 30 (10+20)", got["f"])
	}
}

// TestSumOrAvgMixedOrSubqueryRate_ChDB proves rate — the issue's own
// second FOLD-family evidence shape — folds "h"/"f" to a positive rate.
func TestSumOrAvgMixedOrSubqueryRate_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, sumAvgMixedOrSeed)
	s := schema.DefaultOTelMetrics()

	sqlStr, args := lowerAndEmit(t, sumAvgMixedOrQuery("rate"), s, sumAvgMixedOrEvalTS)

	histRows := subqHistQueryRows(t, fixture, sqlStr, args)
	var gotHist bool
	for _, r := range histRows {
		if r.series == "h" {
			gotHist = true
			if r.cnt <= 0 {
				t.Errorf("rate, histogram arm series h = %+v, want a positive Count", r)
			}
		}
	}
	if !gotHist {
		t.Fatalf("rate: no histogram-arm row for series h; got %+v", histRows)
	}

	got := sampleValueRows(t, fixture, sqlStr, args)
	if got["f"] <= 0 {
		t.Errorf("rate, float arm series f = %v, want a positive rate", got["f"])
	}
}

// TestSumOrAvgMixedOrSubqueryTsOfLastOverTime_ChDB proves
// ts_of_last_over_time — a type-blind name that reads no per-sample value
// at all — reports the correct latest-sample timestamp for both groups.
func TestSumOrAvgMixedOrSubqueryTsOfLastOverTime_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, sumAvgMixedOrSeed)
	s := schema.DefaultOTelMetrics()

	sqlStr, args := lowerAndEmit(t, sumAvgMixedOrQuery("ts_of_last_over_time"), s, sumAvgMixedOrEvalTS)
	got := sampleValueRows(t, fixture, sqlStr, args)

	want := float64(time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC).Unix())
	if got["h"] != want {
		t.Errorf("ts_of_last_over_time: series h = %v, want %v (00:02:00)", got["h"], want)
	}
	if got["f"] != want {
		t.Errorf("ts_of_last_over_time: series f = %v, want %v (00:02:00)", got["f"], want)
	}
}
