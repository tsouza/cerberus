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
// Deliberately root-only as a RECOGNIZER — but not, since cerberus
// issue #2555, as a NODE. [mixedExpHistogramSetOp] is registered in
// [lowerRoot] directly — NOT inside [lowerExpHistogramValuedShape]'s
// recursive dispatch table the way [expHistogramSetOp] is — so it never
// fires merely because SOME expression nested arbitrarily deep happens
// to look like `(a or b)`; every wrapper that composes over its Mixed
// result (the three exceptions below, the last one added by #2555) has
// its own explicit, deliberate recognition of that composition rather
// than inheriting it implicitly the way a histogram-valued shape
// composes through [lowerExpHistogramValuedShape]'s recursion. That is
// a deliberate scope line, not an oversight: every generic forwarder
// (projectValueOverInner, projectAttributesOverInner) that reads `Value`
// unconditionally would silently drop the histogram on a Mixed result's
// histogram-shaped rows, where Value is only ever the placeholder — see
// [assertValueShapedInput] (histogram_shape_guard.go), which panics if a
// Mixed node reaches [projectValueOverInner] or bypasses
// [projectAttributesOverInner]'s own MixedRowShape branch. `abs(a or b)`
// for a mixed `a or b` still falls through to
// [expHistogramSelectorRouting]'s pre-existing rejection — tracked as an
// open divergence under cerberus issue #2449 in
// test/rejection-parity/catalogue — exactly as it did before this file
// existed.
//
// `sum`/`avg` [by/without] wrapping a mixed `or` DOES compose, since
// cerberus issue #2346: histogram_native_mixed_or_aggregate.go's own
// root-only recognizer ([sumOrAvgOverMixedExpHistogramSetOp]) matches
// that specific AggregateExpr-over-BinaryExpr shape directly and builds
// its own dedicated plan — reusing [lowerMixedExpHistogramOperands]
// below for the two operands, but never routing through THIS function's
// leaf Mixed node or through a generic forwarder. See that file's header
// for the reduction (reference Prometheus drops a sum()/avg() output
// group whose members mix float and histogram samples — this
// recognizer's [mixedExpHistogramMatch] doc reuse covers the shadow
// resolution the wrapped case still needs first).
//
// `label_replace`/`label_join` wrapping a mixed `or` ALSO compose, since
// cerberus issue #2449: histogram_native_mixed_or_label.go's own
// root-only recognizer ([labelCallOverMixedExpHistogramSetOp]) DOES route
// through THIS function's own [lowerMixedExpHistogramSetOp] for the two
// operands, then hands the resulting Mixed node to
// [projectAttributesOverInner] directly — the one generic forwarder that
// has grown a MixedRowShape branch, because a label rewrite touches only
// Attributes and needs no per-payload conditional the way a value
// transform would.
//
// A further vector set operator (`and`/`or`/`unless`) wrapping a mixed
// `or` ALSO composes, since cerberus issue #2555 — but through a THIRD
// mechanism, distinct from both cases above: neither a sibling root-only
// recognizer (the `sum`/`avg` and `label_replace`/`label_join` pattern)
// nor a recursive dispatch-table entry (the way a histogram-valued shape
// composes through [lowerExpHistogramValuedShape]). Instead,
// histogram_native_set_op.go's [lowerVectorSetOpOperand] — the helper
// binary.go's [lowerVectorSetOp] already uses to lower EACH operand of
// an outer `and`/`or`/`unless` — checks [mixedExpHistogramSetOp] on that
// OPERAND expression directly and routes a match through THIS function.
// That is necessary rather than merely sufficient: [lowerVectorSetOp]
// is reachable for an `and`/`or`/`unless` node that is NOT the query
// root (any nesting the generic `lower()` BinaryExpr dispatch reaches),
// so a sibling root-only recognizer registered next to this one in
// [lowerRoot] would never fire for it — the outer set op's own AST node
// is not, itself, `(a or b)` shaped. `and`/`unless` forward the Mixed
// operand's own fourteen columns unchanged (see
// internal/chsql/vector_set_op.go's vectorSetOpCanonicalArmFrag /
// vectorSetOpOutputCols); `or` composes a Mixed operand with a
// Histogram-shaped, Sample-shaped, or another Mixed-shaped other arm
// (mixedVectorSetOpArmFrag) the same way it already composes the
// two-pure-shapes case this function itself builds. See
// [chplan.VectorSetOp.Mixed]'s doc comment for the full picture.
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
// only builds the [chplan.VectorSetOp] naming which side is which. The
// float side may be any of the three row shapes
// internal/chsql's vectorSetOpCanonicalArmFrag doc comment names —
// canonical-shape, matrix RangeWindow, or instant derived-shape
// (cerberus issue #2333) — because that package's
// mixedVectorSetOpArmFrag canonicalises the float arm through the exact
// same vectorSetOpCanonicalQuartetFrags helper the plain-VectorSetOp
// path uses; see this function's shape guard below.
func lowerMixedExpHistogramSetOp(b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if b.ReturnBool {
		return nil, fmt.Errorf("promql: 'bool' modifier is only allowed on comparison binary ops")
	}

	// A windowed/derived float side is accepted here (cerberus issue
	// #2333) and, since cerberus issue #2453, by the sum/avg-wrapped
	// sibling too (histogram_native_mixed_or_aggregate.go, issue #2346) —
	// see lowerMixedExpHistogramOperands's own doc comment.
	histNode, floatNode, histOnLeft, err := lowerMixedExpHistogramOperands(b, s, ctx)
	if err != nil {
		return nil, err
	}

	left, right := floatNode, histNode
	if histOnLeft {
		left, right = histNode, floatNode
	}

	return &chplan.VectorSetOp{
		Left:                 left,
		Right:                right,
		Op:                   chplan.VectorSetOr,
		Match:                mixedExpHistogramMatch(b),
		StepAligned:          ctx.step > 0,
		Mixed:                true,
		MixedHistogramOnLeft: histOnLeft,
		MetricNameColumn:     s.MetricNameColumn,
		AttributesColumn:     s.AttributesColumn,
		TimestampColumn:      s.TimestampColumn,
		ValueColumn:          s.ValueColumn,
	}, nil
}

