package chplan

// VectorSetOpKind identifies a PromQL vector set-operator: `and` (semi-
// join over label signatures), `or` (union with anti-right), and
// `unless` (anti-join). Parameterising the node by kind keeps the
// three lowerings on a single typed path so optimizer rules and the
// emitter can dispatch on the same shape.
type VectorSetOpKind string

const (
	// VectorSetAnd is PromQL's `A and B`: keep samples from A whose
	// match-key signature appears at least once in B.
	VectorSetAnd VectorSetOpKind = "and"
	// VectorSetOr is PromQL's `A or B`: keep all samples from A, plus
	// samples from B whose match-key signature does not appear in A.
	VectorSetOr VectorSetOpKind = "or"
	// VectorSetUnless is PromQL's `A unless B`: keep samples from A
	// whose match-key signature does NOT appear in B.
	VectorSetUnless VectorSetOpKind = "unless"
)

// VectorSetOp models a PromQL vector set-operator binary expression.
//
// The set ops are inherently many-to-many on labels: each sample on the
// LHS / RHS is matched against the opposite side's match-key signature
// — defined by the full Attributes map (default), or by the listed
// labels (`on(...)` / `ignoring(...)`). PromQL's parser rejects
// `group_left` / `group_right` on set ops ("set operations must always
// be many-to-many"), so the node deliberately omits a Card slot — the
// chsql emitter assumes many-to-many.
//
// The result carries the LHS sample values verbatim for `and` / `unless`;
// for `or` the LHS rows plus the LHS-anti-matched RHS rows are unioned.
// Output Attributes preserves each surviving row's full Attributes —
// set ops never derive a new sample, they filter / union existing ones,
// so `__name__` flows through unchanged (unlike arithmetic / comparison
// V-V binops which always drop the metric name).
type VectorSetOp struct {
	Left  Node
	Right Node
	Op    VectorSetOpKind
	Match VectorMatch

	MetricNameColumn string
	AttributesColumn string
	TimestampColumn  string
	ValueColumn      string
}

func (*VectorSetOp) planNode() {}

func (s *VectorSetOp) Children() []Node { return []Node{s.Left, s.Right} }

func (s *VectorSetOp) Equal(other Node) bool {
	o, ok := other.(*VectorSetOp)
	if !ok {
		return false
	}
	if s.Op != o.Op || !s.Match.Equal(o.Match) {
		return false
	}
	if s.MetricNameColumn != o.MetricNameColumn ||
		s.AttributesColumn != o.AttributesColumn ||
		s.TimestampColumn != o.TimestampColumn ||
		s.ValueColumn != o.ValueColumn {
		return false
	}
	return s.Left.Equal(o.Left) && s.Right.Equal(o.Right)
}
