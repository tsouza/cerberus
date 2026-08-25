//go:build chdb

// chDB-backed proof that the eight value-reading date-component functions
// (`year`/`month`/`day_of_month`/`day_of_week`/`day_of_year`/
// `days_in_month`/`hour`/`minute`), directly wrapping a mixed
// float/histogram `or` (cerberus issue #2609,
// histogram_native_mixed_or_datefn.go's
// [dateFnOverMixedExpHistogramSetOp] /
// [lowerDateFnOverMixedExpHistogramSetOp]) actually DROP every
// histogram-shaped row and compute the date component from the
// float-shaped rows alone at real ClickHouse execution — including the
// `or`'s own LHS-wins shadow rule when a histogram row and a float row
// share the identical label signature, and that the composition still
// applies when the date function is reached NESTED under a further
// wrapper rather than only at the query root.
//
// Reuses foSeed / foHistMetric / foFloatMetric / foEvalTS from
// histogram_native_mixed_or_aggregate_float_only_chdb_test.go and
// tkShadowSeed / tkShadowHistMetric / tkShadowFloatMetric from
// histogram_native_mixed_or_aggregate_topk_chdb_test.go (same package,
// same build tag).
package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

// dfSeriesValues runs query and returns the `series` label -> `Value`
// column of every output row.
func dfSeriesValues(t *testing.T, fixture *chdbFixture, s schema.Metrics, p parser.Parser, query string) map[string]float64 {
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
		t.Fatalf("lower(%q): plan root publishes %s, want %s (a date function always drops to a plain float vector over a mixed or)", query, shape, chplan.SampleRowShape)
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

// dfPromDayOfWeek mirrors PromQL's day_of_week (Sun=0..Sat=6), the one
// function among the eight whose numbering diverges from Go's own
// time.Weekday (which already happens to agree: Sunday==0).
func dfPromDayOfWeek(tm time.Time) float64 { return float64(tm.Weekday()) }

// dfDaysInMonth returns the day count of tm's month — the day-of-month of
// that month's last day, matching [dateFnExpr]'s own
// toDayOfMonth(toLastDayOfMonth(...)) lowering.
func dfDaysInMonth(tm time.Time) float64 {
	firstOfNextMonth := time.Date(tm.Year(), tm.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	lastOfMonth := firstOfNextMonth.Add(-24 * time.Hour)
	return float64(lastOfMonth.Day())
}

// TestDateComponentFnsOverMixedSetOpOr_ChDB proves each of the eight
// date-component functions, wrapped directly around a mixed `or`,
// computes its component from the three float-shaped rows (f1=3s, f2=9s,
// f3=1s past the Unix epoch) alone and drops the two histogram-shaped
// rows (h1, h2) entirely — for both source-AST operand orders, since the
// mixed `or`'s shadow rule depends on which side is LHS.
func TestDateComponentFnsOverMixedSetOpOr_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, foSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	floats := map[string]float64{"f1": 3, "f2": 9, "f3": 1}

	fns := []struct {
		name string
		want func(time.Time) float64
	}{
		{"year", func(tm time.Time) float64 { return float64(tm.Year()) }},
		{"month", func(tm time.Time) float64 { return float64(tm.Month()) }},
		{"day_of_month", func(tm time.Time) float64 { return float64(tm.Day()) }},
		{"day_of_year", func(tm time.Time) float64 { return float64(tm.YearDay()) }},
		{"day_of_week", dfPromDayOfWeek},
		{"days_in_month", dfDaysInMonth},
		{"hour", func(tm time.Time) float64 { return float64(tm.Hour()) }},
		{"minute", func(tm time.Time) float64 { return float64(tm.Minute()) }},
	}

	for _, fn := range fns {
		fn := fn
		t.Run(fn.name, func(t *testing.T) {
			want := map[string]float64{}
			for series, v := range floats {
				want[series] = fn.want(time.Unix(int64(v), 0).UTC())
			}

			for _, order := range []struct {
				name  string
				query string
			}{
				{"histLHS", fn.name + "(" + foHistMetric + " or " + foFloatMetric + ")"},
				{"floatLHS", fn.name + "(" + foFloatMetric + " or " + foHistMetric + ")"},
			} {
				order := order
				t.Run(order.name, func(t *testing.T) {
					got := dfSeriesValues(t, fixture, s, p, order.query)
					if len(got) != len(want) {
						t.Fatalf("query %q: got %d series %v, want %d series %v (a histogram-blind bug would leak h1/h2 into the output)", order.query, len(got), got, len(want), want)
					}
					for series, w := range want {
						g, ok := got[series]
						if !ok {
							t.Fatalf("query %q: missing series %q, got %v", order.query, series, got)
						}
						if g != w {
							t.Errorf("query %q: series %q = %v, want %v", order.query, series, g, w)
						}
					}
				})
			}
		})
	}
}

// TestDateComponentFnOverMixedSetOpOr_ShadowCollision_ChDB proves the
// `or`'s own LHS-wins shadow rule composes correctly with a date
// function's histogram drop: when the histogram side is the source-AST
// LHS, the colliding float row is shadowed out of the `or`'s own output
// entirely, so it can never appear in the date function's output. When
// the float side is LHS instead, its own row is never shadowed.
// Reuses tkShadowSeed / tkShadowHistMetric / tkShadowFloatMetric from
// histogram_native_mixed_or_aggregate_topk_chdb_test.go.
func TestDateComponentFnOverMixedSetOpOr_ShadowCollision_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, tkShadowSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{
			"histLHS_dupShadowedOut",
			"day_of_month(" + tkShadowHistMetric + " or " + tkShadowFloatMetric + ")",
			[]string{"solo"},
		},
		{
			"floatLHS_dupSurvives",
			"day_of_month(" + tkShadowFloatMetric + " or " + tkShadowHistMetric + ")",
			[]string{"solo", "dup"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dfSeriesValues(t, fixture, s, p, tc.query)
			if len(got) != len(tc.want) {
				t.Fatalf("query %q: got series %v, want %v", tc.query, got, tc.want)
			}
			for _, series := range tc.want {
				if _, ok := got[series]; !ok {
					t.Errorf("query %q: missing series %q, got %v", tc.query, series, got)
				}
			}
		})
	}
}

