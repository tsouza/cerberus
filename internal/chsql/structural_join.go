package chsql

import (
	"fmt"
	"strconv"

	"github.com/tsouza/cerberus/internal/chplan"
)

// verbatim returns a Frag that emits sql as an unquoted token sequence.
// Used in this file for synthetic emitter-chosen identifiers — local
// CTE names (`_struct_closure`, `_seed`), the unquoted `_depth` alias,
// and the qualifier-prefixed `c._depth` / `t.<col>` references that
// the recursive CTE walks — none of which take user input. The
// surrounding shape (alias names, the `_depth` column) is fixed by
// the emitter and pinned by the TraceQL golden fixtures, so the
// unquoted form is deliberate.
//
// This is the in-package writeSQL drop the R6.12.e brief sanctions for
// shapes that don't fit a typed Frag constructor. R6.12.f deletes
// chsql.Raw; the structural-CTE shapes here keep needing one of:
// (a) unquoted alias support on As (the `_depth` literal), (b) an
// unquoted-qualifier helper for `c.<unquoted-alias>` references,
// (c) a literal-int Frag for inline depth bounds. Until those land
// the verbatim escape keeps the typed-Frag rewrite local without
// growing builder.go's public surface for one-off CTE plumbing.
func verbatim(sql string) Frag {
	return func(b *Builder) { b.sb.WriteString(sql) }
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
// Ported to chsql.QueryBuilder at RC6 R6.6: the direct case uses the
// new QueryBuilder.Join slot; the recursive case uses the new
// QueryBuilder.WithRecursive slot for the WITH RECURSIVE … UNION ALL
// CTE shape.
func (e *emitter) emitStructuralJoin(j *chplan.StructuralJoin) error {
	if j.TraceIDColumn == "" || j.SpanIDColumn == "" || j.ParentSpanIDColumn == "" {
		return fmt.Errorf("%w: StructuralJoin column names unset", ErrUnsupported)
	}

	switch j.Op {
	case chplan.StructuralChild, chplan.StructuralParent, chplan.StructuralSibling:
		return e.emitStructuralDirectJoin(j)
	case chplan.StructuralDescendant, chplan.StructuralAncestor:
		return e.emitStructuralRecursive(j)
	default:
		return fmt.Errorf("%w: structural op %q", ErrUnsupported, j.Op)
	}
}

// emitStructuralDirectJoin renders the single-INNER-JOIN form used by
// `>`, `<`, and `~`. Mirrors the M4.2 seed; MaxDepth is ignored here.
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

	sb := NewQuery().
		Select(verbatim("R.*")).
		From(aliasedFrag(leftSub, "L")).
		Join(
			InnerJoin,
			aliasedFrag(rightSub, "R"),
			structuralDirectOnFrag(j, relFrag),
		)
	e.emitSelect(sb)
	return nil
}

// structuralDirectRelFrag returns the relation predicate that pairs
// with the trace-id equality. The leading `L.<TraceID> = R.<TraceID>
// AND` glue is composed in structuralDirectOnFrag — this helper just
// emits the operator-specific clause.
func structuralDirectRelFrag(j *chplan.StructuralJoin) (Frag, error) {
	switch j.Op {
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
func (e *emitter) emitStructuralRecursive(j *chplan.StructuralJoin) error {
	// Recursive step direction depends on the operator:
	//   >>  — descendants of L: child's ParentSpanId = closure's SpanId
	//   <<  — ancestors  of L: parent's SpanId = closure's ParentSpanId
	var stepRel Frag
	switch j.Op {
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

	// Outer SELECT R.* FROM (<closure>) AS L INNER JOIN (<R>) AS R ON L.TraceId = R.TraceId AND L.SpanId = R.SpanId.
	onClause := func(b *Builder) {
		spanIDPairFrag("L", j.TraceIDColumn, "R", j.TraceIDColumn)(b)
		b.writeSQL(" AND ")
		spanIDPairFrag("L", j.SpanIDColumn, "R", j.SpanIDColumn)(b)
	}
	sb := NewQuery().
		Select(verbatim("R.*")).
		From(aliasedFrag(closure.Frag(), "L")).
		Join(InnerJoin, aliasedFrag(rightSub, "R"), onClause)
	e.emitSelect(sb)
	return nil
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
