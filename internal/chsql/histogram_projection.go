// histogram_projection.go emits the SQL for
// chplan.HistogramProjection: a subquery whose FINAL output columns are
// the OTel exponential (native) histogram's raw structural fields —
// Count, Sum, Scale, ZeroThreshold, ZeroCount, PositiveOffset,
// PositiveBucketCounts, NegativeOffset, NegativeBucketCounts — aliased
// to the fixed chplan.Histogram*Column names, rather than collapsed to
// a scalar the way histogram_quantile_native.go's emitter is.
//
// No lowering builds a chplan.HistogramProjection today; see that
// type's doc comment for the shared-prerequisite rationale (#1926).
// This file's only consumer today is internal/chsql's own tests,
// exercising the emitter directly against a hand-built plan node.
package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// emitHistogramProjection renders a chplan.HistogramProjection. The
// outer QueryBuilder projects the GroupBy columns aliased per
// GroupByAliases, then the nine histogram columns under their fixed
// chplan.Histogram*Column aliases — matching the shape a future
// decode-side consumer (chclient.Sample.Histogram) binds by name.
func (e *emitter) emitHistogramProjection(h *chplan.HistogramProjection) error {
	if h.Input == nil {
		return fmt.Errorf("%w: HistogramProjection.Input is nil", ErrUnsupported)
	}
	// ZeroThresholdColumn is intentionally NOT required, mirroring
	// HistogramQuantileNative: the upstream OTel-CH exp-histogram DDL
	// does not persist the OTLP zero_threshold field, so the default
	// schema leaves it empty and the projection renders a constant `0.`
	// literal for the HistogramZeroThreshold output column instead —
	// the output shape stays nine columns regardless of what the input
	// schema persists.
	if h.CountColumn == "" || h.SumColumn == "" || h.ScaleColumn == "" ||
		h.ZeroCountColumn == "" || h.PositiveOffsetColumn == "" ||
		h.PositiveBucketCountsColumn == "" || h.NegativeOffsetColumn == "" ||
		h.NegativeBucketCountsColumn == "" {
		return fmt.Errorf("%w: HistogramProjection requires Count / Sum / Scale / ZeroCount / "+
			"PositiveOffset / PositiveBucketCounts / NegativeOffset / NegativeBucketCounts column names",
			ErrUnsupported)
	}
	sub, err := e.subqueryFrag(h.Input)
	if err != nil {
		return err
	}

	sb := NewQuery().From(sub)
	for i, g := range h.GroupBy {
		expr := g
		alias := ""
		if i < len(h.GroupByAliases) {
			alias = h.GroupByAliases[i]
		}
		sb.SelectAs(func(b *Builder) { _ = b.Expr(expr) }, alias)
	}
	sb.SelectAs(Col(h.CountColumn), chplan.HistogramCountColumn)
	sb.SelectAs(Col(h.SumColumn), chplan.HistogramSumColumn)
	sb.SelectAs(Col(h.ScaleColumn), chplan.HistogramScaleColumn)
	sb.SelectAs(histogramProjectionZeroThresholdFrag(h), chplan.HistogramZeroThresholdColumn)
	sb.SelectAs(Col(h.ZeroCountColumn), chplan.HistogramZeroCountColumn)
	sb.SelectAs(Col(h.PositiveOffsetColumn), chplan.HistogramPositiveOffsetColumn)
	sb.SelectAs(Col(h.PositiveBucketCountsColumn), chplan.HistogramPositiveBucketCountsColumn)
	sb.SelectAs(Col(h.NegativeOffsetColumn), chplan.HistogramNegativeOffsetColumn)
	sb.SelectAs(Col(h.NegativeBucketCountsColumn), chplan.HistogramNegativeBucketCountsColumn)
	return e.emitSelect(sb)
}

// histogramProjectionZeroThresholdFrag renders the HistogramZeroThreshold
// output column: the stored per-row value when the schema persists one,
// or the CH-portable shape token `0.` when ZeroThresholdColumn is empty
// (see zeroBandOrigin in histogram_quantile_native.go, the same
// precedent for a constant Float64 literal that rides verbatim rather
// than through InlineLit).
func histogramProjectionZeroThresholdFrag(h *chplan.HistogramProjection) Frag {
	if h.ZeroThresholdColumn == "" {
		return zeroBandOrigin()
	}
	return Col(h.ZeroThresholdColumn)
}
