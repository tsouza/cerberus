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

// TestLower_ExpHistogram_MixedSetOpOr_ScaleWrapped pins cerberus issue
// #2449's sixth wrapper family: `*` (either operand order) and
// histogram-left `/` directly wrapping a mixed float/histogram `or` now
// lower successfully instead of falling through to
// internal/promql/binary.go's lowerVectorSetOp rejection ("'or' between a
// float-valued and a histogram-valued operand is not supported"), and —
// unlike every drop-family wrapper (arithmetic, comparison, math-fn) —
// the plan root STAYS MixedRowShape: these two ops scale both arms'
// payload in place rather than filtering the histogram-shaped rows away.
func TestLower_ExpHistogram_MixedSetOpOr_ScaleWrapped(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		query string
	}{
		{
			name:  "*, histogram-or left, scalar right",
			query: `(demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist)) * 2`,
		},
		{
			name:  "*, scalar left, histogram-or right",
			query: `2 * (demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist))`,
		},
		{
			name:  "/, histogram-or left (histogram-left DIV scales)",
			query: `(demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist)) / 2`,
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
			if shape := chplan.RowShapeOf(plan); shape != chplan.MixedRowShape {
				t.Fatalf("lower(%q): plan root publishes %s, want %s (scaling keeps both arms' rows)", tc.query, shape, chplan.MixedRowShape)
			}
			proj, ok := plan.(*chplan.Project)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.Project", tc.query, plan)
			}
			if len(proj.Projections) != 14 {
				t.Fatalf("lower(%q): plan root projects %d columns, want 14 (the fourteen-column Mixed shape shapeSampleMixed's decode-side scan pins)", tc.query, len(proj.Projections))
			}
			last := proj.Projections[len(proj.Projections)-1]
			if last.Alias != chplan.MixedDiscriminatorColumn {
				t.Fatalf("lower(%q): last projection alias = %q, want %q", tc.query, last.Alias, chplan.MixedDiscriminatorColumn)
			}
			if _, ok := last.Expr.(*chplan.ColumnRef); !ok {
				t.Fatalf("lower(%q): discriminator column is %T, want *chplan.ColumnRef (forwarded unchanged, not recomputed)", tc.query, last.Expr)
			}
			nameProj := proj.Projections[0]
			if nameProj.Alias != s.MetricNameColumn {
				t.Fatalf("lower(%q): first projection alias = %q, want %q", tc.query, nameProj.Alias, s.MetricNameColumn)
			}
			lit, ok := nameProj.Expr.(*chplan.LitString)
			if !ok || lit.V != "" {
				t.Fatalf("lower(%q): __name__ projection = %#v, want an empty LitString (MUL/DIV changes the schema, so __name__ is dropped)", tc.query, nameProj.Expr)
			}
		})
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_ScalarLeftDivStillDropFamily pins
// that `<scalar> / (a or b)` is NOT this recognizer's shape — division is
// not commutative, so scalar-left DIV stays classified as drop-family
// (histogram_native_mixed_or_arithmetic.go) and the histogram side is
// dropped, not scaled: the plan root resolves to the ordinary
// SampleRowShape, not MixedRowShape.
func TestLower_ExpHistogram_MixedSetOpOr_ScalarLeftDivStillDropFamily(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	query := `2 / (demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist))`
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
	if err != nil {
		t.Fatalf("LowerAt(%q): unexpected error: %v", query, err)
	}
	if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
		t.Fatalf("lower(%q): plan root publishes %s, want %s (scalar-left DIV is drop-family, not scale)", query, shape, chplan.SampleRowShape)
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_ScaleVectorVectorStillRejects pins
// that `(a or b) * demo_num_cpus` — a mixed `or` arithmetic-multiplied by
// a PLAIN (non-mixed) vector operand — is still a further, unattempted
// shape. It is NOT [mulOrDivScaleOverMixedExpHistogramSetOp]'s shape
// (that recognizer only matches a scalar LITERAL on exactly one side,
// not a vector), and it is NOT
// histogram_native_mixed_or_vector_arithmetic.go's vector-vector shape
// either — that recognizer requires BOTH operands to themselves be a
// mixed `or` (cerberus issue #2449's own scope statement: "neither is a
// scalar literal" means neither collapses to a scalar, not that either
// side may be an arbitrary plain vector); `demo_num_cpus` is a plain
// selector, not an `or`. A mixed-or-times-plain-vector operand pairing
// remains unimplemented and out of THIS issue's stated scope.
func TestLower_ExpHistogram_MixedSetOpOr_ScaleVectorVectorStillRejects(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	query := `(demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist)) * demo_num_cpus`
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	if _, err := promql.LowerAt(context.Background(), expr, s, at, at); err == nil {
		t.Fatalf("lower(%q): expected an error, got none", query)
	}
}
