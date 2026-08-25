//go:build chdb

// chDB-backed proof that `topk()`/`bottomk()` directly wrapping a mixed
// float/histogram `or` (cerberus issue #2600,
// histogram_native_mixed_or_aggregate_topk.go's
// [topKOverMixedExpHistogramSetOp] / [lowerTopKOverMixedExpHistogramSetOp])
// actually DROP every histogram-shaped row and rank the float-shaped rows
// alone at real ClickHouse execution — including the `or`'s own
// LHS-wins shadow rule when a histogram row and a float row share the
// identical label signature, and the reference-faithful "no output row
// at all" answer for a `by(...)` group whose only member is
// histogram-shaped.
//
// Reuses foSeed / foHistMetric / foFloatMetric / foEvalTS from
// histogram_native_mixed_or_aggregate_float_only_chdb_test.go (same
// package, same build tag): two histogram series (h1 in bucket b1, h2
// alone in bucket b2) and three float series (f1=3 and f2=9 in bucket
// b1, f3=1 alone in bucket b3).
package promql_test

import (
	"context"
	"math"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

// tkSeriesValues runs query and returns a map of the `series` label to
// `Value`, for queries expected to preserve the topk/bottomk-selected
// series' own labels (no `by`/`without` re-keying).
func tkSeriesValues(t *testing.T, fixture *chdbFixture, s schema.Metrics, p parser.Parser, query string) map[string]float64 {
	t.Helper()
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, foEvalTS, foEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}
	rows := fixture.queryOverEmitted(t, "`Attributes`['series'] AS series, `Value` AS val", sqlStr, args)
	defer func() { _ = rows.Close() }()

	got := map[string]float64{}
	for rows.Next() {
		var series string
		var val float64
		if err := rows.Scan(&series, &val); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[series] = val
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return got
}

func assertSeriesValues(t *testing.T, query string, got, want map[string]float64) {
	t.Helper()
	for series, wantVal := range want {
		gotVal, ok := got[series]
		if !ok {
			t.Errorf("query %q: no row for series %q, want Value=%v (got %v)", query, series, wantVal, got)
			continue
		}
		if gotVal != wantVal {
			t.Errorf("query %q: series %q Value=%v, want %v", query, series, gotVal, wantVal)
		}
	}
	if len(got) != len(want) {
		t.Errorf("query %q: got %d series %v, want %d series %v", query, len(got), got, len(want), want)
	}
}

// TestTopKBottomKOverMixedSetOpOr_ChDB proves topk()/bottomk(), with no
// `by`/`without` clause, ignore the two histogram-shaped rows (h1, h2)
// entirely and rank the three float-shaped rows (f1=3, f2=9, f3=1) alone
// — for both source-AST operand orders, since the mixed `or`'s shadow
// rule (and this composition's shadow-resolution) depends on which side
// is LHS.
func TestTopKBottomKOverMixedSetOpOr_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, foSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	cases := []struct {
		name  string
		query string
		want  map[string]float64
	}{
		{
			"topk2_histLHS",
			"topk(2, " + foHistMetric + " or " + foFloatMetric + ")",
			map[string]float64{"f2": 9, "f1": 3},
		},
		{
			"topk2_floatLHS",
			"topk(2, " + foFloatMetric + " or " + foHistMetric + ")",
			map[string]float64{"f2": 9, "f1": 3},
		},
		{
			"bottomk2_histLHS",
			"bottomk(2, " + foHistMetric + " or " + foFloatMetric + ")",
			map[string]float64{"f3": 1, "f1": 3},
		},
		{
			"bottomk2_floatLHS",
			"bottomk(2, " + foFloatMetric + " or " + foHistMetric + ")",
			map[string]float64{"f3": 1, "f1": 3},
		},
		// K >= the float-row count: every float row survives, still no
		// histogram row.
		{
			"topk10_allFloats",
			"topk(10, " + foHistMetric + " or " + foFloatMetric + ")",
			map[string]float64{"f1": 3, "f2": 9, "f3": 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tkSeriesValues(t, fixture, s, p, tc.query)
			assertSeriesValues(t, tc.query, got, tc.want)
		})
	}
}

// TestTopKOverMixedSetOpOr_ByBucket_ChDB proves `topk(1, ...) by (bucket)`
// partitions the K-selection per bucket, and that bucket b2 — whose ONLY
// member is histogram-shaped (h2) — produces NO output row at all,
// matching reference (aggregationK never adds a histogram sample to any
// group's heap).
func TestTopKOverMixedSetOpOr_ByBucket_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, foSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	query := "topk(1, " + foHistMetric + " or " + foFloatMetric + ") by (bucket)"
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, foEvalTS, foEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}
	rows := fixture.queryOverEmitted(t, "`Attributes`['bucket'] AS bucket, `Value` AS val", sqlStr, args)
	defer func() { _ = rows.Close() }()

	seen := map[string]float64{}
	for rows.Next() {
		var bucket string
		var val float64
		if err := rows.Scan(&bucket, &val); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen[bucket] = val
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	want := map[string]float64{
		"b1": 9, // h1 (ignored) + f1(3) + f2(9) -> top1 = 9
		"b3": 1, // f3 alone
	}
	for bucket, wantVal := range want {
		gotVal, ok := seen[bucket]
		if !ok {
			t.Errorf("query %q: no row for bucket %q, want Value=%v", query, bucket, wantVal)
			continue
		}
		if gotVal != wantVal {
			t.Errorf("query %q: bucket %q Value=%v, want %v", query, bucket, gotVal, wantVal)
		}
	}
	if val, ok := seen["b2"]; ok {
		t.Errorf("query %q: got a row for bucket %q (Value = %v), want none (its only member is histogram-shaped, so reference never marks the group seen)", query, "b2", val)
	}
	if len(seen) != len(want) {
		t.Errorf("query %q: got %d buckets %v, want %d buckets %v", query, len(seen), seen, len(want), want)
	}
}

