package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// histogram_float_vector_join.go emits chplan.HistogramFloatVectorJoin
// (cerberus issue #2339): a real INNER JOIN between a histogram-valued
// operand (Left, a *chplan.HistogramProjection) and a plain float-valued
// operand (Right), keyed on Attributes under DEFAULT one-to-one vector
// matching only.
//
// Unlike emitHistogramVectorJoin, the two sides are NOT symmetric: only
// Left carries the nine histogram fields, and they ride through under
// their PLAIN canonical names (MetricName, Attributes, TimeUnix, the
// fixed Histogram*Column set) rather than `_hq_L_*`/`_hq_R_*` — matching
// exactly what Left's own HistogramProjection cap already publishes, so
// the calling lowering can feed this node straight into the same
// per-bucket scale-fold literal-scalar scaling uses
// (internal/promql/histogram_native_scalar_binop.go's
// scaleHistogramProjection), reading the scale factor off the joined
// Value column instead of a compile-time constant. Right contributes
// only that single Value column, reusing [emitter.joinSideFrag] — the
// exact per-side "latest sample" argMax / derived-shape / step-aligned
// logic emitVectorJoin's own Right arm already exercises — so this file
// adds no new per-side join mechanics of its own, only the histogram
// Left arm and the outer SELECT/JOIN shape.
func (e *emitter) emitHistogramFloatVectorJoin(j *chplan.HistogramFloatVectorJoin) error {
	if err := e.validateHistogramFloatVectorJoinCols(j); err != nil {
		return err
	}

	leftFrag, err := e.histogramFloatVectorJoinHistSideFrag(j)
	if err != nil {
		return err
	}
	rightFrag, err := e.joinSideFrag(
		j.Match, j.MetricNameColumn, j.AttributesColumn, j.TimestampColumn, j.ValueColumn, j.StepAligned, j.Right, roleMany,
	)
	if err != nil {
		return err
	}

	cols := histogramFloatVectorJoinHistCols(j)
	selects := make([]Frag, 0, len(cols)+1)
	for _, col := range cols {
		selects = append(selects, As(qualColFrag("L", col), col))
	}
	selects = append(selects, As(qualColFrag("R", j.ValueColumn), j.ValueColumn))

	sb := NewQuery().
		Select(selects...).
		From(aliasedFrag(leftFrag, "L")).
		Join(InnerJoin, aliasedFrag(rightFrag, "R"), vectorMatchPredicateFrag(j.Match, j.AttributesColumn, j.TimestampColumn, j.StepAligned))
	return e.emitSelect(sb)
}

func (e *emitter) validateHistogramFloatVectorJoinCols(j *chplan.HistogramFloatVectorJoin) error {
	switch {
	case j.AttributesColumn == "":
		return fmt.Errorf("%w: HistogramFloatVectorJoin.AttributesColumn unset", ErrUnsupported)
	case j.MetricNameColumn == "":
		return fmt.Errorf("%w: HistogramFloatVectorJoin.MetricNameColumn unset", ErrUnsupported)
	case j.TimestampColumn == "":
		return fmt.Errorf("%w: HistogramFloatVectorJoin.TimestampColumn unset", ErrUnsupported)
	case j.ValueColumn == "":
		return fmt.Errorf("%w: HistogramFloatVectorJoin.ValueColumn unset", ErrUnsupported)
	case len(j.Match.Labels) != 0 || j.Match.On:
		// on()/ignoring() reduced-key matching for this histogram/
		// float-vector scaling shape is tracked as follow-up work (see
		// the node's own doc comment) — reject outright rather than
		// silently mis-joining on the wrong key.
		return fmt.Errorf("%w: HistogramFloatVectorJoin only supports default (full-Attributes) vector matching", ErrUnsupported)
	}
	return nil
}

// histogramFloatVectorJoinHistCols lists every field the join's Left
// (histogram) arm carries through: MetricName, Attributes, TimeUnix,
// then the nine fixed histogram payload columns — byte-identical to
// histogramVectorJoinFieldCols' own list, minus the Right side's fields
// (there are none to list: Right contributes only ValueColumn, handled
// separately by [emitter.joinSideFrag]).
func histogramFloatVectorJoinHistCols(j *chplan.HistogramFloatVectorJoin) []string {
	return []string{
		j.MetricNameColumn, j.AttributesColumn, j.TimestampColumn,
		chplan.HistogramScaleColumn, chplan.HistogramCountColumn, chplan.HistogramSumColumn,
		chplan.HistogramZeroCountColumn, chplan.HistogramZeroThresholdColumn,
		chplan.HistogramPositiveOffsetColumn, chplan.HistogramPositiveBucketCountsColumn,
		chplan.HistogramNegativeOffsetColumn, chplan.HistogramNegativeBucketCountsColumn,
	}
}

// histogramFloatVectorJoinHistSideFrag renders the join's Left
// (histogram) arm as a Frag emitting a parenthesised SELECT subquery. A
// *chplan.HistogramProjection input already carries at most one row per
// (series, anchor) by construction — the same "many" guarantee
// histogramVectorJoinSideFrag's own roleMany branch relies on — so this
// is a straight passthrough of the nine histogram fields plus
// MetricName/Attributes/TimeUnix, no aggregation needed: default
// (full-Attributes) matching is the only shape
// [validateHistogramFloatVectorJoinCols] admits, and a HistogramProjection
// row is already unique on that exact key.
//
// Field columns route through the shared `_join_*`-alias-then-outer-
// rename pattern vectorJoinSideFrag documents (breaks CH's alias-chain
// trace through the aggregation subquery, avoiding a false
// ILLEGAL_AGGREGATION) — joinAlias is reused verbatim from vector_join.go.
func (e *emitter) histogramFloatVectorJoinHistSideFrag(j *chplan.HistogramFloatVectorJoin) (Frag, error) {
	sub, err := e.subqueryFrag(j.Left)
	if err != nil {
		return nil, err
	}
	cols := histogramFloatVectorJoinHistCols(j)

	inner := NewQuery().From(sub)
	innerProjs := make([]Frag, 0, len(cols))
	for _, col := range cols {
		innerProjs = append(innerProjs, As(Col(col), joinAlias(col)))
	}
	inner.Select(innerProjs...)

	outerProjs := make([]Frag, 0, len(cols))
	for _, col := range cols {
		outerProjs = append(outerProjs, As(Col(joinAlias(col)), col))
	}
	outer := NewQuery().Select(outerProjs...).From(inner.Frag())
	return outer.Frag(), nil
}
