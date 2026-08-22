package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// Synthetic bare-identifier column names the VectorSetOr single-pass
// emission pins. `_setop_side` tags each UNION-ALL arm (0 = LHS, 1 =
// RHS) so the windowed `_setop_has_left` flag — `max(_setop_side = 0)
// OVER (PARTITION BY <sig>)` — can decide which RHS rows survive the
// `or` (drop those whose signature already has a LHS row). Neither name
// takes user input; both match CH's bare-identifier grammar.
const (
	setOpSideCol    = "_setop_side"
	setOpHasLeftCol = "_setop_has_left"
	// setOpMixedIsHistogramCol is the trailing per-row discriminator a
	// Mixed VectorSetOr's fourteen-column projection carries (cerberus
	// issue #2330): 1 when the row came from the histogram-shaped arm
	// (its nine Histogram*Column outputs are real, its Value is the
	// [HistogramProjection] placeholder), 0 when it came from the
	// float-shaped arm (the mirror image — Value is real, the nine
	// histogram columns are placeholders). internal/chclient/cursor.go's
	// shapeSampleMixed probes for this exact trailing alias, the same
	// way shapeSampleHistogram probes for histogramLastColumn.
	// chsql may (and does) import chplan, so this aliases
	// [chplan.MixedDiscriminatorColumn] — the single definition
	// [chplan.RowShapeOf]'s own [*chplan.Project] case reads back — rather
	// than duplicating the literal the way chclient's decode-side probe
	// has to.
	setOpMixedIsHistogramCol = chplan.MixedDiscriminatorColumn
)

