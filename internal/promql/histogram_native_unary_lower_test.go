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

// expHistogramUnaryNegationFactor is the scalar `-hist` folds into: unary
// minus over a histogram-valued operand IS "multiply by the compile-time
// literal -1", which is what negates
// Count/Sum/ZeroCount/PositiveBucketCounts/NegativeBucketCounts while
// leaving Scale/ZeroThreshold/both offsets untouched. See
// histogram_native_unary.go's own doc for the reference-semantics citation.
const expHistogramUnaryNegationFactor = -1.0

// TestLower_ExpHistogram_UnaryFoldsToTheLiteralNegation pins what
// [lowerUnaryOverExpHistogram] actually produces for the two unary
// operators, which is a strictly stronger statement than "it lowers":
//
//   - `-<exp-hist>` reshapes through a *chplan.Project whose count-bearing
//     projections are `<col> * -1`. The factor is asserted by VALUE: a fold
//     that multiplied by +1 (or by anything else) would still publish
//     thirteen well-typed columns and still decode, and would answer every
//     query with the wrong sign.
//   - `+<exp-hist>` is reference's identity case and must add NO scaling
//     reshape at all — it defers to the operand's own histogram-valued
//     lowering unchanged.
//
// The two halves are what make each other discriminating: swapping the
// operator arms (so `+` negated and `-` did not) satisfies neither.
//
// Until the scaled-operand wrapper cases in
// histogram_native_scalar_binop_test.go were added, this lowering was
// reached by no untagged test at all — its mutants sat in gremlins'
// NOT COVERED bucket, outside both sides of the efficacy ratio, so the gap
// was invisible rather than merely unmeasured (cerberus issue #2943).
func TestLower_ExpHistogram_UnaryFoldsToTheLiteralNegation(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	lower := func(t *testing.T, query string) chplan.Node {
		t.Helper()
		expr, err := p.ParseExpr(query)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", query, err)
		}
		plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
		if err != nil {
			t.Fatalf("LowerAt(%q): unexpected error: %v", query, err)
		}
		return plan
	}

	// The count-bearing scalar fields the negation must reach. The two
	// bucket ladders are arrayMap-shaped rather than a plain Binary and are
	// already pinned field-by-field by
	// TestLower_ExpHistogram_ScalarBinopScalesOnlyTheCountFields; this test
	// is about the FACTOR, so it asserts on the three scalar carriers.
	scalarFields := []string{
		chplan.HistogramCountColumn,
		chplan.HistogramSumColumn,
		chplan.HistogramZeroCountColumn,
	}

	t.Run("unary minus scales by -1", func(t *testing.T) {
		t.Parallel()
		const query = `-latency_exp_hist`
		plan := lower(t, query)
		hp, ok := plan.(*chplan.HistogramProjection)
		if !ok {
			t.Fatalf("lower(%q): plan root is %T, want *chplan.HistogramProjection", query, plan)
		}
		reshape, ok := hp.Input.(*chplan.Project)
		if !ok {
			t.Fatalf("lower(%q): HistogramProjection.Input is %T, want the scaling *chplan.Project", query, hp.Input)
		}
		byAlias := make(map[string]chplan.Expr, len(reshape.Projections))
		for _, proj := range reshape.Projections {
			byAlias[proj.Alias] = proj.Expr
		}
		for _, alias := range scalarFields {
			e, ok := byAlias[alias]
			if !ok {
				t.Errorf("lower(%q): no projection for %q", query, alias)
				continue
			}
			bin, ok := e.(*chplan.Binary)
			if !ok || bin.Op != chplan.OpMul {
				t.Errorf("lower(%q): projection for %q = %#v, want a %s Binary", query, alias, e, chplan.OpMul)
				continue
			}
			lit, ok := bin.Right.(*chplan.LitFloat)
			if !ok {
				t.Errorf("lower(%q): projection for %q scales by %#v, want a *chplan.LitFloat", query, alias, bin.Right)
				continue
			}
			if lit.V != expHistogramUnaryNegationFactor {
				t.Errorf("lower(%q): projection for %q scales by %v, want %v — unary minus must NEGATE, not rescale",
					query, alias, lit.V, expHistogramUnaryNegationFactor)
			}
		}
		// Scale is position-bearing and must survive untouched, so the
		// negation is proven to be selective rather than blanket.
		if e, ok := byAlias[chplan.HistogramScaleColumn]; !ok {
			t.Errorf("lower(%q): no projection for %q", query, chplan.HistogramScaleColumn)
		} else if _, isBinary := e.(*chplan.Binary); isBinary {
			t.Errorf("lower(%q): position-bearing column %q was scaled: %#v", query, chplan.HistogramScaleColumn, e)
		}
	})

	t.Run("unary plus is the identity", func(t *testing.T) {
		t.Parallel()
		const query = `+latency_exp_hist`
		plan := lower(t, query)
		hp, ok := plan.(*chplan.HistogramProjection)
		if !ok {
			t.Fatalf("lower(%q): plan root is %T, want *chplan.HistogramProjection", query, plan)
		}
		if reshape, isProject := hp.Input.(*chplan.Project); isProject {
			t.Fatalf("lower(%q): unary + added a scaling reshape (%d projections) — reference's `+` is the identity, so the operand's own lowering must pass through unchanged",
				query, len(reshape.Projections))
		}
		// Byte-for-byte the bare operand's own plan: `+hist` and `hist`
		// differ only in the AST, never in the IR.
		bare := lower(t, `latency_exp_hist`)
		if !plan.Equal(bare) {
			t.Errorf("lower(%q) differs from lower(`latency_exp_hist`) — unary + must be a no-op on the plan", query)
		}
	})
}
