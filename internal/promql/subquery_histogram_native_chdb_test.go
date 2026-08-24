//go:build chdb

// chDB-backed proof that a bare top-level subquery wrapping a
// native-histogram-valued shape (`(<expr>)[range:step]`, no outer
// range-vector function) actually routes through histogram-native
// lowering at real ClickHouse execution — not merely that the emitted
// plan's Go shape looks right (cerberus issue #2543).
//
// Before this fix, lowerSubquery never consulted lowerRoot's
// histogram-native dispatch table for a subquery's OWN inner expression
// ([lowerHistogramNativeSubqueryInner] / [lowerHistogramNativeRoot],
// lower.go / subquery.go). Every shape below either hard-rejected under
// LowerAtRange (bare selector, sum(), the +/- merge, the group_left()
// scaling join) or — the genuinely silent half of #2543 — lowered
// WITHOUT error to an EMPTY float result via
// lowerExpHistogramArgAsCanonicalFloat's "preserve family, reproject to
// empty float" branch (the `* 2` scale shape,
// TestSubqueryHistogramScale_ChDB below). Every case here proves the
// FIXED behaviour: a real, non-empty, correctly-valued matrix at
// multiple distinct subquery anchors, read back from a real chDB
// execution.
package promql_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

// subqHistDDL is the OTel exponential-histogram table layout every case
// in this file seeds against — byte-for-byte the same columns
// histogram_native_mixed_or_scale_chdb_test.go's own seed uses.
const subqHistDDL = "CREATE OR REPLACE TABLE otel_metrics_exponential_histogram (" +
	"`MetricName` String, `Attributes` Map(String, String), " +
	"`ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', " +
	"`TimeUnix` DateTime64(9), " +
	"`Count` UInt64, `Sum` Float64, `Scale` Int32, `ZeroCount` UInt64, " +
	"`PositiveOffset` Int32, `PositiveBucketCounts` Array(UInt64), " +
	"`NegativeOffset` Int32, `NegativeBucketCounts` Array(UInt64)" +
	") ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n"

// subqHistProjection reads back the per-anchor matrix contract every case
// below emits: the wall-clock anchor (`TimeUnix`), plus the three
// scalar/first-bucket histogram fields the assertions need. `[1]` on
// HistogramPositiveBucketCounts sidesteps chdb-go's Array(Float64)
// decode the same way histogram_native_mixed_or_scale_chdb_test.go's own
// projection does.
const subqHistProjection = "`Attributes`['series'] AS series, toUnixTimestamp(`TimeUnix`) AS ts, " +
	"`HistogramCount` AS cnt, `HistogramSum` AS sum, `HistogramPositiveBucketCounts`[1] AS bucket1"

type subqHistRow struct {
	series  string
	ts      int64
	cnt     float64
	sum     float64
	bucket1 float64
}

