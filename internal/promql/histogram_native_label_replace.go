package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// labelReplaceOverExpHistogram recognizes label_replace around one of the
// native-histogram shapes whose result is histogram-valued. Reference
// Prometheus rewrites only the series labels and leaves the float or histogram
// sample payload untouched (promql/functions.go: evalLabelReplace).
func labelReplaceOverExpHistogram(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.Call, bool) {
	call, ok := peelWrappers(expr).(*parser.Call)
	if !ok || call.Func.Name != "label_replace" || len(call.Args) != 5 {
		return nil, false
	}
	return call, isExpHistogramValuedShape(call.Args[0], s, ctx)
}

func lowerLabelReplaceOverExpHistogram(call *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	attrs, err := labelReplaceAttributes(call, s)
	if err != nil {
		return nil, err
	}

	inner, err := lowerExpHistogramValuedShape(call.Args[0], s, ctx)
	if err != nil {
		return nil, err
	}
	hp, ok := inner.(*chplan.HistogramProjection)
	if !ok {
		return nil, fmt.Errorf("promql: internal invariant violated: exp-histogram label_replace input is %T, want *chplan.HistogramProjection", inner)
	}
	return rewriteHistogramProjectionAttributes(hp, attrs, s), nil
}

// lowerExpHistogramValuedShape lowers the closed set recognized by
// isExpHistogramValuedShape. Keeping the recognizer and dispatcher paired
// prevents a newly admitted shape from silently falling back to scalar
// lowering and dropping its histogram payload.
func lowerExpHistogramValuedShape(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if vs, ok := bareExpHistogramSelector(expr, s, ctx); ok {
		return lowerExpHistogramBare(vs, s, ctx)
	}
	if agg, vs, ok := sumOrAvgOverExpHistogram(expr, s, ctx); ok {
		return lowerExpHistogramSumOrAvg(agg, vs, s, ctx)
	}
	if shape, ok := rateOverExpHistogram(expr, s, ctx); ok {
		return lowerExpHistogramRate(shape, s, ctx)
	}
	return nil, fmt.Errorf("promql: internal invariant violated: expression is not a known histogram-valued shape: %v", expr)
}

// rewriteHistogramProjectionAttributes applies a label rewrite without
// converting a histogram row into an ordinary Value-shaped row. The inner
// HistogramProjection first gives the payload stable output aliases; a
// Project rewrites Attributes while forwarding all thirteen columns; the
// Aggregate enforces Prometheus's duplicate-labelset rule per evaluation
// step and carries the nine histogram fields through; the outer
// HistogramProjection restores HistogramRowShape for the wire decoder.
func rewriteHistogramProjectionAttributes(hp *chplan.HistogramProjection, attrs chplan.Expr, s schema.Metrics) *chplan.HistogramProjection {
	projections := []chplan.Projection{
		{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}},
		{Expr: attrs, Alias: s.AttributesColumn},
		{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}},
		{Expr: &chplan.ColumnRef{Name: s.ValueColumn}},
	}
	for _, name := range histogramProjectionOutputColumns() {
		projections = append(projections, chplan.Projection{Expr: &chplan.ColumnRef{Name: name}})
	}
	rewritten := &chplan.Project{Input: hp, Projections: projections}

	aggs := []chplan.AggFunc{
		{Fn: chplan.FnAny, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ValueColumn}}, Alias: s.ValueColumn},
	}
	for _, name := range histogramProjectionOutputColumns() {
		aggs = append(aggs, chplan.AggFunc{
			Fn: chplan.FnAny, Args: []chplan.Expr{&chplan.ColumnRef{Name: name}}, Alias: name,
		})
	}
	guarded := &chplan.Aggregate{
		Input: rewritten,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: s.MetricNameColumn},
			&chplan.ColumnRef{Name: s.AttributesColumn},
			&chplan.ColumnRef{Name: s.TimestampColumn},
		},
		GroupByAliases: []string{s.MetricNameColumn, s.AttributesColumn, s.TimestampColumn},
		AggFuncs:       aggs,
		Having:         duplicateLabelsetRowCountGuardExpr(),
	}

	return &chplan.HistogramProjection{
		Input:                      guarded,
		CountColumn:                chplan.HistogramCountColumn,
		SumColumn:                  chplan.HistogramSumColumn,
		ScaleColumn:                chplan.HistogramScaleColumn,
		ZeroThresholdColumn:        chplan.HistogramZeroThresholdColumn,
		ZeroCountColumn:            chplan.HistogramZeroCountColumn,
		PositiveOffsetColumn:       chplan.HistogramPositiveOffsetColumn,
		PositiveBucketCountsColumn: chplan.HistogramPositiveBucketCountsColumn,
		NegativeOffsetColumn:       chplan.HistogramNegativeOffsetColumn,
		NegativeBucketCountsColumn: chplan.HistogramNegativeBucketCountsColumn,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: s.MetricNameColumn},
			&chplan.ColumnRef{Name: s.AttributesColumn},
			&chplan.ColumnRef{Name: s.TimestampColumn},
			&chplan.ColumnRef{Name: s.ValueColumn},
		},
		GroupByAliases:   []string{s.MetricNameColumn, s.AttributesColumn, s.TimestampColumn, s.ValueColumn},
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
	}
}

func histogramProjectionOutputColumns() []string {
	return []string{
		chplan.HistogramCountColumn,
		chplan.HistogramSumColumn,
		chplan.HistogramScaleColumn,
		chplan.HistogramZeroThresholdColumn,
		chplan.HistogramZeroCountColumn,
		chplan.HistogramPositiveOffsetColumn,
		chplan.HistogramPositiveBucketCountsColumn,
		chplan.HistogramNegativeOffsetColumn,
		chplan.HistogramNegativeBucketCountsColumn,
	}
}
