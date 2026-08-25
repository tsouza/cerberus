//go:build chdb

// chDB-backed proof for cerberus issue #2555: a mixed float/histogram
// `or` (histogram_native_mixed_or.go, cerberus issue #2330) NESTED as the
// operand of a further vector set operator (`and`/`unless`/`or`) computes
// the correct payload for both discriminator values the mixed side can
// carry, AND still honours that outer operator's own label-signature
// filtering — not merely that the emitted plan lowers without error.
//
// Four `series` label values exercise the filtering together with the
// mixed side's own two discriminator values:
//
//   - "f": the mixed `or`'s float arm has data (via monBaseMetric, the
//     SAME [1,2,3,4]/Count=10/Sum=10.0 layout
//     histogram_native_mixed_or_vector_arithmetic_chdb_test.go's own
//     mvvSeed establishes, so "f" reuses that same oracle-pinned
//     [mvvQuantileBaseline]) -- the mixed side resolves float. Also
//     present on the plain operand.
//   - "h": the mixed `or`'s histogram arm has data (via monHistMetric,
//     Count=2/Sum=4.0/bucket=[6], the same layout
//     histogram_native_mixed_or_vector_plain_chdb_test.go's own mvpSeed
//     uses for "h") -- the mixed side resolves histogram. Also present on
//     the plain operand.
//   - "x": mixed-side-ONLY (same float layout as "f", so it resolves
//     float too) -- absent from the plain operand, proving `and`/`unless`
//     still apply their own signature filter to a Mixed-shaped LEFT
//     rather than forwarding every LHS row unconditionally.
//   - "p": plain-operand-ONLY -- absent from the mixed side entirely,
//     proving `or`'s anti-right union still applies to a Mixed LEFT the
//     same way it does to a plain float one, and that `and`/`unless`
//     never surface a RHS-only row at all.
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
	monHistMetric  = "mon_hist_exp_hist"
	monBaseMetric  = "mon_base_exp_hist"
	monPlainMetric = "mon_plain_gauge"
)

// monPlainValue is monPlainMetric's own seeded Value for every series it
// carries ("f", "h", "p").
const monPlainValue = 42.0

var monEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// monSeed backs every subtest in this file: "f" and "x" both resolve the
// mixed `or`'s float arm (monBaseMetric, mvvSeed's own layout); "h"
// resolves its histogram arm (monHistMetric); monPlainMetric carries "f",
// "h" (so the outer and/unless/or filter has real overlap to resolve) and
// "p" (mixed-side-absent, so `or`'s anti-right union has a genuine RHS-only
// row to carry through).
var monSeed = "" +
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
	"    ('" + monBaseMetric + "', map('series', 'f'), toDateTime64('2026-01-01 00:00:00', 9), 10, 10.0, 0, 0, 0, [1, 2, 3, 4], 0, []),\n" +
	"    ('" + monHistMetric + "', map('series', 'h'), toDateTime64('2026-01-01 00:00:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []),\n" +
	"    ('" + monBaseMetric + "', map('series', 'x'), toDateTime64('2026-01-01 00:00:00', 9), 10, 10.0, 0, 0, 0, [1, 2, 3, 4], 0, []);\n" +
	swapGaugeSeedDDL +
	"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
	"    ('" + monPlainMetric + "', map('series', 'f'), toDateTime64('2026-01-01 00:00:00', 9), 42.0),\n" +
	"    ('" + monPlainMetric + "', map('series', 'h'), toDateTime64('2026-01-01 00:00:00', 9), 42.0),\n" +
	"    ('" + monPlainMetric + "', map('series', 'p'), toDateTime64('2026-01-01 00:00:00', 9), 42.0);\n"

func monMixedExpr() string {
	return `(` + monHistMetric + ` or histogram_quantile(0.5, ` + monBaseMetric + `))`
}

// monRow is one decoded output row — mirrors mvpRow
// (histogram_native_mixed_or_vector_plain_chdb_test.go).
type monRow struct {
	series   string
	disc     int
	val      float64
	cnt, sum float64
	bucket1  float64
}

