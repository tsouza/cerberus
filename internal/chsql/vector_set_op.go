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
//     `SELECT * FROM (<A>) WHERE <sig> IN (SELECT DISTINCT <sig> FROM (<B>))`.
//
//   - VectorSetUnless (`A unless B`): anti-join — keep LHS rows whose
//     match-key signature does NOT appear in B. Rendered as
//     `SELECT * FROM (<A>) WHERE <sig> NOT IN (SELECT DISTINCT <sig> FROM (<B>))`.
//
//   - VectorSetOr (`A or B`): left + anti-right — every LHS row plus
//     every RHS row whose match-key signature does NOT appear in A.
//     Rendered as
//     `(SELECT * FROM (<A>)) UNION ALL (SELECT * FROM (<B>) WHERE <sig> NOT IN (SELECT DISTINCT <sig> FROM (<A>)))`.
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
		// SELECT * FROM (<A>) WHERE <sig> IN (SELECT DISTINCT <sig> FROM (<B>))
		sb := NewQuery().
			From(leftFrag).
			Where(setOpInSubqueryFrag(s.Match, s.AttributesColumn, rightFrag, true /*in*/))
		e.emitSelect(sb)
		return nil
	case chplan.VectorSetUnless:
		// SELECT * FROM (<A>) WHERE <sig> NOT IN (SELECT DISTINCT <sig> FROM (<B>))
		sb := NewQuery().
			From(leftFrag).
			Where(setOpInSubqueryFrag(s.Match, s.AttributesColumn, rightFrag, false /*notIn*/))
		e.emitSelect(sb)
		return nil
	case chplan.VectorSetOr:
		// (SELECT * FROM (<A>)) UNION ALL (SELECT * FROM (<B>) WHERE <sig> NOT IN (SELECT DISTINCT <sig> FROM (<A>)))
		leftSelect := NewQuery().From(leftFrag)
		rightSelect := NewQuery().
			From(rightFrag).
			Where(setOpInSubqueryFrag(s.Match, s.AttributesColumn, leftFrag, false /*notIn*/))
		b := NewBuilder()
		UnionAll(leftSelect.Frag(), rightSelect.Frag())(b)
		e.splice(b)
		return nil
	}
	return fmt.Errorf("%w: vector set op %q", ErrUnsupported, s.Op)
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
