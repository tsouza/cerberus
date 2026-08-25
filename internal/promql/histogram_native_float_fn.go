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
