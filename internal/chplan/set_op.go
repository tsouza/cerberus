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
	// to a UNION DISTINCT of the two subqueries.
	SetUnion SetOp = "||"
)

// SetOperation models a TraceQL spanset set-op (`A && B`, `A || B`).
// Both sides produce span rows from the same traces table; the result
// is keyed on (TraceIDColumn, SpanIDColumn) for dedup / intersect.
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
