//go:build chdb

// chDB-backed proof for cerberus issue #2449's tenth and final wrapper
// family: a mixed float/histogram `or` operand paired with an ORDINARY,
// non-mixed plain vector
// (histogram_native_mixed_or_vector_plain_arithmetic.go /
// _plain_comparison.go) computes the correct payload for both
// discriminator values the mixed side can carry, not merely that the
// emitted plan lowers without error.
//
// Two `series` label values realise the two combinations the plain side's
// own static always-float discriminator leaves possible (see
// histogram_native_mixed_or_vector_plain_arithmetic.go's own header for why
// "both histogram" can never occur here):
//
//   - "f": the mixed `or`'s float arm has data -> the mixed side resolves
//     float.
//   - "h": the mixed `or`'s histogram arm has data -> the mixed side
//     resolves histogram.
//
// The mixed side's float arm reuses the EXACT [1,2,3,4]/Count=10/Sum=10.0
// bucket layout histogram_native_mixed_or_vector_arithmetic_chdb_test.go's
// own mvvSeed already establishes, so this file's "f" float value is the
// SAME oracle-pinned mvvQuantileBaseline rather than a value re-derived and
// liable to drift. The plain side reuses histogram_native_float_vector_
// scaling_binop_swap_chdb_test.go's own swapGaugeSeedDDL for the
// otel_metrics_gauge/otel_metrics_sum table pair.
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
	mvpHistMetric  = "mvp_hist_exp_hist"
	mvpBaseMetric  = "mvp_base_exp_hist"
	mvpPlainMetric = "mvp_plain_gauge"
)

// mvpPlainValue is mvpPlainMetric's own seeded Value for both series.
const mvpPlainValue = 3.0

var mvpEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// mvpSeed seeds the two-series set every arithmetic/comparison subtest in
// this file shares: "f"'s mixed-or float arm (via mvpBaseMetric,
// reusing mvvSeed's own [1,2,3,4]/Count=10/Sum=10.0 layout), "h"'s
// mixed-or histogram arm (mvpHistMetric, Count=2/Sum=4.0/bucket=[6]), and
// mvpPlainMetric's own row for both series.
var mvpSeed = "" +
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
	"    ('" + mvpBaseMetric + "', map('series', 'f'), toDateTime64('2026-01-01 00:00:00', 9), 10, 10.0, 0, 0, 0, [1, 2, 3, 4], 0, []),\n" +
	"    ('" + mvpHistMetric + "', map('series', 'h'), toDateTime64('2026-01-01 00:00:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []);\n" +
	swapGaugeSeedDDL +
	"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
	"    ('" + mvpPlainMetric + "', map('series', 'f'), toDateTime64('2026-01-01 00:00:00', 9), 3.0),\n" +
	"    ('" + mvpPlainMetric + "', map('series', 'h'), toDateTime64('2026-01-01 00:00:00', 9), 3.0);\n"

func mvpMixedExpr() string {
	return `(` + mvpHistMetric + ` or histogram_quantile(0.5, ` + mvpBaseMetric + `))`
}

// mvpRow is one decoded output row — mirrors mvvRow
// (histogram_native_mixed_or_vector_arithmetic_chdb_test.go).
type mvpRow struct {
	series   string
	disc     int
	val      float64
	cnt, sum float64
	bucket1  float64
}