// emitVectorSetOp renders a PromQL vector set operator (`and`, `or`,
// `unless`) over the two child plans. The shape depends on the kind:
//
//   - VectorSetAnd (`A and B`): semi-join — keep LHS rows whose match
//     key appears in B. Rendered as
//     `SELECT MetricName, Attributes, TimeUnix, Value FROM (SELECT * FROM (<A>) WHERE <key> IN (SELECT DISTINCT <key> FROM (<canonical B>)))`.
//
//   - VectorSetUnless (`A unless B`): anti-join — keep LHS rows whose
//     match key does NOT appear in B. Rendered as
//     `SELECT MetricName, Attributes, TimeUnix, Value FROM (SELECT * FROM (<A>) WHERE <key> NOT IN (SELECT DISTINCT <key> FROM (<canonical B>)))`.
//
//   - VectorSetOr (`A or B`): left + anti-right — every LHS row plus
//     every RHS row whose match-key signature does NOT appear in A.
//     Rendered as
//     `SELECT MetricName, Attributes, TimeUnix, Value FROM ((SELECT * FROM (<A>)) UNION ALL (SELECT * FROM (<B>) WHERE <sig> NOT IN (SELECT DISTINCT <sig> FROM (<A>))))`.
//
// The outer `SELECT MetricName, Attributes, TimeUnix, Value` is required
// for the round-trip test runner, which wraps the Map column in
// `toJSONString(...)` before handing the SQL to chDB's parquet driver
// (a bare `SELECT *` over a Map column trips chdb-go's parquet scanner).
// We isolate the IN / NOT IN / UNION ALL plumbing in an inner sub-SELECT
// (renderable with `SELECT *` because the Map column never crosses the
// outer projection boundary) so the outer alias `Attributes` only ever
// resolves to the subquery's Map column. Inlining the WHERE on the
// outer SELECT would otherwise make CH's optimizer push the alias
// rewrite (`toJSONString(Attributes)`) into the WHERE comparison,
// producing a `String IN Set<Map>` type mismatch.
//
// The match key is a label signature — the full Attributes column
// (default), or the projected mapFilter expression for `on(...)` /
// `ignoring(...)` — extended with the evaluation timestamp in range
// mode (StepAligned), where PromQL matches the arms once per evaluation
// timestamp and every arm carries per-step rows under the shared grid
// anchor. Instant mode keys on the signature alone: each arm holds one
// row per series under an arm-local timestamp.
//
// The IN-subquery uses `SELECT DISTINCT` so a many-rows-per-key RHS
// doesn't blow up the IN list — set ops are inherently many-to-many on
// labels (PromQL parser rejects `group_left` / `group_right` here), so
// the DISTINCT is both semantically correct and small. Its source is
// the CANONICALISED right arm, whose 4-column projection guarantees the
// TimestampColumn the range key references.
//
// Set ops never derive a new sample — they filter / union existing
// ones — so MetricName / Attributes / TimeUnix / Value flow through
// each surviving row unchanged. The `__name__` retention rule is the
// opposite of arithmetic / comparison V-V binops where the metric name
// is always dropped.
func (e *emitter) emitVectorSetOp(s *chplan.VectorSetOp) error {
	if err := e.validateVectorSetOpCols(s); err != nil {
		return err
	}
	if s.Mixed {
		return e.emitMixedVectorSetOp(s)
	}

	leftFrag, err := e.subqueryFrag(s.Left)
	if err != nil {
		return err
	}
	rightFrag, err := e.subqueryFrag(s.Right)
	if err != nil {
		return err
	}

	// Canonicalise each arm's projection to (MetricName, Attributes,
	// TimeUnix, Value) regardless of whether the inner plan exposes
	// MetricName directly. Without this normalisation, mixing arms with
	// different column shapes — e.g. a canonical-shape Project
	// (MetricName, Attributes, TimeUnix, Value) on one side and a matrix
	// RangeWindow (Attributes, anchor_ts, TimeUnix, Value) on the other —
	// produces a UNION ALL where positional column type unification fails
	// with NO_COMMON_TYPE (String vs Map). Three-arm `A or B or C`
	// recursion hits this: the inner `(A or B)` already projects the
	// canonical shape (MetricName-first), while a sibling `increase(C)[
	// 5m]` arm in range mode projects Attributes-first; the outer UNION
	// then tries to coalesce String and Map at column position 0. See
	// the docstring on emitVectorSetOp for the original 2-arm motivation;
	// the per-arm canonical projection covers both that case and the
	// matrix-shape mismatch surfaced by the dashboard sweep
	// (otelcol dashboard's "refused / send-failed / dropped" panels).
	leftArm := vectorSetOpCanonicalArmFrag(s, s.Left, leftFrag)
	rightArm := vectorSetOpCanonicalArmFrag(s, s.Right, rightFrag)

	switch s.Op {
	case chplan.VectorSetAnd:
		// SELECT MetricName, Attributes, TimeUnix, Value
		//   FROM (SELECT MetricName, Attributes, TimeUnix, Value FROM (<A>) WHERE <sig> IN (SELECT DISTINCT <sig> FROM (<B>)))
		inner := NewQuery().
			Select(vectorSetOpOutputCols(s)...).
			From(leftArm).
			Where(setOpInSubqueryFrag(s, rightArm, true /*in*/))
		outer := NewQuery().
			Select(vectorSetOpOutputCols(s)...).
			From(inner.Frag())
		return e.emitSelect(outer)
	case chplan.VectorSetUnless:
		// SELECT MetricName, Attributes, TimeUnix, Value
		//   FROM (SELECT MetricName, Attributes, TimeUnix, Value FROM (<A>) WHERE <sig> NOT IN (SELECT DISTINCT <sig> FROM (<B>)))
		inner := NewQuery().
			Select(vectorSetOpOutputCols(s)...).
			From(leftArm).
			Where(setOpInSubqueryFrag(s, rightArm, false /*notIn*/))
		outer := NewQuery().
			Select(vectorSetOpOutputCols(s)...).
			From(inner.Frag())
		return e.emitSelect(outer)
	case chplan.VectorSetOr:
		// `A or B`: every LHS sample, plus every RHS sample whose match-
		// key signature does NOT appear in A. Rendered as a SINGLE pass
		// over `A UNION ALL B`, tagging each arm with a side marker and
		// a windowed "does a left row share this signature?" flag:
		//
		//   SELECT MetricName, Attributes, TimeUnix, Value FROM (
		//     SELECT MetricName, Attributes, TimeUnix, Value, _setop_side,
		//            max(_setop_side = 0) OVER (PARTITION BY <sig>)
		//              AS _setop_has_left
		//       FROM (
		//         (SELECT <canonical A cols>, 0 AS _setop_side FROM (<A>))
		//         UNION ALL
		//         (SELECT <canonical B cols>, 1 AS _setop_side FROM (<B>))
		//       )
		//   ) WHERE (_setop_side = 0) OR (_setop_has_left = 0)
		//
		// WHY this shape and NOT a CTE / anti-join.
		// The previous emission referenced the LHS in two places — the
		// UNION-ALL left leg AND a `<sig> NOT IN (SELECT DISTINCT <sig>
		// FROM <lhs>)` anti-join. #810 hoisted the LHS into a non-
		// recursive `WITH _setop_lhs_<n> AS (…)` CTE to stop the SQL
		// TEXT doubling per chain level, but ClickHouse does NOT
		// materialise a non-recursive CTE — it INLINES it at every
		// reference. So a left-assoc chain `((a or b) or c) …` still
		// RE-EXECUTED the whole nested LHS subplan twice per level →
		// super-linear EXECUTION (BUG #88: K=2/4/6/8 ≈ 38ms/205ms/1.4s/
		// 12.9s on the scaling harness, ~312x over a 4x K sweep) even
		// though SQL text + intermediate cardinality stayed linear.
		//
		// This single-pass shape scans each arm EXACTLY ONCE: the
		// "is there a left row for this signature?" test that drove the
		// anti-join becomes a window aggregate over the unified arms, so
		// the LHS subplan is never re-read. The result is byte-identical
		// to the anti-join — left rows (`_setop_side = 0`) always
		// survive; right rows survive iff no left row shares their
		// signature (`_setop_has_left = 0`). on()/ignoring() flow through
		// the same matchKeyGroupExprFrag signature used by the window's
		// PARTITION BY, so default / on / ignoring matching are all
		// covered. Measured K=1..8 on chDB: ~flat/sub-quadratic (K=8 at
		// ~77ms vs the CTE-inline shape's ~721ms — 9.4x).
		//
		// Each arm still projects the canonical 4-column shape via
		// vectorSetOpCanonicalArmFrag so positional UNION-ALL column
		// unification never hits the String-vs-Map (NO_COMMON_TYPE)
		// supertype error when one arm is a derived-shape RangeWindow /
		// Aggregate that drops `__name__`. The side marker is appended as
		// a 5th projected column so it never disturbs that unification.
		//
		// The window-bearing inner SELECT must PROJECT `_setop_side`
		// explicitly (alongside `_setop_has_left`): the outer WHERE
		// references it, and CH 24.x cannot resolve a UNION-arm alias
		// from a window-SELECT's own WHERE — lifting the filter one level
		// out, with `_setop_side` re-projected, is what makes the
		// analyzer bind it.
		sideArmL := vectorSetOpSideArmFrag(s, leftArm, 0)
		sideArmR := vectorSetOpSideArmFrag(s, rightArm, 1)
		windowed := NewQuery().
			Select(append(
				vectorSetOpOutputCols(s),
				Col(setOpSideCol),
				As(
					Window(
						Call("max", Eq(Col(setOpSideCol), InlineLit(0))),
						setOpMatchKeyFrags(s.Match, s.AttributesColumn, s.TimestampColumn, s.StepAligned),
						nil,
					),
					setOpHasLeftCol,
				),
			)...).
			From(Paren(UnionAll(sideArmL, sideArmR)))
		outer := NewQuery().
			Select(vectorSetOpOutputCols(s)...).
			From(windowed.Frag()).
			Where(Or(
				Eq(Col(setOpSideCol), InlineLit(0)),
				Eq(Col(setOpHasLeftCol), InlineLit(0)),
			))
		return e.emitSelect(outer)
	}
	return fmt.Errorf("%w: vector set op %q", ErrUnsupported, s.Op)
}

