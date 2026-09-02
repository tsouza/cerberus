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
	case fnLabelReplace:
		if len(call.Args) != 5 {
			return nil, false
		}
	case fnLabelJoin:
		if len(call.Args) < 3 {
			return nil, false
		}
	default:
		return nil, false
	}
	// A rejection answers the zero-value tuple, never a
	// partially-populated one — the contract every exp-histogram
	// recognizer keeps, pinned across the whole set by
	// [TestExpHistogramRecognizersRejectWhenLoweringUnavailable].
	if !isExpHistogramValuedShape(call.Args[0], s, ctx) {
		return nil, false
	}
	return call, true
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
	case fnLabelReplace:
		attrs, err = labelReplaceAttributes(call, s)
	case fnLabelJoin:
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
	// inner is either a bare *chplan.HistogramProjection (a selector,
	// sum()/avg(), or a histogram-valued range function) or a
	// *chplan.VectorSetOp with Histogram set — histogram_native_set_op.go's
	// lowerExpHistogramSetOp builds the latter for a both-histogram
	// `and`/`or`/`unless` (cerberus issue #2324), and [isExpHistogramValuedShape]
	// recognises it too, so [labelCallOverExpHistogram] matches an outer
	// label_replace/label_join around one. Both shapes publish the exact same
	// thirteen-column contract under the fixed Histogram*Column aliases (see
	// [chplan.RowShapeOf]), so any node answering [chplan.HistogramRowShape]
	// here is a valid input to [rewriteHistogramProjectionAttributes] — a
	// stricter Go-type assertion is what issue #2468 reports as the bug.
	if shape := chplan.RowShapeOf(inner); shape != chplan.HistogramRowShape {
		return nil, fmt.Errorf("promql: internal invariant violated: exp-histogram label_replace input publishes %s row shape (%T), want %s", shape, inner, chplan.HistogramRowShape)
	}
	return rewriteHistogramProjectionAttributes(inner, attrs, s), nil
}

func lowerLabelReplaceOverExpHistogram(call *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	return lowerLabelCallOverExpHistogram(call, s, ctx)
}

// rewriteHistogramProjectionAttributes applies a label rewrite without
// converting a histogram row into an ordinary Value-shaped row. inner must
// publish [chplan.HistogramRowShape] — a bare *chplan.HistogramProjection
// (a selector, sum()/avg(), or a histogram-valued range function) or a
// *chplan.VectorSetOp with Histogram set (a both-histogram `and`/`or`/
// `unless`, cerberus issue #2324): both publish the identical thirteen-
// column contract (the canonical quartet plus the nine fixed
// Histogram*Column aliases), so this rewrite treats them identically and
// never inspects inner's own Go type. A Project first gives that payload
// stable output aliases while rewriting Attributes and forwarding all
// thirteen columns; the Aggregate enforces Prometheus's duplicate-labelset
// rule per evaluation step and carries the nine histogram fields through;
// the outer HistogramProjection restores HistogramRowShape for the wire
// decoder.
func rewriteHistogramProjectionAttributes(inner chplan.Node, attrs chplan.Expr, s schema.Metrics) *chplan.HistogramProjection {
	projections := []chplan.Projection{
		{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}},
		{Expr: attrs, Alias: s.AttributesColumn},
		{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}},
		{Expr: &chplan.ColumnRef{Name: s.ValueColumn}},
	}
	for _, name := range histogramProjectionOutputColumns() {
		projections = append(projections, chplan.Projection{Expr: &chplan.ColumnRef{Name: name}})
	}
	rewritten := &chplan.Project{Input: inner, Projections: projections}

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
