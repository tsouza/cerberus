// Tests in this file kill the LIVED gremlins mutants assigned to the
// histogram_native_binop_eq.go / histogram_native_count.go /
// histogram_native_count_values.go cluster from a phase4-promql-i mutation
// run (mutation.yml, cerberus issue #2636 — the leg whose original run
// crashed on a network flake before any mutant ran). See gremlins_kill_test.go
// for the shared file-header convention this file follows.
//
// Two INVERT_LOGICAL mutants are NOT addressed with a dedicated test here —
// both are provably equivalent, not coverage gaps:
//
//   - histogram_native_binop_eq.go:119:31 (`s.ExpHistogramTable == "" ||
//     ctx.metadataFullRange` -> `&&` inside expHistogramHistogramCompareBinop).
//   - histogram_native_binop_eq.go:191:31 (the FIRST `||` of the three-way
//     `s.ExpHistogramTable == "" || ctx.metadataFullRange || !b.ReturnBool`
//     inside expHistogramHistogramCompareBoolBinop).
//
// Both functions re-validate the identical table/metadataFullRange
// condition one level down, via isExpHistogramValuedShape(b.LHS/b.RHS, s,
// ctx) a few lines later (line 140 / line 197) — and EVERY leaf recognizer
// isExpHistogramValuedShape dispatches to (bareExpHistogramSelector,
// sumOrAvgOverExpHistogram, rangeFnOverExpHistogram, and so on) carries the
// exact same `s.ExpHistogramTable == "" || ctx.metadataFullRange` guard.
// So whenever that condition holds, isExpHistogramValuedShape is
// UNCONDITIONALLY false regardless of which branch of the outer OR/AND
// mutation let control reach it — and both functions' every rejection path
// returns the SAME literal zero-value tuple (`nil, nil, false, nil, false`),
// never a partially-populated one. There is no way for the AND mutant to
// produce a tuple the OR original could not also produce, on ANY input:
// verified by manually applying each mutation and confirming
// `go test ./internal/promql/...` (this package) stays green.
package promql

import (
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestExpHistogramHistogramCompareBoolBinop_RequiresReturnBool kills the
// CONDITIONALS_BOUNDARY... no — INVERT_LOGICAL mutant at
// histogram_native_binop_eq.go:191:56, the SECOND `||` of
//
//	if s.ExpHistogramTable == "" || ctx.metadataFullRange || !b.ReturnBool {
//
// (parsed left-associatively as `(A || B) || C`, so this is the outer
// `||` joining `(A || B)` with `C = !b.ReturnBool`). With a normal schema
// and ctx (A = B = false), the guard's truth rests entirely on C: a
// non-bool comparison (`b.ReturnBool == false`, so C == true) must be
// rejected — this recognizer is exclusively the `bool`-modifier variant;
// `expHistogramHistogramCompareBinop` (this file's own non-bool sibling)
// answers the shape without `bool`. Flipping the outer `||` to `&&` makes
// the guard depend on BOTH `(A||B)` and `C` — with A and B both false,
// `(A||B) && C` is false regardless of C, so the guard never fires and a
// non-bool `==`/`!=` between two histograms is wrongly recognised here too.
//
// Unlike the two EQUIVALENT `||`s this file's header documents, this
// mutation is NOT masked by the later isExpHistogramValuedShape re-check:
// A and B are both false in this test (a normal schema, no
// metadataFullRange), so that later check genuinely passes on real
// histogram-valued operands — nothing downstream catches the wrongly
// bypassed ReturnBool guard.
func TestExpHistogramHistogramCompareBoolBinop_RequiresReturnBool(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	b, ok := mustParse(t, `latency_exp_hist == other_exp_hist`).(*parser.BinaryExpr)
	if !ok {
		t.Fatalf("parsed %T, want *parser.BinaryExpr", mustParse(t, `latency_exp_hist == other_exp_hist`))
	}
	if _, _, _, _, ok := expHistogramHistogramCompareBoolBinop(b, s, lowerCtx{}); ok {
		t.Fatalf("expected a non-bool histogram/histogram comparison to be rejected by the " +
			"bool-modifier-only recognizer; got ok=true (mutant `||`->`&&` at " +
			"histogram_native_binop_eq.go:191:56 would accept it despite ReturnBool=false)")
	}
}

// TestExpHistogramHistogramCompareBoolBinop_BothSidesMustBeHistogramValued
// kills the INVERT_LOGICAL mutant at histogram_native_binop_eq.go:197:47:
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
			"(mutant `||`->`&&` at histogram_native_binop_eq.go:197:47 would accept it as long as " +
			"ONE side is histogram-valued)")
	}
}

// TestProjectHistogramCompareSide_ProjectionsCapacityIsTight kills the
// ARITHMETIC_BASE mutant at histogram_native_binop_eq.go:353:49 inside
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
			"histogram_native_binop_eq.go:353:49 would yield a different cap via forced regrowth)",
			gotCap, wantLen)
	}
}

