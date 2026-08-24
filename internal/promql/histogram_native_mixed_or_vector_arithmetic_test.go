package promql_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// mixedOrExpr is the #2330/#2335 mixed float/histogram `or` shape every
// case below composes over: one arm genuinely histogram-valued, one
// genuinely float-valued (a histogram_quantile() over the same series).
const mixedOrExpr = `(demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist))`

// TestLower_ExpHistogram_MixedSetOpOr_VectorVectorAdditiveArithmetic pins
// cerberus issue #2449's histogram,histogram `+`/`-` wrapper family:
// reference Prometheus keeps the float,float combination (plain float
// arithmetic) AND the histogram,histogram combination (a genuine merge,
// histogram_native_mixed_or_vector_arithmetic.go's header has the full
// four-combination accounting) for these two ops, so the lowered plan is
// the full fourteen-column Mixed shape — like the MUL/DIV case below,
// but with an extra Project stage between the keep Filter and the join
// (mixedVVHistMergeInputProjections's merge-array materialisation).
func TestLower_ExpHistogram_MixedSetOpOr_VectorVectorAdditiveArithmetic(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for _, op := range []string{"+", "-"} {
		op := op
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			query := mixedOrExpr + " " + op + " " + mixedOrExpr
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt(%q): unexpected error: %v", query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.MixedRowShape {
				t.Fatalf("lower(%q): plan root publishes %s, want %s (float,float AND histogram,histogram survive +/-)", query, shape, chplan.MixedRowShape)
			}
			proj, ok := plan.(*chplan.Project)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.Project", query, plan)
			}
			if len(proj.Projections) != 14 {
				t.Fatalf("lower(%q): plan root projects %d columns, want 14 (the fourteen-column Mixed shape)", query, len(proj.Projections))
			}
			if proj.Projections[0].Alias != s.MetricNameColumn {
				t.Fatalf("lower(%q): first projection alias = %q, want %q", query, proj.Projections[0].Alias, s.MetricNameColumn)
			}
			lit, ok := proj.Projections[0].Expr.(*chplan.LitString)
			if !ok || lit.V != "" {
				t.Fatalf("lower(%q): __name__ projection = %#v, want an empty LitString", query, proj.Projections[0].Expr)
			}
			last := proj.Projections[len(proj.Projections)-1]
			if last.Alias != chplan.MixedDiscriminatorColumn {
				t.Fatalf("lower(%q): last projection alias = %q, want %q", query, last.Alias, chplan.MixedDiscriminatorColumn)
			}
			filter, ok := proj.Input.(*chplan.Filter)
			if !ok {
				t.Fatalf("lower(%q): Project.Input is %T, want *chplan.Filter (the same-type keep predicate)", query, proj.Input)
			}
			mergeInputs, ok := filter.Input.(*chplan.Project)
			if !ok {
				t.Fatalf("lower(%q): Filter.Input is %T, want *chplan.Project (the merge-array materialisation)", query, filter.Input)
			}
			if _, ok := mergeInputs.Input.(*chplan.MixedVectorJoin); !ok {
				t.Fatalf("lower(%q): merge-input Project.Input is %T, want *chplan.MixedVectorJoin", query, mergeInputs.Input)
			}
		})
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_VectorVectorScaledArithmetic pins
// cerberus issue #2449's seventh wrapper family for `*`/`/`: reference
// keeps THREE of the four combinations (float,float always; float,
// histogram for `*` only; histogram,float for both), so the plan stays
// the full fourteen-column Mixed shape.
func TestLower_ExpHistogram_MixedSetOpOr_VectorVectorScaledArithmetic(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for _, op := range []string{"*", "/"} {
		op := op
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			query := mixedOrExpr + " " + op + " " + mixedOrExpr
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
			proj, ok := plan.(*chplan.Project)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.Project", query, plan)
			}
			if len(proj.Projections) != 14 {
				t.Fatalf("lower(%q): plan root projects %d columns, want 14 (the fourteen-column Mixed shape)", query, len(proj.Projections))
			}
			last := proj.Projections[len(proj.Projections)-1]
			if last.Alias != chplan.MixedDiscriminatorColumn {
				t.Fatalf("lower(%q): last projection alias = %q, want %q", query, last.Alias, chplan.MixedDiscriminatorColumn)
			}
			filter, ok := proj.Input.(*chplan.Filter)
			if !ok {
				t.Fatalf("lower(%q): Project.Input is %T, want *chplan.Filter (the drop-histogram,histogram keep predicate)", query, proj.Input)
			}
			if _, ok := filter.Input.(*chplan.MixedVectorJoin); !ok {
				t.Fatalf("lower(%q): Filter.Input is %T, want *chplan.MixedVectorJoin", query, filter.Input)
			}
		})
	}
}

