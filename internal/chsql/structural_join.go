package chsql

import (
	"fmt"
	"strconv"

	"github.com/tsouza/cerberus/internal/chplan"
)

// defaultStructuralRecursionDepth caps the WITH RECURSIVE closure walk
// for `>>` / `<<` (and their negated / union variants) when the plan
// leaves StructuralJoin.MaxDepth unset (== 0, "unbounded"). It exists
// to defend against trace data with a span-id cycle — a span whose
// ParentSpanId chain loops back on itself (clock skew, instrumentation
// bug, OTLP span-id reuse). Without a bound such a trace drives CH's
// recursive-CTE evaluator past `max_recursive_cte_evaluation_depth`
// (default 1000) and FAILS the whole query with error 306
// (TOO_DEEP_RECURSION). A single malformed trace must never 500 a
// structural TraceQL query; the cap degrades a cyclic trace to a
// bounded/partial closure with no error.
//
// 128 is deep enough that no real, acyclic trace reaches it — span
// trees that deep are vanishingly rare and themselves a sign of a
// pathological producer — so acyclic closures stay byte-identical to
// the pre-cap output (the cap-depth row is never produced). It is also
// chosen to sit comfortably below ClickHouse's
// max_recursive_cte_evaluation_depth (default 1000): the recursive arm
// re-evaluates the seed-trace-id IN subquery each iteration, so CH's
// internal per-iteration evaluation-step count is a small multiple of
// the logical depth (empirically a cap of ~200 is the point at which a
// pathological self-loop trace crosses the 1000-step limit and errors
// 306). 128 leaves a wide margin so a cyclic trace degrades to a
// bounded, error-free partial closure on every supported CH. A caller
// that needs a different ceiling sets StructuralJoin.MaxDepth
// explicitly; any positive value overrides this default.
const defaultStructuralRecursionDepth = 128

// effectiveRecursionDepth resolves the depth cap actually emitted into
// the recursive CTE: the plan's explicit MaxDepth when positive, else
// the package default. The result is always > 0 — the recursive arm is
// now always bounded, so a cyclic trace can never run the CTE away.
func effectiveRecursionDepth(maxDepth int) int {
	if maxDepth > 0 {
		return maxDepth
	}
	return defaultStructuralRecursionDepth
}

// emitStructuralJoin renders a TraceQL structural relation against
// the otel_traces span table. The result projects the right-hand
// span's columns (TraceQL convention: `A > B` returns the matched B
// spans).
//
// Direct ops (single INNER JOIN, MaxDepth ignored):
//
//	StructuralChild  (`>`):  L.SpanID = R.ParentSpanID  (R's parent matches L)
//	StructuralParent (`<`):  L.ParentSpanID = R.SpanID  (R is L's parent)
//	StructuralSibling (`~`): L.ParentSpanID = R.ParentSpanID AND
//	                        L.SpanID != R.SpanID (same parent, distinct
//	                        spans)
//
// Recursive ops (CH `WITH RECURSIVE` CTE, walks the parent chain):
//
//	StructuralDescendant (`>>`): R is anywhere in the subtree rooted at any L
//	StructuralAncestor   (`<<`): R is anywhere in the ancestor chain of any L
//
// For the recursive forms an anchor row is each (TraceId, SpanId) pair
// produced by the L subquery; the recursive step joins otel_traces back
// to the CTE on (TraceId, ParentSpanId/SpanId) and walks one level per
// iteration. MaxDepth (when > 0) caps the iteration count; 0 means
// unbounded. The final SELECT inner-joins R against the closure.
//
// Negated ops (`!>` / `!<` / `!~` / `!>>` / `!<<`) reuse the relation
// predicate but swap the final outer INNER JOIN for a LEFT ANTI JOIN
// keyed (R left, L right) on the same relation. The result is the
// set of R rows for which no L participates in the relation.
//
// Union ops (`&>` / `&<` / `&~` / `&>>` / `&<<`) emit the positive
// relation twice — once projecting R.*, once projecting L.* — and
// glue the two arms with UNION DISTINCT. The output is the set of
// spans on either side that participate in the relation.
//
// The direct case uses the QueryBuilder.Join slot; the recursive case
// uses the QueryBuilder.WithRecursive slot for the WITH RECURSIVE …
// UNION ALL CTE shape.
func (e *emitter) emitStructuralJoin(j *chplan.StructuralJoin) error {
	if j.TraceIDColumn == "" || j.SpanIDColumn == "" || j.ParentSpanIDColumn == "" {
		return fmt.Errorf("%w: StructuralJoin column names unset", ErrUnsupported)
	}

	// Stamp the resolved cap when the plan reaches the emitter unbounded
	// (MaxDepth == 0). The rendered bound already resolves 0 via
	// effectiveRecursionDepth, so this is byte-neutral on the SQL — it
	// keeps the in-memory plan's MaxDepth in agreement with the emitted
	// `c._depth < N` literal, so the structural recursion and the
	// nested-set recursion (which also bounds at defaultStructuralRecursionDepth)
	// agree on the same ceiling.
	if j.MaxDepth == 0 {
		j.MaxDepth = defaultStructuralRecursionDepth
	}

	switch j.Op.Positive() {
	case chplan.StructuralChild, chplan.StructuralParent, chplan.StructuralSibling:
		return e.emitStructuralDirectJoin(j)
	case chplan.StructuralDescendant, chplan.StructuralAncestor:
		return e.emitStructuralRecursive(j)
	default:
		return fmt.Errorf("%w: structural op %q", ErrUnsupported, j.Op)
	}
}

