package promql

import (
	"fmt"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// defaultSubqueryStep is the step cerberus substitutes when a subquery
// omits an explicit `step` (`expr[5m:]`). Prom defines empty-step
// semantics as "use the engine's eval step"; cerberus doesn't thread
// that through lowering yet (M2.1 territory) so we hardcode 1m, which
// matches Prom's default eval step.
const defaultSubqueryStep = time.Minute

// lowerSubquery handles `<expr>[<range>:<step>]`. P0 4.5 scope: the
// inner is a `*parser.VectorSelector` (`up[5m:1m]`). Inner ranges over
// other call shapes (`rate(m[5m])[1h:5m]`) land in P0 4.6; outer
// range-vector functions over a subquery (`max_over_time(...)[1h:5m]`)
// land in P0 4.7.
//
// The lowered shape is a matrix-mode RangeWindow with Identity=true
// (the "last value in window" emission). Each anchor across
// `[End-OuterRange, End]` evaluates the inner selector by picking the
// last sample whose timestamp falls within `[anchor-Step, anchor]`.
func lowerSubquery(e *parser.SubqueryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if e.Range <= 0 {
		return nil, fmt.Errorf("promql: subquery range must be positive, got %s", e.Range)
	}
	step := e.Step
	if step == 0 {
		step = defaultSubqueryStep
	}
	if step < 0 {
		return nil, fmt.Errorf("promql: subquery step must be positive, got %s", e.Step)
	}

	switch inner := e.Expr.(type) {
	case *parser.VectorSelector:
		return lowerSubqueryOverVectorSelector(e, inner, step, s, ctx)
	case *parser.Call:
		return lowerSubqueryOverCall(e, inner, step, s, ctx)
	case *parser.ParenExpr:
		// `(<expr>)[5m:1m]` — unwrap and retry with the same subquery.
		// Build a synthetic SubqueryExpr around the inner expr so the
		// modifiers + range/step are preserved.
		inner2 := *e
		inner2.Expr = inner.Expr
		return lowerSubquery(&inner2, s, ctx)
	case *parser.AggregateExpr:
		return lowerSubqueryOverAggregate(e, inner, step, s, ctx)
	case *parser.BinaryExpr:
		return lowerSubqueryOverBinary(e, inner, step, s, ctx)
	case *parser.SubqueryExpr:
		return lowerSubqueryOverSubquery(e, inner, step, s, ctx)
	}
	return nil, fmt.Errorf("promql: subquery over %T is unsupported", e.Expr)
}

// lowerSubqueryOverCall — `<range-vector-fn>(<inner>[<inner_range>])[<outer_range>:<step>]`.
// The most common shape is `rate(m[5m])[1h:5m]`. Lowers to a single
// matrix-shape RangeWindow where:
//
//   - Func    = the inner range function (rate / increase / *_over_time)
//   - Range   = the inner matrix selector's range (the 5m in `rate(m[5m])`)
//   - OuterRange / Step come from the subquery
//
// I.e. the same RangeWindow IR that lowerRangeVectorCall produces for
// instant rate, but with OuterRange + Step populated to fan out N anchors.
func lowerSubqueryOverCall(
	sub *parser.SubqueryExpr,
	call *parser.Call,
	step time.Duration,
	s schema.Metrics,
	ctx lowerCtx,
) (chplan.Node, error) {
	if len(call.Args) != 1 {
		return nil, fmt.Errorf("promql: subquery inner %s expects exactly 1 argument, got %d",
			call.Func.Name, len(call.Args))
	}
	if innerSub, ok := call.Args[0].(*parser.SubqueryExpr); ok {
		// Nested subquery: `<fn>(<inner-sub>)[<outer-range>:<step>]`.
		// e.g. `max_over_time(rate(m[1m])[5m:30s])[1h:5m]`.
		return lowerSubqueryOverCallSubquery(sub, call, innerSub, step, s, ctx)
	}
	ms, ok := call.Args[0].(*parser.MatrixSelector)
	if !ok {
		return nil, fmt.Errorf("promql: subquery inner %s must wrap a MatrixSelector, got %T",
			call.Func.Name, call.Args[0])
	}
	vs, ok := ms.VectorSelector.(*parser.VectorSelector)
	if !ok {
		return nil, fmt.Errorf("promql: subquery inner matrix selector must wrap a VectorSelector, got %T",
			ms.VectorSelector)
	}

	// Strip the inner VS modifier — the subquery's own modifier shadows it.
	// inRangeVector also suppresses the bare-selector LWR wrap so every
	// in-window sample reaches the surrounding RangeWindow.
	vsNoModifier := *vs
	vsNoModifier.Timestamp = nil
	vsNoModifier.OriginalOffset = 0
	vsNoModifier.Offset = 0
	vsNoModifier.StartOrEnd = 0
	rangeCtx := ctx
	rangeCtx.inRangeVector = true
	inner, err := lowerVectorSelector(&vsNoModifier, s, rangeCtx)
	if err != nil {
		return nil, err
	}

	anchor, err := subqueryAnchor(sub, ctx)
	if err != nil {
		return nil, err
	}

	return &chplan.RangeWindow{
		Input:           inner,
		Func:            call.Func.Name,
		Range:           ms.Range,
		OuterRange:      sub.Range,
		Step:            step,
		End:             anchor.End,
		Offset:          anchor.Offset,
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     s.ValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}, nil
}

// lowerSubqueryOverBinary — `(<vec> op <scalar>)[range:step]` /
// `(<vec> op <vec>)[range:step]` lowering. The inner BinaryExpr is
// lowered in range-vector context so the LWR collapse is suppressed
// and every in-window sample reaches the wrapping matrix RangeWindow.
// The wrapping RangeWindow uses `Identity=true` — same shape as the
// bare-vector subquery case — to emit the "last value in window" per
// anchor across `[End - sub.Range, End]` spaced by `step`. Subquery
// `@`/`offset` modifiers thread onto the wrapper via subqueryAnchor.
//
// PromQL evaluates `(<expr>)[range:step]` by re-evaluating `<expr>`
// at each anchor; the BinaryExpr's per-sample `(Value op scalar)` /
// `(Value_L op Value_R)` rewrite or comparison-Filter drop is applied
// to every sample inside the window, and the wrapper picks the most
// recent one. Comparison ops without `bool` modifier still resolve to
// a Filter on the un-projected value, so the matrix RangeWindow sees
// only samples that satisfied the predicate — matching Prom's "drop
// non-matching samples then take the latest" semantics for
// `(up > 0.5)[5m:1m]`.
func lowerSubqueryOverBinary(
	sub *parser.SubqueryExpr,
	b *parser.BinaryExpr,
	step time.Duration,
	s schema.Metrics,
	ctx lowerCtx,
) (chplan.Node, error) {
	rangeCtx := ctx
	rangeCtx.inRangeVector = true
	inner, err := lowerBinary(b, s, rangeCtx)
	if err != nil {
		return nil, err
	}

	anchor, err := subqueryAnchor(sub, ctx)
	if err != nil {
		return nil, err
	}

	return &chplan.RangeWindow{
		Input:           inner,
		Identity:        true,
		Range:           step, // per-anchor lookback = subquery step
		OuterRange:      sub.Range,
		Step:            step,
		End:             anchor.End,
		Offset:          anchor.Offset,
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     s.ValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}, nil
}

// lowerSubqueryOverVectorSelector — `metric[range:step]` lowering.
//
// The subquery's own modifiers (offset, @) shadow any modifiers on the
// inner VectorSelector — Prom evaluates `up[5m:1m] offset 10m` as the
// subquery anchored at `now - 10m`, NOT at the inner VS's modifier
// (which is illegal on a subquery's inner anyway). We strip the
// inner's modifier before lowering and apply the subquery's own.
func lowerSubqueryOverVectorSelector(
	sub *parser.SubqueryExpr,
	vs *parser.VectorSelector,
	step time.Duration,
	s schema.Metrics,
	ctx lowerCtx,
) (chplan.Node, error) {
	vsNoModifier := *vs
	vsNoModifier.Timestamp = nil
	vsNoModifier.OriginalOffset = 0
	vsNoModifier.Offset = 0
	vsNoModifier.StartOrEnd = 0
	rangeCtx := ctx
	rangeCtx.inRangeVector = true
	inner, err := lowerVectorSelector(&vsNoModifier, s, rangeCtx)
	if err != nil {
		return nil, err
	}

	anchor, err := subqueryAnchor(sub, ctx)
	if err != nil {
		return nil, err
	}

	return &chplan.RangeWindow{
		Input:           inner,
		Identity:        true,
		Range:           step, // per-anchor lookback = subquery step
		OuterRange:      sub.Range,
		Step:            step,
		End:             anchor.End,
		Offset:          anchor.Offset,
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     s.ValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}, nil
}

// lowerOuterRangeFnOverSubquery — `max_over_time(rate(m[5m])[1h:5m])`,
// the canonical Grafana subquery shape. The outer call is a
// range-vector function reducing over the inner matrix output.
//
// IR is a chained RangeWindow:
//
//	RangeWindow{
//	  Func:       <outer fn name>,      // "max_over_time", "sum_over_time", …
//	  Range:      <subquery range>,     // the full inner matrix lookback
//	  Step:       0,                    // instant — single value per series
//	  Input:      RangeWindow{          // matrix from lowerSubquery
//	    Func:       <inner fn name>,
//	    Range:      <inner matrix range>,
//	    OuterRange: <subquery range>,
//	    Step:       <subquery step>,
//	    ...,
//	  },
//	  TimestampColumn: "anchor_ts",     // inner matrix's per-row anchor
//	  ValueColumn:     s.ValueColumn,   // inner matrix emits s.ValueColumn
//	}
//
// The outer's TimestampColumn / ValueColumn point at the inner matrix
// output columns rather than the underlying table's TimeUnix/Value.
// The inner matrix uses `anchor_ts` for the per-row anchor and emits
// the per-window value under `s.ValueColumn` (the schema's canonical
// PascalCase `Value` — the windowed-array emitter projects `r.ValueColumn`
// at every outer SELECT site since the fix to chsql.range_window in
// commit 1 of this PR).
func lowerOuterRangeFnOverSubquery(
	outer *parser.Call,
	sub *parser.SubqueryExpr,
	s schema.Metrics,
	ctx lowerCtx,
) (chplan.Node, error) {
	if _, ok := rangeVectorFn[outer.Func.Name]; !ok {
		return nil, fmt.Errorf("promql: %s does not accept a subquery argument", outer.Func.Name)
	}

	inner, err := lowerSubquery(sub, s, ctx)
	if err != nil {
		return nil, err
	}

	rw := &chplan.RangeWindow{
		Input:           inner,
		Func:            outer.Func.Name,
		Range:           sub.Range,
		TimestampColumn: "anchor_ts",
		ValueColumn:     s.ValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}
	// Range mode: the outer reducer in `max_over_time(rate(m[5m])[1h:5m])`
	// must fan across the request's step grid so each anchor in
	// [start, end] emits its own per-window reduction. Without this the
	// outer RangeWindow keeps Step=0 and collapses to a single anchor at
	// end_ts (compat-lane: 502 / single-point matrix). Mirrors the
	// `lowerRangeVectorCall` matrix fan-out introduced for bare
	// range-vector calls — the same gate (ctx.step > 0 with start/end
	// threaded through LowerAtRange) applies here.
	if ctx.step > 0 && !ctx.start.IsZero() && !ctx.end.IsZero() {
		rw.Start = ctx.start.UTC()
		rw.End = ctx.end.UTC()
		rw.Step = ctx.step
		rw.OuterRange = ctx.end.Sub(ctx.start)
		// Widen the inner subquery's anchor grid so it covers the union
		// of the outer evaluation window plus one full subquery range.
		// Without this, the inner stays anchored at `now64(9) - sub.Range`
		// (`lowerSubqueryOverCall`'s `End` defaults to zero because
		// `subqueryAnchor` only honours `@start()` / `@end()`). For every
		// outer anchor `t` in `[ctx.start, ctx.end]` the outer's
		// per-anchor filter `(t - sub.Range, t]` reads the inner's
		// `anchor_ts` column — so the inner anchor grid must extend
		// across `[ctx.start - sub.Range, ctx.end]` or the filter falls
		// outside the emitted inner anchors and the matrix collapses to
		// an empty matrix. Compat-lane manifestation:
		// `avg_over_time(rate(demo_cpu_usage_seconds_total[1m])[2m:10s])`
		// returned [] vs Prom's populated matrix (`#400` Bucket 3).
		// The Identity-true sibling (`max_over_time((<binary>)[5m:10s])`)
		// also benefits from the same widening — the inner's anchor
		// grid is the same shape, the bug just manifests less obviously
		// because the binary inner doesn't carry the `>= 2 samples`
		// guard that the counter-extrapolation path adds.
		if innerRW, ok := inner.(*chplan.RangeWindow); ok {
			innerRW.Start = ctx.start.Add(-sub.Range).UTC()
			innerRW.End = ctx.end.UTC()
			innerRW.OuterRange = ctx.end.Sub(ctx.start) + sub.Range
		}
	}
	return rw, nil
}

// rangeVectorFn is the set of PromQL functions cerberus's emitter
// handles as range-vector reducers. Subquery-argument lowering only
// fires for these.
var rangeVectorFn = map[string]struct{}{
	"rate":            {},
	"increase":        {},
	"delta":           {},
	"sum_over_time":   {},
	"avg_over_time":   {},
	"min_over_time":   {},
	"max_over_time":   {},
	"count_over_time": {},
	"last_over_time":  {},
}

// subqueryAnchor reads the subquery's `@` + `offset` modifiers into an
// evalAnchor. Mirrors anchorFromSelector for SubqueryExpr's identical
// modifier fields.
func subqueryAnchor(e *parser.SubqueryExpr, ctx lowerCtx) (evalAnchor, error) {
	a := evalAnchor{Offset: e.OriginalOffset}
	switch e.StartOrEnd {
	case parser.START:
		if ctx.start.IsZero() {
			return evalAnchor{}, fmt.Errorf("promql: subquery `@ start()` modifier requires query range context (use LowerAt)")
		}
		a.End = ctx.start.UTC()
	case parser.END:
		if ctx.end.IsZero() {
			return evalAnchor{}, fmt.Errorf("promql: subquery `@ end()` modifier requires query range context (use LowerAt)")
		}
		a.End = ctx.end.UTC()
	case 0:
		// no start/end modifier
	default:
		return evalAnchor{}, fmt.Errorf("promql: unexpected subquery StartOrEnd token %v", e.StartOrEnd)
	}
	if e.Timestamp != nil {
		a.End = time.UnixMilli(*e.Timestamp).UTC()
	}
	return a, nil
}

// lowerSubqueryOverAggregate — `<agg>(<inner>)[<outer_range>:<step>]`.
//
// The canonical Grafana shape is
// `max_over_time(sum by(job)(rate(http_requests_total[1m]))[1h:30s])` —
// at each anchor `t` across `[End - sub.Range, End]` spaced by `step`,
// evaluate `<agg>(<inner>)` at `t`.
//
// For per-bucket reducers (sum / count / avg / min / max / quantile)
// the lowered tree is a Project[Aggregate[matrix-RangeWindow]]:
//
//	Project[Attributes = map('<label>', gkey_0, ...), anchor_ts, value]
//	  Aggregate[GroupBy: [<by/without-keys>, anchor_ts], AggFuncs: [<op>(value) AS value]]
//	    RangeWindow[matrix shape: per-(series, anchor) Inner-call output]
//	      <Filter/Scan from the AggregateExpr.Expr lowering>
//
// The outer Project re-exposes the canonical (Attributes, anchor_ts,
// value) shape so a wrapping RangeWindow (the `max_over_time(...)`)
// can group by Attributes and window over anchor_ts/value without
// caring that the underlying series identity came from a `by(...)` /
// `without(...)` clause rather than the raw scan's Attributes map.
//
// Shape-changing aggregations (`topk` / `bottomk` / `count_values`) are
// dispatched to dedicated helpers — topk/bottomk preserve every input
// label and emit a TopK plan node, count_values builds a synthetic
// label from the per-bucket value via toString().
func lowerSubqueryOverAggregate(
	sub *parser.SubqueryExpr,
	agg *parser.AggregateExpr,
	step time.Duration,
	s schema.Metrics,
	ctx lowerCtx,
) (chplan.Node, error) {
	switch agg.Op {
	case parser.TOPK, parser.BOTTOMK:
		return lowerSubqueryOverTopK(sub, agg, step, s, ctx)
	case parser.COUNT_VALUES:
		return lowerSubqueryOverCountValues(sub, agg, step, s, ctx)
	case parser.SUM, parser.COUNT, parser.AVG, parser.MIN, parser.MAX, parser.QUANTILE:
		// Per-bucket reducers — sum/count/avg/min/max/quantile over the
		// matrix value column produce one value per (anchor, group-tuple)
		// row.
	default:
		return nil, fmt.Errorf("promql: subquery over %s aggregation is not supported",
			agg.Op.String())
	}

	// Lower the aggregate's inner argument as a matrix-shape subquery
	// (OuterRange + Step set). Produces (Attributes, anchor_ts, value)
	// rows — one per (series, anchor) bucket — that the wrapping
	// Aggregate groups across.
	matrix, err := lowerSubqueryInnerMatrix(sub, agg.Expr, step, s, ctx)
	if err != nil {
		return nil, err
	}

	// Build the Aggregate's GroupBy. For `by(l1, l2, ...)` one MapAccess
	// per named label; for `without(l1, l2, ...)` a single MapWithoutKeys
	// spanning the full Attributes map. Plus the per-anchor key so the
	// reducer fires once per (anchor, group-tuple). Mirrors lower.go's
	// `aggregateGroupBy` for the basic / `without` symmetry.
	groupBy, groupAliases, err := subqueryAggregateGroupBy(agg, s)
	if err != nil {
		return nil, err
	}
	const anchorAlias = "anchor_ts"
	groupBy = append(groupBy, &chplan.ColumnRef{Name: anchorAlias})
	groupAliases = append(groupAliases, anchorAlias)

	// Build the AggFunc. The matrix RangeWindow emits its windowed
	// value under `s.ValueColumn` (the schema's canonical PascalCase
	// `Value`); the Aggregate's output column reuses the same alias so
	// the outer RangeWindow can reference it transparently via its
	// `ValueColumn = s.ValueColumn`.
	aggFunc, err := buildSubqueryAggFunc(agg, s.ValueColumn)
	if err != nil {
		return nil, err
	}

	innerAgg := &chplan.Aggregate{
		Input:              matrix,
		GroupBy:            groupBy,
		GroupByAliases:     groupAliases,
		AggFuncs:           []chplan.AggFunc{aggFunc},
		DropEmptyOnNoGroup: true,
	}

	// Rebuild Attributes from the gkey aliases so the outer
	// RangeWindow's `GroupBy: [ColumnRef("Attributes")]` lights up
	// without further plumbing. anchor_ts + s.ValueColumn pass through
	// as matching aliases. Value is cast to Float64 so the outer
	// RangeWindow's counter_delta arithmetic (always emitted even by
	// arrayAvg / arrayMax / arrayMin reducers) can do `c - p` without
	// hitting CH's NO_COMMON_TYPE error on count's UInt64 result.
	groupKeyAliases := groupAliases[:len(groupAliases)-1]
	attrsExpr := buildAttributesFromAggregate(agg, groupKeyAliases)
	wrapped := chplan.Node(&chplan.Project{
		Input: innerAgg,
		Projections: []chplan.Projection{
			{Expr: attrsExpr, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: anchorAlias}, Alias: anchorAlias},
			{
				Expr: &chplan.FuncCall{
					Name: "toFloat64",
					Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ValueColumn}},
				},
				Alias: s.ValueColumn,
			},
		},
	})
	// `quantile(phi, V)` with phi outside [0, 1] is well-defined in
	// PromQL — phi<0 → -Inf, phi>1 → +Inf. CH's `quantile` aggregate
	// rejects out-of-range phi at the wire layer, so buildSubqueryAggFunc
	// has already clamped the emitted phi to 0.5; wrap the Project's
	// Value column in the PromQL-spec Inf constant so the per-bucket
	// output matches Prom's funcQuantile semantics. Mirrors
	// `lowerAggregate`'s `projectValueOverInner` shim — the subquery
	// aggregate's 3-column output (Attributes, anchor_ts, Value) needs a
	// matching 3-column wrap rather than the instant-mode 4-column
	// (MetricName, Attributes, TimeUnix, Value) shape.
	if agg.Op == parser.QUANTILE {
		if phi, ok := tryScalarLiteral(agg.Param); ok {
			if infValue, outOfRange := outOfRangePhiInf(phi); outOfRange {
				wrapped = &chplan.Project{
					Input: wrapped,
					Projections: []chplan.Projection{
						{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
						{Expr: &chplan.ColumnRef{Name: anchorAlias}, Alias: anchorAlias},
						{Expr: &chplan.LitFloat{V: infValue}, Alias: s.ValueColumn},
					},
				}
			}
		}
	}
	return wrapped, nil
}