// emitMixedVectorSetOp renders a VectorSetOr whose two arms disagree on
// value type — one is histogram-shaped, one is a plain float sample
// (cerberus issue #2330; chplan.VectorSetOp.Mixed's doc comment has the
// design). It is the identical single-pass left+anti-right shape
// [emitVectorSetOp]'s VectorSetOr case renders — a side marker plus a
// windowed "does a left row share this signature?" flag deciding which
// right-arm rows survive — with two differences localised entirely to
// the per-arm projection:
//
//  1. Each arm is canonicalised to FOURTEEN columns instead of four —
//     the canonical quartet, the nine Histogram*Column outputs, and a
//     trailing discriminator — via [mixedVectorSetOpArmFrag], instead of
//     [vectorSetOpCanonicalArmFrag]. The histogram-shaped arm publishes
//     its nine columns for real and 0/1 for the discriminator; the
//     float-shaped arm publishes nine typed placeholders (matching the
//     exact CH types histogramFloatFrag / histogramIndexFrag /
//     histogramBucketsFrag pin, so the UNION ALL's positional type
//     unification never hits NO_COMMON_TYPE) and the opposite
//     discriminator value.
//  2. The output column list is [mixedVectorSetOpOutputCols], not
//     [vectorSetOpOutputCols] — it carries the discriminator all the way
//     to the final SELECT (unlike `_setop_side` / `_setop_has_left`,
//     which are internal bookkeeping the outer SELECT drops), because
//     internal/chclient's cursor needs it on every row of the actual
//     result set to decide which of Sample.Value / Sample.Histogram is
//     the real answer.
func (e *emitter) emitMixedVectorSetOp(s *chplan.VectorSetOp) error {
	leftFrag, err := e.subqueryFrag(s.Left)
	if err != nil {
		return err
	}
	rightFrag, err := e.subqueryFrag(s.Right)
	if err != nil {
		return err
	}

	leftArm := mixedVectorSetOpArmFrag(s, s.Left, leftFrag, s.MixedHistogramOnLeft)
	rightArm := mixedVectorSetOpArmFrag(s, s.Right, rightFrag, !s.MixedHistogramOnLeft)

	sideArmL := mixedVectorSetOpSideArmFrag(s, leftArm, 0)
	sideArmR := mixedVectorSetOpSideArmFrag(s, rightArm, 1)
	windowed := NewQuery().
		Select(append(
			mixedVectorSetOpOutputCols(s),
			Col(setOpSideCol),
			As(
				Window(
					Call("max", Eq(Col(setOpSideCol), InlineLit(0))),
					setOpMatchKeyFrags(s.Match, s.AttributesColumn, s.TimestampColumn, s.StepAligned),
					nil,
				),
				setOpHasLeftCol,
			),
		)...).
		From(Paren(UnionAll(sideArmL, sideArmR)))
	outer := NewQuery().
		Select(mixedVectorSetOpOutputCols(s)...).
		From(windowed.Frag()).
		Where(Or(
			Eq(Col(setOpSideCol), InlineLit(0)),
			Eq(Col(setOpHasLeftCol), InlineLit(0)),
		))
	return e.emitSelect(outer)
}

