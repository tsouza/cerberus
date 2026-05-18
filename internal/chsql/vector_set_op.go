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

	switch s.Op {
	case chplan.VectorSetAnd:
		// SELECT MetricName, Attributes, TimeUnix, Value
		//   FROM (SELECT * FROM (<A>) WHERE <sig> IN (SELECT DISTINCT <sig> FROM (<B>)))
		inner := NewQuery().
			From(leftFrag).
			Where(setOpInSubqueryFrag(s.Match, s.AttributesColumn, rightFrag, true /*in*/))
		outer := NewQuery().
			Select(vectorSetOpOutputCols(s)...).
			From(inner.Frag())
		e.emitSelect(outer)
		return nil
	case chplan.VectorSetUnless:
		// SELECT MetricName, Attributes, TimeUnix, Value
		//   FROM (SELECT * FROM (<A>) WHERE <sig> NOT IN (SELECT DISTINCT <sig> FROM (<B>)))
		inner := NewQuery().
			From(leftFrag).
			Where(setOpInSubqueryFrag(s.Match, s.AttributesColumn, rightFrag, false /*notIn*/))
		outer := NewQuery().
			Select(vectorSetOpOutputCols(s)...).
			From(inner.Frag())
		e.emitSelect(outer)
		return nil
	case chplan.VectorSetOr:
		// SELECT MetricName, Attributes, TimeUnix, Value FROM (
		//   (SELECT * FROM (<A>))
		//   UNION ALL
		//   (SELECT * FROM (<B>)
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
		leftSelect := NewQuery().From(leftFrag)
		rightSelect := NewQuery().
			From(rightFrag).
			Where(setOpInSubqueryFrag(s.Match, s.AttributesColumn, leftFrag, false /*notIn*/))
		outer := NewQuery().
			Select(vectorSetOpOutputCols(s)...).
			From(Paren(UnionAll(leftSelect.Frag(), rightSelect.Frag())))
		e.emitSelect(outer)
		return nil
	}
	return fmt.Errorf("%w: vector set op %q", ErrUnsupported, s.Op)
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