// mvpRunQuery mirrors mvvRunQuery exactly, projecting the Mixed
// fourteen-column shape when the plan roots in one (every arithmetic op
// and the non-`bool` comparison) or the plain four-column shape otherwise
// (`bool`-modified comparisons force a float-only SampleRowShape output —
// see histogram_native_mixed_or_vector_comparison.go's own header).
func mvpRunQuery(t *testing.T, fixture *chdbFixture, s schema.Metrics, p parser.Parser, query string) map[string]mvpRow {
	t.Helper()
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, mvpEvalTS, mvpEvalTS)
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
			"`Value` AS val, `HistogramCount` AS cnt, `HistogramSum` AS sum, `HistogramPositiveBucketCounts`[1] AS bucket1"
	}

	rows := fixture.queryOverEmitted(t, projection, sqlStr, args)
	defer func() { _ = rows.Close() }()

	out := map[string]mvpRow{}
	for rows.Next() {
		var r mvpRow
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

// TestVectorPlainArithmeticOverMixedSetOpOr_ChDB proves `+`/`-`/`*`/`/`
// against a real chDB execution for a mixed `or` operand paired with an
// ordinary plain vector: "f" (mixed resolves float) always keeps with the
// genuine float arithmetic result; "h" (mixed resolves histogram) keeps
// ONLY for `*`/`/` (scaled), matching the histogram,float combination
// histogram_native_mixed_or_vector_arithmetic.go's own header already
// derives from reference's vectorElemBinop — this file's own header
// explains why the plain side's static discriminator=0 makes that fold
// reusable unchanged.
func TestVectorPlainArithmeticOverMixedSetOpOr_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, mvpSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	q := mvvQuantileBaseline

	t.Run("+", func(t *testing.T) {
		rows := mvpRunQuery(t, fixture, s, p, mvpMixedExpr()+" + "+mvpPlainMetric)
		if got, want := rows["f"].val, q+mvpPlainValue; math.Abs(got-want) > 1e-9 {
			t.Errorf("f: Value = %v, want %v", got, want)
		}
		if _, ok := rows["h"]; ok {
			t.Errorf("h: got a row, want none (+ drops a histogram,float pair)")
		}
	})

	t.Run("-", func(t *testing.T) {
		rows := mvpRunQuery(t, fixture, s, p, mvpMixedExpr()+" - "+mvpPlainMetric)
		if got, want := rows["f"].val, q-mvpPlainValue; math.Abs(got-want) > 1e-9 {
			t.Errorf("f: Value = %v, want %v", got, want)
		}
		if _, ok := rows["h"]; ok {
			t.Errorf("h: got a row, want none (- drops a histogram,float pair)")
		}
	})

	t.Run("*", func(t *testing.T) {
		rows := mvpRunQuery(t, fixture, s, p, mvpMixedExpr()+" * "+mvpPlainMetric)
		f, ok := rows["f"]
		if !ok {
			t.Fatalf("f: no row, want one")
		}
		if f.disc != 0 {
			t.Errorf("f: disc = %d, want 0", f.disc)
		}
		if got, want := f.val, q*mvpPlainValue; math.Abs(got-want) > 1e-9 {
			t.Errorf("f: Value = %v, want %v", got, want)
		}

		h, ok := rows["h"]
		if !ok {
			t.Fatalf("h: no row, want one (histogram,float keeps for *, scaled)")
		}
		if h.disc != 1 {
			t.Errorf("h: disc = %d, want 1", h.disc)
		}
		if got, want := h.cnt, 2*mvpPlainValue; math.Abs(got-want) > 1e-9 {
			t.Errorf("h: HistogramCount = %v, want %v", got, want)
		}
		if got, want := h.sum, 4*mvpPlainValue; math.Abs(got-want) > 1e-9 {
			t.Errorf("h: HistogramSum = %v, want %v", got, want)
		}
		if got, want := h.bucket1, 6*mvpPlainValue; math.Abs(got-want) > 1e-9 {
			t.Errorf("h: HistogramPositiveBucketCounts[1] = %v, want %v", got, want)
		}
	})

	t.Run("/ mixed-numerator", func(t *testing.T) {
		// Mixed on the syntactic LHS (numerator), plain on RHS
		// (denominator, always float) — DIV keeps float,float AND
		// histogram,float (a histogram-shaped numerator scales fine over
		// a float denominator).
		rows := mvpRunQuery(t, fixture, s, p, mvpMixedExpr()+" / "+mvpPlainMetric)
		f, ok := rows["f"]
		if !ok {
			t.Fatalf("f: no row, want one")
		}
		if got, want := f.val, q/mvpPlainValue; math.Abs(got-want) > 1e-9 {
			t.Errorf("f: Value = %v, want %v", got, want)
		}
		h, ok := rows["h"]
		if !ok {
			t.Fatalf("h: no row, want one (histogram,float keeps for /, scaled)")
		}
		if h.disc != 1 {
			t.Errorf("h: disc = %d, want 1", h.disc)
		}
		if got, want := h.cnt, 2/mvpPlainValue; math.Abs(got-want) > 1e-9 {
			t.Errorf("h: HistogramCount = %v, want %v", got, want)
		}
		if got, want := h.sum, 4/mvpPlainValue; math.Abs(got-want) > 1e-9 {
			t.Errorf("h: HistogramSum = %v, want %v", got, want)
		}
	})

	t.Run("/ mixed-denominator", func(t *testing.T) {
		// Plain on the syntactic LHS (numerator, always float), mixed on
		// RHS (denominator) — DIV requires a FLOAT denominator, so "h"
		// (mixed resolves histogram there) must drop even though it kept
		// when mixed played the numerator above: proves the fold reads
		// the join's OWN Left/Right positions, not merely "the mixed
		// side", for a non-commutative op.
		rows := mvpRunQuery(t, fixture, s, p, mvpPlainMetric+" / "+mvpMixedExpr())
		f, ok := rows["f"]
		if !ok {
			t.Fatalf("f: no row, want one")
		}
		if got, want := f.val, mvpPlainValue/q; math.Abs(got-want) > 1e-9 {
			t.Errorf("f: Value = %v, want %v", got, want)
		}
		if _, ok := rows["h"]; ok {
			t.Errorf("h: got a row, want none (a histogram-shaped denominator is never valid for /)")
		}
	})
}