// subqueryAggregateGroupBy returns the (GroupBy, aliases) pair for the
// subquery-over-aggregate's inner Aggregate. Mirrors lower.go's
// `aggregateGroupBy` shape so the by / without symmetry stays
// consistent: `by(l1, l2)` produces N `Attributes[lN] AS gkey_N`
// columns; `without(l1, l2)` produces a single `MapWithoutKeys(...) AS
// gkey_0` column; `without()` (empty Grouping) collapses to a bare
// `Attributes AS gkey_0` reference because CH's `mapFilter(_ -> NOT (k
// IN ()))` rejects an empty IN-list.
func subqueryAggregateGroupBy(agg *parser.AggregateExpr, s schema.Metrics) ([]chplan.Expr, []string, error) {
	switch {
	case agg.Without && len(agg.Grouping) == 0:
		return []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}}, []string{"gkey_0"}, nil
	case agg.Without:
		return []chplan.Expr{&chplan.MapWithoutKeys{
				Map:  &chplan.ColumnRef{Name: s.AttributesColumn},
				Keys: append([]string(nil), agg.Grouping...),
			}},
			[]string{"gkey_0"}, nil
	}
	groupBy := make([]chplan.Expr, 0, len(agg.Grouping))
	aliases := make([]string, 0, len(agg.Grouping))
	for i, label := range agg.Grouping {
		groupBy = append(groupBy, &chplan.MapAccess{
			Map: &chplan.ColumnRef{Name: s.AttributesColumn},
			Key: &chplan.LitString{V: label},
		})
		aliases = append(aliases, fmt.Sprintf("gkey_%d", i))
	}
	return groupBy, aliases, nil
}

