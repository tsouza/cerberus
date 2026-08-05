package chsql

import (
	"fmt"
	"strconv"

	"github.com/tsouza/cerberus/internal/chplan"
)

// unionDedupLimitPerIdentity is the `LIMIT <n> BY (TraceId, SpanId)`
// count for the span-identity dedup both set ops end in: keep exactly
// ONE row per distinct span identity. The value 1 *is* the dedup —
// `LIMIT 1 BY` returns the first row of each (TraceId, SpanId) group
// with no global row cap, which is exactly TraceQL's "a span appears at
// most once in the result" semantics. Named so the literal isn't a bare
// magic 1.
const unionDedupLimitPerIdentity = 1

// The `&&` arm CTE names. Each arm subplan is materialised once under
// these names and referenced three times (its UNION-ALL leg plus both
// trace-cohort IN subqueries); inlining instead would re-render the
// whole subplan three times per nesting level, so `A && B && C && D`
// would grow the emitted text exponentially. The `<n>` suffix comes
// from emitter.nextCTESeq so a nested `&&` never shadows its parent's
// arm names in the outer CH scope.
const (
	intersectLeftArmCTEPrefix  = "_setand_l_"
	intersectRightArmCTEPrefix = "_setand_r_"
)

// emitSetOperation renders a TraceQL spanset set-op (`A && B`, `A || B`).
//
// Both ops end in the SAME span-level union, deduped on SPAN IDENTITY —
// they differ only in which traces survive. That is not a cerberus
// simplification; it is what reference Tempo does. In
// `grafana/tempo pkg/traceql/ast_execute.go` `SpansetOperation.evaluate`
// runs both sides against one trace's spanset at a time and then:
//
//	case OpSpansetAnd:
//	    if len(lhs) > 0 && len(rhs) > 0 { output = addSpanset(input[i], uniqueSpans(lhs, rhs), output) }
//	case OpSpansetUnion:
//	    if len(lhs) > 0 || len(rhs) > 0 { output = addSpanset(input[i], uniqueSpans(lhs, rhs), output) }
//
// `uniqueSpans` is a span-level UNION with duplicate removal, used
// verbatim by both branches. So `&&` is a TRACE-level conjunction over a
// SPAN-level union — "traces where both sides matched something, and for
// those traces every span either side matched" — NOT a span-level
// intersection. `{ a } && { b }` where `a` and `b` match different spans
// of one trace returns both spans, where a span intersection returns
// nothing.
//
//   - SetIntersect (`&&`): `WITH l AS (<left>), r AS (<right>)
//     SELECT * FROM ((SELECT * FROM l) UNION ALL (SELECT * FROM r))
//     WHERE TraceId IN (SELECT TraceId FROM l)
//     AND TraceId IN (SELECT TraceId FROM r)
//     LIMIT 1 BY (TraceID, SpanID)` — the two IN predicates are the
//     `len(lhs) > 0 && len(rhs) > 0` trace gate, the UNION ALL +
//     `LIMIT 1 BY` is `uniqueSpans`.
//   - SetUnion (`||`): the same shape minus the trace gate, because
//     `len(lhs) > 0 || len(rhs) > 0` admits every trace either arm
//     already emitted a row for.
//
// Cerberus's row model has one spanset per trace (chplan.SetOperation's
// contract: spanset boundaries collapse to a flat row stream keyed on
// (TraceId, SpanId)), so "one input spanset" and "one trace" coincide
// and the trace-scoped gate is exactly Tempo's per-spanset gate.
//
// Dedup is on span identity, never the full row tuple. Both arms read
// the same spans table, so a span surfaced by both predicates is
// byte-identical and keeping one is result-identical to a full-row
// UNION DISTINCT — but it streams cheaply via CH's `LIMIT n BY` instead
// of building a hash set keyed on every (wide) column, including the
// nested Events.* / Links.* array columns the OTel-CH schema carries.
// The full-row UNION DISTINCT was the root cause of the
// showcase-traceql "Spanset ||" panel never loading at self-telemetry
// scale (~150k+ spans): the dedup hash over wide rows with array
// columns is O(rows × row-width) memory; identity dedup is
// O(rows × 2 ids). It also sits at the top of the Drilldown
// structure-tab `(... &>>) || (...)` query, so the same wide-row tax is
// lifted off that path's outer union. A CH Map compares POSITIONALLY
// over its (keys, values) arrays, so full-row dedup would additionally
// treat one span redelivered under a different OTLP attribute key order
// as two rows; identity dedup never looks at the Map columns.
//
// Both shapes flow through QueryBuilder so the typed slot lifecycle
// (FROM source, WHERE predicates, etc.) stays intact. UNION is a
// SELECT-level binary operator, not a clause keyword inside a single
// SELECT, so the UNION token sits between two pre-rendered QueryBuilder
// Frags rather than abusing a clause slot.
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
		return e.emitSelect(intersectQuery(s, leftFrag, rightFrag, e.nextCTESeq()))
	case chplan.SetUnion:
		sb := NewQuery().
			Select(verbatim("*")).
			From(Paren(UnionAll(leftFrag, rightFrag))).
			Limit(unionDedupLimitPerIdentity).
			LimitBy(Col(s.TraceIDColumn), Col(s.SpanIDColumn))
		return e.emitSelect(sb)
	}
	return fmt.Errorf("%w: set op %q", ErrUnsupported, s.Op)
}

// intersectQuery assembles the `&&` shape described in
// emitSetOperation's godoc: both arms materialised as CTEs, their rows
// unioned and deduped on span identity, gated on the trace appearing in
// BOTH arms.
//
// seq disambiguates the arm CTE names against an enclosing `&&` (or an
// enclosing structural closure, which draws from the same counter).
func intersectQuery(s *chplan.SetOperation, leftFrag, rightFrag Frag, seq int) *QueryBuilder {
	suffix := strconv.Itoa(seq)
	leftCTE := intersectLeftArmCTEPrefix + suffix
	rightCTE := intersectRightArmCTEPrefix + suffix

	// (SELECT * FROM <cte>) — the arm's rows, as a UNION ALL leg.
	armRows := func(cte string) Frag { return NewQuery().From(BareIdent(cte)).Frag() }
	// TraceId IN (SELECT TraceId FROM <cte>) — "this arm matched at
	// least one span in this trace", i.e. Tempo's `len(side) > 0`.
	armTraceCohort := func(cte string) Frag {
		return InSubquery(
			Col(s.TraceIDColumn),
			NewQuery().Select(Col(s.TraceIDColumn)).From(BareIdent(cte)).Frag(),
		)
	}

	return NewQuery().
		With(leftCTE, leftFrag).
		With(rightCTE, rightFrag).
		Select(verbatim("*")).
		From(Paren(UnionAll(armRows(leftCTE), armRows(rightCTE)))).
		Where(armTraceCohort(leftCTE), armTraceCohort(rightCTE)).
		Limit(unionDedupLimitPerIdentity).
		LimitBy(Col(s.TraceIDColumn), Col(s.SpanIDColumn))
}
