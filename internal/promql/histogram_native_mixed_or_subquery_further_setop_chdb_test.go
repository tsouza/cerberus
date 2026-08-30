//go:build chdb

// chDB-backed proof of cerberus issue #2724's own fix: a SELECT/FOLD-family
// outer function over a subquery whose own inner is a further
// `and`/`unless`/`or` wrapping a mixed float/histogram `or` actually
// executes correctly against real ClickHouse — not merely that it lowers
// without error.
//
// Series "h1"/"h2" are the mixed `or`'s histogram arm (metric
// fsop_exp_hist); "f1"/"f2" are its float arm (metric fsop_gauge). Metric
// fsop_c seeds ONLY "h1" and "f1", so `and (fsop_c)` must keep h1/f1 and
// drop h2/f2, and `unless (fsop_c)` must do the exact opposite — proving
// the filter genuinely filters rather than passing every row through
// regardless of type. Metric fsop_d seeds a disjoint series "d1", so a
// further `or (fsop_d)` proves the union adds it in without disturbing
// either arm.
package promql_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/schema"
)

const (
	fsopExpHistMetric = "fsop_exp_hist"
	fsopGaugeMetric   = "fsop_gauge"
	fsopCMetric       = "fsop_c"
	fsopDMetric       = "fsop_d"
)

var fsopSeed = subqHistDDL +
	"INSERT INTO otel_metrics_exponential_histogram " +
	"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
	"    ('" + fsopExpHistMetric + "', map('series', 'h1'), toDateTime64('2026-01-01 00:01:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []),\n" +
	"    ('" + fsopExpHistMetric + "', map('series', 'h2'), toDateTime64('2026-01-01 00:01:00', 9), 9, 9.0, 0, 0, 0, [9], 0, []);\n" +
	subqSetOpGaugeDDL +
	"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
	"    ('" + fsopGaugeMetric + "', map('series', 'f1'), toDateTime64('2026-01-01 00:01:00', 9), 42.0),\n" +
	"    ('" + fsopGaugeMetric + "', map('series', 'f2'), toDateTime64('2026-01-01 00:01:00', 9), 99.0),\n" +
	"    ('" + fsopCMetric + "', map('series', 'h1'), toDateTime64('2026-01-01 00:01:00', 9), 1.0),\n" +
	"    ('" + fsopCMetric + "', map('series', 'f1'), toDateTime64('2026-01-01 00:01:00', 9), 1.0),\n" +
	"    ('" + fsopDMetric + "', map('series', 'd1'), toDateTime64('2026-01-01 00:01:00', 9), 7.0);\n"

var fsopEvalTS = time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)

func fsopMixedOr() string {
	return "((" + fsopExpHistMetric + ") or (" + fsopGaugeMetric + "))"
}

// TestFurtherSetOpAnd_ChDB proves `last_over_time((((h) or (f)) and on(series)
// (c))[1m:1m])` keeps series h1 (histogram, real payload intact) and f1
// (float, real value intact) — both present in fsop_c — while dropping h2
// and f2, neither of which fsop_c seeds.
func TestFurtherSetOpAnd_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, fsopSeed)
	s := schema.DefaultOTelMetrics()

	query := "last_over_time((" + fsopMixedOr() + " and on(series) (" + fsopCMetric + "))[1m:1m])"
	sqlStr, args := lowerAndEmit(t, query, s, fsopEvalTS)

	got := sampleValueRows(t, fixture, sqlStr, args)
	if got["f1"] != 42 {
		t.Errorf("and: series f1 = %v, want 42 (matched fsop_c, float arm survives with its real value)", got["f1"])
	}
	if _, present := got["f2"]; present {
		t.Errorf("and: series f2 present with value %v, want DROPPED (not seeded in fsop_c)", got["f2"])
	}

	histRows := subqHistQueryRows(t, fixture, sqlStr, args)
	h1, ok := findSubqHistRow(histRows, "h1")
	if !ok || h1.cnt != 2 || h1.sum != 4.0 || h1.bucket1 != 6 {
		t.Errorf("and: series h1 = %+v (found=%v), want Count=2 Sum=4 Bucket1=6 (matched fsop_c, histogram arm survives with its real payload)", h1, ok)
	}
	for _, r := range histRows {
		if r.series == "h2" && (r.cnt != 0 || r.sum != 0) {
			t.Errorf("and: series h2 has real histogram fields %+v, want DROPPED (not seeded in fsop_c)", r)
		}
	}
}

