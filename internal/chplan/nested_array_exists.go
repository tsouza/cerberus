package chplan

// NestedArrayExists is a predicate over a ClickHouse `Nested` column's
// parallel-array layout. TraceQL link / event spanset filters
// (`{ link.span_id = "abc" }`, `{ event.exception.type = "x" }`) lower to
// this shape: the OTel-CH `Links` / `Events` Nested columns expose each
// sub-field as an Array, and we filter by checking whether *any* row in
// the nested array's attribute map matches.
//
// Column is the Nested parent column name ("Links" or "Events" in the
// default OTel-CH schema). SubField is the sub-column under it (always
// "Attributes" today; future expansions may target "Name" for the
// event-name intrinsic). Key is the attribute key looked up inside each
// row's map; Op + Value form the comparison.
//
// The emitter renders this as:
//
//	arrayExists(x -> x[?] <op> ?, `<Column>`.`<SubField>`)
//
// Both Key and Value bind as positional `?` parameters so the values are
// driver-parameterised, not splice into the SQL.
type NestedArrayExists struct {
	Column   string
	SubField string
	Key      string
	Op       BinaryOp
	Value    Expr
}

func (*NestedArrayExists) exprNode() {}

func (n *NestedArrayExists) Equal(other Expr) bool {
	o, ok := other.(*NestedArrayExists)
	if !ok {
		return false
	}
	if n.Column != o.Column || n.SubField != o.SubField || n.Key != o.Key || n.Op != o.Op {
		return false
	}
	if n.Value == nil || o.Value == nil {
		return n.Value == o.Value
	}
	return n.Value.Equal(o.Value)
}