// TestDateComponentFnOverMixedSetOpOr_NestedUnderWrapper_ChDB proves the
// composition applies when a date function is reached NESTED under a
// further wrapper, not only at the query root — the "nested wrappers"
// half of the sort()/sort_desc() precedent
// (histogram_native_mixed_or_sort.go, cerberus issue #2605) applied to
// this family. abs() is an arbitrary instant-math wrapper chosen because
// it reads the shadow-resolved float arm's own (already date-transformed)
// Value column, so a histogram row wrongly surviving into it would surface
// as a wrong count or a wrong value.
func TestDateComponentFnOverMixedSetOpOr_NestedUnderWrapper_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, foSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	query := "abs(days_in_month(" + foHistMetric + " or " + foFloatMetric + "))"
	got := dfSeriesValues(t, fixture, s, p, query)

	// f1=3s, f2=9s, f3=1s past the Unix epoch are all still within
	// 1970-01-01 UTC, so every surviving row reports January's 31 days.
	want := map[string]float64{"f1": 31, "f2": 31, "f3": 31}
	if len(got) != len(want) {
		t.Fatalf("query %q: got %v, want %v (a histogram-blind bug would leak h1/h2 into the output)", query, got, want)
	}
	for series, w := range want {
		g, ok := got[series]
		if !ok {
			t.Fatalf("query %q: missing series %q, got %v", query, series, got)
		}
		if g != w {
			t.Errorf("query %q: series %q = %v, want %v", query, series, g, w)
		}
	}
}
