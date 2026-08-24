package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// histogram_float_vector_join.go emits chplan.HistogramFloatVectorJoin
// (cerberus issue #2339, widened to on()/ignoring()/group_left()/
// group_right() by #2342, further widened to the histogram operand
// playing either the "many" or the "one" role under group_left()/
// group_right() regardless of which side of the PromQL expression it was
// written on by #2537): a real INNER JOIN between a histogram-valued
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
// (the histogram side) at roleMany and collapses Right to roleOne;
// CardOneToMany is the mirror image — Left (the histogram side)
// collapses to roleOne, broadcasting the single matched histogram across
// every matching Right (float) row at roleMany. Left reuses
// [emitter.histogramFieldsJoinSideFrag] — the exact collapse-and-guard
// logic emitHistogramVectorJoin's own roleOne arm already exercises.
// Right reuses [emitter.joinSideFrag] — the exact per-side "latest
// sample" argMax / derived-shape / step-aligned logic emitVectorJoin's
// own Right arm already exercises — so this file adds no new per-side
// join mechanics of its own, only the histogram Left arm's field list,
// the output-Attributes fold (CardOneToOne Keep/Del, sided by HistIsLHS,
// or CardManyToOne/CardOneToMany Include overlay — [histogramFloat
// VectorJoinOutputAttributesFrag]), and the outer SELECT/JOIN shape.
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
	case j.Card != chplan.CardOneToOne && j.Card != chplan.CardManyToOne && j.Card != chplan.CardOneToMany:
		return fmt.Errorf("%w: HistogramFloatVectorJoin.Card must be CardOneToOne, CardManyToOne, or CardOneToMany", ErrUnsupported)
	}
	return nil
}

// histogramFloatVectorJoinRoles resolves the per-side aggregation roles,
// mirroring [vectorJoinRoles] / [histogramVectorJoinRoles]:
//
//   - CardManyToOne → Left (histogram) is many, Right (float) is one.
//   - CardOneToMany → Left (histogram) is one, Right (float) is many —
//     the single matched histogram row broadcasts across every matching
//     float row (cerberus issue #2537).
//   - CardOneToOne with full-Attributes match → both sides are "many";
//     the per-series aggregation already guarantees one row per
//     matching key.
//   - CardOneToOne with a subset (on()/ignoring()) match → both sides
//     are "one" (uniqueness enforced at runtime on both).
func histogramFloatVectorJoinRoles(j *chplan.HistogramFloatVectorJoin) (sideRole, sideRole) {
	switch j.Card {
	case chplan.CardManyToOne:
		return roleMany, roleOne
	case chplan.CardOneToMany:
		return roleOne, roleMany
	}
	if len(j.Match.Labels) == 0 && !j.Match.On {
		return roleMany, roleMany
	}
	return roleOne, roleOne
}

// histogramFloatVectorJoinOutputAttributesFrag returns the join's output
// Attributes expression:
//
//   - CardOneToOne: Left (the histogram operand)'s own Attributes,
//     reduced to the matching label set per Prometheus's resultMetric
//     Keep/Del rule ([outputMatchSetFrag], the exact same reduction
//     emitVectorJoin's own CardOneToOne output applies). Prometheus
//     reduces the SYNTACTIC LHS operand's own labels, which is not
//     always Left here — but the join's own ON-clause equality already
//     forces L's and R's reduced Attributes to be byte-identical for any
//     row that joins at all (see [chplan.HistogramFloatVectorJoin]'s own
//     doc), so reducing Left unconditionally is correct regardless of
//     which side of `*`/`/` the histogram operand was written on.
//     Default matching is a no-op reduction, so this renders the
//     identical bare `L.Attributes` the pre-#2342 code always emitted —
//     byte-stable for every existing default-matching fixture.
//   - CardManyToOne / CardOneToMany: the "many" side's full Attributes
//     (Left under CardManyToOne, Right under CardOneToMany), optionally
//     overlaid with the "one" side's Include labels via `mapConcat` —
//     CH's later-argument-wins map merge, mirroring
//     [outputAttributesFrag]'s identical group_left/right overlay for
//     plain VectorJoin. Bare group_left/right (no Include labels) leaves
//     the "many" side's Attributes unchanged: Prometheus's CardOneToOne
//     Keep/Del reduction does not apply to a many-to-one cardinality.
func histogramFloatVectorJoinOutputAttributesFrag(j *chplan.HistogramFloatVectorJoin) Frag {
	attrs := j.AttributesColumn
	manySide := ""
	switch j.Card {
	case chplan.CardManyToOne:
		manySide = "L"
	case chplan.CardOneToMany:
		manySide = "R"
	}
	if manySide == "" {
		return outputMatchSetFrag(j.Match, "L", attrs)
	}
	if len(j.Include) == 0 {
		return qualColFrag(manySide, attrs)
	}
	oneSide := "R"
	if manySide == "R" {
		oneSide = "L"
	}
	includes := make([]Frag, len(j.Include))
	for i, lbl := range j.Include {
		includes[i] = Lit(lbl)
	}
	overlay := Call(
		"mapFilter",
		Lambda2("k", "v", In(BareIdent("k"), includes...)),
		qualColFrag(oneSide, attrs),
	)
	return Call("mapConcat", qualColFrag(manySide, attrs), overlay)
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
