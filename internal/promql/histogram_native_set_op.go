package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_set_op.go lowers PromQL's vector set operators —
// `and` / `or` / `unless` — between two histogram-VALUED shapes into a
// histogram-VALUED result (cerberus issue #2324).
//
// Reference Prometheus's set operators never inspect a matched sample's
// VALUE, only its label set: `and` keeps LHS rows whose signature also
// appears on the RHS, `unless` keeps LHS rows whose signature does NOT,
// and `or` unions LHS with the RHS rows whose signature is absent from
// LHS (promql/engine.go's VectorAnd / VectorOr / VectorUnless, all keyed
// on resultMetric's label hash). So — unlike `+`/`-` (histogram_native_
// binop.go, which reconciles two bucket ladders into one) or `==`/`!=`
// (a structural-equality FILTER) — there is no value-combining mechanic
// to design here at all: the existing float-valued lowering
// ([lowerVectorSetOp] in binary.go) already IS a pure label-signature
// join/union that forwards whichever operand's row(s) survive verbatim.
//
// The gap this file closes is a ROUTING gap, not a missing mechanism:
// [lowerVectorSetOp] calls the generic [lower], which sends each operand
// through [lowerVectorSelector] and hits [expHistogramSelectorRouting]'s
// catch-all rejection before ever reaching the join/union machinery.
// This file recognises the shape at [lowerRoot] — mirroring
// [expHistogramHistogramBinop] / [expHistogramDroppingHistogramBinop] —
// and lowers both operands through their own existing histogram-valued
// recognisers, then builds the SAME [chplan.VectorSetOp] the float path
// does, with [chplan.VectorSetOp.Histogram] set so the chsql emitter
// widens the projection to carry the nine Histogram*Column outputs
// through instead of forwarding the meaningless Value placeholder (see
// that field's doc comment).
//
// on()/ignoring() matching modifiers fall out of this "just route it
// through the existing join" design for free: the match key is built
// from Attributes/Timestamp alone (setOpMatchKeyFrags), never Value, so
// nothing here needs the "default matching only" restriction
// [lowerExpHistogramHistogramBinop] imposes for the +/- merge (that
// restriction exists because on()/ignoring() reshapes the merge's own
// GROUP BY key, not because the join itself can't handle it).
// group_left()/group_right() need no handling either: reference
// Prometheus's parser rejects any cardinality modifier on a set operator
// ("set operations must always be many-to-many") before cerberus ever
// sees the query.
//
// A MIXED shape — one side histogram-valued, the other already reduced
// to a float (`histogram_quantile(0.5, m_exp_hist) and m_exp_hist`,
// cerberus issue #2325) — is deliberately NOT recognised by
// [expHistogramSetOp]: this file's mechanism always builds a
// histogram-VALUED result (both operands cap with a
// [chplan.HistogramProjection]-shaped node), but a mixed `and`/`unless`
// forwards whichever side is FLOAT-valued when that's the LHS, so the
// result isn't always histogram-valued. [lowerVectorSetOpOperand], used
// by the generic float-path lowering in binary.go's [lowerVectorSetOp],
// is the mixed case's entry point instead — it reuses
// [lowerExpHistogramSetOpOperand] below for whichever operand actually
// IS histogram-valued and leaves the other to lower through the
// ordinary float path, so the two mechanisms share the one histogram-
// operand lowering helper without duplicating it.

// expHistogramSetOp reports whether expr is a `and`/`or`/`unless` binary
// expression whose BOTH operands are histogram-valued shapes. It is
// registered inside [lowerExpHistogramValuedShape] (see this file's
// header doc), so it is reachable at the query root, nested under a
// further `sum`/`avg`, and as either operand of an outer set op — a
// `VectorSetOp` itself answers histogram-valued once built, which is
// what lets `a or b or c` (an outer set op over an inner one, before the
// optimizer's FlattenVectorSetOp rule linearises the chain) compose the
// same way `sum(rate(...))` does for the range-function shape.
func expHistogramSetOp(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.BinaryExpr, bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
		return nil, false
	}
	b, isBin := unwrapBinaryExpr(expr)
	if !isBin || !b.Op.IsSetOperator() {
		return nil, false
	}
	if !isExpHistogramValuedShape(b.LHS, s, ctx) || !isExpHistogramValuedShape(b.RHS, s, ctx) {
		return nil, false
	}
	return b, true
}

