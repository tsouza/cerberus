//go:build chdb

// chDB-backed proof that `sort()`/`sort_desc()` directly wrapping a mixed
// float/histogram `or` (cerberus issue #2605,
// histogram_native_mixed_or_sort.go's [sortOverMixedExpHistogramSetOp] /
// [lowerSortOverMixedExpHistogramSetOp]) actually DROP every
// histogram-shaped row and order the float-shaped rows alone at real
// ClickHouse execution — including the `or`'s own LHS-wins shadow rule
// when a histogram row and a float row share the identical label
// signature, and that the composition still applies when `sort()` is
// reached NESTED under a further wrapper rather than only at the query
// root.
//
// Reuses foSeed / foHistMetric / foFloatMetric / foEvalTS from
// histogram_native_mixed_or_aggregate_float_only_chdb_test.go and
// tkShadowSeed / tkShadowHistMetric / tkShadowFloatMetric from
// histogram_native_mixed_or_aggregate_topk_chdb_test.go (same package,
// same build tag).
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

// srSeriesOrder runs query and returns the `series` label of every output
// row IN THE ORDER the emitted SQL returns them, for queries expected to
// preserve row order (sort()/sort_desc()'s whole point).
func srSeriesOrder(t *testing.T, fixture *chdbFixture, s schema.Metrics, p parser.Parser, query string) []string {
	t.Helper()
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, foEvalTS, foEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
		t.Fatalf("lower(%q): plan root publishes %s, want %s (sort/sort_desc always drop to a plain float vector over a mixed or)", query, shape, chplan.SampleRowShape)
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

// TestSortSortDescOverMixedSetOpOr_ChDB proves sort()/sort_desc(), wrapped
// directly around a mixed `or`, order the three float-shaped rows
// (f1=3, f2=9, f3=1) alone by Value and drop the two histogram-shaped
// rows (h1, h2) entirely — for both source-AST operand orders, since the
// mixed `or`'s shadow rule depends on which side is LHS.
func TestSortSortDescOverMixedSetOpOr_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, foSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{
			"sort_histLHS",
			"sort(" + foHistMetric + " or " + foFloatMetric + ")",
			[]string{"f3", "f1", "f2"},
		},
		{
			"sort_floatLHS",
			"sort(" + foFloatMetric + " or " + foHistMetric + ")",
			[]string{"f3", "f1", "f2"},
		},
		{
			"sort_desc_histLHS",
			"sort_desc(" + foHistMetric + " or " + foFloatMetric + ")",
			[]string{"f2", "f1", "f3"},
		},
		{
			"sort_desc_floatLHS",
			"sort_desc(" + foFloatMetric + " or " + foHistMetric + ")",
			[]string{"f2", "f1", "f3"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := srSeriesOrder(t, fixture, s, p, tc.query)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("query %q: order = %v, want %v (a histogram-blind bug would include h1/h2's placeholder 0.0 in the ordering)", tc.query, got, tc.want)
			}
		})
	}
}

// TestSortOverMixedSetOpOr_ShadowCollision_ChDB proves the `or`'s own
// LHS-wins shadow rule composes correctly with sort()'s histogram drop:
// when the histogram side is the source-AST LHS, the colliding float row
// ("dup"=42) is shadowed out of the `or`'s own output entirely, so it can
// never appear in sort()'s output. When the float side is LHS instead,
// its own "dup" row is never shadowed and sort() ranks it normally.
// Reuses tkShadowSeed / tkShadowHistMetric / tkShadowFloatMetric from
// histogram_native_mixed_or_aggregate_topk_chdb_test.go.
func TestSortOverMixedSetOpOr_ShadowCollision_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, tkShadowSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{
			"histLHS_dupShadowedOut",
			"sort(" + tkShadowHistMetric + " or " + tkShadowFloatMetric + ")",
			[]string{"solo"},
		},
		{
			"floatLHS_dupSurvives",
			"sort(" + tkShadowFloatMetric + " or " + tkShadowHistMetric + ")",
			[]string{"solo", "dup"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := srSeriesOrder(t, fixture, s, p, tc.query)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("query %q: order = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

// TestSortOverMixedSetOpOr_NestedUnderWrapper_ChDB proves the composition
// applies when sort()/sort_desc() is reached NESTED under a further
// wrapper, not only at the query root — the "nested wrappers" half of
// cerberus issue #2605's title. abs() is an arbitrary instant-math
// wrapper chosen because it reads the shadow-resolved float arm's own
// Value column, so a histogram row wrongly surviving into it would
// surface as a wrong count or a wrong value rather than merely a wrong
// order.
func TestSortOverMixedSetOpOr_NestedUnderWrapper_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, foSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	query := "abs(sort_desc(" + foHistMetric + " or " + foFloatMetric + "))"
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
	var order []string
	for rows.Next() {
		var series string
		var val float64
		if err := rows.Scan(&series, &val); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[series] = val
		order = append(order, series)
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	wantOrder := []string{"f2", "f1", "f3"}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Errorf("query %q: order = %v, want %v", query, order, wantOrder)
	}
	want := map[string]float64{"f1": 3, "f2": 9, "f3": 1}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("query %q: values = %v, want %v (a histogram-blind bug would include h1/h2's placeholder 0.0)", query, got, want)
	}
}
