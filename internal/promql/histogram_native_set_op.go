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