// lowerSubqueryOverTopK — `(topk|bottomk)(K, <inner>)[<outer_range>:<step>]`.
//
// topk/bottomk preserve every input label — `by(...)` / `without(...)`
// only partitions. The lowered tree is a TopK over the matrix
// RangeWindow, with the partition key including the per-anchor column
// so K series are selected per (group-tuple, anchor) bucket. The
// matrix already emits the canonical (Attributes, anchor_ts, Value)
// row shape; TopK preserves those columns so a wrapping RangeWindow
// (`max_over_time(topk(3, rate(m[1m]))[5m:30s])`) can window over
// anchor_ts / Value without further reshaping.
func lowerSubqueryOverTopK(
	sub *parser.SubqueryExpr,
	agg *parser.AggregateExpr,
	step time.Duration,
	s schema.Metrics,
	ctx lowerCtx,
) (chplan.Node, error) {
	kF, ok := tryScalarLiteral(agg.Param)
	if !ok {
		return nil, fmt.Errorf("promql: subquery over %s requires a scalar literal K", agg.Op.String())
	}
	if kF < 0 || kF != float64(int64(kF)) {
		return nil, fmt.Errorf("promql: subquery over %s K must be a non-negative integer literal, got %v",
			agg.Op.String(), kF)
	}
	if kF == 0 {
		return nil, fmt.Errorf("promql: subquery over %s K must be > 0", agg.Op.String())
	}

	matrix, err := lowerSubqueryInnerMatrix(sub, agg.Expr, step, s, ctx)
	if err != nil {
		return nil, err
	}

	// Partition list: by/without keys (in TopK form — see lower.go's
	// lowerTopK) PLUS anchor_ts so the LIMIT K BY fires per outer-anchor
	// bucket. Without the anchor key, K series would be selected once
	// across the whole matrix instead of K per evaluation step.
	var by []chplan.Expr
	switch {
	case agg.Without && len(agg.Grouping) == 0:
		by = []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}}
	case agg.Without:
		by = []chplan.Expr{&chplan.MapWithoutKeys{
			Map:  &chplan.ColumnRef{Name: s.AttributesColumn},
			Keys: append([]string(nil), agg.Grouping...),
		}}
	default:
		by = make([]chplan.Expr, 0, len(agg.Grouping))
		for _, label := range agg.Grouping {
			by = append(by, &chplan.MapAccess{
				Map: &chplan.ColumnRef{Name: s.AttributesColumn},
				Key: &chplan.LitString{V: label},
			})
		}
	}
	by = append(by, &chplan.ColumnRef{Name: "anchor_ts"})

	return &chplan.TopK{
		Input:    matrix,
		K:        int64(kF),
		By:       by,
		SortExpr: &chplan.ColumnRef{Name: s.ValueColumn},
		Desc:     agg.Op == parser.TOPK,
		Columns: []string{
			s.AttributesColumn,
			"anchor_ts",
			s.ValueColumn,
		},
	}, nil
}

