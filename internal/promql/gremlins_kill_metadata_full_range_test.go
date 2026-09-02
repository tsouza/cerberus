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
// INVERT_LOGICAL mutant at histogram_native_last_first_over_time.go:54:31 —
// the `||` of `s.ExpHistogramTable == "" || ctx.metadataFullRange` inside
// [lastFirstOverExpHistogram], the `last_over_time` / `first_over_time`
// recognizer.
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
// INVERT_LOGICAL mutant at histogram_native_over_time.go:28:31 — the `||` of
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
// INVERT_LOGICAL mutant at histogram_native_resets.go:121:31 — the `||` of
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
// INVERT_LOGICAL mutant at histogram_native_bare.go:68:31 — the `||` of the
// same guard inside [bareExpHistogramSelector], the bare-selector recognizer
// every composite shape's `isExpHistogramValuedShape` eventually reaches.
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
// INVERT_LOGICAL mutant at histogram_native_ts_of_first_last_over_time.go:81:31
// — the `||` of the same guard inside [tsOfFirstLastOverExpHistogram], the
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
// INVERT_LOGICAL mutant at histogram_native_sum.go:106:31 — the `||` of the
// same guard inside [sumOrAvgOverExpHistogram], the mergeable-aggregation
// recognizer.
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
