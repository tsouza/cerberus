package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_binop_eq.go lowers `==`/`!=` between two
// histogram-VALUED shapes — `<exp-hist shape> (==|!=) <exp-hist shape>`
// — into a histogram-VALUED result (cerberus issue #2273, gap 2).
//
// Reference Prometheus answers `==`/`!=` between two native-histogram
// samples with a STRUCTURAL-EQUALITY FILTER, not a merge
// (promql/engine.go's `vectorElemBinop`, the `hlhs != nil && hrhs !=
// nil` case):
//
//	case parser.EQLC:
//	    // This operation expects that both histograms are compacted.
//	    return 0, hlhs, hlhs.Equals(hrhs), nil, nil
//	case parser.NEQ:
//	    // This operation expects that both histograms are compacted.
//	    return 0, hlhs, !hlhs.Equals(hrhs), nil, nil
//
// The kept-row VALUE is `hlhs` — the LHS histogram — UNCHANGED (no
// scale reconciliation, no bucket merge); `keep` decides whether the
// pair survives the join. This is a genuinely different mechanism from
// `+`/`-`'s bucket-merge reuse (histogram_native_binop.go's
// [mergeTwoHistogramProjections]): there is no reconciliation index
// math here at all, because `FloatHistogram.Equals` (model/histogram/
// float_histogram.go) is a literal, bit-exact field comparison —
// Schema, Count, Sum, ZeroThreshold, ZeroCount, and both signed bucket
// ladders (spans + values) — with NO scale/offset normalisation of its
// own. Reference's own comment above flags the consequence: two
// histograms describing the same distribution at different native
// scales compare UNEQUAL unless the caller already compacted them to a
// matching representation first. Cerberus's raw per-selector row is
// exactly the stored representation (the same thing reference's
// storage-layer sample would carry), so reading the raw Scale /
// ZeroCount / ZeroThreshold / offset / bucket-count columns directly,
// with no reconciliation step, matches reference's literal-field
// semantics rather than diverging from it.
//
// Every field CH's `=`/`!=` compares here already matches reference's
// bit-exact intent for the scalar fields (Scale is an Int, ZeroThreshold
// / Offset are exact-valued in practice) and CH's native Array equality
// for the two bucket-count arrays (same length, same values,
// position-for-position) mirrors floatBucketsMatch + the span-derived
// index alignment reference's Equals performs — spelled as a dense
// Offset+Array pair rather than reference's sparse Span list, but
// describing the identical bucket layout for data OTel-CH's exporter
// itself produced (no independent second encoder to disagree on
// trailing zero-length spans). The one acknowledged gap is float NaN /
// signed-zero BIT-PATTERN parity: reference's Equals compares
// math.Float64bits (NaN equals NaN, +0 != -0), while ClickHouse's `=`
// follows plain IEEE754 (NaN never equals anything, +0 == -0). Real
// exp-histogram data has no route to a NaN Sum or a negative-zero
// ZeroCount, so this is a documented simplification, not an observed
// divergence.
//
// Reference rejects every other histogram/histogram comparison
// (`>`, `<`, `>=`, `<=`) with NewIncompatibleTypesInBinOpInfo — cerberus's
// existing rejection at expHistogramSelectorRouting (lower.go) already
// answers those the same way reference does (this file only recognises
// EQLC/NEQ), so there is no gap to close there.
//
// on()/ignoring() matching is supported via
// [applyVectorMatchToHistogramOperand] (histogram_native_binop_match.go),
// the same mechanism [lowerExpHistogramHistogramBinop] uses for `+`/`-`.
// group_left()/group_right() (many-to-one broadcast) is supported via
// [compareTwoHistogramProjectionsCard] (histogram_native_binop_card.go,
// cerberus issue #2328).
//
// The `bool` modifier is ALSO answered, but by a different function
// ([expHistogramHistogramCompareBoolBinop] /
// [lowerExpHistogramHistogramCompareBoolBinop] below): reference's
// VectorBinop overrides the histogram result to nil and the value to
// 1.0/0.0 for every matched pair regardless of `keep`, so the result
// becomes FLOAT-valued rather than histogram-valued — a third,
// still-different output shape from both the merge in
// histogram_native_binop.go and the structural filter this function
// builds. Because it is float-valued, it cannot hook into
// [lowerExpHistogramValuedShape]'s histogram-valued recursive gate the
// way this file's non-bool recogniser does — see that recogniser's own
// doc for how the two stay disjoint.
//
// Lowering shape (instant mode):
//
//	HistogramProjection [MetricName (from LHS), Attributes, now64(9), Value=0, <nine histogram columns, from LHS>]
//	  Project [Attributes (rebuilt from the shared match key), LHS's own
//	           MetricName / Scale / ZeroCount / ZeroThreshold /
//	           {Pos,Neg}{Offset,BucketCounts} / Count / Sum]
//	    Aggregate groupBy=[Attributes]
//	              having=(count()=2 AND <field-by-field equality, negated for !=>)
//	              funcs=<groupArrayIf(field, side=L) / groupArrayIf(field, side=R) per compared field>
//	      UnionAll
//	        <lhs HistogramProjection, tagged side=L>
//	        <rhs HistogramProjection, tagged side=R>
//
// The `count() = 2` guard is the same default-matching stand-in
// [histogramBinopBothSidesMatchedGuard] documents for `+`/`-`: default
// vector matching keys on the full Attributes map, which each operand's
// own HistogramProjection is already grouped by, so a matched key with
// fewer than two contributing rows means the series existed on only one
// side — dropped by PromQL's default one-to-one V-V matching regardless
// of the operator.
//
// Every compared field is collected TWICE — once per side, via a
// literal 0/1 discriminator column ([histEqSideAlias]) each operand is
// tagged with before the UnionAll — because, unlike the symmetric `+`/
// `-` fold (order-invariant plain addition), `!=`'s kept output is
// LHS-specific: reference always projects `hlhs`, never `hrhs`, even
// though the two disagree (that's WHY the row survived `!=`'s filter).
// `==`'s kept rows have hlhs and hrhs bit-identical by construction (the
// Having guard already proved it), so projecting the L-tagged value is
// equally correct there — one projection shape serves both operators.
func expHistogramHistogramCompareBinop(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (lhs, rhs parser.Expr, ne bool, vm *parser.VectorMatching, ok bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
		return nil, nil, false, nil, false
	}
	b, isBin := unwrapBinaryExpr(expr)
	if !isBin || (b.Op != parser.EQLC && b.Op != parser.NEQ) {
		return nil, nil, false, nil, false
	}
	if b.ReturnBool {
		// The `bool` modifier overrides the result to a FLOAT 1.0/0.0 for
		// every matched pair (reference's VectorBinop, see
		// [expHistogramHistogramCompareBoolBinop]'s doc) — a different,
		// non-histogram-valued output shape from this recogniser's
		// structural-equality filter, so it must NOT participate in
		// [lowerExpHistogramValuedShape]'s histogram-valued recursive
		// gate. Reporting no match here lets the query fall through the
		// dispatch chain to [lowerVectorVector] (binary.go), the entry
		// point every vector-vector binop reaches regardless of nesting
		// depth, where [expHistogramHistogramCompareBoolBinop] picks it
		// up instead.
		return nil, nil, false, nil, false
	}
	if !isExpHistogramValuedShape(b.LHS, s, ctx) || !isExpHistogramValuedShape(b.RHS, s, ctx) {
		return nil, nil, false, nil, false
	}
	return b.LHS, b.RHS, b.Op == parser.NEQ, b.VectorMatching, true
}