// lowerSubqueryOverCountValues — `count_values("label", <inner>) [by/without (g)] [<outer_range>:<step>]`.
//
// For each distinct value of `<inner>` (within each partition + anchor)
// emit a row whose Attributes carry the unique value as a synthetic
// label binding (`<label>=<stringified value>`) plus the preserved
// per-partition labels, and whose Value is the count of input series
// hitting that value at the anchor. Mirrors lower.go's
// `lowerCountValues` but adds the per-anchor column to the GROUP BY so
// the reducer fires once per (anchor, partition, distinct value).
func lowerSubqueryOverCountValues(
	sub *parser.SubqueryExpr,
	agg *parser.AggregateExpr,
	step time.Duration,
	s schema.Metrics,
	ctx lowerCtx,
) (chplan.Node, error) {
	label, ok := tryStringLiteral(agg.Param)
	if !ok {
		return nil, fmt.Errorf("promql: count_values requires a string-literal label name as the first arg")
	}
	if label == "" {
		return nil, fmt.Errorf("promql: count_values requires a non-empty label name")
	}

	matrix, err := lowerSubqueryInnerMatrix(sub, agg.Expr, step, s, ctx)
	if err != nil {
		return nil, err
	}

	const (
		valueKeyAlias = "cv_val"
		countAlias    = "cv_count"
		anchorAlias   = "anchor_ts"
	)

	// Partition-key list (matches lower.go's lowerCountValues by/without
	// branches), followed by the synthetic value-as-label key and the
	// per-anchor key so the count reducer fires once per (partition,
	// distinct value, anchor).
	var (
		groupBy []chplan.Expr
		aliases []string
	)
	switch {
	case agg.Without && len(agg.Grouping) == 0:
		groupBy = []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}}
		aliases = []string{"gkey_0"}
	case agg.Without:
		groupBy = []chplan.Expr{&chplan.MapWithoutKeys{
			Map:  &chplan.ColumnRef{Name: s.AttributesColumn},
			Keys: append([]string(nil), agg.Grouping...),
		}}
		aliases = []string{"gkey_0"}
	default:
		groupBy = make([]chplan.Expr, 0, len(agg.Grouping))
		aliases = make([]string, 0, len(agg.Grouping))
		for i, lbl := range agg.Grouping {
			groupBy = append(groupBy, &chplan.MapAccess{
				Map: &chplan.ColumnRef{Name: s.AttributesColumn},
				Key: &chplan.LitString{V: lbl},
			})
			aliases = append(aliases, fmt.Sprintf("gkey_%d", i))
		}
	}
	groupBy = append(groupBy, &chplan.FuncCall{
		Name: "toString",
		Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ValueColumn}},
	})
	aliases = append(aliases, valueKeyAlias)
	groupBy = append(groupBy, &chplan.ColumnRef{Name: anchorAlias})
	aliases = append(aliases, anchorAlias)

	innerAgg := &chplan.Aggregate{
		Input:          matrix,
		GroupBy:        groupBy,
		GroupByAliases: aliases,
		// Alias count() as cv_count (not Value) so CH's name resolution
		// in the GROUP BY clause doesn't pick up the aggregate alias
		// when it sees `toString(Value)` — CH otherwise errors with
		// `Aggregate function count() AS Value is found in GROUP BY`.
		// The outer Project re-aliases cv_count back to Value.
		AggFuncs: []chplan.AggFunc{
			{Name: "count", Args: []chplan.Expr{}, Alias: countAlias},
		},
		// count_values returns one row per distinct value per anchor;
		// empty input naturally produces no rows.
		DropEmptyOnNoGroup: false,
	}

	// Build the Attributes expression for the wrapping Project.
	// Mirrors lower.go's lowerCountValues: `without(...)` overlays the
	// synthetic binding onto the partition map via mapConcat;
	// `by(...)` / no grouping rebuilds the partition map by string-
	// literal pairs and wraps with MapWithoutEmptyValues to drop
	// unset-label slots.
	var attrs chplan.Expr
	switch {
	case agg.Without:
		attrs = &chplan.FuncCall{
			Name: "mapConcat",
			Args: []chplan.Expr{
				&chplan.ColumnRef{Name: "gkey_0"},
				&chplan.FuncCall{
					Name: "map",
					Args: []chplan.Expr{
						&chplan.LitString{V: label},
						&chplan.ColumnRef{Name: valueKeyAlias},
					},
				},
			},
		}
	default:
		mapArgs := make([]chplan.Expr, 0, (len(agg.Grouping)+1)*2)
		for i, lbl := range agg.Grouping {
			mapArgs = append(mapArgs,
				&chplan.LitString{V: lbl},
				&chplan.ColumnRef{Name: aliases[i]},
			)
		}
		mapArgs = append(mapArgs,
			&chplan.LitString{V: label},
			&chplan.ColumnRef{Name: valueKeyAlias},
		)
		attrs = &chplan.MapWithoutEmptyValues{
			Map: &chplan.FuncCall{Name: "map", Args: mapArgs},
		}
	}

	return &chplan.Project{
		Input: innerAgg,
		Projections: []chplan.Projection{
			{Expr: attrs, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: anchorAlias}, Alias: anchorAlias},
			{
				Expr: &chplan.FuncCall{
					Name: "toFloat64",
					Args: []chplan.Expr{&chplan.ColumnRef{Name: countAlias}},
				},
				Alias: s.ValueColumn,
			},
		},
	}, nil
}

