package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_binop_match.go generalizes the two histogram-histogram
// binop lowerings — histogram_native_binop.go's `+`/`-` merge and
// histogram_native_binop_eq.go's `==`/`!=` structural-equality filter —
// from default (full label set) vector matching to on()/ignoring()
// matching (cerberus issue #2273, gap 1). `group_left()`/`group_right()`
// (many-to-one broadcast) stay unsupported: see
// [isSupportedHistogramMatchCardinality].
//
// Both callers already collapse each operand to one row per FULL
// Attributes key ([lowerExpHistogramValuedOperand]'s
// *chplan.HistogramProjection cap) before merging/comparing the two
// sides via a `UnionAll` + single `Aggregate` grouped on the (by
// default, full) Attributes column with a `having count() = 2` guard
// standing in for VectorJoin's INNER JOIN (see histogram_native_binop.go's
// header doc). That guard is only sound when each side is guaranteed at
// most one row per GROUP KEY — true by construction for the full-
// Attributes default case, but not for an on()/ignoring() SUBSET key,
// where two distinct series can share one reduced key.
//
// [applyVectorMatchToHistogramOperand] restores that guarantee ahead of
// the merge/compare stage: it re-groups one operand by the on()/ignoring()
// reduced key, mirroring chsql's VectorJoin `roleOne` per-side aggregation
// (internal/chsql/vector_join.go) — including its runtime
// `throwIf(uniqExact(<original Attributes>) > 1, ...)` ambiguity guard,
// which surfaces reference's own "many-to-many matching not allowed"
// error (ManyToManyMatchMessage) at ClickHouse query-execution time
// rather than silently dropping (or arbitrarily picking) an ambiguous
// group. Once every operand has been through this prepare step, its
// Attributes column already IS the match key, so the existing merge/
// compare Aggregate's `identity = Attributes` grouping needs no further
// change — on()/ignoring() and default matching produce byte-identical
// downstream SQL shapes, just fed a differently-prepared operand.
//
// Default matching is deliberately a no-op passthrough
// ([isDefaultMatching]): each operand's own HistogramProjection already
// groups by the full Attributes map, so re-grouping by that same key
// would only add a redundant Aggregate stage.

// isSupportedHistogramMatchCardinality reports whether vm's cardinality
// is a histogram-binop merge/compare can honour today: one-to-one,
// covering both default matching (vm == nil) and any on()/ignoring()
// modifier ([applyVectorMatchToHistogramOperand] handles those via a
// reduced-key regroup). `group_left()`/`group_right()` — cerberus issue
// #2273's carved-out remainder — set vm.Card to CardManyToOne /
// CardOneToMany and are rejected by callers of this function; the
// many-to-one broadcast that cardinality implies needs a materially
// different shape (one side keeps per-series granularity and gets the
// other side's histogram broadcast onto every matching row) that neither
// the `+`/`-` merge nor the `==`/`!=` compare implements.
func isSupportedHistogramMatchCardinality(vm *parser.VectorMatching) bool {
	return vm == nil || vm.Card == parser.CardOneToOne
}

// histogramMatchKeyAggExpr translates vm's on()/ignoring() modifier into
// the *parser.AggregateExpr shape [histogramAggGroupBy] already knows how
// to turn into a GroupBy key + Attributes-rebuild expression, mirroring
// on()'s "match on exactly these labels" as `by(...)` and ignoring()'s
// "match on everything but these" as `without(...)` — the same Keep/Del
// duality reference's own `resultMetric` (promql/engine.go) applies for
// one-to-one V-V matching. Only called once [isDefaultMatching] has ruled
// out the full-Attributes case, so the empty-list edge cases it still
// must handle are genuine ones: `on()` with no labels collapses every
// series into a single group (histogramAggGroupBy's `by` empty-list arm),
// matching reference's "match on nothing, one group" semantics for a bare
// `on()`.
func histogramMatchKeyAggExpr(vm *parser.VectorMatching) *parser.AggregateExpr {
	return &parser.AggregateExpr{Without: !vm.On, Grouping: append([]string(nil), vm.MatchingLabels...)}
}

