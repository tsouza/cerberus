package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_dropping_shape.go builds the exp-histogram "drop"
// family's own reusable recogniser/lowering pair (cerberus issue #2528) —
// the sibling [lowerExpHistogramValuedShape] (histogram_native_float_fn.go)
// has had since the "preserve" family's earliest fix.
//
// lowerRoot already tries the drop family's four leaf recognisers
// ([droppingAggregationOverExpHistogram], [expHistogramDroppingScalarBinop],
// [expHistogramDroppingHistogramBinop], [expHistogramDroppingVectorBinop])
// directly against the WHOLE query, but each of those only ever sees the
// expression lowerRoot itself was called with — never a NESTED
// sub-expression a wrapper composes around. sum(), topk(), count(),
// sort(), limitk(), a unary math function, label_replace(), … all lower
// their own argument through the plain [lower] dispatcher, which retries
// none of [lowerRoot]'s sweep: that function runs exactly twice, both
// public entry points, never recursively (the issue's own root-cause
// finding). So `sum(demo_latency_exp_hist + 0)` — an AggregateExpr whose
// argument is a histogram-dropping binop — fell through straight to
// [expHistogramSelectorRouting]'s catch-all rejection instead of
// reference's real "drop the sample, evaluate to empty" answer.
//
// [lowerExpHistogramDroppingShape] closes that gap the same way
// [lowerExpHistogramValuedShape] already closes it for the "preserve"
// family: a single reusable (matched chplan.Node, bool, error) pair,
// threaded into every EXISTING per-callsite opt-in a wrapper already has
// for the preserve family (grep this package for
// lowerExpHistogramValuedShape's call sites — every genuine "try preserve,
// fall back to the generic lower() dispatcher" callsite gets a sibling
// call here), plus lowerRoot's own dispatch chain, plus
// [lowerLimitKInput] (lower.go, cerberus issue #2518), which already
// threads the preserve check at ITS OWN callsite.
//
// Unlike [lowerExpHistogramValuedShape], whose result stays in the
// thirteen-column histogram row shape a further histogram-aware consumer
// needs, this function's result is ALWAYS reprojected to the canonical
// FOUR-column float-Value row shape ([floatShapedExpHistogramDrop]'s
// shape) — because "drop" answers with an ORDINARY (currently empty)
// float vector under reference Prometheus, so every consumer receiving
// this result is the same consumer that would receive a genuinely
// float-valued operand: nothing downstream needs to know a histogram was
// ever involved. That is what makes the result freely composable under
// ANY further wrapper — generic or histogram-aware — without that wrapper
// needing its OWN opt-in: `sum(...)`/`abs(...)`/`sort(...)` stacked on top
// of an already-recognised drop-family shape sees an ordinary,
// currently-empty float-Value plan and runs its perfectly normal
// (non-histogram) lowering over it. This is why the two recursive-
// composition recognisers below ([aggregationOverExpHistogramDroppingShape],
// [labelCallOverExpHistogramDroppingShape]) suffice to reach an
// arbitrarily deep stack of aggregations/label calls around a single
// drop-family leaf without a bespoke recogniser per wrapper: each level
// hands its parent an ordinary float vector, and the parent's own generic
// lowering — Aggregate, OrderBy, a Project — never needed to be
// histogram-aware in the first place.
func lowerExpHistogramDroppingShape(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (chplan.Node, bool, error) {
	if agg, vs, ok := droppingAggregationOverExpHistogram(expr, s, ctx); ok {
		plan, err := lowerExpHistogramDroppingAggregation(agg, vs, s, ctx)
		if err != nil {
			return nil, true, err
		}
		return floatShapedExpHistogramDrop(plan, s), true, nil
	}
	if agg, ok := aggregationOverExpHistogramDroppingShape(expr, s, ctx); ok {
		plan, err := lowerAggregationOverExpHistogramDroppingShape(agg, s, ctx)
		return plan, true, err
	}
	if call, ok := labelCallOverExpHistogramDroppingShape(expr, s, ctx); ok {
		plan, err := lowerLabelCallOverExpHistogramDroppingShape(call, s, ctx)
		return plan, true, err
	}
	if histSide, ok := expHistogramDroppingScalarBinop(expr, s, ctx); ok {
		plan, err := lowerExpHistogramScalarBinop(histSide, "", nil, s, ctx, true)
		if err != nil {
			return nil, true, err
		}
		return floatShapedExpHistogramDrop(plan, s), true, nil
	}
	if lhs, rhs, ok := expHistogramDroppingHistogramBinop(expr, s, ctx); ok {
		plan, err := lowerExpHistogramDroppingHistogramBinop(lhs, rhs, s, ctx)
		if err != nil {
			return nil, true, err
		}
		return floatShapedExpHistogramDrop(plan, s), true, nil
	}
	if histSide, floatSide, ok := expHistogramDroppingVectorBinop(expr, s, ctx); ok {
		plan, err := lowerExpHistogramDroppingVectorBinop(histSide, floatSide, s, ctx)
		if err != nil {
			return nil, true, err
		}
		return floatShapedExpHistogramDrop(plan, s), true, nil
	}
	return nil, false, nil
}

// isExpHistogramDroppingShape is the pure-predicate sibling of
// [lowerExpHistogramDroppingShape] — mirroring [isExpHistogramValuedShape]'s
// relationship to [lowerExpHistogramValuedShape] — used by the two
// recursive-composition recognisers below so shape membership can be
// tested without lowering (and discarding) a plan.
func isExpHistogramDroppingShape(expr parser.Expr, s schema.Metrics, ctx lowerCtx) bool {
	if _, _, ok := droppingAggregationOverExpHistogram(expr, s, ctx); ok {
		return true
	}
	if _, ok := aggregationOverExpHistogramDroppingShape(expr, s, ctx); ok {
		return true
	}
	if _, ok := labelCallOverExpHistogramDroppingShape(expr, s, ctx); ok {
		return true
	}
	if _, ok := expHistogramDroppingScalarBinop(expr, s, ctx); ok {
		return true
	}
	if _, _, ok := expHistogramDroppingHistogramBinop(expr, s, ctx); ok {
		return true
	}
	if _, _, ok := expHistogramDroppingVectorBinop(expr, s, ctx); ok {
		return true
	}
	return false
}

// aggregationOverExpHistogramDroppingShape recognises ANY aggregation
// operator wrapping an argument that is itself one of the "drop" family's
// shapes — unlike [droppingAggregationOverExpHistogram], which is limited
// to the specific ops reference's OWN evaluator skips histogram samples
// for (min/max/stddev/stdvar/quantile/topk/bottomk) when the ARGUMENT is
// histogram-VALUED. The reason here is different, and applies to every
// aggregation operator uniformly regardless of what it does with a
// histogram sample: PromQL aggregation over an EMPTY input vector answers
// an empty result for every reducer — there is no operator for which zero
// input groups produces a non-empty output. That makes the two conditions
// disjoint (an argument cannot be simultaneously histogram-valued and a
// drop-family shape), so this never shadows the existing entry.
//
// limitk/limit_ratio are deliberately excluded: [lowerLimitKInput]
// (lower.go, cerberus issue #2518) already threads
// [lowerExpHistogramValuedShape]/[lowerExpHistogramDroppingShape] at its
// OWN callsite inside [lowerLimitK]/[lowerLimitRatio] — reachable whether
// the aggregation is the query root or nested, unlike every other
// aggregation op handled here (whose only histogram-aware reach, before
// this function existed, was being the literal query root and hitting
// lowerRoot's dispatch chain before ever reaching [lowerAggregate]'s
// generic path). Matching them here too would run this function's more
// generic param validation ahead of their own K-domain-specific one and
// skip the LIMIT/TopK node their own lowering emits for a non-degenerate K.
func aggregationOverExpHistogramDroppingShape(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.AggregateExpr, bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
		return nil, false
	}
	agg, ok := unwrapAggregateExpr(expr)
	if !ok || agg.Op == parser.LIMITK || agg.Op == parser.LIMIT_RATIO {
		return nil, false
	}
	if !isExpHistogramDroppingShape(agg.Expr, s, ctx) {
		return nil, false
	}
	return agg, true
}

