package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_binop_card.go answers group_left()/group_right()
// (many-to-one vector-matching cardinality) for both the `+`/`-` merge
// (histogram_native_binop.go's mergeTwoHistogramProjections) and the
// `==`/`!=` compare (histogram_native_binop_eq.go's
// compareTwoHistogramProjections), including the `bool` modifier variant
// — cerberus issue #2328, the gap the one-to-one-only join shape used to
// leave rejected outright.
//
// [chplan.HistogramVectorJoin] does the actual broadcast (a real INNER
// JOIN — see its own doc for why the one-to-one path's UnionAll+Aggregate
// `count() = 2` shape cannot express it) and exposes every relevant field
// from both operands under `_hq_L_<field>` / `_hq_R_<field>` aliases,
// deliberately "dumb" about what merge/compare/Include-overlay computation
// happens on top. This file builds that computation as ordinary
// chplan.Expr projections over those aliases, reusing the ONE-TO-ONE
// path's exact field-fold helpers unchanged: histogramBinopMergeProjections
// (histogram_native_binop.go) for the merge fold, and
// histogramCompareFieldsExprCard / histogramCompareOutputProjectionsCard
// below (the array-collection-free siblings of histogram_native_binop_eq.go's
// histogramCompareFieldsExpr / histogramCompareOutputProjections — a
// HistogramVectorJoin row already IS one L/R pair, so there's no
// groupArrayIf collection to unwrap via arrayElement).
const (
	histJoinSideL = "L"
	histJoinSideR = "R"
)

// histJoinFieldAlias names a HistogramVectorJoin output column. MUST
// match chsql's histogramVectorJoinAlias (internal/chsql/
// histogram_vector_join.go) byte-for-byte; the two are independent
// literals rather than a shared constant because internal/promql may not
// depend on internal/chsql (see .go-arch-lint.yml — the same boundary
// [chplan.ManyToManyMatchMessage]'s doc explains).
func histJoinFieldAlias(side, col string) string {
	return "_hq_" + side + "_" + col
}

// histJoinFieldRef is a ColumnRef reading side's own value of col off a
// HistogramVectorJoin's output.
func histJoinFieldRef(side, col string) chplan.Expr {
	return &chplan.ColumnRef{Name: histJoinFieldAlias(side, col)}
}

// histogramCardManySide and histogramCardOneSide report which of "L"/"R"
// (the join's Left/Right — the operator's syntactic LHS/RHS operand)
// plays the "many" role for card: CardManyToOne (`group_left`) keeps
// Left's own per-series granularity; CardOneToMany (`group_right`) keeps
// Right's.
func histogramCardManySide(card chplan.VectorCard) string {
	if card == chplan.CardManyToOne {
		return histJoinSideL
	}
	return histJoinSideR
}

func histogramCardOneSide(card chplan.VectorCard) string {
	if card == chplan.CardManyToOne {
		return histJoinSideR
	}
	return histJoinSideL
}

// histogramCardOutputAttributesExpr renders the group_left(<labels>) /
// group_right(<labels>) output Attributes: the "many" side's own full
// Attributes, optionally overlaid with the "one" side's Include labels —
// the chplan.Expr-level mirror of chsql's outputAttributesFrag
// (internal/chsql/vector_join.go) for the plain float VectorJoin. Bare
// group_left/right (no Include labels) leaves the "many" side's
// Attributes unchanged, matching reference: Prometheus's resultMetric
// Keep/Del reduction does not apply to a non-one-to-one cardinality, so
// only an explicit Include list adds anything beyond the many side's own
// full label set.
func histogramCardOutputAttributesExpr(card chplan.VectorCard, include []string, attrsCol string) chplan.Expr {
	many := histJoinFieldRef(histogramCardManySide(card), attrsCol)
	if len(include) == 0 {
		return many
	}
	one := histJoinFieldRef(histogramCardOneSide(card), attrsCol)
	list := make([]chplan.Expr, len(include))
	for i, lbl := range include {
		list[i] = &chplan.LitString{V: lbl}
	}
	overlay := &chplan.FuncCall{
		Fn: chplan.FnMapFilter,
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{"k", "v"},
				Body:   &chplan.InList{Left: &chplan.BareIdent{Name: "k"}, List: list},
			},
			one,
		},
	}
	// mapConcat(<base>, <overlay>) — CH's later-argument-wins map merge,
	// same as outputAttributesFrag's own mapConcat(manySide, overlay).
	return &chplan.FuncCall{Fn: chplan.FnMapMerge, Args: []chplan.Expr{many, overlay}}
}

