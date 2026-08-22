package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_aggregate.go lowers `sum`/`avg` [by/without]
// wrapping a mixed float/histogram `or` — cerberus issue #2346, the
// WHERE-recognized follow-on histogram_native_mixed_or.go's own doc
// comment named as deliberately unattempted: that file's leaf recognizer
// ([mixedExpHistogramSetOp] / [lowerMixedExpHistogramSetOp]) is
// registered ONLY at the query root, so `sum(a or b)` for a mixed `a or
// b` fell through [lowerExpHistogramValuedShape] (whose
// mergeableExpHistogramAggregate case tries and fails to resolve the
// mixed operand as purely histogram-valued) all the way to
// internal/promql/binary.go's lowerVectorSetOp, which rejects any `or`
// whose two operands disagree on histogram-valuedness outright.
//
// This file adds a SEPARATE, sibling root-only recognizer for the
// specific `sum`/`avg` composition instead of widening the leaf
// recognizer's own registration: [mixedExpHistogramSetOp] still never
// nests under anything (its doc comment's "impossible state" argument
// for [assertValueShapedInput] stays true), and every wrapper OTHER than
// a direct `sum`/`avg` around a mixed `or` (`abs(a or b)`, a further
// binop, a windowed float arm per cerberus issue #2333) keeps falling
// through to the pre-existing rejection unchanged.
//
// Reference Prometheus semantics (promql/engine.go's aggregation()):
// `sum()`/`avg()` group their input by the `by`/`without` clause (the
// whole vector into one group when neither is given), and a GROUP whose
// members disagree on value type — some float, some histogram, AT THE
// SAME evaluation timestamp — is dropped from the result entirely (a
// `MixedFloatsHistogramsAggWarning` annotation, not a query error) while
// every group whose members agree on type reduces normally (float sum,
// or the native-histogram FloatHistogram.Add merge). This file answers
// exactly that reduction, built entirely from existing machinery:
//
//  1. [lowerMixedExpHistogramOperands] lowers the `or`'s two operands
//     exactly as the leaf recognizer does — one histogram-valued, one
//     plain float-valued, now including a windowed/derived float side
//     (cerberus issue #2453 taught [canonicalizeFloatArmForAgg] the
//     matrix/derived-shape canonicalisation
//     [lowerPlainAggOverMixedFloatArm] needs to reduce one; issue #2333
//     shipped the identical acceptance for the root-only leaf recognizer
//     first).
//  2. The `or`'s OWN shadow rule (every row of the side that's LHS in
//     the source AST, plus only the RHS rows whose label signature is
//     absent from LHS) is resolved with a [chplan.VectorSetOp] UNLESS —
//     the identical mixed-type semi/anti-join #2337 already ships for
//     `and`/`unless` between a histogram-valued and a float-valued
//     operand, reused here rather than re-derived.
//  3. Each of the two shadow-resolved arms is reduced independently by
//     its own existing SUM/AVG machinery: the histogram arm through
//     [lowerExpHistogramSumOrAvgOverPlan] (the SAME reduction
//     `sum(rate(m_exp_hist[5m]))` already uses), the float arm through
//     [lowerPlainAggOverMixedFloatArm] (this file — the CH-native
//     `sum`/`avg` an ordinary float aggregation already uses, applied to
//     an arbitrary already-lowered plan instead of a freshly lowered
//     selector).
//  4. The two per-group reductions are combined with the reference
//     "drop a group present on both sides" rule via TWO MORE VectorSetOp
//     UNLESSes (histogram-only groups, float-only groups) and a final
//     Mixed `or` union — reusing the plain (non-mixed) UNLESS's
//     label-signature-and-timestamp matching and the leaf mixed-`or`
//     union's [chplan.VectorSetOp.Mixed] contract respectively, neither
//     of which needed a single new emitter line. A group present on
//     both sides never reaches EITHER unless's output, so it is dropped
//     exactly where reference drops it; a group present on only one
//     side survives through that side's own reduction unchanged.
//
// No new chplan or chsql surface: every node this file builds is a
// [chplan.VectorSetOp], a [chplan.Aggregate], or a [chplan.Project],
// composed the same way their existing callers already do.