// TestFurtherSetOpUnless_ChDB proves `unless` is the exact mirror image of
// TestFurtherSetOpAnd_ChDB's own and: it keeps h2/f2 (absent from fsop_c)
// and drops h1/f1 (present in fsop_c).
func TestFurtherSetOpUnless_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, fsopSeed)
	s := schema.DefaultOTelMetrics()

	query := "last_over_time((" + fsopMixedOr() + " unless on(series) (" + fsopCMetric + "))[1m:1m])"
	sqlStr, args := lowerAndEmit(t, query, s, fsopEvalTS)

	got := sampleValueRows(t, fixture, sqlStr, args)
	if got["f2"] != 99 {
		t.Errorf("unless: series f2 = %v, want 99 (NOT matched in fsop_c, float arm survives)", got["f2"])
	}
	if _, present := got["f1"]; present {
		t.Errorf("unless: series f1 present with value %v, want DROPPED (matched fsop_c)", got["f1"])
	}

	histRows := subqHistQueryRows(t, fixture, sqlStr, args)
	h2, ok := findSubqHistRow(histRows, "h2")
	if !ok || h2.cnt != 9 || h2.sum != 9.0 || h2.bucket1 != 9 {
		t.Errorf("unless: series h2 = %+v (found=%v), want Count=9 Sum=9 Bucket1=9 (NOT matched in fsop_c, histogram arm survives)", h2, ok)
	}
	for _, r := range histRows {
		if r.series == "h1" && (r.cnt != 0 || r.sum != 0) {
			t.Errorf("unless: series h1 has real histogram fields %+v, want DROPPED (matched fsop_c)", r)
		}
	}
}

// TestFurtherSetOpOr_ChDB proves a further `or` unions in a disjoint third
// operand (fsop_d's own series "d1") without disturbing either arm of the
// mixed `or` itself: all four original series (h1, h2, f1, f2) plus d1
// survive.
func TestFurtherSetOpOr_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, fsopSeed)
	s := schema.DefaultOTelMetrics()

	query := "last_over_time((" + fsopMixedOr() + " or (" + fsopDMetric + "))[1m:1m])"
	sqlStr, args := lowerAndEmit(t, query, s, fsopEvalTS)

	got := sampleValueRows(t, fixture, sqlStr, args)
	if got["f1"] != 42 {
		t.Errorf("or: series f1 = %v, want 42", got["f1"])
	}
	if got["f2"] != 99 {
		t.Errorf("or: series f2 = %v, want 99", got["f2"])
	}
	if got["d1"] != 7 {
		t.Errorf("or: series d1 = %v, want 7 (the further or's own operand)", got["d1"])
	}

	histRows := subqHistQueryRows(t, fixture, sqlStr, args)
	h1, ok1 := findSubqHistRow(histRows, "h1")
	if !ok1 || h1.cnt != 2 {
		t.Errorf("or: series h1 = %+v (found=%v), want Count=2", h1, ok1)
	}
	h2, ok2 := findSubqHistRow(histRows, "h2")
	if !ok2 || h2.cnt != 9 {
		t.Errorf("or: series h2 = %+v (found=%v), want Count=9", h2, ok2)
	}
}

// TestFurtherSetOpAnd_FoldFamily_ChDB proves the FOLD-family's own split-
// and-recombine machinery ([lowerFurtherWrapMixedOrSubqueryFoldFn]) folds
// correctly, not just selects: sum_over_time over the SAME and-filtered
// shape sums each arm's real payload rather than forwarding a bare sample.
func TestFurtherSetOpAnd_FoldFamily_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, fsopSeed)
	s := schema.DefaultOTelMetrics()

	query := "sum_over_time((" + fsopMixedOr() + " and on(series) (" + fsopCMetric + "))[1m:1m])"
	sqlStr, args := lowerAndEmit(t, query, s, fsopEvalTS)

	got := sampleValueRows(t, fixture, sqlStr, args)
	if got["f1"] != 42 {
		t.Errorf("sum_over_time and: series f1 = %v, want 42 (single sample, sum_over_time of one point)", got["f1"])
	}

	histRows := subqHistQueryRows(t, fixture, sqlStr, args)
	h1, ok := findSubqHistRow(histRows, "h1")
	if !ok || h1.cnt != 2 || h1.sum != 4.0 || h1.bucket1 != 6 {
		t.Errorf("sum_over_time and: series h1 = %+v (found=%v), want Count=2 Sum=4 Bucket1=6", h1, ok)
	}
}