// emitStructuralDirectJoin renders the single-INNER-JOIN form used by
// `>`, `<`, and `~`. Negated variants (`!>` / `!<` / `!~`) swap the
// JOIN kind to LEFT ANTI JOIN with R on the left side (so the result
// projects R rows missing any matching L). Union variants (`&>` /
// `&<` / `&~`) emit the positive INNER JOIN twice — once projecting
// R.*, once L.* — joined with UNION DISTINCT. MaxDepth is ignored for
// all direct flavours.
//
// The projection list re-aliases the join-key columns (TraceId,
// SpanId, ParentSpanId) to their bare names instead of letting `R.*`
// pass through with the `R.` qualifier baked into the output column
// names. CH 25.8's analyzer otherwise refuses `L.TraceId` against a
// subquery whose only matching identifier is `R.TraceId` — see
// https://github.com/tsouza/cerberus/issues/57 for the failing nested-
// structural-join repro and the chDB error trace.
func (e *emitter) emitStructuralDirectJoin(j *chplan.StructuralJoin) error {
	// Sibling relations need a dedicated shape: their distinct-span
	// clause (`L.SpanId != R.SpanId`) is an inequality between the two
	// join sides, which ClickHouse (24.x, and 25.x without
	// allow_experimental_join_condition) rejects inside JOIN ON with
	// "join expression contains column from left and right table". The
	// sibling emitter keeps the ON equality-only and expresses the
	// distinct-span rule WHERE-side.
	if j.Op.Positive() == chplan.StructuralSibling {
		return e.emitStructuralSiblingJoin(j)
	}
	relFrag, err := structuralDirectRelFrag(j)
	if err != nil {
		return err
	}

	leftSub, err := e.subqueryFrag(j.Left)
	if err != nil {
		return err
	}
	rightSub, err := e.subqueryFrag(j.Right)
	if err != nil {
		return err
	}

	rightProj := structuralProjectionFrags(j, "R")
	leftProj := structuralProjectionFrags(j, "L")

	switch {
	case j.Op.IsNegated():
		// Negated direct: R LEFT ANTI JOIN L on the positive relation.
		// CH's LEFT ANTI JOIN returns rows from the left input that
		// have no match on the right; placing R on the left lets the
		// SELECT R.* projection mirror the positive form.
		sb := NewQuery().
			Select(rightProj...).
			From(aliasedFrag(rightSub, "R")).
			Join(
				LeftAntiJoin,
				aliasedFrag(leftSub, "L"),
				structuralDirectOnFrag(j, relFrag),
			)
		e.emitSelect(sb)
		return nil
	case j.Op.IsUnion():
		// Union direct: (SELECT R.* FROM L INNER JOIN R ON <rel>)
		//   UNION DISTINCT
		// (SELECT L.* FROM L INNER JOIN R ON <rel>).
		rightArm := NewQuery().
			Select(rightProj...).
			From(aliasedFrag(leftSub, "L")).
			Join(
				InnerJoin,
				aliasedFrag(rightSub, "R"),
				structuralDirectOnFrag(j, relFrag),
			)
		leftArm := NewQuery().
			Select(leftProj...).
			From(aliasedFrag(leftSub, "L")).
			Join(
				InnerJoin,
				aliasedFrag(rightSub, "R"),
				structuralDirectOnFrag(j, relFrag),
			)
		b := NewBuilder()
		UnionDistinct(rightArm.Frag(), leftArm.Frag())(b)
		e.splice(b)
		return nil
	default:
		sb := NewQuery().
			Select(rightProj...).
			From(aliasedFrag(leftSub, "L")).
			Join(
				InnerJoin,
				aliasedFrag(rightSub, "R"),
				structuralDirectOnFrag(j, relFrag),
			)
		e.emitSelect(sb)
		return nil
	}
}