// lowerSubqueryInnerMatrix produces a matrix-shape RangeWindow for the
// expression that lives inside an `<agg>[range:step]` clause's
// aggregate. Recurses through ParenExpr; dispatches Call /
// VectorSelector to the same matrix-emitting helpers
// lowerSubqueryOverCall / lowerSubqueryOverVectorSelector use.
//
// Only shapes that already produce per-anchor matrix output are
// supported — extending coverage to BinaryExpr / nested aggregations
// would need additional plan-tree shaping the matrix emitter can't
// currently consume.
func lowerSubqueryInnerMatrix(
	sub *parser.SubqueryExpr,
	expr parser.Expr,
	step time.Duration,
	s schema.Metrics,
	ctx lowerCtx,
) (chplan.Node, error) {
	switch inner := expr.(type) {
	case *parser.ParenExpr:
		return lowerSubqueryInnerMatrix(sub, inner.Expr, step, s, ctx)
	case *parser.Call:
		return lowerSubqueryOverCall(sub, inner, step, s, ctx)
	case *parser.VectorSelector:
		return lowerSubqueryOverVectorSelector(sub, inner, step, s, ctx)
	}
	return nil, fmt.Errorf("promql: subquery over aggregation of %T is unsupported", expr)
}

// buildSubqueryAggFunc maps a PromQL AggregateExpr to the chplan
// AggFunc that runs INSIDE the subquery-over-aggregate pipeline.
// Mirrors buildAggFunc but takes the value column name as a parameter
// — used for both the input (the matrix RangeWindow's emitted value
// column) and the output alias (so a wrapping RangeWindow can
// reference the aggregate output via its ValueColumn). Callers pass
// `s.ValueColumn` (the schema-canonical `Value`) so the inner / outer
// references stay consistent end-to-end.
func buildSubqueryAggFunc(a *parser.AggregateExpr, valCol string) (chplan.AggFunc, error) {
	valueArg := &chplan.ColumnRef{Name: valCol}
	switch a.Op {
	case parser.SUM:
		return chplan.AggFunc{Name: "sum", Args: []chplan.Expr{valueArg}, Alias: valCol}, nil
	case parser.COUNT:
		return chplan.AggFunc{Name: "count", Args: []chplan.Expr{valueArg}, Alias: valCol}, nil
	case parser.AVG:
		return chplan.AggFunc{Name: "avg", Args: []chplan.Expr{valueArg}, Alias: valCol}, nil
	case parser.MIN:
		return chplan.AggFunc{Name: "min", Args: []chplan.Expr{valueArg}, Alias: valCol}, nil
	case parser.MAX:
		return chplan.AggFunc{Name: "max", Args: []chplan.Expr{valueArg}, Alias: valCol}, nil
	case parser.QUANTILE:
		phi, ok := tryScalarLiteral(a.Param)
		if !ok {
			return chplan.AggFunc{}, fmt.Errorf("promql: subquery over quantile(phi, ...) requires a scalar literal phi")
		}
		// CH's `quantile(phi)` aggregate errors on phi outside [0, 1].
		// The lowerSubqueryOverAggregate caller post-Projects the Value
		// column to ±Inf for out-of-range phi (matching Prom's
		// funcQuantile semantics) so the clamped value here is never
		// observed in the final output. Mirrors lower.go's buildAggFunc.
		emitPhi := phi
		if _, outOfRange := outOfRangePhiInf(phi); outOfRange {
			emitPhi = 0.5
		}
		return chplan.AggFunc{
			Name:   "quantile",
			Params: []chplan.Expr{&chplan.LitFloat{V: emitPhi}},
			Args:   []chplan.Expr{valueArg},
			Alias:  valCol,
		}, nil
	}
	return chplan.AggFunc{}, fmt.Errorf("promql: subquery over %s aggregation is not supported", a.Op.String())
}

