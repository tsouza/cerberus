//go:build chdb

// chDB-backed proof that `last_over_time`/`first_over_time` over a
// sum/avg-wrapped mixed float/histogram `or` subquery — cerberus issue
// #2714, histogram_native_mixed_or_subquery_last_first.go — actually picks
// the newest/oldest in-window sample VERBATIM, whichever type it happens to
// be, at real ClickHouse execution.
//
// Two series prove the selection is genuinely type-aware rather than
// favouring one arm:
//
//	series "hf": histogram at 00:00 (Count=2 Sum=4 Bucket1=6), float at
//	  00:06 (Value=42) — the LATER sample is the float one.
//	series "fh": float at 00:00 (Value=7), histogram at 00:06 (Count=5
//	  Sum=11 Bucket1=9) — the LATER sample is the histogram one.
//
// So last_over_time picks the float for "hf" and the histogram for "fh";
// first_over_time picks the opposite for each — proving the argMax/argMin
// selection keys on timestamp, not on which arm of the `or` a row came
// from.
package promql_test

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/schema"
)

const (
	mlfMixedExpHistMetric = "mlf_mixed_exp_hist"
	mlfMixedGaugeMetric   = "mlf_mixed_gauge"
)

var mlfMixedOrSeed = subqHistDDL +
	"INSERT INTO otel_metrics_exponential_histogram " +
	"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
	"    ('" + mlfMixedExpHistMetric + "', map('series', 'hf'), toDateTime64('2026-01-01 00:00:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []),\n" +
	"    ('" + mlfMixedExpHistMetric + "', map('series', 'fh'), toDateTime64('2026-01-01 00:06:00', 9), 5, 11.0, 0, 0, 0, [9], 0, []);\n" +
	swapGaugeSeedDDL +
	"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
	"    ('" + mlfMixedGaugeMetric + "', map('series', 'hf'), toDateTime64('2026-01-01 00:06:00', 9), 42.0),\n" +
	"    ('" + mlfMixedGaugeMetric + "', map('series', 'fh'), toDateTime64('2026-01-01 00:00:00', 9), 7.0);\n"

var mlfMixedOrEvalTS = time.Date(2026, 1, 1, 0, 6, 0, 0, time.UTC)

func mlfMixedOrQuery(fn string) string {
	inner := "sum by (series) ((" + mlfMixedExpHistMetric + ") or (" + mlfMixedGaugeMetric + "))"
	return fn + "((" + inner + ")[7m:6m])"
}

// TestMixedOrSubqueryLastOverTime_Instant_ChDB proves last_over_time picks
// the LATER sample for both series — the float for "hf", the histogram for
// "fh".
func TestMixedOrSubqueryLastOverTime_Instant_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, mlfMixedOrSeed)
	s := schema.DefaultOTelMetrics()

	sqlStr, args := lowerAndEmit(t, mlfMixedOrQuery("last_over_time"), s, mlfMixedOrEvalTS)

	got := sampleValueRows(t, fixture, sqlStr, args)
	if got["hf"] != 42 {
		t.Errorf("last_over_time: series hf (float arm) = %v, want 42 (the later, float sample)", got["hf"])
	}

	histRows := subqHistQueryRows(t, fixture, sqlStr, args)
	fhRow, ok := findSubqHistRow(histRows, "fh")
	if !ok {
		t.Fatalf("last_over_time: no histogram-arm row for series fh; got %+v", histRows)
	}
	if fhRow.cnt != 5 || fhRow.sum != 11.0 || fhRow.bucket1 != 9 {
		t.Errorf("last_over_time: series fh (histogram arm) = %+v, want Count=5 Sum=11 Bucket1=9 (the later, histogram sample)", fhRow)
	}
}

// TestMixedOrSubqueryFirstOverTime_Instant_ChDB proves first_over_time
// picks the EARLIER sample for both series — the mirror image of
// last_over_time's own answer.
func TestMixedOrSubqueryFirstOverTime_Instant_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, mlfMixedOrSeed)
	s := schema.DefaultOTelMetrics()

	sqlStr, args := lowerAndEmit(t, mlfMixedOrQuery("first_over_time"), s, mlfMixedOrEvalTS)

	got := sampleValueRows(t, fixture, sqlStr, args)
	if got["fh"] != 7 {
		t.Errorf("first_over_time: series fh (float arm) = %v, want 7 (the earlier, float sample)", got["fh"])
	}

	histRows := subqHistQueryRows(t, fixture, sqlStr, args)
	hfRow, ok := findSubqHistRow(histRows, "hf")
	if !ok {
		t.Fatalf("first_over_time: no histogram-arm row for series hf; got %+v", histRows)
	}
	if hfRow.cnt != 2 || hfRow.sum != 4.0 || hfRow.bucket1 != 6 {
		t.Errorf("first_over_time: series hf (histogram arm) = %+v, want Count=2 Sum=4 Bucket1=6 (the earlier, histogram sample)", hfRow)
	}
}

