// Tests in this file kill the LIVED gremlins mutants assigned to the
// histogram_native_mixed_or_label.go / histogram_native_mixed_or_vector_
// comparison.go / histogram_native_value_producing_call.go cluster from a
// phase4-promql-i mutation run (mutation.yml, cerberus issue #2636 — the
// leg whose original run crashed on a network flake before any mutant
// ran). See gremlins_kill_test.go for the shared file-header convention
// this file follows.
//
// One CONDITIONALS_BOUNDARY mutant is NOT addressed with a dedicated test
// here — it is provably equivalent, not a coverage gap:
//
//   - histogram_native_mixed_or_vector_comparison.go:`len(b.VectorMatching.Include) > 0`
//     (`len(b.VectorMatching.Include) > 0` -> `>= 0`, inside
//     comparisonVectorVectorOverMixedExpHistogramSetOp).
//
// The guarded statement is `include = append([]string(nil),
// b.VectorMatching.Include...)`. Go's append, given zero elements to
// append (whether the source is nil or a non-nil empty slice), always
// returns its destination UNCHANGED — verified directly:
// `append([]string(nil))` and `append([]string(nil), []string{}...)` both
// evaluate to nil. So whenever len(Include) == 0 (the only value the `>
// 0` vs `>= 0` boundary can possibly disagree on), running the append
// unconditionally (mutant) or skipping it (original) produces the exact
// same `include == nil` result either way; for len(Include) > 0 both
// branches run the identical copy. No input can make the two branches
// disagree. Confirmed by manually applying the mutation and running
// `go test ./internal/promql/...`: green.
package promql

