package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerSumOrAvgOverMixedPlan reduces an already-authorized mixed relation.
// Partition only after its union and wrappers have resolved sample membership;
// reducing placeholder Values would lose histogram-only groups and retain groups
// which Prometheus removes because they contain both sample kinds.
func lowerSumOrAvgOverMixedPlan(a *parser.AggregateExpr, input chplan.Node, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	// Histogram reductions group by timestamp even for instant queries. Normalize
	// their input to one evaluation instant, never separate source scrape times.
	histInput := nativeHistogramProjection(mixedDiscriminatorFilter(input, mixedDiscriminatorHistogram),
		&chplan.ColumnRef{Name: s.MetricNameColumn}, evalInstantExpr(s, ctx), histogramProjectionSchema(s))
	histBranch, err := lowerExpHistogramSumOrAvgOverPlan(a, histInput, s, ctx.resourceBounds.HistogramMergeMaxCostUnits)
	if err != nil {
		return nil, err
	}
	floatBranch, err := lowerPlainAggOverMixedFloatArm(a, wrapMixedFloatPartition(input, s), s, ctx)
	if err != nil {
		return nil, err
	}
	return combineMixedAggregateBranches(histBranch, floatBranch, s, ctx.step > 0), nil
}