// lowerExpHistogramSetOp lowers the shape [expHistogramSetOp] recognised.
// Both operands defer to their own existing histogram-valued lowering
// unchanged — this function only builds the label-signature join/union
// on top, exactly like [lowerVectorSetOp] does for the float case.
func lowerExpHistogramSetOp(b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if b.ReturnBool {
		return nil, fmt.Errorf("promql: 'bool' modifier is only allowed on comparison binary ops")
	}
	kind, err := promVectorSetOpKind(b.Op)
	if err != nil {
		return nil, err
	}
	hpL, err := lowerExpHistogramSetOpOperand(b.LHS, s, ctx)
	if err != nil {
		return nil, err
	}
	hpR, err := lowerExpHistogramSetOpOperand(b.RHS, s, ctx)
	if err != nil {
		return nil, err
	}

	match := chplan.VectorMatch{}
	if b.VectorMatching != nil {
		match.Labels = append([]string(nil), b.VectorMatching.MatchingLabels...)
		match.On = b.VectorMatching.On
	}

	return &chplan.VectorSetOp{
		Left:             hpL,
		Right:            hpR,
		Op:               kind,
		Match:            match,
		StepAligned:      ctx.step > 0,
		Histogram:        true,
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
		ValueColumn:      s.ValueColumn,
	}, nil
}

// lowerExpHistogramSetOpOperand lowers one set-op operand through its own
// histogram-valued recogniser and asserts the shared histogram-shaped
// output contract every such lowering publishes.
//
// This is deliberately looser than [lowerExpHistogramValuedOperand] (used
// by the +/- merge and the scalar-binop paths), which requires the
// operand to lower to literally a *chplan.HistogramProjection: a set
// op's own operand can itself be a *chplan.VectorSetOp — the inner arm
// of a chain like `a or b or c`, parsed as `(a or b) or c` — and that
// node publishes the exact same thirteen-column contract
// ([chplan.RowShapeOf] answers [chplan.HistogramRowShape] for it, see
// [chplan.VectorSetOp.Histogram]) without BEING a HistogramProjection.
// The join/union machinery below only ever references its operands by
// the canonical Sample column names plus the fixed Histogram*Column
// aliases (see [chplan.VectorSetOp]'s chsql emitter), never by Go type,
// so accepting any histogram-shaped [chplan.Node] here is correct, not
// merely permissive.
func lowerExpHistogramSetOpOperand(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	node, matched, err := lowerExpHistogramValuedShape(expr, s, ctx)
	if err != nil {
		return nil, err
	}
	if !matched {
		return nil, fmt.Errorf("promql: internal invariant violated: exp-histogram set-op operand matched no known histogram-valued shape for %v", expr)
	}
	if chplan.RowShapeOf(node) != chplan.HistogramRowShape {
		return nil, fmt.Errorf(
			"promql: internal invariant violated: exp-histogram set-op operand lowering published %s row shape (%T), want %s",
			chplan.RowShapeOf(node), node, chplan.HistogramRowShape,
		)
	}
	return node, nil
}