// histogramCardFromVectorMatching translates vm's PromQL cardinality
// modifier to chplan.VectorCard, mirroring binary.go's lowerVectorVector
// card mapping. Callers only reach here once vm.Card has already been
// confirmed != CardOneToOne (that case stays on the one-to-one
// merge/compare path). parser.CardManyToMany never arises: reference
// only sets it for the `and`/`or`/`unless` set operators, and every
// caller's own op recogniser (ADD/SUB, EQLC/NEQ) already restricts to
// arithmetic/equality ops before vm.Card is ever inspected.
func histogramCardFromVectorMatching(vm *parser.VectorMatching) chplan.VectorCard {
	if vm.Card == parser.CardOneToMany {
		return chplan.CardOneToMany
	}
	return chplan.CardManyToOne
}

// newHistogramVectorJoin builds the chplan.HistogramVectorJoin both the
// merge and compare card paths share. hpL/hpR are plain chplan.Node —
// [chplan.HistogramVectorJoin.Left]/[.Right] are typed Node themselves, and
// its own emitter reads both sides by column name through a subquery
// boundary (internal/chsql's histogramFieldsJoinSideFrag), so a set-op
// operand (*chplan.VectorSetOp, cerberus issue #2559) composes exactly like
// a *chplan.HistogramProjection one.
func newHistogramVectorJoin(hpL, hpR chplan.Node, vm *parser.VectorMatching, s schema.Metrics, ctx lowerCtx) *chplan.HistogramVectorJoin {
	return &chplan.HistogramVectorJoin{
		Left:             hpL,
		Right:            hpR,
		Match:            chplan.VectorMatch{Labels: append([]string(nil), vm.MatchingLabels...), On: vm.On},
		Card:             histogramCardFromVectorMatching(vm),
		Include:          append([]string(nil), vm.Include...),
		StepAligned:      ctx.step > 0,
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
	}
}

// histJoinArrayPair renders `array(<Left's own col>, <Right's own col>)`
// — the two-element array [histogramBinopMergeAggs]'s groupArray columns
// (hqMergeCountsArrayAlias and friends) collect in the one-to-one path.
// The existing merge-fold helpers (plainArraySum, arrayMin/arrayMax over
// these arrays) are all order-invariant over a fixed two-element set —
// see histogram_native_binop.go's header doc — so a literal two-element
// array built directly from the join's L/R columns is a drop-in
// replacement for a groupArray-collected one.
func histJoinArrayPair(col string) chplan.Expr {
	return &chplan.FuncCall{
		Fn:   chplan.FnArray,
		Args: []chplan.Expr{histJoinFieldRef(histJoinSideL, col), histJoinFieldRef(histJoinSideR, col)},
	}
}

