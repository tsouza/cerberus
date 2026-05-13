package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// emitSetOperation renders a TraceQL spanset set-op (`A && B`, `A || B`).
//
//   - SetIntersect (`&&`): INNER JOIN of the two subqueries on
//     (TraceID, SpanID); the result projects the left side's columns
//     (TraceQL convention: the row identity comes from the left
//     spanset).
//   - SetUnion     (`||`): `(<left>) UNION DISTINCT (<right>)` — CH
//     UNION DISTINCT collapses identical rows across the two arms.
//
// Both shapes flow through QueryBuilder so the typed slot lifecycle
// (FROM source, WHERE predicates, etc.) stays intact. UNION is a
// SELECT-level binary operator, not a clause keyword inside a single
// SELECT, so the `" UNION DISTINCT "` token sits between two
// pre-rendered QueryBuilder Frags rather than abusing a clause slot.
func (e *emitter) emitSetOperation(s *chplan.SetOperation) error {
	if s.TraceIDColumn == "" || s.SpanIDColumn == "" {
		return fmt.Errorf("%w: SetOperation column names unset", ErrUnsupported)
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
	case chplan.SetIntersect:
		// SELECT L.* FROM (<left>) AS L INNER JOIN (<right>) AS R
		//   ON L.TraceId = R.TraceId AND L.SpanId = R.SpanId
		traceID := s.TraceIDColumn
		spanID := s.SpanIDColumn
		from := func(b *Builder) {
			leftFrag(b)
			b.WriteSQL(" AS L INNER JOIN ")
			rightFrag(b)
			b.WriteSQL(" AS R ON ")
			b.QualIdent("L", traceID)
			b.WriteSQL(" = ")
			b.QualIdent("R", traceID)
			b.WriteSQL(" AND ")
			b.QualIdent("L", spanID)
			b.WriteSQL(" = ")
			b.QualIdent("R", spanID)
		}
		sb := NewQuery().
			Select(Raw("L.*")).
			From(from)
		e.emitSelect(sb)
		return nil
	case chplan.SetUnion:
		// (<left>) UNION DISTINCT (<right>). CH's UNION DISTINCT
		// dedupes on the full row tuple, which matches TraceQL's
		// "spans appearing on either side" semantics for spans drawn
		// from the same underlying table. Each subquery Frag is
		// already a `(SELECT ...)` parenthesised form, so the
		// rendered output is well-formed CH UNION.
		b := NewBuilder()
		leftFrag(b)
		b.WriteSQL(" UNION DISTINCT ")
		rightFrag(b)
		e.splice(b)
		return nil
	}
	return fmt.Errorf("%w: set op %q", ErrUnsupported, s.Op)
}
