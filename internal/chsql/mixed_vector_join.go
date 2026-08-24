package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// emitMixedVectorJoin renders a vector-vector PromQL binary expression
// whose BOTH operands are themselves a mixed float/histogram `or`
// (internal/promql's histogram_native_mixed_or.go, cerberus issue #2330)
// as a real INNER JOIN of two per-side aggregations — [chplan.MixedVectorJoin]'s
// own doc has the design.
//
// Mirrors [emitHistogramVectorJoin]'s shape almost exactly, generalised
// from the nine histogram-only fields to all fourteen Mixed columns via
// the SAME generic per-side aggregation helper
// ([emitter.histogramFieldsJoinSideFrag]) — this node just supplies a
// different field list and resolves its OWN roleMany/roleOne split the
// way plain [emitVectorJoin]'s does ([mixedVectorJoinRoles]): CardManyToOne
// (`group_left`) keeps Left at per-series granularity (roleMany) and
// collapses Right to one row per matching key (roleOne); CardOneToMany
// (`group_right`) mirrors with sides swapped; CardOneToOne with a
// full-Attributes match keeps both sides at per-series granularity
// (roleMany — the per-series uniqueness is already guaranteed by
// construction, since each side is itself an already-reduced Mixed
// VectorSetOp); CardOneToOne with an on()/ignoring() subset match
// collapses both sides to one row per matching key with the runtime
// `throwIf(uniqExact(...) > 1, ...)` ambiguity guard (roleOne).
//
// Deliberately "dumb" about the per-combination fold: every field of both
// sides is exposed under `_mvj_L_<field>` / `_mvj_R_<field>` aliases, and
// the caller (internal/promql's
// histogram_native_mixed_or_vector_arithmetic.go /
// histogram_native_mixed_or_vector_comparison.go) builds the Value /
// Histogram* / discriminator / output-Attributes projections on top —
// including the Card-aware manySide+Include output-Attributes overlay,
// this node has no notion of.
func (e *emitter) emitMixedVectorJoin(j *chplan.MixedVectorJoin) error {
	if err := e.validateMixedVectorJoinCols(j); err != nil {
		return err
	}
	leftRole, rightRole := mixedVectorJoinRoles(j)
	cols := mixedVectorJoinFieldCols(j)

	leftFrag, err := e.histogramFieldsJoinSideFrag(j.Match, j.AttributesColumn, j.TimestampColumn, j.StepAligned, cols, j.Left, leftRole)
	if err != nil {
		return err
	}
	rightFrag, err := e.histogramFieldsJoinSideFrag(j.Match, j.AttributesColumn, j.TimestampColumn, j.StepAligned, cols, j.Right, rightRole)
	if err != nil {
		return err
	}

	selects := make([]Frag, 0, 2*len(cols))
	for _, col := range cols {
		selects = append(
			selects,
			As(qualColFrag("L", col), mixedVectorJoinAlias("L", col)),
			As(qualColFrag("R", col), mixedVectorJoinAlias("R", col)),
		)
	}

	sb := NewQuery().
		Select(selects...).
		From(aliasedFrag(leftFrag, "L")).
		Join(InnerJoin, aliasedFrag(rightFrag, "R"), vectorMatchPredicateFrag(j.Match, j.AttributesColumn, j.TimestampColumn, j.StepAligned))
	return e.emitSelect(sb)
}

func (e *emitter) validateMixedVectorJoinCols(j *chplan.MixedVectorJoin) error {
	switch {
	case j.AttributesColumn == "":
		return fmt.Errorf("%w: MixedVectorJoin.AttributesColumn unset", ErrUnsupported)
	case j.MetricNameColumn == "":
		return fmt.Errorf("%w: MixedVectorJoin.MetricNameColumn unset", ErrUnsupported)
	case j.TimestampColumn == "":
		return fmt.Errorf("%w: MixedVectorJoin.TimestampColumn unset", ErrUnsupported)
	case j.ValueColumn == "":
		return fmt.Errorf("%w: MixedVectorJoin.ValueColumn unset", ErrUnsupported)
	}
	return nil
}

// mixedVectorJoinRoles resolves the per-side aggregation roles for a
// MixedVectorJoin, mirroring [vectorJoinRoles] exactly:
//
//   - CardManyToOne (`group_left`)   → Left is many, Right is one.
//   - CardOneToMany (`group_right`)  → Right is many, Left is one.
//   - CardOneToOne with subset match → both sides are "one" (uniqueness
//     enforced at runtime).
//   - CardOneToOne with full-Attributes match → both sides are "many";
//     the per-series aggregation already guarantees one row per matching
//     key, since each side is already an at-most-one-row-per-series
//     Mixed VectorSetOp.
func mixedVectorJoinRoles(j *chplan.MixedVectorJoin) (sideRole, sideRole) {
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

// mixedVectorJoinFieldCols lists all fourteen Mixed columns the join
// carries through from both sides: the canonical quartet, the nine
// Histogram*Column outputs, and the trailing [chplan.MixedDiscriminatorColumn].
func mixedVectorJoinFieldCols(j *chplan.MixedVectorJoin) []string {
	return []string{
		j.MetricNameColumn, j.AttributesColumn, j.TimestampColumn, j.ValueColumn,
		chplan.HistogramCountColumn, chplan.HistogramSumColumn, chplan.HistogramScaleColumn,
		chplan.HistogramZeroThresholdColumn, chplan.HistogramZeroCountColumn,
		chplan.HistogramPositiveOffsetColumn, chplan.HistogramPositiveBucketCountsColumn,
		chplan.HistogramNegativeOffsetColumn, chplan.HistogramNegativeBucketCountsColumn,
		chplan.MixedDiscriminatorColumn,
	}
}

// mixedVectorJoinAlias names the outer SELECT's output column carrying
// side's own value of col — `_mvj_L_<col>` / `_mvj_R_<col>`. MUST match
// internal/promql's mixedJoinFieldAlias byte-for-byte; the two are
// independent literals rather than a shared constant because
// internal/promql may not depend on internal/chsql (see .go-arch-lint.yml
// — the same boundary [chplan.ManyToManyMatchMessage]'s doc explains, and
// the same duplication [histogramVectorJoinAlias] /
// internal/promql/histogram_native_binop_card.go's histJoinFieldAlias
// already carry).
func mixedVectorJoinAlias(side, col string) string {
	return "_mvj_" + side + "_" + col
}
