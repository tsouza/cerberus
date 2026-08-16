package promql

import (
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_drop_aggregation.go handles aggregations for which
// reference Prometheus ignores native-histogram samples. min, max, stddev,
// stdvar and quantile reduce only the float samples in each group; topk and
// bottomk likewise rank only float samples. A histogram-only selector
// therefore produces a successful empty vector (plus an informational
// annotation, for which cerberus has no wire channel), never a lowering
// error.
//
// A selector that can match both float and histogram series already follows
// the generic lowering: regex routing contributes the ordinary float arms and
// deliberately contributes no bare native-histogram arm. This root-only path
// supplies the missing histogram-only half. It lowers the histogram selector
// through its canonical native-histogram path and caps it with a false filter,
// preserving instant/range/@ evaluation validation while publishing no
// samples.

// droppingAggregationOverExpHistogram recognizes a histogram-dropping
// aggregation directly over a bare exponential-histogram selector.
func droppingAggregationOverExpHistogram(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.AggregateExpr, *parser.VectorSelector, bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
		return nil, nil, false
	}
	agg, ok := unwrapAggregateExpr(expr)
	if !ok || !histogramDroppingAggregationOp(agg.Op) {
		return nil, nil, false
	}
	vs, ok := unwrapVectorSelector(agg.Expr)
	if !ok || !s.IsExpHistogramMetric(metricNameFromMatchers(vs.LabelMatchers)) {
		return nil, nil, false
	}
	return agg, vs, true
}

func histogramDroppingAggregationOp(op parser.ItemType) bool {
	switch op {
	case parser.MIN, parser.MAX, parser.STDDEV, parser.STDVAR, parser.QUANTILE,
		parser.TOPK, parser.BOTTOMK:
		return true
	default:
		return false
	}
}

// lowerExpHistogramDroppingAggregation preserves the aggregation parameter's
// normal validation before folding the histogram-only input to an empty
// result. This matters for topk/bottomk: reference validates K before walking
// the input samples, so NaN and int64 overflow still reject even when every
// input sample is a histogram.
func lowerExpHistogramDroppingAggregation(agg *parser.AggregateExpr, vs *parser.VectorSelector, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if err := validateHistogramDroppingAggregationParam(agg, s, ctx); err != nil {
		return nil, err
	}
	input, err := lowerExpHistogramBare(vs, s, ctx)
	if err != nil {
		return nil, err
	}
	return &chplan.Filter{Input: input, Predicate: &chplan.LitBool{V: false}}, nil
}

func validateHistogramDroppingAggregationParam(agg *parser.AggregateExpr, s schema.Metrics, ctx lowerCtx) error {
	probe := *agg
	probe.Expr = &parser.VectorSelector{LabelMatchers: []*labels.Matcher{
		labels.MustNewMatcher(labels.MatchEqual, model.MetricNameLabel, "__cerberus_histogram_drop_param_probe"),
	}}
	_, err := lowerAggregate(&probe, s, ctx)
	return err
}