// mixedVectorSetOpOutputCols returns the fourteen-column explicit
// projection list a Mixed VectorSetOr uses for both its per-arm and its
// outer SELECTs: the canonical quartet, the nine Histogram*Column
// outputs, and the trailing [setOpMixedIsHistogramCol] discriminator.
func mixedVectorSetOpOutputCols(s *chplan.VectorSetOp) []Frag {
	return append(
		append(vectorSetOpOutputCols(s), vectorSetOpHistogramCols()...),
		Col(setOpMixedIsHistogramCol),
	)
}

// mixedVectorSetOpArmFrag canonicalises one arm of a Mixed VectorSetOr to
// the shared fourteen-column shape [mixedVectorSetOpOutputCols] names.
// isHistogramArm selects which half is real and which is a placeholder:
// a histogram-shaped arm publishes its own nine structural columns for
// real and forwards Value unchanged (it is already the
// [HistogramProjection] placeholder, nothing more to do); a float-shaped
// arm publishes Value for real and nine typed placeholders standing in
// for the histogram columns it doesn't have.
func mixedVectorSetOpArmFrag(s *chplan.VectorSetOp, arm chplan.Node, armFrag Frag, isHistogramArm bool) Frag {
	cols := vectorSetOpCanonicalQuartetFrags(s, arm)
	discriminator := InlineLit(0)
	if isHistogramArm {
		cols = append(cols, vectorSetOpHistogramCols()...)
		discriminator = InlineLit(1)
	} else {
		cols = append(cols, mixedVectorSetOpHistogramPlaceholderCols()...)
	}
	cols = append(cols, As(discriminator, setOpMixedIsHistogramCol))
	inner := NewQuery().
		Select(cols...).
		From(armFrag)
	return inner.Frag()
}

// mixedVectorSetOpHistogramPlaceholderCols returns the nine
// Histogram*Column placeholders a Mixed VectorSetOr's float-shaped arm
// projects, typed to match [histogramFloatFrag] / [histogramIndexFrag] /
// [histogramBucketsFrag] exactly — the same Float64 / Int32 /
// Array(Float64) triple [vectorSetOpHistogramCols] forwards for real
// from the histogram-shaped arm — so the UNION ALL's positional column
// unification sees identical types on both arms instead of failing with
// NO_COMMON_TYPE. The discriminator (appended by the caller, not here)
// is what marks these unread on decode, mirroring
// histogramSampleValuePlaceholder's identical convention for the
// histogram-shaped arm's own placeholder Value.
func mixedVectorSetOpHistogramPlaceholderCols() []Frag {
	zeroFloat := Call("toFloat64", InlineLit(0))
	zeroInt := Call("toInt32", InlineLit(0))
	emptyBuckets := Call("emptyArrayFloat64")
	return []Frag{
		As(zeroFloat, chplan.HistogramCountColumn),
		As(zeroFloat, chplan.HistogramSumColumn),
		As(zeroInt, chplan.HistogramScaleColumn),
		As(zeroFloat, chplan.HistogramZeroThresholdColumn),
		As(zeroFloat, chplan.HistogramZeroCountColumn),
		As(zeroInt, chplan.HistogramPositiveOffsetColumn),
		As(emptyBuckets, chplan.HistogramPositiveBucketCountsColumn),
		As(zeroInt, chplan.HistogramNegativeOffsetColumn),
		As(emptyBuckets, chplan.HistogramNegativeBucketCountsColumn),
	}
}

