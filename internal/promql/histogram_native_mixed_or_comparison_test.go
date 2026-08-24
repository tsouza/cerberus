package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_MixedSetOpOr_ComparisonWrapped pins cerberus
// issue #2449's fifth wrapper family: a scalar comparison binop directly
// wrapping a mixed float/histogram `or` now lowers successfully instead
// of falling through to internal/promql/binary.go's lowerVectorSetOp
// rejection ("'or' between a float-valued and a histogram-valued operand
// is not supported"), for every comparison operator (all six drop the
// histogram-valued sample unconditionally in reference Prometheus,
// regardless of the `bool` modifier).
func TestLower_ExpHistogram_MixedSetOpOr_ComparisonWrapped(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		query string
	}{
		{
			name:  ">, histogram left, scalar right, no bool",
			query: `(demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist)) > 1`,
		},
		{
			name:  "<, scalar left, histogram right, no bool",
			query: `1 < (demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist))`,
		},
		{
			name:  "==, with bool",
			query: `(demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist)) == bool 1`,
		},
		{
			name:  "!=, with bool",
			query: `(demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist)) != bool 1`,
		},
		{
			name:  "<=",
			query: `(demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist)) <= 1`,
		},
		{
			name:  ">=, with bool",
			query: `(demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist)) >= bool 1`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt(%q): unexpected error: %v", tc.query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
				t.Fatalf("lower(%q): plan root publishes %s, want %s (comparisons drop the histogram rows)", tc.query, shape, chplan.SampleRowShape)
			}
			proj, ok := plan.(*chplan.Project)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.Project", tc.query, plan)
			}
			if _, ok := proj.Input.(*chplan.Filter); !ok {
				t.Fatalf("lower(%q): plan root's input is %T, want *chplan.Filter", tc.query, proj.Input)
			}
		})
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_ComparisonNoBoolPreservesName pins
// the non-`bool` filter path's name-preservation rule: unlike every other
// mixed-`or` wrapper in this package (which derives a new sample and
// drops `__name__`), a bare comparison FILTERS rather than transforms, so
// the surviving float side's own `__name__` is forwarded unchanged —
// mirroring internal/promql/binary.go's [lowerVectorScalar] doc comment
// on the identical rule for a non-mixed vector-scalar comparison.
func TestLower_ExpHistogram_MixedSetOpOr_ComparisonNoBoolPreservesName(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	query := `(demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist)) > 1`
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
	if err != nil {
		t.Fatalf("LowerAt(%q): unexpected error: %v", query, err)
	}
	proj, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("lower(%q): plan root is %T, want *chplan.Project", query, plan)
	}
	if len(proj.Projections) == 0 {
		t.Fatalf("lower(%q): plan root has no projections", query)
	}
	nameProj := proj.Projections[0]
	if _, isLit := nameProj.Expr.(*chplan.LitString); isLit {
		t.Fatalf("lower(%q): MetricName projection is a literal (name dropped); want the underlying column forwarded (comparison filters, does not transform)", query)
	}
	if ref, ok := nameProj.Expr.(*chplan.ColumnRef); !ok || ref.Name != s.MetricNameColumn {
		t.Fatalf("lower(%q): MetricName projection = %#v, want ColumnRef(%s)", query, nameProj.Expr, s.MetricNameColumn)
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_ComparisonBoolDropsName pins the
// `bool`-modified path's opposite rule: the comparison now derives a new
// 1.0/0.0 sample, so `__name__` is dropped exactly like every other
// value-deriving mixed-`or` wrapper (arithmetic, math fns).
func TestLower_ExpHistogram_MixedSetOpOr_ComparisonBoolDropsName(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	query := `(demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist)) > bool 1`
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
	if err != nil {
		t.Fatalf("LowerAt(%q): unexpected error: %v", query, err)
	}
	proj, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("lower(%q): plan root is %T, want *chplan.Project", query, plan)
	}
	if len(proj.Projections) == 0 {
		t.Fatalf("lower(%q): plan root has no projections", query)
	}
	nameProj := proj.Projections[0]
	lit, isLit := nameProj.Expr.(*chplan.LitString)
	if !isLit || lit.V != "" {
		t.Fatalf("lower(%q): MetricName projection = %#v, want LitString(\"\") (bool comparison derives a new sample)", query, nameProj.Expr)
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_ComparisonVectorVectorNowComposes
// pins that a vector-vector comparison over a mixed `or` where the OTHER
// operand is a plain, non-mixed vector (`(a or b) > other_metric`,
// neither side a scalar literal) — this recognizer only matches a scalar
// on exactly one side, the identical exclusion
// arithmeticOverMixedExpHistogramSetOp makes for vector-vector arithmetic
// — now composes via cerberus issue #2449's tenth and final wrapper
// family, histogram_native_mixed_or_vector_plain_comparison.go. See that
// file's own test coverage
// (histogram_native_mixed_or_vector_plain_comparison_test.go) for the
// full shape pinning; this test only confirms the trigger query this
// recognizer's own doc used to cite as still-rejected now lowers.
func TestLower_ExpHistogram_MixedSetOpOr_ComparisonVectorVectorNowComposes(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	query := `(demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist)) > demo_num_cpus`
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	if _, err := promql.LowerAt(context.Background(), expr, s, at, at); err != nil {
		t.Fatalf("lower(%q): unexpected error: %v", query, err)
	}
}