// emitStructuralSiblingJoin renders the sibling family (`~` / `!~` /
// `&~`) with an equality-only JOIN ON — ClickHouse rejects the naive
// `... AND L.SpanId != R.SpanId` ON clause with error 403
// INVALID_JOIN_ON_EXPRESSION ("join expression contains column from
// left and right table") on every released CH the compose / k3d / chDB
// stacks run.
//
// Shapes:
//
//   - `~` (inner): INNER JOIN on (TraceId, ParentSpanId) equality with
//     the distinct-span rule (`L.SpanId != R.SpanId`) moved to WHERE —
//     row-for-row equivalent to the old ON form.
//   - `&~` (union): the inner shape emitted twice (projecting R.* and
//     L.*) glued with UNION DISTINCT, mirroring the other union ops.
//   - `!~` (negated): an anti join cannot move the inequality to WHERE
//     (the non-match decision happens inside the join), so the L side
//     collapses to one row per (TraceId, ParentSpanId) carrying the
//     group's span count + span-id set, LEFT JOIN'd on the equality
//     keys. R has a sibling iff the group contains an L span other
//     than R itself: `_l_cnt - has(_l_span_ids, R.SpanId) > 0`. The
//     negated form keeps rows where that quantity is 0 — including
//     rows with no L group at all (LEFT JOIN defaults: _l_cnt = 0,
//     _l_span_ids = []).
func (e *emitter) emitStructuralSiblingJoin(j *chplan.StructuralJoin) error {
	leftSub, err := e.subqueryFrag(j.Left)
	if err != nil {
		return err
	}
	rightSub, err := e.subqueryFrag(j.Right)
	if err != nil {
		return err
	}

	rightProj := structuralProjectionFrags(j, "R")
	leftProj := structuralProjectionFrags(j, "L")

	onEq := And(
		spanIDPairFrag("L", j.TraceIDColumn, "R", j.TraceIDColumn),
		spanIDPairFrag("L", j.ParentSpanIDColumn, "R", j.ParentSpanIDColumn),
	)
	distinctSpan := Neq(qualColFrag("L", j.SpanIDColumn), qualColFrag("R", j.SpanIDColumn))

	switch {
	case j.Op.IsNegated():
		// Aggregate L per (TraceId, ParentSpanId): how many L spans the
		// group holds and which span ids they are.
		// `_l_cnt` / `_l_span_ids` are emitter-pinned bare aliases (no
		// backticks); the AS suffix rides verbatim while count() /
		// groupUniqArray(...) compose as Frags.
		cntAgg := func(b *Builder) { Call("count")(b); verbatim(" AS _l_cnt")(b) }
		spanIDsAgg := func(b *Builder) {
			Call("groupUniqArray", Col(j.SpanIDColumn))(b)
			verbatim(" AS _l_span_ids")(b)
		}
		aggL := NewQuery().
			Select(
				Col(j.TraceIDColumn),
				Col(j.ParentSpanIDColumn),
				cntAgg,
				spanIDsAgg,
			).
			From(aliasedFrag(leftSub, "_l")).
			GroupBy(Col(j.TraceIDColumn), Col(j.ParentSpanIDColumn))
		// `(L._l_cnt - has(L._l_span_ids, R.<spanID>)) = 0`. The two
		// synthetic aggregate columns are referenced bare-qualified
		// (`L._l_cnt`) — emitter-pinned tokens, so verbatim; the has()
		// call and arithmetic compose as Frags.
		negWhere := Eq(
			Paren(Sub(
				verbatim("L._l_cnt"),
				Call("has", verbatim("L._l_span_ids"), qualColFrag("R", j.SpanIDColumn)),
			)),
			InlineLit(0),
		)
		sb := NewQuery().
			Select(rightProj...).
			From(aliasedFrag(rightSub, "R")).
			Join(LeftJoin, aliasedFrag(aggL.Frag(), "L"), onEq).
			Where(negWhere)
		e.emitSelect(sb)
		return nil
	case j.Op.IsUnion():
		rightArm := NewQuery().
			Select(rightProj...).
			From(aliasedFrag(leftSub, "L")).
			Join(InnerJoin, aliasedFrag(rightSub, "R"), onEq).
			Where(distinctSpan)
		leftArm := NewQuery().
			Select(leftProj...).
			From(aliasedFrag(leftSub, "L")).
			Join(InnerJoin, aliasedFrag(rightSub, "R"), onEq).
			Where(distinctSpan)
		b := NewBuilder()
		UnionDistinct(rightArm.Frag(), leftArm.Frag())(b)
		e.splice(b)
		return nil
	default:
		sb := NewQuery().
			Select(rightProj...).
			From(aliasedFrag(leftSub, "L")).
			Join(InnerJoin, aliasedFrag(rightSub, "R"), onEq).
			Where(distinctSpan)
		e.emitSelect(sb)
		return nil
	}
}

