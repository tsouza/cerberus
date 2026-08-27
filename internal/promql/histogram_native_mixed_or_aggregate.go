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
// binop) keeps falling through to the pre-existing rejection unchanged.
// A windowed/derived float arm (cerberus issue #2333's own axis) DOES
// compose here too as of cerberus issue #2453 — see
// [canonicalizeMixedFloatArmForAgg] below.
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
//     plain float-valued, now including a windowed/derived float arm
//     (issue #2453 lifted the shape restriction this file used to pin
//     with its own now-widened
//     TestLower_ExpHistogram_MixedSetOpOr_SumWrapped_WindowedFloatSide
//     test).
//  2. The `or`'s OWN shadow rule (every row of the side that's LHS in
//     the source AST, plus only the RHS rows whose label signature is
//     absent from LHS) is resolved with a [chplan.VectorSetOp] UNLESS —
//     the identical mixed-type semi/anti-join #2337 already ships for
//     `and`/`unless` between a histogram-valued and a float-valued
//     operand, reused here rather than re-derived. Note this ONLY runs
//     on the source-AST RHS side (mixedOrShadowUnless's single call
//     site below); the LHS side is forwarded verbatim, so when the
//     float arm is the source-AST LHS it reaches step 3 un-canonicalised
//     by this step — which is exactly why step 3's float branch needs
//     its own canonicalisation, see [canonicalizeMixedFloatArmForAgg].
//  3. Each of the two shadow-resolved arms is reduced independently by
//     its own existing SUM/AVG machinery: the histogram arm through
//     [lowerExpHistogramSumOrAvgOverPlan] (the SAME reduction
//     `sum(rate(m_exp_hist[5m]))` already uses), the float arm through
//     [lowerPlainAggOverMixedFloatArm] (this file — the CH-native
//     `sum`/`avg` an ordinary float aggregation already uses, applied to
//     an arbitrary already-lowered plan instead of a freshly lowered
//     selector, after [canonicalizeMixedFloatArmForAgg] widens it onto
//     the canonical Timestamp contract the aggregate's own GroupBy
//     needs).
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

	// cerberus issue #2453 taught lowerPlainAggOverMixedFloatArm (this
	// file, below) the matrix/derived-shape canonicalisation
	// [canonicalizeMixedFloatArmForAgg] gives it, so the sum/avg-wrapped
	// path now accepts a windowed float side exactly like the root-only
	// leaf recognizer (issue #2333) does — see
	// lowerMixedExpHistogramOperands's own doc comment.
	histForAgg, floatForAgg, err := shadowResolveMixedExpHistogramOperands(b, s, ctx)
	if err != nil {
		return nil, err
	}

	histBranch, err := lowerExpHistogramSumOrAvgOverPlan(agg, histForAgg, s, ctx.resourceBounds.HistogramMergeMaxCostUnits)
	if err != nil {
		return nil, err
	}
	floatBranch, err := lowerPlainAggOverMixedFloatArm(agg, floatForAgg, s, ctx)
	if err != nil {
		return nil, err
	}

	return combineMixedAggregateBranches(histBranch, floatBranch, s, ctx), nil
}