// lowerAggregationOverExpHistogramDroppingShape mirrors
// [lowerExpHistogramDroppingAggregation]'s own param-before-drop
// discipline: reference validates an aggregation's parameter (K, phi, the
// count_values label) before ever walking its input samples, so an
// invalid parameter must still surface as an error even though the input
// is already empty. [validateHistogramDroppingAggregationParam] is itself
// op-agnostic — it re-lowers agg (with its own Op, Param and Grouping
// preserved) against a dummy input, so it already exercises whichever
// op-specific validation that Op's own generic lowering applies.
func lowerAggregationOverExpHistogramDroppingShape(agg *parser.AggregateExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if err := validateHistogramDroppingAggregationParam(agg, s, ctx); err != nil {
		return nil, err
	}
	plan, matched, err := lowerExpHistogramDroppingShape(agg.Expr, s, ctx)
	if err != nil {
		return nil, err
	}
	if !matched {
		return nil, fmt.Errorf("promql: internal invariant violated: aggregation input is not a known exp-histogram dropping shape: %v", agg.Expr)
	}
	return plan, nil
}

// labelCallOverExpHistogramDroppingShape recognises label_replace/
// label_join wrapping a "drop"-family argument — the sibling
// [labelCallOverExpHistogram] (histogram_native_label_replace.go) has for
// the "preserve" family. Reference copies whatever base vector it is
// given: an already-empty one composes for free, so the answer is simply
// the canonical empty float vector [lowerExpHistogramDroppingShape]
// already produces for the base argument alone — no attribute rewrite is
// observable since no row survives to carry it, but the static arguments
// (dst/replacement/src/regex, or dst/separator/srcs) still need their own
// validation, exactly as [lowerLabelCallOverExpHistogram] validates them
// for the preserve case.
func labelCallOverExpHistogramDroppingShape(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.Call, bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
		return nil, false
	}
	call, ok := peelWrappers(expr).(*parser.Call)
	if !ok {
		return nil, false
	}
	switch call.Func.Name {
	case "label_replace":
		if len(call.Args) != 5 {
			return nil, false
		}
	case "label_join":
		if len(call.Args) < 3 {
			return nil, false
		}
	default:
		return nil, false
	}
	if !isExpHistogramDroppingShape(call.Args[0], s, ctx) {
		return nil, false
	}
	return call, true
}

// lowerLabelCallOverExpHistogramDroppingShape lowers the shape
// [labelCallOverExpHistogramDroppingShape] recognised. The static label
// arguments are validated (and their derived chplan.Expr discarded) purely
// so a bad regex/template still surfaces as an error, mirroring
// [lowerLabelCallOverExpHistogram]'s own validate-then-rewrite split — the
// rewrite itself would never be observed here since zero rows survive.
func lowerLabelCallOverExpHistogramDroppingShape(call *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	var err error
	switch call.Func.Name {
	case "label_replace":
		_, err = labelReplaceAttributes(call, s)
	case "label_join":
		_, err = labelJoinAttributes(call, s)
	}
	if err != nil {
		return nil, err
	}
	plan, matched, err := lowerExpHistogramDroppingShape(call.Args[0], s, ctx)
	if err != nil {
		return nil, err
	}
	if !matched {
		return nil, fmt.Errorf("promql: internal invariant violated: %s base is not a known exp-histogram dropping shape: %v", call.Func.Name, call.Args[0])
	}
	return plan, nil
}
