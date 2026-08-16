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

// TestLower_ExpHistogram_UnsupportedShapesRejectExplicitly pins issue #1704:
// every PromQL shape over a pinned exponential-histogram selector OTHER
// than histogram_quantile() / histogram_count() / histogram_sum(), the
// `_count` / `_sum` companion selectors, and the three top-level
// histogram-VALUED shapes issue #1967 answers — a BARE selector
// (TestLower_ExpHistogram_BareSelectorIsHistogramValued), `sum()` over
// one and its `avg()` twin (TestLower_ExpHistogram_SumIsHistogramValued,
// TestLower_ExpHistogram_AvgIsHistogramValued) and `rate()` /
// `increase()` over one
// (TestLower_ExpHistogram_RateIsHistogramValued) — must fail lowering
// with a clear error, never silently resolve against the Gauge/Sum tables
// and return an empty-but-200 result. Before the fix, TablesFor never
// yielded ExpHistogramTable for these shapes, so cerberus quietly scanned
// the wrong tables and matched zero rows.
//
// The nested cases are what keeps the #1967 narrowing HONEST. Each
// answerable shape is answerable because of what sits ABOVE it — nothing,
// for a bare selector; a bucket-ladder merge rather than a `Value` read,
// for `sum()` — so each case here wraps one of them in a consumer that
// does read a `Value` column the histogram row shape never publishes, or
// hands `sum()` an aggregand that is not a bare selector. They stay
// rejected until each grows its own histogram-aware lowering.
//
// `count` is deliberately NOT in that list any more, and the difference
// is reference's own: its aggregation switch reaches `group.groupCount++`
// with no guard on the sample's value and raises no
// HistogramIgnoredInAggregation annotation, because counting a sample
// never has to look at it. That is why it is answered
// (histogram_native_count.go) while its five neighbours are not.
func TestLower_ExpHistogram_UnsupportedShapesRejectExplicitly(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	cases := []struct {
		name  string
		query string
	}{
		{name: "delta", query: `delta(latency_exp_hist[5m])`},
		{name: "irate", query: `irate(latency_exp_hist[5m])`},
		{name: "sum_over_time", query: `sum_over_time(latency_exp_hist[5m])`},
		{name: "absent_over_time", query: `absent_over_time(latency_exp_hist[5m])`},
		{name: "abs", query: `abs(latency_exp_hist)`},
		{name: "raw range vector", query: `latency_exp_hist[5m]`},
		{name: "subquery", query: `max_over_time(latency_exp_hist[5m:1m])`},

		// `sum()` over a bare selector IS answered (see
		// TestLower_ExpHistogram_SumIsHistogramValued). These are the
		// shapes the narrowing must NOT reach: a `sum` whose aggregand is
		// not a bare selector, and a `sum` that is not the root.
		{name: "sum over rate", query: `sum(rate(latency_exp_hist[5m]))`},
		{name: "sum over increase", query: `sum(increase(latency_exp_hist[5m]))`},
		{name: "sum of sum", query: `sum(sum(latency_exp_hist))`},
		{name: "sum under topk", query: `topk(3, sum by (service) (latency_exp_hist))`},
		{name: "sum under abs", query: `abs(sum(latency_exp_hist))`},

		// `rate()` / `increase()` over a range-vector selector ARE answered
		// (see TestLower_ExpHistogram_RateIsHistogramValued). These are the
		// shapes that narrowing must NOT reach: a rate under a consumer that
		// reads a Value, and a rate under an aggregation — which needs the
		// across-series merge stacked on top of the window reduction, so
		// answering it with the window reduction alone would silently drop
		// the sum.
		{name: "rate under abs", query: `abs(rate(latency_exp_hist[5m]))`},
		{name: "rate under topk", query: `topk(3, rate(latency_exp_hist[5m]))`},
		{name: "rate under avg", query: `avg(rate(latency_exp_hist[5m]))`},
		{name: "rate over subquery", query: `rate(latency_exp_hist[5m:1m])`},

		// `resets()` / `changes()` / `count()` over a bare selector ARE
		// answered (see TestLower_ExpHistogram_ResetsChangesCountAreFloatValued).
		// Their answer is an ordinary float sample, so unlike the
		// histogram-valued shapes above nothing about the RESULT stops a
		// consumer from reading it. What keeps these rejected is the
		// ARGUMENT: the selector nested under them still reaches
		// lowerVectorSelector through the ordinary descent, where it is
		// still an exp-histogram selector with no Value column.
		{name: "resets under arithmetic", query: `resets(latency_exp_hist[5m]) * 2`},
		{name: "resets under sum", query: `sum(resets(latency_exp_hist[5m]))`},
		{name: "changes under arithmetic", query: `changes(latency_exp_hist[5m]) + 1`},
		{name: "changes under abs", query: `abs(changes(latency_exp_hist[5m]))`},
		{name: "resets over subquery", query: `resets(latency_exp_hist[5m:1m])`},
		{name: "changes over subquery", query: `changes(latency_exp_hist[5m:1m])`},
		{name: "count under arithmetic", query: `count(latency_exp_hist) + 1`},
		{name: "count of count", query: `count(count(latency_exp_hist))`},
		{name: "count over rate", query: `count(rate(latency_exp_hist[5m]))`},
		{name: "count_values", query: `count_values("v", latency_exp_hist)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			_, err = promql.Lower(context.Background(), expr, s)
			if err == nil {
				t.Fatalf("Lower(%q): expected an explicit rejection error, got nil (silent empty result)", tc.query)
			}
			if !strings.Contains(err.Error(), "latency_exp_hist") {
				t.Fatalf("Lower(%q): error %q does not name the offending metric", tc.query, err.Error())
			}
			if !strings.Contains(err.Error(), "exponential histogram") {
				t.Fatalf("Lower(%q): error %q does not explain the exp-histogram shape mismatch", tc.query, err.Error())
			}
		})
	}
}

// TestLower_ExpHistogram_LabelReplaceIsHistogramValued pins issue #2219:
// label_replace changes only labels, so it must preserve the histogram
// payload and the root HistogramRowShape for every supported
// histogram-valued operand. The guarded Aggregate is part of the contract:
// without it a non-injective rewrite could publish two series with the same
// output label set, which reference Prometheus rejects.
func TestLower_ExpHistogram_LabelReplaceIsHistogramValued(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	for _, tc := range []struct {
		name  string
		query string
		lower func(parser.Expr) (chplan.Node, error)
	}{
		{
			name:  "bare instant",
			query: `label_replace(latency_exp_hist, "copy", "$1-copy", "service", "(.*)")`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "sum instant",
			query: `label_replace(sum by (service) (latency_exp_hist), "copy", "$1-copy", "service", "(.*)")`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "rate range",
			query: `label_replace(rate(latency_exp_hist[5m]), "copy", "$1-copy", "service", "(.*)")`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, time.Minute)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := tc.lower(expr)
			if err != nil {
				t.Fatalf("Lower(%q): %v", tc.query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.HistogramRowShape {
				t.Fatalf("RowShapeOf(lower(%q)) = %s, want %s", tc.query, shape, chplan.HistogramRowShape)
			}
			hp, ok := plan.(*chplan.HistogramProjection)
			if !ok {
				t.Fatalf("lower(%q) root = %T, want *chplan.HistogramProjection", tc.query, plan)
			}
			guard, ok := hp.Input.(*chplan.Aggregate)
			if !ok {
				t.Fatalf("lower(%q) HistogramProjection.Input = %T, want duplicate-labelset *chplan.Aggregate", tc.query, hp.Input)
			}
			if guard.Having == nil {
				t.Fatalf("lower(%q) duplicate-labelset Aggregate has nil Having", tc.query)
			}
			if len(guard.AggFuncs) != 10 {
				t.Fatalf("lower(%q) guard carries %d aggregates, want Value plus nine histogram fields", tc.query, len(guard.AggFuncs))
			}
		})
	}
}

// TestLower_ExpHistogram_CompanionSuffixesStillWork pins the shapes that
// MUST keep resolving after the #1704 fix: histogram_quantile() /
// histogram_count() / histogram_sum() against a bare exp-histogram
// selector, all of which detect the exp-histogram shape via their own
// dedicated lowering before ever reaching the generic vector-selector
// path this fix changed.
func TestLower_ExpHistogram_DedicatedFunctionsStillWork(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	queries := []string{
		`histogram_quantile(0.9, latency_exp_hist)`,
		`histogram_count(latency_exp_hist)`,
		`histogram_sum(latency_exp_hist)`,
	}
	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(q)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", q, err)
			}
			if _, err := promql.Lower(context.Background(), expr, s); err != nil {
				t.Fatalf("Lower(%q): unexpected error: %v", q, err)
			}
		})
	}
}

// TestLower_ExpHistogram_BareSelectorIsHistogramValued pins issue #1967's
// first cut: a bare exp-histogram selector is answerable, and the answer
// is a chplan.HistogramProjection whose output columns are the canonical
// sample quartet followed by the nine chplan.Histogram*Column aliases.
//
// The column ORDER and the leading quartet are the load-bearing part, not
// decoration: internal/chclient's row cursor probes the result set's LAST
// column for HistogramNegativeBucketCounts and then binds thirteen scan
// destinations positionally. A projection that published the nine columns
// in a different order, or that omitted MetricName, would still emit valid
// SQL and still latch the probe — and hand back scrambled histograms.
func TestLower_ExpHistogram_BareSelectorIsHistogramValued(t *testing.T) {
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
			query: `latency_exp_hist`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "instant with matchers",
			query: `latency_exp_hist{service="api"}`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "range",
			query: `latency_exp_hist`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, 30*time.Second)
			},
		},
		{
			name:  "range with absolute @ pin",
			query: `latency_exp_hist @ 1767225600`,
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
			if len(hp.GroupBy) != len(wantAliases) {
				t.Fatalf("lower(%q): %d leading projections for %d aliases", tc.query, len(hp.GroupBy), len(wantAliases))
			}
			if hp.CountColumn != s.CountColumn || hp.SumColumn != s.SumColumn {
				t.Fatalf("lower(%q): Count/Sum bound to %q/%q, want %q/%q",
					tc.query, hp.CountColumn, hp.SumColumn, s.CountColumn, s.SumColumn)
			}
		})
	}
}

// TestLower_ExpHistogram_BareSelectorEmitsThirteenColumns pins the emitted
// SQL's projection width end to end — the property the chclient probe
// depends on and the one a plan-level assertion cannot see, because the
// nine histogram aliases are supplied by the emitter rather than by the
// lowering.
func TestLower_ExpHistogram_BareSelectorEmitsThirteenColumns(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(`latency_exp_hist`)
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
	// The outermost SELECT list, up to the first FROM.
	head, _, ok := strings.Cut(sql, " FROM ")
	if !ok {
		t.Fatalf("emitted SQL has no FROM: %s", sql)
	}
	wantSuffix := []string{
		chplan.HistogramCountColumn, chplan.HistogramSumColumn, chplan.HistogramScaleColumn,
		chplan.HistogramZeroThresholdColumn, chplan.HistogramZeroCountColumn,
		chplan.HistogramPositiveOffsetColumn, chplan.HistogramPositiveBucketCountsColumn,
		chplan.HistogramNegativeOffsetColumn, chplan.HistogramNegativeBucketCountsColumn,
	}
	prev := -1
	for _, alias := range append([]string{s.MetricNameColumn, s.AttributesColumn, s.TimestampColumn, s.ValueColumn}, wantSuffix...) {
		at := strings.Index(head, "AS `"+alias+"`")
		if at < 0 {
			t.Fatalf("emitted projection is missing the %q output alias: %s", alias, head)
		}
		if at <= prev {
			t.Fatalf("emitted projection has %q out of contract order: %s", alias, head)
		}
		prev = at
	}
	last := wantSuffix[len(wantSuffix)-1]
	if !strings.HasSuffix(strings.TrimSpace(head), "AS `"+last+"`") {
		t.Fatalf("emitted projection does not END with %q — chclient's probe reads the LAST column: %s", last, head)
	}
}

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
// it. The three `sum(...)` folds are the scalars that must add across the
// group rather than be picked from it.
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

// TestLower_ExpHistogram_RateIsHistogramValued pins issue #1967's third
// and last cut: `rate(<exp-histogram selector>[range])` and its
// `increase` twin are answerable, and the answer is a
// chplan.HistogramProjection publishing the same thirteen-column
// contract as the bare and `sum()` siblings.
//
// Like `sum()`, and unlike the bare selector, the quartet's first slot
// must be an EMPTY literal: reference PromQL drops `__name__` from every
// range-vector function result too.
func TestLower_ExpHistogram_RateIsHistogramValued(t *testing.T) {
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
			name:  "instant rate",
			query: `rate(latency_exp_hist[5m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "instant increase",
			query: `increase(latency_exp_hist[5m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "instant with matchers",
			query: `rate(latency_exp_hist{service="api"}[10m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "parenthesised",
			query: `(rate(latency_exp_hist[5m]))`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "offset",
			query: `rate(latency_exp_hist[5m] offset 10m)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "range",
			query: `rate(latency_exp_hist[5m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, 30*time.Second)
			},
		},
		{
			name:  "range increase",
			query: `increase(latency_exp_hist[5m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, 30*time.Second)
			},
		},
		{
			name:  "range with absolute @ pin",
			query: `rate(latency_exp_hist[5m] @ 1767225600)`,
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
					"a range-vector function result carries no metric name", tc.query, hp.GroupBy[0])
			}
		})
	}
}

// TestLower_ExpHistogram_RateReachesTheWindowFold pins that `rate()`
// reaches the SHARED exponential-histogram WINDOW fold — the
// counter-reset differencing plus Prometheus's boundary extrapolation —
// rather than reducing the histogram columns some other way.
//
// This is the assertion that would catch the worst plausible bug in this
// lowering: a plan that took each series' NEWEST in-window sample (the
// bare selector's own `argMax(<col>, TimeUnix)` collapse, the easiest
// thing to copy from the sibling file) would still produce a
// HistogramProjection, still emit thirteen columns in contract order, and
// still pass every structural check above — while answering `rate()` with
// a raw cumulative counter reading.
//
// Each marker below is load-bearing:
//
//   - `arrayPopBack` / `arrayPopFront` are counterIncreaseFold's
//     consecutive-pair map, i.e. the counter-reset differencing itself.
//   - `dateDiff` is secondsBetweenTsExpr, which only the
//     boundary-extrapolation factor calls — its absence would mean the
//     window increase ships unextrapolated (the defect #1958 fixed on the
//     classic path).
//   - `groupArray(<Sum>)` is the whole-histogram Sum series this lowering
//     adds; without it the published Sum could only be a single sample's.
//   - `uniqExact` is the two-sample floor, which reference applies per
//     series before emitting anything at all.
//   - the absence of `argMax(<Count>` is the negative half: the bare
//     path's latest-sample collapse must NOT appear on this one.
func TestLower_ExpHistogram_RateReachesTheWindowFold(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(`rate(latency_exp_hist[5m])`)
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
		"arrayPopBack(",
		"arrayPopFront(",
		"dateDiff(",
		"groupArray(`" + s.SumColumn + "`)",
		"groupArray(`" + s.CountColumn + "`)",
		"uniqExact(",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("emitted SQL does not reach the shared native-histogram window fold: missing %q\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "argMax(`"+s.CountColumn+"`") {
		t.Fatalf("emitted SQL collapses to the newest sample per series — that is the BARE selector's "+
			"reduction, not a window rate:\n%s", sql)
	}
}

// TestLower_ExpHistogram_RateDividesByRangeAndIncreaseDoesNot pins the
// one arithmetic difference between the two functions this file answers.
//
// Reference PromQL folds the per-second division into the very scalar the
// boundary extrapolation produces — `factor /= ms.Range.Seconds()` before
// a single `Mul(factor)` over the whole histogram — so `rate` is
// `increase` divided by the range, field for field. The quantile path
// leaves that division out on purpose (a per-series constant cancels out
// of every bucket RATIO), which makes "forgot to divide" the single most
// likely way to ship a plausible-looking wrong answer here: it is
// invisible to every structural assertion and to `histogram_quantile`
// itself, and shows up only as a result 300x too large.
//
// It also pins WHERE the division lands, which is a separate fact from
// whether it happens. Reference divides the factor and then multiplies
// once; dividing the product is the same value in exact arithmetic and a
// different one in float64, and cerberus shipped the latter until the
// histogram-valued compat cases compared bucket counts against reference
// with no epsilon and caught the ulp. Asserting only "a division by 300
// exists somewhere" would have passed on the wrong shape, so the
// assertions below read the multiplication and its factor separately.
//
// The assertion reads the plan rather than the SQL because the divisor is
// a numeric literal, and the emitter parameterises those — `/ ?` in the
// SQL text tells a reader nothing about which number.
func TestLower_ExpHistogram_RateDividesByRangeAndIncreaseDoesNot(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// countProjection returns the window-reduced Count expression, i.e. the
	// projection the histogram row publishes as its total observation count.
	countProjection := func(t *testing.T, query string) chplan.Expr {
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
		for _, proj := range reshape.Projections {
			if proj.Alias == s.CountColumn {
				return proj.Expr
			}
		}
		t.Fatalf("lower(%q): reshape publishes no %q projection", query, s.CountColumn)
		return nil
	}

	const fiveMinutesInSeconds = 300.0

	// dividesByRange reports whether e is a division whose divisor is the
	// range literal.
	dividesByRange := func(e chplan.Expr) bool {
		div, ok := e.(*chplan.Binary)
		if !ok || div.Op != chplan.OpDiv {
			return false
		}
		secs, ok := div.Right.(*chplan.LitFloat)
		return ok && secs.V == fiveMinutesInSeconds
	}
	// countShapes walks a Count projection and reports the two shapes that
	// tell the correct placement from the one that shipped. The walk is
	// needed because the fold binds its arrays through hqLet, so the
	// arithmetic sits inside an arrayMap lambda rather than at the root.
	countShapes := func(e chplan.Expr) (factorDivided, productDivided bool) {
		chplan.InspectExpr(e, func(sub chplan.Expr) bool {
			b, ok := sub.(*chplan.Binary)
			if !ok {
				return true
			}
			// factor divided: Mul(increase, Div(factor, range)) — one
			// multiplication by an already-divided scalar, as reference does.
			if b.Op == chplan.OpMul && dividesByRange(b.Right) {
				factorDivided = true
			}
			// product divided: Div(Mul(...), range) — the inverted order.
			if dividesByRange(b) {
				if _, isMul := b.Left.(*chplan.Binary); isMul && b.Left.(*chplan.Binary).Op == chplan.OpMul {
					productDivided = true
				}
			}
			return true
		})
		return factorDivided, productDivided
	}

	rateFactorDivided, rateProductDivided := countShapes(countProjection(t, `rate(latency_exp_hist[5m])`))
	if !rateFactorDivided {
		t.Fatalf("rate's Count projection never divides the extrapolation FACTOR by %v — "+
			"reference computes `factor /= ms.Range.Seconds()` and then scales the histogram once",
			fiveMinutesInSeconds)
	}
	// Dividing the product instead is the same value in exact arithmetic
	// and a different one in float64 — (a*b)/c != a*(b/c) — which is not
	// cosmetic here: a histogram-valued answer is compared against
	// reference bucket by bucket with NO epsilon, and this exact inversion
	// shipped a divergence the compat corpus caught on
	// rate(demo_shifting_latency_exp_hist[1m]).
	if rateProductDivided {
		t.Fatal("rate's Count projection divides the PRODUCT of the increase and the factor — " +
			"the per-second division belongs on the factor, or the answer drifts an ulp off reference")
	}

	increaseFactorDivided, increaseProductDivided := countShapes(countProjection(t, `increase(latency_exp_hist[5m])`))
	if increaseFactorDivided || increaseProductDivided {
		t.Fatalf("increase's Count projection divides by the range (factor=%v product=%v) — "+
			"increase is the window total, NOT a per-second figure",
			increaseFactorDivided, increaseProductDivided)
	}
}

// TestLower_ExpHistogram_ResetsChangesCountAreFloatValued pins issue
// #1772's last cut: `resets()` / `changes()` over an exp-histogram
// range-vector selector and `count()` over an exp-histogram instant
// selector are answerable, and — unlike every other exp-histogram
// lowering in this package — their answer is an ordinary FLOAT sample.
//
// The row-shape assertion is the load-bearing half. All three reference
// functions return `Sample{F: ...}`, so a plan that capped itself with a
// chplan.HistogramProjection would publish thirteen columns into a
// four-column contract: the wire would render a histogram where Grafana
// expects a number, and every consumer that reads Value would read the
// projection's placeholder instead of the count. That is the single most
// likely way to ship a plausible-looking wrong answer here, because the
// three shapes sit next to three siblings that ARE histogram-valued and
// share their entire window-aggregate stage.
//
// The `__name__` assertion is the same rule its histogram-valued
// siblings pin: reference drops `__name__` from every range-vector
// function result and every aggregation result alike, so the quartet's
// first slot must be an EMPTY literal rather than the MetricName column
// the bare selector carries up.
func TestLower_ExpHistogram_ResetsChangesCountAreFloatValued(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	queries := []string{
		`resets(latency_exp_hist[5m])`,
		`changes(latency_exp_hist[5m])`,
		`resets(latency_exp_hist{service="api"}[10m])`,
		`(changes(latency_exp_hist[5m]))`,
		`resets(latency_exp_hist[5m] offset 10m)`,
		`resets(latency_exp_hist[5m] @ 1767225600)`,
		`count(latency_exp_hist)`,
		`count by (service) (latency_exp_hist)`,
		`count without (pod) (latency_exp_hist{service="api"})`,
		`(count(latency_exp_hist))`,
	}
	modes := []struct {
		name  string
		lower func(parser.Expr) (chplan.Node, error)
	}{
		{name: "instant", lower: func(e parser.Expr) (chplan.Node, error) {
			return promql.LowerAt(context.Background(), e, s, end, end)
		}},
		{name: "range", lower: func(e parser.Expr) (chplan.Node, error) {
			return promql.LowerAtRange(context.Background(), e, s, start, end, 30*time.Second)
		}},
	}

	for _, q := range queries {
		for _, mode := range modes {
			t.Run(q+"/"+mode.name, func(t *testing.T) {
				t.Parallel()
				expr, err := p.ParseExpr(q)
				if err != nil {
					t.Fatalf("ParseExpr(%q): %v", q, err)
				}
				plan, err := mode.lower(expr)
				if err != nil {
					t.Fatalf("lower(%q): unexpected error: %v", q, err)
				}
				if shape := chplan.RowShapeOf(plan); shape == chplan.HistogramRowShape {
					t.Fatalf("lower(%q): plan root publishes a HISTOGRAM row shape — "+
						"reference returns Sample{F: ...} for all three of these, so the answer "+
						"is a four-column float sample, not the thirteen-column contract", q)
				}
				proj, ok := plan.(*chplan.Project)
				if !ok {
					t.Fatalf("lower(%q): plan root is %T, want *chplan.Project", q, plan)
				}
				wantAliases := []string{
					s.MetricNameColumn, s.AttributesColumn, s.TimestampColumn, s.ValueColumn,
				}
				if len(proj.Projections) != len(wantAliases) {
					t.Fatalf("lower(%q): root publishes %d columns, want the %d-column sample quartet",
						q, len(proj.Projections), len(wantAliases))
				}
				for i, want := range wantAliases {
					if got := proj.Projections[i].Alias; got != want {
						t.Fatalf("lower(%q): output column %d is %q, want %q", q, i, got, want)
					}
				}
				name, ok := proj.Projections[0].Expr.(*chplan.LitString)
				if !ok || name.V != "" {
					t.Fatalf("lower(%q): __name__ projection is %#v, want an empty literal — "+
						"reference drops __name__ from a range-vector function and an aggregation alike",
						q, proj.Projections[0].Expr)
				}
				if _, _, err := chsql.Emit(context.Background(), plan); err != nil {
					t.Fatalf("Emit(%q): %v", q, err)
				}
			})
		}
	}
}

// TestLower_ExpHistogram_ResetsAndChangesUseDifferentKernels pins the one
// thing a structural assertion cannot see and the compat corpus would
// only catch on a fixture that happens to distinguish them: `resets()`
// and `changes()` ask DIFFERENT questions of the same samples.
//
// `resets()` transcribes FloatHistogram.DetectReset — a directional
// REGRESSION test, so its comparisons are `<` (and the schema one `>`,
// since only a resolution increase implies a restart). `changes()`
// transcribes !FloatHistogram.Equals — structural whole-row INEQUALITY,
// so its comparisons are `!=` across every stored field.
//
// Swapping the two kernels is the defect this catches, and it is a
// plausible one precisely because both reduce a per-pair mask with the
// same arraySum over the same time-ordered positions. It would be
// invisible to the shape assertions above, and on a monotonically
// growing counter — the common case — `changes()` answered with the
// reset kernel returns 0 where reference returns the sample count.
//
// Sum is the sharpest single discriminator and is asserted both ways:
// reference's DetectReset deliberately does NOT read Sum ("in any
// bucket, in the zero count, or in the count of observations, but NOT
// the sum of observations") while Equals does, so the Sum list must
// appear in exactly one of the two emitted queries.
func TestLower_ExpHistogram_ResetsAndChangesUseDifferentKernels(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	emit := func(t *testing.T, query string) string {
		t.Helper()
		expr, err := p.ParseExpr(query)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", query, err)
		}
		plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
		if err != nil {
			t.Fatalf("LowerAt(%q): %v", query, err)
		}
		sql, _, err := chsql.Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit(%q): %v", query, err)
		}
		return sql
	}

	// The emitter's own spelling of hqWindowSumArrayAlias, which is
	// unexported and so cannot be referenced from this external test
	// package. Pinning the literal is the point: it is the column the
	// two kernels must disagree about.
	const sumListColumn = "`_hq_sum_list`"

	resetsSQL := emit(t, `resets(latency_exp_hist[5m])`)
	changesSQL := emit(t, `changes(latency_exp_hist[5m])`)

	// Both reduce a per-pair mask the same way, over the same pairing.
	for _, want := range []string{"arraySum(", "arrayPopBack(", "arrayPopFront(", "arraySort("} {
		if !strings.Contains(resetsSQL, want) {
			t.Fatalf("resets() SQL is missing %q — it must reduce a per-pair mask:\n%s", want, resetsSQL)
		}
		if !strings.Contains(changesSQL, want) {
			t.Fatalf("changes() SQL is missing %q — it must reduce a per-pair mask:\n%s", want, changesSQL)
		}
	}

	// resets() is the DetectReset kernel: a bucket-ladder regression walk
	// (arrayExists over the rescaled ladders) and no equality anywhere.
	if !strings.Contains(resetsSQL, "arrayExists(") {
		t.Fatalf("resets() SQL never walks the bucket ladders for a regression — "+
			"DetectReset condemns a pair when ANY bucket falls:\n%s", resetsSQL)
	}
	// The verdict's DIRECTION, read off the Count comparison specifically.
	// A blanket scan for "!=" would be meaningless here: the attribute
	// canonicalisation every query carries renders `v != ?`, so the token
	// is present in both SQLs no matter which kernel ran. The two lambdas
	// bind different parameter names (ra/rb vs ca/cb), so these two
	// substrings cannot collide.
	if !strings.Contains(resetsSQL, "`_hq_count_list`[rb] < `_hq_count_list`[ra]") {
		t.Fatalf("resets() does not compare Count as a REGRESSION (`<`) — that is "+
			"DetectReset's first condition:\n%s", resetsSQL)
	}
	if strings.Contains(resetsSQL, "`_hq_count_list`[rb] != `_hq_count_list`[ra]") {
		t.Fatalf("resets() compares Count for INEQUALITY — DetectReset is directional; "+
			"this is changes()'s kernel:\n%s", resetsSQL)
	}
	if strings.Contains(resetsSQL, sumListColumn) {
		t.Fatalf("resets() SQL reads the Sum list — reference's DetectReset deliberately does "+
			"NOT let Sum vote on a reset:\n%s", resetsSQL)
	}

	// changes() is the !Equals kernel: structural inequality over every
	// stored field, Sum included, and no regression walk at all.
	if !strings.Contains(changesSQL, "`_hq_count_list`[cb] != `_hq_count_list`[ca]") {
		t.Fatalf("changes() does not compare Count for INEQUALITY — !Equals is structural "+
			"whole-row inequality:\n%s", changesSQL)
	}
	if strings.Contains(changesSQL, "`_hq_count_list`[cb] < `_hq_count_list`[ca]") {
		t.Fatalf("changes() compares Count as a regression (`<`) — that is resets()'s "+
			"kernel; a histogram that merely GROWS is a change:\n%s", changesSQL)
	}
	if !strings.Contains(changesSQL, sumListColumn) {
		t.Fatalf("changes() SQL never reads the Sum list — FloatHistogram.Equals compares Sum, "+
			"so two samples differing only in Sum are a change:\n%s", changesSQL)
	}
	if strings.Contains(changesSQL, "arrayExists(") {
		t.Fatalf("changes() SQL walks the ladders with arrayExists — that is DetectReset's "+
			"per-bucket regression search, not an equality test:\n%s", changesSQL)
	}
	// The both-NaN carve-out: reference's Equals compares Sum by BIT
	// PATTERN, under which two NaNs are equal, while ClickHouse's `!=`
	// follows IEEE-754 and would report a change at every sample of a
	// NaN-valued series.
	if !strings.Contains(changesSQL, "isNaN(") {
		t.Fatalf("changes() SQL has no both-NaN carve-out on Sum — Equals compares bit "+
			"patterns, so a NaN Sum equals itself:\n%s", changesSQL)
	}
}
