package chplan

import "slices"

// VectorMatch describes how two vector inputs join on labels in a PromQL
// binary expression. `Labels` is empty + `On` false → default matching on
// the full Attributes map. `On` true (with Labels non-empty) → match only
// on the listed labels. `On` false (with Labels non-empty) → match on
// everything except the listed labels (`ignoring(...)`).
type VectorMatch struct {
	Labels []string
	On     bool
}

// Equal reports structural equality with another VectorMatch.
func (m VectorMatch) Equal(other VectorMatch) bool {
	return m.On == other.On && slices.Equal(m.Labels, other.Labels)
}

// VectorJoin produces the per-pair binary-op result of matching two vector
// inputs by labels. The emitter renders an INNER JOIN of per-series latest
// samples (argMax(Value, TimeUnix) grouped by series-identity columns),
// then evaluates `(L.Value <Op> R.Value)` on the joined rows.
//
// `group_left` / `group_right` (many-to-one / one-to-many) cardinality
// modifiers are intentionally out of scope here and surface as a lowering
// error from internal/promql.
type VectorJoin struct {
	Left  Node
	Right Node
	Op    BinaryOp
	Match VectorMatch

	MetricNameColumn string
	AttributesColumn string
	TimestampColumn  string
	ValueColumn      string
}

func (*VectorJoin) planNode() {}

func (v *VectorJoin) Children() []Node { return []Node{v.Left, v.Right} }

func (v *VectorJoin) Equal(other Node) bool {
	o, ok := other.(*VectorJoin)
	if !ok {
		return false
	}
	if v.Op != o.Op || !v.Match.Equal(o.Match) {
		return false
	}
	if v.MetricNameColumn != o.MetricNameColumn ||
		v.AttributesColumn != o.AttributesColumn ||
		v.TimestampColumn != o.TimestampColumn ||
		v.ValueColumn != o.ValueColumn {
		return false
	}
	return v.Left.Equal(o.Left) && v.Right.Equal(o.Right)
}
