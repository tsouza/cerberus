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
)

// emitVectorSetOp renders a PromQL vector set operator (`and`, `or`,
// `unless`) over the two child plans. The shape depends on the kind:
//
//   - VectorSetAnd (`A and B`): semi-join — keep LHS rows whose match-
//     key signature appears in B. Rendered as
//     `SELECT MetricName, Attributes, TimeUnix, Value FROM (SELECT * FROM (<A>) WHERE <sig> IN (SELECT DISTINCT <sig> FROM (<B>)))`.
//
//   - VectorSetUnless (`A unless B`): anti-join — keep LHS rows whose
//     match-key signature does NOT appear in B. Rendered as
//     `SELECT MetricName, Attributes, TimeUnix, Value FROM (SELECT * FROM (<A>) WHERE <sig> NOT IN (SELECT DISTINCT <sig> FROM (<B>)))`.
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
// The match-key signature is the full Attributes column (default), or
// the projected mapFilter expression for `on(...)` / `ignoring(...)`.
// The IN-subquery uses `SELECT DISTINCT` so a many-rows-per-signature
// RHS doesn't blow up the IN list — set ops are inherently many-to-
// many on labels (PromQL parser rejects `group_left` / `group_right`
// here), so the DISTINCT is both semantically correct and small.
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
			Where(setOpInSubqueryFrag(s.Match, s.AttributesColumn, rightFrag, true /*in*/))
		outer := NewQuery().
			Select(vectorSetOpOutputCols(s)...).
			From(inner.Frag())
		e.emitSelect(outer)
		return nil
	case chplan.VectorSetUnless:
		// SELECT MetricName, Attributes, TimeUnix, Value
		//   FROM (SELECT MetricName, Attributes, TimeUnix, Value FROM (<A>) WHERE <sig> NOT IN (SELECT DISTINCT <sig> FROM (<B>)))
		inner := NewQuery().
			Select(vectorSetOpOutputCols(s)...).
			From(leftArm).
			Where(setOpInSubqueryFrag(s.Match, s.AttributesColumn, rightFrag, false /*notIn*/))
		outer := NewQuery().
			Select(vectorSetOpOutputCols(s)...).
			From(inner.Frag())
		e.emitSelect(outer)
		return nil
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
						[]Frag{matchKeyGroupExprFrag(s.Match, s.AttributesColumn)},
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
		e.emitSelect(outer)
		return nil
	}
	return fmt.Errorf("%w: vector set op %q", ErrUnsupported, s.Op)
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
	derived := vectorSetOpArmIsDerivedShape(arm, s)
	matrix := vectorSetOpArmIsMatrixRangeWindow(arm)

	var metricNameFrag Frag
	if derived {
		metricNameFrag = As(Lit(""), s.MetricNameColumn)
	} else {
		metricNameFrag = Col(s.MetricNameColumn)
	}

	var timeFrag Frag
	if derived && !matrix {
		timeFrag = As(vectorSetOpSynthesizedAnchorFrag(), s.TimestampColumn)
	} else {
		timeFrag = Col(s.TimestampColumn)
	}

	inner := NewQuery().
		Select(
			metricNameFrag,
			Col(s.AttributesColumn),
			timeFrag,
			Col(s.ValueColumn),
		).
		From(armFrag)
	return inner.Frag()
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

// vectorSetOpArmIsMatrixRangeWindow reports whether arm is — after
// walking past any value-rewrite Project / Filter — a matrix-mode
// RangeWindow (OuterRange > 0). Matrix-mode RangeWindow's outer SELECT
// aliases `anchor_ts AS <TimestampColumn>` (see
// emitWindowedArrayPairsMatrix and emitWindowedArrayMatrix), so a
// canonical-shape projection above it can reference TimeUnix as a real
// column. Mirrors `isMatrixRangeWindow` in
// internal/api/prom/handler.go.
//
// Nested VectorSetOp arms are NOT matrix shape per se — but they emit
// their own canonical 4-column SELECT (this very function's caller),
// so the outer references TimeUnix by name regardless. They're
// classified as canonical (not derived) by
// vectorSetOpArmIsDerivedShape, so this helper isn't reached for them.
func vectorSetOpArmIsMatrixRangeWindow(n chplan.Node) bool {
	switch v := n.(type) {
	case *chplan.RangeWindow:
		return v.OuterRange > 0
	case *chplan.Project:
		return vectorSetOpArmIsMatrixRangeWindow(v.Input)
	case *chplan.Filter:
		return vectorSetOpArmIsMatrixRangeWindow(v.Input)
	}
	return false
}

