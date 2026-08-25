//go:build chdb

// chDB-backed proof that `sort_by_label()`/`sort_by_label_desc()` directly
// wrapping a mixed float/histogram `or` (cerberus issue #2611,
// histogram_native_mixed_or_sort_by_label.go's
// [sortByLabelArgOverMixedExpHistogramSetOp] /
// [lowerSortByLabelArgOverMixedExpHistogramSetOp]) preserves BOTH
// histogram-shaped and float-shaped rows — the opposite composition from
// sort()/sort_desc() (which drop every histogram-shaped row) — at real
// ClickHouse execution, and that the `or`'s own LHS-wins shadow rule
// composes correctly with that preservation.
//
// Reuses foSeed / foHistMetric / foFloatMetric / foEvalTS from
// histogram_native_mixed_or_aggregate_float_only_chdb_test.go and
// tkShadowSeed / tkShadowHistMetric / tkShadowFloatMetric from
// histogram_native_mixed_or_aggregate_topk_chdb_test.go (same package,
// same build tag).
//
// Deliberately its own [sblSeriesOrder] rather than reusing
// histogram_native_mixed_or_sort_chdb_test.go's [srSeriesOrder]: that
// helper asserts the plan root publishes [chplan.SampleRowShape], which
// holds for sort()/sort_desc() (they always drop to a plain float
// vector) but NOT for sort_by_label() over a mixed `or` — this
// composition's whole point is that the plan root stays
// [chplan.MixedRowShape], preserving both arms.
package promql_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

// sblSeriesOrder runs query and returns the `series` label of every
// output row in emission order, asserting the plan root stays
// [chplan.MixedRowShape] — sort_by_label()'s whole point over a mixed
// `or` is that BOTH arms survive, unlike sort()/sort_desc()'s own
// SampleRowShape guarantee.
func sblSeriesOrder(t *testing.T, fixture *chdbFixture, s schema.Metrics, p parser.Parser, query string) []string {
	t.Helper()
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, foEvalTS, foEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	if shape := chplan.RowShapeOf(plan); shape != chplan.MixedRowShape {
		t.Fatalf("lower(%q): plan root publishes %s, want %s (sort_by_label preserves both arms of a mixed or)", query, shape, chplan.MixedRowShape)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}
	rows := fixture.queryOverEmitted(t, "`Attributes`['series'] AS series", sqlStr, args)
	defer func() { _ = rows.Close() }()

	var order []string
	for rows.Next() {
		var series string
		if err := rows.Scan(&series); err != nil {
			t.Fatalf("scan: %v", err)
		}
		order = append(order, series)
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return order
}

// TestSortByLabelOverMixedSetOpOr_ChDB proves sort_by_label()/
// sort_by_label_desc(), wrapped directly around a mixed `or`, order ALL
// FIVE rows — the three float-shaped rows (f1, f2, f3) AND the two
// histogram-shaped rows (h1, h2) alike — by the natural-sort order of
// their "series" label, for both source-AST operand orders. A
// histogram-blind bug (accidentally dropping h1/h2, mirroring
// sort()/sort_desc()'s OWN correct behaviour) would answer only three
// rows here instead of five.
func TestSortByLabelOverMixedSetOpOr_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, foSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{
			"asc_histLHS",
			`sort_by_label(` + foHistMetric + " or " + foFloatMetric + `, "series")`,
			[]string{"f1", "f2", "f3", "h1", "h2"},
		},
		{
			"asc_floatLHS",
			`sort_by_label(` + foFloatMetric + " or " + foHistMetric + `, "series")`,
			[]string{"f1", "f2", "f3", "h1", "h2"},
		},
		{
			"desc_histLHS",
			`sort_by_label_desc(` + foHistMetric + " or " + foFloatMetric + `, "series")`,
			[]string{"h2", "h1", "f3", "f2", "f1"},
		},
		{
			"desc_floatLHS",
			`sort_by_label_desc(` + foFloatMetric + " or " + foHistMetric + `, "series")`,
			[]string{"h2", "h1", "f3", "f2", "f1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sblSeriesOrder(t, fixture, s, p, tc.query)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("query %q: order = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

// TestSortByLabelOverMixedSetOpOr_ShadowCollision_ChDB proves the `or`'s
// own LHS-wins shadow rule composes correctly with sort_by_label()'s
// preserve-both-arms rule: when the histogram side is the source-AST
// LHS, its "dup" row survives, the float side's own colliding "dup" row
// is shadowed out, and the surviving histogram "dup" row sorts alongside
// the surviving float "solo" row in the SAME result — proving a
// histogram-shaped and a float-shaped row coexist correctly in one
// sorted output, not merely that each shape sorts correctly in
// isolation. When the float side is LHS instead, the float "dup" row
// wins and the histogram "dup" row is shadowed out entirely, leaving two
// float-shaped rows.
func TestSortByLabelOverMixedSetOpOr_ShadowCollision_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, tkShadowSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{
			"histLHS_histDupSurvives_coexistsWithFloatSolo",
			`sort_by_label(` + tkShadowHistMetric + " or " + tkShadowFloatMetric + `, "series")`,
			[]string{"dup", "solo"},
		},
		{
			"floatLHS_floatDupSurvives_histDupShadowedOut",
			`sort_by_label(` + tkShadowFloatMetric + " or " + tkShadowHistMetric + `, "series")`,
			[]string{"dup", "solo"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sblSeriesOrder(t, fixture, s, p, tc.query)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("query %q: order = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}
