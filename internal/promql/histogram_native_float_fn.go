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
	if lhs, rhs, sub, vm, ok := expHistogramHistogramBinop(expr, s, ctx); ok {
		plan, err := lowerExpHistogramHistogramBinop(lhs, rhs, sub, vm, s, ctx)
		return plan, true, err
	}
	if lhs, rhs, ne, vm, returnBool, ok := expHistogramHistogramCompareBinop(expr, s, ctx); ok {
		plan, err := lowerExpHistogramHistogramCompareBinop(lhs, rhs, ne, vm, returnBool, s, ctx)
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
	return &chplan.Project{
		Input: &chplan.Filter{
			Input:     input,
			Predicate: &chplan.LitBool{V: false},
		},
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			{Expr: &chplan.LitFloat{V: 0}, Alias: s.ValueColumn},
		},
	}
}