// buildAttributesFromAggregate rebuilds an Attributes map literal from
// the gkey_N aliases produced by the subquery's inner Aggregate. The
// result lets a wrapping RangeWindow group by `Attributes` without
// needing to know that the underlying identity came from a `by(...)`
// or `without(...)` clause.
//
// `by(label1, label2)` → `map('label1', gkey_0, 'label2', gkey_1)`.
// `by()` (no labels) → empty `Map(String,String)` literal.
// `without(...)` / `without()` → bare `gkey_0` (the single
// MapWithoutKeys-derived column already carries the partition map).
func buildAttributesFromAggregate(agg *parser.AggregateExpr, gkeyAliases []string) chplan.Expr {
	if agg.Without {
		// `without(...)` / `without()` — partition map is the single
		// MapWithoutKeys / bare Attributes column at gkey_0.
		return &chplan.ColumnRef{Name: gkeyAliases[0]}
	}
	if len(agg.Grouping) == 0 {
		return &chplan.FuncCall{
			Name: "CAST",
			Args: []chplan.Expr{
				&chplan.FuncCall{Name: "map", Args: nil},
				&chplan.LitString{V: "Map(String,String)"},
			},
		}
	}
	args := make([]chplan.Expr, 0, len(agg.Grouping)*2)
	for i, label := range agg.Grouping {
		args = append(args,
			&chplan.LitString{V: label},
			&chplan.ColumnRef{Name: gkeyAliases[i]},
		)
	}
	return &chplan.FuncCall{Name: "map", Args: args}
}

