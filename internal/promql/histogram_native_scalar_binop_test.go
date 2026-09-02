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

// TestLower_ExpHistogram_ScalarBinopIsHistogramValued pins cerberus
// issue #2087: `<exp-hist shape> (*|/) <scalar>` is answerable over all
// three histogram-VALUED lowerings — a bare selector, sum()/avg(), and
// rate()/increase() — and the answer is a chplan.HistogramProjection
// publishing the same thirteen-column contract every other
// histogram-valued shape does.
//
// The incompatible scalar operators are covered separately by
// TestLower_ExpHistogram_IncompatibleScalarBinopDropsSamples: reference
// answers those with an empty result plus an info annotation, never a
// histogram value.
func TestLower_ExpHistogram_ScalarBinopIsHistogramValued(t *testing.T) {
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
			name:  "bare selector times scalar",
			query: `latency_exp_hist * 2`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "scalar times bare selector (commutative)",
			query: `2 * latency_exp_hist`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "bare selector divided by scalar",
			query: `latency_exp_hist / 4`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "bare selector times scalar, range",
			query: `latency_exp_hist * 2`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, 30*time.Second)
			},
		},
		{
			name:  "sum divided by scalar",
			query: `sum(latency_exp_hist) / 2`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "sum by(...) times scalar",
			query: `sum by (service) (latency_exp_hist) * 3`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "avg divided by scalar",
			query: `avg(latency_exp_hist) / 2`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "rate times scalar",
			query: `rate(latency_exp_hist[5m]) * 2`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "increase divided by scalar",
			query: `increase(latency_exp_hist[5m]) / 2`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "rate times scalar, range",
			query: `rate(latency_exp_hist[5m]) * 2`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, 30*time.Second)
			},
		},
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
			name, ok := hp.GroupBy[0].(*chplan.LitString)
			if !ok || name.V != "" {
				t.Fatalf("lower(%q): metric-name projection is %#v, want empty literal", tc.query, hp.GroupBy[0])
			}
		})
	}
}

// TestLower_ExpHistogram_ScalarBinopOverSetOpIsHistogramValued pins
// cerberus issue #2557: `<exp-hist shape> (*|/) <scalar>` composes with a
// `and`/`or`/`unless` set-op result (cerberus issue #2324) the same way
// it already composes with a bare selector / sum() / avg() / rate().
//
// Before the fix, [isExpHistogramValuedShape] — the very recognizer
// [expHistogramScalarBinop] gates on to decide the histogram side is
// eligible — reported a set-op result histogram-valued, but
// [lowerExpHistogramScalarBinop]'s own consumer required the histogram
// side's lowering to cap with literally a *chplan.HistogramProjection; a
// set op lowers to a *chplan.VectorSetOp instead, so the mismatch
// surfaced as an "internal invariant violated" lowering error — a clean
// HTTP 422 (the promql "lower" stage error mapping), not a process crash
// or an unrecovered panic — instead of the two shapes composing.
func TestLower_ExpHistogram_ScalarBinopOverSetOpIsHistogramValued(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		query string
	}{
		{name: "and times scalar", query: `(latency_exp_hist and other_exp_hist) * 2`},
		{name: "scalar times and", query: `2 * (latency_exp_hist and other_exp_hist)`},
		{name: "or divided by scalar", query: `(latency_exp_hist or other_exp_hist) / 4`},
		{name: "unless times scalar", query: `(latency_exp_hist unless other_exp_hist) * 2`},
		{name: "on() and times scalar", query: `(latency_exp_hist and on(service) other_exp_hist) * 2`},
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
			if shape := chplan.RowShapeOf(plan); shape != chplan.HistogramRowShape {
				t.Fatalf("lower(%q): plan root publishes %s, want histogram", tc.query, shape)
			}
			hp, ok := plan.(*chplan.HistogramProjection)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.HistogramProjection", tc.query, plan)
			}
			reshape, ok := hp.Input.(*chplan.Project)
			if !ok {
				t.Fatalf("lower(%q): HistogramProjection.Input is %T, want *chplan.Project", tc.query, hp.Input)
			}
			setOp, ok := reshape.Input.(*chplan.VectorSetOp)
			if !ok {
				t.Fatalf("lower(%q): the scaling Project's Input is %T, want *chplan.VectorSetOp", tc.query, reshape.Input)
			}
			if !setOp.Histogram {
				t.Fatalf("lower(%q): VectorSetOp.Histogram = false, want true", tc.query)
			}
		})
	}
}

