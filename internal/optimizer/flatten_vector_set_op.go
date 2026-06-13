package optimizer

import "github.com/tsouza/cerberus/internal/chplan"

// FlattenVectorSetOp linearises a left-associative chain of the SAME
// associative PromQL vector set-op — `a or b or c …` or
// `a and b and c …` — into one N-ary chplan.NaryVectorSetOp.
//
// The PromQL parser produces a left-leaning binary tree:
// `a or b or c or d` parses as `((a or b) or c) or d`, lowered to
// `VectorSetOp(VectorSetOp(VectorSetOp(a, b), c), d)`. The binary chsql
// emitter renders each nesting level as its own windowed single-pass
// (#88), so a K-arm chain runs K-1 stacked window passes over the
// re-accumulated left subtree. Collapsing the chain to a single
// NaryVectorSetOp lets the emitter scan each arm exactly once under one
// window aggregate — true linearisation.
//
// PARITY: the rewrite changes execution SHAPE, not results. For `or`
// the N-ary "earliest-arm-wins" survival test is byte-identical to the
// nested left-assoc anti-join; for `and` the "present-in-every-arm"
// test is byte-identical to the nested semi-join. See
// internal/chsql/nary_vector_set_op.go for the survival-shape proof.
//
// Only `or` / `and` are flattened — both are associative, so reordering
// the binary nesting into one N-ary node preserves results. `unless` is
// NOT associative (`a unless (b unless c) != (a unless b) unless c`),
// so the rule deliberately skips it; an `unless` chain keeps its binary
// VectorSetOp shape.
//
// The rule fires bottom-up to a fixpoint: the innermost `(a or b)`
// flattens first into NaryVectorSetOp(a, b); the next level
// `(NaryVectorSetOp(a, b) or c)` then absorbs the already-flattened left
// child's arms into NaryVectorSetOp(a, b, c); and so on up the chain.
// Only the LEFT child is absorbed (matching the parser's left-leaning
// nesting) so `or`'s earliest-arm-wins ordering is preserved exactly —
// the right operand is always a single arm in a left-assoc chain.
//
// Two links of a chain are mergeable only when they agree on the
// operator, the match modifier (default / on / ignoring), and every
// canonical Sample column name. A chain whose links disagree on any of
// these isn't a single associative chain and is left untouched.
type FlattenVectorSetOp struct{}

func (FlattenVectorSetOp) Name() string { return "flatten-vector-set-op" }

func (FlattenVectorSetOp) Apply(n chplan.Node) (chplan.Node, bool) {
	binary, ok := n.(*chplan.VectorSetOp)
	if !ok {
		return n, false
	}
	if !flattenableVectorSetOp(binary.Op) {
		return n, false
	}

	// Gather the left-leaning chain's arms in left-to-right source
	// order. The left child is absorbed when it's a same-shaped binary
	// VectorSetOp or an already-flattened NaryVectorSetOp; otherwise it
	// is a single arm.
	arms, absorbed := flattenLeftArms(binary)
	if !absorbed {
		return n, false
	}
	arms = append(arms, binary.Right)

	return &chplan.NaryVectorSetOp{
		Arms:             arms,
		Op:               binary.Op,
		Match:            binary.Match,
		MetricNameColumn: binary.MetricNameColumn,
		AttributesColumn: binary.AttributesColumn,
		TimestampColumn:  binary.TimestampColumn,
		ValueColumn:      binary.ValueColumn,
	}, true
}

// flattenLeftArms returns the arms contributed by the left child of a
// binary VectorSetOp, in left-to-right order, plus whether the child was
// an absorbable same-shaped chain link. When the left child is a
// matching binary VectorSetOp its own (recursively gathered) arms are
// returned; when it's an already-flattened NaryVectorSetOp its arms are
// spliced in; otherwise the left child is a single opaque arm.
func flattenLeftArms(binary *chplan.VectorSetOp) (arms []chplan.Node, absorbed bool) {
	switch left := binary.Left.(type) {
	case *chplan.VectorSetOp:
		if !sameVectorSetOpShape(binary, left.Op, left.Match,
			left.MetricNameColumn, left.AttributesColumn,
			left.TimestampColumn, left.ValueColumn) {
			return []chplan.Node{binary.Left}, true
		}
		inner, _ := flattenLeftArms(left)
		return append(inner, left.Right), true
	case *chplan.NaryVectorSetOp:
		if !sameVectorSetOpShape(binary, left.Op, left.Match,
			left.MetricNameColumn, left.AttributesColumn,
			left.TimestampColumn, left.ValueColumn) {
			return []chplan.Node{binary.Left}, true
		}
		out := make([]chplan.Node, len(left.Arms))
		copy(out, left.Arms)
		return out, true
	default:
		return []chplan.Node{binary.Left}, true
	}
}

// flattenableVectorSetOp reports whether op is one of the two
// associative set-operators the flatten rule linearises. `unless` is
// excluded because it is not associative.
func flattenableVectorSetOp(op chplan.VectorSetOpKind) bool {
	return op == chplan.VectorSetOr || op == chplan.VectorSetAnd
}

// sameVectorSetOpShape reports whether a candidate chain link agrees
// with the root binary node on the operator, match modifier, and every
// canonical Sample column name — the full set of fields the N-ary node
// carries once for the whole chain. Two links that disagree on any of
// these are not part of one associative chain and must not be merged.
func sameVectorSetOpShape(
	root *chplan.VectorSetOp,
	op chplan.VectorSetOpKind,
	match chplan.VectorMatch,
	metricName, attributes, timestamp, value string,
) bool {
	return root.Op == op &&
		root.Match.Equal(match) &&
		root.MetricNameColumn == metricName &&
		root.AttributesColumn == attributes &&
		root.TimestampColumn == timestamp &&
		root.ValueColumn == value
}