// lowerSubqueryOverSubquery handles `<inner-sub>[<outer-range>:<step>]` —
// a SubqueryExpr whose body is itself a *parser.SubqueryExpr.
//
// PromQL's parser type system forbids this shape: SubqueryExpr.Expr
// must evaluate to an instant vector and a SubqueryExpr produces a
// range vector. So a parser-produced AST will never reach here.
// We still handle the shape defensively to keep the lowering pipeline
// total over the AST node space (e.g. for AST built programmatically
// by an optimizer rewrite, or for any future parser change that
// relaxes the type check).
//
// Semantics: the inner subquery's matrix is treated as the source of
// per-(series, t_inner) samples; the outer subquery picks the latest
// in-window sample per outer anchor. We widen the inner's `Range` to
// cover the union of outer + inner ranges so every outer anchor's
// lookback finds inner anchors (same trick as
// `lowerSubqueryOverCallSubquery`), then wrap with an Identity-mode
// RangeWindow on the outer's step grid (same shape as
// `lowerSubqueryOverVectorSelector`).
func lowerSubqueryOverSubquery(
	sub *parser.SubqueryExpr,
	innerSub *parser.SubqueryExpr,
	step time.Duration,
	s schema.Metrics,
	ctx lowerCtx,
) (chplan.Node, error) {
	if innerSub.Range <= 0 {
		return nil, fmt.Errorf("promql: inner subquery range must be positive, got %s", innerSub.Range)
	}

	widened := *innerSub
	widened.Range = sub.Range + innerSub.Range
	wideInner, err := lowerSubquery(&widened, s, ctx)
	if err != nil {
		return nil, err
	}

	anchor, err := subqueryAnchor(sub, ctx)
	if err != nil {
		return nil, err
	}

	return &chplan.RangeWindow{
		Input:           wideInner,
		Identity:        true,
		Range:           step,
		OuterRange:      sub.Range,
		Step:            step,
		End:             anchor.End,
		Offset:          anchor.Offset,
		TimestampColumn: "anchor_ts",
		ValueColumn:     s.ValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}, nil
}

