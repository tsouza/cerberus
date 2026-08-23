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

// TestLower_ExpHistogram_MixedSetOpOr_ArithmeticWrapped pins cerberus
// issue #2449's fourth wrapper family, and the issue's own originally-
// named example: a scalar arithmetic binop directly wrapping a mixed
// float/histogram `or` now lowers successfully instead of falling
// through to internal/promql/binary.go's lowerVectorSetOp rejection
// ("'or' between a float-valued and a histogram-valued operand is not
// supported"), for the drop-family ops (ADD, SUB, POW, MOD, ATAN2, and
// scalar-left DIV).
//
// Like the math-fn composition (#2479) and unlike label_replace/
// label_join's (which forwards the Mixed shape unchanged), an
// arithmetic op READS Value and reference Prometheus drops every
// histogram-valued sample for this op family — so the result here is
// the ordinary canonical [chplan.SampleRowShape], not MixedRowShape.
func TestLower_ExpHistogram_MixedSetOpOr_ArithmeticWrapped(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		query string
	}{
		{
			name:  "+, histogram left, scalar right — the issue's own named example",
			query: `(demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist)) + 1`,
		},
		{
			name:  "+, scalar left, histogram right",
			query: `1 + (demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist))`,
		},
		{
			name:  "-, float left",
			query: `(histogram_quantile(0.5, latency_exp_hist) or latency_exp_hist) - 5`,
		},
		{
			name:  "%",
			query: `(demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist)) % 3`,
		},
		{
			name:  "^",
			query: `(demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist)) ^ 2`,
		},
		{
			name:  "atan2",
			query: `(demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist)) atan2 1`,
		},
		{
			name:  "scalar-left / (drop family; histogram-left / scales instead)",
			query: `1 / (demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist))`,
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
				t.Fatalf("lower(%q): plan root publishes %s, want %s (drop-family arithmetic drops the histogram rows)", tc.query, shape, chplan.SampleRowShape)
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

// TestLower_ExpHistogram_MixedSetOpOr_MulStillRejects pins that `*` —
// which SCALES the histogram side rather than dropping it (reference's
// `hlhs.Copy().Mul(rhs)`) — is deliberately NOT widened by this
// recognizer: it would need the histogram side's own nine
// Histogram*Column outputs actually scaled, a materially different
// lowering from the single float Project this file's recognizer builds.
// Tracked by test/rejection-parity/catalogue's rotated trigger under
// cerberus issue #2449.
func TestLower_ExpHistogram_MixedSetOpOr_MulStillRejects(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	query := `(demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist)) * 2`
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	_, err = promql.LowerAt(context.Background(), expr, s, at, at)
	if err == nil {
		t.Fatalf("lower(%q): expected an error, got none", query)
	}
	const want = "promql: 'or' between a float-valued and a histogram-valued operand is not supported; 'and'/'unless' support mixing them"
	if err.Error() != want {
		t.Fatalf("lower(%q): error = %q, want %q", query, err.Error(), want)
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_HistogramLeftDivStillRejects pins
// the same scaling exclusion for histogram-left `/` (reference's
// `hlhs.Copy().Div(rhs)`) — only scalar-left `/` (histogram on the
// right, denominator drop) is in this recognizer's drop family.
func TestLower_ExpHistogram_MixedSetOpOr_HistogramLeftDivStillRejects(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	query := `(demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist)) / 2`
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	if _, err := promql.LowerAt(context.Background(), expr, s, at, at); err == nil {
		t.Fatalf("lower(%q): expected an error, got none", query)
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_ComparisonNowComposes pins that
// comparison ops — which [expHistogramScalarOpDropsSample] classifies as
// "drop" too, and which internal/promql/binary.go's [lowerVectorScalar]
// answers through a structurally different Filter / bool-Project shape,
// not the single arithmetic Project this recognizer builds — now compose
// via their own sibling recognizer, histogram_native_mixed_or_comparison.go
// (cerberus issue #2449's fifth wrapper family). See that file's header.
func TestLower_ExpHistogram_MixedSetOpOr_ComparisonNowComposes(t *testing.T) {
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
	if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
		t.Fatalf("lower(%q): plan root publishes %s, want %s", query, shape, chplan.SampleRowShape)
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_VectorVectorArithmeticStillRejects
// pins that a vector-vector arithmetic binop over a mixed `or`
// (`(a or b) + other_metric`, neither side a scalar literal) is a
// further unattempted shape — this recognizer only matches a scalar on
// exactly one side.
func TestLower_ExpHistogram_MixedSetOpOr_VectorVectorArithmeticStillRejects(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	query := `(demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist)) + demo_num_cpus`
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	if _, err := promql.LowerAt(context.Background(), expr, s, at, at); err == nil {
		t.Fatalf("lower(%q): expected an error, got none", query)
	}
}
