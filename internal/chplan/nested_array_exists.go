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
	// Presence switches the node from a value comparison (the zero
	// value, PresenceCompare: `x[Key] <Op> <Value>`) to an existence
	// probe — the lowering of TraceQL nil comparisons on event./link.
	// scoped attributes and nested intrinsics. When Presence is not
	// PresenceCompare, Op and Value are unused (Value is nil).
	Presence NestedPresence
}

// NestedPresence enumerates the existence-probe modes of
// NestedArrayExists. Reference Tempo evaluates `<attr> != nil` as
// "the attribute resolves to a non-nil static" (pkg/traceql
// ast_execute.go OpExists) — for event./link. scoped attributes that
// means "at least one Nested-array element carries the key", and for
// the nested intrinsics (event:name / link:traceID / link:spanID,
// required sub-fields of every element) simply "at least one element
// exists".
type NestedPresence int

const (
	// PresenceCompare is the default value-comparison rendering:
	// arrayExists(x -> x[Key] <Op> <Value>, Column.SubField).
	PresenceCompare NestedPresence = iota
	// PresenceHasKey renders `<attr> != nil`:
	// arrayExists(x -> mapContains(x, Key), Column.SubField); with an
	// empty Key the probe degenerates to notEmpty(Column.SubField)
	// (any element at all — the nested-intrinsic form).
	PresenceHasKey
	// PresenceLacksKey renders `<attr> = nil`:
	// arrayExists(x -> not(mapContains(x, Key)), Column.SubField) —
	// at least one element that lacks the key, mirroring reference
	// Tempo's per-element nil sentinel (vparquet4 collectors surface
	// fetched-but-null attribute cells as the "nil" static that
	// OpNotExists matches).
	PresenceLacksKey
)

func (*NestedArrayExists) exprNode() {}

func (n *NestedArrayExists) Equal(other Expr) bool {
	o, ok := other.(*NestedArrayExists)
	if !ok {
		return false
	}
	if n.Column != o.Column || n.SubField != o.SubField || n.Key != o.Key || n.Op != o.Op || n.Presence != o.Presence {
		return false
	}
	if n.Value == nil || o.Value == nil {
		return n.Value == o.Value
	}
	return n.Value.Equal(o.Value)
}