// lowerExpHistogramHistogramCompareBinop lowers the shape
// [expHistogramHistogramCompareBinop] recognised. Both operands defer
// to their own existing histogram-valued lowering unchanged — this
// function only adds the join + structural-equality filter on top.
//
// group_left()/group_right() (vm.Card != CardOneToOne) routes through
// [compareTwoHistogramProjectionsCard] (histogram_native_binop_card.go),
// which broadcasts via a real [chplan.HistogramVectorJoin] — cerberus
// issue #2328.
func lowerExpHistogramHistogramCompareBinop(lhsExpr, rhsExpr parser.Expr, ne bool, vm *parser.VectorMatching, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	hpL, err := lowerExpHistogramValuedOperand(lhsExpr, s, ctx)
	if err != nil {
		return nil, err
	}
	hpR, err := lowerExpHistogramValuedOperand(rhsExpr, s, ctx)
	if err != nil {
		return nil, err
	}
	if vm != nil && vm.Card != parser.CardOneToOne {
		return compareTwoHistogramProjectionsCard(hpL, hpR, vm, ne, false /* returnBool */, s, ctx), nil
	}
	hpL = applyVectorMatchToHistogramOperand(hpL, vm, s, ctx)
	hpR = applyVectorMatchToHistogramOperand(hpR, vm, s, ctx)
	return compareTwoHistogramProjections(hpL, hpR, ne, false /* returnBool */, s, ctx), nil
}