// TestLower_ExpHistogram_DroppingScalarBinopOverSetOpDoesNotError pins
// the "drop family" (cerberus issue #2189) sibling of the same #2557
// gap: `<set-op result> (+|-|^|%|==|!=|>|<|>=|<=|atan2) <scalar>` (and
// scalar-left `/`) must answer reference's empty keep=false result
// through the same constant-false [chplan.Filter] the drop family always
// builds, not the "internal invariant violated" lowering error.
func TestLower_ExpHistogram_DroppingScalarBinopOverSetOpDoesNotError(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	queries := []string{
		`(latency_exp_hist and other_exp_hist) + 1`,
		`(latency_exp_hist or other_exp_hist) == 1`,
		`2 / (latency_exp_hist unless other_exp_hist)`,
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
			filter, ok := plan.(*chplan.Filter)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want constant-false *chplan.Filter", query, plan)
			}
			predicate, ok := filter.Predicate.(*chplan.LitBool)
			if !ok || predicate.V {
				t.Fatalf("lower(%q): filter predicate is %#v, want false literal", query, filter.Predicate)
			}
			if shape := chplan.RowShapeOf(filter.Input); shape != chplan.HistogramRowShape {
				t.Fatalf("lower(%q): filtered input publishes %s, want histogram", query, shape)
			}
		})
	}
}

func TestLower_ExpHistogram_IncompatibleScalarBinopDropsSamples(t *testing.T) {
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
		{name: "histogram plus scalar", query: `latency_exp_hist + 1`},
		{name: "scalar plus histogram", query: `1 + latency_exp_hist`},
		{name: "histogram minus scalar", query: `latency_exp_hist - 1`},
		{name: "scalar minus histogram", query: `1 - latency_exp_hist`},
		{name: "histogram power scalar", query: `latency_exp_hist ^ 2`},
		{name: "scalar power histogram", query: `2 ^ latency_exp_hist`},
		{name: "histogram modulo scalar", query: `latency_exp_hist % 2`},
		{name: "scalar modulo histogram", query: `2 % latency_exp_hist`},
		{name: "scalar divided by histogram", query: `2 / latency_exp_hist`},
		{name: "equal", query: `latency_exp_hist == 1`},
		{name: "not equal", query: `latency_exp_hist != 1`},
		{name: "greater", query: `latency_exp_hist > 1`},
		{name: "less", query: `latency_exp_hist < 1`},
		{name: "greater equal", query: `latency_exp_hist >= 1`},
		{name: "less equal", query: `latency_exp_hist <= 1`},
		{name: "bool comparison", query: `latency_exp_hist > bool 1`},
		{name: "atan2", query: `latency_exp_hist atan2 1`},
		{name: "parenthesised", query: `(latency_exp_hist) + 1`},
		{name: "sum plus scalar", query: `sum(latency_exp_hist) + 1`},
		{name: "increase plus scalar", query: `increase(latency_exp_hist[5m]) + 1`},
		{
			name:  "range query",
			query: `latency_exp_hist + 1`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, 30*time.Second)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			lower := tc.lower
			if lower == nil {
				lower = func(e parser.Expr) (chplan.Node, error) {
					return promql.LowerAt(context.Background(), e, s, end, end)
				}
			}
			plan, err := lower(expr)
			if err != nil {
				t.Fatalf("lower(%q): unexpected error: %v", tc.query, err)
			}
			filter, ok := plan.(*chplan.Filter)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want constant-false *chplan.Filter", tc.query, plan)
			}
			predicate, ok := filter.Predicate.(*chplan.LitBool)
			if !ok || predicate.V {
				t.Fatalf("lower(%q): filter predicate is %#v, want false literal", tc.query, filter.Predicate)
			}
			if shape := chplan.RowShapeOf(filter.Input); shape != chplan.HistogramRowShape {
				t.Fatalf("lower(%q): filtered input publishes %s, want histogram", tc.query, shape)
			}
		})
	}
}

