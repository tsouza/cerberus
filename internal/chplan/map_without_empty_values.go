package chplan

// MapWithoutEmptyValues is a Map-valued expression that yields the
// input Map with every entry whose value is the empty string dropped.
// Lowers to ClickHouse's `mapFilter((k, v) -> v != '', <map>)`.
//
// Used by PromQL `aggregation by (...)` lowering to canonicalise the
// output label-set: a series whose grouped-by label is absent in
// OTel-CH's Map(String, String) Attributes column produces an
// empty-string slot via the CH Map missing-key default. Prometheus's
// Labels representation canonicalises an empty-valued label to
// "no label" rather than `{job=""}`, so this expression strips those
// slots before the wire layer renders the series identity.
//
// Semantically lossless for real PromQL inputs: per the spec, a label
// with the empty-string value is indistinguishable from the label
// being absent, so dropping empty-valued entries is safe.
type MapWithoutEmptyValues struct {
	Map Expr
}

func (*MapWithoutEmptyValues) exprNode() {}

func (m *MapWithoutEmptyValues) Equal(other Expr) bool {
	o, ok := other.(*MapWithoutEmptyValues)
	if !ok {
		return false
	}
	return m.Map.Equal(o.Map)
}
