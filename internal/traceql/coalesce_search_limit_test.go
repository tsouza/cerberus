package traceql

import (
	"context"
	"slices"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestCoalesceSearchGroupsOnlyTheLimitedTraces pins the statement a search
// ending in coalesce() over a plain-search source lowers to. The coalesce
// aggregate groups the spans of the `limit` newest traces only: its input is a
// SearchTraceLimit with that limit. Grouping every span in the window by span
// identity is what made `{ } | coalesce()` cost several times `{ }`. The
// limited source is the one ranked (no separate Rank), and its span rows read
// the request window alone, so each trace's extent and span count cover the
// same rows as the unlimited aggregate's. A by() before coalesce() keeps every
// span, so coalesce() over it reads the same plain-search source.
func TestCoalesceSearchGroupsOnlyTheLimitedTraces(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	ctx := WithSearchWindow(WithSearchTraceLimit(context.Background(), 20), winStart, winEnd)
	window := []int64{winStart.UnixNano(), winEnd.UnixNano()}
	for _, query := range []string{
		`{ } | coalesce()`,
		`{ name = "b" } | coalesce()`,
		`{ resource.service.name = "a" && kind = server } | coalesce()`,
		`{ rootServiceName = "frontend" } | coalesce()`,
		`{ rootName = name } | coalesce()`,
		`{ } | by(name) | coalesce()`,
	} {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			agg, ok := lowerWithSchema(t, ctx, query, s).(*chplan.Aggregate)
			if !ok || !isCoalesceAggregate(agg, s) {
				t.Fatalf("plan root is %T, want the coalesce aggregate", agg)
			}
			limited, ok := agg.Input.(*chplan.SearchTraceLimit)
			if !ok {
				t.Fatalf("coalesce aggregate reads %T, want a SearchTraceLimit", agg.Input)
			}
			if limited.TraceLimit != 20 || limited.Rank != nil {
				t.Fatalf("SearchTraceLimit{TraceLimit: %d, Rank set: %v}, want {20, false}", limited.TraceLimit, limited.Rank != nil)
			}
			var leaves []leafWindow
			collectLeafWindows(limited, false, s.TimestampColumn, &leaves)
			rows := 0
			for i, leaf := range leaves {
				if leaf.perTrace {
					continue
				}
				rows++
				if !slices.Equal(leaf.instants, window) {
					t.Errorf("span-row leaf %d reads %v, want the request window %v", i, leaf.instants, window)
				}
			}
			if rows == 0 {
				t.Fatal("no span-row leaf under the SearchTraceLimit")
			}
		})
	}
}

// TestCoalesceSearchLimitLeavesOtherShapesUnbounded pins the shapes the
// coalesce bound must not reach. A coalesce() before a scalar filter answers
// the traces the filter keeps, which need not be the newest ones, so ranking
// before the filter would drop answers; the same holds for a scalar filter
// between by() and coalesce(), whose spans are no plain-search source.
// Without a search limit (metrics, trace by ID, the spec harness) nothing is
// bounded.
func TestCoalesceSearchLimitLeavesOtherShapesUnbounded(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	limited := WithSearchWindow(WithSearchTraceLimit(context.Background(), 20), winStart, winEnd)
	unlimited := WithSearchWindow(context.Background(), winStart, winEnd)
	for _, tc := range []struct {
		ctx   context.Context
		query string
	}{
		{limited, `{ } | coalesce() | count() > 1`},
		{limited, `{ } | coalesce() | avg(duration) > 1s`},
		{limited, `{ } | by(name) | count() > 1 | coalesce()`},
		{unlimited, `{ } | coalesce()`},
	} {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			if n := countSearchTraceLimits(lowerWithSchema(t, tc.ctx, tc.query, s)); n != 0 {
				t.Errorf("plan carries %d SearchTraceLimit nodes, want none", n)
			}
		})
	}
}

// countSearchTraceLimits counts the SearchTraceLimit nodes in n, including
// those behind a cohort subquery's Expr slot.
func countSearchTraceLimits(n chplan.Node) int {
	if n == nil {
		return 0
	}
	count := 0
	if _, ok := n.(*chplan.SearchTraceLimit); ok {
		count++
	}
	if f, ok := n.(*chplan.Filter); ok {
		for _, in := range chplan.CohortSubqueries(f.Predicate) {
			count += countSearchTraceLimits(in.Subquery)
		}
	}
	for _, c := range n.Children() {
		count += countSearchTraceLimits(c)
	}
	return count
}