// TestVectorPlainPowModAtan2OverMixedSetOpOr_ChDB proves `^`/`%`/`atan2`:
// reference drops EVERY histogram-involving combination for these three
// ops, so "h" must drop regardless of which side is mixed while "f"
// computes the ordinary float result.
func TestVectorPlainPowModAtan2OverMixedSetOpOr_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, mvpSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	q := mvvQuantileBaseline

	assertOnlyFloatSurvives := func(t *testing.T, rows map[string]mvpRow, want float64) {
		t.Helper()
		f, ok := rows["f"]
		if !ok {
			t.Fatalf("f: no row, want one")
		}
		if math.Abs(f.val-want) > 1e-9 {
			t.Errorf("f: Value = %v, want %v", f.val, want)
		}
		if _, ok := rows["h"]; ok {
			t.Errorf("h: got a row, want none (a histogram-involving pair always drops for this op)")
		}
	}

	t.Run("^", func(t *testing.T) {
		rows := mvpRunQuery(t, fixture, s, p, mvpMixedExpr()+" ^ "+mvpPlainMetric)
		assertOnlyFloatSurvives(t, rows, math.Pow(q, mvpPlainValue))
	})
	t.Run("%", func(t *testing.T) {
		rows := mvpRunQuery(t, fixture, s, p, mvpMixedExpr()+" % "+mvpPlainMetric)
		assertOnlyFloatSurvives(t, rows, math.Mod(q, mvpPlainValue))
	})
	t.Run("atan2", func(t *testing.T) {
		rows := mvpRunQuery(t, fixture, s, p, mvpMixedExpr()+" atan2 "+mvpPlainMetric)
		assertOnlyFloatSurvives(t, rows, math.Atan2(q, mvpPlainValue))
	})
}

// TestVectorPlainComparisonOverMixedSetOpOr_ChDB proves comparisons: a
// non-`bool` `>` keeps "f" (q=6.3496.. > 3, real float comparison) and
// drops "h" unconditionally (a histogram,float pair is always an
// incompatible-type error, `bool` or not — verified against reference's
// own vectorElemBinop by histogram_native_mixed_or_vector_comparison.go's
// header); a `bool`-modified `==` proves the always-float 1.0/0.0 output
// shape the same way.
func TestVectorPlainComparisonOverMixedSetOpOr_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, mvpSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	q := mvvQuantileBaseline
	if q <= mvpPlainValue {
		t.Fatalf("test assumption violated: mvvQuantileBaseline (%v) must be > mvpPlainValue (%v)", q, mvpPlainValue)
	}

	t.Run(">", func(t *testing.T) {
		rows := mvpRunQuery(t, fixture, s, p, mvpMixedExpr()+" > "+mvpPlainMetric)
		f, ok := rows["f"]
		if !ok {
			t.Fatalf("f: no row, want one (q > mvpPlainValue is true)")
		}
		if got, want := f.val, q; math.Abs(got-want) > 1e-9 {
			t.Errorf("f: Value = %v, want %v (comparison forwards L's own float Value unchanged)", got, want)
		}
		if _, ok := rows["h"]; ok {
			t.Errorf("h: got a row, want none (a histogram,float pair is always dropped for a comparison)")
		}
	})

	t.Run("== bool", func(t *testing.T) {
		// q != mvpPlainValue, so "f" keeps (bool always keeps a
		// float,float pair) with Value=0.0; "h" still drops — bool never
		// rescues a histogram-involving pair.
		rows := mvpRunQuery(t, fixture, s, p, mvpMixedExpr()+" == bool "+mvpPlainMetric)
		f, ok := rows["f"]
		if !ok {
			t.Fatalf("f: no row, want one (bool always keeps a float,float pair)")
		}
		if f.val != 0.0 {
			t.Errorf("f: Value = %v, want 0.0 (q != mvpPlainValue)", f.val)
		}
		if _, ok := rows["h"]; ok {
			t.Errorf("h: got a row, want none (bool never rescues a histogram,float pair)")
		}
	})
}

