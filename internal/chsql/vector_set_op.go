package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
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
		// SELECT MetricName, Attributes, TimeUnix, Value FROM (
		//   (SELECT MetricName, Attributes, TimeUnix, Value FROM (<A>))
		//   UNION ALL
		//   (SELECT MetricName, Attributes, TimeUnix, Value FROM (<B>)
		//      WHERE <sig> NOT IN (SELECT DISTINCT <sig> FROM (<A>)))
		// )
		//
		// The UNION ALL must be wrapped in an extra outer paren so the
		// outer SELECT's FROM clause sees a single subquery; otherwise
		// the rendered SQL is `… FROM (<left>) UNION ALL (<right>)`,
		// which parses as a top-level UNION between two whole SELECTs
		// (and CH then asks for a supertype between the outer SELECT's
		// projection and the right UNION arm — Map vs. String — and
		// fails with NO_COMMON_TYPE).
		//
		// Each arm projects the canonical 4-column shape via
		// vectorSetOpCanonicalArmFrag so positional column unification
		// across arms never hits the String-vs-Map supertype error even
		// when one arm is a derived-shape RangeWindow / Aggregate that
		// drops `__name__`.
		leftSelect := NewQuery().
			Select(vectorSetOpOutputCols(s)...).
			From(leftArm)
		rightSelect := NewQuery().
			Select(vectorSetOpOutputCols(s)...).
			From(rightArm).
			Where(setOpInSubqueryFrag(s.Match, s.AttributesColumn, leftFrag, false /*notIn*/))
		outer := NewQuery().
			Select(vectorSetOpOutputCols(s)...).
			From(Paren(UnionAll(leftSelect.Frag(), rightSelect.Frag())))
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
	return func(b *Builder) {
		matchKeyGroupExprFrag(m, attrsCol)(b)
		if in {
			b.writeSQL(" IN (")
		} else {
			b.writeSQL(" NOT IN (")
		}
		inner := NewQuery().
			Select(Distinct(matchKeyGroupExprFrag(m, attrsCol))).
			From(sub)
		inner.Frag()(b)
		b.writeSQL(")")
	}
}