// TestFurtherSetOpAnd_TrueFanout_ChDB proves the FOLD family's true
// query_range fan-out mode (cerberus issue #2724's own
// [lowerFloatFoldOverSubqueryInput] / [lowerExpHistogramRangeFnOverSubqueryInput]
// wiring) evaluates each output step's own window independently: series f1
// gets a SECOND gauge sample at 00:06, so sum_over_time's own two output
// anchors (00:01 and 00:06, 5 minutes apart, matching the [1m:1m] subquery's
// own tiny range) see different totals.
func TestFurtherSetOpAnd_TrueFanout_ChDB(t *testing.T) {
	// fsop_c's own series f1 must stay freshly visible at BOTH output
	// anchors (default staleness is a strict "ts > anchor - 5m" bound, and
	// 00:06 minus 5m lands exactly on 00:01 — right at the exclusive edge),
	// so it gets a second sample at 00:06 alongside the second gauge one.
	seed := fsopSeed +
		"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
		"    ('" + fsopGaugeMetric + "', map('series', 'f1'), toDateTime64('2026-01-01 00:06:00', 9), 55.0),\n" +
		"    ('" + fsopCMetric + "', map('series', 'f1'), toDateTime64('2026-01-01 00:06:00', 9), 1.0);\n"
	fixture := newChDBFixture(t, seed)
	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)
	end := time.Date(2026, 1, 1, 0, 6, 0, 0, time.UTC)

	query := "sum_over_time((" + fsopMixedOr() + " and on(series) (" + fsopCMetric + "))[1m:1m])"
	sqlStr, args := lowerAndEmitRange(t, query, s, start, end, 5*time.Minute)

	rows := rangeSampleValueRows(t, fixture, sqlStr, args)
	if got := rows["f1"][start.Unix()]; got != 42 {
		t.Errorf("sum_over_time and @ %v: series f1 = %v, want 42 (only the 00:01 sample is in this anchor's own [1m] window)", start, got)
	}
	if got := rows["f1"][end.Unix()]; got != 55 {
		t.Errorf("sum_over_time and @ %v: series f1 = %v, want 55 (only the 00:06 sample is in this anchor's own [1m] window)", end, got)
	}
}

// TestFurtherSetOpMixedOr_PinnedBroadcast_ChDB pins the `@`-pinned
// query_range shape of the very same FOLD-family lowering:
// [lowerFloatFoldOverSubqueryInput]'s pinned branch broadcasts its
// single-window reduction across the step grid through a StepGrid CROSS
// JOIN, and the recombination that unions it with the histogram half has
// to see that broadcast as a DERIVED shape and synthesise MetricName
// rather than reference it.
//
// It did not: [chplan.IsDerivedShape] had no CrossJoin arm, so the
// broadcast Project fell through to its "canonical" default and the
// emitted union named a `MetricName` column that scope has none of —
// ClickHouse code 47 (UNKNOWN_IDENTIFIER), for every query of this shape.
// The chdb-tagged coverage this file already had never reached it because
// every other case here is instant or unpinned.
func TestFurtherSetOpMixedOr_PinnedBroadcast_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, fsopSeed)
	s := schema.DefaultOTelMetrics()

	// A query_range window 999 hours past every seeded sample, with the
	// real eval instant pinned: every output step must reduce the pinned
	// window and report the same per-series answer.
	wrongStart := fsopEvalTS.Add(999 * time.Hour)
	wrongEnd := wrongStart.Add(2 * time.Minute)
	query := "sum_over_time(" + fsopMixedOr() + "[1m:1m] @ " + strconv.FormatInt(fsopEvalTS.Unix(), 10) + ")"
	sqlStr, args := lowerAndEmitRange(t, query, s, wrongStart, wrongEnd, time.Minute)

	rows := rangeSampleValueRows(t, fixture, sqlStr, args)
	// [wrongStart, wrongEnd] at 1m spacing, end-inclusive.
	wantSteps := int(wrongEnd.Sub(wrongStart)/time.Minute) + 1
	for series, want := range map[string]float64{"f1": 42, "f2": 99} {
		if got := len(rows[series]); got != wantSteps {
			t.Errorf("series %s landed on %d steps, want %d (the pinned answer broadcast across the whole grid)", series, got, wantSteps)
		}
		for ts, got := range rows[series] {
			if got != want {
				t.Errorf("series %s step %v = %v, want %v (the pinned window's own sum, not the ambient window's)", series, time.Unix(ts, 0).UTC(), got, want)
			}
		}
	}

	histRows := subqHistQueryRows(t, fixture, sqlStr, args)
	histSteps := map[int64]bool{}
	for _, r := range histRows {
		if r.series != "h1" {
			continue
		}
		histSteps[r.ts] = true
		if r.cnt != 2 || r.sum != 4 {
			t.Errorf("series h1 step %v: Count=%v Sum=%v, want 2/4 (the pinned window's own single histogram sample)", time.Unix(r.ts, 0).UTC(), r.cnt, r.sum)
		}
	}
	if len(histSteps) != wantSteps {
		t.Errorf("series h1 landed on %d steps, want %d", len(histSteps), wantSteps)
	}
}
