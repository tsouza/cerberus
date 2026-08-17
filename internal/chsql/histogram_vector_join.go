package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// emitHistogramVectorJoin renders group_left()/group_right() (many-to-one)
// matching between two histogram-valued operands as a real INNER JOIN —
// see chplan.HistogramVectorJoin's doc for why the one-to-one path's
// UnionAll+Aggregate `count() = 2` shape cannot express this broadcast.
//
// Mirrors emitVectorJoin's per-side role split (vector_join.go): the
// "many" role keeps per-series granularity — unlike VectorJoin's
// roleMany, no argMax "latest sample" pick is needed, because a
// HistogramProjection input already carries at most one row per
// (series, anchor) by construction, so the many side is a straight
// passthrough. The "one" role collapses to one row per Match-reduced key
// via `any()` per field, guarded by the same
// `throwIf(uniqExact(...) > 1, ManyToManyMatchMessage)` ambiguity check
// [matchCheckGuardFrag] plain VectorJoin's roleOne plants.
//
// Deliberately "dumb": every relevant field from BOTH sides — MetricName,
// Attributes (untouched, full — never reduced to the match key), TimeUnix,
// and each of the nine histogram payload columns — is projected under
// `_hq_L_<field>` / `_hq_R_<field>` aliases (Left/Right, matching the
// operator's syntactic LHS/RHS regardless of which plays the "many"/"one"
// role). The merge value fold, the compare equality check, and the
// Include-label output-Attributes overlay are NOT computed here — see
// chplan.HistogramVectorJoin's doc.
func (e *emitter) emitHistogramVectorJoin(j *chplan.HistogramVectorJoin) error {
	if err := e.validateHistogramVectorJoinCols(j); err != nil {
		return err
	}
	leftRole, rightRole := histogramVectorJoinRoles(j)

	leftFrag, err := e.histogramVectorJoinSideFrag(j, j.Left, leftRole)
	if err != nil {
		return err
	}
	rightFrag, err := e.histogramVectorJoinSideFrag(j, j.Right, rightRole)
	if err != nil {
		return err
	}

	cols := histogramVectorJoinFieldCols(j)
	selects := make([]Frag, 0, 2*len(cols))
	for _, col := range cols {
		selects = append(
			selects,
			As(qualColFrag("L", col), histogramVectorJoinAlias("L", col)),
			As(qualColFrag("R", col), histogramVectorJoinAlias("R", col)),
		)
	}

	sb := NewQuery().
		Select(selects...).
		From(aliasedFrag(leftFrag, "L")).
		Join(InnerJoin, aliasedFrag(rightFrag, "R"), vectorMatchPredicateFrag(j.Match, j.AttributesColumn, j.TimestampColumn, j.StepAligned))
	return e.emitSelect(sb)
}

func (e *emitter) validateHistogramVectorJoinCols(j *chplan.HistogramVectorJoin) error {
	switch {
	case j.AttributesColumn == "":
		return fmt.Errorf("%w: HistogramVectorJoin.AttributesColumn unset", ErrUnsupported)
	case j.MetricNameColumn == "":
		return fmt.Errorf("%w: HistogramVectorJoin.MetricNameColumn unset", ErrUnsupported)
	case j.TimestampColumn == "":
		return fmt.Errorf("%w: HistogramVectorJoin.TimestampColumn unset", ErrUnsupported)
	case j.Card != chplan.CardManyToOne && j.Card != chplan.CardOneToMany:
		return fmt.Errorf("%w: HistogramVectorJoin.Card must be CardManyToOne or CardOneToMany", ErrUnsupported)
	}
	return nil
}

// histogramVectorJoinRoles resolves the per-side aggregation roles —
// CardManyToOne (`group_left`) → Left is many, Right is one.
// CardOneToMany (`group_right`) → Right is many, Left is one. Validated by
// validateHistogramVectorJoinCols to be one of exactly these two before
// this is called.
func histogramVectorJoinRoles(j *chplan.HistogramVectorJoin) (sideRole, sideRole) {
	if j.Card == chplan.CardManyToOne {
		return roleMany, roleOne
	}
	return roleOne, roleMany
}