// lowerMixedExpHistogramOperands lowers b's two operands the way every
// mixed float/histogram `or` lowering needs them — the histogram-valued
// side through its own existing histogram-valued lowering, the
// float-valued side through the ordinary [lower] — and reports which
// side was histogram-valued in the source AST. Split out of
// [lowerMixedExpHistogramSetOp] so histogram_native_mixed_or_aggregate.go's
// `sum`/`avg`-wrapped recognizer (cerberus issue #2346) can build the
// SAME two operand plans without duplicating the histogram-valued /
// float-valued dispatch.
//
// The float side may be any of the three row shapes internal/chsql's
// vectorSetOpCanonicalArmFrag doc comment names — canonical-shape,
// matrix RangeWindow, or instant derived-shape (cerberus issue #2333).
// Both callers accept all three as of cerberus issue #2453: the
// root-only leaf recognizer ([lowerMixedExpHistogramSetOp]) gets the
// matrix/derived canonicalisation for free from internal/chsql's
// mixedVectorSetOpArmFrag / vectorSetOpCanonicalQuartetFrags at
// SQL-emission time, and the sum/avg-wrapped recognizer
// ([lowerSumOrAvgOverMixedExpHistogramSetOp]) now gets the equivalent
// canonicalisation at the chplan level from
// histogram_native_mixed_or_aggregate.go's
// [canonicalizeMixedFloatArmForAgg] before its own aggregation reduction
// ([lowerPlainAggOverMixedFloatArm]) runs.
func lowerMixedExpHistogramOperands(
	b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx,
) (histNode, floatNode chplan.Node, histOnLeft bool, err error) {
	histOnLeft = isExpHistogramValuedShape(b.LHS, s, ctx)
	histExpr, floatExpr := b.LHS, b.RHS
	if !histOnLeft {
		histExpr, floatExpr = b.RHS, b.LHS
	}

	histNode, err = lowerExpHistogramSetOpOperand(histExpr, s, ctx)
	if err != nil {
		return nil, nil, false, err
	}
	floatNode, err = lower(floatExpr, s, ctx)
	if err != nil {
		return nil, nil, false, err
	}
	switch shape := chplan.RowShapeOf(floatNode); shape {
	case chplan.SampleRowShape, chplan.GridWindowRowShape, chplan.ReducedWindowRowShape:
		return histNode, floatNode, histOnLeft, nil
	default:
		return nil, nil, false, fmt.Errorf(
			"promql: 'or' between a float-valued and a histogram-valued operand does not "+
				"support a %s-shaped float operand, which cerberus issue #2333 does not "+
				"cover",
			shape,
		)
	}
}

// mixedExpHistogramMatch translates b's `on`/`ignoring` vector-matching
// clause (or the default, when absent) into the [chplan.VectorMatch] the
// mixed `or`'s own shadow resolution needs — shared by
// [lowerMixedExpHistogramSetOp] and histogram_native_mixed_or_aggregate.go's
// `sum`/`avg`-wrapped lowering, which needs the identical shadow test
// before it ever reaches its own aggregation-level grouping.
func mixedExpHistogramMatch(b *parser.BinaryExpr) chplan.VectorMatch {
	match := chplan.VectorMatch{}
	if b.VectorMatching != nil {
		match.Labels = append([]string(nil), b.VectorMatching.MatchingLabels...)
		match.On = b.VectorMatching.On
	}
	return match
}
