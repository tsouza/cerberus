package chsql

import (
	"fmt"
	"strconv"

	"github.com/tsouza/cerberus/internal/chplan"
)

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
// `alias` both backtick-quoted.
func aliasedSideCol(side, col, alias string) Frag {
	return func(b *Builder) {
		writeSideCol(b, side, col)
		b.writeSQL(" AS ")
		b.Ident(alias)
	}
}

// starExceptKeys renders `<side>.* EXCEPT (<k1>, <k2>, <k3>)` with each
// key backtick-quoted. Used in tandem with the leading aliased-key
// projections to pass through every other column without re-emitting
// the keys twice.
func starExceptKeys(side, k1, k2, k3 string) Frag {
	return func(b *Builder) {
		b.writeSQL(side)
		b.writeSQL(".* EXCEPT (")
		b.Ident(k1)
		b.writeSQL(", ")
		b.Ident(k2)
		b.writeSQL(", ")
		b.Ident(k3)
		b.writeSQL(")")
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
	case chplan.StructuralSibling:
		// `A ~ B`: same trace, same parent, distinct spans. The
		// distinct-span clause keeps a row from matching itself
		// when both sides of the spanset select the same span.
		return func(b *Builder) {
			spanIDPairFrag("L", j.ParentSpanIDColumn, "R", j.ParentSpanIDColumn)(b)
			b.writeSQL(" AND ")
			writeSideCol(b, "L", j.SpanIDColumn)
			b.writeSQL(" != ")
			writeSideCol(b, "R", j.SpanIDColumn)
		}, nil
	default:
		return nil, fmt.Errorf("%w: direct structural op %q", ErrUnsupported, j.Op)
	}
}

// spanIDPairFrag returns a Frag for `<lside>.<lcol> = <rside>.<rcol>`.
func spanIDPairFrag(lside, lcol, rside, rcol string) Frag {
	return func(b *Builder) {
		writeSideCol(b, lside, lcol)
		b.writeSQL(" = ")
		writeSideCol(b, rside, rcol)
	}
}

// structuralDirectOnFrag composes the full ON clause:
// `L.<TraceID> = R.<TraceID> AND <rel>`. The trace-id equality is
// always present — direct ops scope every relation to within a trace.
func structuralDirectOnFrag(j *chplan.StructuralJoin, rel Frag) Frag {
	return func(b *Builder) {
		spanIDPairFrag("L", j.TraceIDColumn, "R", j.TraceIDColumn)(b)
		b.writeSQL(" AND ")
		rel(b)
	}
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
// The closure tracks a depth column so MaxDepth can cap the walk;
// unbounded walks (MaxDepth == 0) omit the cap and rely on the
// natural fixpoint (no further rows produced when ParentSpanId hits
// the trace root).
//
// Rendered shape (>> case, MaxDepth = 0):
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

	leftSub, err := e.subqueryFrag(j.Left)
	if err != nil {
		return err
	}
	rightSub, err := e.subqueryFrag(j.Right)
	if err != nil {
		return err
	}

	// Anchor: SELECT TraceId, SpanId, ParentSpanId, 0 AS _depth FROM (<L>) AS _seed.
	anchor := NewQuery().
		Select(
			Col(j.TraceIDColumn),
			Col(j.SpanIDColumn),
			Col(j.ParentSpanIDColumn),
			verbatim("0 AS _depth"),
		).
		From(aliasedFrag(leftSub, "_seed"))

	// Recursive step: SELECT t.<...>, c._depth + 1 FROM `<table>` AS t INNER JOIN _struct_closure AS c ON <stepOn> [WHERE c._depth < N].
	stepOn := func(b *Builder) {
		spanIDPairFrag("t", j.TraceIDColumn, "c", j.TraceIDColumn)(b)
		b.writeSQL(" AND ")
		stepRel(b)
	}
	step := NewQuery().
		Select(
			qualColFrag("t", j.TraceIDColumn),
			qualColFrag("t", j.SpanIDColumn),
			qualColFrag("t", j.ParentSpanIDColumn),
			verbatim("c._depth + 1"),
		).
		From(aliasedFrag(Col(table), "t")).
		Join(
			InnerJoin,
			aliasedFrag(verbatim("_struct_closure"), "c"),
			stepOn,
		)
	if j.MaxDepth > 0 {
		// MaxDepth caps the iteration count. Rendered as a literal
		// integer — depth bounds are part of the query shape, not
		// user data, and CH's recursive-CTE planner needs them
		// visible (not parameterised).
		step.Where(verbatim("c._depth < " + strconv.Itoa(j.MaxDepth)))
	}

	// Closure subquery: WITH RECURSIVE _struct_closure AS (<anchor> UNION ALL <step>) SELECT DISTINCT TraceId, SpanId FROM _struct_closure WHERE _depth > 0.
	closure := NewQuery().
		WithRecursive("_struct_closure", anchor, step).
		Select(
			Distinct(Col(j.TraceIDColumn)),
			Col(j.SpanIDColumn),
		).
		From(verbatim("_struct_closure")).
		Where(verbatim("_depth > 0"))

	onClause := func(b *Builder) {
		spanIDPairFrag("L", j.TraceIDColumn, "R", j.TraceIDColumn)(b)
		b.writeSQL(" AND ")
		spanIDPairFrag("L", j.SpanIDColumn, "R", j.SpanIDColumn)(b)
	}
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
		inverseClosure, err := buildStructuralInverseClosure(j, rightSub, table)
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
func buildStructuralInverseClosure(j *chplan.StructuralJoin, rightSub Frag, table string) (*QueryBuilder, error) {
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

	anchor := NewQuery().
		Select(
			Col(j.TraceIDColumn),
			Col(j.SpanIDColumn),
			Col(j.ParentSpanIDColumn),
			verbatim("0 AS _depth"),
		).
		From(aliasedFrag(rightSub, "_seed"))

	stepOn := func(b *Builder) {
		spanIDPairFrag("t", j.TraceIDColumn, "c", j.TraceIDColumn)(b)
		b.writeSQL(" AND ")
		stepRel(b)
	}
	step := NewQuery().
		Select(
			qualColFrag("t", j.TraceIDColumn),
			qualColFrag("t", j.SpanIDColumn),
			qualColFrag("t", j.ParentSpanIDColumn),
			verbatim("c._depth + 1"),
		).
		From(aliasedFrag(Col(table), "t")).
		Join(
			InnerJoin,
			aliasedFrag(verbatim("_struct_closure_inv"), "c"),
			stepOn,
		)
	if j.MaxDepth > 0 {
		step.Where(verbatim("c._depth < " + strconv.Itoa(j.MaxDepth)))
	}

	closure := NewQuery().
		WithRecursive("_struct_closure_inv", anchor, step).
		Select(
			Distinct(Col(j.TraceIDColumn)),
			Col(j.SpanIDColumn),
		).
		From(verbatim("_struct_closure_inv")).
		Where(verbatim("_depth > 0"))
	return closure, nil
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