// expHistogramHistogramCompareBoolBinop recognises `<exp-hist shape>
// (==|!=) bool <exp-hist shape>` — reference overrides EVERY matched
// pair's result to a FLOAT 1.0/0.0 for the `bool` modifier
// (promql/engine.go's `VectorBinop`: `if returnBool { histogramValue =
// nil; floatValue = ... }`), regardless of `keep` — a different, FLOAT-
// valued output shape from both [expHistogramHistogramBinop]'s
// histogram-valued merge and [expHistogramHistogramCompareBinop]'s
// histogram-valued structural filter (cerberus issue #2273, gap 2).
//
// Because the output is float-valued, this recogniser does NOT hook into
// [lowerExpHistogramValuedShape]'s histogram-valued recursive gate the
// way the other two histogram-histogram binop recognisers do —
// [expHistogramHistogramCompareBinop] explicitly defers to this one by
// reporting no match whenever `b.ReturnBool` is set. Instead,
// [lowerVectorVector] (binary.go) — the canonical entry point every
// vector-vector binop reaches, top-level or nested — dispatches here
// directly, mirroring how a plain (non-histogram) `bool`-compare reaches
// `chplan.VectorJoin` with `ReturnBool: true` from the same call site.
func expHistogramHistogramCompareBoolBinop(b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (lhs, rhs parser.Expr, ne bool, vm *parser.VectorMatching, ok bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange || !b.ReturnBool {
		return nil, nil, false, nil, false
	}
	if b.Op != parser.EQLC && b.Op != parser.NEQ {
		return nil, nil, false, nil, false
	}
	if !isExpHistogramValuedShape(b.LHS, s, ctx) || !isExpHistogramValuedShape(b.RHS, s, ctx) {
		return nil, nil, false, nil, false
	}
	return b.LHS, b.RHS, b.Op == parser.NEQ, b.VectorMatching, true
}

// lowerExpHistogramHistogramCompareBoolBinop lowers the shape
// [expHistogramHistogramCompareBoolBinop] recognised, reusing
// [compareTwoHistogramProjections]'s join machinery with returnBool=true
// — see that function's doc for the Having/output-shape difference the
// `bool` modifier makes. group_left()/group_right() routes through
// [compareTwoHistogramProjectionsCard] the same way the non-bool compare
// does — cerberus issue #2328.
func lowerExpHistogramHistogramCompareBoolBinop(lhsExpr, rhsExpr parser.Expr, ne bool, vm *parser.VectorMatching, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	hpL, err := lowerExpHistogramValuedOperand(lhsExpr, s, ctx)
	if err != nil {
		return nil, err
	}
	hpR, err := lowerExpHistogramValuedOperand(rhsExpr, s, ctx)
	if err != nil {
		return nil, err
	}
	if vm != nil && vm.Card != parser.CardOneToOne {
		return compareTwoHistogramProjectionsCard(hpL, hpR, vm, ne, true /* returnBool */, s, ctx), nil
	}
	hpL = applyVectorMatchToHistogramOperand(hpL, vm, s, ctx)
	hpR = applyVectorMatchToHistogramOperand(hpR, vm, s, ctx)
	return compareTwoHistogramProjections(hpL, hpR, ne, true /* returnBool */, s, ctx), nil
}

const (
	// histEqSideAlias names the literal 0/1 discriminator column each
	// operand is tagged with before the UnionAll, so the per-field
	// groupArrayIf collections below can tell LHS's contributed row
	// apart from RHS's within one matched Attributes group. See this
	// file's header doc for why `!=`'s LHS-specific output needs it.
	histEqSideAlias = "_hq_eq_side"
)

// histEqSideLHS and histEqSideRHS are the two [histEqSideAlias] values.
const (
	histEqSideLHS int64 = 0
	histEqSideRHS int64 = 1
)

// compareTwoHistogramProjections builds the join + comparison
// [expHistogramHistogramCompareBinop] / [expHistogramHistogramCompareBoolBinop]
// recognised. ne selects `!=` (keep the mismatching pairs, or — with
// returnBool — value 1.0 where the pair mismatches) over `==`.
//
// returnBool selects between two genuinely different output shapes,
// mirroring reference's own VectorBinop branch:
//
//   - false (structural filter): Having additionally requires the
//     field-by-field comparison to hold ([histogramCompareHavingGuard]),
//     so only matching (`==`) or mismatching (`!=`) pairs survive the
//     join at all; the surviving row's value is LHS's own histogram,
//     unchanged, capped with [nativeHistogramProjection] — a
//     histogram-VALUED result.
//   - true (`bool` modifier): Having is just the both-sides-matched
//     guard — reference keeps EVERY matched pair regardless of the
//     comparison's truth value — and the output Value is
//     `toFloat64(<field comparison>)`, a plain 4-column
//     MetricName/Attributes/Timestamp/Value Sample shape (MetricName
//     dropped to "", matching reference's dropMetricName rule for a
//     `bool`-modified V-V binop) rather than a HistogramProjection: the
//     result is FLOAT-valued, not histogram-valued.
func compareTwoHistogramProjections(hpL, hpR *chplan.HistogramProjection, ne, returnBool bool, s schema.Metrics, ctx lowerCtx) chplan.Node {
	histSchema := histogramProjectionSchema(s)
	stepAligned := ctx.step > 0

	lSide := projectHistogramCompareSide(hpL, histEqSideLHS, histSchema, stepAligned)
	rSide := projectHistogramCompareSide(hpR, histEqSideRHS, histSchema, stepAligned)

	groupBy, groupByAliases, attrsRebuild := histogramAggGroupBy(
		nil, &chplan.ColumnRef{Name: histSchema.AttributesColumn}, histSchema,
	)

	var projs []chplan.Projection
	if stepAligned {
		// Range mode: both operands' HistogramProjection already
		// publishes the per-step grid anchor under the canonical
		// TimestampColumn, so grouping by it keeps each anchor's
		// comparison independent — same reasoning as
		// [mergeTwoHistogramProjections]'s own anchor prepend.
		anchor := &chplan.ColumnRef{Name: histSchema.TimestampColumn}
		groupBy = append([]chplan.Expr{anchor}, groupBy...)
		groupByAliases = append([]string{histSchema.TimestampColumn}, groupByAliases...)
		projs = append(projs, chplan.Projection{Expr: anchor, Alias: histSchema.TimestampColumn})
	}

	having := histogramCompareHavingGuard(histSchema, ne)
	if returnBool {
		// Every matched pair survives — the both-sides-matched guard
		// alone, with no field-equality condition.
		having = histogramBinopBothSidesMatchedGuard()
	}

	compared := &chplan.Aggregate{
		Input:              &chplan.UnionAll{Inputs: []chplan.Node{lSide, rSide}},
		GroupBy:            groupBy,
		GroupByAliases:     groupByAliases,
		AggFuncs:           histogramCompareMergeAggs(histSchema),
		Having:             having,
		DropEmptyOnNoGroup: true,
	}
	projs = append(projs, chplan.Projection{Expr: attrsRebuild, Alias: histSchema.AttributesColumn})

	if returnBool {
		if !stepAligned {
			// Instant mode: the shared prefix above only projects
			// Timestamp in range mode (the `stepAligned` anchor). The
			// non-bool path defers to nativeHistogramProjection's own
			// tsExpr for this case; the bool path builds its own
			// canonical Sample row directly, so it projects the literal
			// here instead.
			projs = append(projs, chplan.Projection{Expr: chplan.NowNano(), Alias: histSchema.TimestampColumn})
		}
		valueExpr := &chplan.FuncCall{Fn: chplan.FnToFloat64, Args: []chplan.Expr{histogramCompareFieldsExpr(histSchema, ne)}}
		projs = append(
			projs,
			chplan.Projection{Expr: &chplan.LitString{V: ""}, Alias: histSchema.MetricNameColumn},
			chplan.Projection{Expr: valueExpr, Alias: histSchema.ValueColumn},
		)
		return &chplan.Project{Input: compared, Projections: projs}
	}

	projs = append(projs, histogramCompareOutputProjections(histSchema)...)
	reshaped := &chplan.Project{Input: compared, Projections: projs}

	nameExpr := chplan.Expr(&chplan.ColumnRef{Name: histSchema.MetricNameColumn})
	tsExpr := chplan.Expr(chplan.NowNano())
	if stepAligned {
		tsExpr = &chplan.ColumnRef{Name: histSchema.TimestampColumn}
	}
	return nativeHistogramProjection(reshaped, nameExpr, tsExpr, histSchema)
}

// projectHistogramCompareSide wraps one operand's HistogramProjection in
// an explicit column list — MetricName, Attributes, (Timestamp in range
// mode), and every field [histogramCompareFieldColumns] names — plus the
// literal [histEqSideAlias] discriminator, so both operands expose an
// IDENTICAL column set/order for the UnionAll below to combine.
func projectHistogramCompareSide(hp *chplan.HistogramProjection, side int64, histSchema schema.Metrics, stepAligned bool) *chplan.Project {
	cols := []string{histSchema.MetricNameColumn, histSchema.AttributesColumn}
	if stepAligned {
		cols = append(cols, histSchema.TimestampColumn)
	}
	cols = append(cols, histogramCompareFieldColumns(histSchema)...)

	projs := make([]chplan.Projection, 0, len(cols)+1)
	for _, col := range cols {
		projs = append(projs, chplan.Projection{Expr: &chplan.ColumnRef{Name: col}, Alias: col})
	}
	projs = append(projs, chplan.Projection{Expr: &chplan.LitInt{V: side}, Alias: histEqSideAlias})
	return &chplan.Project{Input: hp, Projections: projs}
}

// histogramCompareFieldColumns names the nine histogram payload columns
// [FloatHistogram.Equals] compares — Scale, Count, Sum, ZeroCount,
// ZeroThreshold (when the schema persists it), and both signed bucket
// ladders' Offset + BucketCounts. Ordering is arbitrary; the same order
// is used to collect each field's per-side groupArrayIf and to rebuild
// the output row, so it never needs to agree with anything else.
func histogramCompareFieldColumns(s schema.Metrics) []string {
	cols := []string{s.ScaleColumn, s.CountColumn, s.SumColumn, s.ZeroCountColumn}
	if s.ZeroThresholdColumn != "" {
		cols = append(cols, s.ZeroThresholdColumn)
	}
	return append(
		cols,
		s.PositiveOffsetColumn, s.PositiveBucketCountsColumn,
		s.NegativeOffsetColumn, s.NegativeBucketCountsColumn,
	)
}

// histEqSideCond renders `_hq_eq_side = <side>` — the groupArrayIf
// condition that restricts a collection to one operand's own row.
func histEqSideCond(side int64) chplan.Expr {
	return &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: histEqSideAlias}, Right: &chplan.LitInt{V: side}}
}