const (
	mvpBroadcastHistMetric  = "mvp_broadcast_hist_exp_hist"
	mvpBroadcastPlainMetric = "mvp_broadcast_plain_gauge"
)

// mvpBroadcastSeed backs the group_right()/CardOneToMany scenario: ONE
// mixed-or row (job="api", resolving histogram: Count=3/Sum=6.0/bucket=[9])
// and TWO plain rows sharing job="api" but distinguished by "zone" — the
// same broadcast shape histogram_native_float_vector_scaling_binop_swap_
// chdb_test.go's own swapBroadcastSeed proves for a non-mixed histogram,
// here proving [chplan.MixedVectorJoin]'s Card plumbing composes with a
// widened plain "many" side too.
var mvpBroadcastSeed = "" +
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
	"    ('" + mvpBroadcastHistMetric + "', map('job', 'api', 'region', 'eu'), toDateTime64('2026-01-01 00:00:00', 9), 3, 6.0, 0, 0, 0, [9], 0, []);\n" +
	swapGaugeSeedDDL +
	"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
	"    ('" + mvpBroadcastPlainMetric + "', map('job', 'api', 'zone', 'z1'), toDateTime64('2026-01-01 00:00:00', 9), 2.0),\n" +
	"    ('" + mvpBroadcastPlainMetric + "', map('job', 'api', 'zone', 'z2'), toDateTime64('2026-01-01 00:00:00', 9), 5.0);\n"

// TestVectorPlainGroupRightOverMixedSetOpOr_ChDB proves
// `(hist or ...) * on(job) group_right(region) plain` — the mixed `or`
// (resolving histogram here) on the syntactic LHS playing the "one" role,
// broadcast across the plain side's two "many" rows, each scaled by its
// own Value, with Include(region) carrying the mixed side's own "region"
// label onto every broadcast row.
func TestVectorPlainGroupRightOverMixedSetOpOr_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, mvpBroadcastSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	query := `(` + mvpBroadcastHistMetric + ` or histogram_quantile(0.5, ` + mvpBroadcastHistMetric + `))` +
		" * on(job) group_right(region) " + mvpBroadcastPlainMetric
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, mvpEvalTS, mvpEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}

	projection := "`Attributes`['job'] AS job, `Attributes`['zone'] AS zone, `Attributes`['region'] AS region, " +
		"`HistogramCount` AS cnt, `HistogramSum` AS sum, `HistogramPositiveBucketCounts`[1] AS bucket1"
	rows := fixture.queryOverEmitted(t, projection, sqlStr, args)
	defer func() { _ = rows.Close() }()

	wantByZone := map[string]struct{ cnt, sum, bucket1 float64 }{
		"z1": {6, 12.0, 18},  // hist(3,6.0,[9]) * plain(Value=2)
		"z2": {15, 30.0, 45}, // hist(3,6.0,[9]) * plain(Value=5)
	}
	seen := map[string]bool{}
	for rows.Next() {
		var job, zone, region string
		var cnt, sum, bucket1 float64
		if err := rows.Scan(&job, &zone, &region, &cnt, &sum, &bucket1); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if job != "api" {
			t.Errorf("job = %q, want %q", job, "api")
		}
		if region != "eu" {
			t.Errorf("zone %s: region = %q, want %q (Include must carry the mixed side's own label onto the broadcast row)", zone, region, "eu")
		}
		want, ok := wantByZone[zone]
		if !ok {
			t.Fatalf("unexpected zone %q", zone)
		}
		seen[zone] = true
		if math.Abs(cnt-want.cnt) > 1e-9 {
			t.Errorf("zone %s: HistogramCount = %v, want %v", zone, cnt, want.cnt)
		}
		if math.Abs(sum-want.sum) > 1e-9 {
			t.Errorf("zone %s: HistogramSum = %v, want %v", zone, sum, want.sum)
		}
		if math.Abs(bucket1-want.bucket1) > 1e-9 {
			t.Errorf("zone %s: HistogramPositiveBucketCounts[1] = %v, want %v", zone, bucket1, want.bucket1)
		}
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if len(seen) != len(wantByZone) {
		t.Fatalf("got rows for zones %v, want both %v present (a broken broadcast drops one matching plain row)", seen, wantByZone)
	}
}