// mergeTwoHistogramProjectionsCard is mergeTwoHistogramProjections'
// group_left()/group_right() sibling: same two-histogram Add/Sub fold
// (histogramBinopMergeProjections, unchanged), fed by a
// chplan.HistogramVectorJoin broadcast instead of a UnionAll+Aggregate
// `count() = 2` join. hpR is already negated by the caller for `-`,
// exactly as the one-to-one path expects.
//
// The join's raw `_hq_L_*`/`_hq_R_*` output is first materialised into
// the SAME alias set histogramBinopMergeAggs's groupArray/min/max
// AggFuncs would have produced (_hq_merge_counts, _hq_scales, ...) via a
// wrapping Project, so histogramBinopMergeProjections needs no changes at
// all — it cannot tell the difference between a two-row aggregation and a
// two-element array literal.
func mergeTwoHistogramProjectionsCard(hpL, hpR chplan.Node, vm *parser.VectorMatching, s schema.Metrics, ctx lowerCtx) chplan.Node {
	histSchema := histogramProjectionSchema(s)
	join := newHistogramVectorJoin(hpL, hpR, vm, s, ctx)
	stepAligned := ctx.step > 0

	stage1 := []chplan.Projection{
		{Expr: histogramCardOutputAttributesExpr(join.Card, join.Include, histSchema.AttributesColumn), Alias: histSchema.AttributesColumn},
		{Expr: histJoinArrayPair(histSchema.CountColumn), Alias: hqMergeCountsArrayAlias},
		{Expr: histJoinArrayPair(histSchema.SumColumn), Alias: hqMergeSumsArrayAlias},
		{
			Expr: &chplan.FuncCall{
				Fn:   chplan.FnLeast,
				Args: []chplan.Expr{histJoinFieldRef(histJoinSideL, histSchema.ScaleColumn), histJoinFieldRef(histJoinSideR, histSchema.ScaleColumn)},
			},
			Alias: hqAggMergedScaleAlias,
		},
		{Expr: histJoinArrayPair(histSchema.ZeroCountColumn), Alias: hqMergeZeroCountsArrayAlias},
		{
			Expr: &chplan.FuncCall{
				Fn:   chplan.FnGreatest,
				Args: []chplan.Expr{histJoinFieldRef(histJoinSideL, histSchema.ZeroThresholdColumn), histJoinFieldRef(histJoinSideR, histSchema.ZeroThresholdColumn)},
			},
			Alias: histSchema.ZeroThresholdColumn,
		},
		{Expr: histJoinArrayPair(histSchema.ScaleColumn), Alias: hqAggScalesArrayAlias},
		{Expr: histJoinArrayPair(histSchema.PositiveOffsetColumn), Alias: hqAggPosOffsetsArrayAlias},
		{Expr: histJoinArrayPair(histSchema.PositiveBucketCountsColumn), Alias: hqAggPosBucketsArrayAlias},
		{Expr: histJoinArrayPair(histSchema.NegativeOffsetColumn), Alias: hqAggNegOffsetsArrayAlias},
		{Expr: histJoinArrayPair(histSchema.NegativeBucketCountsColumn), Alias: hqAggNegBucketsArrayAlias},
	}
	if stepAligned {
		stage1 = append(stage1, chplan.Projection{
			Expr:  histJoinFieldRef(histogramCardManySide(join.Card), histSchema.TimestampColumn),
			Alias: histSchema.TimestampColumn,
		})
	}
	staged := &chplan.Project{Input: join, Projections: stage1}

	projs := []chplan.Projection{{Expr: &chplan.ColumnRef{Name: histSchema.AttributesColumn}, Alias: histSchema.AttributesColumn}}
	projs = append(projs, histogramBinopMergeProjections(histSchema)...)
	reshaped := &chplan.Project{Input: staged, Projections: projs}

	tsExpr := chplan.Expr(chplan.NowNano())
	if stepAligned {
		tsExpr = &chplan.ColumnRef{Name: histSchema.TimestampColumn}
	}
	return nativeHistogramProjection(reshaped, &chplan.LitString{V: ""}, tsExpr, histSchema)
}

// histogramCompareFieldsExprCard is histogramCompareFieldsExpr's
// group_left()/group_right() sibling: the identical field-by-field
// AND (`==`) / OR (`!=`, ne=true) structural-equality expression, but
// reading each field DIRECTLY off the join's `_hq_L_<col>` / `_hq_R_<col>`
// columns rather than through histCompareFirstElem's arrayElement(...)
// unwrap — a HistogramVectorJoin row already IS one L/R pair, so there is
// no groupArrayIf collection to unwrap.
func histogramCompareFieldsExprCard(s schema.Metrics, ne bool) chplan.Expr {
	op, combine := chplan.OpEq, chplan.OpAnd
	if ne {
		op, combine = chplan.OpNe, chplan.OpOr
	}
	var fieldCmp chplan.Expr
	for _, f := range histogramCompareFieldColumns(s) {
		cond := &chplan.Binary{
			Op:    op,
			Left:  histJoinFieldRef(histJoinSideL, f),
			Right: histJoinFieldRef(histJoinSideR, f),
		}
		if fieldCmp == nil {
			fieldCmp = cond
			continue
		}
		fieldCmp = &chplan.Binary{Op: combine, Left: fieldCmp, Right: cond}
	}
	return fieldCmp
}

