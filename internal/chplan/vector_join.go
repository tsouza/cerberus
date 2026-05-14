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

// VectorCard captures the cardinality modifier on a PromQL vector-vector
// binary expression — the `group_left` / `group_right` family.
//
// Default is one-to-one: each row on each side must match exactly one row
// on the other side under the matching key. Many-to-one (`group_left`)
// allows multiple rows on the left side to share one row on the right;
// one-to-many (`group_right`) mirrors the other way. The chsql emitter
// uses Card to shape its per-side aggregation + cardinality check;
// `group_left(...)` / `group_right(...)` Include labels surface as extra
// columns in the output via `Include`.
type VectorCard int

const (
	// CardOneToOne is the default vector-matching cardinality: each
	// matching-key value must appear at most once on each side. The
	// emitter wraps each side with a runtime throwIf-count check so
	// many-to-many ambiguity surfaces as a CH error rather than a
	// silent cross-product.
	CardOneToOne VectorCard = iota
	// CardManyToOne is the `group_left` shape: the left side is the
	// "many" and the right side is the "one". Include labels copy
	// onto the output from the right side.
	CardManyToOne
	// CardOneToMany is the `group_right` shape: the right side is the
	// "many" and the left side is the "one". Include labels copy onto
	// the output from the left side.
	CardOneToMany
)

// VectorJoin produces the per-pair binary-op result of matching two vector
// inputs by labels. The emitter renders an INNER JOIN of per-series latest
// samples (argMax(Value, TimeUnix) grouped by series-identity columns),
// then evaluates `(L.Value <Op> R.Value)` on the joined rows.
//
// Card + Include capture the `group_left(...)` / `group_right(...)`
// cardinality modifier. Default (CardOneToOne, Include nil) is the
// one-to-one match shape; the emitter enforces uniqueness on the
// matching key at runtime via a throwIf-count guard. With CardManyToOne
// the right side is the "one"; the emitter aggregates the right side by
// the matching key (carrying the Include labels), and the output's
// Attributes map merges the right's Include values onto the left's
// Attributes. CardOneToMany mirrors the orientation.
//
// ReturnBool models PromQL's `bool` modifier on a comparison op. When
// the input op is one of the six comparison ops (=, !=, <, <=, >, >=)
// and ReturnBool is true, the emitter produces `toFloat64(L.Value <Op>
// R.Value)` instead of the default `(L.Value <Op> R.Value)` so the
// join keeps every matched pair (emitting 1.0 / 0.0 per pair) rather
// than letting the comparison drop non-matching rows the way the
// default V-V comparison shape would in Prometheus. The flag has no
// effect for non-comparison ops; lowerings set it only for ops where
// `isComparison(Op)` holds.
type VectorJoin struct {
	Left  Node
	Right Node
	Op    BinaryOp
	Match VectorMatch

	// Card is the cardinality modifier; default CardOneToOne.
	Card VectorCard
	// Include is the `group_left(<labels>)` / `group_right(<labels>)`
	// extra-label list. Nil/empty when no Include was specified.
	Include []string
	// ReturnBool models PromQL's `bool` modifier on a comparison op
	// (`lhs > bool rhs`). Only meaningful when `Op` is a comparison op;
	// the emitter wraps the per-pair binary result with `toFloat64(...)`
	// so every matched pair surfaces as 1.0 / 0.0 rather than being
	// filtered out by the comparison.
	ReturnBool bool

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
	if v.Card != o.Card || !slices.Equal(v.Include, o.Include) {
		return false
	}
	if v.ReturnBool != o.ReturnBool {
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