func subqHistQueryRows(t *testing.T, fixture *chdbFixture, sqlStr string, args []any) []subqHistRow {
	t.Helper()
	rows := fixture.queryOverEmitted(t, subqHistProjection, sqlStr, args)
	defer func() { _ = rows.Close() }()
	var out []subqHistRow
	for rows.Next() {
		var r subqHistRow
		if err := rows.Scan(&r.series, &r.ts, &r.cnt, &r.sum, &r.bucket1); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

func subqHistRowAt(t *testing.T, rows []subqHistRow, series string, ts time.Time) subqHistRow {
	t.Helper()
	for _, r := range rows {
		if r.series == series && r.ts == ts.Unix() {
			return r
		}
	}
	t.Fatalf("no row for series %q at ts %v; got rows %+v", series, ts, rows)
	return subqHistRow{}
}

// TestSubqueryHistogramBareSelector_ChDB proves `(<bare exp-hist
// selector>)[range:step]` — the issue's own first example, and the
// shape that "accidentally" looks like it should work at the top level
// because lowerVectorSelector has its own histogram routing, but still
// hard-rejected under a subquery before this fix (expHistogramSelectorRouting's
// catch-all: a selector nested under anything other than the bare-root
// case it is asked from). Two distinct anchors read back two distinct
// seeded samples, proving genuine per-anchor windowing rather than one
// value broadcast across the matrix.
func TestSubqueryHistogramBareSelector_ChDB(t *testing.T) {
	const metric = "subq_bare_exp_hist"
	seed := subqHistDDL +
		"INSERT INTO otel_metrics_exponential_histogram " +
		"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		"    ('" + metric + "', map('series', 'a'), toDateTime64('2026-01-01 00:01:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []),\n" +
		"    ('" + metric + "', map('series', 'a'), toDateTime64('2026-01-01 00:02:00', 9), 3, 9.0, 0, 0, 0, [12], 0, []);\n"
	fixture := newChDBFixture(t, seed)

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	evalTS := time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC)

	query := "(" + metric + ")[2m:1m]"
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, evalTS, evalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}

	rows := subqHistQueryRows(t, fixture, sqlStr, args)
	if len(rows) != 2 {
		t.Fatalf("%s: got %d rows, want 2 (one per subquery anchor): %+v", query, len(rows), rows)
	}
	anchor1 := subqHistRowAt(t, rows, "a", time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC))
	if anchor1.cnt != 2 || anchor1.sum != 4.0 || anchor1.bucket1 != 6 {
		t.Errorf("%s: anchor 00:01 = %+v, want Count=2 Sum=4 Bucket1=6", query, anchor1)
	}
	anchor2 := subqHistRowAt(t, rows, "a", time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC))
	if anchor2.cnt != 3 || anchor2.sum != 9.0 || anchor2.bucket1 != 12 {
		t.Errorf("%s: anchor 00:02 = %+v, want Count=3 Sum=9 Bucket1=12", query, anchor2)
	}
}

