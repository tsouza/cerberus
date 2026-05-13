package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// emitStructuralJoin renders a TraceQL structural relation as an INNER
// JOIN of two span subqueries. The result projects the right-hand
// span's columns (TraceQL convention: `A > B` returns the matched B
// spans).
//
//	StructuralChild  (`>`):  L.SpanID = R.ParentSpanID  (R's parent matches L)
//	StructuralParent (`<`):  L.ParentSpanID = R.SpanID  (R is L's parent)
//	StructuralSibling (`~`): L.ParentSpanID = R.ParentSpanID AND
//	                        L.SpanID != R.SpanID (same parent, distinct
//	                        spans)
//
// Recursive forms (`>>`, `<<`) need a recursive CTE / multi-level join
// and surface as ErrUnsupported until the M4.2 follow-up.
func (e *emitter) emitStructuralJoin(j *chplan.StructuralJoin) error {
	if j.TraceIDColumn == "" || j.SpanIDColumn == "" || j.ParentSpanIDColumn == "" {
		return fmt.Errorf("%w: StructuralJoin column names unset", ErrUnsupported)
	}

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
	case chplan.StructuralDescendant, chplan.StructuralAncestor:
		// `>>` / `<<` need a recursive walk through the parent chain —
		// CH's WITH RECURSIVE syntax or a bounded-depth UNION of
		// self-joins. Both want CH-integration testing that's hard to
		// land without a live cluster; deferred to RC3 alongside the
		// optimizer rewrite. Direct parent-child `>` and `<` work today.
		return fmt.Errorf("%w: structural op %q — recursive `>>` / `<<` deferred to RC3", ErrUnsupported, j.Op)
	default:
		return fmt.Errorf("%w: structural op %q", ErrUnsupported, j.Op)
	}

	e.b.WriteString("SELECT R.* FROM ")
	if err := e.emitSubquery(j.Left); err != nil {
		return err
	}
	e.b.WriteString(" AS L INNER JOIN ")
	if err := e.emitSubquery(j.Right); err != nil {
		return err
	}
	fmt.Fprintf(&e.b, " AS R ON L.%s = R.%s AND %s", traceCol, traceCol, rel)
	return nil
}
