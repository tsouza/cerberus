package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// labelCallOverExpHistogram recognizes either PromQL label-only function
// around a histogram-valued result. Reference's evalLabelReplace and
// evalLabelJoin rewrite only the series labels and carry the histogram sample
// pointer through unchanged.
func labelCallOverExpHistogram(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.Call, bool) {
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
	return call, isExpHistogramValuedShape(call.Args[0], s, ctx)
}

// labelReplaceOverExpHistogram remains the root-dispatch spelling while the
// implementation now covers both label-only consumers.
func labelReplaceOverExpHistogram(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.Call, bool) {
	return labelCallOverExpHistogram(expr, s, ctx)
}

func lowerLabelCallOverExpHistogram(call *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	var (
		attrs chplan.Expr
		err   error
	)
	switch call.Func.Name {
	case "label_replace":
		attrs, err = labelReplaceAttributes(call, s)
	case "label_join":
		attrs, err = labelJoinAttributes(call, s)
	default:
		return nil, fmt.Errorf("promql: internal invariant violated: %s is not a label-only histogram consumer", call.Func.Name)
	}
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

func lowerLabelReplaceOverExpHistogram(call *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	return lowerLabelCallOverExpHistogram(call, s, ctx)
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
