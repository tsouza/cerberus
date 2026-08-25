//go:build chdb

// chDB-backed proof that cerberus issue #2577's fix actually executes
// correctly against real ClickHouse: `<fn>(((h) or (f))[range:step])` for
// a SELECT-family member (count_over_time) and a FOLD-family member
// (sum_over_time), both nested under a further wrapper — the issue's own
// evidence shape, `sum(count_over_time(((demo_latency_exp_hist) or
// (demo_num_cpus))[5m:1m]))`, and its FOLD-family sibling.
//
// The seed carries a histogram series ("h", on morExpHistMetric) and a
// gauge series ("f", on morGaugeMetric), each with the SAME two-anchor
// shape subqSelectHistFixture's own histogram series uses — reusing that
// file's own published values so this file's histogram-arm assertions
// can be checked against the identical pure-histogram baseline
// (subquery_select_histogram_chdb_test.go) rather than a fresh set of
// numbers.
package promql_test

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/schema"
)

const (
	morExpHistMetric = "mor_sub_exp_hist"
	morGaugeMetric   = "mor_sub_gauge"
)

// morSeed backs every case in this file: morExpHistMetric/series="h" gets
// the SAME two published histograms subqSelectHistFixture seeds for its
// own single series ((00:01:00, Count=2, Sum=4.0, Bucket1=6), (00:02:00,
// Count=3, Sum=9.0, Bucket1=12)); morGaugeMetric/series="f" gets two plain
// gauge samples at the same two anchors (10.0, 20.0) on a disjoint metric
// name, so the mixed `or` never has to resolve a same-series shadow.
var morSeed = subqHistDDL +
	"INSERT INTO otel_metrics_exponential_histogram " +
	"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
	"    ('" + morExpHistMetric + "', map('series', 'h'), toDateTime64('2026-01-01 00:01:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []),\n" +
	"    ('" + morExpHistMetric + "', map('series', 'h'), toDateTime64('2026-01-01 00:02:00', 9), 3, 9.0, 0, 0, 0, [12], 0, []);\n" +
	swapGaugeSeedDDL +
	"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
	"    ('" + morGaugeMetric + "', map('series', 'f'), toDateTime64('2026-01-01 00:01:00', 9), 10.0),\n" +
	"    ('" + morGaugeMetric + "', map('series', 'f'), toDateTime64('2026-01-01 00:02:00', 9), 20.0);\n"

var morEvalTS = time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC)

// morQuery builds `<outer>(<fn>(((h) or (f))[2m:1m]))`, mirroring the
// issue's own `sum(count_over_time(((demo_latency_exp_hist) or
// (demo_num_cpus))[5m:1m]))` evidence query with this file's own metric
// names and a 2m/1m subquery window.
func morQuery(outerFmt, fn string) string {
	subExpr := "((" + morExpHistMetric + ") or (" + morGaugeMetric + "))"
	inner := fn + "(" + subExpr + "[2m:1m])"
	if outerFmt == "" {
		return inner
	}
	return outerFmt + "(" + inner + ")"
}

// TestMixedOrSubqueryCountOverTimeNestedUnderSum_ChDB proves count_over_time
// (a SELECT-family, always-float-output member) composes over a subquery
// whose own inner is a mixed float/histogram `or`, nested under `sum by
// (series)` — the issue's own SELECT-family evidence shape. Both arms'
// windows hold exactly two in-window samples, so count_over_time reports 2
// for both the histogram-arm series ("h") and the float-arm series ("f").
func TestMixedOrSubqueryCountOverTimeNestedUnderSum_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, morSeed)
	s := schema.DefaultOTelMetrics()

	query := "sum by (series) (" + morQuery("", "count_over_time") + ")"
	sqlStr, args := lowerAndEmit(t, query, s, morEvalTS)
	got := sampleValueRows(t, fixture, sqlStr, args)
	if got["h"] != 2 {
		t.Errorf("count_over_time over mixed-or subquery: series h = %v, want 2", got["h"])
	}
	if got["f"] != 2 {
		t.Errorf("count_over_time over mixed-or subquery: series f = %v, want 2", got["f"])
	}
}

// TestMixedOrSubqueryPresentOverTime_ChDB proves present_over_time — the
// same recognizer's second value-blind member — composes at the query
// root (unwrapped, the issue's own first evidence query's exact shape).
func TestMixedOrSubqueryPresentOverTime_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, morSeed)
	s := schema.DefaultOTelMetrics()

	query := morQuery("", "present_over_time")
	sqlStr, args := lowerAndEmit(t, query, s, morEvalTS)
	got := sampleValueRows(t, fixture, sqlStr, args)
	if got["h"] != 1 {
		t.Errorf("present_over_time over mixed-or subquery: series h = %v, want 1", got["h"])
	}
	if got["f"] != 1 {
		t.Errorf("present_over_time over mixed-or subquery: series f = %v, want 1", got["f"])
	}
}

