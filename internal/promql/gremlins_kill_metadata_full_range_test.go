// Tests in this file kill the `INVERT_LOGICAL` mutants that rewrite the `||`
// of
//
//	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
//
// into `&&`, at the LEAF exp-histogram recognizers — the ones that decide a
// shape by asking the SCHEMA whether the selected metric is an exp-histogram
// (`s.IsExpHistogramMetric`), which reads nothing from `ctx`. See
// gremlins_kill_test.go for the shared file-header convention this file
// follows; cerberus issue #2949 owns the legs these mutants were reported on
// (phase4-promql-b / -d / -f / -i / -other).
//
// Why the leaf/composite distinction is the whole story here
// ---------------------------------------------------------
// The guard is one condition with two independent reasons to bail: the schema
// declares no exp-histogram table at all, or this lowering is a Prometheus
// metadata full-range walk, which must never be answered from the
// exp-histogram table. The original bails when EITHER holds. The `&&` mutant
// bails only when BOTH hold, so the two differ exactly when one holds and the
// other does not — and the reachable half of that is a metadata full-range
// lowering against a schema that DOES declare the table.
//
// At a leaf recognizer nothing downstream re-checks `ctx`: the remaining
// guards are shape guards (is this a Call, a MatrixSelector, a positive
// range) plus `s.IsExpHistogramMetric`, which consults only the metric name
// and the schema's suffix. So with `metadataFullRange: true` and a normal
// schema the mutant runs the whole recognizer to completion and ACCEPTS a
// shape the original rejects — an observable difference, and what each test
// below pins.
//
// At a COMPOSITE recognizer the same mutation is equivalent, because the
// composite re-validates the identical condition one level down through
// `isExpHistogramValuedShape` / `isExpHistogramDroppingShape`, whose every
// leaf carries this very guard. Those sites are adjudicated in this file's
// NOT KILLABLE footer rather than tested, on the same ground
// gremlins_kill_histogram_binop_count_test.go's header already records for
// histogram_native_binop_eq.go's two.
//
// Each test asserts BOTH directions. The negative assertion alone would pass
// vacuously against any expression the recognizer rejects for an unrelated
// reason (a typo in the query, a metric name the schema does not treat as an
// exp-histogram), so every test first pins that the SAME expression IS
// recognised with `metadataFullRange` false. That positive control is what
// makes the negative one evidence.
package promql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/schema"
)

// TestLastFirstOverExpHistogram_MetadataFullRangeRejects kills the
// INVERT_LOGICAL mutant on the `||` of
//
//	histogram_native_last_first_over_time.go:lastFirstOverExpHistogram:`s.ExpHistogramTable == "" || ctx.metadataFullRange`
//
// inside [lastFirstOverExpHistogram], the `last_over_time` /
// `first_over_time` recognizer.
//
// The recognizer's only other schema question is
// `s.IsExpHistogramMetric(...)`, which is true here regardless of `ctx`, so
// under the mutant the metadata full-range lowering is recognised as an
// exp-histogram window shape.
func TestLastFirstOverExpHistogram_MetadataFullRangeRejects(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	const q = `last_over_time(latency_exp_hist[5m])`
	expr := mustParse(t, q)

	if _, _, _, ok := lastFirstOverExpHistogram(expr, s, lowerCtx{}); !ok {
		t.Fatalf("positive control: lastFirstOverExpHistogram(%q) = ok false with an ordinary ctx; "+
			"the negative assertion below would then hold for the wrong reason", q)
	}
	if _, _, _, ok := lastFirstOverExpHistogram(expr, s, lowerCtx{metadataFullRange: true}); ok {
		t.Fatalf("lastFirstOverExpHistogram(%q) = ok true under metadataFullRange; a metadata "+
			"full-range lowering must never be answered from the exp-histogram table (mutant "+
			"`||`->`&&` on the ExpHistogramTable/metadataFullRange guard would accept it)", q)
	}
}

