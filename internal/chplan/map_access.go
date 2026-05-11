package chplan

// MapAccess is a key lookup on a ClickHouse Map column, rendered by the
// emitter as `<Map>[<Key>]`. PromQL label matchers lower into MapAccess on
// the configured Attributes column.
type MapAccess struct {
	Map Expr
	Key Expr
}

func (*MapAccess) exprNode() {}

func (m *MapAccess) Equal(other Expr) bool {
	o, ok := other.(*MapAccess)
	if !ok {
		return false
	}
	return m.Map.Equal(o.Map) && m.Key.Equal(o.Key)
}