// histogramVectorJoinFieldCols lists every field the join carries through
// from both sides — MetricName, Attributes (untouched, full), TimeUnix,
// then the nine histogram payload columns. Left/Right are each an
// already-lowered *chplan.HistogramProjection, whose own emitter has
// already normalised ZeroThreshold to a real column (a literal `0.`
// placeholder when the physical schema persists none) — so, unlike
// histogramCompareFieldColumns' conditional (which guards against a RAW,
// possibly-ZeroThreshold-less schema), ZeroThreshold is unconditionally
// present here.
func histogramVectorJoinFieldCols(j *chplan.HistogramVectorJoin) []string {
	return []string{
		j.MetricNameColumn, j.AttributesColumn, j.TimestampColumn,
		chplan.HistogramScaleColumn, chplan.HistogramCountColumn, chplan.HistogramSumColumn,
		chplan.HistogramZeroCountColumn, chplan.HistogramZeroThresholdColumn,
		chplan.HistogramPositiveOffsetColumn, chplan.HistogramPositiveBucketCountsColumn,
		chplan.HistogramNegativeOffsetColumn, chplan.HistogramNegativeBucketCountsColumn,
	}
}

// histogramVectorJoinAlias names the outer SELECT's output column
// carrying side's own value of col — `_hq_L_<col>` / `_hq_R_<col>`.
func histogramVectorJoinAlias(side, col string) string {
	return "_hq_" + side + "_" + col
}

// histogramVectorJoinSideFrag renders one side of the join as a Frag
// emitting a parenthesised SELECT subquery. roleMany passes every field
// of n straight through (no aggregation: a HistogramProjection input is
// already one row per (series, anchor)). roleOne collapses to one row
// per Match-reduced key via `any()` per field, with the same
// `throwIf(uniqExact(...) > 1, ...)` guard plain VectorJoin's roleOne
// uses.
//
// Field columns route through the shared `_join_*`-alias-then-outer-
// rename pattern vectorJoinSideFrag documents (breaks CH's alias-chain
// trace through the aggregation subquery, avoiding a false
// ILLEGAL_AGGREGATION) — joinAlias / matchCheckGuardFrag /
// matchKeyGroupExprFrag are reused verbatim from vector_join.go.
func (e *emitter) histogramVectorJoinSideFrag(j *chplan.HistogramVectorJoin, n chplan.Node, role sideRole) (Frag, error) {
	sub, err := e.subqueryFrag(n)
	if err != nil {
		return nil, err
	}
	cols := histogramVectorJoinFieldCols(j)
	inner := NewQuery().From(sub)

	if role == roleMany {
		projs := make([]Frag, 0, len(cols))
		for _, col := range cols {
			projs = append(projs, As(Col(col), joinAlias(col)))
		}
		inner.Select(projs...)
	} else {
		groupFrags := []Frag{matchKeyGroupExprFrag(j.Match, j.AttributesColumn)}
		if j.StepAligned {
			groupFrags = append(groupFrags, Col(j.TimestampColumn))
		}
		projs := make([]Frag, 0, len(cols))
		for _, col := range cols {
			projs = append(projs, As(Call("any", Col(col)), joinAlias(col)))
		}
		inner.Select(projs...).
			GroupBy(groupFrags...).
			Having(matchCheckGuardFrag(j.AttributesColumn))
	}

	outerProjs := make([]Frag, 0, len(cols))
	for _, col := range cols {
		outerProjs = append(outerProjs, As(Col(joinAlias(col)), col))
	}
	outer := NewQuery().Select(outerProjs...).From(inner.Frag())
	return outer.Frag(), nil
}
