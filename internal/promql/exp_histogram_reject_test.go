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
// `_count` / `_sum` companion selectors, and a top-level BARE selector
// (issue #1967 — see TestLower_ExpHistogram_BareSelectorIsHistogramValued)
// must fail lowering with a clear error, never silently resolve against
// the Gauge/Sum tables and return an empty-but-200 result. Before the fix,
// TablesFor never yielded ExpHistogramTable for these shapes, so cerberus
// quietly scanned the wrong tables and matched zero rows.
//
// The nested cases are what keeps the #1967 narrowing HONEST: a bare
// selector is answerable because nothing consumes it, and each of these
// wraps that same selector in a consumer that reads a `Value` column the
// histogram row shape does not publish. They stay rejected until each
// grows its own histogram-aware lowering.
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
		{name: "sum aggregation", query: `sum(latency_exp_hist)`},
		{name: "sum by", query: `sum by (service) (latency_exp_hist)`},
		{name: "absent_over_time", query: `absent_over_time(latency_exp_hist[5m])`},
		{name: "avg aggregation", query: `avg(latency_exp_hist)`},
		{name: "scalar arithmetic", query: `latency_exp_hist * 2`},
		{name: "parenthesised scalar arithmetic", query: `(latency_exp_hist) + 1`},
		{name: "label_replace", query: `label_replace(latency_exp_hist, "a", "b", "service", "(.*)")`},
		{name: "abs", query: `abs(latency_exp_hist)`},
		{name: "topk", query: `topk(3, latency_exp_hist)`},
		{name: "raw range vector", query: `latency_exp_hist[5m]`},
		{name: "subquery", query: `max_over_time(latency_exp_hist[5m:1m])`},
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
