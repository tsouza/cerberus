// Tests in this file kill the LIVED gremlins mutants assigned to the
// histogram_native_binop_eq.go / histogram_native_count.go /
// histogram_native_count_values.go cluster from a phase4-promql-i mutation
// run (mutation.yml, cerberus issue #2636 — the leg whose original run
// crashed on a network flake before any mutant ran). See gremlins_kill_test.go
// for the shared file-header convention this file follows.
//
// This file used to open with an equivalence adjudication for two
// INVERT_LOGICAL mutants on histogram_native_binop_eq.go's copies of the
// exp-histogram availability rule, inside expHistogramHistogramCompareBinop
// and expHistogramHistogramCompareBoolBinop. Both copies decided nothing —
// each function re-derives the identical verdict a few lines later through
// isExpHistogramValuedShape — so neither mutant could ever be killed.
// Cerberus issue #2963 deleted those copies along with the other nineteen
// composite ones, leaving the rule stated once in
// [expHistogramLoweringAvailable]; the mutants no longer exist, so the
// adjudication has been retired rather than restated.
package promql

import (
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestExpHistogramHistogramCompareBoolBinop_RequiresReturnBool pins
//
//	histogram_native_binop_eq.go:expHistogramHistogramCompareBoolBinop:`if !b.ReturnBool {`
//
// the recognizer's exclusivity to the `bool`-modifier variant: a non-bool
// comparison (`b.ReturnBool == false`) must be rejected, because
// [expHistogramHistogramCompareBinop] — this file's own non-bool sibling —
// answers that shape instead.
//
// Until cerberus issue #2963 this guard read
// `s.ExpHistogramTable == "" || ctx.metadataFullRange || !b.ReturnBool`,
// and the test existed to kill the INVERT_LOGICAL mutant on its SECOND
// `||`: with a normal schema and ctx the first two disjuncts are false, so
// `(A||B) && C` never fires and a non-bool comparison was wrongly accepted.
// The first two disjuncts were the composite copy of the availability rule
// and are gone; `!b.ReturnBool` is what actually decided this case and is
// all that remains. The condition now carries no logical operator and so no
// mutant, but the contract it states is real and this test keeps pinning
// it.
func TestExpHistogramHistogramCompareBoolBinop_RequiresReturnBool(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	b, ok := mustParse(t, `latency_exp_hist == other_exp_hist`).(*parser.BinaryExpr)
	if !ok {
		t.Fatalf("parsed %T, want *parser.BinaryExpr", mustParse(t, `latency_exp_hist == other_exp_hist`))
	}
	if _, _, _, _, ok := expHistogramHistogramCompareBoolBinop(b, s, lowerCtx{}); ok {
		t.Fatalf("expected a non-bool histogram/histogram comparison to be rejected by the " +
			"bool-modifier-only recognizer; got ok=true despite ReturnBool=false " +
			"(histogram_native_binop_eq.go:expHistogramHistogramCompareBoolBinop:`if !b.ReturnBool {`)")
	}
}

// TestExpHistogramHistogramCompareBoolBinop_BothSidesMustBeHistogramValued
// kills the INVERT_LOGICAL mutant at
// histogram_native_binop_eq.go:expHistogramHistogramCompareBoolBinop:`!isExpHistogramValuedShape(b.LHS, s, ctx) || !isExpHistogramValuedShape(b.RHS, s, ctx)`:
//
//	if !isExpHistogramValuedShape(b.LHS, s, ctx) || !isExpHistogramValuedShape(b.RHS, s, ctx) {
//
// Reference rejects a `bool`-modified `==`/`!=` between a histogram and a
// float operand outright (NewIncompatibleTypesInBinOpInfo). The original
// `||` rejects whenever EITHER side is not histogram-valued. Flipping to
// `&&` rejects only when BOTH sides are not histogram-valued — accepting a
// mismatched histogram/float pair as long as at least one side qualifies.
//
// latency_exp_hist (histogram) == bool other_metric (float, no
// _exp_hist suffix) exercises exactly that asymmetric case.
func TestExpHistogramHistogramCompareBoolBinop_BothSidesMustBeHistogramValued(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	expr := mustParse(t, `latency_exp_hist == bool other_metric`)
	b, ok := expr.(*parser.BinaryExpr)
	if !ok {
		t.Fatalf("parsed %T, want *parser.BinaryExpr", expr)
	}
	if _, _, _, _, ok := expHistogramHistogramCompareBoolBinop(b, s, lowerCtx{}); ok {
		t.Fatalf("expected a histogram/float mismatched bool-compare to be rejected; got ok=true " +
			"(mutant `||`->`&&` at histogram_native_binop_eq.go:expHistogramHistogramCompareBoolBinop:" +
			"`!isExpHistogramValuedShape(b.LHS, s, ctx) || !isExpHistogramValuedShape(b.RHS, s, ctx)` " +
			"would accept it as long as " +
			"ONE side is histogram-valued)")
	}
}

// TestProjectHistogramCompareSide_ProjectionsCapacityIsTight kills the
// ARITHMETIC_BASE mutant at histogram_native_binop_eq.go:`projs := make([]chplan.Projection, 0, len(cols)+1)` inside
// projectHistogramCompareSide's slice-capacity hint:
//
//	projs := make([]chplan.Projection, 0, len(cols)+1)
//
// The function appends exactly one projection per `cols` entry plus one
// more for the [histEqSideAlias] discriminator — len(cols)+1 appends total
// — so the original capacity is an exact fit. Flipping `+` to `-` shrinks
// the hint to len(cols)-1, forcing `append`'s growth path to reallocate,
// which lands on a capacity other than len(cols)+1 (mirrors
// TestLowerLabelJoin_SrcsSliceCapacityIsTight's own strategy,
// gremlins_kill_test.go).
func TestProjectHistogramCompareSide_ProjectionsCapacityIsTight(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	histSchema := histogramProjectionSchema(s)
	hp := &chplan.Scan{Table: s.ExpHistogramTable}

	got := projectHistogramCompareSide(hp, histEqSideLHS, histSchema, true /* stepAligned */)

	cols := []string{histSchema.MetricNameColumn, histSchema.AttributesColumn, histSchema.TimestampColumn}
	cols = append(cols, histogramCompareFieldColumns(histSchema)...)
	wantLen := len(cols) + 1

	if gotLen := len(got.Projections); gotLen != wantLen {
		t.Fatalf("len(Projections) = %d, want %d", gotLen, wantLen)
	}
	if gotCap := cap(got.Projections); gotCap != wantLen {
		t.Fatalf("cap(Projections) = %d, want %d (mutant `+`->`-` at "+
			"histogram_native_binop_eq.go:`projs := make([]chplan.Projection, 0, len(cols)+1)` would yield a different cap via forced regrowth)",
			gotCap, wantLen)
	}
}

// TestCountOverExpHistogram_MetadataFullRangeShortCircuits kills the
// INVERT_LOGICAL mutant at histogram_native_availability.go:expHistogramLoweringAvailable:`s.ExpHistogramTable != "" && !ctx.metadataFullRange`, where
//
//	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
//
// must reject on EITHER condition. countOverExpHistogram has no downstream
// call that independently re-checks this same guard (unwrapAggregateExpr,
// unwrapVectorSelector and IsExpHistogramMetric are all guard-free leaf
// checks — unlike countOrGroupOverExpHistogramValue just below, which
// delegates to isExpHistogramValuedShape), so metadataFullRange alone is a
// clean, unmasked differentiator — mirrors
// TestCountPresentOverExpHistogram_MetadataFullRangeShortCircuits
// (histogram_native_range_family_gremlins_test.go).
func TestCountOverExpHistogram_MetadataFullRangeShortCircuits(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	expr := mustParse(t, `count(latency_exp_hist)`)
	if _, _, ok := countOverExpHistogram(expr, s, lowerCtx{metadataFullRange: true}); ok {
		t.Fatalf("expected metadataFullRange to reject recognition; got ok=true " +
			"(mutant `||`->`&&` at histogram_native_count.go:countOverExpHistogram:" +
			"`s.ExpHistogramTable == \"\" || ctx.metadataFullRange`)")
	}
}

// TestCountOrGroupOverExpHistogramValue_MetadataFullRangeRejectsBeforeParsing
// pins the zero-value-tuple contract at
//
//	histogram_native_count.go:countOrGroupOverExpHistogramValue:`if !isExpHistogramValuedShape(agg.Expr, s, ctx) {`
//
// A rejection must answer `nil, false`, never a real `*parser.AggregateExpr`
// alongside a false `ok`. Every sibling exp-histogram recognizer keeps that
// contract, and all three of this function's callers scope the aggregate to
// the `ok` branch — but a caller that stopped doing so would read a
// populated value out of a rejection, which is the failure this pins.
//
// Until cerberus issue #2963 the function opened with a copy of the
// exp-histogram availability rule and closed with
// `return agg, isExpHistogramValuedShape(agg.Expr, s, ctx)`. That copy
// decided nothing about `ok` — the tail re-derives the identical verdict —
// so its INVERT_LOGICAL mutant was killable only through this second return
// value, which the copy alone kept at nil. Deleting the copy along with the
// other twenty composite ones would have started answering
// `(non-nil agg, false)`, so the tail was normalised to an explicit
// rejection instead, and the contract survives the deletion. The condition
// now carries no logical operator and so no mutant; the contract is real
// and this test keeps pinning it.
func TestCountOrGroupOverExpHistogramValue_MetadataFullRangeRejectsBeforeParsing(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	expr := mustParse(t, `count(latency_exp_hist)`)
	agg, ok := countOrGroupOverExpHistogramValue(expr, s, lowerCtx{metadataFullRange: true})
	if ok {
		t.Fatalf("expected metadataFullRange to reject recognition; got ok=true")
	}
	if agg != nil {
		t.Fatalf("expected countOrGroupOverExpHistogramValue to answer the zero-value tuple when "+
			"it rejects under metadataFullRange (agg == nil); got %#v — a rejection must never "+
			"hand back a populated aggregate alongside ok=false "+
			"(histogram_native_count.go:countOrGroupOverExpHistogramValue:"+
			"`if !isExpHistogramValuedShape(agg.Expr, s, ctx) {`)", agg)
	}
}

// TestCountValuesOverExpHistogramValue_MetadataFullRangeRejectsBeforeParsing
// pins the same zero-value-tuple contract at
//
//	histogram_native_count_values.go:`if !isExpHistogramValuedShape(agg.Expr, s, ctx) {`
//
// for [countValuesOverExpHistogramValue], whose tail cerberus issue #2963
// normalised the same way and for the same reason —
// TestCountOrGroupOverExpHistogramValue_MetadataFullRangeRejectsBeforeParsing
// above carries the full account.
func TestCountValuesOverExpHistogramValue_MetadataFullRangeRejectsBeforeParsing(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	expr := mustParse(t, `count_values("val", latency_exp_hist)`)
	agg, ok := countValuesOverExpHistogramValue(expr, s, lowerCtx{metadataFullRange: true})
	if ok {
		t.Fatalf("expected metadataFullRange to reject recognition; got ok=true")
	}
	if agg != nil {
		t.Fatalf("expected countValuesOverExpHistogramValue to answer the zero-value tuple when "+
			"it rejects under metadataFullRange (agg == nil); got %#v — a rejection must never "+
			"hand back a populated aggregate alongside ok=false "+
			"(histogram_native_count_values.go:`if !isExpHistogramValuedShape(agg.Expr, s, ctx) {`)", agg)
	}
}

// TestNativeHistogramBucketStrings_NegativeBoundsAreSignFlipped kills the
// two adjacent mutants at
// histogram_native_count_values.go:`histStringBinary(chplan.OpMul, &chplan.LitFloat{V: -1}, upperBound)`
// and histogram_native_count_values.go:`histStringBinary(chplan.OpMul, &chplan.LitFloat{V: -1}, lowerBound)`
// (ARITHMETIC_BASE `-`->`+` and INVERT_NEGATIVES `-1`->`1` — both
// produce the identical observable effect, the multiplier flips from -1 to
// 1) inside nativeHistogramBucketStrings's negative-bucket sign flip:
//
//	if !positive {
//		lowerBound, upperBound = histStringBinary(chplan.OpMul, &chplan.LitFloat{V: -1}, upperBound),
//			histStringBinary(chplan.OpMul, &chplan.LitFloat{V: -1}, lowerBound)
//	}
//
// Negative-side bucket boundaries are stored as positive magnitudes and
// must be negated (and swapped) to render the actual negative-valued
// bounds FloatHistogram.String prints. Rather than rendering the full SQL
// (a deep arrayMap/hqLet nest), this test descends the returned
// chplan.Expr tree directly to the two multiply nodes and asserts each
// multiplier is exactly -1 — the mutation's only observable effect.
func TestNativeHistogramBucketStrings_NegativeBoundsAreSignFlipped(t *testing.T) {
	t.Parallel()

	buckets := &chplan.ColumnRef{Name: "neg_bucket_counts"}
	offset := &chplan.ColumnRef{Name: "neg_offset"}
	scale := &chplan.ColumnRef{Name: "scale"}

	got := nativeHistogramBucketStrings(buckets, offset, scale, false /* positive */)

	reverseCall, ok := got.(*chplan.FuncCall)
	if !ok || reverseCall.Fn != chplan.FnArrayReverse {
		t.Fatalf("nativeHistogramBucketStrings(..., positive=false) = %#v, want outer "+
			"arrayReverse(...) FuncCall", got)
	}
	if len(reverseCall.Args) != 1 {
		t.Fatalf("arrayReverse args = %#v, want exactly 1", reverseCall.Args)
	}
	mappedCall, ok := reverseCall.Args[0].(*chplan.FuncCall)
	if !ok || mappedCall.Fn != chplan.FnArrayMap {
		t.Fatalf("arrayReverse's arg = %#v, want arrayMap(...) FuncCall", reverseCall.Args[0])
	}
	if len(mappedCall.Args) != 2 {
		t.Fatalf("arrayMap args = %#v, want exactly 2", mappedCall.Args)
	}
	lambda, ok := mappedCall.Args[0].(*chplan.Lambda)
	if !ok {
		t.Fatalf("arrayMap's first arg = %#v, want *chplan.Lambda", mappedCall.Args[0])
	}
	body, ok := lambda.Body.(*chplan.FuncCall)
	if !ok || body.Fn != chplan.FnConcat {
		t.Fatalf("lambda body = %#v, want concat(...) FuncCall", lambda.Body)
	}
	const lowerBoundArgIdx, upperBoundArgIdx = 1, 3
	if len(body.Args) <= upperBoundArgIdx {
		t.Fatalf("concat has %d args, want more than %d", len(body.Args), upperBoundArgIdx)
	}

	assertNegatedBound := func(label string, e chplan.Expr) {
		t.Helper()
		val := unwrapHqLetBoundValue(t, e)
		mul, ok := val.(*chplan.Binary)
		if !ok || mul.Op != chplan.OpMul {
			t.Fatalf("%s = %#v, want the `-1 * <bound>` Binary the negative-bucket sign flip builds", label, val)
		}
		lit, ok := mul.Left.(*chplan.LitFloat)
		if !ok || lit.V != -1 {
			t.Fatalf("%s multiplier = %#v, want LitFloat{-1} (the mutants at "+
				"histogram_native_count_values.go:`histStringBinary(chplan.OpMul, &chplan.LitFloat{V: -1}, upperBound)` "+
				"and histogram_native_count_values.go:`histStringBinary(chplan.OpMul, &chplan.LitFloat{V: -1}, lowerBound)` "+
				"would flip this to 1, dropping the negative-bucket sign flip)", label, mul.Left)
		}
	}
	assertNegatedBound("lowerBound (rendered from the swapped upperBound expr)", body.Args[lowerBoundArgIdx])
	assertNegatedBound("upperBound (rendered from the swapped lowerBound expr)", body.Args[upperBoundArgIdx])
}

// TestNativeHistogramBucketStrings_PositiveBoundsAreNotSignFlipped is the
// positive-side control for the test above: with positive=true, the `if
// !positive` swap never runs, so neither bound is wrapped in a `-1 *`
// Binary — it stays the bare nativeHistogramBoundExpr call. This confirms
// the assertion above genuinely distinguishes the two branches rather than
// vacuously matching any expression shape.
func TestNativeHistogramBucketStrings_PositiveBoundsAreNotSignFlipped(t *testing.T) {
	t.Parallel()

	buckets := &chplan.ColumnRef{Name: "pos_bucket_counts"}
	offset := &chplan.ColumnRef{Name: "pos_offset"}
	scale := &chplan.ColumnRef{Name: "scale"}

	got := nativeHistogramBucketStrings(buckets, offset, scale, true /* positive */)

	mappedCall, ok := got.(*chplan.FuncCall)
	if !ok || mappedCall.Fn != chplan.FnArrayMap {
		t.Fatalf("nativeHistogramBucketStrings(..., positive=true) = %#v, want arrayMap(...) FuncCall", got)
	}
	lambda, ok := mappedCall.Args[0].(*chplan.Lambda)
	if !ok {
		t.Fatalf("arrayMap's first arg = %#v, want *chplan.Lambda", mappedCall.Args[0])
	}
	body, ok := lambda.Body.(*chplan.FuncCall)
	if !ok || body.Fn != chplan.FnConcat {
		t.Fatalf("lambda body = %#v, want concat(...) FuncCall", lambda.Body)
	}
	const lowerBoundArgIdx = 1
	val := unwrapHqLetBoundValue(t, body.Args[lowerBoundArgIdx])
	if _, isMul := val.(*chplan.Binary); isMul {
		t.Fatalf("positive-side lowerBound = %#v, want the bare nativeHistogramBoundExpr call "+
			"(no sign-flip multiply) — got a Binary instead", val)
	}
	if _, isPow := val.(*chplan.FuncCall); !isPow {
		t.Fatalf("positive-side lowerBound = %#v, want a pow(...) FuncCall "+
			"(nativeHistogramBoundExpr's own shape)", val)
	}
}

// unwrapHqLetBoundValue recovers the raw value hqLet bound, given the
// result of nativeHistogramFloatString(value) — hqLet renders
// `arrayMap(<param> -> <body>, array(<val>))[1]`, so `val` sits at
// Container.(*FuncCall).Args[1].(*FuncCall /* array */).Args[0].
func unwrapHqLetBoundValue(t *testing.T, e chplan.Expr) chplan.Expr {
	t.Helper()
	sub, ok := e.(*chplan.Subscript)
	if !ok {
		t.Fatalf("expected *chplan.Subscript (hqLet's own shape), got %T: %#v", e, e)
	}
	fm, ok := sub.Container.(*chplan.FuncCall)
	if !ok || fm.Fn != chplan.FnArrayMap {
		t.Fatalf("hqLet Container = %#v, want an arrayMap(...) FuncCall", sub.Container)
	}
	if len(fm.Args) != 2 {
		t.Fatalf("hqLet arrayMap args = %#v, want exactly 2", fm.Args)
	}
	arrayCall, ok := fm.Args[1].(*chplan.FuncCall)
	if !ok || arrayCall.Fn != chplan.FnArray {
		t.Fatalf("hqLet's second arrayMap arg = %#v, want an array(...) FuncCall", fm.Args[1])
	}
	if len(arrayCall.Args) != 1 {
		t.Fatalf("hqLet's array(...) call has %d args, want exactly 1", len(arrayCall.Args))
	}
	return arrayCall.Args[0]
}

// TestNativeHistogramBoundExpr_ScaleExponentIsNegated kills the two
// adjacent mutants at
// histogram_native_count_values.go:`histStringBinary(chplan.OpMul, &chplan.LitFloat{V: -1}, scale)`
// (ARITHMETIC_BASE `-`->`+` and INVERT_NEGATIVES `-1`->`1`, the same
// observable-effect pair as the negative-bucket kill above) inside
// nativeHistogramBoundExpr's growth-factor base:
//
//	base := histStringCall(chplan.FnPow, &chplan.LitFloat{V: 2},
//		histStringCall(chplan.FnPow, &chplan.LitFloat{V: 2},
//			histStringBinary(chplan.OpMul, &chplan.LitFloat{V: -1}, scale)))
//
// base = 2^(2^-scale) — the OTel exponential-histogram growth factor.
// Flipping the exponent's sign inverts growth vs. shrinkage as scale
// increases, silently misrendering every bucket boundary. Asserted
// structurally (no chsql rendering needed): descend to the `-1 * scale`
// Binary and check the multiplier.
func TestNativeHistogramBoundExpr_ScaleExponentIsNegated(t *testing.T) {
	t.Parallel()

	index := &chplan.ColumnRef{Name: "idx"}
	scale := &chplan.ColumnRef{Name: "scale"}

	got := nativeHistogramBoundExpr(index, scale)

	outer, ok := got.(*chplan.FuncCall)
	if !ok || outer.Fn != chplan.FnPow {
		t.Fatalf("nativeHistogramBoundExpr(...) = %#v, want the outer pow(base, index) FuncCall", got)
	}
	if len(outer.Args) != 2 {
		t.Fatalf("outer pow args = %#v, want exactly 2", outer.Args)
	}
	if !outer.Args[1].Equal(index) {
		t.Fatalf("outer pow's second arg = %#v, want the index expr unchanged", outer.Args[1])
	}
	base, ok := outer.Args[0].(*chplan.FuncCall)
	if !ok || base.Fn != chplan.FnPow {
		t.Fatalf("base = %#v, want the inner pow(2, pow(2, -scale)) FuncCall", outer.Args[0])
	}
	if len(base.Args) != 2 {
		t.Fatalf("base pow args = %#v, want exactly 2", base.Args)
	}
	innerPow, ok := base.Args[1].(*chplan.FuncCall)
	if !ok || innerPow.Fn != chplan.FnPow {
		t.Fatalf("base.Args[1] = %#v, want the pow(2, -scale) FuncCall", base.Args[1])
	}
	if len(innerPow.Args) != 2 {
		t.Fatalf("innerPow args = %#v, want exactly 2", innerPow.Args)
	}
	mul, ok := innerPow.Args[1].(*chplan.Binary)
	if !ok || mul.Op != chplan.OpMul {
		t.Fatalf("innerPow.Args[1] = %#v, want the `-1 * scale` Binary", innerPow.Args[1])
	}
	lit, ok := mul.Left.(*chplan.LitFloat)
	if !ok || lit.V != -1 {
		t.Fatalf("mul.Left = %#v, want LitFloat{-1} (mutants at "+
			"histogram_native_count_values.go:`histStringBinary(chplan.OpMul, &chplan.LitFloat{V: -1}, scale)` "+
			"would flip this to 1, negating the "+
			"exponent's sign)", mul.Left)
	}
	if !mul.Right.Equal(scale) {
		t.Fatalf("mul.Right = %#v, want the scale expr unchanged", mul.Right)
	}
}
