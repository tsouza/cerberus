package chplan

// StructuralOp identifies a TraceQL-style spanset relation.
//
//	StructuralChild     — `A > B`  : A is the direct parent of B  (return B rows)
//	StructuralDescendant — `A >> B`: A is an ancestor of B         (return B rows)
//	StructuralParent    — `A < B`  : A is the direct child of B   (return B rows)
//	StructuralAncestor  — `A << B` : A is a descendant of B        (return B rows)
//	StructuralSibling   — `A ~ B`  : A and B share the same parent (return B rows)
//
// Direct parent-child (`>` / `<`) and sibling (`~`) emit as a single
// INNER JOIN on (TraceID, SpanID/ParentSpanID). Recursive forms
// (`>>` / `<<`) walk the parent chain via a CH `WITH RECURSIVE` CTE
// — see internal/chsql/structural_join.go for the emission strategy
// and docs/roadmap.md § RC3 for the deferred-from-RC2 rationale.
//
// Multi-hop chains (`a > b > c`) already fall out of the binary node
// shape: the lowering produces `StructuralJoin{Left: a, Right:
// StructuralJoin{Left: b, Right: c}}` by recursing into LHS/RHS
// SpansetOperation nodes.
type StructuralOp string

const (
	StructuralChild      StructuralOp = ">"
	StructuralParent     StructuralOp = "<"
	StructuralDescendant StructuralOp = ">>"
	StructuralAncestor   StructuralOp = "<<"
	StructuralSibling    StructuralOp = "~"
)

// StructuralJoin produces the rows from `Right` whose spans satisfy the
// requested structural relation with a span in `Left`. Both sides
// produce span rows from otel_traces (or a derived projection thereof);
// the join key uses TraceID + (Span/Parent)ID columns named in the
// schema.
//
// MaxDepth bounds the parent-chain walk for recursive ops (`>>` / `<<`):
// 0 means unbounded (the CH `WITH RECURSIVE` CTE iterates until the
// fixpoint). Positive values cap the recursion at that many levels —
// useful for cost control on deep traces; the optimizer may set this
// from a configured ceiling. For the direct ops (`>` / `<` / `~`) the
// field is ignored: those always emit a single-level INNER JOIN.
type StructuralJoin struct {
	Left, Right Node
	Op          StructuralOp

	TraceIDColumn      string
	SpanIDColumn       string
	ParentSpanIDColumn string

	MaxDepth int
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
	if j.MaxDepth != o.MaxDepth {
		return false
	}
	return j.Left.Equal(o.Left) && j.Right.Equal(o.Right)
}
