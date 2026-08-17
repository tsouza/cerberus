package promql_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_SumIsHistogramValued pins issue #1967's second
// cut: `sum [by/without] (<exp-histogram selector>)` is answerable, and
// the answer is a chplan.HistogramProjection publishing the same
// thirteen-column contract as the bare sibling.
//
// The `__name__` assertion is the load-bearing difference from the bare
// path and the one a reader is most likely to get wrong by copying it:
// reference PromQL drops `__name__` from every aggregation result, so the
// quartet's first slot must be an EMPTY literal, not the MetricName
// column the bare path carries up. Reading the column here would report a
// merged series under whichever member's stored name argMax happened to
// pick.
func TestLower_ExpHistogram_SumIsHistogramValued(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	cases := []struct {
		name  string
		query string
		lower func(parser.Expr) (chplan.Node, error)
	}{
		{
			name:  "instant",
			query: `sum(latency_exp_hist)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "instant by",
			query: `sum by (service) (latency_exp_hist)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "instant without",
			query: `sum without (pod) (latency_exp_hist{service="api"})`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "parenthesised",
			query: `(sum(latency_exp_hist))`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "range",
			query: `sum by (service) (latency_exp_hist)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, 30*time.Second)
			},
		},
		{
			name:  "range with absolute @ pin",
			query: `sum(latency_exp_hist @ 1767225600)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, 30*time.Second)
			},
		},
	}

	wantAliases := []string{
		s.MetricNameColumn, s.AttributesColumn, s.TimestampColumn, s.ValueColumn,
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := tc.lower(expr)
			if err != nil {
				t.Fatalf("lower(%q): unexpected error: %v", tc.query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.HistogramRowShape {
				t.Fatalf("lower(%q): plan root publishes %s, want histogram", tc.query, shape)
			}
			hp, ok := plan.(*chplan.HistogramProjection)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.HistogramProjection", tc.query, plan)
			}
			if !slices.Equal(hp.GroupByAliases, wantAliases) {
				t.Fatalf("lower(%q): leading output aliases = %v, want %v", tc.query, hp.GroupByAliases, wantAliases)
			}
			name, ok := hp.GroupBy[0].(*chplan.LitString)
			if !ok || name.V != "" {
				t.Fatalf("lower(%q): __name__ projection is %#v, want an empty literal — "+
					"an aggregation result carries no metric name", tc.query, hp.GroupBy[0])
			}
		})
	}
}

// TestLower_ExpHistogram_SumMergesBucketLadders pins that `sum()` reaches
// the SHARED native-histogram merge — the scale-fold + offset-align +
// zero-pad that mirrors Prometheus's FloatHistogram.Add — rather than
// reducing the histogram columns some other way.
//
// This is the assertion that would catch the worst plausible bug in this
// lowering: a plan that carried ONE member series' ladder straight
// through (an argMax, say, or a groupArray never folded) would still
// produce a HistogramProjection, still emit thirteen columns in contract
// order, and still pass every structural check above — while silently
// answering `sum()` with one of its operands.
//
// `min(Scale)` is the fold's signature: it is the merged scale every
// per-row ladder is downshifted to, and nothing else in the plan emits
// it. The three groupArray + arrayFold paths are the scalars that must add
// across the group with Prometheus's compensated histogram summation rather
// than be picked from it or fed through ClickHouse's plain sum aggregate.
func TestLower_ExpHistogram_SumMergesBucketLadders(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(`sum by (service) (latency_exp_hist)`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
	if err != nil {
		t.Fatalf("LowerAt: %v", err)
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, want := range []string{
		"min(`" + s.ScaleColumn + "`)",
		"groupArray(`" + s.CountColumn + "`) AS `_hq_merge_counts`",
		"groupArray(`" + s.SumColumn + "`) AS `_hq_merge_sums`",
		"groupArray(`" + s.ZeroCountColumn + "`) AS `_hq_merge_zero_counts`",
		"arrayFold(",
		"bitShiftRight",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("emitted SQL does not reach the shared native-histogram merge: missing %q\n%s", want, sql)
		}
	}
}