// structuralProjectionFrags returns the projection list for a structural
// join's outer SELECT, rendered with bare aliases for every column the
// downstream consumer might reach for so the result can be wrapped in
// a parent subquery without losing column resolvability.
//
// Shape (side = "R", ExtraProjectionColumns = [SpanName, Duration,
// Timestamp, ResourceAttributes]):
//
//	R.`TraceId` AS `TraceId`, R.`SpanId` AS `SpanId`,
//	R.`ParentSpanId` AS `ParentSpanId`, R.`SpanName` AS `SpanName`,
//	R.`Duration` AS `Duration`, R.`Timestamp` AS `Timestamp`,
//	R.`ResourceAttributes` AS `ResourceAttributes`
//
// CH 25.8's analyzer drops `R.*`-introduced columns from outer-scope
// resolution when the JOIN's L and R sides have colliding column names
// (the otel_traces self-join is the canonical case): `R.SpanName` and
// bare `SpanName` both fail to resolve against `R.* EXCEPT (...)` in a
// wrap subquery. Listing each column the consumer will read here
// forces CH to expose them as bare-name aliases that the outer scope
// can resolve.
//
// When `ExtraProjectionColumns` is empty the helper falls back to the
// legacy `R.* EXCEPT (<keys>)` shape — kept for tests that construct
// StructuralJoin directly without populating the schema columns the
// API-layer wrap projection reads.
func structuralProjectionFrags(j *chplan.StructuralJoin, side string) []Frag {
	traceID := j.TraceIDColumn
	spanID := j.SpanIDColumn
	parentSpanID := j.ParentSpanIDColumn
	frags := []Frag{
		aliasedSideCol(side, traceID, traceID),
		aliasedSideCol(side, spanID, spanID),
		aliasedSideCol(side, parentSpanID, parentSpanID),
	}
	if len(j.ExtraProjectionColumns) == 0 {
		frags = append(frags, starExceptKeys(side, traceID, spanID, parentSpanID))
		return frags
	}
	for _, col := range j.ExtraProjectionColumns {
		frags = append(frags, aliasedSideCol(side, col, col))
	}
	return frags
}

// aliasedSideCol renders `<side>.<col> AS <alias>` with `col` and
// `alias` both backtick-quoted. The bare side (L / R) rides qualColFrag;
// As applies the alias's backtick quoting.
func aliasedSideCol(side, col, alias string) Frag {
	return As(qualColFrag(side, col), alias)
}

// starExceptKeys renders `<side>.* EXCEPT (<k1>, <k2>, <k3>)` with each
// key backtick-quoted. Used in tandem with the leading aliased-key
// projections to pass through every other column without re-emitting
// the keys twice. The `<side>.* EXCEPT (` and `)` glue is an
// emitter-chosen synthetic shape (bare side alias + CH's EXCEPT modifier
// on a star projection — no Frag constructor covers a star-except), so
// it rides verbatim; the keys flow through Col's quoting.
func starExceptKeys(side, k1, k2, k3 string) Frag {
	return func(b *Builder) {
		verbatim(side + ".* EXCEPT (")(b)
		Col(k1)(b)
		verbatim(", ")(b)
		Col(k2)(b)
		verbatim(", ")(b)
		Col(k3)(b)
		verbatim(")")(b)
	}
}

// structuralDirectRelFrag returns the relation predicate that pairs
// with the trace-id equality. The leading `L.<TraceID> = R.<TraceID>
// AND` glue is composed in structuralDirectOnFrag — this helper just
// emits the operator-specific clause. The predicate is keyed off the
// positive form of j.Op so negated / union variants share the same
// shape as their base relation.
func structuralDirectRelFrag(j *chplan.StructuralJoin) (Frag, error) {
	switch j.Op.Positive() {
	case chplan.StructuralChild:
		// `A > B`: L.SpanID = R.ParentSpanID.
		return spanIDPairFrag("L", j.SpanIDColumn, "R", j.ParentSpanIDColumn), nil
	case chplan.StructuralParent:
		// `A < B`: L.ParentSpanID = R.SpanID.
		return spanIDPairFrag("L", j.ParentSpanIDColumn, "R", j.SpanIDColumn), nil
	default:
		// StructuralSibling never reaches here — the sibling family
		// routes through emitStructuralSiblingJoin (its distinct-span
		// inequality cannot live inside JOIN ON).
		return nil, fmt.Errorf("%w: direct structural op %q", ErrUnsupported, j.Op)
	}
}

// spanIDPairFrag returns a Frag for `<lside>.<lcol> = <rside>.<rcol>`.
func spanIDPairFrag(lside, lcol, rside, rcol string) Frag {
	return Eq(qualColFrag(lside, lcol), qualColFrag(rside, rcol))
}

// structuralDirectOnFrag composes the full ON clause:
// `L.<TraceID> = R.<TraceID> AND <rel>`. The trace-id equality is
// always present — direct ops scope every relation to within a trace.
func structuralDirectOnFrag(j *chplan.StructuralJoin, rel Frag) Frag {
	return And(spanIDPairFrag("L", j.TraceIDColumn, "R", j.TraceIDColumn), rel)
}

