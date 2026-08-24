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

// TestLower_ExpHistogram_BinopAcceptsSetOpOperand pins cerberus issue
// #2559: `+`/`-`/`==`/`!=` between two exp-histogram shapes used to
// require BOTH operands to lower to a literal *chplan.HistogramProjection
// ([lowerExpHistogramValuedOperand]'s old type assertion), but
// [isExpHistogramValuedShape] — the very recogniser these binops gate on
// to decide an operand is even eligible — reports true for a
// `and`/`or`/`unless` set-op result too (cerberus issue #2324), which
// lowers to a *chplan.VectorSetOp instead. A query like
// `(<histA> and <histB>) + (<histC> and <histD>)` therefore leaked an
// "internal invariant violated" Go type name to the API caller instead of
// lowering correctly.
//
// [lowerExpHistogramValuedOperand] now accepts any histogram-shaped
// [chplan.Node] (checked via [chplan.RowShapeOf], mirroring
// [lowerExpHistogramSetOpOperand]'s existing looser contract) — this test
// pins that every downstream consumer that assumption threads through
// (mergeTwoHistogramProjections, compareTwoHistogramProjections,
// applyVectorMatchToHistogramOperand, the two "drop family" lowerings,
// and the float-vector scaling join) now composes with a set-op operand
// on EITHER side, not just a bare selector / sum() / range-function
// operand.
func TestLower_ExpHistogram_BinopAcceptsSetOpOperand(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	const setOpLHS = "(hist_a_exp_hist and hist_b_exp_hist)"
	const setOpRHS = "(hist_c_exp_hist and hist_d_exp_hist)"

	cases := []struct {
		name      string
		query     string
		wantShape chplan.RowShape
		wantPlan  func(t *testing.T, plan chplan.Node)
	}{
		{
			name:      "addition, both operands set-ops",
			query:     setOpLHS + " + " + setOpRHS,
			wantShape: chplan.HistogramRowShape,
		},
		{
			name:      "subtraction, both operands set-ops",
			query:     setOpLHS + " - " + setOpRHS,
			wantShape: chplan.HistogramRowShape,
		},
		{
			name:      "equality, both operands set-ops",
			query:     setOpLHS + " == " + setOpRHS,
			wantShape: chplan.HistogramRowShape,
		},
		{
			name:      "inequality, both operands set-ops",
			query:     setOpLHS + " != " + setOpRHS,
			wantShape: chplan.HistogramRowShape,
		},
		{
			name:      "bool equality, both operands set-ops",
			query:     setOpLHS + " == bool " + setOpRHS,
			wantShape: chplan.SampleRowShape,
		},
		{
			name:      "mixed: set-op on the left, bare selector on the right",
			query:     setOpLHS + " + hist_e_exp_hist",
			wantShape: chplan.HistogramRowShape,
		},
		{
			name:      "mixed: bare selector on the left, set-op on the right",
			query:     "hist_e_exp_hist - " + setOpRHS,
			wantShape: chplan.HistogramRowShape,
		},
		{
			name:      "on()/ignoring() default-cardinality merge with a set-op operand",
			query:     setOpLHS + " + on(service) " + setOpRHS,
			wantShape: chplan.HistogramRowShape,
		},
		{
			// lowerExpHistogramDroppingHistogramBinop caps its
			// constant-false Filter WITHOUT setting Filter.Histogram (see
			// TestLower_ExpHistogram_IncompatibleHistogramBinopDropsSamples,
			// histogram_native_binop_test.go, for the pre-existing
			// non-set-op case this mirrors) — the top-level plan therefore
			// publishes SampleRowShape, and it's the wrapped Filter.Input
			// that must still publish histogram.
			name:      "incompatible-type drop family (MUL) with set-op operands",
			query:     setOpLHS + " * " + setOpRHS,
			wantShape: chplan.SampleRowShape,
			wantPlan: func(t *testing.T, plan chplan.Node) {
				t.Helper()
				filter, ok := plan.(*chplan.Filter)
				if !ok {
					t.Fatalf("plan root is %T, want *chplan.Filter", plan)
				}
				if shape := chplan.RowShapeOf(filter.Input); shape != chplan.HistogramRowShape {
					t.Fatalf("filter.Input publishes %s, want histogram", shape)
				}
			},
		},
		{
			name:      "mixed float-vector drop family (ADD) with a set-op operand",
			query:     setOpLHS + " + some_float_vector",
			wantShape: chplan.SampleRowShape,
		},
		{
			name:      "float-vector scaling (MUL) with a set-op operand",
			query:     setOpLHS + " * some_float_vector",
			wantShape: chplan.HistogramRowShape,
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
				t.Fatalf("LowerAt(%q): unexpected error (this exact shape used to leak \"internal invariant violated\" — cerberus issue #2559): %v", tc.query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != tc.wantShape {
				t.Fatalf("lower(%q): plan root publishes %s, want %s", tc.query, shape, tc.wantShape)
			}
			if tc.wantPlan != nil {
				tc.wantPlan(t, plan)
			}
		})
	}
}

// TestLower_ExpHistogram_BinopAcceptsSetOpOperand_GroupLeftRight pins the
// same #2559 fix for the group_left()/group_right() (Card) siblings —
// mergeTwoHistogramProjectionsCard / compareTwoHistogramProjectionsCard —
// which route through a real chplan.HistogramVectorJoin instead of the
// one-to-one UnionAll+Aggregate shape. A set-op operand on either side
// must still reach a HistogramVectorJoin, not the old type-asserting
// panic-adjacent error.
func TestLower_ExpHistogram_BinopAcceptsSetOpOperand_GroupLeftRight(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	const setOpLHS = "(hist_a_exp_hist and hist_b_exp_hist)"
	const setOpRHS = "(hist_c_exp_hist and hist_d_exp_hist)"

	queries := []string{
		setOpLHS + " + on(service) group_left() " + setOpRHS,
		setOpLHS + " - on(service) group_right() " + setOpRHS,
		setOpLHS + " == on(service) group_left() " + setOpRHS,
		setOpLHS + " != on(service) group_right() " + setOpRHS,
	}

	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt(%q): unexpected error: %v", query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.HistogramRowShape {
				t.Fatalf("lower(%q): plan root publishes %s, want histogram", query, shape)
			}
			if _, ok := plan.(*chplan.HistogramProjection); !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.HistogramProjection", query, plan)
			}
		})
	}
}
