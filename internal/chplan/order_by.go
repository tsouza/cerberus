package chplan

// OrderBy sorts the rows of its input by one or more keys. Emits a
// SQL `ORDER BY <key1> [DESC], <key2> [DESC], ...` clause.
//
// The IR carries a small struct per key rather than parallel slices so
// the (expression, direction) pair stays atomic — easier to reason
// about during rewrites.
type OrderBy struct {
	Input Node
	Keys  []OrderKey
}

// OrderKey is one sort key. Desc=false means ASC (the SQL default).
type OrderKey struct {
	Expr Expr
	Desc bool
}

func (*OrderBy) planNode() {}

func (o *OrderBy) Children() []Node { return []Node{o.Input} }

func (o *OrderBy) Equal(other Node) bool {
	p, ok := other.(*OrderBy)
	if !ok {
		return false
	}
	if len(o.Keys) != len(p.Keys) {
		return false
	}
	for i := range o.Keys {
		if o.Keys[i].Desc != p.Keys[i].Desc {
			return false
		}
		if !o.Keys[i].Expr.Equal(p.Keys[i].Expr) {
			return false
		}
	}
	return o.Input.Equal(p.Input)
}