// emitStructuralRecursive renders `>>` / `<<` as a `WITH RECURSIVE`
// CTE that walks the parent chain inside otel_traces, seeded by the
// L subquery's (TraceId, SpanId) pairs. The closure is then
// inner-joined against the R subquery on the matched span identity:
//
//	`>>`: descendant of L — recursive step joins child spans
//	      (otel_traces.ParentSpanId = closure.SpanId).
//	`<<`: ancestor of L   — recursive step joins parent spans
//	      (otel_traces.SpanId = closure.ParentSpanId).
//
// The closure tracks a depth column. The recursive arm is always
// bounded: MaxDepth caps the walk when positive, otherwise the package
// default (defaultStructuralRecursionDepth) applies — so a span-id
// cycle degrades to a partial closure instead of erroring with CH code
// 306. For the common case (acyclic traces shallower than the cap) the
// walk still terminates at the natural fixpoint (no further rows once
// ParentSpanId hits the trace root), byte-identical to the unbounded
// output, well before the cap.
//
// The recursive arm also scopes its scan of the span table to the
// seed's trace-id set (`t.TraceId IN (SELECT TraceId FROM (<L>))`),
// so each iteration reads only the candidate traces' rows rather than
// re-scanning the whole table — the #77 predicate pushdown. This is
// semantics-preserving (the step ON already pins t.TraceId =
// c.TraceId) and pulls the seed's `[start,end]` time window into the
// recursive scan for free.
//
// Rendered shape (>> case, default cap):
//
//	SELECT R.* FROM (
//	  WITH RECURSIVE _struct_closure AS (
//	    SELECT `TraceId`, `SpanId`, `ParentSpanId`, 0 AS _depth
//	      FROM (<L>) AS _seed
//	    UNION ALL
//	    SELECT t.`TraceId`, t.`SpanId`, t.`ParentSpanId`, c._depth + 1
//	      FROM `otel_traces` AS t
//	      INNER JOIN _struct_closure AS c
//	        ON t.`TraceId` = c.`TraceId`
//	       AND t.`ParentSpanId` = c.`SpanId`
//	      WHERE c._depth < <cap>
//	        AND t.`TraceId` IN (SELECT `TraceId` FROM (<L>) AS _seed_ids)
//	  )
//	  SELECT DISTINCT `TraceId`, `SpanId` FROM _struct_closure
//	) AS L INNER JOIN (<R>) AS R
//	  ON L.`TraceId` = R.`TraceId` AND L.`SpanId` = R.`SpanId`
//
// Anchor rows are *not* themselves part of the descendant/ancestor
// closure for the join — TraceQL semantics require R to be strictly
// downstream / upstream of L. We achieve this by excluding the
// anchor depth (0) from the final projection (depth > 0 filter).
//
// Negated recursive variants (`!>>` / `!<<`) reuse the same closure
// and swap the outer INNER JOIN for a LEFT ANTI JOIN with R on the
// left side — the R rows that the L-rooted closure does *not* reach.
// Union recursive variants (`&>>` / `&<<`) emit the closure-keyed
// INNER JOIN twice (projecting R.* and L.* respectively) joined by
// UNION DISTINCT, mirroring the direct-union shape.
func (e *emitter) emitStructuralRecursive(j *chplan.StructuralJoin) error {
	// Recursive step direction depends on the *positive* form of the
	// operator — negated / union variants reuse the same closure.
	var stepRel Frag
	switch j.Op.Positive() {
	case chplan.StructuralDescendant:
		stepRel = spanIDPairFrag("t", j.ParentSpanIDColumn, "c", j.SpanIDColumn)
	case chplan.StructuralAncestor:
		stepRel = spanIDPairFrag("t", j.SpanIDColumn, "c", j.ParentSpanIDColumn)
	default:
		return fmt.Errorf("%w: recursive structural op %q", ErrUnsupported, j.Op)
	}

	// Resolve the source table name from the *first* Scan we encounter
	// inside the L subtree. The recursive step needs an explicit table
	// reference rather than re-running the L subquery (which would
	// duplicate filter work and double-count anchors).
	table := findScanTable(j.Left)
	if table == "" {
		return fmt.Errorf("%w: recursive StructuralJoin needs a Scan in L subtree", ErrUnsupported)
	}
	// The recursive arm scans the spans table directly. Put it under the
	// resource-bound invariant so the step FROM is gated by fromSpansScan even
	// when the caller did not thread WithSpansTable onto the emit context.
	e.spansTable = table

	leftSub, err := e.subqueryFrag(j.Left)
	if err != nil {
		return err
	}
	rightSub, err := e.subqueryFrag(j.Right)
	if err != nil {
		return err
	}

	// Unique CTE name for this closure. Nested structural joins
	// (`A << B << C`) embed an inner closure inside the outer closure's
	// recursive arm via the seed-trace-id pushdown subquery; a shared
	// `_struct_closure` name makes CH bind the inner CTE in the outer
	// scope and reject the outer as "not recursive" (error 49). The
	// per-emit counter keeps every closure name distinct. The inner
	// (L / R) subqueries are rendered above, so they already consumed
	// their sequence numbers — this outer closure takes the next one.
	cteName := "_struct_closure_" + strconv.Itoa(e.nextStructSeq())

	// Anchor: SELECT DISTINCT TraceId, SpanId, ParentSpanId, 0 AS _depth
	// FROM (<L>) AS _seed. DISTINCT on both CTE arms keeps the closure
	// linear in the number of UNIQUE spans: duplicate span rows (OTLP
	// retries, rolling re-seeds) otherwise multiply at every recursion
	// level — dup^depth rows — and a 4-deep walk over a table with a
	// few hundred copies per span blows straight through the per-query
	// memory cap. Within one iteration every row carries the same
	// _depth, so the DISTINCT collapses exact duplicates only.
	anchor := NewQuery().
		Select(
			Distinct(Col(j.TraceIDColumn)),
			Col(j.SpanIDColumn),
			Col(j.ParentSpanIDColumn),
			verbatim("0 AS _depth"),
		).
		From(aliasedFrag(leftSub, "_seed"))

	// Recursive step: SELECT t.<...>, c._depth + 1 FROM `<table>` AS t
	// INNER JOIN _struct_closure AS c ON <stepOn>
	// WHERE c._depth < <cap> [AND t.TraceId IN (SELECT TraceId FROM (<L>) AS _seed_ids)].
	stepOn := And(spanIDPairFrag("t", j.TraceIDColumn, "c", j.TraceIDColumn), stepRel)
	stepFrom, err := e.fromSpansScan(table, structuralStepBound(j.Left, j.TraceIDColumn, leftSub))
	if err != nil {
		return err
	}
	step := NewQuery().
		Select(
			Distinct(qualColFrag("t", j.TraceIDColumn)),
			qualColFrag("t", j.SpanIDColumn),
			qualColFrag("t", j.ParentSpanIDColumn),
			verbatim("c._depth + 1"),
		).
		From(aliasedFrag(stepFrom, "t")).
		Join(
			InnerJoin,
			aliasedFrag(verbatim(cteName), "c"),
			stepOn,
		).
		Where(structuralStepWhere(j, j.Left, leftSub)...)

	// Closure subquery: WITH RECURSIVE <cteName> AS (<anchor> UNION ALL <step>) SELECT DISTINCT TraceId, SpanId FROM <cteName> WHERE _depth > 0.
	closure := NewQuery().
		WithRecursive(cteName, anchor, step).
		Select(
			Distinct(Col(j.TraceIDColumn)),
			Col(j.SpanIDColumn),
		).
		From(verbatim(cteName)).
		Where(verbatim("_depth > 0"))

	onClause := And(
		spanIDPairFrag("L", j.TraceIDColumn, "R", j.TraceIDColumn),
		spanIDPairFrag("L", j.SpanIDColumn, "R", j.SpanIDColumn),
	)
	rightProj := structuralProjectionFrags(j, "R")
	leftProj := structuralProjectionFrags(j, "L")
	switch {
	case j.Op.IsNegated():
		// Negated recursive: R LEFT ANTI JOIN closure(L). The closure
		// stays on the right so the SELECT R.* projection holds; the
		// LEFT ANTI returns R rows the L-rooted closure misses.
		sb := NewQuery().
			Select(rightProj...).
			From(aliasedFrag(rightSub, "R")).
			Join(LeftAntiJoin, aliasedFrag(closure.Frag(), "L"), onClause)
		e.emitSelect(sb)
		return nil
	case j.Op.IsUnion():
		// Union recursive: emit two closure-keyed INNER-JOIN arms —
		// one projecting R.*, one L.* — and dedup with UNION
		// DISTINCT. The L arm pulls back to the L subquery via a
		// second join on the closure so multi-level matches are
		// recovered, mirroring the positive recursive shape.
		rightArm := NewQuery().
			Select(rightProj...).
			From(aliasedFrag(closure.Frag(), "L")).
			Join(InnerJoin, aliasedFrag(rightSub, "R"), onClause)
		// Closure for the L-projection arm walks in the *inverse*
		// direction so an R span finds the L spans related to it.
		// For `&>>` (L ancestor of R) the inverse closure starts at
		// each R span and walks towards ancestors — matching the L
		// subquery on the upward-walked SpanIds. We rebuild the
		// closure with R as the seed and step direction inverted.
		invCTEName := "_struct_closure_inv_" + strconv.Itoa(e.nextStructSeq())
		inverseClosure, err := e.buildStructuralInverseClosure(j, j.Right, rightSub, table, invCTEName)
		if err != nil {
			return err
		}
		leftArm := NewQuery().
			Select(leftProj...).
			From(aliasedFrag(leftSub, "L")).
			Join(InnerJoin, aliasedFrag(inverseClosure.Frag(), "R"), onClause)
		b := NewBuilder()
		UnionDistinct(rightArm.Frag(), leftArm.Frag())(b)
		e.splice(b)
		return nil
	default:
		// Outer SELECT R.* FROM (<closure>) AS L INNER JOIN (<R>) AS R ON L.TraceId = R.TraceId AND L.SpanId = R.SpanId.
		sb := NewQuery().
			Select(rightProj...).
			From(aliasedFrag(closure.Frag(), "L")).
			Join(InnerJoin, aliasedFrag(rightSub, "R"), onClause)
		e.emitSelect(sb)
		return nil
	}
}