// shadowResolveMixedExpHistogramOperands lowers b's two operands
// ([lowerMixedExpHistogramOperands]) and resolves the `or`'s own shadow
// rule BEFORE any aggregation reduction runs: the side that is LHS in the
// source AST keeps every row; the RHS side keeps only the rows whose
// label signature has no match on the LHS side. This is the identical
// mixed-type UNLESS #2337 already ships for `and`/`unless` between a
// histogram-valued and a float-valued operand — reused verbatim rather
// than re-derived.
//
// Split out of [lowerSumOrAvgOverMixedExpHistogramSetOp] so cerberus issue
// #2595's sibling aggregate families (histogram_native_mixed_or_aggregate_
// presence.go's count()/group(), histogram_native_mixed_or_aggregate_
// float_only.go's min()/max()/stddev()/stdvar(), and histogram_native_
// mixed_or_aggregate_count_values.go's count_values()) can build the SAME
// two shadow-resolved arms without duplicating the histogram-valued /
// float-valued dispatch or the shadow-rule application.
func shadowResolveMixedExpHistogramOperands(b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (histForAgg, floatForAgg chplan.Node, err error) {
	histNode, floatNode, histOnLeft, err := lowerMixedExpHistogramOperands(b, s, ctx)
	if err != nil {
		return nil, nil, err
	}

	orMatch := mixedExpHistogramMatch(b)
	histForAgg, floatForAgg = histNode, floatNode
	if histOnLeft {
		floatForAgg = mixedOrShadowUnless(floatNode, histNode, false, orMatch, s, ctx)
	} else {
		histForAgg = mixedOrShadowUnless(histNode, floatNode, true, orMatch, s, ctx)
	}
	return histForAgg, floatForAgg, nil
}

// shadowResolveFloatArmChecked is [shadowResolveMixedExpHistogramOperands]
// for the composer families that only ever need the shadow-resolved FLOAT
// arm (never the histogram one) and read no other field off b themselves:
// scalar() (histogram_native_mixed_or_scalar.go), sort()/sort_desc()
// (histogram_native_mixed_or_sort.go), and the date-component functions
// (histogram_native_mixed_or_datefn.go). Each of those composers calls
// shadowResolveMixedExpHistogramOperands directly, which never checks
// ReturnBool itself — `bool` is only valid on comparison binary ops, and
// none of these three are — so each callsite had to repeat the identical
// three-line "reject bool" guard ahead of the call. Centralising it here
// removes that duplication and the risk a future 4th composer forgets the
// guard.
func shadowResolveFloatArmChecked(b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if b.ReturnBool {
		return nil, fmt.Errorf("promql: 'bool' modifier is only allowed on comparison binary ops")
	}
	_, floatForAgg, err := shadowResolveMixedExpHistogramOperands(b, s, ctx)
	if err != nil {
		return nil, err
	}
	return floatForAgg, nil
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

// lowerPlainAggOverMixedFloatArm reduces input with the SAME CH-native
// aggregate an ordinary (non-histogram) PromQL aggregation uses
// ([aggregateGroupBy], [buildAggFunc], [promAggregateAttributesExpr]),
// grouped by the evaluation Timestamp in addition to the user's
// `by`/`without` labels. [canonicalizeMixedFloatArmForAgg] runs first so
// this GroupBy's `ColumnRef{Name: s.TimestampColumn}` resolves against
// input's own SELECT regardless of which of the three RowShapes
// [lowerMixedExpHistogramOperands] accepted for it.
//
// Despite the name (kept for [combineMixedAggregateBranches]'s SUM/AVG
// callers, where input genuinely is the shadow-resolved float-valued arm
// alone), this function is agnostic to what input actually is:
// [buildAggFunc] dispatches on agg.Op alone, so cerberus issue #2595's
// sibling aggregate families reuse it unchanged for two OTHER shapes —
// count()/group() (histogram_native_mixed_or_aggregate_presence.go) pass
// the FULL shadow-resolved Mixed union ([lowerMixedExpHistogramSetOp]'s
// own leaf output) because those two ops read no per-row value at all
// (COUNT counts [chplan.VectorSetOp.Mixed]'s placeholder-safe Value
// column; GROUP ignores Value entirely — see [histogramSampleValuePlaceholder]'s
// own doc for why a placeholder Value is safe to count but not to
// transform), while min()/max()/stddev()/stdvar()
// (histogram_native_mixed_or_aggregate_float_only.go) pass the
// shadow-resolved float arm alone, exactly like the SUM/AVG callers, but
// with no histogram branch or [combineMixedAggregateBranches] recombine
// at all — those four ops DROP histogram samples in reference rather than
// merging them (see that file's own doc for the citation).
//
// Grouping by Timestamp ONLY when ctx.step > 0 (range mode) mirrors the
// ordinary, non-histogram aggregate path's own identical choice
// ([lowerAggregate]'s `rangeBucketed` in lower.go) — matrix input
// publishes a real per-step anchor there, the correct per-step key to
// preserve range-mode granularity by. At instant mode this function used
// to group by Timestamp unconditionally too (cerberus issue #2346's
// original shape), on the theory that a canonical-shape or
// reduced/derived arm's Timestamp is always a synthesised, per-query-
// uniform value there — true for [chplan.ReducedWindowRowShape] (which
// [canonicalizeMixedFloatArmForAgg] widens onto that exact convention)
// but FALSE for a bare [chplan.SampleRowShape] selector arm or the full
// Mixed union count()/group() (histogram_native_mixed_or_aggregate_
// presence.go, cerberus issue #2595) pass here: a bare selector's
// instant-mode Timestamp is the REAL per-series collapsed-to-latest
// sample time (verified directly against chDB — two series scraped a
// second apart keep two different Timestamp values there), and the
// histogram branch's own Timestamp is always a synthesised "now" —
// mixing either of those into the group key silently splits what
// reference treats as ONE group into several, or drops the group
// entirely when [combineMixedAggregateBranches]'s two sub-branches no
// longer agree row-for-row. Not grouping by Timestamp at instant mode —
// exactly the ordinary aggregate path's own rule — removes the hazard
// instead of relying on every caller's input happening to be uniform.
func lowerPlainAggOverMixedFloatArm(agg *parser.AggregateExpr, input chplan.Node, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	input = canonicalizeMixedFloatArmForAgg(input, s)

	labelGroupBy, err := aggregateGroupBy(agg, s)
	if err != nil {
		return nil, err
	}
	labelAliases := groupKeyAliases(len(labelGroupBy))

	aggFunc, err := buildAggFunc(agg, s, ctx)
	if err != nil {
		return nil, err
	}

	const bucketAlias = "mixed_agg_bucket_ts"
	rangeBucketed := ctx.step > 0
	groupBy := labelGroupBy
	groupByAliases := labelAliases
	if rangeBucketed {
		groupBy = append([]chplan.Expr{&chplan.ColumnRef{Name: s.TimestampColumn}}, groupBy...)
		groupByAliases = append([]string{bucketAlias}, groupByAliases...)
	}
	merged := &chplan.Aggregate{
		Input:              input,
		GroupBy:            groupBy,
		GroupByAliases:     groupByAliases,
		AggFuncs:           []chplan.AggFunc{aggFunc},
		DropEmptyOnNoGroup: true,
	}

	// Instant mode stamps the query's own uniform evaluation instant
	// (chplan.NowNano(), matching [wrapAggregateForSample]'s identical
	// choice for the ordinary aggregate path); range mode forwards the
	// per-step anchor the GroupBy above just captured.
	tsExpr := chplan.NowNano()
	if rangeBucketed {
		tsExpr = &chplan.ColumnRef{Name: bucketAlias}
	}

	return &chplan.Project{
		Input: merged,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: promAggregateAttributesExpr(agg, labelAliases), Alias: s.AttributesColumn},
			{Expr: tsExpr, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}, nil
}

// canonicalizeMixedFloatArmForAgg widens input — the (possibly still raw,
// un-shadow-resolved) float-valued arm [lowerMixedExpHistogramOperands]
// accepted — onto the Timestamp contract
// [lowerPlainAggOverMixedFloatArm]'s own GroupBy needs, cerberus issue
// #2453's fix for the gap this file used to leave uncovered: the
// ROOT-only leaf recognizer
// (histogram_native_mixed_or.go, issue #2333) gets this canonicalisation
// for free from internal/chsql's mixedVectorSetOpArmFrag /
// vectorSetOpCanonicalQuartetFrags at SQL-emission time, but
// internal/promql cannot import internal/chsql (see .go-arch-lint.yml's
// layering — chsql depends on chplan, and QL packages depend on chplan,
// never on chsql), so this function re-derives the one piece of that
// canonicalisation lowerPlainAggOverMixedFloatArm's own Aggregate
// actually needs, expressed as a [chplan.Project] instead of a chsql
// Frag.
//
// Attributes and Value need no widening: every [chplan.RowShape] a mixed
// `or`'s float arm can carry publishes both under their canonical schema
// names already — vectorSetOpCanonicalQuartetFrags references
// `Col(s.AttributesColumn)` and `Col(s.ValueColumn)` unconditionally for
// every shape, and [chplan.RangeWindow] (the only node
// [chplan.GridWindowRowShape] / [chplan.ReducedWindowRowShape] name) is
// always built with `GroupBy: [ColumnRef{Name: s.AttributesColumn}]`
// (internal/promql/lower.go) — so [aggregateGroupBy] /
// [promAggregateAttributesExpr]'s existing `s.AttributesColumn`
// references and [buildAggFunc]'s existing `s.ValueColumn` reference
// already resolve correctly against any of the three shapes without
// modification.
//
// Timestamp is the one column that genuinely differs by shape:
//
//   - [chplan.SampleRowShape] already exposes it under `s.TimestampColumn`
//     by construction — nothing to do.
//   - [chplan.GridWindowRowShape] — a matrix-mode [chplan.RangeWindow]
//     (`OuterRange > 0`) — also already exposes it there: the matrix
//     emitter aliases its per-row grid anchor to the node's OWN
//     TimestampColumn field, which every PromQL construction site sets to
//     `s.TimestampColumn` (see internal/chsql's vectorSetOpArmTimestampCol
//     doc comment for the general rule this is a specific case of), so
//     the alias target already matches what this function's caller
//     references — nothing to do.
//   - [chplan.ReducedWindowRowShape] — an instant-mode [chplan.RangeWindow]
//     (`OuterRange == 0`, only reachable when the query itself is instant;
//     see modifiers.go's rangeGridShapeFor gridSingleAnchor case) — has no
//     timestamp column of any kind: `rate(x[5m])` reduces every series to
//     one row with no per-row time left to report. A [chplan.Project]
//     synthesises the identical "5 seconds before CH-now" anchor
//     internal/chsql's vectorSetOpSynthesizedAnchorFrag stamps on the
//     SAME shape for the root-only leaf recognizer's canonical-shape
//     projection (both spellings of `now64(9) -
//     toIntervalNanosecond(5e9)` share the one chplan-level builder,
//     [chplan.NowNanoMinusStaleness], that internal/api/prom/handler.go's
//     synthesizedAnchor also calls), keeping the two mixed-or lowerings'
//     instant-mode anchors identical.
func canonicalizeMixedFloatArmForAgg(input chplan.Node, s schema.Metrics) chplan.Node {
	if chplan.RowShapeOf(input) != chplan.ReducedWindowRowShape {
		return input
	}
	return &chplan.Project{
		Input: input,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: chplan.NowNanoMinusStaleness(), Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}
}