// histCompareFieldAlias names the groupArrayIf column collecting col's
// value from the given side — a single guaranteed-length-1 array (the
// `count() = 2` Having guard, applied after these AggFuncs run,
// guarantees at most one row per side per matched Attributes group; see
// this file's header doc).
func histCompareFieldAlias(col string, side int64) string {
	suffix := "l"
	if side == histEqSideRHS {
		suffix = "r"
	}
	return "_hq_eq_" + col + "_" + suffix
}

// histogramCompareMergeAggs collects, per matched Attributes group, each
// side's own MetricName plus every [histogramCompareFieldColumns] field —
// via groupArrayIf keyed on [histEqSideCond] — as the raw material both
// the Having guard (field-by-field L-vs-R equality) and the output
// projection (L's own value, unconditionally — see this file's header
// doc for why `!=` needs that specifically) read back by alias.
//
// MetricName is collected for LHS only: reference's resultMetric always
// takes the LHS sample's full label set for a non-bool comparison
// (changesMetricSchema(EQLC/NEQ) is false), so RHS's metric name is
// never read.
func histogramCompareMergeAggs(s schema.Metrics) []chplan.AggFunc {
	aggs := []chplan.AggFunc{
		{
			Fn:          chplan.FnGroupArray,
			Combinators: []chplan.AggCombinator{chplan.CombIf},
			Args:        []chplan.Expr{&chplan.ColumnRef{Name: s.MetricNameColumn}, histEqSideCond(histEqSideLHS)},
			Alias:       histCompareFieldAlias(s.MetricNameColumn, histEqSideLHS),
		},
	}
	for _, f := range histogramCompareFieldColumns(s) {
		aggs = append(
			aggs,
			chplan.AggFunc{
				Fn:          chplan.FnGroupArray,
				Combinators: []chplan.AggCombinator{chplan.CombIf},
				Args:        []chplan.Expr{&chplan.ColumnRef{Name: f}, histEqSideCond(histEqSideLHS)},
				Alias:       histCompareFieldAlias(f, histEqSideLHS),
			},
			chplan.AggFunc{
				Fn:          chplan.FnGroupArray,
				Combinators: []chplan.AggCombinator{chplan.CombIf},
				Args:        []chplan.Expr{&chplan.ColumnRef{Name: f}, histEqSideCond(histEqSideRHS)},
				Alias:       histCompareFieldAlias(f, histEqSideRHS),
			},
		)
	}
	return aggs
}