// mixedVectorSetOpSideArmFrag is [vectorSetOpSideArmFrag]'s Mixed
// sibling: it wraps an already fourteen-column-canonicalised arm Frag in
// a `SELECT <mixed cols>, <side> AS _setop_side FROM (<arm>)` so the
// VectorSetOr single-pass UNION ALL carries the side marker as a 15th
// column, the same role it plays in the symmetric emission.
func mixedVectorSetOpSideArmFrag(s *chplan.VectorSetOp, armFrag Frag, side int) Frag {
	q := NewQuery().
		Select(append(
			mixedVectorSetOpOutputCols(s),
			As(InlineLit(side), setOpSideCol),
		)...).
		From(armFrag)
	return Paren(q.Frag())
}

// vectorSetOpCanonicalArmFrag returns a Frag rendering the per-arm
// canonical 4-column projection used inside the VectorSetOp UNION ALL /
// IN-subquery shapes. The arm's chplan node is inspected to decide
// which of the four canonical columns is available as a real column
// from the rendered subquery and which must be synthesised.
//
// NOTE — still reachable post-#710 / #716 (audited 2026-05-22).
// PR #710 fixed schema.Metrics.TableFor → TablesFor so unsuffixed
// OTel-emitter metric names route to merge(gauge|sum) instead of
// gauge-only; #716 extended that to _count / _sum suffixes. Both
// changes touch which Scan tables the matcher resolves and are
// orthogonal to this helper, which addresses two distinct SQL
// emission issues:
//
//  1. UNION ALL positional column-type unification: nested
//     `(A or B) or C` mixes a canonical-shape inner (String, Map,
//     DateTime64, Float64) with a matrix-mode RangeWindow arm whose
//     inner SELECT exposes (Attributes, anchor_ts, TimeUnix, Value)
//     — column 0 is Map there, String on the inner. CH then refuses
//     with NO_COMMON_TYPE (Map vs. String).
//  2. Instant-mode derived inner SELECTs only project (group-keys…,
//     Value); a parent SELECT that references TimeUnix unqualified
//     fails at CH 24.x with "Unknown expression identifier
//     'TimeUnix'".
//
// Reachability was empirically confirmed by lowering the PR #706
// failing-shape query
// `sum(rate(otelcol_exporter_send_failed_log_records[5m]) or
//
//	rate(otelcol_exporter_send_failed_metric_points[5m]))` on the
//
// post-#710/#716 tree: the per-arm canonical-shape projection is
// invoked for both arms, with `derived` true and the synthesised
// anchor branch taken (instant mode, no matrix RangeWindow). The
// pinned regression fixtures
// `test/spec/promql/binary_or_increase_{range,instant}_canonicalises_arms.txtar`
// continue to gate the SQL shape this helper produces.
//
// Three resolution shapes need handling, mirroring the three branches
// in `wrapWithSampleProjection` (internal/api/prom/handler.go):
//
//  1. Canonical-shape input (Scan / Filter(Scan) / a Project that names
//     all four canonical columns) — every column passes through by name.
//  2. Matrix-mode RangeWindow (OuterRange > 0) — the inner SELECT
//     projects `anchor_ts AS <TimestampColumn>` at emit time (see
//     emitWindowedArrayPairsMatrix + emitWindowedArrayMatrix), so
//     TimeUnix resolves under the canonical alias. MetricName isn't in
//     scope and is synthesised as the empty string.
//  3. Instant-mode derived-shape input (instant RangeWindow / Aggregate /
//     MetricsAggregate / MetricsHistogramOverTime / a Project on top of
//     one of those) — the inner SELECT projects only
//     `(group-keys…, <ValueColumn>)`. Both MetricName and TimeUnix are
//     out of scope. MetricName is synthesised as the empty string and
//     TimeUnix as `now64(9) - toIntervalNanosecond(5000000000)` — the
//     same synthetic anchor `wrapWithSampleProjection`'s instant branch
//     stamps on derived-shape rows for instant-mode query bucketing.
//
// Without the instant-mode TimeUnix synthesis CH 24.x rejects the inner
// SELECT with "Unknown expression identifier 'TimeUnix'" / "Resolve
// identifier 'TimeUnix' from parent scope only supported for constants
// and CTE" (the parent's column-ref tries to bind to the outer SELECT's
// canonical projection and fails because the derived inner doesn't
// expose it).
//
// The Attributes / Value columns are always referenced by name (every
// derived emitter still passes Attributes + the schema-named
// ValueColumn through).
func vectorSetOpCanonicalArmFrag(s *chplan.VectorSetOp, arm chplan.Node, armFrag Frag) Frag {
	cols := vectorSetOpCanonicalQuartetFrags(s, arm)
	// Widen by THIS ARM's own row shape, not s.Histogram: for a
	// homogeneous set op (both arms histogram, or both float) the two
	// answers agree, but a MIXED `and`/`unless` (cerberus issue #2325 —
	// one operand already reduced to a float, the other a raw histogram
	// selector) has s.Histogram reporting only the OUTPUT arm's shape
	// (see chplan.VectorSetOp.Histogram / internal/promql's
	// lowerVectorSetOp). The other arm is canonicalised here purely to
	// supply setOpInSubqueryFrag's label-signature membership test — it
	// is never a source of output columns — so it must be widened (or
	// not) by what its OWN SELECT actually publishes, or referencing a
	// Histogram*Column the arm never projects (float side) / omitting
	// one it does (histogram side, harmlessly unused downstream) would
	// desync from reality. In the homogeneous cases this is
	// byte-identical to the old s.Histogram check.
	if chplan.RowShapeOf(arm) == chplan.HistogramRowShape {
		cols = append(cols, vectorSetOpHistogramCols()...)
	}
	inner := NewQuery().
		Select(cols...).
		From(armFrag)
	return inner.Frag()
}

