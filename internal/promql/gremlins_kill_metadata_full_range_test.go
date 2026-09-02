// Tests in this file kill the mutants on the exp-histogram availability rule
// — the `INVERT_LOGICAL` rewrite of the `&&` and the `CONDITIONALS_NEGATION`
// rewrite of the `!=` in
//
//	histogram_native_availability.go:expHistogramLoweringAvailable:`s.ExpHistogramTable != "" && !ctx.metadataFullRange`
//
// — through the LEAF exp-histogram recognizers, the ones that decide a shape
// by asking the SCHEMA whether the selected metric is an exp-histogram
// (`s.IsExpHistogramMetric`), which reads nothing from `ctx`. See
// gremlins_kill_test.go for the shared file-header convention this file
// follows; cerberus issue #2949 owns the legs these mutants were reported on
// (phase4-promql-b / -d / -f / -i / -other), and cerberus issue #2963 is why
// the rule now lives in one function instead of at 31 copied sites.
//
// Why the leaf/composite distinction is the whole story here
// ---------------------------------------------------------
// The rule is one condition with two independent reasons to say no: the
// schema declares no exp-histogram table at all, or this lowering is a
// Prometheus metadata full-range walk, which must never be answered from the
// exp-histogram table. [expHistogramLoweringAvailable] says yes only when
// NEITHER holds. The `&&` -> `||` mutant says yes when either does, so the
// two differ exactly when one holds and the other does not — and the
// reachable half of that is a metadata full-range lowering against a schema
// that DOES declare the table. The `!=` -> `==` mutant inverts the first
// disjunct instead, and is caught by each test's positive control.
//
// At a leaf recognizer nothing downstream re-checks `ctx`: the remaining
// guards are shape guards (is this a Call, a MatrixSelector, a positive
// range) plus `s.IsExpHistogramMetric`, which consults only the metric name
// and the schema's suffix. So with `metadataFullRange: true` and a normal
// schema the mutant runs the whole recognizer to completion and ACCEPTS a
// shape the original rejects — an observable difference, and what each test
// below pins. Ten leaves apply the rule; six of them are pinned here and the
// rest in the sibling files listed by
// [TestExpHistogramRecognizersRejectWhenLoweringUnavailable].
//
// At a COMPOSITE recognizer the rule is not applied at all, because it would
// decide nothing there: a composite re-derives the identical verdict one
// level down through [isExpHistogramValuedShape] /
// [isExpHistogramDroppingShape], whose arms recurse on strict
// sub-expressions and bottom out at those leaves. That asymmetry is the
// whole reason the rule has one statement rather than 31 — see this file's
// closing note, and [expHistogramLoweringAvailable]'s own doc.
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
//	histogram_native_availability.go:expHistogramLoweringAvailable:`s.ExpHistogramTable != "" && !ctx.metadataFullRange`
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
			"`&&`->`||` in expHistogramLoweringAvailable would accept it)", q)
	}
}

// TestOverTimeOverExpHistogram_MetadataFullRangeRejects kills the
// INVERT_LOGICAL mutant on the `||` of
//
//	histogram_native_availability.go:expHistogramLoweringAvailable:`s.ExpHistogramTable != "" && !ctx.metadataFullRange`
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
			"`&&`->`||` in expHistogramLoweringAvailable would accept it)", q)
	}
}

// TestResetsOrChangesOverExpHistogram_MetadataFullRangeRejects kills the
// INVERT_LOGICAL mutant on the `||` of
//
//	histogram_native_availability.go:expHistogramLoweringAvailable:`s.ExpHistogramTable != "" && !ctx.metadataFullRange`
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
			"`&&`->`||` in expHistogramLoweringAvailable would accept it)", q)
	}
}

// TestBareExpHistogramSelector_MetadataFullRangeRejects kills the
// INVERT_LOGICAL mutant on the `||` of
//
//	histogram_native_availability.go:expHistogramLoweringAvailable:`s.ExpHistogramTable != "" && !ctx.metadataFullRange`
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
			"`&&`->`||` in expHistogramLoweringAvailable would accept it)", q)
	}
}

// TestTsOfFirstLastOverExpHistogram_MetadataFullRangeRejects kills the
// INVERT_LOGICAL mutant on the `||` of
//
//	histogram_native_availability.go:expHistogramLoweringAvailable:`s.ExpHistogramTable != "" && !ctx.metadataFullRange`
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
			"`&&`->`||` in expHistogramLoweringAvailable would accept it)", q)
	}
}

// TestSumOrAvgOverExpHistogram_MetadataFullRangeRejects kills the
// INVERT_LOGICAL mutant on the `||` of
//
//	histogram_native_availability.go:expHistogramLoweringAvailable:`s.ExpHistogramTable != "" && !ctx.metadataFullRange`
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
			"`&&`->`||` in expHistogramLoweringAvailable would accept it)", q)
	}
}

// WHY THERE IS NO "NOT KILLABLE" FOOTER HERE ANY MORE (cerberus issue #2963).
//
// This file used to close with an equivalence adjudication covering twelve
// COMPOSITE recognizers that carried a copy of the same guard: the `||` ->
// `&&` rewrite could not be killed at any of them, because a composite
// re-derives the identical condition one level down through
// [isExpHistogramValuedShape] / [isExpHistogramDroppingShape]. Those copies
// decided nothing, so the mutants on them were permanently equivalent and
// consumed mutation denominator forever.
//
// The copies are gone. The rule is now stated once, in
// [expHistogramLoweringAvailable], and the composites carry nothing to
// mutate — so the adjudication has no mutants left to adjudicate and has
// been retired rather than restated. What replaces it is proof rather than
// prose: [TestExpHistogramRecognizersRejectWhenLoweringUnavailable] drives
// every recognizer and both predicates across the whole shape matrix under
// each disjunct, pinning the property the deleted copies used to assert
// site by site.