// buildStructuralInverseClosure constructs the recursive CTE used by
// the L-projection arm of a union recursive structural join. The
// canonical closure (built in the caller of emitStructuralRecursive)
// walks from L spans towards R; this helper walks the *inverse*
// direction so each R span surfaces the L spans connected to it.
//
// For `A &>> B` the canonical closure walks down from each L
// (`t.ParentSpanId = c.SpanId`); the inverse walks up from each R
// (`t.SpanId = c.ParentSpanId`). The two arms of the UNION DISTINCT
// thus cover both projection directions, mirroring upstream's
// `union=true` Span.DescendantOf semantics.
func (e *emitter) buildStructuralInverseClosure(j *chplan.StructuralJoin, seedNode chplan.Node, rightSub Frag, table, cteName string) (*QueryBuilder, error) {
	var stepRel Frag
	switch j.Op.Positive() {
	case chplan.StructuralDescendant:
		// L &>> R means L is ancestor of R. Inverse closure walks up
		// from R: t.SpanId = c.ParentSpanId.
		stepRel = spanIDPairFrag("t", j.SpanIDColumn, "c", j.ParentSpanIDColumn)
	case chplan.StructuralAncestor:
		// L &<< R means L is descendant of R. Inverse closure walks
		// down from R: t.ParentSpanId = c.SpanId.
		stepRel = spanIDPairFrag("t", j.ParentSpanIDColumn, "c", j.SpanIDColumn)
	default:
		return nil, fmt.Errorf("%w: union recursive structural op %q", ErrUnsupported, j.Op)
	}

	// DISTINCT on both arms — same duplicate-row containment as the
	// canonical closure (see emitStructuralRecursive).
	anchor := NewQuery().
		Select(
			Distinct(Col(j.TraceIDColumn)),
			Col(j.SpanIDColumn),
			Col(j.ParentSpanIDColumn),
			verbatim("0 AS _depth"),
		).
		From(aliasedFrag(rightSub, "_seed"))

	stepOn := And(spanIDPairFrag("t", j.TraceIDColumn, "c", j.TraceIDColumn), stepRel)
	stepFrom, err := e.fromSpansScan(table, structuralStepBound(seedNode, j.TraceIDColumn, rightSub))
	if err != nil {
		return nil, err
	}
	step := NewQuery().
		Select(
			Distinct(qualColFrag("t", j.TraceIDColumn)),
			qualColFrag("t", j.SpanIDColumn),
			qualColFrag("t", j.ParentSpanIDColumn),
			verbatim("c._depth + 1"),
		).
		From(aliasedFrag(stepFrom, "t")).
		Join(
			InnerJoin,
			aliasedFrag(verbatim(cteName), "c"),
			stepOn,
		).
		Where(structuralStepWhere(j, seedNode, rightSub)...)

	closure := NewQuery().
		WithRecursive(cteName, anchor, step).
		Select(
			Distinct(Col(j.TraceIDColumn)),
			Col(j.SpanIDColumn),
		).
		From(verbatim(cteName)).
		Where(verbatim("_depth > 0"))
	return closure, nil
}