// isExpHistogramForwardedThroughSetOp reports whether expr is an
// `and`/`unless` binary expression whose LHS is histogram-valued —
// directly ([isExpHistogramValuedShape]) or, recursively, itself another
// `and`/`unless` forwarding one, so an arbitrarily deep chain like
// `((a and b) and c) and d` is recognised the same as a single-level
// `a and b` (cerberus issue #2571).
//
// `and`/`unless` always forward exactly the LEFT side's rows verbatim —
// the right side's value type never reaches the output, only its label
// signature is used to filter (see [lowerVectorSetOp]'s own doc comment
// and its `Histogram: leftHistogram` return) — so only the LHS matters
// here; a histogram-valued RHS with a non-histogram LHS does NOT make
// the `and`/`unless` itself histogram-valued, and this predicate
// correctly answers false for that shape. `or` is deliberately excluded:
// it unions rather than forwards one side, so a nested `or`'s own
// histogram-valued-ness is decided by [chplan.RowShapeOf] once lowered
// (HistogramRowShape when [expHistogramSetOp] matched both arms,
// MixedRowShape when [mixedExpHistogramSetOp] matched — both already
// recognised by their own existing static checks) rather than by this
// LHS-forwarding predicate.
//
// This is deliberately a SEPARATE predicate from
// [isExpHistogramValuedShape] rather than a new branch inside it:
// [isExpHistogramValuedShape] is paired 1:1 with
// [lowerExpHistogramValuedShape] (see that function's own doc comment on
// why the pairing must stay exhaustive), which has no lowering branch for
// an `and`/`unless` whose RHS is NOT itself histogram-valued — only
// [expHistogramSetOp]'s BOTH-sides-histogram shape is wired there. The
// shape this predicate recognises instead already lowers correctly
// through the ORDINARY [lower] dispatcher: binary.go's [lowerVectorSetOp]
// computes its own Histogram/Mixed output flags from each operand's
// ACTUAL lowered [chplan.RowShapeOf], not from a static recognizer, so an
// `and`/`unless` whose LHS operand recursively resolves to
// HistogramRowShape (via THIS SAME recursion inside
// [lowerVectorSetOpOperand], or directly via
// [lowerExpHistogramSetOpOperand]) already publishes HistogramRowShape
// itself with no extra plumbing — this predicate exists purely so
// [mixedExpHistogramSetOp] / [lowerMixedExpHistogramOperands] can SEE
// that fact before lowering, the same way they already see a directly
// histogram-valued operand via [isExpHistogramValuedShape].
func isExpHistogramForwardedThroughSetOp(expr parser.Expr, s schema.Metrics, ctx lowerCtx) bool {
	b, isBin := unwrapBinaryExpr(expr)
	if !isBin || (b.Op != parser.LAND && b.Op != parser.LUNLESS) {
		return false
	}
	return isExpHistogramValuedShape(b.LHS, s, ctx) || isExpHistogramForwardedThroughSetOp(b.LHS, s, ctx)
}

// isExpHistogramValuedOrForwarded reports whether expr is EITHER of the
// two shapes [lowerExpHistogramValuedOrForwardedOperand] below can lower
// to a histogram-shaped node: directly histogram-valued
// ([isExpHistogramValuedShape]) or an `and`/`unless` forwarding one
// ([isExpHistogramForwardedThroughSetOp]). Shared by
// [mixedExpHistogramSetOp] and [lowerMixedExpHistogramOperands]
// (histogram_native_mixed_or.go) so both stay in sync on which side of a
// mixed `or` is the histogram side — see cerberus issue #2571.
func isExpHistogramValuedOrForwarded(expr parser.Expr, s schema.Metrics, ctx lowerCtx) bool {
	return isExpHistogramValuedShape(expr, s, ctx) || isExpHistogramForwardedThroughSetOp(expr, s, ctx)
}

// lowerExpHistogramValuedOrForwardedOperand lowers expr as the
// histogram-valued side of a mixed `or` construction
// ([lowerMixedExpHistogramOperands], histogram_native_mixed_or.go),
// accepting both shapes [isExpHistogramValuedOrForwarded] recognises:
//
//   - A shape [isExpHistogramValuedShape] already recognised — routed
//     through [lowerExpHistogramSetOpOperand] unchanged, exactly as
//     before cerberus issue #2571.
//   - An `and`/`unless` chain forwarding a histogram-valued LHS
//     ([isExpHistogramForwardedThroughSetOp], cerberus issue #2571) —
//     routed through the ordinary [lower] dispatcher instead:
//     binary.go's [lowerVectorSetOp] already resolves this shape
//     correctly on its own (its Histogram output flag is computed from
//     the ACTUAL lowered [chplan.RowShapeOf] of its Left operand, not a
//     static recognizer — see that function's doc comment), so no
//     dedicated lowering machinery is needed here, only this
//     recognition plus the same row-shape assertion
//     [lowerExpHistogramSetOpOperand] already applies.
func lowerExpHistogramValuedOrForwardedOperand(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if isExpHistogramValuedShape(expr, s, ctx) {
		return lowerExpHistogramSetOpOperand(expr, s, ctx)
	}
	if !isExpHistogramForwardedThroughSetOp(expr, s, ctx) {
		return nil, fmt.Errorf("promql: internal invariant violated: mixed exp-histogram operand matched neither isExpHistogramValuedShape nor isExpHistogramForwardedThroughSetOp for %v", expr)
	}
	node, err := lower(expr, s, ctx)
	if err != nil {
		return nil, err
	}
	if chplan.RowShapeOf(node) != chplan.HistogramRowShape {
		return nil, fmt.Errorf(
			"promql: internal invariant violated: exp-histogram-forwarding set-op operand lowering published %s row shape (%T), want %s",
			chplan.RowShapeOf(node), node, chplan.HistogramRowShape,
		)
	}
	return node, nil
}