// vectorSetOpCanonicalQuartetFrags returns the four canonical-shape Frags
// (MetricName, Attributes, TimeUnix, Value) for arm, synthesising
// whichever of MetricName / TimeUnix arm's own SELECT doesn't expose —
// see [vectorSetOpCanonicalArmFrag]'s doc comment for the three
// resolution shapes this covers. Split out so
// [mixedVectorSetOpArmFrag] (cerberus issue #2330) can reuse the exact
// same derived/matrix-shape resolution the symmetric Histogram /
// float-only paths already use, instead of re-deriving it.
func vectorSetOpCanonicalQuartetFrags(s *chplan.VectorSetOp, arm chplan.Node) []Frag {
	derived := chplan.IsDerivedShape(arm, vectorSetOpSampleColumns(s))
	armTsCol, matrix := vectorSetOpArmTimestampCol(arm, s)

	var metricNameFrag Frag
	if derived {
		metricNameFrag = As(Lit(""), s.MetricNameColumn)
	} else {
		metricNameFrag = Col(s.MetricNameColumn)
	}

	var timeFrag Frag
	switch {
	case derived && !matrix:
		timeFrag = As(vectorSetOpSynthesizedAnchorFrag(), s.TimestampColumn)
	case matrix && armTsCol != s.TimestampColumn:
		timeFrag = As(Col(armTsCol), s.TimestampColumn)
	default:
		timeFrag = Col(s.TimestampColumn)
	}

	return []Frag{
		metricNameFrag,
		Col(s.AttributesColumn),
		timeFrag,
		Col(s.ValueColumn),
	}
}

// vectorSetOpHistogramCols returns the nine chplan.Histogram*Column
// references a histogram-shaped set op (VectorSetOp.Histogram /
// NaryVectorSetOp.Histogram) appends after the canonical quartet. Every
// arm feeding such a set op is, by construction, a
// [chplan.HistogramProjection] (see internal/promql's
// lowerExpHistogramSetOp), whose own emitter always aliases its nine
// structural outputs to these FIXED names — see
// internal/chsql/histogram_projection.go — regardless of the physical
// schema column names the underlying scan used. Referencing the fixed
// aliases here therefore needs no schema plumbing of its own.
func vectorSetOpHistogramCols() []Frag {
	return []Frag{
		Col(chplan.HistogramCountColumn),
		Col(chplan.HistogramSumColumn),
		Col(chplan.HistogramScaleColumn),
		Col(chplan.HistogramZeroThresholdColumn),
		Col(chplan.HistogramZeroCountColumn),
		Col(chplan.HistogramPositiveOffsetColumn),
		Col(chplan.HistogramPositiveBucketCountsColumn),
		Col(chplan.HistogramNegativeOffsetColumn),
		Col(chplan.HistogramNegativeBucketCountsColumn),
	}
}

