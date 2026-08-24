//go:build chdb

// chDB-backed proof that vector-vector COMPARISONS over TWO independently
// mixed float/histogram `or` operands (cerberus issue #2449's eighth
// wrapper family, histogram_native_mixed_or_vector_comparison.go) compute
// the correct keep/drop decision and payload for each of the four
// float/histogram combinations a matched row pair can carry, for both the
// non-`bool` filter and the `bool` 1.0/0.0 fold — not merely that the
// emitted plan's Go shape looks right.
//
// Five `series` label values realise the interesting combinations, on top
// of the four-arm scheme histogram_native_mixed_or_vector_arithmetic_chdb_test.go
// already established (reused here under an "mvc" prefix so the two files'
// package-level names never collide):
//
//   - "ff": both operands resolve float, LHS value < RHS value (10 < 20)
//     — exercises `<`/`<=`/`!=` keeping and `>`/`>=`/`==` dropping (or
//     answering false under `bool`).
//   - "fh": LHS float, RHS histogram — every op drops, `bool` or not
//     (the float,histogram incompatible-type case).
//   - "hf": LHS histogram, RHS float — every op drops, `bool` or not
//     (the histogram,float incompatible-type case).
//   - "hh_ne": both operands resolve histogram, with DIFFERENT payloads
//     — `==` drops, `!=` keeps (LHS's own histogram, unchanged); ordering
//     ops always drop, `bool` or not.
//   - "hh_eq": both operands resolve histogram, with IDENTICAL payloads
//     — `==` keeps (LHS's own histogram, unchanged), `!=` drops; ordering
//     ops still always drop, `bool` or not.
package promql_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

const (
	mvcLHSHistMetric = "mvc_lhs_hist_exp_hist"
	mvcLHSBaseMetric = "mvc_lhs_base_exp_hist"
	mvcRHSHistMetric = "mvc_rhs_hist_exp_hist"
	mvcRHSBaseMetric = "mvc_rhs_base_exp_hist"
)

// mvcSeed seeds five series. "ff" reuses two DIFFERENT bucket layouts on
// the base (quantile) metrics so the two resulting float values are
// unequal and orderable, unlike mvv's own "ff" (which reuses the exact
// same layout on both sides, making every comparison degenerate). "hh_ne"
// gives LHS and RHS histogram metrics different payloads; "hh_eq" gives
// them BIT-IDENTICAL payloads, the case mvv's own fixture never covers.
var mvcSeed = "" +
	"CREATE OR REPLACE TABLE otel_metrics_exponential_histogram (" +
	"`MetricName` String, `Attributes` Map(String, String), " +
	"`ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', " +
	"`TimeUnix` DateTime64(9), " +
	"`Count` UInt64, `Sum` Float64, `Scale` Int32, `ZeroCount` UInt64, " +
	"`PositiveOffset` Int32, `PositiveBucketCounts` Array(UInt64), " +
	"`NegativeOffset` Int32, `NegativeBucketCounts` Array(UInt64)" +
	") ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n" +
	"INSERT INTO otel_metrics_exponential_histogram " +
	"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
	// "ff": LHS quantile over [1,2,3,4] (Count=10, Sum=10.0), RHS quantile
	// over a doubled layout [2,4,6,8] (Count=10, Sum=20.0) -> unequal,
	// orderable float values.
	"    ('" + mvcLHSBaseMetric + "', map('series', 'ff'), toDateTime64('2026-01-01 00:00:00', 9), 10, 10.0, 0, 0, 0, [1, 2, 3, 4], 0, []),\n" +
	"    ('" + mvcRHSBaseMetric + "', map('series', 'ff'), toDateTime64('2026-01-01 00:00:00', 9), 10, 20.0, 0, 0, 0, [2, 4, 6, 8], 0, []),\n" +
	// "fh": LHS float, RHS histogram.
	"    ('" + mvcLHSBaseMetric + "', map('series', 'fh'), toDateTime64('2026-01-01 00:00:00', 9), 10, 10.0, 0, 0, 0, [1, 2, 3, 4], 0, []),\n" +
	"    ('" + mvcRHSHistMetric + "', map('series', 'fh'), toDateTime64('2026-01-01 00:00:00', 9), 5, 10.0, 0, 0, 0, [15], 0, []),\n" +
	// "hf": LHS histogram, RHS float.
	"    ('" + mvcLHSHistMetric + "', map('series', 'hf'), toDateTime64('2026-01-01 00:00:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []),\n" +
	"    ('" + mvcRHSBaseMetric + "', map('series', 'hf'), toDateTime64('2026-01-01 00:00:00', 9), 10, 10.0, 0, 0, 0, [1, 2, 3, 4], 0, []),\n" +
	// "hh_ne": both histogram, DIFFERENT payloads.
	"    ('" + mvcLHSHistMetric + "', map('series', 'hh_ne'), toDateTime64('2026-01-01 00:00:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []),\n" +
	"    ('" + mvcRHSHistMetric + "', map('series', 'hh_ne'), toDateTime64('2026-01-01 00:00:00', 9), 5, 10.0, 0, 0, 0, [15], 0, []),\n" +
	// "hh_eq": both histogram, IDENTICAL payloads.
	"    ('" + mvcLHSHistMetric + "', map('series', 'hh_eq'), toDateTime64('2026-01-01 00:00:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []),\n" +
	"    ('" + mvcRHSHistMetric + "', map('series', 'hh_eq'), toDateTime64('2026-01-01 00:00:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []);\n"

var mvcEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

func mvcLHSExpr() string {
	return `(` + mvcLHSHistMetric + ` or histogram_quantile(0.5, ` + mvcLHSBaseMetric + `))`
}

func mvcRHSExpr() string {
	return `(` + mvcRHSHistMetric + ` or histogram_quantile(0.5, ` + mvcRHSBaseMetric + `))`
}

// mvcRow is one decoded output row: which series, whether it is
// histogram-shaped (non-`bool` path only), its Value, and (non-`bool`,
// histogram-shaped rows only) the LHS Count/first-positive-bucket probe.
type mvcRow struct {
	series  string
	disc    int
	val     float64
	cnt     float64
	bucket1 float64
}

func mvcRunQuery(t *testing.T, fixture *chdbFixture, s schema.Metrics, p parser.Parser, query string) map[string]mvcRow {
	t.Helper()
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, mvcEvalTS, mvcEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}

	mixed := chplan.RowShapeOf(plan) == chplan.MixedRowShape
	projection := "`Attributes`['series'] AS series, `Value` AS val"
	if mixed {
		projection = "`Attributes`['series'] AS series, `_setop_is_histogram` AS disc, " +
			"`Value` AS val, `HistogramCount` AS cnt, `HistogramPositiveBucketCounts`[1] AS bucket1"
	}

	rows := fixture.queryOverEmitted(t, projection, sqlStr, args)
	defer func() { _ = rows.Close() }()

	out := map[string]mvcRow{}
	for rows.Next() {
		var r mvcRow
		if mixed {
			if err := rows.Scan(&r.series, &r.disc, &r.val, &r.cnt, &r.bucket1); err != nil {
				t.Fatalf("scan: %v", err)
			}
		} else {
			if err := rows.Scan(&r.series, &r.val); err != nil {
				t.Fatalf("scan: %v", err)
			}
		}
		out[r.series] = r
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

// wantNoRows fails the test if any of the given series names have a row
// in got.
func wantNoRows(t *testing.T, got map[string]mvcRow, series ...string) {
	t.Helper()
	for _, s := range series {
		if _, ok := got[s]; ok {
			t.Errorf("%s: got a row, want none", s)
		}
	}
}

// TestVectorVectorCompareOverMixedSetOpOr_ChDB_NoBool proves the non-bool
// filter path: incompatible-type pairs (fh, hf) always drop; float,float
// filters on the comparison result and forwards the raw LHS value;
// histogram,histogram filters on structural equality for `==`/`!=` and
// forwards LHS's own histogram unchanged, and always drops for ordering
// ops.
func TestVectorVectorCompareOverMixedSetOpOr_ChDB_NoBool(t *testing.T) {
	fixture := newChDBFixture(t, mvcSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	const lhsQuantile = 6.3496042078727974 // histogram_quantile(0.5, [1,2,3,4]/10/10.0)
	const rhsQuantile = 12.699208415745595 // histogram_quantile(0.5, [2,4,6,8]/10/20.0) — exactly double lhsQuantile.
	if got, want := rhsQuantile, 2*lhsQuantile; math.Abs(got-want) > 1e-9 {
		t.Fatalf("fixture invariant violated: rhsQuantile (%v) is not exactly double lhsQuantile (%v) — the \"ff\" series's orderability assumption below no longer holds", got, want)
	}

	t.Run("==", func(t *testing.T) {
		rows := mvcRunQuery(t, fixture, s, p, mvcLHSExpr()+" == "+mvcRHSExpr())
		wantNoRows(t, rows, "ff", "fh", "hf", "hh_ne")
		eq, ok := rows["hh_eq"]
		if !ok {
			t.Fatalf("hh_eq: no row, want one (identical histograms == keeps)")
		}
		if eq.disc != 1 {
			t.Errorf("hh_eq: disc = %d, want 1 (histogram-valued)", eq.disc)
		}
		if got, want := eq.cnt, 2.0; math.Abs(got-want) > 1e-9 {
			t.Errorf("hh_eq: HistogramCount = %v, want %v", got, want)
		}
		if got, want := eq.bucket1, 6.0; math.Abs(got-want) > 1e-9 {
			t.Errorf("hh_eq: HistogramPositiveBucketCounts[1] = %v, want %v", got, want)
		}
	})

	t.Run("!=", func(t *testing.T) {
		rows := mvcRunQuery(t, fixture, s, p, mvcLHSExpr()+" != "+mvcRHSExpr())
		wantNoRows(t, rows, "fh", "hf", "hh_eq")
		ff, ok := rows["ff"]
		if !ok {
			t.Fatalf("ff: no row, want one (unequal floats != keeps)")
		}
		if got, want := ff.val, lhsQuantile; math.Abs(got-want) > 1e-9 {
			t.Errorf("ff: Value = %v, want %v (LHS's own raw value, unchanged)", got, want)
		}
		ne, ok := rows["hh_ne"]
		if !ok {
			t.Fatalf("hh_ne: no row, want one (different histograms != keeps)")
		}
		if ne.disc != 1 {
			t.Errorf("hh_ne: disc = %d, want 1 (histogram-valued)", ne.disc)
		}
		if got, want := ne.cnt, 2.0; math.Abs(got-want) > 1e-9 {
			t.Errorf("hh_ne: HistogramCount = %v, want %v (LHS's own, unchanged)", got, want)
		}
	})

	t.Run("<", func(t *testing.T) {
		rows := mvcRunQuery(t, fixture, s, p, mvcLHSExpr()+" < "+mvcRHSExpr())
		wantNoRows(t, rows, "fh", "hf", "hh_ne", "hh_eq")
		ff, ok := rows["ff"]
		if !ok {
			t.Fatalf("ff: no row, want one (lhsQuantile < rhsQuantile)")
		}
		if got, want := ff.val, lhsQuantile; math.Abs(got-want) > 1e-9 {
			t.Errorf("ff: Value = %v, want %v", got, want)
		}
	})

	t.Run(">", func(t *testing.T) {
		rows := mvcRunQuery(t, fixture, s, p, mvcLHSExpr()+" > "+mvcRHSExpr())
		wantNoRows(t, rows, "ff", "fh", "hf", "hh_ne", "hh_eq")
	})
}

// TestVectorVectorCompareOverMixedSetOpOr_ChDB_Bool proves the `bool`
// path: every combination the non-bool test keeps still emits, now as
// 1.0/0.0 with no histogram payload; every combination the non-bool test
// DROPS (incompatible types, and histogram,histogram for ordering ops)
// still drops entirely — reference's `bool` modifier does NOT rescue an
// incompatible-type pair.
func TestVectorVectorCompareOverMixedSetOpOr_ChDB_Bool(t *testing.T) {
	fixture := newChDBFixture(t, mvcSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	t.Run("== bool", func(t *testing.T) {
		rows := mvcRunQuery(t, fixture, s, p, mvcLHSExpr()+" == bool "+mvcRHSExpr())
		wantNoRows(t, rows, "fh", "hf")
		if got, want := rows["ff"].val, 0.0; got != want {
			t.Errorf("ff: Value = %v, want %v (unequal floats)", got, want)
		}
		if got, want := rows["hh_ne"].val, 0.0; got != want {
			t.Errorf("hh_ne: Value = %v, want %v (different histograms)", got, want)
		}
		if got, want := rows["hh_eq"].val, 1.0; got != want {
			t.Errorf("hh_eq: Value = %v, want %v (identical histograms)", got, want)
		}
	})

	t.Run("< bool", func(t *testing.T) {
		rows := mvcRunQuery(t, fixture, s, p, mvcLHSExpr()+" < bool "+mvcRHSExpr())
		wantNoRows(t, rows, "fh", "hf", "hh_ne", "hh_eq")
		if got, want := rows["ff"].val, 1.0; got != want {
			t.Errorf("ff: Value = %v, want %v", got, want)
		}
	})

	t.Run("> bool", func(t *testing.T) {
		rows := mvcRunQuery(t, fixture, s, p, mvcLHSExpr()+" > bool "+mvcRHSExpr())
		wantNoRows(t, rows, "fh", "hf", "hh_ne", "hh_eq")
		if got, want := rows["ff"].val, 0.0; got != want {
			t.Errorf("ff: Value = %v, want %v", got, want)
		}
	})
}
