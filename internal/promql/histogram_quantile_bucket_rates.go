package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// isClassicBucketRateGrid identifies finished per-source bucket-rate arrays.
// Summing those arrays before expanding anchors preserves rate-before-sum and
// avoids copying every source's label map for every requested timestamp. The
// native rate lowerer has already established server and input eligibility.
func isClassicBucketRateGrid(input chplan.Node, s schema.Metrics) bool {
	grid, ok := input.(*chplan.RangeWindowGridNative)
	if !ok || grid.Func != "rate" {
		return false
	}
	found := false
	chplan.Walk(grid.Input, func(node chplan.Node) bool {
		if project, ok := node.(*chplan.Project); ok && isClassicBucketFanoutProject(project, s) {
			found = true
		}
		return true
	})
	return found
}

// lowerHistogramQuantileNativeBucketRates shares the ordinary bucket-rate
// pipeline when its boot-selected strategy produces native grids throughout.
// Rates remain per original counter before the user's aggregation. Building
// histogram arrays only AFTER that reduction avoids retaining each source
// histogram's full ladder inside the cross-series quantile merge.
// Older servers and non-eligible grids keep their specialized bounded path.
//
// phiArgAST is the ORIGINAL, un-lowered `histogram_quantile` phi argument.
// The caller lowers phi eagerly under the array-domain rung fold's
// stepGridAnchorColumn scope — the right scope for the fold this function
// bypasses — but lowerHistogramQuantileClassicFloatOverPlan's float-domain
// reuse re-forms a Sample-row contract keyed on the Sample timestamp
// column instead. A computed phi (phi.expr != nil) is therefore re-lowered
// from phiArgAST under the ordinary ctx once this route is confirmed, so
// its per-anchor lookup keys on the column this route actually exposes
// rather than the one the fold it bypassed would have.
func tryLowerHistogramQuantileNativeBucketRates(
	shape histogramAggShape,
	phi phiArg,
	phiArgAST parser.Expr,
	s schema.Metrics,
	ctx lowerCtx,
	lowerInput func() (chplan.Node, error),
) (chplan.Node, bool, error) {
	if shape.windowFn != "rate" {
		return nil, false, nil
	}
	inner, err := lowerInput()
	if err != nil {
		return nil, true, err
	}
	if !nativeBucketRateEligible(inner) {
		return nil, false, nil
	}
	if phi.expr != nil {
		expr, err := lowerScalarArg(phiArgAST, s, ctx)
		if err != nil {
			return nil, true, err
		}
		phi.expr = expr
	}
	return lowerHistogramQuantileClassicFloatOverPlan(inner, phi, s, ctx), true, nil
}

// nativeBucketRateEligible reports whether inner's rate lowering produced
// native grids throughout, with no fan-out fallback anywhere in the tree.
func nativeBucketRateEligible(inner chplan.Node) bool {
	native, fallback := false, false
	chplan.Walk(inner, func(node chplan.Node) bool {
		switch node.(type) {
		case *chplan.RangeWindowGridNative, *chplan.RangeWindowGridNativeInstant:
			native = true
		case *chplan.RangeWindow:
			fallback = true
		}
		return true
	})
	return native && !fallback
}
