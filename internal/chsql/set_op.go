package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// unionDedupLimitPerIdentity is the `LIMIT <n> BY (TraceId, SpanId)`
// count for the SetUnion identity dedup: keep exactly ONE row per
// distinct span identity. The value 1 *is* the dedup — `LIMIT 1 BY`
// returns the first row of each (TraceId, SpanId) group with no global
// row cap, which is exactly TraceQL's "a span appears at most once in
// the union" semantics. Named so the literal isn't a bare magic 1.
const unionDedupLimitPerIdentity = 1

// emitSetOperation renders a TraceQL spanset set-op (`A && B`, `A || B`).
//
//   - SetIntersect (`&&`): INNER JOIN of the two subqueries on the
//     identity key; the result projects the left side's columns
//     (TraceQL convention: the row identity comes from the left
//     spanset).
//
// The identity key is (TraceID, SpanID) whenever both arms carry span
// identity, and TraceID alone when an arm has been folded to trace
// granularity — a parenthesised sub-pipeline ending in an aggregate, e.g.
// `({ … } | count() > 0) && ({ … } | count() > 0)`, groups on TraceID and
// exposes no SpanID column. The lowerer decides which applies and leaves
// SpanIDColumn empty for the trace-level case (see
// internal/traceql.spansetArmExposesSpanID).
//   - SetUnion     (`||`): `SELECT * FROM (<left> UNION ALL <right>)
//     LIMIT 1 BY (TraceID, SpanID)` — dedup on SPAN IDENTITY, not the
//     full row tuple. Both arms read the same spans table, so a span
//     surfaced by both predicates is byte-identical; deduping on
//     (TraceID, SpanID) is therefore result-identical to a full-row
//     UNION DISTINCT but streams cheaply via CH's `LIMIT n BY` instead
//     of building a hash set keyed on every (wide) column — including
//     the nested Events.* / Links.* array columns the OTel-CH schema
//     carries. The full-row UNION DISTINCT was the root cause of the
//     showcase-traceql "Spanset ||" panel never loading at
//     self-telemetry scale (~150k+ spans): the dedup hash over wide
//     rows with array columns is O(rows × row-width) memory; identity
//     dedup is O(rows × 2 ids). It also sits at the top of the
//     Drilldown structure-tab `(... &>>) || (...)` query, so the same
//     wide-row tax is lifted off that path's outer union.
//
// Both shapes flow through QueryBuilder so the typed slot lifecycle
// (FROM source, WHERE predicates, etc.) stays intact. UNION is a
// SELECT-level binary operator, not a clause keyword inside a single
// SELECT, so the UNION token sits between two pre-rendered QueryBuilder
// Frags rather than abusing a clause slot.
func (e *emitter) emitSetOperation(s *chplan.SetOperation) error {
	if s.TraceIDColumn == "" {
		return fmt.Errorf("%w: SetOperation trace-id column unset", ErrUnsupported)
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
		//   ON L.TraceId = R.TraceId [AND L.SpanId = R.SpanId]
		on := Eq(Qual("L", s.TraceIDColumn), Qual("R", s.TraceIDColumn))
		if s.SpanIDColumn != "" {
			on = And(on, Eq(Qual("L", s.SpanIDColumn), Qual("R", s.SpanIDColumn)))
		}
		sb := NewQuery().
			Select(verbatim("L.*")).
			From(As(leftFrag, "L")).
			Join(InnerJoin, As(rightFrag, "R"), on)
		e.emitSelect(sb)
		return nil
	case chplan.SetUnion:
		// SELECT * FROM (<left> UNION ALL <right>)
		//   LIMIT 1 BY (TraceId[, SpanId])
		// — dedup on span identity, not the full (wide) row. See the
		// godoc above for why this replaced the full-row UNION DISTINCT.
		by := []Frag{Col(s.TraceIDColumn)}
		if s.SpanIDColumn != "" {
			by = append(by, Col(s.SpanIDColumn))
		}
		sb := NewQuery().
			Select(verbatim("*")).
			From(Paren(UnionAll(leftFrag, rightFrag))).
			Limit(unionDedupLimitPerIdentity).
			LimitBy(by...)
		e.emitSelect(sb)
		return nil
	}
	return fmt.Errorf("%w: set op %q", ErrUnsupported, s.Op)
}
