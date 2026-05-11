package chplan

import "slices"

// MapWithoutKeys is a Map-valued expression that yields the input Map with
// the named Keys removed. Lowers to ClickHouse's `mapFilter` with a lambda
// rejecting the key set.
//
// Used by PromQL `aggregation without (k1, k2) (expr)`: the group key is
// the Attributes map with the listed labels stripped, so each output group
// preserves every other label of the input series.
type MapWithoutKeys struct {
	Map  Expr
	Keys []string
}

func (*MapWithoutKeys) exprNode() {}

func (m *MapWithoutKeys) Equal(other Expr) bool {
	o, ok := other.(*MapWithoutKeys)
	if !ok {
		return false
	}
	return m.Map.Equal(o.Map) && slices.Equal(m.Keys, o.Keys)
}