// applyVectorMatchToHistogramOperand re-groups hp by vm's on()/ignoring()
// reduced match key ahead of the merge/compare stage, with a runtime
// ambiguity guard — see this file's header doc. Default matching is a
// no-op: hp already carries one row per full-Attributes key, matching the
// eventual merge/compare Aggregate's own default grouping, so re-grouping
// again would only add a redundant stage.
//
// The nine raw histogram columns ride through via `any()`: the ambiguity
// HAVING guard has already aborted every group whose rows disagree on the
// original (unreduced) Attributes, and each side was already deduplicated
// to one row per full-Attributes key upstream, so a surviving reduced-key
// group holds rows that all share the identical original Attributes value
// — meaning exactly one underlying row. `any()` therefore picks that one
// row's own value, not an arbitrary one among several — the same
// "guard-then-any" pattern [duplicate_labelset_guard.go] and
// [duplicateLabelsetGuardExpr] apply for the unrelated name-drop /
// label-rewrite collision guards.
func applyVectorMatchToHistogramOperand(hp *chplan.HistogramProjection, vm *parser.VectorMatching, s schema.Metrics, ctx lowerCtx) *chplan.HistogramProjection {
	if isDefaultMatching(vm) {
		return hp
	}
	stepAligned := ctx.step > 0

	// hp's own SQL-level output publishes its nine histogram fields under
	// the FIXED chplan.Histogram*Column aliases (HistogramCount,
	// HistogramScale, ...), not the raw schema.Metrics column names those
	// aliases are derived from — histogramProjectionSchema is the
	// existing translation every hp-consuming lowering in this package
	// applies before reading hp's own output columns back (see
	// mergeTwoHistogramProjections / compareTwoHistogramProjections's own
	// `histSchema := histogramProjectionSchema(s)` first line). MetricName /
	// Attributes / Timestamp are NOT remapped — hp's GroupByAliases keeps
	// those under their original schema names — so `s` (not histSchema)
	// is still correct for those three.
	histSchema := histogramProjectionSchema(s)

	groupBy, groupByAliases, attrsRebuild := histogramAggGroupBy(
		histogramMatchKeyAggExpr(vm), &chplan.ColumnRef{Name: s.AttributesColumn}, s,
	)
	if stepAligned {
		groupBy = append(groupBy, &chplan.ColumnRef{Name: s.TimestampColumn})
		groupByAliases = append(groupByAliases, s.TimestampColumn)
	}

	const preparedNameAlias = "_hq_match_name"
	const preparedTsAlias = "_hq_match_ts"
	aggs := []chplan.AggFunc{
		{Fn: chplan.FnAny, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.MetricNameColumn}}, Alias: preparedNameAlias},
	}
	if !stepAligned {
		// Step-aligned mode already put TimestampColumn in the group key
		// (each anchor keeps its own row); instant mode needs it carried
		// through as a value, same as MetricName.
		aggs = append(aggs, chplan.AggFunc{
			Fn: chplan.FnAny, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.TimestampColumn}}, Alias: preparedTsAlias,
		})
	}
	for _, col := range histogramCompareFieldColumns(histSchema) {
		aggs = append(aggs, chplan.AggFunc{Fn: chplan.FnAny, Args: []chplan.Expr{&chplan.ColumnRef{Name: col}}, Alias: col})
	}

	agg := &chplan.Aggregate{
		Input:              hp,
		GroupBy:            groupBy,
		GroupByAliases:     groupByAliases,
		AggFuncs:           aggs,
		Having:             histogramMatchAmbiguityGuardExpr(s.AttributesColumn),
		DropEmptyOnNoGroup: true,
	}

	tsExpr := chplan.Expr(&chplan.ColumnRef{Name: s.TimestampColumn})
	if !stepAligned {
		tsExpr = &chplan.ColumnRef{Name: preparedTsAlias}
	}
	projs := []chplan.Projection{
		{Expr: &chplan.ColumnRef{Name: preparedNameAlias}, Alias: s.MetricNameColumn},
		{Expr: attrsRebuild, Alias: s.AttributesColumn},
		{Expr: tsExpr, Alias: s.TimestampColumn},
	}
	for _, col := range histogramCompareFieldColumns(histSchema) {
		projs = append(projs, chplan.Projection{Expr: &chplan.ColumnRef{Name: col}, Alias: col})
	}

	// nativeHistogramProjection maps its CountColumn/SumColumn/... args
	// onto the FIXED Histogram*Column output aliases again — passing
	// histSchema (whose fields already ARE those fixed names) makes this
	// a same-name passthrough, matching how mergeTwoHistogramProjections
	// / compareTwoHistogramProjections cap their own reshaped output.
	return nativeHistogramProjection(
		&chplan.Project{Input: agg, Projections: projs},
		&chplan.ColumnRef{Name: s.MetricNameColumn},
		&chplan.ColumnRef{Name: s.TimestampColumn},
		histSchema,
	)
}

// histogramMatchAmbiguityGuardExpr renders the runtime uniqueness guard
// `throwIf(uniqExact(mapSort(<attrsCol>)) > 1, ManyToManyMatchMessage) =
// 0` — the same ManyToManyMatchMessage chsql's VectorJoin plants via
// matchCheckGuardFrag (internal/chsql/vector_join.go) for its own
// on()/ignoring() roleOne side, so a histogram binop's ambiguous match
// surfaces through the identical HTTP classification
// (chsql.IsManyToManyMatchError, wired in api/prom/handler.go) reference
// Prometheus's own "many-to-many matching not allowed" error gets.
//
// The Map argument is wrapped in [chplan.CanonicalAttributesExpr]: a
// ClickHouse Map compares positionally, so two deliveries of one logical
// series under different physical key orders would otherwise register as
// two DISTINCT values and trip a false ambiguity — the same reason
// chsql's own canonicalMatchKeyFrag wraps its uniqExact argument.
//
// It must be a HAVING predicate, not a SELECT-list column — ClickHouse's
// analyzer prunes a subquery SELECT-list expression nothing outside the
// subquery reads, throwIf side effect included, which is why
// duplicateLabelsetGuardExpr and matchCheckGuardFrag both make the same
// choice. `throwIf` returns 0 on success, so `= 0` is the gate.
func histogramMatchAmbiguityGuardExpr(attrsCol string) chplan.Expr {
	return &chplan.Binary{
		Op: chplan.OpEq,
		Left: &chplan.FuncCall{
			Fn: chplan.FnThrowIf,
			Args: []chplan.Expr{
				&chplan.Binary{
					Op: chplan.OpGt,
					Left: &chplan.FuncCall{
						Fn:   chplan.FnUniqExact,
						Args: []chplan.Expr{chplan.CanonicalAttributesExpr(&chplan.ColumnRef{Name: attrsCol})},
					},
					Right: &chplan.LitInt{V: 1},
				},
				&chplan.InlineString{V: chplan.ManyToManyMatchMessage},
			},
		},
		Right: &chplan.LitInt{V: 0},
	}
}