// structuralStepWhere builds the WHERE conjuncts for a recursive
// closure's step arm: always the #78 depth bound, plus the #77
// seed-trace-id pushdown when it is safe to emit.
//
// The pushdown re-embeds the seed subquery (<seedNode> rendered as
// seedSub) inside the recursive arm's IN subquery. ClickHouse rejects
// a recursive CTE nested inside another recursive CTE's recursive arm
// (error 49 "is not recursive"), so when the seed subtree itself
// lowers to a recursive structural closure (a nested `>>` / `<<`
// chain) the pushdown is skipped — that level keeps the depth bound
// only. The skip is correctness-preserving: the recursive arm still
// joins `t.TraceId = c.TraceId` (scoping to the working set), it just
// doesn't additionally pre-filter `t` by the seed's trace-id set. The
// common single-level case (and the innermost level of a nested chain)
// always gets the pushdown.
func structuralStepWhere(j *chplan.StructuralJoin, seedNode chplan.Node, seedSub Frag) []Frag {
	conds := []Frag{structuralDepthBoundFrag(j.MaxDepth)}
	if !subtreeHasRecursiveStructural(seedNode) {
		conds = append(conds, structuralSeedTraceFilter(j.TraceIDColumn, seedSub))
	}
	return conds
}

// structuralStepBound classifies the resource bound on a recursive structural
// step's spans scan, mirroring structuralStepWhere's pushdown decision. When
// the seed subtree is NOT itself recursive, the step carries the seed-trace-id
// IN pushdown (`t.TraceId IN (SELECT TraceId FROM (<seed>))`) — a finite
// trace-id set (form-b). When the seed IS recursive the pushdown is dropped to
// avoid CH error 49 (a recursive subquery nested in a recursive arm), so the
// step is bounded only by the recursion depth cap + the finite recursive
// working set — honestly classified as memory-streaming, not a partition claim.
func structuralStepBound(seedNode chplan.Node, traceIDCol string, seedSub Frag) scanResourceBound {
	if subtreeHasRecursiveStructural(seedNode) {
		return memoryStreamingBound()
	}
	return traceIDSetBound(structuralSeedTraceFilter(traceIDCol, seedSub))
}

