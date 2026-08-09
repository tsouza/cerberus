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
// `_count` / `_sum` companion selectors, and the two top-level
// histogram-VALUED shapes issue #1967 answers — a BARE selector
// (TestLower_ExpHistogram_BareSelectorIsHistogramValued) and `sum()` over
// one (TestLower_ExpHistogram_SumIsHistogramValued) — must fail lowering
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
func TestLower_ExpHistogram_UnsupportedShapesRejectExplicitly(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	cases := []struct {
		name  string
		query string
	}{
		{name: "rate", query: `rate(latency_exp_hist[5m])`},
		{name: "resets", query: `resets(latency_exp_hist[5m])`},
		{name: "changes", query: `changes(latency_exp_hist[5m])`},
		{name: "increase", query: `increase(latency_exp_hist[5m])`},
		{name: "absent_over_time", query: `absent_over_time(latency_exp_hist[5m])`},
		{name: "avg aggregation", query: `avg(latency_exp_hist)`},
		{name: "min aggregation", query: `min(latency_exp_hist)`},
		{name: "max aggregation", query: `max(latency_exp_hist)`},
		{name: "count aggregation", query: `count(latency_exp_hist)`},
		{name: "scalar arithmetic", query: `latency_exp_hist * 2`},
		{name: "parenthesised scalar arithmetic", query: `(latency_exp_hist) + 1`},
		{name: "label_replace", query: `label_replace(latency_exp_hist, "a", "b", "service", "(.*)")`},
		{name: "abs", query: `abs(latency_exp_hist)`},
		{name: "topk", query: `topk(3, latency_exp_hist)`},
		{name: "raw range vector", query: `latency_exp_hist[5m]`},
		{name: "subquery", query: `max_over_time(latency_exp_hist[5m:1m])`},

		// `sum()` over a bare selector IS answered (see
		// TestLower_ExpHistogram_SumIsHistogramValued). These are the
		// shapes the narrowing must NOT reach: a `sum` whose aggregand is
		// not a bare selector, and a `sum` that is not the root.
		{name: "sum over rate", query: `sum(rate(latency_exp_hist[5m]))`},
		{name: "sum over increase", query: `sum(increase(latency_exp_hist[5m]))`},
		{name: "sum of sum", query: `sum(sum(latency_exp_hist))`},
		{name: "sum under arithmetic", query: `sum(latency_exp_hist) + 1`},
		{name: "sum under label_replace", query: `label_replace(sum(latency_exp_hist), "a", "b", "service", "(.*)")`},
		{name: "sum under topk", query: `topk(3, sum by (service) (latency_exp_hist))`},
		{name: "sum under abs", query: `abs(sum(latency_exp_hist))`},
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
