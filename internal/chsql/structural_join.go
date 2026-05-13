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
// This file is grandfathered for the no-Sprintf rule until RC6 R6.6
// (per docs/chsql-audit.md); the recursive helper introduced here
// follows the file's existing strings.Builder + WriteString style so
// it ports cleanly when R6.6 lands.
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
	traceCol := quoteIdent(j.TraceIDColumn)
	spanCol := quoteIdent(j.SpanIDColumn)
	parentCol := quoteIdent(j.ParentSpanIDColumn)

	var rel string
	switch j.Op {
	case chplan.StructuralChild:
		rel = "L." + spanCol + " = R." + parentCol
	case chplan.StructuralParent:
		rel = "L." + parentCol + " = R." + spanCol
	case chplan.StructuralSibling:
		// `A ~ B` — same trace, same parent, distinct spans. The
		// distinct-span clause keeps a row from matching itself when
		// both sides of the spanset select the same span.
		rel = "L." + parentCol + " = R." + parentCol + " AND L." + spanCol + " != R." + spanCol
	default:
		return fmt.Errorf("%w: direct structural op %q", ErrUnsupported, j.Op)
	}

	e.b.WriteString("SELECT R.* FROM ")
	if err := e.emitSubquery(j.Left); err != nil {
		return err
	}
	e.b.WriteString(" AS L INNER JOIN ")
	if err := e.emitSubquery(j.Right); err != nil {
		return err
	}
	e.b.WriteString(" AS R ON L.")
	e.b.WriteString(traceCol)
	e.b.WriteString(" = R.")
	e.b.WriteString(traceCol)
	e.b.WriteString(" AND ")
	e.b.WriteString(rel)
	return nil
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
	traceCol := quoteIdent(j.TraceIDColumn)
	spanCol := quoteIdent(j.SpanIDColumn)
	parentCol := quoteIdent(j.ParentSpanIDColumn)

	// Recursive step direction depends on the operator:
	//   >>  — descendants of L: child's ParentSpanId = closure's SpanId
	//   <<  — ancestors  of L: parent's SpanId = closure's ParentSpanId
	var stepOn string
	switch j.Op {
	case chplan.StructuralDescendant:
		stepOn = "t." + traceCol + " = c." + traceCol +
			" AND t." + parentCol + " = c." + spanCol
	case chplan.StructuralAncestor:
		stepOn = "t." + traceCol + " = c." + traceCol +
			" AND t." + spanCol + " = c." + parentCol
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

	e.b.WriteString("SELECT R.* FROM (WITH RECURSIVE _struct_closure AS (SELECT ")
	e.b.WriteString(traceCol)
	e.b.WriteString(", ")
	e.b.WriteString(spanCol)
	e.b.WriteString(", ")
	e.b.WriteString(parentCol)
	e.b.WriteString(", 0 AS _depth FROM ")
	if err := e.emitSubquery(j.Left); err != nil {
		return err
	}
	e.b.WriteString(" AS _seed UNION ALL SELECT t.")
	e.b.WriteString(traceCol)
	e.b.WriteString(", t.")
	e.b.WriteString(spanCol)
	e.b.WriteString(", t.")
	e.b.WriteString(parentCol)
	e.b.WriteString(", c._depth + 1 FROM ")
	e.b.WriteString(quoteIdent(table))
	e.b.WriteString(" AS t INNER JOIN _struct_closure AS c ON ")
	e.b.WriteString(stepOn)
	if j.MaxDepth > 0 {
		e.b.WriteString(" WHERE c._depth < ")
		e.b.WriteString(strconv.Itoa(j.MaxDepth))
	}
	e.b.WriteString(") SELECT DISTINCT ")
	e.b.WriteString(traceCol)
	e.b.WriteString(", ")
	e.b.WriteString(spanCol)
	e.b.WriteString(" FROM _struct_closure WHERE _depth > 0) AS L INNER JOIN ")
	if err := e.emitSubquery(j.Right); err != nil {
		return err
	}
	e.b.WriteString(" AS R ON L.")
	e.b.WriteString(traceCol)
	e.b.WriteString(" = R.")
	e.b.WriteString(traceCol)
	e.b.WriteString(" AND L.")
	e.b.WriteString(spanCol)
	e.b.WriteString(" = R.")
	e.b.WriteString(spanCol)
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