// histCompareFirstElem reads the single element [histCompareFieldAlias]'s
// groupArrayIf collection is guaranteed to hold.
func histCompareFirstElem(alias string) chplan.Expr {
	const firstArrayIndex = 1
	return &chplan.FuncCall{Fn: chplan.FnArrayElement, Args: []chplan.Expr{&chplan.ColumnRef{Name: alias}, &chplan.LitInt{V: firstArrayIndex}}}
}

// histogramCompareFieldsExpr renders the field-by-field comparison
// [FloatHistogram.Equals] performs — the AND of every
// [histogramCompareFieldColumns] field comparing equal for `==`, or its
// De Morgan dual (the OR of any one field comparing unequal) for `!=`
// (ne=true). Neither direction needs a NOT() wrapper.
//
// Shared by two callers with different uses for the same boolean:
// [histogramCompareHavingGuard] ANDs it with the both-sides-matched guard
// to decide which rows the non-bool structural filter KEEPS;
// [lowerExpHistogramHistogramCompareBoolBinop] wraps it in `toFloat64`
// directly as the `bool`-modifier VALUE, since reference keeps every
// matched pair there regardless of this expression's truth value.
func histogramCompareFieldsExpr(s schema.Metrics, ne bool) chplan.Expr {
	op, combine := chplan.OpEq, chplan.OpAnd
	if ne {
		op, combine = chplan.OpNe, chplan.OpOr
	}

	var fieldCmp chplan.Expr
	for _, f := range histogramCompareFieldColumns(s) {
		cond := &chplan.Binary{
			Op:    op,
			Left:  histCompareFirstElem(histCompareFieldAlias(f, histEqSideLHS)),
			Right: histCompareFirstElem(histCompareFieldAlias(f, histEqSideRHS)),
		}
		if fieldCmp == nil {
			fieldCmp = cond
			continue
		}
		fieldCmp = &chplan.Binary{Op: combine, Left: fieldCmp, Right: cond}
	}
	return fieldCmp
}

