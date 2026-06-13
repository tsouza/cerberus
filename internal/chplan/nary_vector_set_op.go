package chplan

// NaryVectorSetOp is the linearised form of a left-associative chain of
// the SAME associative PromQL vector set-operator — `a or b or c or …`
// or `a and b and c and …`. The PromQL parser produces a left-leaning
// binary tree (`((a or b) or c) or d`), which the binary VectorSetOp
// emitter renders as one windowed pass PER nesting LEVEL. The
// optimizer's FlattenVectorSetOp rule collapses such a chain into a
// single NaryVectorSetOp carrying every arm in left-to-right order, and
// the chsql emitter renders it as ONE windowed single-pass over the
// arms' UNION ALL — so a K-arm chain scans each arm exactly once
// instead of re-wrapping the accumulated left subtree K-1 times.
//
// Only the two associative set-ops are representable here:
//
//   - VectorSetOr  (`or`)  — union with later-arm anti-join. Each
//     signature is owned by its EARLIEST contributing arm; that arm's
//     rows survive, every later arm's rows for the same signature drop.
//   - VectorSetAnd (`and`) — semi-join intersection. Arm-0's rows
//     survive iff their signature appears in EVERY arm.
//
// `unless` is deliberately NOT representable: `a unless b unless c`
// is `(a unless b) unless c`, and `unless` is not associative
// (`a unless (b unless c)` differs), so flattening it would change
// results. The flatten rule only ever targets `or` / `and`, and Op is
// validated to one of those two by the emitter.
//
// The match key (Match / AttributesColumn) and the canonical Sample
// column names are shared across all arms — the flatten rule only
// collapses a chain whose links agree on every one of these, so the
// single N-ary node carries one copy. Arms keep the same per-arm output
// shape the binary node required (each arm canonicalised to the 4-column
// Sample tuple at emit time).
type NaryVectorSetOp struct {
	// Arms lists the chain's operands in stable left-to-right source
	// order. A well-formed NaryVectorSetOp always has at least two arms
	// — the flatten rule never mints a degenerate single-arm node, and
	// the emitter rejects fewer than two.
	Arms []Node
	// Op is the associative set-operator the whole chain shares. Only
	// VectorSetOr / VectorSetAnd are valid; VectorSetUnless is rejected
	// by the emitter because `unless` is not associative.
	Op    VectorSetOpKind
	Match VectorMatch

	MetricNameColumn string
	AttributesColumn string
	TimestampColumn  string
	ValueColumn      string
}

func (*NaryVectorSetOp) planNode() {}

// Children returns the per-arm subtrees in stable left-to-right order,
// matching the Node depth-first visitor contract.
func (s *NaryVectorSetOp) Children() []Node {
	out := make([]Node, len(s.Arms))
	copy(out, s.Arms)
	return out
}

// Equal reports positional structural equality with another
// NaryVectorSetOp. Arm order is significant: `or` linearisation keeps
// earliest-arm-wins semantics, so re-ordering arms changes the emitted
// result, mirroring the order-sensitive equality every other plan node
// uses.
func (s *NaryVectorSetOp) Equal(other Node) bool {
	o, ok := other.(*NaryVectorSetOp)
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
	if len(s.Arms) != len(o.Arms) {
		return false
	}
	for i := range s.Arms {
		if !s.Arms[i].Equal(o.Arms[i]) {
			return false
		}
	}
	return true
}
