//go:build chdb

// chDB-backed proof that the eight SELECT/COUNT-family range-vector
// functions cerberus issue #2545 newly supports over a bare top-level
// histogram-native subquery — count_over_time, present_over_time,
// last_over_time, first_over_time, resets, changes, ts_of_first_over_time,
// ts_of_last_over_time — plus the sum_over_time/avg_over_time FOLD-family
// gap the same issue found and closed (histogram_native_range_fn.go's
// switch simply never named them) actually execute correctly against real
// ClickHouse, not merely that the emitted plan's Go shape looks right.
//
// Every case seeds the SAME two-sample series `subq_select_exp_hist` /
// `series=a`: (00:01:00, Count=2, Sum=4.0, Bucket1=6), (00:02:00, Count=3,
// Sum=9.0, Bucket1=12) — reused from subquery_histogram_native_chdb_test.go's
// own DDL/seed helpers — and evaluates `<fn>((subq_select_exp_hist)[2m:1m])`
// at 00:02:00, so the subquery's own per-anchor grid produces exactly two
// histogram rows (one per real underlying sample, per
// TestSubqueryHistogramBareSelector_ChDB's own baseline) for every
// function's outer reduction to read.
package promql_test

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

// subqSelectHistFixture seeds the shared two-sample series and returns the
// fixture plus the metric name and evaluation instant every case in this
// file reduces over.
func subqSelectHistFixture(t *testing.T) (fixture *chdbFixture, metric string, evalTS time.Time) {
	t.Helper()
	metric = "subq_select_exp_hist"
	seed := subqHistDDL +
		"INSERT INTO otel_metrics_exponential_histogram " +
		"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		"    ('" + metric + "', map('series', 'a'), toDateTime64('2026-01-01 00:01:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []),\n" +
		"    ('" + metric + "', map('series', 'a'), toDateTime64('2026-01-01 00:02:00', 9), 3, 9.0, 0, 0, 0, [12], 0, []);\n"
	return newChDBFixture(t, seed), metric, time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC)
}

// sampleValueRows reads back the canonical float-quartet shape
// (count_over_time / present_over_time / resets / changes /
// ts_of_first_over_time / ts_of_last_over_time all answer, __name__
// dropped) as (series, value) pairs keyed off the `series` Attributes
// entry.
func sampleValueRows(t *testing.T, fixture *chdbFixture, sqlStr string, args []any) map[string]float64 {
	t.Helper()
	rows := fixture.queryOverEmitted(t, "`Attributes`['series'] AS series, `Value` AS val", sqlStr, args)
	defer func() { _ = rows.Close() }()
	out := map[string]float64{}
	for rows.Next() {
		var series string
		var val float64
		if err := rows.Scan(&series, &val); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[series] = val
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

// TestSubqueryHistogramCountPresentOverTime_ChDB proves count_over_time /
// present_over_time over the subquery's two published anchors.
func TestSubqueryHistogramCountPresentOverTime_ChDB(t *testing.T) {
	fixture, metric, evalTS := subqSelectHistFixture(t)
	s := schema.DefaultOTelMetrics()

	sqlStr, args := lowerAndEmit(t, "count_over_time(("+metric+")[2m:1m])", s, evalTS)
	got := sampleValueRows(t, fixture, sqlStr, args)
	if got["a"] != 2 {
		t.Errorf("count_over_time: series a = %v, want 2 (one per subquery anchor)", got["a"])
	}

	sqlStr, args = lowerAndEmit(t, "present_over_time(("+metric+")[2m:1m])", s, evalTS)
	got = sampleValueRows(t, fixture, sqlStr, args)
	if got["a"] != 1 {
		t.Errorf("present_over_time: series a = %v, want 1", got["a"])
	}
}

// TestSubqueryHistogramLastFirstOverTime_ChDB proves last_over_time /
// first_over_time PRESERVE the newest / oldest subquery-anchor histogram
// verbatim, reading back the full (Count, Sum, Bucket1) triple.
func TestSubqueryHistogramLastFirstOverTime_ChDB(t *testing.T) {
	fixture, metric, evalTS := subqSelectHistFixture(t)
	s := schema.DefaultOTelMetrics()

	sqlStr, args := lowerAndEmit(t, "last_over_time(("+metric+")[2m:1m])", s, evalTS)
	rows := subqHistQueryRows(t, fixture, sqlStr, args)
	if len(rows) != 1 {
		t.Fatalf("last_over_time: got %d rows, want 1: %+v", len(rows), rows)
	}
	if got := subqHistRowAt(t, rows, "a", evalTS); got.cnt != 3 || got.sum != 9.0 || got.bucket1 != 12 {
		t.Errorf("last_over_time = %+v, want Count=3 Sum=9 Bucket1=12 (the 00:02 sample)", got)
	}

	sqlStr, args = lowerAndEmit(t, "first_over_time(("+metric+")[2m:1m])", s, evalTS)
	rows = subqHistQueryRows(t, fixture, sqlStr, args)
	if len(rows) != 1 {
		t.Fatalf("first_over_time: got %d rows, want 1: %+v", len(rows), rows)
	}
	if got := subqHistRowAt(t, rows, "a", evalTS); got.cnt != 2 || got.sum != 4.0 || got.bucket1 != 6 {
		t.Errorf("first_over_time = %+v, want Count=2 Sum=4 Bucket1=6 (the 00:01 sample)", got)
	}
}

// TestSubqueryHistogramResetsChanges_ChDB proves resets() finds no counter
// reset across the two (monotonically growing) subquery-anchor histograms,
// while changes() counts the one pair as a genuine value change — the two
// functions' own documented divergence (histogram_native_resets.go): a
// counter GROWING is a changes() hit and a resets() miss.
func TestSubqueryHistogramResetsChanges_ChDB(t *testing.T) {
	fixture, metric, evalTS := subqSelectHistFixture(t)
	s := schema.DefaultOTelMetrics()

	sqlStr, args := lowerAndEmit(t, "resets(("+metric+")[2m:1m])", s, evalTS)
	got := sampleValueRows(t, fixture, sqlStr, args)
	if got["a"] != 0 {
		t.Errorf("resets: series a = %v, want 0 (monotonically growing histogram, no counter reset)", got["a"])
	}

	sqlStr, args = lowerAndEmit(t, "changes(("+metric+")[2m:1m])", s, evalTS)
	got = sampleValueRows(t, fixture, sqlStr, args)
	if got["a"] != 1 {
		t.Errorf("changes: series a = %v, want 1 (the two anchors' histograms differ)", got["a"])
	}
}

// TestSubqueryHistogramTsOfFirstLastOverTime_ChDB proves
// ts_of_first_over_time / ts_of_last_over_time report the earliest / latest
// subquery-anchor timestamp as an epoch-seconds float.
func TestSubqueryHistogramTsOfFirstLastOverTime_ChDB(t *testing.T) {
	fixture, metric, evalTS := subqSelectHistFixture(t)
	s := schema.DefaultOTelMetrics()
	anchor1 := time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)
	anchor2 := time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC)

	sqlStr, args := lowerAndEmit(t, "ts_of_first_over_time(("+metric+")[2m:1m])", s, evalTS)
	got := sampleValueRows(t, fixture, sqlStr, args)
	if got["a"] != float64(anchor1.Unix()) {
		t.Errorf("ts_of_first_over_time: series a = %v, want %v (00:01 anchor)", got["a"], anchor1.Unix())
	}

	sqlStr, args = lowerAndEmit(t, "ts_of_last_over_time(("+metric+")[2m:1m])", s, evalTS)
	got = sampleValueRows(t, fixture, sqlStr, args)
	if got["a"] != float64(anchor2.Unix()) {
		t.Errorf("ts_of_last_over_time: series a = %v, want %v (00:02 anchor)", got["a"], anchor2.Unix())
	}
}

// TestSubqueryHistogramSumAvgOverTime_ChDB proves sum_over_time /
// avg_over_time — cerberus issue #2545's fix to
// [rangeFnOverExpHistogramSubquery]'s switch, not a new lowering — fold the
// subquery's two published histograms ("sum whatever is there", no
// boundary-extrapolation correction — [expHistogramValuedOverTimeFold]).
func TestSubqueryHistogramSumAvgOverTime_ChDB(t *testing.T) {
	fixture, metric, evalTS := subqSelectHistFixture(t)
	s := schema.DefaultOTelMetrics()

	sqlStr, args := lowerAndEmit(t, "sum_over_time(("+metric+")[2m:1m])", s, evalTS)
	rows := subqHistQueryRows(t, fixture, sqlStr, args)
	if len(rows) != 1 {
		t.Fatalf("sum_over_time: got %d rows, want 1: %+v", len(rows), rows)
	}
	if got := subqHistRowAt(t, rows, "a", evalTS); got.cnt != 5 || got.sum != 13.0 || got.bucket1 != 18 {
		t.Errorf("sum_over_time = %+v, want Count=5 Sum=13 Bucket1=18 (2+3, 4+9, 6+12)", got)
	}

	sqlStr, args = lowerAndEmit(t, "avg_over_time(("+metric+")[2m:1m])", s, evalTS)
	rows = subqHistQueryRows(t, fixture, sqlStr, args)
	if len(rows) != 1 {
		t.Fatalf("avg_over_time: got %d rows, want 1: %+v", len(rows), rows)
	}
	if got := subqHistRowAt(t, rows, "a", evalTS); got.cnt != 2.5 || got.sum != 6.5 || got.bucket1 != 9 {
		t.Errorf("avg_over_time = %+v, want Count=2.5 Sum=6.5 Bucket1=9 (mean of the two samples)", got)
	}
}
