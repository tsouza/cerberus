package chsql

import (
	"errors"
	"testing"
)

// TestNativeTSAggregateOverTableIsRefused pins errNativeTSAggregateOverTable
// in both directions: every shape where a native timeSeries* aggregate shares
// a SELECT level with a physical table read — a plain table, a merge()
// union, a JOIN — fails the render, and every shape where the aggregate reads
// a subquery, or the table-reading level calls no native aggregate, renders.
func TestNativeTSAggregateOverTableIsRefused(t *testing.T) {
	grid := func(fn string) Frag {
		return Parametric(fn, []Frag{InlineLit(int64(0)), InlineLit(int64(60)), InlineLit(int64(60)), InlineLit(int64(60))},
			Col("TimeUnix"), Col("Value"))
	}
	table := func() Frag { return physicalTableFrag("otel_metrics_sum") }
	union := func() Frag {
		return countPhysicalScans(2, mergeTableFrag("", []string{"otel_metrics_sum", "otel_metrics_gauge"}))
	}
	rows := func(src Frag) Frag { return NewQuery().From(src).Frag() }

	cases := []struct {
		name   string
		query  *QueryBuilder
		refuse bool
	}{
		{"aggregate over a table", NewQuery().Select(grid("timeSeriesRateToGrid")).From(table()), true},
		{"aggregate over merge()", NewQuery().Select(grid("timeSeriesRateToGrid")).From(union()), true},
		{"-State over a table", NewQuery().Select(grid("timeSeriesRateToGridState")).From(table()), true},
		{"non-parametric aggregate over a table", NewQuery().Select(Call("timeSeriesLastTwoSamplesMerge", Col("s"))).From(table()), true},
		{"aggregate joined to a table", NewQuery().Select(grid("timeSeriesDeltaToGrid")).From(rows(table())).
			Join(InnerJoin, table(), Eq(Col("a"), Col("b"))), true},
		{
			"-Merge over a subquery whose -State level reads a table",
			NewQuery().Select(Parametric("timeSeriesRateToGridMerge", []Frag{InlineLit(int64(0))}, Col("s"))).
				From(NewQuery().Select(grid("timeSeriesRateToGridState")).From(table()).Frag()),
			true,
		},
		{"aggregate over a subquery of a table", NewQuery().Select(grid("timeSeriesRateToGrid")).From(rows(table())), false},
		{"aggregate over a subquery of merge()", NewQuery().Select(grid("timeSeriesRateToGrid")).From(rows(union())), false},
		{
			"-Merge over -State over a subquery",
			NewQuery().Select(Parametric("timeSeriesRateToGridMerge", []Frag{InlineLit(int64(0))}, Col("s"))).
				From(NewQuery().Select(grid("timeSeriesRateToGridState")).From(rows(table())).Frag()),
			false,
		},
		{"scalar timeSeriesRange over a table", NewQuery().Select(Call("timeSeriesRange", Col("a"), Col("b"), Col("c"))).From(table()), false},
		{"table read with the aggregate one level up", NewQuery().Select(grid("timeSeriesRateToGrid")).
			From(NewQuery().Select(Col("TimeUnix"), Col("Value")).From(table()).Where(Eq(Col("a"), Col("b"))).Frag()), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sql, _, err := tc.query.subquerySQL()
			if got := errors.Is(err, errNativeTSAggregateOverTable); got != tc.refuse {
				t.Fatalf("refused=%v, want %v (err=%v)\n%s", got, tc.refuse, err, sql)
			}
			if !tc.refuse && err != nil {
				t.Fatalf("unexpected render error: %v\n%s", err, sql)
			}
		})
	}
}

// TestIsNativeTSAggregate pins the name classification: the aggregates and
// their combinator forms match, the ordinary timeSeries* functions do not.
func TestIsNativeTSAggregate(t *testing.T) {
	for _, name := range []string{
		"timeSeriesRateToGrid", "timeSeriesRateToGridState", "timeSeriesRateToGridMerge",
		"timeSeriesGroupArrayIf", "timeSeriesLastTwoSamplesMerge", "timeSeriesResampleToGridWithStaleness",
	} {
		if !isNativeTSAggregate(name) {
			t.Errorf("isNativeTSAggregate(%q) = false, want true", name)
		}
	}
	for _, name := range []string{
		"timeSeriesRange", "timeSeriesFromGrid", "timeSeriesTagsToGroup",
		"timeSeriesThrowDuplicateSeriesIf", "groupArray", "max",
	} {
		if isNativeTSAggregate(name) {
			t.Errorf("isNativeTSAggregate(%q) = true, want false", name)
		}
	}
}
