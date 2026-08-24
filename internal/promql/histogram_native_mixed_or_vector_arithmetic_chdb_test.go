//go:build chdb

// chDB-backed proof that vector-vector arithmetic over TWO independently
// mixed float/histogram `or` operands (cerberus issue #2449's seventh
// wrapper family, histogram_native_mixed_or_vector_arithmetic.go) computes
// the correct payload for each of the four float/histogram combinations a
// matched row pair can carry — not merely that the emitted plan's Go shape
// looks right.
//
// Four `series` label values realise the four combinations, each driven by
// which of the two underlying selectors (a bare exp-histogram metric vs. a
// histogram_quantile()-reduced float metric) has data for that series —
// the mixed `or`'s own shadow rule means at most one arm survives per
// series:
//
//   - "ff": neither side's histogram metric has a row -> both mixed-or
//     operands resolve float (via histogram_quantile).
//   - "fh": only the RHS histogram metric has a row -> LHS float, RHS
//     histogram.
//   - "hf": only the LHS histogram metric has a row -> LHS histogram, RHS
//     float.
//   - "hh": both histogram metrics have a row -> both operands resolve
//     histogram.
//
// Both mixed-or operands' float arm reuses the EXACT [1,2,3,4]/Count=10/
// Sum=10.0 bucket layout histogram_native_mixed_or_scale_chdb_test.go's
// own scaleWrappedFloatMetric does, so this test's "ff"/"fh"/"hf" float
// values are the SAME oracle-pinned 6.3496042078727974 quantile that file
// (and test/spec/promql's own exp_histogram_set_op_or_mixed_float_left.txtar)
// already establishes, rather than a value re-derived and liable to drift.
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
	mvvLHSHistMetric = "mvv_lhs_hist_exp_hist"
	mvvLHSBaseMetric = "mvv_lhs_base_exp_hist"
	mvvRHSHistMetric = "mvv_rhs_hist_exp_hist"
	mvvRHSBaseMetric = "mvv_rhs_base_exp_hist"
)

// mvvQuantileBaseline is the histogram_quantile(0.5, ...) answer for the
// [1,2,3,4]/Count=10/Sum=10.0 bucket layout both mvvLHSBaseMetric and
// mvvRHSBaseMetric seed for every series whose float arm participates —
// see this file's header for why it is reused rather than re-derived.
const mvvQuantileBaseline = 6.3496042078727974

// mvvSeed seeds all four series' active arms: mvvLHSHistMetric only for
// "hf"/"hh" (LHS resolves histogram there), mvvRHSHistMetric only for
// "fh"/"hh", and both base (quantile) metrics for every series whose own
// side needs to resolve float ("ff" on both sides; "fh"'s LHS; "hf"'s
// RHS) — a series whose histogram metric is seeded does not also need its
// base metric seeded, since the `or`'s shadow rule means that arm would
// never surface even if present.
var mvvSeed = "" +
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
	// "ff": both sides float.
	"    ('" + mvvLHSBaseMetric + "', map('series', 'ff'), toDateTime64('2026-01-01 00:00:00', 9), 10, 10.0, 0, 0, 0, [1, 2, 3, 4], 0, []),\n" +
	"    ('" + mvvRHSBaseMetric + "', map('series', 'ff'), toDateTime64('2026-01-01 00:00:00', 9), 10, 10.0, 0, 0, 0, [1, 2, 3, 4], 0, []),\n" +
	// "fh": LHS float, RHS histogram (Count=5, Sum=10.0, bucket=[15]).
	"    ('" + mvvLHSBaseMetric + "', map('series', 'fh'), toDateTime64('2026-01-01 00:00:00', 9), 10, 10.0, 0, 0, 0, [1, 2, 3, 4], 0, []),\n" +
	"    ('" + mvvRHSHistMetric + "', map('series', 'fh'), toDateTime64('2026-01-01 00:00:00', 9), 5, 10.0, 0, 0, 0, [15], 0, []),\n" +
	// "hf": LHS histogram (Count=2, Sum=4.0, bucket=[6]), RHS float.
	"    ('" + mvvLHSHistMetric + "', map('series', 'hf'), toDateTime64('2026-01-01 00:00:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []),\n" +
	"    ('" + mvvRHSBaseMetric + "', map('series', 'hf'), toDateTime64('2026-01-01 00:00:00', 9), 10, 10.0, 0, 0, 0, [1, 2, 3, 4], 0, []),\n" +
	// "hh": both sides histogram.
	"    ('" + mvvLHSHistMetric + "', map('series', 'hh'), toDateTime64('2026-01-01 00:00:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []),\n" +
	"    ('" + mvvRHSHistMetric + "', map('series', 'hh'), toDateTime64('2026-01-01 00:00:00', 9), 5, 10.0, 0, 0, 0, [15], 0, []);\n"

var mvvEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

func mvvLHSExpr() string {
	return `(` + mvvLHSHistMetric + ` or histogram_quantile(0.5, ` + mvvLHSBaseMetric + `))`
}

func mvvRHSExpr() string {
	return `(` + mvvRHSHistMetric + ` or histogram_quantile(0.5, ` + mvvRHSBaseMetric + `))`
}

// mvvRow is one decoded output row: which series, whether it is
// histogram-shaped, and its float Value / three histogram probe fields
// (Count, Sum, first positive bucket).
type mvvRow struct {
	series   string
	disc     int
	val      float64
	cnt, sum float64
	bucket1  float64
}

func mvvRunQuery(t *testing.T, fixture *chdbFixture, s schema.Metrics, p parser.Parser, query string) map[string]mvvRow {
	t.Helper()
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, mvvEvalTS, mvvEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}

	// The projected column set differs between the float-only (+/-) and
	// Mixed (*//) shapes — [chplan.RowShapeOf] is the same classifier
	// the lowering's own tests use to distinguish them ([chplan.
	// SampleRowShape] plain quartet vs. [chplan.MixedRowShape]'s
	// fourteen columns). ADD/SUB queries probe only `Attributes`/`Value`
	// (correct: reference keeps only float,float for these two ops, so
	// every surviving row IS float-shaped).
	mixed := chplan.RowShapeOf(plan) == chplan.MixedRowShape
	projection := "`Attributes`['series'] AS series, `Value` AS val"
	if mixed {
		projection = "`Attributes`['series'] AS series, `_setop_is_histogram` AS disc, " +
			"`Value` AS val, `HistogramCount` AS cnt, `HistogramSum` AS sum, `HistogramPositiveBucketCounts`[1] AS bucket1"
	}

	rows := fixture.queryOverEmitted(t, projection, sqlStr, args)
	defer func() { _ = rows.Close() }()

	out := map[string]mvvRow{}
	for rows.Next() {
		var r mvvRow
		if mixed {
			if err := rows.Scan(&r.series, &r.disc, &r.val, &r.cnt, &r.sum, &r.bucket1); err != nil {
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

// TestVectorVectorArithmeticOverMixedSetOpOr_ChDB proves each of the four
// float/histogram combinations against a real chDB execution, for each of
// the four arithmetic ops this file's lowering answers.
func TestVectorVectorArithmeticOverMixedSetOpOr_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, mvvSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	q := mvvQuantileBaseline

	t.Run("+", func(t *testing.T) {
		rows := mvvRunQuery(t, fixture, s, p, mvvLHSExpr()+" + "+mvvRHSExpr())
		if got, want := rows["ff"].val, q+q; math.Abs(got-want) > 1e-9 {
			t.Errorf("ff: Value = %v, want %v", got, want)
		}
		for _, series := range []string{"fh", "hf", "hh"} {
			if _, ok := rows[series]; ok {
				t.Errorf("%s: got a row, want none (+ drops every combination but float,float, including the histogram,histogram merge this PR does not implement)", series)
			}
		}
	})

	t.Run("-", func(t *testing.T) {
		rows := mvvRunQuery(t, fixture, s, p, mvvLHSExpr()+" - "+mvvRHSExpr())
		if got, want := rows["ff"].val, q-q; math.Abs(got-want) > 1e-9 {
			t.Errorf("ff: Value = %v, want %v", got, want)
		}
		for _, series := range []string{"fh", "hf", "hh"} {
			if _, ok := rows[series]; ok {
				t.Errorf("%s: got a row, want none", series)
			}
		}
	})

	t.Run("*", func(t *testing.T) {
		rows := mvvRunQuery(t, fixture, s, p, mvvLHSExpr()+" * "+mvvRHSExpr())
		if got, want := rows["ff"].val, q*q; math.Abs(got-want) > 1e-9 {
			t.Errorf("ff: Value = %v, want %v", got, want)
		}
		if rows["ff"].disc != 0 {
			t.Errorf("ff: disc = %d, want 0", rows["ff"].disc)
		}

		// fh: R's histogram (Count=5, Sum=10.0, bucket1=15) scaled by L's
		// float (q).
		fh, ok := rows["fh"]
		if !ok {
			t.Fatalf("fh: no row, want one (float,histogram keeps for *)")
		}
		if fh.disc != 1 {
			t.Errorf("fh: disc = %d, want 1", fh.disc)
		}
		if got, want := fh.cnt, 5*q; math.Abs(got-want) > 1e-6 {
			t.Errorf("fh: HistogramCount = %v, want %v", got, want)
		}
		if got, want := fh.sum, 10*q; math.Abs(got-want) > 1e-6 {
			t.Errorf("fh: HistogramSum = %v, want %v", got, want)
		}
		if got, want := fh.bucket1, 15*q; math.Abs(got-want) > 1e-6 {
			t.Errorf("fh: HistogramPositiveBucketCounts[1] = %v, want %v", got, want)
		}

		// hf: L's histogram (Count=2, Sum=4.0, bucket1=6) scaled by R's
		// float (q).
		hf, ok := rows["hf"]
		if !ok {
			t.Fatalf("hf: no row, want one (histogram,float keeps for *)")
		}
		if hf.disc != 1 {
			t.Errorf("hf: disc = %d, want 1", hf.disc)
		}
		if got, want := hf.cnt, 2*q; math.Abs(got-want) > 1e-6 {
			t.Errorf("hf: HistogramCount = %v, want %v", got, want)
		}
		if got, want := hf.sum, 4*q; math.Abs(got-want) > 1e-6 {
			t.Errorf("hf: HistogramSum = %v, want %v", got, want)
		}
		if got, want := hf.bucket1, 6*q; math.Abs(got-want) > 1e-6 {
			t.Errorf("hf: HistogramPositiveBucketCounts[1] = %v, want %v", got, want)
		}

		if _, ok := rows["hh"]; ok {
			t.Errorf("hh: got a row, want none (histogram,histogram drops for *, matching reference exactly)")
		}
	})

	t.Run("/", func(t *testing.T) {
		rows := mvvRunQuery(t, fixture, s, p, mvvLHSExpr()+" / "+mvvRHSExpr())
		if got, want := rows["ff"].val, q/q; math.Abs(got-want) > 1e-9 {
			t.Errorf("ff: Value = %v, want %v", got, want)
		}

		if _, ok := rows["fh"]; ok {
			t.Errorf("fh: got a row, want none (float,histogram drops for / — DIV only scales a histogram-shaped NUMERATOR)")
		}

		// hf: L's histogram (Count=2, Sum=4.0, bucket1=6) divided by R's
		// float (q).
		hf, ok := rows["hf"]
		if !ok {
			t.Fatalf("hf: no row, want one (histogram,float keeps for /)")
		}
		if hf.disc != 1 {
			t.Errorf("hf: disc = %d, want 1", hf.disc)
		}
		if got, want := hf.cnt, 2/q; math.Abs(got-want) > 1e-6 {
			t.Errorf("hf: HistogramCount = %v, want %v", got, want)
		}
		if got, want := hf.sum, 4/q; math.Abs(got-want) > 1e-6 {
			t.Errorf("hf: HistogramSum = %v, want %v", got, want)
		}
		if got, want := hf.bucket1, 6/q; math.Abs(got-want) > 1e-6 {
			t.Errorf("hf: HistogramPositiveBucketCounts[1] = %v, want %v", got, want)
		}

		if _, ok := rows["hh"]; ok {
			t.Errorf("hh: got a row, want none (histogram,histogram drops for /, matching reference exactly)")
		}
	})
}
