package chplan

// SetOp identifies a TraceQL spanset set-operation: `A && B` (intersect)
// or `A || B` (union). Spanset boundaries collapse to a flat row stream
// keyed on (TraceID, SpanID); SetIntersect keeps rows that appear on
// both sides, SetUnion concatenates while deduping on the identity key.
type SetOp string

const (
	// SetIntersect — `A && B` — keep rows whose (TraceID, SpanID) appears
	// on both sides. Lowers to an INNER JOIN on the identity key.
	SetIntersect SetOp = "&&"
	// SetUnion — `A || B` — keep rows appearing on either side. Lowers
	// to a UNION ALL of the two subqueries deduped on span identity.
	SetUnion SetOp = "||"
)

// SetOperation models a TraceQL spanset set-op (`A && B`, `A || B`).
// Both child schemas must expose trace and span identity roles. The result
// inherits the left schema, while UNION aligns the right arm positionally.
// TraceIDColumn and SpanIDColumn name that public result identity; they are
// output contracts, not declarations of either child's physical inputs.
type SetOperation struct {
	Left, Right Node
	Op          SetOp

	TraceIDColumn string
	SpanIDColumn  string
}

func (*SetOperation) planNode() {}

func (s *SetOperation) Children() []Node { return []Node{s.Left, s.Right} }

func (s *SetOperation) Equal(other Node) bool {
	o, ok := other.(*SetOperation)
	if !ok {
		return false
	}
	if s.Op != o.Op {
		return false
	}
	if s.TraceIDColumn != o.TraceIDColumn || s.SpanIDColumn != o.SpanIDColumn {
		return false
	}
	return s.Left.Equal(o.Left) && s.Right.Equal(o.Right)
}