const (
	tkShadowHistMetric  = "tk_shadow_hist_side_exp_hist"
	tkShadowFloatMetric = "tk_shadow_float_side_gauge"
)

// tkShadowSeed keys a histogram series and a float series onto the
// IDENTICAL label signature ("series"="dup") — the collision case the
// `or`'s own shadow rule (mixedOrShadowUnless) resolves: whichever side
// is the source-AST LHS keeps its "dup" row, and the RHS's "dup" row is
// shadowed out entirely. A second, non-colliding float series ("solo")
// is seeded on both metrics' sides so a non-empty topk always has at
// least one row to select regardless of which arm wins the collision.
var tkShadowSeed = "" +
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
	"    ('" + tkShadowHistMetric + "', map('series', 'dup'), toDateTime64('2026-01-01 00:00:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []);\n" +
	swapGaugeSeedDDL +
	"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
	"    ('" + tkShadowFloatMetric + "', map('series', 'dup'), toDateTime64('2026-01-01 00:00:00', 9), 42.0),\n" +
	"    ('" + tkShadowFloatMetric + "', map('series', 'solo'), toDateTime64('2026-01-01 00:00:00', 9), 7.0);\n"

// TestTopKOverMixedSetOpOr_ShadowCollision_ChDB proves the `or`'s own
// LHS-wins shadow rule composes correctly with topk/bottomk's histogram
// drop: when the histogram side is the source-AST LHS, the colliding
// float row ("dup"=42) is shadowed out of the `or`'s own output
// entirely, so it can never appear in topk's ranking — matching
// reference, where the `or`'s output vector contains ONLY the histogram
// "dup" row for that label signature, which topk/bottomk then drops
// unconditionally, leaving no candidate at all for "dup". When the float
// side is LHS instead, its own "dup" row is never shadowed and topk
// selects it normally.
func TestTopKOverMixedSetOpOr_ShadowCollision_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, tkShadowSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	cases := []struct {
		name  string
		query string
		want  map[string]float64
	}{
		{
			"histLHS_dupShadowedOut",
			"topk(10, " + tkShadowHistMetric + " or " + tkShadowFloatMetric + ")",
			map[string]float64{"solo": 7},
		},
		{
			"floatLHS_dupSurvives",
			"topk(10, " + tkShadowFloatMetric + " or " + tkShadowHistMetric + ")",
			map[string]float64{"solo": 7, "dup": 42},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tkSeriesValues(t, fixture, s, p, tc.query)
			assertSeriesValues(t, tc.query, got, tc.want)
		})
	}
}

// TestQuantileOverMixedSetOpOr_ChDB proves quantile(phi, ...) directly
// wrapping a mixed `or` (cerberus issue #2600, folded into
// histogram_native_mixed_or_aggregate_float_only.go's float-only family)
// ignores the two histogram-shaped rows (h1, h2) and reduces the three
// float-shaped rows (1, 3, 9) alone with reference's own quantile
// semantics — including the out-of-[0,1]-phi ±Inf answer
// [wrapQuantilePhiGuard] applies on top of the CH-native `quantile()`
// aggregate.
func TestQuantileOverMixedSetOpOr_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, foSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	orExpr := "(" + foHistMetric + " or " + foFloatMetric + ")"

	cases := []struct {
		name  string
		query string
		want  float64
	}{
		// sorted floats [1, 3, 9]; rank = phi*(n-1) = 0.5*2 = 1 -> index 1 -> 3.
		{"median", "quantile(0.5, " + orExpr + ")", 3},
		{"min_phi", "quantile(0, " + orExpr + ")", 1},
		{"max_phi", "quantile(1, " + orExpr + ")", 9},
		// PromQL spec (funcQuantile): phi < 0 -> -Inf, phi > 1 -> +Inf,
		// regardless of the underlying values.
		{"phi_below_domain", "quantile(-0.5, " + orExpr + ")", math.Inf(-1)},
		{"phi_above_domain", "quantile(1.5, " + orExpr + ")", math.Inf(1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := foSingleValue(t, fixture, s, p, tc.query)
			if math.IsInf(tc.want, 0) {
				if got != tc.want {
					t.Errorf("%s: Value = %v, want %v", tc.query, got, tc.want)
				}
				return
			}
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("%s: Value = %v, want %v (a histogram-blind bug would include h1/h2's placeholder 0.0 in the reduction)", tc.query, got, tc.want)
			}
		})
	}
}

// tkRowShapeSanity is a compile-time reminder that the mixed-or topk/
// bottomk composition always publishes the canonical Sample row shape
// (never Histogram or Mixed) — the same invariant
// histogram_native_mixed_or_aggregate_float_only_chdb_test.go's foLower
// checks explicitly; this file's tkSeriesValues/foSingleValue helpers
// already lower successfully or fail loudly, so no separate assertion is
// needed, but the referenced type keeps the import honest if that ever
// changes.
var _ = chplan.SampleRowShape