// histogramCompareOutputProjectionsCard is
// histogramCompareOutputProjections' group_left()/group_right() sibling:
// the kept row's MetricName and nine histogram fields are always the
// operator's syntactic LHS's own value (reference's `hlhs`, unconditional
// on cardinality — see histogram_native_binop_eq.go's header doc), read
// directly off `_hq_L_<col>` (no arrayElement unwrap — see
// histogramCompareFieldsExprCard).
func histogramCompareOutputProjectionsCard(s schema.Metrics) []chplan.Projection {
	projs := []chplan.Projection{
		{Expr: histJoinFieldRef(histJoinSideL, s.MetricNameColumn), Alias: s.MetricNameColumn},
	}
	for _, f := range histogramCompareFieldColumns(s) {
		projs = append(projs, chplan.Projection{Expr: histJoinFieldRef(histJoinSideL, f), Alias: f})
	}
	return projs
}

// compareTwoHistogramProjectionsCard is compareTwoHistogramProjections'
// group_left()/group_right() sibling, fed by a chplan.HistogramVectorJoin
// broadcast instead of a UnionAll+Aggregate `count() = 2` join. See that
// function's doc for the returnBool branch's genuinely different output
// shape (float-valued 1.0/0.0 vs. the non-bool structural filter's
// histogram-valued LHS passthrough) — both apply identically here.
func compareTwoHistogramProjectionsCard(hpL, hpR chplan.Node, vm *parser.VectorMatching, ne, returnBool bool, s schema.Metrics, ctx lowerCtx) chplan.Node {
	histSchema := histogramProjectionSchema(s)
	join := newHistogramVectorJoin(hpL, hpR, vm, s, ctx)
	stepAligned := ctx.step > 0

	attrsProj := chplan.Projection{
		Expr:  histogramCardOutputAttributesExpr(join.Card, join.Include, histSchema.AttributesColumn),
		Alias: histSchema.AttributesColumn,
	}

	if returnBool {
		// Every matched pair survives (reference keeps every matched
		// pair for the `bool` modifier regardless of the comparison's
		// truth value) — no Filter, just the toFloat64(...) value fold
		// directly over the join's raw output.
		projs := []chplan.Projection{attrsProj}
		if stepAligned {
			projs = append(projs, chplan.Projection{
				Expr:  histJoinFieldRef(histogramCardManySide(join.Card), histSchema.TimestampColumn),
				Alias: histSchema.TimestampColumn,
			})
		} else {
			projs = append(projs, chplan.Projection{Expr: chplan.NowNano(), Alias: histSchema.TimestampColumn})
		}
		valueExpr := &chplan.FuncCall{Fn: chplan.FnToFloat64, Args: []chplan.Expr{histogramCompareFieldsExprCard(histSchema, ne)}}
		projs = append(
			projs,
			chplan.Projection{Expr: &chplan.LitString{V: ""}, Alias: histSchema.MetricNameColumn},
			chplan.Projection{Expr: valueExpr, Alias: histSchema.ValueColumn},
		)
		return &chplan.Project{Input: join, Projections: projs}
	}

	// Non-bool structural filter: only matching (`==`) or mismatching
	// (`!=`) pairs survive. Filter directly over the join's raw
	// `_hq_L_*`/`_hq_R_*` output, ahead of the stage-2 Project that maps
	// LHS's own fields onto the canonical output column names.
	filtered := &chplan.Filter{Input: join, Predicate: histogramCompareFieldsExprCard(histSchema, ne)}

	projs := []chplan.Projection{attrsProj}
	if stepAligned {
		projs = append(projs, chplan.Projection{
			Expr:  histJoinFieldRef(histogramCardManySide(join.Card), histSchema.TimestampColumn),
			Alias: histSchema.TimestampColumn,
		})
	}
	projs = append(projs, histogramCompareOutputProjectionsCard(histSchema)...)
	staged := &chplan.Project{Input: filtered, Projections: projs}

	nameExpr := chplan.Expr(&chplan.ColumnRef{Name: histSchema.MetricNameColumn})
	tsExpr := chplan.Expr(chplan.NowNano())
	if stepAligned {
		tsExpr = &chplan.ColumnRef{Name: histSchema.TimestampColumn}
	}
	return nativeHistogramProjection(staged, nameExpr, tsExpr, histSchema)
}