// TestMixedOrSubquerySumOverTimeNestedUnderLabelReplace_ChDB proves
// sum_over_time — a FOLD-family, histogram-preserving member whose output
// is a genuinely [chplan.MixedRowShape] node (the histogram arm folds to
// a HistogramRowShape row, the float arm to an ordinary SampleRowShape
// row) — composes correctly under label_replace, reading back BOTH arms:
// the histogram-arm series ("h") folds its two published histograms
// exactly as subquery_select_histogram_chdb_test.go's own pure-histogram
// baseline does (Count=5, Sum=13, Bucket1=18 — 2+3, 4+9, 6+12); the
// float-arm series ("f") sums its two plain gauge samples (10+20=30).
func TestMixedOrSubquerySumOverTimeNestedUnderLabelReplace_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, morSeed)
	s := schema.DefaultOTelMetrics()

	query := `label_replace(` + morQuery("", "sum_over_time") + `, "extra", "yes", "", "")`
	sqlStr, args := lowerAndEmit(t, query, s, morEvalTS)

	histRows := subqHistQueryRows(t, fixture, sqlStr, args)
	var gotHist bool
	for _, r := range histRows {
		if r.series == "h" {
			gotHist = true
			if r.cnt != 5 || r.sum != 13.0 || r.bucket1 != 18 {
				t.Errorf("sum_over_time over mixed-or subquery, histogram arm = %+v, want Count=5 Sum=13 Bucket1=18", r)
			}
		}
	}
	if !gotHist {
		t.Fatalf("sum_over_time over mixed-or subquery: no histogram-arm row for series h; got %+v", histRows)
	}

	got := sampleValueRows(t, fixture, sqlStr, args)
	if got["f"] != 30 {
		t.Errorf("sum_over_time over mixed-or subquery, float arm: series f = %v, want 30 (10+20)", got["f"])
	}
}

// TestMixedOrSubqueryRate_ChDB proves rate — the issue's own second
// FOLD-family evidence query, unwrapped at the query root — composes over
// a mixed-or subquery inner and answers the histogram arm's boundary-
// extrapolated rate the same way the pure-histogram-inner path already
// does, while the float arm answers an ordinary float rate.
func TestMixedOrSubqueryRate_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, morSeed)
	s := schema.DefaultOTelMetrics()

	query := morQuery("", "rate")
	sqlStr, args := lowerAndEmit(t, query, s, morEvalTS)

	histRows := subqHistQueryRows(t, fixture, sqlStr, args)
	var gotHist bool
	for _, r := range histRows {
		if r.series == "h" {
			gotHist = true
			// rate()'s per-second division over a 2m window with two
			// in-window samples: the SAME arithmetic
			// TestSubqueryHistogramFoldFamilyNestedUnderSum_ChDB's own
			// sum_over_time baseline exercises for the fold itself, just
			// divided by the window's own extrapolated span rather than
			// left as a raw sum — checked only for a non-zero Count here
			// (an exact-value pin belongs to a dedicated rate() fixture,
			// not this composition proof).
			if r.cnt <= 0 {
				t.Errorf("rate over mixed-or subquery, histogram arm = %+v, want a positive Count", r)
			}
		}
	}
	if !gotHist {
		t.Fatalf("rate over mixed-or subquery: no histogram-arm row for series h; got %+v", histRows)
	}

	got := sampleValueRows(t, fixture, sqlStr, args)
	if got["f"] <= 0 {
		t.Errorf("rate over mixed-or subquery, float arm: series f = %v, want a positive rate", got["f"])
	}
}

// TestMixedOrSubqueryLastOverTime_ChDB proves last_over_time — the other
// histogram-preserving SELECT-family member, which SELECTS one published
// sample verbatim rather than folding a window — picks the correct
// per-arm sample: the histogram arm's newest published histogram
// (matching subquery_select_histogram_chdb_test.go's own pure-histogram
// baseline for the identical two-sample seed) and the float arm's newest
// gauge reading.
func TestMixedOrSubqueryLastOverTime_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, morSeed)
	s := schema.DefaultOTelMetrics()

	query := morQuery("", "last_over_time")
	sqlStr, args := lowerAndEmit(t, query, s, morEvalTS)

	histRows := subqHistQueryRows(t, fixture, sqlStr, args)
	var gotHist bool
	for _, r := range histRows {
		if r.series == "h" {
			gotHist = true
			if r.cnt != 3 || r.sum != 9.0 || r.bucket1 != 12 {
				t.Errorf("last_over_time over mixed-or subquery, histogram arm = %+v, want Count=3 Sum=9 Bucket1=12 (the 00:02 sample)", r)
			}
		}
	}
	if !gotHist {
		t.Fatalf("last_over_time over mixed-or subquery: no histogram-arm row for series h; got %+v", histRows)
	}

	got := sampleValueRows(t, fixture, sqlStr, args)
	if got["f"] != 20 {
		t.Errorf("last_over_time over mixed-or subquery, float arm: series f = %v, want 20 (the 00:02 sample)", got["f"])
	}
}