// TestOverTimeOverExpHistogram_MetadataFullRangeRejects kills the
// INVERT_LOGICAL mutant on the `||` of
//
//	histogram_native_over_time.go:overTimeOverExpHistogram:`s.ExpHistogramTable == "" || ctx.metadataFullRange`
//
// the same guard inside [overTimeOverExpHistogram], the `sum_over_time` /
// `avg_over_time` recognizer.
func TestOverTimeOverExpHistogram_MetadataFullRangeRejects(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	const q = `sum_over_time(latency_exp_hist[5m])`
	expr := mustParse(t, q)

	if _, ok := overTimeOverExpHistogram(expr, s, lowerCtx{}); !ok {
		t.Fatalf("positive control: overTimeOverExpHistogram(%q) = ok false with an ordinary ctx; "+
			"the negative assertion below would then hold for the wrong reason", q)
	}
	if _, ok := overTimeOverExpHistogram(expr, s, lowerCtx{metadataFullRange: true}); ok {
		t.Fatalf("overTimeOverExpHistogram(%q) = ok true under metadataFullRange; a metadata "+
			"full-range lowering must never be answered from the exp-histogram table (mutant "+
			"`||`->`&&` on the ExpHistogramTable/metadataFullRange guard would accept it)", q)
	}
}

// TestResetsOrChangesOverExpHistogram_MetadataFullRangeRejects kills the
// INVERT_LOGICAL mutant on the `||` of
//
//	histogram_native_resets.go:resetsOrChangesOverExpHistogram:`s.ExpHistogramTable == "" || ctx.metadataFullRange`
//
// the same guard inside [resetsOrChangesOverExpHistogram], the `resets` /
// `changes` recognizer.
func TestResetsOrChangesOverExpHistogram_MetadataFullRangeRejects(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	const q = `resets(latency_exp_hist[5m])`
	expr := mustParse(t, q)

	if _, ok := resetsOrChangesOverExpHistogram(expr, s, lowerCtx{}); !ok {
		t.Fatalf("positive control: resetsOrChangesOverExpHistogram(%q) = ok false with an ordinary "+
			"ctx; the negative assertion below would then hold for the wrong reason", q)
	}
	if _, ok := resetsOrChangesOverExpHistogram(expr, s, lowerCtx{metadataFullRange: true}); ok {
		t.Fatalf("resetsOrChangesOverExpHistogram(%q) = ok true under metadataFullRange; a metadata "+
			"full-range lowering must never be answered from the exp-histogram table (mutant "+
			"`||`->`&&` on the ExpHistogramTable/metadataFullRange guard would accept it)", q)
	}
}

// TestBareExpHistogramSelector_MetadataFullRangeRejects kills the
// INVERT_LOGICAL mutant on the `||` of
//
//	histogram_native_bare.go:bareExpHistogramSelector:`s.ExpHistogramTable == "" || ctx.metadataFullRange`
//
// the same guard inside [bareExpHistogramSelector], the bare-selector
// recognizer every composite shape's `isExpHistogramValuedShape` eventually
// reaches.
func TestBareExpHistogramSelector_MetadataFullRangeRejects(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	const q = `latency_exp_hist`
	expr := mustParse(t, q)

	if _, ok := bareExpHistogramSelector(expr, s, lowerCtx{}); !ok {
		t.Fatalf("positive control: bareExpHistogramSelector(%q) = ok false with an ordinary ctx; "+
			"the negative assertion below would then hold for the wrong reason", q)
	}
	if _, ok := bareExpHistogramSelector(expr, s, lowerCtx{metadataFullRange: true}); ok {
		t.Fatalf("bareExpHistogramSelector(%q) = ok true under metadataFullRange; a metadata "+
			"full-range lowering must never be answered from the exp-histogram table (mutant "+
			"`||`->`&&` on the ExpHistogramTable/metadataFullRange guard would accept it)", q)
	}
}

