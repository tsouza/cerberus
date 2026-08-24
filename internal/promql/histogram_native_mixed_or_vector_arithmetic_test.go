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

// mixedOrExpr is the #2330/#2335 mixed float/histogram `or` shape every
// case below composes over: one arm genuinely histogram-valued, one
// genuinely float-valued (a histogram_quantile() over the same series).
const mixedOrExpr = `(demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist))`

// TestLower_ExpHistogram_MixedSetOpOr_VectorVectorFloatOnlyArithmetic pins
// cerberus issue #2449's seventh wrapper family for `+`/`-`: reference
// Prometheus keeps ONLY the float,float combination for these two ops
// (histogram_native_mixed_or_vector_arithmetic.go's header has the full
// four-combination accounting), so the lowered plan is a PLAIN canonical
// quartet — no discriminator, [chplan.SampleRowShape] — unlike the
// MUL/DIV case below.
func TestLower_ExpHistogram_MixedSetOpOr_VectorVectorFloatOnlyArithmetic(t *testing.T) {
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
			if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
				t.Fatalf("lower(%q): plan root publishes %s, want %s (only float,float survives +/-)", query, shape, chplan.SampleRowShape)
			}
			proj, ok := plan.(*chplan.Project)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.Project", query, plan)
			}
			if len(proj.Projections) != 4 {
				t.Fatalf("lower(%q): plan root projects %d columns, want 4 (the plain canonical quartet)", query, len(proj.Projections))
			}
			if proj.Projections[0].Alias != s.MetricNameColumn {
				t.Fatalf("lower(%q): first projection alias = %q, want %q", query, proj.Projections[0].Alias, s.MetricNameColumn)
			}
			lit, ok := proj.Projections[0].Expr.(*chplan.LitString)
			if !ok || lit.V != "" {
				t.Fatalf("lower(%q): __name__ projection = %#v, want an empty LitString", query, proj.Projections[0].Expr)
			}
			filter, ok := proj.Input.(*chplan.Filter)
			if !ok {
				t.Fatalf("lower(%q): Project.Input is %T, want *chplan.Filter (float,float keep predicate)", query, proj.Input)
			}
			if _, ok := filter.Input.(*chplan.MixedVectorJoin); !ok {
				t.Fatalf("lower(%q): Filter.Input is %T, want *chplan.MixedVectorJoin", query, filter.Input)
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

// TestLower_ExpHistogram_MixedSetOpOr_VectorVectorComparisonStillRejects
// pins that vector-vector COMPARISONS over two mixed `or` operands remain
// unimplemented — cerberus issue #2449's own remaining scope after this
// PR, alongside group_left()/group_right() and the histogram,histogram
// `+`/`-` merge (see this package's histogram_native_mixed_or_vector_
// arithmetic.go header for the full accounting).
func TestLower_ExpHistogram_MixedSetOpOr_VectorVectorComparisonStillRejects(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for _, op := range []string{"==", "!=", "<", "<=", ">", ">="} {
		op := op
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			query := mixedOrExpr + " " + op + " " + mixedOrExpr
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			if _, err := promql.LowerAt(context.Background(), expr, s, at, at); err == nil {
				t.Fatalf("lower(%q): expected an error, got none (vector-vector comparisons over a mixed or remain unimplemented)", query)
			}
		})
	}
}

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

// TestLower_ExpHistogram_MixedSetOpOr_VectorVectorGroupLeftStillRejects
// pins that group_left()/group_right() between two mixed `or` operands
// remains unimplemented — [chplan.MixedVectorJoin]'s own doc names why
// broadcasting the "many" side while ALSO discriminating each row's own
// payload is a separately-scoped extension.
func TestLower_ExpHistogram_MixedSetOpOr_VectorVectorGroupLeftStillRejects(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	query := mixedOrExpr + ` * on(job) group_left() ` + mixedOrExpr
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	if _, err := promql.LowerAt(context.Background(), expr, s, at, at); err == nil {
		t.Fatalf("lower(%q): expected an error, got none", query)
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
