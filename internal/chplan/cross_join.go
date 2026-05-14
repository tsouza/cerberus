package chplan

// CrossJoin is the unconditional Cartesian product of Left and Right.
// The emitter renders it as `SELECT * FROM (<Left>) CROSS JOIN
// (<Right>)`; output rows expose the union of both sides' columns.
//
// Used by the range-mode `absent(...)` lowering to fan a single-row
// count-check across the StepGrid's anchor column. The general shape
// is intentionally narrow — neither side is allowed to reference the
// other's columns, no join key — so the emit-time SQL stays a bare
// `CROSS JOIN` with no `ON` predicate. Use [VectorJoin] /
// [StructuralJoin] when a real join condition exists.
type CrossJoin struct {
	Left  Node
	Right Node
}

func (*CrossJoin) planNode() {}

func (c *CrossJoin) Children() []Node { return []Node{c.Left, c.Right} }

func (c *CrossJoin) Equal(other Node) bool {
	o, ok := other.(*CrossJoin)
	if !ok {
		return false
	}
	return c.Left.Equal(o.Left) && c.Right.Equal(o.Right)
}
