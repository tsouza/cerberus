package chplan

// StructuralOp identifies a TraceQL-style spanset relation.
//
//	StructuralChild     — `A > B`  : A is the direct parent of B  (return B rows)
//	StructuralDescendant — `A >> B`: A is an ancestor of B         (return B rows)
//	StructuralParent    — `A < B`  : A is the direct child of B   (return B rows)
//	StructuralAncestor  — `A << B` : A is a descendant of B        (return B rows)
//
// `>>` and `<<` need recursive CTE / multi-level joins; the seed
// (M4.2) emits `>` and `<` and rejects the recursive forms with a
// pointer to the M4.2 follow-up.
type StructuralOp string

const (
	StructuralChild      StructuralOp = ">"
	StructuralParent     StructuralOp = "<"
	StructuralDescendant StructuralOp = ">>"
	StructuralAncestor   StructuralOp = "<<"
)

// StructuralJoin produces the rows from `Right` whose spans satisfy the
// requested structural relation with a span in `Left`. Both sides
// produce span rows from otel_traces (or a derived projection thereof);
// the join key uses TraceID + (Span/Parent)ID columns named in the
// schema.
type StructuralJoin struct {
	Left, Right Node
	Op          StructuralOp

	TraceIDColumn      string
	SpanIDColumn       string
	ParentSpanIDColumn string
}

func (*StructuralJoin) planNode() {}

func (j *StructuralJoin) Children() []Node { return []Node{j.Left, j.Right} }

func (j *StructuralJoin) Equal(other Node) bool {
	o, ok := other.(*StructuralJoin)
	if !ok {
		return false
	}
	if j.Op != o.Op {
		return false
	}
	if j.TraceIDColumn != o.TraceIDColumn ||
		j.SpanIDColumn != o.SpanIDColumn ||
		j.ParentSpanIDColumn != o.ParentSpanIDColumn {
		return false
	}
	return j.Left.Equal(o.Left) && j.Right.Equal(o.Right)
}
