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

// TestLower_ExpHistogram_AvgIsHistogramValued pins the fifth
// histogram-VALUED lowering: `avg [by/without] (<exp-histogram
// selector>)` is answerable, and the answer is a
// chplan.HistogramProjection publishing the same thirteen-column
// contract as the bare, `sum()` and `rate()` siblings.
//
// Like `sum()`, the quartet's first slot must be an EMPTY literal:
// reference PromQL drops `__name__` from every aggregation result.
func TestLower_ExpHistogram_AvgIsHistogramValued(t *testing.T) {
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
			query: `avg(latency_exp_hist)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "instant by",
			query: `avg by (service) (latency_exp_hist)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "instant without",
			query: `avg without (pod) (latency_exp_hist{service="api"})`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "parenthesised",
			query: `(avg(latency_exp_hist))`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "range",
			query: `avg by (service) (latency_exp_hist)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, 30*time.Second)
			},
		},
		{
			name:  "range with absolute @ pin",
			query: `avg(latency_exp_hist @ 1767225600)`,
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

// TestLower_ExpHistogram_AvgDividesOnlyTheCountFields is the correctness
// assertion this lowering lives or dies by, and it asserts in BOTH
// directions.
//
// Reference's FloatHistogram.Div scales exactly five fields — ZeroCount,
// Count, Sum and the two signed bucket ladders — and leaves Scale, the
// zero threshold and both bucket OFFSETS alone. Those four describe
// where the buckets sit on the value axis rather than how much fell into
// them, so dividing any of them would slide the distribution sideways
// instead of averaging it.
//
// A plan that divided all nine would still publish thirteen well-typed
// columns in contract order, still decode, and still pass every
// structural check in this file's sibling test — and would answer with a
// silently wrong distribution. So the negative half (four fields
// untouched) is the half that actually catches the bug, and `sum()` is
// checked alongside as the control: the SAME merge with NO division
// anywhere.
func TestLower_ExpHistogram_AvgDividesOnlyTheCountFields(t *testing.T) {
	t.Parallel()

	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// reshapeProjections returns the merged row's projections — the
	// Project directly beneath the histogram cap, where the division (if
	// any) is applied.
	reshapeProjections := func(t *testing.T, s schema.Metrics, query string) map[string]chplan.Expr {
		t.Helper()
		expr, err := p.ParseExpr(query)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", query, err)
		}
		plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
		if err != nil {
			t.Fatalf("LowerAt(%q): %v", query, err)
		}
		hp, ok := plan.(*chplan.HistogramProjection)
		if !ok {
			t.Fatalf("lower(%q): plan root is %T, want *chplan.HistogramProjection", query, plan)
		}
		reshape, ok := hp.Input.(*chplan.Project)
		if !ok {
			t.Fatalf("lower(%q): projection input is %T, want *chplan.Project", query, hp.Input)
		}
		byAlias := make(map[string]chplan.Expr, len(reshape.Projections))
		for _, proj := range reshape.Projections {
			byAlias[proj.Alias] = proj.Expr
		}
		return byAlias
	}

	// dividesBySeriesCount reports whether e is a division by the group's
	// member count, looking through the per-bucket arrayMap the two
	// ladders are scaled through.
	dividesBySeriesCount := func(e chplan.Expr) bool {
		if call, ok := e.(*chplan.FuncCall); ok && call.Name == "arrayMap" && len(call.Args) == 2 {
			lambda, ok := call.Args[0].(*chplan.Lambda)
			if !ok {
				return false
			}
			e = lambda.Body
		}
		div, ok := e.(*chplan.Binary)
		if !ok || div.Op != chplan.OpDiv {
			return false
		}
		ref, ok := div.Right.(*chplan.ColumnRef)
		return ok && ref.Name == "_hq_group_series_count"
	}

	// The default OTel schema does not persist a zero threshold (see
	// schema.DefaultOTelMetrics), so the merge never publishes that
	// column and an assertion about it there would be vacuous. The
	// second schema sets one, which is what makes the "ZeroThreshold is
	// NOT divided" half of this test a real assertion rather than a
	// lookup that silently finds nothing.
	withZeroThreshold := schema.DefaultOTelMetrics()
	withZeroThreshold.ZeroThresholdColumn = "ZeroThreshold"

	for _, s := range []schema.Metrics{schema.DefaultOTelMetrics(), withZeroThreshold} {
		scaled := []string{
			s.CountColumn, s.SumColumn, s.ZeroCountColumn,
			s.PositiveBucketCountsColumn, s.NegativeBucketCountsColumn,
		}
		untouched := []string{
			s.ScaleColumn, s.PositiveOffsetColumn, s.NegativeOffsetColumn,
		}
		if s.ZeroThresholdColumn != "" {
			untouched = append(untouched, s.ZeroThresholdColumn)
		}

		avgProjs := reshapeProjections(t, s, `avg by (service) (latency_exp_hist)`)
		for _, alias := range scaled {
			proj, ok := avgProjs[alias]
			if !ok {
				t.Fatalf("avg publishes no %q projection", alias)
			}
			if !dividesBySeriesCount(proj) {
				t.Fatalf("avg does NOT divide %q by the group's member count (%#v) — "+
					"reference's FloatHistogram.Div scales every count-bearing field", alias, proj)
			}
		}
		for _, alias := range untouched {
			proj, ok := avgProjs[alias]
			if !ok {
				t.Fatalf("avg publishes no %q projection", alias)
			}
			if dividesBySeriesCount(proj) {
				t.Fatalf("avg divides %q, which reference's FloatHistogram.Div leaves ALONE — "+
					"that field is a bucket POSITION, not a count; scaling it moves the "+
					"distribution instead of averaging it", alias)
			}
		}

		// The control: the same merge under `sum()` divides nothing at all.
		sumProjs := reshapeProjections(t, s, `sum by (service) (latency_exp_hist)`)
		for _, alias := range append(append([]string{}, scaled...), untouched...) {
			proj, ok := sumProjs[alias]
			if !ok {
				t.Fatalf("sum publishes no %q projection", alias)
			}
			if dividesBySeriesCount(proj) {
				t.Fatalf("sum divides %q by the group's member count — that is avg's "+
					"arithmetic leaking onto the shared merge", alias)
			}
		}
	}
}

// TestLower_ExpHistogram_AvgMergesBucketLadders pins that `avg()` reaches
// the SAME shared native-histogram merge `sum()` does — the scale-fold +
// offset-align + zero-pad mirroring FloatHistogram.Add — rather than
// reducing the histogram columns some other way.
//
// Without this, an `avg()` that carried ONE member series' ladder
// straight through and divided it by the member count would still
// publish a well-formed, plausibly-scaled histogram.
func TestLower_ExpHistogram_AvgMergesBucketLadders(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(`avg by (service) (latency_exp_hist)`)
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
		"sum(`" + s.CountColumn + "`) AS `" + s.CountColumn + "`",
		"sum(`" + s.SumColumn + "`) AS `" + s.SumColumn + "`",
		"sum(`" + s.ZeroCountColumn + "`) AS `" + s.ZeroCountColumn + "`",
		"bitShiftRight",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("emitted SQL does not reach the shared native-histogram merge: missing %q\n%s", want, sql)
		}
	}
}