// vectorSetOpArmIsDerivedShape reports whether a VectorSetOp arm's
// chplan output schema lacks the canonical MetricName column. Mirrors
// `internal/api/prom/handler.go::isDerivedShape` but lives in the
// chsql package so the emitter can decide per-arm without taking a
// dependency on the HTTP-layer helper. The two functions must stay in
// sync; both treat RangeWindow / Aggregate / MetricsAggregate /
// MetricsHistogramOverTime — and a Project that does NOT expose all
// four canonical columns above one of those — as derived.
//
// Nested VectorSetOp arms are canonical: the recursive emit wraps each
// inner VectorSetOp in its own canonical-column SELECT, so a parent
// arm can reference MetricName by name.
func vectorSetOpArmIsDerivedShape(n chplan.Node, s *chplan.VectorSetOp) bool {
	switch v := n.(type) {
	case *chplan.RangeWindow,
		*chplan.Aggregate,
		*chplan.MetricsAggregate,
		*chplan.MetricsHistogramOverTime:
		return true
	case *chplan.Filter:
		return vectorSetOpArmIsDerivedShape(v.Input, s)
	case *chplan.Project:
		if vectorSetOpProjectExposesCanonical(v, s) {
			return false
		}
		return vectorSetOpArmIsDerivedShape(v.Input, s)
	}
	return false
}

// vectorSetOpProjectExposesCanonical reports whether p's projections
// name all four canonical Sample column outputs (MetricName /
// Attributes / TimeUnix / Value). Mirrors
// `internal/api/prom/handler.go::projectionExposesCanonical`; see that
// docstring for the full canonical-shape definition. An output is
// "named" when either Projection.Alias matches, or the Projection.Expr
// is a bare ColumnRef to the canonical column name with no Alias
// rewrite.
func vectorSetOpProjectExposesCanonical(p *chplan.Project, s *chplan.VectorSetOp) bool {
	needed := map[string]bool{
		s.MetricNameColumn: false,
		s.AttributesColumn: false,
		s.TimestampColumn:  false,
		s.ValueColumn:      false,
	}
	for _, proj := range p.Projections {
		name := vectorSetOpProjectionOutputName(proj)
		if _, ok := needed[name]; ok {
			needed[name] = true
		}
	}
	for _, ok := range needed {
		if !ok {
			return false
		}
	}
	return true
}

// vectorSetOpProjectionOutputName returns the column name a Projection
// exposes: the explicit Alias when set, otherwise the bare-ColumnRef
// name when the Expr is a column reference. Mirrors
// `internal/api/prom/handler.go::projectionOutputName`.
func vectorSetOpProjectionOutputName(p chplan.Projection) string {
	if p.Alias != "" {
		return p.Alias
	}
	if cr, ok := p.Expr.(*chplan.ColumnRef); ok {
		return cr.Name
	}
	return ""
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
	return []Frag{
		Col(s.MetricNameColumn),
		Col(s.AttributesColumn),
		Col(s.TimestampColumn),
		Col(s.ValueColumn),
	}
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
	}
	return nil
}

// setOpInSubqueryFrag builds the WHERE predicate for an `IN` or `NOT IN`
// against a DISTINCT signature subquery over the other side.
//
//	<sig> [NOT] IN (SELECT DISTINCT <sig> FROM <subquery>)
//
// <sig> is the match-key expression — for default matching it's the
// bare Attributes column; for `on(labels)` / `ignoring(labels)` it's
// the corresponding mapFilter projection. Reusing the existing
// matchKeyGroupExpr helper means the signature shape stays in sync
// with VectorJoin (so the optimizer's match-key recognition keeps
// working across both V-V binop families).
//
// DISTINCT keeps the IN-list small when the other side has many rows
// per signature; set ops are inherently many-to-many on labels so a
// per-series subquery would otherwise emit one row per LHS+RHS series.
func setOpInSubqueryFrag(m chplan.VectorMatch, attrsCol string, sub Frag, in bool) Frag {
	sig := matchKeyGroupExprFrag(m, attrsCol)
	inner := NewQuery().
		Select(Distinct(matchKeyGroupExprFrag(m, attrsCol))).
		From(sub)
	// inner.Frag() already wraps the SELECT in parens; In / NotInSubquery
	// each add the outer membership parens, giving the existing
	// `<sig> [NOT] IN ((SELECT DISTINCT … FROM …))` byte shape.
	if in {
		return In(sig, inner.Frag())
	}
	return NotInSubquery(sig, inner.Frag())
}
