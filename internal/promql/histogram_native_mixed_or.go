package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or.go lowers PromQL's `or` between a
// float-VALUED operand and a histogram-VALUED operand into a genuinely
// mixed-type result (cerberus issue #2330).
//
// Reference Prometheus never rejects this: a `Vector` is a slice of
// `Sample`, and `Sample` itself carries either a float `F` or a native
// histogram `H` — nothing stops one evaluation's result vector holding
// both at once, which is exactly what `or`'s union produces when its two
// arms disagree on value type. `histogram_native_set_op.go`'s
// [expHistogramSetOp] already answers the case where BOTH arms are
// histogram-valued (cerberus issue #2324); this file is its
// exactly-one-side sibling.
//
// Why this is a genuinely different mechanism from #2324's, not a
// parameter on it: `and`/`unless` always forward one side's rows
// verbatim, so their result is homogeneously that side's own value type
// regardless of what the OTHER side's value type is (issue #2325 covers
// teaching them to accept a differently-typed other side; this file does
// not touch `and`/`unless` at all). `or`'s result is different in kind —
// every LHS row PLUS every RHS row absent from LHS — so when the two
// arms disagree on value type the union genuinely needs to carry both
// shapes at once. Cerberus answers that the same way the wire format
// already does (a Prometheus API result item's `value` and `histogram`
// keys are mutually exclusive per item): both arms are widened to the
// same fourteen-column projection — the canonical quartet, the nine
// Histogram*Column outputs, and a trailing per-row discriminator — with
// the arm that doesn't natively have one half of that shape publishing
// placeholders for it (see [chplan.VectorSetOp.Mixed]'s doc comment).
// [chplan.RowShapeOf] answers [chplan.MixedRowShape] for the result;
// internal/chsql/vector_set_op.go's emitMixedVectorSetOp is the SQL
// shape and internal/chclient/cursor.go's shapeSampleMixed is the decode
// side that reads the discriminator per row.
//
// Deliberately root-only. [mixedExpHistogramSetOp] is registered in
// [lowerRoot] directly — NOT inside [lowerExpHistogramValuedShape]'s
// recursive dispatch table the way [expHistogramSetOp] is — so a Mixed
// node is only ever the WHOLE query's plan, never composed under a
// further `sum`/`avg`/label-rewrite/arithmetic wrapper. That is a
// deliberate scope line, not an oversight: every generic forwarder
// (projectValueOverInner, projectAttributesOverInner) reads `Value`
// unconditionally, which is only a placeholder on a Mixed result's
// histogram-shaped rows — see [assertValueShapedInput]
// (histogram_shape_guard.go), which panics if a Mixed node ever DOES
// reach one of those forwarders. Keeping this recognizer root-only is
// what keeps that an impossible state instead of a live hazard: `sum(a
// or b)` / `abs(a or b)` for a mixed `a or b` still fall through to
// [expHistogramSelectorRouting]'s pre-existing rejection, exactly as
// they did before this file existed. Composing a Mixed result under a
// further PromQL wrapper is real follow-on work, not attempted here.
func mixedExpHistogramSetOp(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.BinaryExpr, bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
		return nil, false
	}
	b, isBin := unwrapBinaryExpr(expr)
	if !isBin || b.Op != parser.LOR {
		return nil, false
	}
	lhsHist := isExpHistogramValuedShape(b.LHS, s, ctx)
	rhsHist := isExpHistogramValuedShape(b.RHS, s, ctx)
	if lhsHist == rhsHist {
		// Both histogram-valued is [expHistogramSetOp]'s shape; neither
		// histogram-valued is the plain float [lowerVectorSetOp] path.
		// Either way this recognizer has nothing to add.
		return nil, false
	}
	return b, true
}

// lowerMixedExpHistogramSetOp lowers the shape [mixedExpHistogramSetOp]
// recognised: the histogram-valued side defers to its own existing
// histogram-valued lowering (via [lowerExpHistogramSetOpOperand], the
// same helper #2324's both-histogram path uses, so a nested set-op or
// sum/avg on that side composes identically); the float-valued side
// defers to the ordinary [lower]. Both are then widened to the shared
// fourteen-column Mixed contract by the chsql emitter — this function
// only builds the [chplan.VectorSetOp] naming which side is which.
func lowerMixedExpHistogramSetOp(b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if b.ReturnBool {
		return nil, fmt.Errorf("promql: 'bool' modifier is only allowed on comparison binary ops")
	}

	histOnLeft := isExpHistogramValuedShape(b.LHS, s, ctx)
	histExpr, floatExpr := b.LHS, b.RHS
	if !histOnLeft {
		histExpr, floatExpr = b.RHS, b.LHS
	}

	histNode, err := lowerExpHistogramSetOpOperand(histExpr, s, ctx)
	if err != nil {
		return nil, err
	}
	floatNode, err := lower(floatExpr, s, ctx)
	if err != nil {
		return nil, err
	}
	// The float side must publish the plain canonical quartet: a
	// windowed / already-derived float shape (a matrix rate(), a
	// reduced instant aggregation) needs its OWN reconciliation against
	// the histogram side's row shape that this file does not attempt —
	// scoped out deliberately rather than silently mis-projected. Every
	// shape this rejects today was ALREADY rejected before this file
	// existed (the histogram side hit expHistogramSelectorRouting), so
	// this is not a regression, only a narrower acceptance than the
	// eventual general case.
	if shape := chplan.RowShapeOf(floatNode); shape != chplan.SampleRowShape {
		return nil, fmt.Errorf(
			"promql: 'or' between a float-valued and a histogram-valued operand only "+
				"supports a plain (non-windowed) float side today; got a %s-shaped float "+
				"operand, which cerberus issue #2330 does not yet cover",
			shape,
		)
	}

	match := chplan.VectorMatch{}
	if b.VectorMatching != nil {
		match.Labels = append([]string(nil), b.VectorMatching.MatchingLabels...)
		match.On = b.VectorMatching.On
	}

	left, right := floatNode, histNode
	if histOnLeft {
		left, right = histNode, floatNode
	}

	return &chplan.VectorSetOp{
		Left:                 left,
		Right:                right,
		Op:                   chplan.VectorSetOr,
		Match:                match,
		StepAligned:          ctx.step > 0,
		Mixed:                true,
		MixedHistogramOnLeft: histOnLeft,
		MetricNameColumn:     s.MetricNameColumn,
		AttributesColumn:     s.AttributesColumn,
		TimestampColumn:      s.TimestampColumn,
		ValueColumn:          s.ValueColumn,
	}, nil
}
