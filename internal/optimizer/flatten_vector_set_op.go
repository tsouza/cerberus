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
// operator, the match modifier (default / on / ignoring), step
// alignment, the histogram-shaped-arm flag (VectorSetOp.Histogram — see
// its doc comment), and every canonical Sample column name. A chain
// whose links disagree on any of these isn't a single associative chain
// and is left untouched.
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
	// A Mixed VectorSetOr (cerberus issue #2330) carries its
	// asymmetric per-arm shape — which side is histogram-valued — on
	// fields chplan.NaryVectorSetOp has no room for (it only threads
	// Histogram, the symmetric both-arms case, through). Rebuilding one
	// as an N-ary node would silently drop MixedHistogramOnLeft, which
	// downgrades chplan.RowShapeOf's answer from MixedRowShape to the
	// default SampleRowShape — exactly the silent-placeholder-Value
	// hazard [assertValueShapedInput] exists to catch, except
	// [wrapWithSampleProjection] has no guard of its own and would
	// happily wrap the flattened node's Value column, discarding every
	// histogram-shaped row's real payload. A Mixed node never actually
	// chains today (mixedExpHistogramSetOp only matches a bare two-arm
	// BinaryExpr at the query root — see its own doc comment), so
	// skipping it here costs nothing: there is no chain to linearise.
	if binary.Mixed {
		return n, false
	}

	// Gather the left-leaning chain's arms in left-to-right source
	// order. The left child is absorbed when it's a same-shaped binary
	// VectorSetOp or an already-flattened NaryVectorSetOp; otherwise it
	// is a single arm.
	arms := flattenLeftArms(binary)
	arms = append(arms, binary.Right)

	// cerberus issue #2325: a mixed `and`/`unless` between a histogram-
	// valued and a float-valued operand sets VectorSetOp.Histogram from
	// the FORWARDED (left) arm's shape alone, not from both arms
	// agreeing (see lowerVectorSetOp's doc comment) — unlike the
	// homogeneous chains this rule was designed for, where Histogram
	// means every arm publishes a HistogramProjection. NaryVectorSetOp
	// carries a single Histogram bool for its whole UNION-ALL
	// projection (naryVectorSetOpOutputCols), widening every arm's SELECT
	// uniformly from that one flag — flattening a chain with a
	// non-uniform arm would ask a float-shaped arm to project columns
	// its own SELECT never produces, which ClickHouse rejects with code
	// 47 "Unknown expression identifier". Bail and leave the (already
	// correct) nested binary VectorSetOp shape in place whenever any arm
	// disagrees with the flag chosen for the whole chain.
	for _, arm := range arms {
		if (chplan.RowShapeOf(arm) == chplan.HistogramRowShape) != binary.Histogram {
			return n, false
		}
	}

	return &chplan.NaryVectorSetOp{
		Arms:             arms,
		Op:               binary.Op,
		Match:            binary.Match,
		StepAligned:      binary.StepAligned,
		Histogram:        binary.Histogram,
		MetricNameColumn: binary.MetricNameColumn,
		AttributesColumn: binary.AttributesColumn,
		TimestampColumn:  binary.TimestampColumn,
		ValueColumn:      binary.ValueColumn,
	}, true
}

// flattenLeftArms returns the arms contributed by the left child of a
// binary VectorSetOp, in left-to-right order. When the left child is a
// matching binary VectorSetOp its own (recursively gathered) arms are
// returned; when it's an already-flattened NaryVectorSetOp its arms are
// spliced in; otherwise the left child is a single opaque arm.
func flattenLeftArms(binary *chplan.VectorSetOp) (arms []chplan.Node) {
	switch left := binary.Left.(type) {
	case *chplan.VectorSetOp:
		if !sameVectorSetOpShape(binary, left.Op, left.Match, left.StepAligned, left.Histogram,
			left.MetricNameColumn, left.AttributesColumn,
			left.TimestampColumn, left.ValueColumn) {
			return []chplan.Node{binary.Left}
		}
		return append(flattenLeftArms(left), left.Right)
	case *chplan.NaryVectorSetOp:
		if !sameVectorSetOpShape(binary, left.Op, left.Match, left.StepAligned, left.Histogram,
			left.MetricNameColumn, left.AttributesColumn,
			left.TimestampColumn, left.ValueColumn) {
			return []chplan.Node{binary.Left}
		}
		out := make([]chplan.Node, len(left.Arms))
		copy(out, left.Arms)
		return out
	default:
		return []chplan.Node{binary.Left}
	}
}

// flattenableVectorSetOp reports whether op is one of the two
// associative set-operators the flatten rule linearises. `unless` is
// excluded because it is not associative.
func flattenableVectorSetOp(op chplan.VectorSetOpKind) bool {
	return op == chplan.VectorSetOr || op == chplan.VectorSetAnd
}

// sameVectorSetOpShape reports whether a candidate chain link agrees
// with the root binary node on the operator, match modifier, step
// alignment, and every canonical Sample column name — the full set of
// fields the N-ary node carries once for the whole chain. Two links
// that disagree on any of these are not part of one associative chain
// and must not be merged: step alignment selects the match key
// (signature vs (signature, timestamp)), so merging a mixed chain onto
// one flag would silently evaluate some links under the wrong key.
func sameVectorSetOpShape(
	root *chplan.VectorSetOp,
	op chplan.VectorSetOpKind,
	match chplan.VectorMatch,
	stepAligned bool,
	histogram bool,
	metricName, attributes, timestamp, value string,
) bool {
	return root.Op == op &&
		root.Match.Equal(match) &&
		root.StepAligned == stepAligned &&
		root.Histogram == histogram &&
		root.MetricNameColumn == metricName &&
		root.AttributesColumn == attributes &&
		root.TimestampColumn == timestamp &&
		root.ValueColumn == value
}