// histogramCompareHavingGuard renders `count() = 2 AND <field equality>`
// for `==`, or `count() = 2 AND <field inequality>` for `!=` (ne=true) —
// see [histogramCompareFieldsExpr] for the field-comparison half.
//
// The `count() = 2` guard is shared verbatim with `+`/`-`'s merge — see
// [histogramBinopBothSidesMatchedGuard]'s doc (histogram_native_binop.go)
// for why that alone implements one-to-one V-V matching's INNER JOIN
// semantics here too (both for default matching and, once
// [applyVectorMatchToHistogramOperand] has run, for on()/ignoring()).
func histogramCompareHavingGuard(s schema.Metrics, ne bool) chplan.Expr {
	return &chplan.Binary{Op: chplan.OpAnd, Left: histogramBinopBothSidesMatchedGuard(), Right: histogramCompareFieldsExpr(s, ne)}
}

// histogramCompareOutputProjections rebuilds the kept row's MetricName
// and nine histogram fields from LHS's own groupArrayIf collections —
// correct for both operators (see this file's header doc: `==`'s kept
// rows have L and R bit-identical by construction, `!=`'s kept output is
// defined as LHS's own value).
func histogramCompareOutputProjections(s schema.Metrics) []chplan.Projection {
	projs := []chplan.Projection{
		{Expr: histCompareFirstElem(histCompareFieldAlias(s.MetricNameColumn, histEqSideLHS)), Alias: s.MetricNameColumn},
	}
	for _, f := range histogramCompareFieldColumns(s) {
		projs = append(projs, chplan.Projection{
			Expr:  histCompareFirstElem(histCompareFieldAlias(f, histEqSideLHS)),
			Alias: f,
		})
	}
	return projs
}
