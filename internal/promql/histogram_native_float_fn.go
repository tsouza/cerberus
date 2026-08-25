package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerExpHistogramValuedShape recognises and lowers every expression shape
// whose result consists exclusively of native-histogram samples. Keeping the
// recogniser and lowering paired prevents consumers from reimplementing the
// root dispatch and accidentally admitting a shape they cannot actually lower.
func lowerExpHistogramValuedShape(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (chplan.Node, bool, error) {
	if call, ok := labelCallOverExpHistogram(expr, s, ctx); ok {
		plan, err := lowerLabelCallOverExpHistogram(call, s, ctx)
		return plan, true, err
	}
	// Unary `+`/`-` over an already histogram-valued operand (cerberus
	// issue #2583) — see histogram_native_unary.go's own doc comment for
	// why registering the producer here, rather than patching
	// unary.go's lowerUnary directly, is what makes it compose under
	// every wrapper that already threads its own argument through this
	// function.
	if operand, op, ok := unaryOverExpHistogram(expr, s, ctx); ok {
		plan, err := lowerUnaryOverExpHistogram(operand, op, s, ctx)
		return plan, true, err
	}
	if vs, ok := bareExpHistogramSelector(expr, s, ctx); ok {
		plan, err := lowerExpHistogramBare(vs, s, ctx)
		return plan, true, err
	}
	if agg, vs, ok := sumOrAvgOverExpHistogram(expr, s, ctx); ok {
		plan, err := lowerExpHistogramSumOrAvg(agg, vs, s, ctx)
		return plan, true, err
	}
	if shape, ok := rangeFnOverExpHistogram(expr, s, ctx); ok {
		plan, err := lowerExpHistogramRangeFn(shape, s, ctx)
		return plan, true, err
	}
	// sum_over_time / avg_over_time over a BARE exp-histogram selector
	// (cerberus issue #2619, the bare-selector sibling of
	// [rangeFnOverExpHistogram]'s rate/increase/delta/irate/idelta match
	// just above): [overTimeOverExpHistogram] already answered this shape
	// at the query ROOT (cerberus issue #2480, via
	// [lowerHistogramNativeRoot]'s own direct dispatch), but that direct
	// dispatch is checked only against the query's own top-level `expr` —
	// it never saw the RECURSIVE call [mergeableExpHistogramAggregate]
	// makes into THIS function for an outer wrapping `sum()`/`avg()`, so
	// `sum(avg_over_time(m_exp_hist[5m]))` fell through this whole switch
	// to [expHistogramSelectorRouting]'s catch-all rejection even though
	// the identical shape composes fine under `rate()`. Registering the
	// producer here — rather than only at the root — gives it the same
	// recursive reach [rangeFnOverExpHistogram] already has, so the root
	// dispatch's own direct check is now redundant and has been folded
	// into this single entry point (see [lowerHistogramNativeRoot]'s own
	// doc for the consolidation). The merge `sum()`/`avg()` then applies
	// via [lowerExpHistogramSumOrAvgOverPlan] below is the SAME
	// across-series histogram merge `sum(rate(...))` already uses — sum_
	// over_time/avg_over_time already fold their own window into one
	// histogram per series, and outer sum()/avg() merges those per-series
	// histograms exactly like it merges rate()'s, so no new merge
	// semantics are needed here.
	if shape, ok := overTimeOverExpHistogram(expr, s, ctx); ok {
		plan, err := lowerExpHistogramOverTime(shape, s, ctx)
		return plan, true, err
	}
	// last_over_time / first_over_time over a BARE exp-histogram selector
	// (cerberus issue #2619, the bare-selector sibling of
	// [selectFnHistogramPreservingSubquery] just below, mirroring the
	// sum_over_time/avg_over_time gap [overTimeOverExpHistogram] closes
	// just above): both functions already select — never merge — a single
	// in-window histogram per series ([lastFirstOverExpHistogram],
	// cerberus issue #2480), so wrapping the result in `sum()`/`avg()`
	// needs only the SAME across-series merge every other bare producer
	// registered in this chain already gets from
	// [lowerExpHistogramSumOrAvgOverPlan]; `count()`/`group()` reach it
	// through [isExpHistogramValuedShape]'s matching widening.
	if fn, ms, vs, ok := lastFirstOverExpHistogram(expr, s, ctx); ok {
		plan, err := lowerLastFirstOverExpHistogram(fn, ms, vs, s, ctx)
		return plan, true, err
	}
	if shape, ok := rangeFnOverExpHistogramSubquery(expr, s, ctx); ok {
		plan, err := lowerExpHistogramRangeFnOverSubquery(shape, s, ctx)
		return plan, true, err
	}
	// last_over_time / first_over_time over a subquery whose inner
	// resolves histogram-native (cerberus issue #2569) — the histogram-
	// PRESERVING half of [selectFnOverExpHistogramSubquery]'s eight-
	// function match; the other six answer a plain float and are threaded
	// through the generic [lowerCall] composition path instead (see that
	// recognizer's own doc for the split). Composing this here, alongside
	// [rangeFnOverExpHistogramSubquery] just above, gives it the identical
	// recursive reach.
	if shape, ok := selectFnHistogramPreservingSubquery(expr, s, ctx); ok {
		plan, err := lowerSelectFnOverExpHistogramSubquery(shape, s, ctx)
		return plan, true, err
	}
	if agg, ok := mergeableExpHistogramAggregate(expr); ok {
		input, matched, err := lowerExpHistogramValuedShape(agg.Expr, s, ctx)
		if err != nil {
			return nil, true, err
		}
		if matched {
			plan, err := lowerExpHistogramSumOrAvgOverPlan(agg, input, s)
			return plan, true, err
		}
	}
	if histSide, op, scale, ok := expHistogramScalarBinop(expr, s, ctx); ok {
		plan, err := lowerExpHistogramScalarBinop(histSide, op, scale, s, ctx, false)
		return plan, true, err
	}
	// limitk(K, <exp-hist shape>) / limit_ratio(R, <exp-hist shape>)
	// (cerberus issue #2575) — see [limitKOrRatioOverExpHistogram]'s doc
	// for why this dispatch is needed even though [lowerLimitKInput]
	// already preserves the histogram shape on its own. Delegating to
	// [lowerLimitK]/[lowerLimitRatio] reuses that exact preserving
	// lowering unchanged — a query where limitk/limit_ratio is itself the
	// root, or reached through [lowerAggregate]'s own op-specific
	// dispatch, lowers byte-for-byte the same way it always has.
	if agg, ok := limitKOrRatioOverExpHistogram(expr, s, ctx); ok {
		var plan chplan.Node
		var err error
		if agg.Op == parser.LIMITK {
			plan, err = lowerLimitK(agg, s, ctx)
		} else {
			plan, err = lowerLimitRatio(agg, s, ctx)
		}
		return plan, true, err
	}
	// The vector-scaling sibling of the literal-scalar case just above
	// (cerberus issue #2540, widening #2339/#2342/#2537's own recognizer
	// into this shared dispatch table): threading it in here is what lets
	// [mergeableExpHistogramAggregate]'s recursion just above resolve
	// `sum(hist * on(x) group_left() float)`'s inner MUL as
	// histogram-valued, and lets every OTHER consumer of this function
	// (count()/group(), count_values(), the drop-family aggregations, a
	// histogram-histogram binop operand, a set-op operand) lower it the
	// same way [isExpHistogramValuedShape]'s own sibling widening lets
	// them recognise it.
	if histSide, floatSide, op, ok := expHistogramFloatVectorScalingBinop(expr, s, ctx); ok {
		plan, err := lowerExpHistogramFloatVectorScalingBinop(histSide, floatSide, op, s, ctx)
		return plan, true, err
	}
	if lhs, rhs, sub, vm, ok := expHistogramHistogramBinop(expr, s, ctx); ok {
		plan, err := lowerExpHistogramHistogramBinop(lhs, rhs, sub, vm, s, ctx)
		return plan, true, err
	}
	if lhs, rhs, ne, vm, ok := expHistogramHistogramCompareBinop(expr, s, ctx); ok {
		plan, err := lowerExpHistogramHistogramCompareBinop(lhs, rhs, ne, vm, s, ctx)
		return plan, true, err
	}
	if b, ok := expHistogramSetOp(expr, s, ctx); ok {
		plan, err := lowerExpHistogramSetOp(b, s, ctx)
		return plan, true, err
	}
	return nil, false, nil
}

// dropExpHistogramSamples converts a histogram-valued plan into the canonical
// float-sample row shape while selecting no rows. Prometheus's float-only
// functions accept native histograms but simpleFloatFunc omits every sample
// whose H field is set. Cerberus knows these inputs are histogram-only, so the
// exact result is an empty float vector rather than a lowering error.
//
// The projection deliberately sits above the false filter. HistogramProjection
// publishes the canonical quartet followed by its histogram columns; selecting
// only the quartet here makes the result's schema float-valued even when no row
// reaches the wire, and the literal Value avoids treating the histogram
// projection's compatibility placeholder as a real sample.
func dropExpHistogramSamples(input chplan.Node, s schema.Metrics) chplan.Node {
	return floatShapedExpHistogramDrop(&chplan.Filter{
		Input:     input,
		Predicate: &chplan.LitBool{V: false},
	}, s)
}

// floatShapedExpHistogramDrop reprojects an ALREADY-empty (constant-false
// filtered) plan onto the canonical float-Sample quartet — the shared
// projection list [dropExpHistogramSamples] and
// [lowerExpHistogramDroppingShape] (histogram_native_dropping_shape.go,
// cerberus issue #2528) both apply, the latter over a plan its OWN drop
// recognizers already capped with a constant-false Filter, so it must not
// add a second, redundant one.
func floatShapedExpHistogramDrop(alreadyEmpty chplan.Node, s schema.Metrics) chplan.Node {
	return &chplan.Project{
		Input: alreadyEmpty,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			{Expr: &chplan.LitFloat{V: 0}, Alias: s.ValueColumn},
		},
	}
}

