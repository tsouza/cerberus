package promql

import (
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
func lowerHistogramQuantileNativeBucketRates(inner chplan.Node, phi phiArg, s schema.Metrics, ctx lowerCtx) (chplan.Node, bool) {
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
	if !native || fallback {
		return nil, false
	}
	return lowerHistogramQuantileClassicFloatOverPlan(inner, phi, s, ctx), true
}
