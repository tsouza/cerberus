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

// TestLower_ExpHistogram_BareMatrixSelectorIsHistogramValued pins cerberus
// issue #2548: a bare TOP-LEVEL range-vector selector
// (`latency_exp_hist[5m]`, no wrapping function) is answerable, and its
// answer is a chplan.HistogramProjection whose Timestamp output is the
// RAW per-row TimeUnix column — not now64() (the instant bare-selector
// shape) and not a grid-anchor column (the range-vector-function /
// subquery shapes) — because reference Prometheus's own "matrix" result
// for this shape preserves every in-window sample's ORIGINAL timestamp.
func TestLower_ExpHistogram_BareMatrixSelectorIsHistogramValued(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	wantAliases := []string{
		s.MetricNameColumn, s.AttributesColumn, s.TimestampColumn, s.ValueColumn,
	}

	for _, tc := range []struct {
		name  string
		query string
	}{
		{name: "bare", query: `latency_exp_hist[5m]`},
		{name: "with matchers", query: `latency_exp_hist{service="api"}[5m]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, end, end)
			if err != nil {
				t.Fatalf("LowerAt(%q): unexpected error: %v", tc.query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.HistogramRowShape {
				t.Fatalf("LowerAt(%q): plan root publishes %s, want histogram", tc.query, shape)
			}
			hp, ok := plan.(*chplan.HistogramProjection)
			if !ok {
				t.Fatalf("LowerAt(%q): plan root is %T, want *chplan.HistogramProjection", tc.query, plan)
			}
			if !slices.Equal(hp.GroupByAliases, wantAliases) {
				t.Fatalf("LowerAt(%q): leading output aliases = %v, want %v", tc.query, hp.GroupByAliases, wantAliases)
			}
			if len(hp.GroupBy) != len(wantAliases) {
				t.Fatalf("LowerAt(%q): %d leading projections for %d aliases", tc.query, len(hp.GroupBy), len(wantAliases))
			}
			tsExpr, ok := hp.GroupBy[2].(*chplan.ColumnRef)
			if !ok || tsExpr.Name != s.TimestampColumn {
				t.Fatalf("LowerAt(%q): Timestamp projection = %#v, want a raw *chplan.ColumnRef{Name: %q} (original per-row timestamp, not now64()/a grid anchor)",
					tc.query, hp.GroupBy[2], s.TimestampColumn)
			}
			if hp.Input == nil {
				t.Fatalf("LowerAt(%q): HistogramProjection.Input is nil", tc.query)
			}
			if _, ok := hp.Input.(*chplan.Aggregate); ok {
				t.Fatalf("LowerAt(%q): HistogramProjection.Input is *chplan.Aggregate — a raw matrix must not collapse per-series samples", tc.query)
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
