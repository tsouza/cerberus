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

// TestLower_ExpHistogram_MixedSetOpOr_MathFnWrapped pins cerberus issue
// #2449's third wrapper family: a single-arg instant math function
// (abs(), ceil(), sqrt(), ...) directly wrapping a mixed float/histogram
// `or` now lowers successfully instead of falling through to
// internal/promql/binary.go's lowerVectorSetOp rejection ("'or' between
// a float-valued and a histogram-valued operand is not supported").
//
// Unlike label_replace/label_join's composition (which forwards the
// Mixed shape unchanged and still answers MixedRowShape), a math
// function READS Value and reference Prometheus's simpleFloatFunc drops
// every histogram-valued sample — so the result here is the ordinary
// canonical [chplan.SampleRowShape], not MixedRowShape.
func TestLower_ExpHistogram_MixedSetOpOr_MathFnWrapped(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		query string
	}{
		{
			name:  "abs, histogram left",
			query: `abs(demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist))`,
		},
		{
			name:  "abs, float left",
			query: `abs(histogram_quantile(0.5, latency_exp_hist) or latency_exp_hist)`,
		},
		{
			name:  "ceil composes identically",
			query: `ceil(histogram_quantile(0.5, latency_exp_hist) or latency_exp_hist)`,
		},
		{
			name:  "sgn composes identically (Int8 dtype fixup preserved)",
			query: `sgn(histogram_quantile(0.5, latency_exp_hist) or latency_exp_hist)`,
		},
		{
			name:  "round with implicit 1-arg to_nearest composes",
			query: `round(histogram_quantile(0.5, latency_exp_hist) or latency_exp_hist)`,
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
				t.Fatalf("lower(%q): plan root publishes %s, want %s (math fns drop the histogram rows)", tc.query, shape, chplan.SampleRowShape)
			}
			proj, ok := plan.(*chplan.Project)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.Project", tc.query, plan)
			}
			if _, ok := proj.Input.(*chplan.Filter); !ok {
				t.Fatalf("lower(%q): plan root's input is %T, want *chplan.Filter (narrowing to the float-shaped rows)", tc.query, proj.Input)
			}
		})
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_RoundToNearestStillRejects pins
// that round()'s 2-arg to_nearest form is deliberately NOT widened by
// this recognizer — it takes a further bound argument and is lowered by
// lowerRoundToNearest, a different code path this issue's PR did not
// teach the mixed-`or` shape.
func TestLower_ExpHistogram_MixedSetOpOr_RoundToNearestStillRejects(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	query := `round(latency_exp_hist or histogram_quantile(0.5, latency_exp_hist), 5)`
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	if _, err := promql.LowerAt(context.Background(), expr, s, at, at); err == nil {
		t.Fatalf("lower(%q): expected an error, got none", query)
	}
}

// Plain arithmetic over a mixed `or` (`(a or b) + 1`) was pinned as
// staying out of scope here by this file's own
// ArithmeticBinopStillRejects test. Cerberus issue #2449's fourth pass
// (PR that added histogram_native_mixed_or_arithmetic.go) closed that
// gap for the drop-family arithmetic ops (`+`, `-`, `%`, `^`, `atan2`,
// and scalar-left `/`): they now compose over this shape instead of
// rejecting. That behaviour is pinned by
// TestLower_ExpHistogram_MixedSetOpOr_ArithmeticWrapped in
// histogram_native_mixed_or_arithmetic_test.go, which covers this file's
// identical query shape (`(<hist> or histogram_quantile(...)) + 1`), so
// the superseded rejection assertion is removed rather than kept
// alongside a contradictory claim. `*` and histogram-left `/` (which
// SCALE rather than drop) and comparison ops (a structurally different
// Filter/bool-Project lowering) still reject — see that new file's own
// StillRejects tests.