// TestMixedOrSubqueryLastOverTime_PinnedBroadcast_ChDB proves the
// `@`-pinned query_range broadcast mode reports the identical instant-mode
// answer at every output step.
func TestMixedOrSubqueryLastOverTime_PinnedBroadcast_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, mlfMixedOrSeed)
	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 6, 0, 0, time.UTC)
	end := time.Date(2026, 1, 1, 0, 12, 0, 0, time.UTC)

	inner := "sum by (series) ((" + mlfMixedExpHistMetric + ") or (" + mlfMixedGaugeMetric + "))"
	query := "last_over_time((" + inner + ")[7m:6m] @ end())"

	sqlStr, args := lowerAndEmitRange(t, query, s, start, end, 6*time.Minute)
	rows := rangeSampleValueRows(t, fixture, sqlStr, args)
	for _, ts := range []time.Time{start, end} {
		if got := rows["hf"][ts.Unix()]; got != 42 {
			t.Errorf("last_over_time @ %v: series hf = %v, want 42 (broadcast of the instant answer, pinned to the query's own end=%v)", ts, got, end)
		}
	}
}

// TestMixedOrSubqueryLastOverTime_TrueFanout_ChDB proves the true
// query_range fan-out mode evaluates each output step's own [7m] window
// independently, using series "hf" (histogram at 00:00, float at 00:06):
// at output anchor 00:00 the window (2026-01-01 23:53:00 (prev day),
// 2026-01-01 00:00:00] holds only the 00:00 histogram sample, so
// last_over_time answers the HISTOGRAM; six minutes later, at output
// anchor 00:06, the window (2026-01-01 00:00:00, 2026-01-01 00:06:00] now
// ALSO holds the later float sample, so last_over_time flips to the
// FLOAT — the SAME series answering with a DIFFERENT type at a
// DIFFERENT anchor, which only a genuine per-anchor fan-out (not a
// broadcast of one shared answer) can produce.
func TestMixedOrSubqueryLastOverTime_TrueFanout_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, mlfMixedOrSeed)
	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 1, 1, 0, 6, 0, 0, time.UTC)

	sqlStr, args := lowerAndEmitRange(t, mlfMixedOrQuery("last_over_time"), s, start, end, 6*time.Minute)

	// This composer always emits exactly one row per (series, anchor) —
	// whichever type won — so both sampleValueRows and subqHistQueryRows
	// see every (series, anchor) pair regardless of which side is real;
	// [chplan.MixedDiscriminatorColumn]'s own placeholder convention
	// ([histogramSampleValuePlaceholder] on the losing histogram side, an
	// all-zero/empty shape on the losing float side) is what makes reading
	// BOTH projections at BOTH anchors a complete, unambiguous proof of
	// which type won where.
	got := rangeSampleValueRows(t, fixture, sqlStr, args)
	if got := got["hf"][start.Unix()]; got != 0 {
		t.Errorf("last_over_time @ %v: series hf Value = %v, want the placeholder 0 (histogram won this anchor)", start, got)
	}
	if got := got["hf"][end.Unix()]; got != 42 {
		t.Errorf("last_over_time @ %v: series hf Value = %v, want 42 (the later, float sample, now inside the window)", end, got)
	}

	histRows := subqHistQueryRows(t, fixture, sqlStr, args)
	startRow, ok := subqHistRowAtOptional(histRows, "hf", start)
	if !ok {
		t.Fatalf("last_over_time @ %v: no row for series hf; got %+v", start, histRows)
	}
	if startRow.cnt != 2 || startRow.sum != 4.0 || startRow.bucket1 != 6 {
		t.Errorf("last_over_time @ %v: series hf = %+v, want Count=2 Sum=4 Bucket1=6 (the earlier, histogram sample — the only one in this anchor's own window)", start, startRow)
	}
	endRow, ok := subqHistRowAtOptional(histRows, "hf", end)
	if !ok {
		t.Fatalf("last_over_time @ %v: no row for series hf; got %+v", end, histRows)
	}
	if endRow.cnt != 0 || endRow.sum != 0 {
		t.Errorf("last_over_time @ %v: series hf histogram fields = %+v, want the placeholder Count=0 Sum=0 (the float arm won this anchor)", end, endRow)
	}
}

func findSubqHistRow(rows []subqHistRow, series string) (subqHistRow, bool) {
	for _, r := range rows {
		if r.series == series {
			return r, true
		}
	}
	return subqHistRow{}, false
}

func subqHistRowAtOptional(rows []subqHistRow, series string, ts time.Time) (subqHistRow, bool) {
	for _, r := range rows {
		if r.series == series && r.ts == ts.Unix() {
			return r, true
		}
	}
	return subqHistRow{}, false
}
