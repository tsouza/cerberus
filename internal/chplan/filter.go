package chplan

// Filter applies a predicate to its input rows, equivalent to a SQL WHERE
// clause. Stacking multiple Filter nodes is permitted; the optimizer fuses
// them via the conjunction-flattening rule.
// Its emitted SELECT is a bare passthrough of every column Input publishes,
// so [Filter.RowType] is always Input's physical schema. Predicate truth can
// refine which rows are live, including selecting none, but never changes the
// output column contract.
type Filter struct {
	Input     Node
	Predicate Expr
}

func (*Filter) planNode() {}

func (f *Filter) Children() []Node { return []Node{f.Input} }

func (f *Filter) Equal(other Node) bool {
	o, ok := other.(*Filter)
	if !ok {
		return false
	}
	return f.Predicate.Equal(o.Predicate) && f.Input.Equal(o.Input)
}