// TestSubqueryHistogramSumOverTime_ChDB proves `sum(<exp-hist
// selector>)[range:step]` — the issue's fourth example — merges two
// series' bucket ladders at each subquery anchor.
func TestSubqueryHistogramSumOverTime_ChDB(t *testing.T) {
	const metric = "subq_sum_exp_hist"
	seed := subqHistDDL +
		"INSERT INTO otel_metrics_exponential_histogram " +
		"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		"    ('" + metric + "', map('job', 'a'), toDateTime64('2026-01-01 00:01:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []),\n" +
		"    ('" + metric + "', map('job', 'b'), toDateTime64('2026-01-01 00:01:00', 9), 5, 1.0, 0, 0, 0, [3], 0, []);\n"
	fixture := newChDBFixture(t, seed)

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	evalTS := time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)

	query := "(sum(" + metric + "))[1m:1m]"
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, evalTS, evalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}

	// sum() drops __name__ and every input label — the merged row's own
	// "series" attribute read by subqHistProjection is therefore absent,
	// so this case reads the row directly rather than through
	// subqHistQueryRows/subqHistRowAt (which key off it).
	projection := "toUnixTimestamp(`TimeUnix`) AS ts, `HistogramCount` AS cnt, `HistogramSum` AS sum, `HistogramPositiveBucketCounts`[1] AS bucket1"
	rows := fixture.queryOverEmitted(t, projection, sqlStr, args)
	defer func() { _ = rows.Close() }()
	var got int
	for rows.Next() {
		var ts int64
		var cnt, sum, bucket1 float64
		if err := rows.Scan(&ts, &cnt, &sum, &bucket1); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got++
		if ts != evalTS.Unix() {
			continue
		}
		if cnt != 7 || sum != 5.0 || bucket1 != 9 {
			t.Errorf("%s: sum() merge = Count=%v Sum=%v Bucket1=%v, want Count=7 Sum=5 Bucket1=9 (2+5, 4.0+1.0, 6+3)", query, cnt, sum, bucket1)
		}
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if got == 0 {
		t.Fatalf("%s: got no rows at all", query)
	}
}

// TestSubqueryHistogramAddMerge_ChDB proves `(<exp-hist> + <exp-hist>)[range:step]`
// — the issue's third example, the ADD/SUB merge shape — under a bare
// top-level subquery.
func TestSubqueryHistogramAddMerge_ChDB(t *testing.T) {
	const metric = "subq_addmerge_exp_hist"
	seed := subqHistDDL +
		"INSERT INTO otel_metrics_exponential_histogram " +
		"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		"    ('" + metric + "', map('series', 'a'), toDateTime64('2026-01-01 00:01:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []);\n"
	fixture := newChDBFixture(t, seed)

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	evalTS := time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)

	query := "(" + metric + " + " + metric + ")[1m:1m]"
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, evalTS, evalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}

	rows := subqHistQueryRows(t, fixture, sqlStr, args)
	if len(rows) == 0 {
		t.Fatalf("%s: got no rows", query)
	}
	got := subqHistRowAt(t, rows, "a", evalTS)
	if got.cnt != 4 || got.sum != 8.0 || got.bucket1 != 12 {
		t.Errorf("%s: merged = %+v, want Count=4 Sum=8 Bucket1=12 (a series added to itself)", query, got)
	}
}

// TestSubqueryHistogramScale_ChDB is the SILENT correctness bug the
// issue reports as the priority half of #2543: before this fix,
// `(<exp-hist> * 2)[range:step]` lowered WITHOUT error — through
// lowerBinary -> lowerScalarBinopOperand -> lowerExpHistogramArgAsCanonicalFloat,
// which treated the histogram-valued LHS as the "preserve family,
// reproject to empty float" case (dropExpHistogramSamples) — to an
// EMPTY result at every subquery anchor, instead of routing through
// expHistogramScalarBinop's actual SCALE semantics. This test asserts
// the scaled, non-empty histogram at TWO distinct subquery anchors.
func TestSubqueryHistogramScale_ChDB(t *testing.T) {
	const metric = "subq_scale_exp_hist"
	seed := subqHistDDL +
		"INSERT INTO otel_metrics_exponential_histogram " +
		"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		"    ('" + metric + "', map('series', 'a'), toDateTime64('2026-01-01 00:01:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []),\n" +
		"    ('" + metric + "', map('series', 'a'), toDateTime64('2026-01-01 00:02:00', 9), 3, 9.0, 0, 0, 0, [12], 0, []);\n"
	fixture := newChDBFixture(t, seed)

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	evalTS := time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC)

	for _, query := range []string{
		"(" + metric + " * 2)[2m:1m]",
		"(2 * " + metric + ")[2m:1m]",
	} {
		t.Run(query, func(t *testing.T) {
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, evalTS, evalTS)
			if err != nil {
				t.Fatalf("LowerAt(%q): %v", query, err)
			}
			sqlStr, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%q): %v", query, err)
			}

			rows := subqHistQueryRows(t, fixture, sqlStr, args)
			if len(rows) != 2 {
				t.Fatalf("%s: got %d rows, want 2 (one per subquery anchor) — a still-silently-empty result would report 0 rows here", query, len(rows))
			}
			anchor1 := subqHistRowAt(t, rows, "a", time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC))
			if anchor1.cnt != 4 || anchor1.sum != 8.0 || anchor1.bucket1 != 12 {
				t.Errorf("%s: anchor 00:01 = %+v, want Count=4 Sum=8 Bucket1=12 (raw Count=2 Sum=4 Bucket1=6, scaled *2)", query, anchor1)
			}
			anchor2 := subqHistRowAt(t, rows, "a", time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC))
			if anchor2.cnt != 6 || anchor2.sum != 18.0 || anchor2.bucket1 != 24 {
				t.Errorf("%s: anchor 00:02 = %+v, want Count=6 Sum=18 Bucket1=24 (raw Count=3 Sum=9 Bucket1=12, scaled *2)", query, anchor2)
			}
		})
	}
}