// TestCountOverExpHistogram_MetadataFullRangeShortCircuits kills the
// INVERT_LOGICAL mutant at histogram_native_count.go:73:31, where
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
			"(mutant `||`->`&&` at histogram_native_count.go:73:31)")
	}
}

// TestCountOrGroupOverExpHistogramValue_MetadataFullRangeRejectsBeforeParsing
// kills the INVERT_LOGICAL mutant at histogram_native_count.go:98:31.
//
// Unlike countOverExpHistogram above, countOrGroupOverExpHistogramValue
// DOES re-validate the identical guard one level down: its own tail
// `return agg, isExpHistogramValuedShape(agg.Expr, s, ctx)` calls a
// function whose every leaf recognizer (bareExpHistogramSelector and
// friends) carries the same `s.ExpHistogramTable == "" ||
// ctx.metadataFullRange` guard, so with metadataFullRange=true the
// returned `ok` is false either way — the `bool` return alone cannot
// differentiate the mutant (this is the SAME masking
// TestExpHistogramHistogramCompareBoolBinop's file-header documents as
// equivalent for histogram_native_binop_eq.go's guards).
//
// The difference IS observable through the OTHER return value, though:
// the original guard returns literal `nil, false` — agg is never parsed.
// The `&&` mutant, with the guard bypassed, proceeds to
// `unwrapAggregateExpr` and returns a REAL, non-nil `*parser.AggregateExpr`
// alongside the (still-false, via the masked recursive check) `ok`. This
// test pins `agg == nil` on the rejection path, which the mutant cannot
// produce once it has fallen through to parse a real aggregate.
func TestCountOrGroupOverExpHistogramValue_MetadataFullRangeRejectsBeforeParsing(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	expr := mustParse(t, `count(latency_exp_hist)`)
	agg, ok := countOrGroupOverExpHistogramValue(expr, s, lowerCtx{metadataFullRange: true})
	if ok {
		t.Fatalf("expected metadataFullRange to reject recognition; got ok=true")
	}
	if agg != nil {
		t.Fatalf("expected countOrGroupOverExpHistogramValue to short-circuit BEFORE parsing the "+
			"aggregate under metadataFullRange (agg == nil); got %#v — the mutant `||`->`&&` at "+
			"histogram_native_count.go:98:31 would fall through to unwrapAggregateExpr and return a "+
			"non-nil agg even though ok stays false via the recursive isExpHistogramValuedShape guard",
			agg)
	}
}

// TestCountValuesOverExpHistogramValue_MetadataFullRangeRejectsBeforeParsing
// kills the INVERT_LOGICAL mutant at histogram_native_count_values.go:18:31
// — the SAME "agg persists past the masked guard" shape
// TestCountOrGroupOverExpHistogramValue_MetadataFullRangeRejectsBeforeParsing
// kills above, for countValuesOverExpHistogramValue's identical
// `return agg, isExpHistogramValuedShape(agg.Expr, s, ctx)` tail.
func TestCountValuesOverExpHistogramValue_MetadataFullRangeRejectsBeforeParsing(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	expr := mustParse(t, `count_values("val", latency_exp_hist)`)
	agg, ok := countValuesOverExpHistogramValue(expr, s, lowerCtx{metadataFullRange: true})
	if ok {
		t.Fatalf("expected metadataFullRange to reject recognition; got ok=true")
	}
	if agg != nil {
		t.Fatalf("expected countValuesOverExpHistogramValue to short-circuit BEFORE parsing the "+
			"aggregate under metadataFullRange (agg == nil); got %#v — the mutant `||`->`&&` at "+
			"histogram_native_count_values.go:18:31 would fall through and return a non-nil agg "+
			"even though ok stays false via the recursive isExpHistogramValuedShape guard", agg)
	}
}

// TestNativeHistogramBucketStrings_NegativeBoundsAreSignFlipped kills the
// two adjacent mutants at histogram_native_count_values.go:116:79 and
// :117:55 (ARITHMETIC_BASE `-`->`+` and INVERT_NEGATIVES `-1`->`1` — both
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
			t.Fatalf("%s multiplier = %#v, want LitFloat{-1} (mutants at "+
				"histogram_native_count_values.go:116:79 and :117:55 would flip this to 1, "+
				"dropping the negative-bucket sign flip)", label, mul.Left)
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
// adjacent mutants at histogram_native_count_values.go:187:107
// (ARITHMETIC_BASE `-`->`+` and INVERT_NEGATIVES `-1`->`1`, the same
// observable-effect pair as the 116/117 kill above) inside
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
			"histogram_native_count_values.go:187:107 would flip this to 1, negating the "+
			"exponent's sign)", mul.Left)
	}
	if !mul.Right.Equal(scale) {
		t.Fatalf("mul.Right = %#v, want the scale expr unchanged", mul.Right)
	}
}