import (
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLabelCallOverMixedExpHistogramSetOp_LabelJoinMinArity kills the
// CONDITIONALS_BOUNDARY mutant at histogram_native_mixed_or_label.go:`len(call.Args) < 3`
// (`len(call.Args) < 3` -> `<= 3`) inside labelCallOverMixedExpHistogramSetOp's
// label_join arm. The minimum valid label_join call has exactly 3 args
// (vector, dst, separator, zero src labels) — label_fns.go's own arity
// error confirms 3 is the floor. The parser itself refuses this degenerate
// shape (real label_join calls always carry at least one src label in
// practice, but nothing in the grammar requires it), so the Call is built
// by hand — mirrors
// TestLabelCallOverExpHistogramDroppingShape_LabelJoinMinArity's identical
// technique for the (non-mixed) drop-family sibling
// (histogram_native_range_family_gremlins_test.go).
func TestLabelCallOverMixedExpHistogramSetOp_LabelJoinMinArity(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	call := &parser.Call{
		Func: parser.MustGetFunction("label_join"),
		Args: parser.Expressions{
			mustParse(t, `latency_exp_hist or other_metric`),
			&parser.StringLiteral{Val: "dst"},
			&parser.StringLiteral{Val: "-"},
		},
	}
	if _, _, ok := labelCallOverMixedExpHistogramSetOp(call, s, lowerCtx{}); !ok {
		t.Fatalf("expected minimum-arity (3-arg) label_join wrapping a mixed histogram/float `or` " +
			"to be recognised; got ok=false (mutant `<`->`<=` at " +
			"histogram_native_mixed_or_label.go:`if len(call.Args) < 3`)")
	}
}

// TestLowerMixedVVCompareBool_HistCmpNegationMatchesOp kills the
// CONDITIONALS_NEGATION mutant at
// histogram_native_mixed_or_vector_comparison.go:lowerMixedVVCompareBool:`histCmp := mixedVVHistogramFieldsExpr(op == chplan.OpNe)`
// (`op == chplan.OpNe` -> `!=`) inside lowerMixedVVCompareBool's
// histogram,histogram branch:
//
//	histCmp := mixedVVHistogramFieldsExpr(op == chplan.OpNe)
//
// mixedVVHistogramFieldsExpr(ne) combines its nine per-field comparisons
// with AND (ne=false, `==`) or OR (ne=true, `!=`) — the top-level Binary's
// Op after the fold is exactly that combine operator. Flipping the `==` to
// `!=` at the call site swaps which literal op value (Eq vs Ne) drives
// ne=true, so an actual `==` would wrongly use OR-of-unequal and an actual
// `!=` would wrongly use AND-of-equal.
//
// Driven directly against lowerMixedVVCompareBool (a zero-value
// *chplan.MixedVectorJoin is safe here — every helper it calls reads only
// Match/Card/Include, never Left/Right) rather than through a full lower(),
// so the structural check ties to the exact call site without depending on
// [chplan.MixedVectorJoin]'s own JOIN semantics.
func TestLowerMixedVVCompareBool_HistCmpNegationMatchesOp(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	join := &chplan.MixedVectorJoin{Card: chplan.CardOneToOne}

	histCmpCombine := func(t *testing.T, op chplan.BinaryOp) chplan.BinaryOp {
		t.Helper()
		plan := lowerMixedVVCompareBool(join, op, s)
		proj, ok := plan.(*chplan.Project)
		if !ok {
			t.Fatalf("lowerMixedVVCompareBool(op=%s) = %T, want *chplan.Project", op, plan)
		}
		const valueProjIdx = 3
		if len(proj.Projections) <= valueProjIdx {
			t.Fatalf("Projections has %d entries, want more than %d", len(proj.Projections), valueProjIdx)
		}
		fc, ok := proj.Projections[valueProjIdx].Expr.(*chplan.FuncCall)
		if !ok || fc.Fn != chplan.FnIf {
			t.Fatalf("Projections[%d].Expr = %#v, want an if(bothFloat, floatCmp, histCmp) FuncCall",
				valueProjIdx, proj.Projections[valueProjIdx].Expr)
		}
		const histArgIdx = 2
		if len(fc.Args) <= histArgIdx {
			t.Fatalf("if(...) has %d args, want more than %d", len(fc.Args), histArgIdx)
		}
		histWrap, ok := fc.Args[histArgIdx].(*chplan.FuncCall)
		if !ok || histWrap.Fn != chplan.FnToFloat64 {
			t.Fatalf("if(...)'s hist-branch arg = %#v, want a toFloat64(...) FuncCall", fc.Args[histArgIdx])
		}
		if len(histWrap.Args) != 1 {
			t.Fatalf("toFloat64(...) has %d args, want exactly 1", len(histWrap.Args))
		}
		bin, ok := histWrap.Args[0].(*chplan.Binary)
		if !ok {
			t.Fatalf("toFloat64's arg = %#v, want a top-level *chplan.Binary (the field-comparison fold)", histWrap.Args[0])
		}
		return bin.Op
	}

	if got := histCmpCombine(t, chplan.OpEq); got != chplan.OpAnd {
		t.Fatalf("op=EQ: histCmp top combine = %s, want AND (ne=false) — mutant `==`->`!=` at "+
			"histogram_native_mixed_or_vector_comparison.go:lowerMixedVVCompareBool:`histCmp := mixedVVHistogramFieldsExpr(op == chplan.OpNe)` would pass ne=true here and use OR",
			got)
	}
	if got := histCmpCombine(t, chplan.OpNe); got != chplan.OpOr {
		t.Fatalf("op=NE: histCmp top combine = %s, want OR (ne=true) — mutant `==`->`!=` at "+
			"histogram_native_mixed_or_vector_comparison.go:lowerMixedVVCompareBool:`histCmp := mixedVVHistogramFieldsExpr(op == chplan.OpNe)` would pass ne=false here and use AND",
			got)
	}
}

// TestHistogramValuedProducerCall_InfoAcceptsTwoArgs kills the
// CONDITIONALS_BOUNDARY mutant at
// histogram_native_value_producing_call.go:`len(call.Args) > 2` (`len(call.Args) > 2` ->
// `>= 2`) inside histogramValuedProducerCall's info() arm. info() accepts
// 1 or 2 arguments; exactly 2 is the valid upper boundary and must NOT be
// rejected.
func TestHistogramValuedProducerCall_InfoAcceptsTwoArgs(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	call := &parser.Call{
		Func: parser.MustGetFunction("info"),
		Args: parser.Expressions{
			mustParseVectorSelector(t, "latency_exp_hist"),
			&parser.StringLiteral{Val: "service"},
		},
	}
	if _, ok := histogramValuedProducerCall(call, s, lowerCtx{}); !ok {
		t.Fatalf("expected info() with exactly 2 args over a histogram-valued base to be " +
			"recognised; got ok=false (mutant `>`->`>=` at " +
			"histogram_native_value_producing_call.go:`if len(call.Args) < 1 || len(call.Args) > 2` would reject the 2-arg boundary)")
	}
}

// TestHistogramValuedProducerCall_SortByLabelAcceptsSingleArg kills the
// CONDITIONALS_BOUNDARY mutant at
// histogram_native_value_producing_call.go:`if len(call.Args) < 1 {` (`len(call.Args) < 1` ->
// `<= 1`) inside histogramValuedProducerCall's sort_by_label(_desc) arm.
// sort_by_label accepts the vector argument alone (zero label names); the
// parser's own grammar allows this shape (no minimum label-name count),
// and the boundary — exactly 1 argument — must NOT be rejected.
func TestHistogramValuedProducerCall_SortByLabelAcceptsSingleArg(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	call := &parser.Call{
		Func: parser.MustGetFunction("sort_by_label"),
		Args: parser.Expressions{mustParseVectorSelector(t, "latency_exp_hist")},
	}
	if _, ok := histogramValuedProducerCall(call, s, lowerCtx{}); !ok {
		t.Fatalf("expected sort_by_label() with exactly 1 arg (no label names) over a " +
			"histogram-valued base to be recognised; got ok=false (mutant `<`->`<=` at " +
			"histogram_native_value_producing_call.go:`if len(call.Args) < 1 {` would reject the 1-arg boundary)")
	}
}