// lowerSubqueryOverCallSubquery handles the nested-subquery shape
// `<outer-fn>(<inner-sub>)[<outer-range>:<step>]`. Canonical example:
// `max_over_time(rate(m[1m])[5m:30s])[1h:5m]`.
//
// Conceptual evaluation: at each outer anchor `t_outer` ∈
// `[End - sub.Range, End]` spaced by `step`, evaluate
// `<outer-fn>(<inner-sub>)` at `t_outer` — which is itself
// `<outer-fn>` over `<inner-sub>` evaluated at the inner anchors
// across `[t_outer - innerSub.Range, t_outer]` spaced by
// `innerSub.Step`.
//
// We widen the inner subquery's `Range` to cover the union of both
// outer + inner ranges, then wrap with a matrix RangeWindow that
// reduces per outer anchor. The widened inner emits one row per
// (series, t_inner) at innerSub.Step resolution across
// `[End - (sub.Range + innerSub.Range), End]`; the outer matrix
// groupArrays per Attributes and arrayFilters to
// `[t_outer - innerSub.Range, t_outer]` per outer anchor before
// applying the outer-fn reducer.
func lowerSubqueryOverCallSubquery(
	sub *parser.SubqueryExpr,
	call *parser.Call,
	innerSub *parser.SubqueryExpr,
	step time.Duration,
	s schema.Metrics,
	ctx lowerCtx,
) (chplan.Node, error) {
	if _, ok := rangeVectorFn[call.Func.Name]; !ok {
		return nil, fmt.Errorf("promql: %s does not accept a subquery argument", call.Func.Name)
	}
	if innerSub.Range <= 0 {
		return nil, fmt.Errorf("promql: inner subquery range must be positive, got %s", innerSub.Range)
	}

	// Widen the inner subquery to cover the outer range PLUS the inner
	// range so every outer anchor's lookback finds inner anchors. Each
	// per-outer-anchor reduction then arrayFilters to the inner-range
	// window — see emitWindowedArrayMatrix.
	widened := *innerSub
	widened.Range = sub.Range + innerSub.Range
	wideInner, err := lowerSubquery(&widened, s, ctx)
	if err != nil {
		return nil, err
	}

	anchor, err := subqueryAnchor(sub, ctx)
	if err != nil {
		return nil, err
	}

	return &chplan.RangeWindow{
		Input:           wideInner,
		Func:            call.Func.Name,
		Range:           innerSub.Range,
		OuterRange:      sub.Range,
		Step:            step,
		End:             anchor.End,
		Offset:          anchor.Offset,
		TimestampColumn: "anchor_ts",
		ValueColumn:     s.ValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}, nil
}