// vectorSetOpSideArmFrag wraps an already-canonicalised arm Frag in a
// `SELECT MetricName, Attributes, TimeUnix, Value, <side> AS _setop_side
// FROM (<arm>)` so the VectorSetOr single-pass UNION ALL carries the
// side marker as a 5th column. The marker is an inline shape constant
// (0 = LHS, 1 = RHS), not user data, so InlineLit is correct. Selecting
// the four canonical columns by name keeps positional UNION-ALL
// unification stable across the two arms (the marker is always the last
// position on both sides).
func vectorSetOpSideArmFrag(s *chplan.VectorSetOp, armFrag Frag, side int) Frag {
	q := NewQuery().
		Select(append(
			vectorSetOpOutputCols(s),
			As(InlineLit(side), setOpSideCol),
		)...).
		From(armFrag)
	return Paren(q.Frag())
}

// vectorSetOpSynthesizedAnchorFrag renders the synthetic anchor
// timestamp cerberus stamps on instant-mode derived-shape arms for the
// VectorSetOp canonical-shape projection: `now64(9) -
// toIntervalNanosecond(5000000000)`, i.e. 5 seconds before CH-now.
// Mirrors `synthesizedAnchor()` in internal/api/prom/handler.go so the
// in-line and per-arm canonical-shape projections produce identical
// TimeUnix values on instant-mode rows.
func vectorSetOpSynthesizedAnchorFrag() Frag {
	return func(b *Builder) {
		b.SubtractNanos(func(b *Builder) { b.Now64() }, 5_000_000_000)
	}
}

// vectorSetOpArmTimestampCol reports whether arm is — after walking past
// any value-rewrite Project / Filter — a matrix-mode range arm
// (RangeWindow with OuterRange > 0, or the ClickHouse-native
// timeSeriesRateToGrid RangeWindowGridNative), and if so the column name its
// outer SELECT surfaces the per-row grid anchor under. Mirrors
// `isMatrixRangeWindow` in internal/api/prom/handler.go.
//
// Both matrix emitters alias the anchor to the node's own
// TimestampColumn — `anchor_ts AS <TimestampColumn>` (see
// emitWindowedArrayPairsMatrix / emitWindowedArrayMatrix and
// emitRangeWindowGridNative) — and skip the alias when TimestampColumn is
// already `anchor_ts`, which is what a subquery-fed arm carries: its
// input is itself a grid, so the timestamp it reduces over is the inner
// grid's anchor. The arm's timestamp therefore lives under the NODE's
// TimestampColumn, not the set op's, and the canonical-shape projection
// above aliases across when the two differ. Reading the node's column
// rather than assuming the set op's is what keeps a subquery arm
// (`a or max_over_time(b[3m:1m])`) from projecting an identifier its
// own SELECT never exposes.
//
// The native path is always matrix shape: it explodes the grid into one
// row per anchor and surfaces the per-row anchor column, exactly like
// the fan-out matrix RangeWindow. The timestamp source is therefore that
// column, not the now64() instant synthesis.
//
// The walk stops at a Project that already exposes the canonical
// 4-column shape, because that Project IS the arm's outermost SELECT and
// it has already re-aliased the matrix anchor to the set op's
// TimestampColumn. Walking past it would report the RangeWindow's own
// column name, which the canonical Project's scope no longer exposes —
// the LogQL heads' `Timestamp` over a scope that surfaces
// `anchor_ts AS TimeUnix`, which ClickHouse rejects with code 47
// "Unknown expression identifier". The two shape classifiers must agree
// on where an arm's outer scope begins, so this one stops exactly where
// chplan.IsDerivedShape does.
//
// Nested VectorSetOp arms are NOT matrix shape per se — but they emit
// their own canonical 4-column SELECT (this very function's caller),
// so the outer references TimeUnix by name regardless. They're
// classified as canonical (not derived) by
// chplan.IsDerivedShape, so this helper isn't reached for them.
func vectorSetOpArmTimestampCol(n chplan.Node, s *chplan.VectorSetOp) (string, bool) {
	switch v := n.(type) {
	case *chplan.RangeWindow:
		if v.OuterRange > 0 {
			return v.TimestampColumn, true
		}
	case *chplan.RangeWindowGridNative:
		return v.TimestampColumn, true
	case *chplan.Project:
		if chplan.ProjectExposesCanonical(v, vectorSetOpSampleColumns(s)) {
			return s.TimestampColumn, true
		}
		return vectorSetOpArmTimestampCol(v.Input, s)
	case *chplan.Filter:
		return vectorSetOpArmTimestampCol(v.Input, s)
	}
	return "", false
}