// sumOrAvgOverMixedExpHistogramSetOp reports whether expr is `sum`/`avg`
// [by/without] directly wrapping a mixed float/histogram `or`
// ([mixedExpHistogramSetOp]'s own shape, applied to the aggregand
// instead of the whole query). Mirrors [mergeableExpHistogramAggregate]'s
// own `unwrapAggregateExpr` + mergeable-op + no-param shape, since this
// is that same aggregate wrapper family — just over an operand
// [isExpHistogramValuedShape] alone cannot resolve.
func sumOrAvgOverMixedExpHistogramSetOp(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.AggregateExpr, *parser.BinaryExpr, bool) {
	agg, ok := unwrapAggregateExpr(expr)
	if !ok || !expHistogramAggOpIsMergeable(agg.Op) || agg.Param != nil {
		return nil, nil, false
	}
	b, ok := mixedExpHistogramSetOp(agg.Expr, s, ctx)
	if !ok {
		return nil, nil, false
	}
	return agg, b, true
}

// lowerSumOrAvgOverMixedExpHistogramSetOp lowers the shape
// [sumOrAvgOverMixedExpHistogramSetOp] recognised. See this file's
// header for the four-stage reduction.
func lowerSumOrAvgOverMixedExpHistogramSetOp(agg *parser.AggregateExpr, b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if b.ReturnBool {
		return nil, fmt.Errorf("promql: 'bool' modifier is only allowed on comparison binary ops")
	}

	// A windowed float side (cerberus issue #2453) is safe here because
	// every floatForAgg branch below is canonicalised to
	// chplan.SampleRowShape before it reaches
	// [lowerPlainAggOverMixedFloatArm]: the mixedOrShadowUnless-wrapped
	// branch gets it as a side effect of that VectorSetOp's own arm
	// canonicalisation (vectorSetOpCanonicalArmFrag /
	// vectorSetOpCanonicalQuartetFrags, internal/chsql), and the
	// unwrapped branch (float side kept unconditionally per the `or`
	// shadow rule) gets it explicitly from [canonicalizeFloatArmForAgg]
	// below, this file's own promql-level mirror of that same
	// canonicalisation.
	histNode, floatNode, histOnLeft, err := lowerMixedExpHistogramOperands(b, s, ctx)
	if err != nil {
		return nil, err
	}

	// Resolve the `or`'s own shadow rule BEFORE aggregating: the side
	// that is LHS in the source AST keeps every row; the RHS side keeps
	// only the rows whose label signature has no match on the LHS side.
	// This is the identical mixed-type UNLESS #2337 already ships for
	// `and`/`unless` between a histogram-valued and a float-valued
	// operand — reused verbatim rather than re-derived.
	//
	// Only the FILTERED side passes through mixedOrShadowUnless's own
	// VectorSetOp, which canonicalises it to chplan.SampleRowShape as a
	// side effect (see this function's own doc comment above). The side
	// kept unconditionally never goes through that node, so a windowed
	// float arm on THAT side needs [canonicalizeFloatArmForAgg] applied
	// explicitly before lowerPlainAggOverMixedFloatArm's Aggregate can
	// reference its Timestamp/Attributes columns by name.
	orMatch := mixedExpHistogramMatch(b)
	histForAgg, floatForAgg := histNode, floatNode
	if histOnLeft {
		floatForAgg = mixedOrShadowUnless(floatNode, histNode, false, orMatch, s, ctx)
	} else {
		histForAgg = mixedOrShadowUnless(histNode, floatNode, true, orMatch, s, ctx)
		floatForAgg = canonicalizeFloatArmForAgg(floatNode, s)
	}

	histBranch, err := lowerExpHistogramSumOrAvgOverPlan(agg, histForAgg, s)
	if err != nil {
		return nil, err
	}
	floatBranch, err := lowerPlainAggOverMixedFloatArm(agg, floatForAgg, s, ctx)
	if err != nil {
		return nil, err
	}

	return combineMixedAggregateBranches(histBranch, floatBranch, s, ctx), nil
}