// TestLower_ExpHistogram_ScalarBinopScalesOnlyTheCountFields pins the
// same split TestLower_ExpHistogram_AvgDividesOnlyTheCountFields pins for
// avg(): scaling a histogram by a PromQL scalar touches exactly the five
// COUNT-bearing fields — Count, Sum, ZeroCount, and both signed bucket
// ladders — and leaves Scale, ZeroThreshold and both bucket offsets
// alone, matching reference's FloatHistogram.Mul / .Div
// (histogram.FloatHistogram in the pinned fork).
//
// A plan that scaled the offsets too would still publish thirteen
// well-typed columns and still decode, which is why this checks the
// wrapping Project's expression SHAPES rather than only checking that
// lowering succeeds.
func TestLower_ExpHistogram_ScalarBinopScalesOnlyTheCountFields(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		query string
		op    chplan.BinaryOp
	}{
		{name: "bare selector times scalar", query: `latency_exp_hist * 2`, op: chplan.OpMul},
		{name: "bare selector divided by scalar", query: `latency_exp_hist / 4`, op: chplan.OpDiv},
		{name: "sum divided by scalar", query: `sum(latency_exp_hist) / 2`, op: chplan.OpDiv},
		{name: "rate times scalar", query: `rate(latency_exp_hist[5m]) * 2`, op: chplan.OpMul},
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
				t.Fatalf("LowerAt(%q): %v", tc.query, err)
			}
			hp, ok := plan.(*chplan.HistogramProjection)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.HistogramProjection", tc.query, plan)
			}
			reshape, ok := hp.Input.(*chplan.Project)
			if !ok {
				t.Fatalf("lower(%q): HistogramProjection.Input is %T, want *chplan.Project", tc.query, hp.Input)
			}
			byAlias := make(map[string]chplan.Expr, len(reshape.Projections))
			for _, proj := range reshape.Projections {
				byAlias[proj.Alias] = proj.Expr
			}

			scaledScalar := func(alias string) {
				t.Helper()
				e, ok := byAlias[alias]
				if !ok {
					t.Fatalf("lower(%q): no projection for %q", tc.query, alias)
				}
				bin, ok := e.(*chplan.Binary)
				if !ok || bin.Op != tc.op {
					t.Fatalf("lower(%q): projection for %q = %#v, want a %s Binary", tc.query, alias, e, tc.op)
				}
				lit, ok := bin.Right.(*chplan.LitFloat)
				if !ok {
					t.Fatalf("lower(%q): projection for %q scales by %#v, want a LitFloat", tc.query, alias, bin.Right)
				}
				_ = lit
			}
			scaledLadder := func(alias string) {
				t.Helper()
				e, ok := byAlias[alias]
				if !ok {
					t.Fatalf("lower(%q): no projection for %q", tc.query, alias)
				}
				call, ok := e.(*chplan.FuncCall)
				if !ok || call.Fn != chplan.FnArrayMap || len(call.Args) != 2 {
					t.Fatalf("lower(%q): projection for %q = %#v, want arrayMap(...)", tc.query, alias, e)
				}
				lambda, ok := call.Args[0].(*chplan.Lambda)
				if !ok {
					t.Fatalf("lower(%q): arrayMap's first arg for %q is %T, want *chplan.Lambda", tc.query, alias, call.Args[0])
				}
				bin, ok := lambda.Body.(*chplan.Binary)
				if !ok || bin.Op != tc.op {
					t.Fatalf("lower(%q): ladder lambda for %q = %#v, want a %s Binary", tc.query, alias, lambda.Body, tc.op)
				}
			}
			unscaled := func(alias string) {
				t.Helper()
				e, ok := byAlias[alias]
				if !ok {
					t.Fatalf("lower(%q): no projection for %q", tc.query, alias)
				}
				if _, isBinary := e.(*chplan.Binary); isBinary {
					t.Fatalf("lower(%q): position-bearing column %q was scaled: %#v", tc.query, alias, e)
				}
				if call, isCall := e.(*chplan.FuncCall); isCall && call.Fn == chplan.FnArrayMap {
					t.Fatalf("lower(%q): position-bearing column %q was scaled: %#v", tc.query, alias, e)
				}
			}

			scaledScalar(chplan.HistogramCountColumn)
			scaledScalar(chplan.HistogramSumColumn)
			scaledScalar(chplan.HistogramZeroCountColumn)
			scaledLadder(chplan.HistogramPositiveBucketCountsColumn)
			scaledLadder(chplan.HistogramNegativeBucketCountsColumn)
			unscaled(chplan.HistogramScaleColumn)
			unscaled(chplan.HistogramPositiveOffsetColumn)
			unscaled(chplan.HistogramNegativeOffsetColumn)
		})
	}
}

