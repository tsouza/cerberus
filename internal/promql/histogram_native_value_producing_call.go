package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_value_producing_call.go registers info() /
// sort_by_label() / sort_by_label_desc()'s OWN OUTPUT as a histogram-valued
// producer inside [isExpHistogramValuedShape] / [lowerExpHistogramValuedShape]'s
// shared, 1:1-paired recursive dispatch — the MIRROR direction of the
// mixed-`or` composability this campaign already gave each function's own
// ARGUMENT (info(): histogram_native_mixed_or_info.go, cerberus issue
// #2618; sort_by_label[_desc](): histogram_native_mixed_or_sort_by_label.go,
// cerberus issue #2611).
//
// Without this, `(info(latency_exp_hist)) or up` and
// `(sort_by_label(latency_exp_hist, "service")) or up` both fall through to
// binary.go's [lowerVectorSetOp] `leftHistogram != rightHistogram`
// rejection: neither [lowerInfo] nor [lowerSortByLabel] is reached through
// [lowerHistogramNativeRoot]'s direct dispatch table when nested (as either
// operand of an outer `or` always is), so — unlike a bare selector,
// sum()/avg(), or a label_replace/label_join wrapper — nothing taught
// [isExpHistogramValuedShape] (the predicate [mixedExpHistogramSetOp]
// consults to decide which side of an outer `or` is histogram-valued) that
// either call's result IS histogram-valued whenever its own base/vector
// argument already is, even though [lowerInfo] (cerberus issue #2509) and
// [lowerSortByLabel] (cerberus issue #2462) already produce a
// [chplan.HistogramRowShape] node in that case.
func histogramValuedProducerCall(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.Call, bool) {
	call, ok := peelWrappers(expr).(*parser.Call)
	if !ok {
		return nil, false
	}
	switch call.Func.Name {
	case "info":
		if len(call.Args) < 1 || len(call.Args) > 2 {
			return nil, false
		}
	case "sort_by_label", "sort_by_label_desc":
		if len(call.Args) < 1 {
			return nil, false
		}
	default:
		return nil, false
	}
	// A rejection answers the zero-value tuple, never a
	// partially-populated one — the contract every exp-histogram
	// recognizer keeps, pinned across the whole set by
	// [TestExpHistogramRecognizersRejectWhenLoweringUnavailable].
	if !isExpHistogramValuedShape(call.Args[0], s, ctx) {
		return nil, false
	}
	return call, true
}

// lowerHistogramValuedProducerCall lowers the shape
// [histogramValuedProducerCall] recognised by delegating to the SAME
// lowering [lowerCall] already uses for that function name — neither
// [lowerInfo] nor [lowerSortByLabel] needs a bespoke histogram-aware
// variant here, only this recognition so the recursive dispatch reaches
// them at all.
func lowerHistogramValuedProducerCall(call *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	switch call.Func.Name {
	case "info":
		return lowerInfo(call, s, ctx)
	case "sort_by_label", "sort_by_label_desc":
		return lowerSortByLabel(call, s, ctx)
	}
	return nil, fmt.Errorf("promql: internal invariant violated: %s is not a histogram-valued-producing call", call.Func.Name)
}
