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

// mvpPlainVector is the plain, non-mixed, non-histogram-valued vector
// every test in this file pairs against [mixedOrExpr]
// (histogram_native_mixed_or_vector_arithmetic_test.go).
const mvpPlainVector = "demo_num_cpus"

// TestLower_ExpHistogram_MixedSetOpOr_VectorPlainAdditiveArithmetic pins
// cerberus issue #2449's tenth (and final) wrapper family for `+`/`-`: a
// mixed `or` operand paired with an ordinary plain vector lowers to the
// SAME Project-over-Filter-over-Project-over-MixedVectorJoin shape
// [TestLower_ExpHistogram_MixedSetOpOr_VectorVectorAdditiveArithmetic]
// already pins for two mixed operands — see
// histogram_native_mixed_or_vector_plain_arithmetic.go's header for why
// the fold needs no shape change at all. Both operand orders are checked
// so [chplan.MixedVectorJoin]'s Left/Right correctly track which side was
// syntactically mixed.
func TestLower_ExpHistogram_MixedSetOpOr_VectorPlainAdditiveArithmetic(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for _, op := range []string{"+", "-"} {
		for _, mixedOnLeft := range []bool{true, false} {
			op, mixedOnLeft := op, mixedOnLeft
			name := op
			if !mixedOnLeft {
				name += "_plain_left"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				query := mixedOrExpr + " " + op + " " + mvpPlainVector
				if !mixedOnLeft {
					query = mvpPlainVector + " " + op + " " + mixedOrExpr
				}
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
					t.Fatalf("lower(%q): plan root projects %d columns, want 14", query, len(proj.Projections))
				}
				filter, ok := proj.Input.(*chplan.Filter)
				if !ok {
					t.Fatalf("lower(%q): Project.Input is %T, want *chplan.Filter", query, proj.Input)
				}
				mergeInputs, ok := filter.Input.(*chplan.Project)
				if !ok {
					t.Fatalf("lower(%q): Filter.Input is %T, want *chplan.Project", query, filter.Input)
				}
				join, ok := mergeInputs.Input.(*chplan.MixedVectorJoin)
				if !ok {
					t.Fatalf("lower(%q): merge-input Project.Input is %T, want *chplan.MixedVectorJoin", query, mergeInputs.Input)
				}
				assertMixedJoinSides(t, query, join, mixedOnLeft)
			})
		}
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_VectorPlainScaledArithmetic pins the
// `*`/`/` shape: Project-over-Filter-over-MixedVectorJoin, mirroring
// [TestLower_ExpHistogram_MixedSetOpOr_VectorVectorScaledArithmetic].
func TestLower_ExpHistogram_MixedSetOpOr_VectorPlainScaledArithmetic(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for _, op := range []string{"*", "/"} {
		for _, mixedOnLeft := range []bool{true, false} {
			op, mixedOnLeft := op, mixedOnLeft
			name := op
			if !mixedOnLeft {
				name += "_plain_left"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				query := mixedOrExpr + " " + op + " " + mvpPlainVector
				if !mixedOnLeft {
					query = mvpPlainVector + " " + op + " " + mixedOrExpr
				}
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
					t.Fatalf("lower(%q): plan root projects %d columns, want 14", query, len(proj.Projections))
				}
				filter, ok := proj.Input.(*chplan.Filter)
				if !ok {
					t.Fatalf("lower(%q): Project.Input is %T, want *chplan.Filter", query, proj.Input)
				}
				join, ok := filter.Input.(*chplan.MixedVectorJoin)
				if !ok {
					t.Fatalf("lower(%q): Filter.Input is %T, want *chplan.MixedVectorJoin", query, filter.Input)
				}
				assertMixedJoinSides(t, query, join, mixedOnLeft)
			})
		}
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_VectorPlainFloatOnlyArithmetic pins
// the `^`/`%`/`atan2` shape.
func TestLower_ExpHistogram_MixedSetOpOr_VectorPlainFloatOnlyArithmetic(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for _, op := range []string{"^", "%", "atan2"} {
		op := op
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			query := mixedOrExpr + " " + op + " " + mvpPlainVector
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
			filter, ok := proj.Input.(*chplan.Filter)
			if !ok {
				t.Fatalf("lower(%q): Project.Input is %T, want *chplan.Filter", query, proj.Input)
			}
			if _, ok := filter.Input.(*chplan.MixedVectorJoin); !ok {
				t.Fatalf("lower(%q): Filter.Input is %T, want *chplan.MixedVectorJoin", query, filter.Input)
			}
		})
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_VectorPlainGroupLeftRight pins
// group_left()/group_right() cardinality composing with a plain vector
// operand — [chplan.MixedVectorJoin]'s Card/Include fields thread through
// unchanged.
func TestLower_ExpHistogram_MixedSetOpOr_VectorPlainGroupLeftRight(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	query := mixedOrExpr + ` * on(job) group_left(region) ` + mvpPlainVector
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
	filter, ok := proj.Input.(*chplan.Filter)
	if !ok {
		t.Fatalf("lower(%q): Project.Input is %T, want *chplan.Filter", query, proj.Input)
	}
	join, ok := filter.Input.(*chplan.MixedVectorJoin)
	if !ok {
		t.Fatalf("lower(%q): Filter.Input is %T, want *chplan.MixedVectorJoin", query, filter.Input)
	}
	if join.Card != chplan.CardManyToOne {
		t.Fatalf("lower(%q): join.Card = %v, want CardManyToOne", query, join.Card)
	}
	if len(join.Include) != 1 || join.Include[0] != "region" {
		t.Fatalf("lower(%q): join.Include = %v, want [region]", query, join.Include)
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_VectorPlain_HistogramOtherStillRejects
// pins that a mixed `or` paired with a GENUINELY histogram-valued (not
// mixed, not plain) other operand remains unimplemented — a different
// shape this piece of #2449 deliberately does not attempt (see
// [lowerPlainOperandForMixedJoin]'s own doc).
func TestLower_ExpHistogram_MixedSetOpOr_VectorPlain_HistogramOtherStillRejects(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	query := mixedOrExpr + " + demo_latency_exp_hist"
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	if _, err := promql.LowerAt(context.Background(), expr, s, at, at); err == nil {
		t.Fatalf("LowerAt(%q): want an error (a mixed `or` paired with a genuinely histogram-valued operand is not this piece's scope), got none", query)
	}
}

// assertMixedJoinSides checks that join's Left/Right correctly track which
// operand was syntactically mixed: the mixed side lowers through
// [lowerMixedExpHistogramSetOp] (a *chplan.VectorSetOp), while the plain
// side is [widenPlainVectorToMixedShape]'s wrapping *chplan.Project.
func assertMixedJoinSides(t *testing.T, query string, join *chplan.MixedVectorJoin, mixedOnLeft bool) {
	t.Helper()
	mixedSide, plainSide := join.Right, join.Left
	if mixedOnLeft {
		mixedSide, plainSide = join.Left, join.Right
	}
	if _, ok := mixedSide.(*chplan.VectorSetOp); !ok {
		t.Errorf("lower(%q): mixed side is %T, want *chplan.VectorSetOp", query, mixedSide)
	}
	plainProj, ok := plainSide.(*chplan.Project)
	if !ok {
		t.Fatalf("lower(%q): plain side is %T, want *chplan.Project (widenPlainVectorToMixedShape)", query, plainSide)
	}
	if len(plainProj.Projections) != 14 {
		t.Fatalf("lower(%q): widened plain side projects %d columns, want 14", query, len(plainProj.Projections))
	}
	last := plainProj.Projections[len(plainProj.Projections)-1]
	if last.Alias != chplan.MixedDiscriminatorColumn {
		t.Fatalf("lower(%q): widened plain side's last projection alias = %q, want %q", query, last.Alias, chplan.MixedDiscriminatorColumn)
	}
	lit, ok := last.Expr.(*chplan.LitInt)
	if !ok || lit.V != 0 {
		t.Fatalf("lower(%q): widened plain side's discriminator = %#v, want a LitInt{0}", query, last.Expr)
	}
}

// findMixedVectorJoin walks n's Input chain looking for the
// *chplan.MixedVectorJoin every mixed `or` vs. plain-vector wrapper
// eventually builds, failing the test if none is found. Shared by this
// file and histogram_native_mixed_or_vector_plain_comparison_test.go (same
// promql_test package) — the plan shape between the two differs (an extra
// merge-input Project for `+`/`-`, none for comparisons/`*`//), so a
// recursive search is needed rather than hardcoding a fixed Input depth.
func findMixedVectorJoin(t *testing.T, query string, n chplan.Node) *chplan.MixedVectorJoin {
	t.Helper()
	var walk func(chplan.Node) *chplan.MixedVectorJoin
	walk = func(cur chplan.Node) *chplan.MixedVectorJoin {
		if cur == nil {
			return nil
		}
		if j, ok := cur.(*chplan.MixedVectorJoin); ok {
			return j
		}
		for _, c := range cur.Children() {
			if j := walk(c); j != nil {
				return j
			}
		}
		return nil
	}
	join := walk(n)
	if join == nil {
		t.Fatalf("lower(%q): no *chplan.MixedVectorJoin found in plan rooted at %#v", query, n)
	}
	return join
}

// TestLower_ExpHistogram_MixedSetOpOr_VectorPlainArithmetic_StepAligned
// pins [chplan.MixedVectorJoin].StepAligned: true for a materialised
// range-mode grid (Step > 0), false for an instant query (Step == 0) —
// the same `ctx.step > 0` boundary
// [TestLower_ExpHistogram_MixedSetOpOr_VectorVectorAdditiveArithmetic] and
// its siblings never actually assert (they only check plan SHAPE, not this
// field), so this is the one place StepAligned's own boundary is pinned
// for the mixed-`or`-vs-plain-vector wrapper family.
func TestLower_ExpHistogram_MixedSetOpOr_VectorPlainArithmetic_StepAligned(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	query := mixedOrExpr + " * " + mvpPlainVector
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}

	t.Run("instant", func(t *testing.T) {
		t.Parallel()
		plan, err := promql.LowerAt(context.Background(), expr, s, end, end)
		if err != nil {
			t.Fatalf("LowerAt(%q): unexpected error: %v", query, err)
		}
		join := findMixedVectorJoin(t, query, plan)
		if join.StepAligned {
			t.Fatalf("lower(%q) instant: MixedVectorJoin.StepAligned = true, want false (Step == 0)", query)
		}
	})
	t.Run("range", func(t *testing.T) {
		t.Parallel()
		plan, err := promql.LowerAtRange(context.Background(), expr, s, start, end, 30*time.Second)
		if err != nil {
			t.Fatalf("LowerAtRange(%q): unexpected error: %v", query, err)
		}
		join := findMixedVectorJoin(t, query, plan)
		if !join.StepAligned {
			t.Fatalf("lower(%q) range: MixedVectorJoin.StepAligned = false, want true (Step > 0)", query)
		}
	})
}