// lowerExpHistogramArgAsCanonicalFloat is the shared "argument opt-in" every
// float-only wrapper in this package threads before falling back to the
// generic [lower] dispatcher (cerberus issues #2221/#2345/#2456/#2498, and
// — for the drop-family half — #2528): arg may be histogram-VALUED (the
// "preserve" family, reprojected to the canonical empty float quartet via
// [dropExpHistogramSamples], since these wrappers read only Value) or
// itself one of the "drop" family's shapes (already reprojected the
// identical way by [lowerExpHistogramDroppingShape]). Folding both checks
// into one function — rather than two sequential `if …, ok := …; ok { … }`
// blocks at every callsite — keeps each callsite a single flat branch
// instead of tripping golangci-lint's nestif complexity gate.
func lowerExpHistogramArgAsCanonicalFloat(arg parser.Expr, s schema.Metrics, ctx lowerCtx) (chplan.Node, bool, error) {
	if hist, ok, err := lowerExpHistogramValuedShape(arg, s, ctx); ok {
		if err != nil {
			return nil, true, err
		}
		return dropExpHistogramSamples(hist, s), true, nil
	}
	if dropped, ok, err := lowerExpHistogramDroppingShape(arg, s, ctx); ok {
		return dropped, true, err
	}
	return nil, false, nil
}