// vectorSetOpSampleColumns names the canonical four-column sample shape
// this set op's arms are canonicalised into, in the form the shared
// chplan classifiers take. The set op carries the schema-configured
// names on its own fields, so this is the one place the emitter converts
// between the two spellings of the same four names.
func vectorSetOpSampleColumns(s *chplan.VectorSetOp) chplan.SampleColumns {
	return chplan.SampleColumns{
		MetricName: s.MetricNameColumn,
		Attributes: s.AttributesColumn,
		Timestamp:  s.TimestampColumn,
		Value:      s.ValueColumn,
	}
}

// vectorSetOpOutputCols returns the explicit projection list a vector
// set op uses for its outer SELECT. Rendering MetricName / Attributes /
// TimeUnix / Value explicitly (vs. the implicit `SELECT *`) lets the
// spec round-trip runner recognise the Map column and wrap it in
// `toJSONString(...)` before handing the SQL to chDB's parquet driver,
// which otherwise panics on Map types. The order matches the canonical
// 4-slot shape every other PromQL emit site pins (vector_join, project,
// range_window, …).
func vectorSetOpOutputCols(s *chplan.VectorSetOp) []Frag {
	cols := []Frag{
		Col(s.MetricNameColumn),
		Col(s.AttributesColumn),
		Col(s.TimestampColumn),
		Col(s.ValueColumn),
	}
	if s.Histogram {
		cols = append(cols, vectorSetOpHistogramCols()...)
	}
	return cols
}

func (e *emitter) validateVectorSetOpCols(s *chplan.VectorSetOp) error {
	switch {
	case s.AttributesColumn == "":
		return fmt.Errorf("%w: VectorSetOp.AttributesColumn unset", ErrUnsupported)
	case s.MetricNameColumn == "":
		return fmt.Errorf("%w: VectorSetOp.MetricNameColumn unset", ErrUnsupported)
	case s.TimestampColumn == "":
		return fmt.Errorf("%w: VectorSetOp.TimestampColumn unset", ErrUnsupported)
	case s.ValueColumn == "":
		return fmt.Errorf("%w: VectorSetOp.ValueColumn unset", ErrUnsupported)
	case s.Mixed && s.Histogram:
		return fmt.Errorf("%w: VectorSetOp.Mixed and .Histogram are mutually exclusive", ErrUnsupported)
	case s.Mixed && s.Op != chplan.VectorSetOr:
		return fmt.Errorf("%w: VectorSetOp.Mixed is only supported for %q, got %q", ErrUnsupported, chplan.VectorSetOr, s.Op)
	}
	return nil
}

// setOpInSubqueryFrag builds the WHERE predicate for an `IN` or `NOT IN`
// against a DISTINCT match-key subquery over the other side.
//
//	<sig> [NOT] IN (SELECT DISTINCT <sig> FROM <subquery>)                  — instant
//	(<sig>, TimeUnix) [NOT] IN (SELECT DISTINCT <sig>, TimeUnix FROM <sub>) — range
//
// <sig> is the label-signature expression — for default matching it's
// the bare Attributes column; for `on(labels)` / `ignoring(labels)` it's
// the corresponding mapFilter projection. Reusing the existing
// matchKeyGroupExpr helper means the signature shape stays in sync
// with VectorJoin (so the optimizer's match-key recognition keeps
// working across both V-V binop families).
//
// In range mode the key carries the evaluation timestamp alongside the
// signature, because PromQL matches set-op arms once per evaluation
// timestamp. `sub` is therefore the CANONICALISED other arm — the
// 4-column projection that structurally guarantees a TimestampColumn —
// not the raw child, whose derived / matrix shapes need not expose one.
//
// DISTINCT keeps the IN-list small when the other side has many rows
// per key; set ops are inherently many-to-many on labels so a
// per-series subquery would otherwise emit one row per LHS+RHS series.
// `Distinct` renders on the first projection only, which is CH's
// row-level DISTINCT over the whole SELECT list — exactly the
// per-(signature, timestamp) key set the range shape needs.
func setOpInSubqueryFrag(s *chplan.VectorSetOp, sub Frag, in bool) Frag {
	keys := setOpMatchKeyFrags(s.Match, s.AttributesColumn, s.TimestampColumn, s.StepAligned)
	var sig Frag
	if len(keys) == 1 {
		sig = keys[0]
	} else {
		sig = Tuple(keys...)
	}
	inner := NewQuery().
		Select(append([]Frag{Distinct(keys[0])}, keys[1:]...)...).
		From(sub)
	// inner.Frag() already wraps the SELECT in parens; In / NotInSubquery
	// each add the outer membership parens, giving the existing
	// `<key> [NOT] IN ((SELECT DISTINCT … FROM …))` byte shape.
	if in {
		return In(sig, inner.Frag())
	}
	return NotInSubquery(sig, inner.Frag())
}