// monRunQuery mirrors mvpRunQuery: projects the Mixed fourteen-column
// shape when the plan roots in one, or the plain four-column shape
// otherwise. Every query this file runs lowers to a Mixed-shaped plan
// (cerberus issue #2555 — `and`/`unless` forward the Mixed LEFT operand
// verbatim, `or` unions it), so the plain branch is unreached here but
// kept for symmetry with the sibling files this mirrors and as a guard:
// if a future change silently regressed the outer op back to a plain
// SampleRowShape, monAssertRows below would fail loudly on the missing
// disc/cnt/sum/bucket1 destinations rather than this helper masking it.
func monRunQuery(t *testing.T, fixture *chdbFixture, s schema.Metrics, p parser.Parser, query string) map[string]monRow {
	t.Helper()
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, monEvalTS, monEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}

	mixed := chplan.RowShapeOf(plan) == chplan.MixedRowShape
	if !mixed {
		t.Fatalf("RowShapeOf(%q) = %v, want MixedRowShape", query, chplan.RowShapeOf(plan))
	}
	projection := "`Attributes`['series'] AS series, `_setop_is_histogram` AS disc, " +
		"`Value` AS val, `HistogramCount` AS cnt, `HistogramSum` AS sum, `HistogramPositiveBucketCounts`[1] AS bucket1"

	rows := fixture.queryOverEmitted(t, projection, sqlStr, args)
	defer func() { _ = rows.Close() }()

	out := map[string]monRow{}
	for rows.Next() {
		var r monRow
		if err := rows.Scan(&r.series, &r.disc, &r.val, &r.cnt, &r.sum, &r.bucket1); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[r.series] = r
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

// assertMonFloatRow checks a decoded row resolves the mixed side's float
// arm: disc=0, Value=mvvQuantileBaseline.
func assertMonFloatRow(t *testing.T, rows map[string]monRow, series string) {
	t.Helper()
	r, ok := rows[series]
	if !ok {
		t.Fatalf("%s: no row, want one (float arm)", series)
	}
	if r.disc != 0 {
		t.Errorf("%s: disc = %d, want 0", series, r.disc)
	}
	if math.Abs(r.val-mvvQuantileBaseline) > 1e-9 {
		t.Errorf("%s: Value = %v, want %v", series, r.val, mvvQuantileBaseline)
	}
}

// assertMonHistogramRow checks a decoded row resolves the mixed side's
// histogram arm unchanged: disc=1, Count=2, Sum=4.0, bucket[1]=6.
func assertMonHistogramRow(t *testing.T, rows map[string]monRow, series string) {
	t.Helper()
	r, ok := rows[series]
	if !ok {
		t.Fatalf("%s: no row, want one (histogram arm)", series)
	}
	if r.disc != 1 {
		t.Errorf("%s: disc = %d, want 1", series, r.disc)
	}
	if r.cnt != 2 {
		t.Errorf("%s: HistogramCount = %v, want 2", series, r.cnt)
	}
	if r.sum != 4.0 {
		t.Errorf("%s: HistogramSum = %v, want 4.0", series, r.sum)
	}
	if r.bucket1 != 6 {
		t.Errorf("%s: HistogramPositiveBucketCounts[1] = %v, want 6", series, r.bucket1)
	}
}

// assertMonPlainFloatRow checks a decoded row resolves the PLAIN
// operand's own placeholder-widened float row: disc=0, Value=monPlainValue.
func assertMonPlainFloatRow(t *testing.T, rows map[string]monRow, series string) {
	t.Helper()
	r, ok := rows[series]
	if !ok {
		t.Fatalf("%s: no row, want one (plain operand)", series)
	}
	if r.disc != 0 {
		t.Errorf("%s: disc = %d, want 0", series, r.disc)
	}
	if math.Abs(r.val-monPlainValue) > 1e-9 {
		t.Errorf("%s: Value = %v, want %v", series, r.val, monPlainValue)
	}
}

func assertMonAbsent(t *testing.T, rows map[string]monRow, series string) {
	t.Helper()
	if _, ok := rows[series]; ok {
		t.Errorf("%s: got a row, want none", series)
	}
}

// TestMixedSetOpOr_NestedAnd_ChDB proves `(hist or histogram_quantile(...))
// and plain` (cerberus issue #2555's first evidence query): the outer
// `and` forwards the mixed `or`'s own rows verbatim for every signature
// ALSO present on the plain side ("f" float, "h" histogram) and drops the
// mixed-only signature ("x") and the plain-only one ("p") exactly as an
// ordinary float `and` would.
func TestMixedSetOpOr_NestedAnd_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, monSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	rows := monRunQuery(t, fixture, s, p, monMixedExpr()+" and "+monPlainMetric)
	assertMonFloatRow(t, rows, "f")
	assertMonHistogramRow(t, rows, "h")
	assertMonAbsent(t, rows, "x")
	assertMonAbsent(t, rows, "p")
}

// TestMixedSetOpOr_NestedUnless_ChDB proves `(hist or
// histogram_quantile(...)) unless plain` (cerberus issue #2555's second
// evidence query): the outer `unless` keeps only the mixed-only signature
// ("x", the mirror image of the `and` case above) and drops every
// signature the plain side also carries ("f", "h") plus the plain-only
// one ("p", never surfaced by `unless` at all).
func TestMixedSetOpOr_NestedUnless_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, monSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	rows := monRunQuery(t, fixture, s, p, monMixedExpr()+" unless "+monPlainMetric)
	assertMonFloatRow(t, rows, "x")
	assertMonAbsent(t, rows, "f")
	assertMonAbsent(t, rows, "h")
	assertMonAbsent(t, rows, "p")
}

// TestMixedSetOpOr_NestedOr_ChDB proves `(hist or histogram_quantile(...))
// or plain` (cerberus issue #2555's third evidence query): every mixed-
// side row survives unconditionally ("f" float, "h" histogram, "x"
// mixed-only float), plus the plain-only row ("p") the mixed side never
// had a signature for — while the plain side's OWN "f"/"h" rows are
// dropped as anti-right matches, proving the union does not duplicate a
// signature the LHS already answered.
func TestMixedSetOpOr_NestedOr_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, monSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	rows := monRunQuery(t, fixture, s, p, monMixedExpr()+" or "+monPlainMetric)
	assertMonFloatRow(t, rows, "f")
	assertMonHistogramRow(t, rows, "h")
	assertMonFloatRow(t, rows, "x")
	assertMonPlainFloatRow(t, rows, "p")
}