// Vector-vector COMPARISONS over two mixed `or` operands are implemented
// by histogram_native_mixed_or_vector_comparison.go, whose own test file
// (histogram_native_mixed_or_vector_comparison_test.go) pins the
// four-combination plan shape. `^`/`%`/`atan2` over two mixed `or`
// operands, and a mixed `or` operand paired with a plain (non-mixed)
// vector, remain cerberus issue #2449's open scope (see this package's
// histogram_native_mixed_or_vector_arithmetic.go header for the full
// accounting).

// TestLower_ExpHistogram_MixedSetOpOr_VectorVectorPowStillRejects pins
// that `^`/`%`/`atan2` over two mixed `or` operands remain unimplemented
// even for the float,float combination — this recognizer only matches
// `+`, `-`, `*`, `/` (see histogram_native_mixed_or_vector_arithmetic.go's
// header for why).
func TestLower_ExpHistogram_MixedSetOpOr_VectorVectorPowStillRejects(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	query := mixedOrExpr + " ^ " + mixedOrExpr
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	if _, err := promql.LowerAt(context.Background(), expr, s, at, at); err == nil {
		t.Fatalf("lower(%q): expected an error, got none", query)
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_VectorVectorGroupLeftRight pins
// cerberus issue #2449's ninth wrapper family: group_left()/group_right()
// between two mixed `or` operands now lowers to a [chplan.MixedVectorJoin]
// carrying the matching Card/Include (rather than falling through to the
// pre-existing rejection), for both the bare modifier and the
// Include-labels form — see [chplan.MixedVectorJoin]'s own doc for why
// broadcasting the "many" side does not compound with the per-row
// float/histogram discrimination this node stays blind to.
func TestLower_ExpHistogram_MixedSetOpOr_VectorVectorGroupLeftRight(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name        string
		query       string
		wantCard    chplan.VectorCard
		wantInclude []string
	}{
		{
			name:     "group_left, no Include",
			query:    mixedOrExpr + ` * on(job) group_left() ` + mixedOrExpr,
			wantCard: chplan.CardManyToOne,
		},
		{
			name:        "group_left, with Include",
			query:       mixedOrExpr + ` * on(job) group_left(instance) ` + mixedOrExpr,
			wantCard:    chplan.CardManyToOne,
			wantInclude: []string{"instance"},
		},
		{
			name:        "group_right, with Include",
			query:       mixedOrExpr + ` * on(job) group_right(instance) ` + mixedOrExpr,
			wantCard:    chplan.CardOneToMany,
			wantInclude: []string{"instance"},
		},
	}

	for _, tc := range cases {
		tc := tc
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
			proj, ok := plan.(*chplan.Project)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.Project", tc.query, plan)
			}
			filter, ok := proj.Input.(*chplan.Filter)
			if !ok {
				t.Fatalf("lower(%q): Project.Input is %T, want *chplan.Filter", tc.query, proj.Input)
			}
			join, ok := filter.Input.(*chplan.MixedVectorJoin)
			if !ok {
				t.Fatalf("lower(%q): Filter.Input is %T, want *chplan.MixedVectorJoin", tc.query, filter.Input)
			}
			if join.Card != tc.wantCard {
				t.Errorf("lower(%q): Card = %v, want %v", tc.query, join.Card, tc.wantCard)
			}
			if !slices.Equal(join.Include, tc.wantInclude) {
				t.Errorf("lower(%q): Include = %v, want %v", tc.query, join.Include, tc.wantInclude)
			}
		})
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_VectorVectorOnIgnoring pins that
// on()/ignoring() vector matching (still CardOneToOne) is supported: the
// output Attributes projection is a mapFilter reduction rather than a
// bare forwarded column.
func TestLower_ExpHistogram_MixedSetOpOr_VectorVectorOnIgnoring(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		query string
	}{
		{"on", mixedOrExpr + ` + on(job) ` + mixedOrExpr},
		{"ignoring", mixedOrExpr + ` - ignoring(job) ` + mixedOrExpr},
	}
	for _, tc := range cases {
		tc := tc
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
			proj, ok := plan.(*chplan.Project)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.Project", tc.query, plan)
			}
			attrsProj := proj.Projections[1]
			if attrsProj.Alias != s.AttributesColumn {
				t.Fatalf("lower(%q): second projection alias = %q, want %q", tc.query, attrsProj.Alias, s.AttributesColumn)
			}
			call, ok := attrsProj.Expr.(*chplan.FuncCall)
			if !ok || call.Fn != chplan.FnMapFilter {
				t.Fatalf("lower(%q): Attributes projection = %#v, want a mapFilter FuncCall (on()/ignoring() reduction)", tc.query, attrsProj.Expr)
			}
		})
	}
}