// mixedOrShadowUnless builds the [chplan.VectorSetOp] UNLESS that keeps
// left's rows whose label signature (per match) is absent from right —
// the RHS-of-`or` half of the mixed `or`'s own shadow rule. leftIsHistogram
// names which of left/right is the histogram-valued side, matching
// [chplan.VectorSetOp.Histogram]'s existing "the arm actually publishing
// the nine Histogram*Column outputs" contract.
func mixedOrShadowUnless(left, right chplan.Node, leftIsHistogram bool, match chplan.VectorMatch, s schema.Metrics, ctx lowerCtx) chplan.Node {
	return &chplan.VectorSetOp{
		Left:             left,
		Right:            right,
		Op:               chplan.VectorSetUnless,
		Match:            match,
		StepAligned:      ctx.step > 0,
		Histogram:        leftIsHistogram,
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
		ValueColumn:      s.ValueColumn,
	}
}

// combineMixedAggregateBranches applies reference Prometheus's
// sum()/avg() mixed-group rule to the two independently-reduced
// branches: a (grouping-key, timestamp) present on BOTH histBranch and
// floatBranch is a group whose members disagreed on value type, which
// reference drops entirely (a MixedFloatsHistogramsAggWarning
// annotation, not an error); a key present on only one side survives
// through that side's own reduction unchanged.
//
// Built from three more [chplan.VectorSetOp] nodes, all reusing existing
// mechanisms unchanged: two plain UNLESSes compute "histogram-only
// groups" and "float-only groups" (matching on the FULL reconstructed
// Attributes — the aggregation's own output identity — not the `or`'s
// on()/ignoring() clause, which governed shadow resolution one stage
// earlier and has no further role here), and the leaf mixed-`or` union
// ([chplan.VectorSetOp.Mixed], histogram_native_mixed_or.go) combines
// them. The two UNLESS outputs are disjoint by construction (a key
// surviving one side's UNLESS cannot also survive the other's), so the
// union's own shadow test is a structural no-op and the arbitrary
// Left/Right choice below doesn't affect the result.
func combineMixedAggregateBranches(histBranch, floatBranch chplan.Node, s schema.Metrics, ctx lowerCtx) chplan.Node {
	groupMatch := chplan.VectorMatch{}
	histOnly := mixedOrShadowUnless(histBranch, floatBranch, true, groupMatch, s, ctx)
	floatOnly := mixedOrShadowUnless(floatBranch, histBranch, false, groupMatch, s, ctx)

	return &chplan.VectorSetOp{
		Left:                 histOnly,
		Right:                floatOnly,
		Op:                   chplan.VectorSetOr,
		Match:                groupMatch,
		StepAligned:          ctx.step > 0,
		Mixed:                true,
		MixedHistogramOnLeft: true,
		MetricNameColumn:     s.MetricNameColumn,
		AttributesColumn:     s.AttributesColumn,
		TimestampColumn:      s.TimestampColumn,
		ValueColumn:          s.ValueColumn,
	}
}

