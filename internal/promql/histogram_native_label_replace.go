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
	// Issue #2224 makes delta / irate / idelta histogram-valued at the
	// query root. It does not also add their label_replace composition;
	// preserve the previously shipped rate / increase boundary here.
	if shape, ok := rangeFnOverExpHistogram(call.Args[0], s, ctx); ok &&
		shape.windowFn != rateWindowFn && shape.windowFn != increaseWindowFn {
		return nil, false
	}
	return call, isExpHistogramValuedShape(call.Args[0], s, ctx)
}

func lowerLabelReplaceOverExpHistogram(call *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	attrs, err := labelReplaceAttributes(call, s)
	if err != nil {
		return nil, err
	}

	inner, ok, err := lowerExpHistogramValuedShape(call.Args[0], s, ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("promql: internal invariant violated: expression is not a known histogram-valued shape: %v", call.Args[0])
	}
	hp, ok := inner.(*chplan.HistogramProjection)
	if !ok {
		return nil, fmt.Errorf("promql: internal invariant violated: exp-histogram label_replace input is %T, want *chplan.HistogramProjection", inner)
	}
	return rewriteHistogramProjectionAttributes(hp, attrs, s), nil
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
