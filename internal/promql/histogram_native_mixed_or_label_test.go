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

// TestLower_ExpHistogram_MixedSetOpOr_LabelCallWrapped pins cerberus
// issue #2449: `label_replace`/`label_join` directly wrapping a mixed
// float/histogram `or` (histogram_native_mixed_or.go's own #2330/#2335
// shape) now lowers successfully instead of falling through to
// internal/promql/binary.go's lowerVectorSetOp rejection ("'or' between
// a float-valued and a histogram-valued operand is not supported") —
// the first non-aggregation wrapper family to compose over that shape
// since cerberus issue #2346 taught `sum`/`avg` to.
func TestLower_ExpHistogram_MixedSetOpOr_LabelCallWrapped(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		query string
	}{
		{
			name:  "label_replace, histogram left",
			query: `label_replace(demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist), "dst", "x", "service", ".*")`,
		},
		{
			name:  "label_replace, float left",
			query: `label_replace(histogram_quantile(0.5, latency_exp_hist) or latency_exp_hist, "dst", "x", "service", ".*")`,
		},
		{
			name:  "label_join, histogram left",
			query: `label_join(demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist), "dst", ",", "service")`,
		},
		{
			name:  "label_join, float left",
			query: `label_join(histogram_quantile(0.5, latency_exp_hist) or latency_exp_hist, "dst", ",", "service")`,
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
			// The plan root must still answer MixedRowShape — the API
			// layer's wrapWithSampleProjection (internal/api/prom/handler.go)
			// relies on RowShapeOf, not on the root being literally a
			// *chplan.VectorSetOp, to decide the wire projection is
			// already complete. This is the chplan.RowShapeOf *Project
			// case this issue's PR adds.
			if shape := chplan.RowShapeOf(plan); shape != chplan.MixedRowShape {
				t.Fatalf("lower(%q): plan root publishes %s, want %s", tc.query, shape, chplan.MixedRowShape)
			}
			proj, ok := plan.(*chplan.Project)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.Project", tc.query, plan)
			}
			foundDiscriminator := false
			for _, p := range proj.Projections {
				if chplan.ProjectionOutputsColumn(p, chplan.MixedDiscriminatorColumn) {
					foundDiscriminator = true
				}
			}
			if !foundDiscriminator {
				t.Fatalf("lower(%q): plan root's projection list does not name %s", tc.query, chplan.MixedDiscriminatorColumn)
			}
		})
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_LabelReplaceWrapped_WindowedFloatSideComposes
// pins that the label-rewrite composition reuses
// [lowerMixedExpHistogramSetOp] wholesale (unlike the sum/avg
// composition's own bespoke reduction, which cannot yet accept one) —
// so a windowed float arm (cerberus issue #2333) composes here exactly
// as it does for the root-only leaf case, with no extra work from this
// file.
func TestLower_ExpHistogram_MixedSetOpOr_LabelReplaceWrapped_WindowedFloatSideComposes(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	query := `label_replace(rate(up[5m]) or latency_exp_hist, "dst", "x", "service", ".*")`
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
	if err != nil {
		t.Fatalf("LowerAt(%q): unexpected error: %v", query, err)
	}
	if shape := chplan.RowShapeOf(plan); shape != chplan.MixedRowShape {
		t.Fatalf("lower(%q): plan root publishes %s, want %s", query, shape, chplan.MixedRowShape)
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_LabelCallWrapped_NestedInsideAbsStillRejects
// pins that a label_replace-over-mixed-`or` result does NOT itself
// widen into a further generic wrapper: `abs(label_replace(a or b,
// ...))` still falls through to the pre-existing rejection, because
// [labelCallOverMixedExpHistogramSetOp] is registered root-only in
// [lowerRoot], exactly like the leaf ([mixedExpHistogramSetOp]) and
// sum/avg ([sumOrAvgOverMixedExpHistogramSetOp]) recognizers it sits
// beside — nesting a further wrapper around ANY of the three still
// requires that further wrapper to grow its own recognizer, which
// `abs` has not (cerberus issue #2449's own acceptance bar names this
// as the remaining, explicitly tracked divergence in
// test/rejection-parity/catalogue).
func TestLower_ExpHistogram_MixedSetOpOr_LabelCallWrapped_NestedInsideAbsStillRejects(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	query := `abs(label_replace(latency_exp_hist or histogram_quantile(0.5, latency_exp_hist), "dst", "x", "service", ".*"))`
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	if _, err := promql.LowerAt(context.Background(), expr, s, at, at); err == nil {
		t.Fatalf("lower(%q): expected an error, got none", query)
	}
}