// TestTsOfFirstLastOverExpHistogram_MetadataFullRangeRejects kills the
// INVERT_LOGICAL mutant on the `||` of
//
//	histogram_native_ts_of_first_last_over_time.go:tsOfFirstLastOverExpHistogram:`s.ExpHistogramTable == "" || ctx.metadataFullRange`
//
// the same guard inside [tsOfFirstLastOverExpHistogram], the
// `ts_of_first_over_time` / `ts_of_last_over_time` recognizer.
func TestTsOfFirstLastOverExpHistogram_MetadataFullRangeRejects(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	const q = `ts_of_last_over_time(latency_exp_hist[5m])`
	expr := mustParseExperimental(t, q)

	if _, _, _, ok := tsOfFirstLastOverExpHistogram(expr, s, lowerCtx{}); !ok {
		t.Fatalf("positive control: tsOfFirstLastOverExpHistogram(%q) = ok false with an ordinary "+
			"ctx; the negative assertion below would then hold for the wrong reason", q)
	}
	if _, _, _, ok := tsOfFirstLastOverExpHistogram(expr, s, lowerCtx{metadataFullRange: true}); ok {
		t.Fatalf("tsOfFirstLastOverExpHistogram(%q) = ok true under metadataFullRange; a metadata "+
			"full-range lowering must never be answered from the exp-histogram table (mutant "+
			"`||`->`&&` on the ExpHistogramTable/metadataFullRange guard would accept it)", q)
	}
}

// TestSumOrAvgOverExpHistogram_MetadataFullRangeRejects kills the
// INVERT_LOGICAL mutant on the `||` of
//
//	histogram_native_sum.go:sumOrAvgOverExpHistogram:`s.ExpHistogramTable == "" || ctx.metadataFullRange`
//
// the same guard inside [sumOrAvgOverExpHistogram], the
// mergeable-aggregation recognizer.
func TestSumOrAvgOverExpHistogram_MetadataFullRangeRejects(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	const q = `sum(latency_exp_hist)`
	expr := mustParse(t, q)

	if _, _, ok := sumOrAvgOverExpHistogram(expr, s, lowerCtx{}); !ok {
		t.Fatalf("positive control: sumOrAvgOverExpHistogram(%q) = ok false with an ordinary ctx; "+
			"the negative assertion below would then hold for the wrong reason", q)
	}
	if _, _, ok := sumOrAvgOverExpHistogram(expr, s, lowerCtx{metadataFullRange: true}); ok {
		t.Fatalf("sumOrAvgOverExpHistogram(%q) = ok true under metadataFullRange; a metadata "+
			"full-range lowering must never be answered from the exp-histogram table (mutant "+
			"`||`->`&&` on the ExpHistogramTable/metadataFullRange guard would accept it)", q)
	}
}