// TestLower_ExpHistogram_ScaledOperandIsHistogramValuedToWrappers pins
// the MUL/DIV arm of [isExpHistogramValuedShape]
// (histogram_native_scalar_binop.go):
//
//	if (b.Op == parser.MUL || b.Op == parser.DIV) && isExpHistogramValuedShape(b.LHS, s, ctx) {
//	    _, scalar := tryScalarLiteral(b.RHS)
//	    return scalar
//	}
//
// That arm is what tells every OTHER recognizer in this package that
// `<exp-hist> * <scalar>` and `<exp-hist> / <scalar>` are themselves
// histogram-valued, so a wrapper around one keeps the histogram
// lowering instead of falling through to the float path. It is reached
// only through a WRAPPER: the bare `<exp-hist> * 2` root goes through
// [expHistogramScalarBinop]'s own scalar-on-the-right arm, which is
// what the tests above cover, and which is why inverting this arm's
// `||` to `&&` — making the operator test unsatisfiable and the whole
// arm dead — left the package's untagged suite green (cerberus issue
// #2943; the mutant reached a verdict for the first time once #2940
// stopped `go vet`'s bools analyzer rejecting it as "suspect and").
//
// Each case below silently degraded to a float/sample-shaped plan with
// the arm dead — no error, just the wrong lowering — so the assertion
// is on the published row shape, not on an error string. The bare
// `<exp-hist> * <scalar>` control at the end does NOT depend on this
// arm and stays histogram-valued either way; it is included so a
// wholesale regression of exp-histogram scaling is distinguishable from
// the wrapper-only gap this test exists for.
func TestLower_ExpHistogram_ScaledOperandIsHistogramValuedToWrappers(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		query string
	}{
		// Unary minus over a scaled operand — the widening
		// histogram_native_unary.go's own doc comment names.
		{name: "unary minus over scaled", query: `-(latency_exp_hist * 2)`},
		{name: "unary minus over divided", query: `-(latency_exp_hist / 4)`},
		// label_replace forwards the histogram shape of its first arg.
		{name: "label_replace over scaled", query: `label_replace(latency_exp_hist * 2, "svc", "$1", "service", "(.*)")`},
		{name: "label_replace over divided", query: `label_replace(latency_exp_hist / 4, "svc", "$1", "service", "(.*)")`},
		// <exp-hist>+<exp-hist> merge with a scaled operand on one side.
		{name: "scaled plus histogram", query: `(latency_exp_hist * 2) + other_exp_hist`},
		{name: "divided plus histogram", query: `(latency_exp_hist / 4) + other_exp_hist`},
		// Set ops over a scaled operand.
		{name: "scaled and histogram", query: `(latency_exp_hist * 2) and other_exp_hist`},
		{name: "divided unless histogram", query: `(latency_exp_hist / 4) unless other_exp_hist`},
		// A second scaling applied to an already-scaled operand.
		{name: "scaled times scalar again", query: `(latency_exp_hist * 2) * 3`},
		{name: "divided by scalar again", query: `(latency_exp_hist / 4) / 5`},
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
			if shape := chplan.RowShapeOf(plan); shape != chplan.HistogramRowShape {
				t.Fatalf("lower(%q): plan root publishes %s, want %s — the wrapper stopped seeing the scaled operand as histogram-valued",
					tc.query, shape, chplan.HistogramRowShape)
			}
		})
	}

	t.Run("control: bare scaled operand does not depend on this arm", func(t *testing.T) {
		t.Parallel()
		const query = `latency_exp_hist * 2`
		expr, err := p.ParseExpr(query)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", query, err)
		}
		plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
		if err != nil {
			t.Fatalf("LowerAt(%q): unexpected error: %v", query, err)
		}
		if shape := chplan.RowShapeOf(plan); shape != chplan.HistogramRowShape {
			t.Fatalf("lower(%q): plan root publishes %s, want %s", query, shape, chplan.HistogramRowShape)
		}
	})
}