// lowerVectorSetOpOperand lowers one operand of a `and`/`or`/`unless`
// vector set op reached through the generic float-path lowering
// (binary.go's [lowerVectorSetOp]) — the entry point for both the plain
// float/float shape and the MIXED float/histogram shape (cerberus issue
// #2325), since the both-histogram shape never reaches it (it is
// intercepted earlier, at [lowerRoot], by [expHistogramSetOp] via
// [lowerExpHistogramValuedShape]).
//
// An operand that resolves to a histogram-VALUED shape — a bare
// exp-histogram selector, sum()/avg() over one, a histogram-valued range
// function, or a nested exp-histogram-valued set op — routes through
// [lowerExpHistogramSetOpOperand] instead of the generic [lower]: every
// one of those shapes is recognised ROOT-reachable-only (see
// [lowerRoot]'s doc comment on why the histogram-valued dispatches live
// there), and [lower] never re-runs that recognition, so without this
// check such an operand would descend into [lowerVectorSelector] and hit
// [expHistogramSelectorRouting]'s catch-all rejection — the exact bug
// #2325 reports.
//
// A drop-family operand — `(demo_latency_exp_hist + 0) or ...` (cerberus
// issue #2534, the set-op sibling of the same gap
// [lowerVectorVectorOperand] (binary.go) closes for arithmetic/comparison
// V-V binops) — reprojects to the canonical empty float quartet via
// [lowerExpHistogramDroppingShape], which already produces exactly the
// Sample row shape a plain float operand would; no set-op-specific
// wrapping is needed.
//
// A MIXED `or` operand — `(demo_latency_exp_hist or
// histogram_quantile(0.5, demo_latency_exp_hist)) and ...` (cerberus issue
// #2555) — routes through [mixedExpHistogramSetOp] /
// [lowerMixedExpHistogramSetOp] (histogram_native_mixed_or.go) instead of
// falling through to the generic [lower] below. Without this check, such
// an operand would descend into [lower]'s own `*parser.BinaryExpr`
// dispatch, which calls [lowerVectorSetOp] directly for a set-operator
// node — reaching a NON-root call of that function, which never gets a
// chance to resolve the inner `(a or b)` as Mixed (that recognition only
// runs from [lowerRoot] and, since this check, from here) and instead
// rejects at [lowerVectorSetOp]'s own `leftHistogram != rightHistogram`
// guard. Checked after the both-histogram case just above (which the
// mixed shape can never match — [mixedExpHistogramSetOp] requires
// exactly one side histogram-valued) and before the drop-family case
// below (a mixed `or`'s own arms are never a drop-family binop), so the
// ordering cannot shadow either sibling.
//
// Every other operand — including a FLOAT-valued function that merely
// reads a histogram selector as its own argument, like
// histogram_quantile(), or an operand with no histogram involvement at
// all — lowers unchanged through [lower]: it already produces the
// canonical Sample row shape [lowerVectorSetOp] needs with no special
// handling.
func lowerVectorSetOpOperand(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if isExpHistogramValuedShape(expr, s, ctx) {
		return lowerExpHistogramSetOpOperand(expr, s, ctx)
	}
	if b, ok := mixedExpHistogramSetOp(expr, s, ctx); ok {
		return lowerMixedExpHistogramSetOp(b, s, ctx)
	}
	if dropped, ok, err := lowerExpHistogramDroppingShape(expr, s, ctx); ok {
		return dropped, err
	}
	return lower(expr, s, ctx)
}