// NOT KILLABLE — documented, not defended by a test.
//
// The same `||` -> `&&` INVERT_LOGICAL rewrite of
//
//	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
//
// is EQUIVALENT at every COMPOSITE recognizer, and ten of the mutants on
// cerberus issue #2949's legs are of that kind:
//
//	histogram_native_float_vector_scaling_binop.go:expHistogramFloatVectorScalingBinop:`s.ExpHistogramTable == "" || ctx.metadataFullRange`
//	histogram_native_dropping_shape.go:aggregationOverExpHistogramDroppingShape:`s.ExpHistogramTable == "" || ctx.metadataFullRange`
//	histogram_native_dropping_shape.go:labelCallOverExpHistogramDroppingShape:`s.ExpHistogramTable == "" || ctx.metadataFullRange`
//	histogram_native_range_fn.go:rangeFnOverExpHistogramSubquery:`s.ExpHistogramTable == "" || ctx.metadataFullRange`
//	histogram_native_mixed_or_subquery_range_fn.go:mixedOrSubqueryOuterFn:`s.ExpHistogramTable == "" || ctx.metadataFullRange`
//	histogram_native_subquery_select.go:selectFnOverExpHistogramSubquery:`s.ExpHistogramTable == "" || ctx.metadataFullRange`
//	histogram_native_float_vector_binop.go:expHistogramDroppingVectorBinop:`s.ExpHistogramTable == "" || ctx.metadataFullRange`
//	histogram_native_scalar_binop.go:expHistogramScalarBinop:`s.ExpHistogramTable == "" || ctx.metadataFullRange`
//	histogram_native_scalar_binop.go:expHistogramDroppingScalarBinop:`s.ExpHistogramTable == "" || ctx.metadataFullRange`
//	histogram_native_set_op.go:expHistogramSetOp:`s.ExpHistogramTable == "" || ctx.metadataFullRange`
//
// (gremlins_kill_histogram_binop_count_test.go's header already records the
// same verdict, on the same ground, for the identical guard in
// histogram_native_binop_eq.go's expHistogramHistogramCompareBinop and
// expHistogramHistogramCompareBoolBinop.)
//
// THE ARGUMENT
//
// Write the guard's two disjuncts A (`s.ExpHistogramTable == ""`) and B
// (`ctx.metadataFullRange`). The original bails on `A || B`; the mutant bails
// on `A && B`. The two therefore differ only where exactly one of A, B holds,
// and in that region the mutant does NOT bail where the original does. So the
// mutant is equivalent iff, whenever `A || B` holds, the recognizer answers
// false anyway for every input.
//
// Each function above answers by asking `isExpHistogramValuedShape` or
// `isExpHistogramDroppingShape` about its operands, and every rejection path
// returns that function's zero-value tuple, never a partially-populated one.
// So it is enough that both predicates are UNCONDITIONALLY false whenever
// `A || B` holds — and they are, by structural induction over a closed
// dispatch set:
//
//   - Base cases. Every leaf `isExpHistogramValuedShape` and
//     `isExpHistogramDroppingShape` dispatch to carries this identical guard
//     as its own first statement: bareExpHistogramSelector,
//     sumOrAvgOverExpHistogram, rangeFnOverExpHistogram,
//     rangeFnOverExpHistogramSubquery, overTimeOverExpHistogram,
//     lastFirstOverExpHistogram, unaryOverExpHistogram,
//     expHistogramHistogramBinop, expHistogramSetOp,
//     expHistogramFloatVectorScalingBinop, limitKOrRatioOverExpHistogram,
//     droppingAggregationOverExpHistogram, expHistogramDroppingScalarBinop,
//     expHistogramDroppingVectorBinop, expHistogramDroppingHistogramBinop,
//     aggregationOverExpHistogramDroppingShape and
//     labelCallOverExpHistogramDroppingShape. Each returns false under
//     `A || B` before reading its input at all.
//   - Inductive cases. The three members of the dispatch set that carry NO
//     guard of their own do not decide anything themselves — each delegates
//     the whole question back into the same set:
//     labelCallOverExpHistogram and histogramValuedProducerCall both end in
//     `isExpHistogramValuedShape(call.Args[0], s, ctx)`, and
//     selectFnHistogramPreservingSubquery delegates to
//     selectFnOverExpHistogramSubquery, which is guarded.
//
// The set is closed — those two predicates dispatch to nothing else — so
// under `A || B` no branch can produce true, and the mutant's extra reachable
// code is dead. Verified by applying each mutation by hand and confirming
// `go test ./internal/promql/` stays green, and by driving both predicates
// with `metadataFullRange: true` over the bare / aggregated / windowed /
// unary / label-call / scalar-binop / vector-binop / set-op / dropping
// shapes, all of which answer false.
//
// The LEAF recognizers are a different matter and are NOT covered by this
// footer: they decide by asking the SCHEMA (`s.IsExpHistogramMetric`), which
// never consults ctx, so the mutant genuinely accepts what the original
// rejects. Those six are killed by the tests above.