// TestSubqueryHistogramFloatVectorScalingJoin_ChDB proves the issue's
// own second (and most involved) example: a histogram-valued selector
// scaled by a `group_left()`-joined FLOAT vector — here
// `histogram_quantile(0.5, ...)` over the same metric, mirroring the
// issue's literal trigger query — under a bare top-level subquery.
//
// The expected scale factor is computed by independently lowering and
// executing the plain (non-subquery) `histogram_quantile(0.5, ...)`
// scalar at the SAME eval anchor, rather than hand-deriving the native
// quantile interpolation arithmetic here — any drift between the join's
// own per-row factor and that independently-computed scalar would fail
// the comparison below.
func TestSubqueryHistogramFloatVectorScalingJoin_ChDB(t *testing.T) {
	const metric = "subq_scalejoin_exp_hist"
	seed := subqHistDDL +
		"INSERT INTO otel_metrics_exponential_histogram " +
		"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		"    ('" + metric + "', map('service', 'svc'), toDateTime64('2026-01-01 00:01:00', 9), 10, 10.0, 0, 0, 0, [1, 2, 3, 4], 0, []);\n"
	fixture := newChDBFixture(t, seed)

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	evalTS := time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)

	// Independently compute the expected scale factor: the plain scalar
	// histogram_quantile(0.5, ...) at the same eval anchor, no subquery.
	quantileExpr, err := p.ParseExpr("histogram_quantile(0.5, " + metric + ")")
	if err != nil {
		t.Fatalf("ParseExpr(quantile probe): %v", err)
	}
	quantilePlan, err := promql.LowerAt(context.Background(), quantileExpr, s, evalTS, evalTS)
	if err != nil {
		t.Fatalf("LowerAt(quantile probe): %v", err)
	}
	quantileSQL, quantileArgs, err := chsql.Emit(context.Background(), quantilePlan)
	if err != nil {
		t.Fatalf("Emit(quantile probe): %v", err)
	}
	quantileRows := fixture.queryOverEmitted(t, "`Value` AS val", quantileSQL, quantileArgs)
	var wantFactor float64
	var gotQuantile bool
	for quantileRows.Next() {
		if err := quantileRows.Scan(&wantFactor); err != nil {
			t.Fatalf("scan quantile probe: %v", err)
		}
		gotQuantile = true
	}
	if err := testsql.TolerantRowsErr(quantileRows.Err()); err != nil {
		t.Fatalf("quantile probe rows.Err: %v", err)
	}
	_ = quantileRows.Close()
	if !gotQuantile {
		t.Fatalf("quantile probe returned no rows")
	}

	query := "(" + metric + " * on(service) group_left() histogram_quantile(0.5, " + metric + "))[1m:1m]"
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, evalTS, evalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}

	// The join's match key is the `service` label, not `series` —
	// subqHistProjection's hardcoded `Attributes['series']` lookup
	// (every other case in this file seeds under the `series` key)
	// would read an absent map entry here, so this case projects
	// `Attributes['service']` directly instead.
	projection := "`Attributes`['service'] AS series, toUnixTimestamp(`TimeUnix`) AS ts, " +
		"`HistogramCount` AS cnt, `HistogramSum` AS sum, `HistogramPositiveBucketCounts`[1] AS bucket1"
	joinRows := fixture.queryOverEmitted(t, projection, sqlStr, args)
	defer func() { _ = joinRows.Close() }()
	var rows []subqHistRow
	for joinRows.Next() {
		var r subqHistRow
		if err := joinRows.Scan(&r.series, &r.ts, &r.cnt, &r.sum, &r.bucket1); err != nil {
			t.Fatalf("scan: %v", err)
		}
		rows = append(rows, r)
	}
	if err := testsql.TolerantRowsErr(joinRows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("%s: got no rows", query)
	}
	got := subqHistRowAt(t, rows, "svc", evalTS)
	wantCount := 10 * wantFactor
	wantSum := 10.0 * wantFactor
	wantBucket1 := 1 * wantFactor
	const tol = 1e-9
	if math.Abs(got.cnt-wantCount) > tol || math.Abs(got.sum-wantSum) > tol || math.Abs(got.bucket1-wantBucket1) > tol {
		t.Errorf("%s: scaled = %+v, want Count=%v Sum=%v Bucket1=%v (raw Count=10 Sum=10 Bucket1=1 scaled by the joined quantile %v)",
			query, got, wantCount, wantSum, wantBucket1, wantFactor)
	}
}
