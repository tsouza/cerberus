package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_scalar.go lowers `scalar(...)` wrapping a
// mixed float/histogram `or` (histogram_native_mixed_or.go's #2330/#2335
// shape) — cerberus issue #2611.
//
// Reference Prometheus's funcScalar (promql/functions.go) walks the
// argument vector counting only samples whose `H` field is nil: a
// histogram-shaped sample is invisible to it entirely — it is never
// "the" single sample scalar() reduces to, and it never contributes
// towards the "more than one sample" NaN branch either. Exactly one
// float-shaped sample answers that sample's own value; zero, or more
// than one, float-shaped sample answers NaN, regardless of how many
// histogram-shaped samples also exist in the vector. This is the
// identical "the histogram side is invisible to this reduction" rule
// the float-only REDUCE family already composes
// (histogram_native_mixed_or_aggregate_float_only.go, cerberus issues
// #2595/#2600) and sort()/sort_desc()'s own filterFloats rule already
// composes (histogram_native_mixed_or_sort.go, cerberus issue #2605) —
// unlike those two, scalar() needs no OrderBy/reduction machinery beyond
// what scalar_args.go's existing count()==1 ? value : NaN reduction
// ([scalarValuePlan]/[scalarStepPlan]) already builds for a plain float
// vector: the shadow-resolved FLOAT arm
// [shadowResolveMixedExpHistogramOperands] already builds for every
// sibling composition in this file group is scalar()'s entire answer,
// fed straight into that SAME reduction — mirroring
// scalar_args.go's [lowerScalarVectorArg] own non-mixed
// all-histogram path, which reuses [dropExpHistogramSamples] (via
// [lowerExpHistogramArgAsCanonicalFloat]) for exactly the same reason:
// zero float samples reduces to NaN with no bespoke NaN literal needed.
func scalarArgOverMixedExpHistogramSetOp(v parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.BinaryExpr, bool) {
	return mixedExpHistogramSetOp(v, s, ctx)
}

// lowerScalarArgOverMixedExpHistogramSetOp lowers the shape
// [scalarArgOverMixedExpHistogramSetOp] recognised: the shadow-resolved
// float arm alone — the histogram arm is discarded entirely, never fed
// into any reduction — see this file's header for why reference's
// funcScalar never sees it.
func lowerScalarArgOverMixedExpHistogramSetOp(b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if b.ReturnBool {
		return nil, fmt.Errorf("promql: 'bool' modifier is only allowed on comparison binary ops")
	}
	_, floatForAgg, err := shadowResolveMixedExpHistogramOperands(b, s, ctx)
	if err != nil {
		return nil, err
	}
	return floatForAgg, nil
}