// canonicalizeFloatArmForAgg widens arm — the "keep unconditionally"
// side of a mixed `or`'s own shadow rule, i.e. the side
// [lowerSumOrAvgOverMixedExpHistogramSetOp] never passes through
// [mixedOrShadowUnless]'s own VectorSetOp — to the canonical
// chplan.SampleRowShape quartet (MetricName, Attributes, Timestamp,
// Value), so [lowerPlainAggOverMixedFloatArm]'s Aggregate can reference
// s.TimestampColumn / s.AttributesColumn by name regardless of whether
// arm is a raw windowed/derived shape.
//
// This is the promql-level mirror of internal/chsql's
// vectorSetOpCanonicalArmFrag / vectorSetOpCanonicalQuartetFrags — the
// exact canonicalisation the root-only leaf recognizer
// ([lowerMixedExpHistogramSetOp], issue #2333) gets for free from the
// chsql emitter of its own VectorSetOp, and the SAME one the OTHER
// (filtered) arm here gets for free from mixedOrShadowUnless's own
// VectorSetOp. Split out as its own chplan.Project — rather than
// re-deriving that chsql logic — because this arm never passes through
// any VectorSetOp at all, so nothing else canonicalises it. Two of the
// three cases that helper distinguishes apply here:
//
//   - chplan.SampleRowShape (already canonical — a plain selector, or
//     anything wrapAggregateForSample already re-projected) — returned
//     unchanged.
//   - chplan.GridWindowRowShape (a matrix RangeWindow / RangeWindowGridNative,
//     e.g. `rate(m[5m])` in range mode) — its own SELECT already
//     exposes the grid anchor under the schema's TimestampColumn name
//     directly (chplan.RowShapeOf's own doc comment), so Timestamp is a
//     passthrough reference, matching vectorSetOpArmTimestampCol's
//     identical no-alias-needed case for an arm whose own TimestampColumn
//     field is the same schema name.
//   - chplan.ReducedWindowRowShape (an instant-reduced window, e.g.
//     `rate(m[5m])` in instant mode) — no per-row timestamp exists at
//     all, so Timestamp is synthesised via chplan.NowNanoMinusStaleness,
//     byte-for-byte the same synthesized anchor
//     vectorSetOpSynthesizedAnchorFrag renders for the identical case.
//
// Attributes and Value are always passed through unchanged: every
// windowed/derived shape this family of lowerings produces preserves the
// full Attributes map under the schema's own column name as its GroupBy
// key (the same assumption vectorSetOpCanonicalQuartetFrags's
// unconditional `Col(s.AttributesColumn)` already relies on), and Value
// is the schema-named reduced/per-row value column on every shape alike.
func canonicalizeFloatArmForAgg(arm chplan.Node, s schema.Metrics) chplan.Node {
	if chplan.RowShapeOf(arm) == chplan.SampleRowShape {
		return arm
	}

	tsExpr := chplan.NowNanoMinusStaleness()
	if chplan.RowShapeOf(arm) == chplan.GridWindowRowShape {
		tsExpr = &chplan.ColumnRef{Name: s.TimestampColumn}
	}

	return &chplan.Project{
		Input: arm,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: tsExpr, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}
}

// lowerPlainAggOverMixedFloatArm reduces input — the shadow-resolved
// float-valued arm of a mixed `or` — with the SAME `sum`/`avg` CH-native
// aggregate an ordinary (non-histogram) PromQL aggregation uses
// ([aggregateGroupBy], [buildAggFunc], [promAggregateAttributesExpr]),
// grouped by the evaluation Timestamp in addition to the user's
// `by`/`without` labels. Callers must canonicalise input to
// chplan.SampleRowShape first (via mixedOrShadowUnless's own VectorSetOp
// or, for the arm that skips it, [canonicalizeFloatArmForAgg]) — this
// function's GroupBy/Projections reference s.TimestampColumn and
// s.AttributesColumn by name, which only a canonical-shape input
// exposes.
//
// Grouping by Timestamp unconditionally (not only when ctx.step > 0)
// mirrors [lowerExpHistogramSumOrAvgOverPlan]'s identical choice for the
// histogram sibling this function's output is matched against in
// [combineMixedAggregateBranches]: cerberus's canonical TimeUnix column
// is already a synthesised, per-step-uniform anchor rather than a raw
// per-row sample timestamp (see chplan.RangeLWRSampleTimestampColumn's
// doc comment — the raw timestamp is a distinct, opt-in column no PromQL
// lowering in this family requests), so grouping by it is a no-op in
// instant mode and the correct per-step key in range mode, on both
// branches identically.
func lowerPlainAggOverMixedFloatArm(agg *parser.AggregateExpr, input chplan.Node, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	labelGroupBy, err := aggregateGroupBy(agg, s)
	if err != nil {
		return nil, err
	}
	labelAliases := groupKeyAliases(len(labelGroupBy))

	aggFunc, err := buildAggFunc(agg, s, ctx)
	if err != nil {
		return nil, err
	}

	groupBy := append([]chplan.Expr{&chplan.ColumnRef{Name: s.TimestampColumn}}, labelGroupBy...)
	groupByAliases := append([]string{s.TimestampColumn}, labelAliases...)
	merged := &chplan.Aggregate{
		Input:              input,
		GroupBy:            groupBy,
		GroupByAliases:     groupByAliases,
		AggFuncs:           []chplan.AggFunc{aggFunc},
		DropEmptyOnNoGroup: true,
	}

	return &chplan.Project{
		Input: merged,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: promAggregateAttributesExpr(agg, labelAliases), Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}, nil
}