// subtreeHasRecursiveStructural reports whether n (or any descendant)
// is a StructuralJoin whose positive op is recursive (`>>` / `<<`),
// i.e. it would itself emit a WITH RECURSIVE closure. Used to decide
// whether the seed-trace-id pushdown is safe to nest inside a parent
// recursive arm (see structuralStepWhere).
func subtreeHasRecursiveStructural(n chplan.Node) bool {
	if n == nil {
		return false
	}
	if sj, ok := n.(*chplan.StructuralJoin); ok {
		switch sj.Op.Positive() {
		case chplan.StructuralDescendant, chplan.StructuralAncestor:
			return true
		}
	}
	for _, c := range n.Children() {
		if subtreeHasRecursiveStructural(c) {
			return true
		}
	}
	return false
}

// structuralDepthBoundFrag renders the recursive-CTE iteration cap
// `c._depth < <cap>`, where <cap> is effectiveRecursionDepth(maxDepth)
// — the plan's explicit MaxDepth when positive, else the package
// default. The bound is rendered as a literal integer: depth caps are
// part of the query shape (CH's recursive-CTE planner needs them
// visible, not parameterised), not user data. Because the cap is
// always positive, every recursive structural query is now bounded —
// a span-id cycle degrades to a partial closure of depth <cap> instead
// of erroring with CH code 306 (TOO_DEEP_RECURSION).
func structuralDepthBoundFrag(maxDepth int) Frag {
	bound := effectiveRecursionDepth(maxDepth)
	return verbatim("c._depth < " + strconv.Itoa(bound))
}

// structuralSeedTraceFilter renders `t.<TraceID> IN (SELECT <TraceID>
// FROM <seed> AS _seed_ids)` — the predicate that restricts the
// recursive arm's scan of the span table to only the trace ids the
// seed (L for the canonical closure, R for the inverse) matched.
//
// This is the #77 predicate pushdown. Without it the recursive arm
// scans the bare full span table on every iteration (O(depth ×
// full-scan)); with it each level reads only the rows of the seed's
// candidate traces (O(matching-trace-rows)). The rewrite is
// semantics-preserving: the closure is per-trace (the step ON already
// pins `t.TraceId = c.TraceId`, and every `c.TraceId` originates in
// the seed), so no descendant/ancestor row can be added or dropped by
// scoping `t` to the seed's trace-id set. Because the seed subquery
// (<L> / <R>) carries the search's `[start,end]` Timestamp filter and
// resource/span predicates, that time window is pushed into the
// recursive scan for free.
//
// seedSub is the already-rendered seed subquery Frag (parenthesised by
// subqueryFrag); aliasing it `_seed_ids` keeps CH's analyzer happy
// when the subquery is referenced from the IN clause.
func structuralSeedTraceFilter(traceIDCol string, seedSub Frag) Frag {
	seedIDs := NewQuery().
		Select(Col(traceIDCol)).
		From(aliasedFrag(seedSub, "_seed_ids"))
	// In() already parenthesises its right-hand list, so splice the
	// seed-id SELECT bare (Spliced, not Subquery — the latter would
	// double-wrap in parens).
	return In(qualColFrag("t", traceIDCol), Spliced(seedIDs))
}

// findScanTable walks a plan subtree looking for the first chplan.Scan
// and returns its Table. Used by the recursive structural emitter to
// resolve the otel_traces table name (which may have been renamed via
// schema config) without re-emitting the entire L subquery for each
// recursion step. Returns "" when no Scan is found.
func findScanTable(n chplan.Node) string {
	if n == nil {
		return ""
	}
	if s, ok := n.(*chplan.Scan); ok {
		return s.Table
	}
	for _, c := range n.Children() {
		if t := findScanTable(c); t != "" {
			return t
		}
	}
	return ""
}
