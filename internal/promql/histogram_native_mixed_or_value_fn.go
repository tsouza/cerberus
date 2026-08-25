package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_value_fn.go composes the native-histogram VALUE
// functions — histogram_avg / histogram_count / histogram_sum /
// histogram_stddev / histogram_stdvar / histogram_fraction
// (histogram_value_fns.go's shared [lowerHistogramValueFnHistogramValuedArg])
// and histogram_quantile (histogram_quantile.go's
// [lowerHistogramQuantileHistogramValuedArg]; histogram_quantiles inherits
// this for free by synthesising a singular histogram_quantile call per phi,
// see [lowerHistogramQuantiles]) — over a mixed float/histogram `or`
// argument (histogram_native_mixed_or.go's #2330 shape), cerberus issue
// #2618.
//
// Reference semantics (tsouza/prometheus@cerberus-parser promql/
// functions.go's simpleHistogramFunc and promql/quantile.go's
// funcHistogramQuantile): every one of these functions reads ONLY
// sample.H per row and SKIPS any sample whose H is nil — a mixed
// argument's float-typed rows (the `or`'s non-histogram arm) are silently
// dropped exactly as a wholly float-valued argument already folds to an
// empty result (see histogram_value_fns.go's own header). The rows to
// keep are precisely [splitMixedExpHistogramSetOpByType]'s histogram
// partition of the correctly shadow-resolved Mixed union — not
// [lowerMixedExpHistogramOperands]'s raw, PRE-union histogram-side
// lowering, which would over-count a histogram-valued RIGHT operand's
// rows that `or`'s own anti-join excludes when their match-key signature
// already appears on a float-valued LEFT operand.
func mixedOrHistogramValuedArg(arg parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.BinaryExpr, bool) {
	return mixedExpHistogramSetOp(arg, s, ctx)
}

// lowerMixedOrHistogramValuedArg lowers the shape [mixedOrHistogramValuedArg]
// recognised into a plan publishing [chplan.HistogramRowShape] — the same
// contract every other producer [lowerExpHistogramValuedShape] recognises
// publishes — so histogram_value_fns.go's and histogram_quantile.go's own
// "histogram-valued retry" opt-ins can consume it exactly like a bare
// selector, a sum()/avg() merge, or any other histogram-valued shape.
func lowerMixedOrHistogramValuedArg(b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	histPart, _, err := splitMixedExpHistogramSetOpByType(b, s, ctx)
	return histPart, err
}

// mixedDiscriminatorHistogram / mixedDiscriminatorFloat are the two values
// [chplan.MixedDiscriminatorColumn] carries per row — 1 on the
// histogram-shaped arm, 0 on the float-shaped arm (see that constant's own
// doc comment).
const (
	mixedDiscriminatorHistogram = 1
	mixedDiscriminatorFloat     = 0
)

// splitMixedExpHistogramSetOpByType lowers b — a shape [mixedExpHistogramSetOp]
// recognised — into its own histogram-shaped and float-shaped row
// PARTITIONS, both re-derived from the single, correctly shadow-resolved
// Mixed union [lowerMixedExpHistogramSetOp] already builds for the plain
// `(hist or float)` leaf case. Splitting the POST-union result, rather than
// re-lowering each operand independently via
// [lowerMixedExpHistogramOperands], is what keeps both partitions correct
// regardless of which side of `or` is histogram-valued: that union's own
// anti-join already resolved which RHS rows a matching-signature LHS row
// shadows, and re-deriving the operands from scratch would lose that.
func splitMixedExpHistogramSetOpByType(b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (histPart, floatPart chplan.Node, err error) {
	mixed, err := lowerMixedExpHistogramSetOp(b, s, ctx)
	if err != nil {
		return nil, nil, err
	}
	return wrapMixedHistogramPartition(mixed, s), wrapMixedFloatPartition(mixed, s), nil
}

// mixedDiscriminatorFilter narrows mixed's rows to the one side of its
// per-row [chplan.MixedDiscriminatorColumn] want names.
func mixedDiscriminatorFilter(mixed chplan.Node, want int64) *chplan.Filter {
	return &chplan.Filter{
		Input: mixed,
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: chplan.MixedDiscriminatorColumn},
			Right: &chplan.LitInt{V: want},
		},
	}
}

// wrapMixedHistogramPartition re-projects mixed's histogram-discriminated
// rows back onto the ordinary [chplan.HistogramRowShape] contract every
// OTHER [lowerExpHistogramValuedShape] producer publishes — mirroring
// histogram_native_label_replace.go's [rewriteHistogramProjectionAttributes],
// which re-wraps an already-fixed-Histogram*Column-alias input the
// identical way.
func wrapMixedHistogramPartition(mixed chplan.Node, s schema.Metrics) *chplan.HistogramProjection {
	return &chplan.HistogramProjection{
		Input:                      mixedDiscriminatorFilter(mixed, mixedDiscriminatorHistogram),
		CountColumn:                chplan.HistogramCountColumn,
		SumColumn:                  chplan.HistogramSumColumn,
		ScaleColumn:                chplan.HistogramScaleColumn,
		ZeroCountColumn:            chplan.HistogramZeroCountColumn,
		ZeroThresholdColumn:        chplan.HistogramZeroThresholdColumn,
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
		GroupByAliases: []string{
			s.MetricNameColumn, s.AttributesColumn, s.TimestampColumn, s.ValueColumn,
		},
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
	}
}

// wrapMixedFloatPartition re-projects mixed's float-discriminated rows back
// onto the canonical four-column Sample contract, dropping the nine
// placeholder histogram columns and the discriminator.
func wrapMixedFloatPartition(mixed chplan.Node, s schema.Metrics) *chplan.Project {
	return &chplan.Project{
		Input: mixedDiscriminatorFilter(mixed, mixedDiscriminatorFloat),
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}
}
