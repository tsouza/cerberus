package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// histogram_float_vector_join.go emits chplan.HistogramFloatVectorJoin
// (cerberus issue #2339, widened to on()/ignoring()/group_left()/
// group_right() by #2342): a real INNER JOIN between a histogram-valued
// operand (Left, a *chplan.HistogramProjection) and a plain float-valued
// operand (Right), keyed on Match.
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
// Value column instead of a compile-time constant.
//
// The per-side role split mirrors emitVectorJoin's own
// [vectorJoinRoles] / emitHistogramVectorJoin's [histogramVectorJoinRoles]
// (see [histogramFloatVectorJoinRoles]): default (full-Attributes)
// CardOneToOne keeps both sides at roleMany (each already unique on the
// full key by construction); an on()/ignoring() reduced key makes both
// sides roleOne (collapse + ambiguity guard); CardManyToOne keeps Left
// (the histogram side, always the "many" under the only cardinality this
// node supports) at roleMany and collapses Right to roleOne. Left reuses
// [emitter.histogramFieldsJoinSideFrag] — the exact collapse-and-guard
// logic emitHistogramVectorJoin's own roleOne arm already exercises.
// Right reuses [emitter.joinSideFrag] — the exact per-side "latest
// sample" argMax / derived-shape / step-aligned logic emitVectorJoin's
// own Right arm already exercises — so this file adds no new per-side
// join mechanics of its own, only the histogram Left arm's field list,
// the output-Attributes fold (CardOneToOne Keep/Del or CardManyToOne
// Include overlay — [histogramFloatVectorJoinOutputAttributesFrag]), and
// the outer SELECT/JOIN shape.
func (e *emitter) emitHistogramFloatVectorJoin(j *chplan.HistogramFloatVectorJoin) error {
	if err := e.validateHistogramFloatVectorJoinCols(j); err != nil {
		return err
	}
	leftRole, rightRole := histogramFloatVectorJoinRoles(j)

	leftFrag, err := e.histogramFloatVectorJoinHistSideFrag(j, leftRole)
	if err != nil {
		return err
	}
	rightFrag, err := e.joinSideFrag(
		j.Match, j.MetricNameColumn, j.AttributesColumn, j.TimestampColumn, j.ValueColumn, j.StepAligned, j.Right, rightRole,
	)
	if err != nil {
		return err
	}

	cols := histogramFloatVectorJoinHistCols(j)
	selects := make([]Frag, 0, len(cols)+1)
	for _, col := range cols {
		if col == j.AttributesColumn {
			selects = append(selects, As(histogramFloatVectorJoinOutputAttributesFrag(j), col))
			continue
		}
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
	case j.Card != chplan.CardOneToOne && j.Card != chplan.CardManyToOne:
		// CardOneToMany would mean the histogram side plays the "one"
		// role, broadcasting a single histogram across many float rows —
		// not a shape this node supports (see its own doc comment).
		// Reject outright rather than silently mis-joining.
		return fmt.Errorf("%w: HistogramFloatVectorJoin.Card must be CardOneToOne or CardManyToOne", ErrUnsupported)
	}
	return nil
}

// histogramFloatVectorJoinRoles resolves the per-side aggregation roles,
// mirroring [vectorJoinRoles] / [histogramVectorJoinRoles]:
//
//   - CardManyToOne → Left (histogram) is many, Right (float) is one —
//     the only broadcast direction this node supports.
//   - CardOneToOne with full-Attributes match → both sides are "many";
//     the per-series aggregation already guarantees one row per
//     matching key.
//   - CardOneToOne with a subset (on()/ignoring()) match → both sides
//     are "one" (uniqueness enforced at runtime on both).
func histogramFloatVectorJoinRoles(j *chplan.HistogramFloatVectorJoin) (sideRole, sideRole) {
	if j.Card == chplan.CardManyToOne {
		return roleMany, roleOne
	}
	if len(j.Match.Labels) == 0 && !j.Match.On {
		return roleMany, roleMany
	}
	return roleOne, roleOne
}

// histogramFloatVectorJoinOutputAttributesFrag returns the join's output
// Attributes expression:
//
//   - CardOneToOne: the histogram (Left) side's own Attributes, reduced
//     to the matching label set per Prometheus's resultMetric Keep/Del
//     rule — [outputMatchSetFrag], the exact same reduction
//     emitVectorJoin's own CardOneToOne output applies. Default matching
//     is a no-op reduction, so this renders the identical bare `L.
//     Attributes` the pre-#2342 code always emitted — byte-stable for
//     every existing default-matching fixture.
//   - CardManyToOne: the "many" side's (Left, the histogram operand)
//     full Attributes, optionally overlaid with the "one" side's
//     (Right, the float operand) Include labels via `mapConcat` — CH's
//     later-argument-wins map merge, mirroring [outputAttributesFrag]'s
//     identical group_left/right overlay for plain VectorJoin. Bare
//     group_left/right (no Include labels) leaves Left's Attributes
//     unchanged: Prometheus's CardOneToOne Keep/Del reduction does not
//     apply to a many-to-one cardinality.
func histogramFloatVectorJoinOutputAttributesFrag(j *chplan.HistogramFloatVectorJoin) Frag {
	attrs := j.AttributesColumn
	if j.Card != chplan.CardManyToOne {
		return outputMatchSetFrag(j.Match, "L", attrs)
	}
	if len(j.Include) == 0 {
		return qualColFrag("L", attrs)
	}
	includes := make([]Frag, len(j.Include))
	for i, lbl := range j.Include {
		includes[i] = Lit(lbl)
	}
	overlay := Call(
		"mapFilter",
		Lambda2("k", "v", In(BareIdent("k"), includes...)),
		qualColFrag("R", attrs),
	)
	return Call("mapConcat", qualColFrag("L", attrs), overlay)
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
// (histogram) arm as a Frag emitting a parenthesised SELECT subquery —
// a thin wrapper over [emitter.histogramFieldsJoinSideFrag] (shared with
// [emitter.histogramVectorJoinSideFrag], internal/chsql/
// histogram_vector_join.go). roleMany (default matching, or CardManyToOne
// where the histogram side is always the "many") is a straight
// passthrough of the nine histogram fields plus MetricName/Attributes/
// TimeUnix — a *chplan.HistogramProjection input already carries at most
// one row per (series, anchor) by construction, which for default
// matching already IS the matching key. roleOne (an on()/ignoring()
// reduced key under CardOneToOne) collapses to one row per Match-reduced
// key via `any()` per field, guarded by the same
// `throwIf(uniqExact(...) > 1, ...)` ambiguity check plain VectorJoin's
// roleOne uses.
func (e *emitter) histogramFloatVectorJoinHistSideFrag(j *chplan.HistogramFloatVectorJoin, role sideRole) (Frag, error) {
	return e.histogramFieldsJoinSideFrag(j.Match, j.AttributesColumn, j.TimestampColumn, j.StepAligned, histogramFloatVectorJoinHistCols(j), j.Left, role)
}
