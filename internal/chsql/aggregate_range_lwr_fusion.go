package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// This file fuses a chplan.Aggregate directly over a chplan.RangeLWR — the
// PromQL `sum by (label) (<bare selector>)` / `count by (label) (<bare
// selector>)` shape over a `query_range` window — instead of letting
// emitAggregate wrap RangeLWR's full rendered SQL as one more opaque
// subquery layer on top of RangeLWR's own three nested SELECTs. See
// https://github.com/tsouza/cerberus/issues/2442 for the motivating
// production measurement (a `sum by (reason)` gauge aggregation over
// 7,024 series paying the FULL per-series argMax collapse cost even
// though the outer aggregation reduces to 8 distinct `reason` values).
//
// TWO fused shapes ship here, deliberately NOT a general "fuse any
// aggregate function" rewrite — see each constructor's doc comment for
// why the other PromQL/CH aggregates (avg/min/max/quantile/stddev/...)
// are NOT safe to fuse the same way and are left on the unfused path:
//
//   - sum(Value): rangeLWRFusionSum. Correctness requires each series'
//     OWN latest in-window sample to still be individually selected via
//     argMax before the values are summed — two different series landing
//     on the same outer group key must NOT be conflated. So this fusion
//     keeps RangeLWR's per-series collapse GROUP BY (MetricName,
//     Attributes, anchor_ts) exactly as-is; it only removes the
//     REDUNDANT rename-only SELECT layer RangeLWR's own outer projection
//     and emitAggregate's opaque-subquery wrap would otherwise stack on
//     top of each other, merging "re-alias the collapsed columns" and
//     "group + sum them" into one SELECT instead of two nested ones.
//     This does NOT shrink the (series × anchor) intermediate — that
//     cardinality is inherent to "pick each series' own latest sample"
//     and cannot be reduced without correlating across series, which sum
//     must not do. It is a real, provably-safe simplification, just a
//     modest one: expect a smaller win than count's.
//
//   - count(Value): rangeLWRFusionCount. This one DOES eliminate the
//     per-series collapse's (series × anchor) intermediate entirely — see
//     emitAggregateRangeLWRFusedDistinctCount's doc for the soundness
//     argument (uniqExact of the series-identity tuple, computed directly
//     on RangeLWR's fan-out rows, exactly answers "how many distinct
//     series had an in-window sample" without ever needing to know WHICH
//     sample each series contributed).
//
// Both fusions are pure emitter-level SQL-shape changes: neither
// introduces a new chplan.Node kind, changes RangeLWR's or Aggregate's
// field contracts, or touches the plan tree at all — the chplan IR a
// query lowers to is byte-identical whether or not the fusion fires, so
// none of chplan's per-node-kind registration surfaces (RewriteChildren,
// the slice-invariant walk, clone, NumAnchors/resource-bound accounting,
// the solver's slicer, ...) need a new case. That is a deliberate scope
// decision, not an oversight: a THIRD, more aggressive fusion — folding
// the outer low-cardinality key directly into RangeLWR's own collapse
// GROUP BY as a dedicated plan shape — was investigated and set aside;
// see the issue for the write-up of why it needs a new node type (and
// the registration surface that comes with one) to do safely, which is
// more design than this PR's correctness bar can respectably absorb in
// one pass.

// rangeLWRFusionKind distinguishes which of the two fused renderings
// [matchRangeLWRFusion] selected.
type rangeLWRFusionKind int

const (
	rangeLWRFusionNone rangeLWRFusionKind = iota
	rangeLWRFusionSum
	rangeLWRFusionCount
)

// matchRangeLWRFusion reports whether a is eligible for the RangeLWR
// fusion fast path and, if so, which kind. Every check below is a
// precondition the two emitAggregateRangeLWRFused* renderers rely on
// without re-verifying — declining (returning rangeLWRFusionNone) always
// falls back to emitAggregate's normal opaque-subquery path, which is
// unconditionally correct, so any doubt here should resolve to declining.
func matchRangeLWRFusion(a *chplan.Aggregate) (*chplan.RangeLWR, rangeLWRFusionKind) {
	lwr, ok := a.Input.(*chplan.RangeLWR)
	if !ok {
		return nil, rangeLWRFusionNone
	}
	// SampleTimestamp publishes a 5th column neither fused rendering
	// accounts for below — decline rather than silently drop it.
	if lwr.SampleTimestamp {
		return nil, rangeLWRFusionNone
	}
	// Having operates on the AGGREGATE's own output; the fused SELECT
	// would need a wrapping HAVING clause of its own to honor it and
	// neither renderer below builds one — decline.
	if a.Having != nil {
		return nil, rangeLWRFusionNone
	}
	// The bucket-key translation (see isRangeLWRBucketKey) and the
	// distinct-count/argMax rewrites both assume a non-empty GroupBy — the
	// empty-GroupBy shape is emitAggregateNoGroup's guarded 2-SELECT
	// idiom, a completely different rendering this file does not attempt.
	if len(a.GroupBy) == 0 {
		return nil, rangeLWRFusionNone
	}
	// Every GroupBy entry must carry a non-empty alias: the fused
	// SELECT-list always projects each key under a.GroupByAliases[i], and
	// [groupKeyFrags] (shared with the ordinary Aggregate path) GROUPs BY
	// that same alias. An empty alias would fall back to re-rendering the
	// RAW expression in the GROUP BY clause — which, for the translated
	// bucket key, would re-evaluate `lwr.TimestampCol` against the
	// fan-out/collapse row scope (the RAW per-sample timestamp, not the
	// anchor) instead of the anchor value the SELECT-list actually
	// projected. Requiring an alias everywhere sidesteps that hazard
	// entirely rather than special-casing it.
	if len(a.GroupByAliases) != len(a.GroupBy) {
		return nil, rangeLWRFusionNone
	}
	for _, alias := range a.GroupByAliases {
		if alias == "" {
			return nil, rangeLWRFusionNone
		}
	}
	for _, g := range a.GroupBy {
		if !rangeLWRFusionSafeGroupKey(g, lwr) {
			return nil, rangeLWRFusionNone
		}
	}

	if len(a.AggFuncs) != 1 {
		return nil, rangeLWRFusionNone
	}
	af := a.AggFuncs[0]
	if len(af.Combinators) != 0 || len(af.Params) != 0 {
		return nil, rangeLWRFusionNone
	}
	if len(af.Args) != 1 {
		return nil, rangeLWRFusionNone
	}
	arg, ok := af.Args[0].(*chplan.ColumnRef)
	if !ok || arg.Qualifier != "" || arg.Name != lwr.ValueCol {
		return nil, rangeLWRFusionNone
	}

	switch af.Fn {
	case chplan.FnSum:
		return lwr, rangeLWRFusionSum
	case chplan.FnCount:
		return lwr, rangeLWRFusionCount
	default:
		// avg/min/max/quantile/stddev/... are deliberately NOT in this
		// switch — see this file's header comment for why only sum and
		// count admit a fusion this file has proven correct.
		return nil, rangeLWRFusionNone
	}
}

// rangeLWRFusionSafeGroupKey reports whether g is safe to evaluate
// against RangeLWR's fan-out/collapse row scope in place of the
// Aggregate's original scope (RangeLWR's own canonical output). Two
// shapes qualify:
//
//   - isRangeLWRBucketKey: the range-mode per-step bucket key
//     lowerAggregate injects as `&chplan.ColumnRef{Name: s.TimestampColumn}`
//     — translated to the fan-out/collapse stage's own anchor column by
//     the caller, not evaluated as a generic expression.
//   - Any expression that reads ONLY lwr.MetricNameCol / lwr.AttributesCol
//     (bare, unqualified ColumnRefs) — both columns pass through
//     RangeLWR's fan-out AND collapse stages unchanged (same name, same
//     per-row value, never renamed or aggregated), so such an expression
//     evaluates identically whether it runs against RangeLWR's canonical
//     output or against the fan-out/collapse row scope directly. This is
//     the same "passthrough" reasoning
//     internal/optimizer/transpose_shared.go's onlyReferencesPassthrough
//     uses to push a Filter through an Aggregate/RangeWindow boundary,
//     applied here to push a GroupBy key DOWN through RangeLWR instead.
//
// A ScalarSubquery / InSubquery / BoundedTraceScope reachable inside g is
// rejected outright rather than inspected further: none of those are
// expected in a PromQL `by(...)`/`without(...)` grouping key, and this
// checker has no way to prove one doesn't reach outside the allowed
// column set.
func rangeLWRFusionSafeGroupKey(g chplan.Expr, lwr *chplan.RangeLWR) bool {
	if isRangeLWRBucketKey(g, lwr) {
		return true
	}
	safe := true
	chplan.InspectExpr(g, func(sub chplan.Expr) bool {
		switch v := sub.(type) {
		case *chplan.ColumnRef:
			if v.Qualifier != "" || (v.Name != lwr.MetricNameCol && v.Name != lwr.AttributesCol) {
				safe = false
				return false
			}
		case *chplan.ScalarSubquery, *chplan.InSubquery, *chplan.BoundedTraceScope:
			safe = false
			return false
		}
		return true
	})
	return safe
}

// isRangeLWRBucketKey reports whether g is exactly the bare, unqualified
// `&chplan.ColumnRef{Name: lwr.TimestampCol}` lowerAggregate injects as
// the range-mode per-step group key (rangeBucketAlias in
// internal/promql/lower.go). Both fused renderers translate this one
// entry to the fan-out/collapse stage's own [rangeLWRAnchorColumn]
// instead of evaluating it as a generic expression, because in that row
// scope `lwr.TimestampCol` still names the RAW per-sample timestamp
// column (every fan-out row keeps its source sample's own timestamp
// alongside the anchor it was fanned to) — the anchor RangeLWR's
// canonical output reports under TimestampCol lives under
// rangeLWRAnchorColumn until the (skipped, in the fused path) outer
// re-alias SELECT renames it.
func isRangeLWRBucketKey(g chplan.Expr, lwr *chplan.RangeLWR) bool {
	cr, ok := g.(*chplan.ColumnRef)
	return ok && cr.Qualifier == "" && cr.Name == lwr.TimestampCol
}

// emitAggregateRangeLWRFused renders the fused SELECT for the shape
// [matchRangeLWRFusion] matched. a.GroupBy / a.GroupByAliases are used
// verbatim for the SELECT-list + GROUP BY (translating only the bucket
// key, per isRangeLWRBucketKey); the AggFuncs[0].Alias names the fused
// reducer's output column exactly as the unfused path would have.
func (e *emitter) emitAggregateRangeLWRFused(a *chplan.Aggregate, lwr *chplan.RangeLWR, kind rangeLWRFusionKind) error {
	switch kind {
	case rangeLWRFusionSum:
		return e.emitAggregateRangeLWRFusedCollapse(a, lwr)
	case rangeLWRFusionCount:
		return e.emitAggregateRangeLWRFusedDistinctCount(a, lwr)
	default:
		return fmt.Errorf("%w: unresolved RangeLWR fusion kind", ErrUnsupported)
	}
}

// groupBySelectFrags renders a's GroupBy list into the fused SELECT's
// key columns, one SelectAs per entry, translating the bucket key (see
// isRangeLWRBucketKey) to rangeLWRAnchorColumn and rendering every other
// entry as-is (safe per rangeLWRFusionSafeGroupKey's passthrough proof).
func groupBySelectFrags(sb *QueryBuilder, a *chplan.Aggregate, lwr *chplan.RangeLWR) {
	for i, g := range a.GroupBy {
		alias := a.GroupByAliases[i]
		if isRangeLWRBucketKey(g, lwr) {
			sb.SelectAs(verbatim(rangeLWRAnchorColumn), alias)
			continue
		}
		expr := g
		sb.SelectAs(func(b *Builder) { _ = b.Expr(expr) }, alias)
	}
}

// emitAggregateRangeLWRFusedCollapse renders the sum() fusion: RangeLWR's
// fan-out + per-series argMax collapse stay exactly as
// [emitter.rangeLWRCollapseFrag] builds them for the unfused path — this
// fusion changes nothing about how each series' latest in-window sample
// is selected — but the outer GROUP BY + sum() read the collapse Frag
// DIRECTLY instead of through RangeLWR's separate rename-only outer
// SELECT (anchor_ts → TimeUnix, lwr_value → Value) that emitAggregate's
// ordinary opaque-subquery wrap would otherwise sit on top of. One
// SELECT does the rename AND the grouping/summing that used to be two.
func (e *emitter) emitAggregateRangeLWRFusedCollapse(a *chplan.Aggregate, lwr *chplan.RangeLWR) error {
	collapse, err := e.rangeLWRCollapseFrag(lwr)
	if err != nil {
		return err
	}
	sb := NewQuery().From(collapse)
	groupBySelectFrags(sb, a, lwr)

	sum := a.AggFuncs[0]
	sum.Args = []chplan.Expr{&chplan.ColumnRef{Name: rangeLWRValueAlias}}
	sb.Select(aggFuncFrag(sum))

	sb.GroupBy(groupKeyFrags(a.GroupBy, a.GroupByAliases)...)
	return e.emitSelect(sb)
}

// emitAggregateRangeLWRFusedDistinctCount renders the count() fusion.
//
// PromQL `count()` over a bare-selector range query counts the number of
// series with an in-window value at each anchor — after RangeLWR's
// collapse, that is exactly the row count per (outer key, anchor_ts)
// group, since the collapse emits AT MOST one row per (series, anchor).
//
// The key soundness fact this fusion relies on: determining WHETHER a
// series has an in-window sample at a given anchor does NOT require
// picking WHICH of that series' samples landed there — only whether at
// least one fanned row for that series exists. RangeLWR's fan-out stage
// (rangeLWRFanoutFrag) already fans every qualifying raw sample to every
// anchor whose staleness window contains it, so "series X contributed
// >=1 fanned row to anchor A" holds if and only if X had >=1 in-window
// sample at A — independent of how many raw samples fan into that same
// bucket (a series scraped faster than the staleness lookback fans
// SEVERAL rows per anchor; uniqExact collapses them back to one).
//
// So `uniqExact(tuple(MetricName, Attributes))` grouped by (outer key,
// anchor_ts), computed DIRECTLY on the fan-out rows, exactly reproduces
// "count of distinct series present" WITHOUT ever materialising
// RangeLWR's per-series argMax collapse — the (series × anchor)
// intermediate the unfused path (and the sum() fusion above) cannot
// avoid is not needed at all for count(). tuple(MetricName, Attributes)
// mirrors the exact pair RangeLWR's OWN collapse GROUP BY uses as its
// series-identity key (chplan.RangeLWR's doc: "GROUP BY (series,
// anchor_ts)"), so this fusion counts precisely the same set of series
// the unfused path would have.
//
// uniqExact returns UInt64; the driver's `*float64` Scan on
// chclient.Sample.Value fails on an unwrapped UInt64 exactly the way the
// plain count() path used to before intReturningAggregates existed (see
// that map's doc comment). This renderer applies its own toFloat64(...)
// wrap directly — deliberately NOT by adding FnUniqExact to the shared
// intReturningAggregates map, which aggFuncFrag also serves for
// histogram_quantile_window.go's unrelated windowSampleCountAgg (an
// internal minSamplesFilter column that is never scanned into
// Sample.Value and so must NOT pick up the wrap as a side effect of a
// change scoped to this fusion).
func (e *emitter) emitAggregateRangeLWRFusedDistinctCount(a *chplan.Aggregate, lwr *chplan.RangeLWR) error {
	fanout, err := e.rangeLWRFanoutFrag(lwr)
	if err != nil {
		return err
	}
	sb := NewQuery().From(fanout)
	groupBySelectFrags(sb, a, lwr)

	// Alias left empty so aggFuncFrag renders the bare
	// `uniqExact(tuple(...))` call with no `AS` clause — the toFloat64
	// wrap and the real alias are applied here instead, outside
	// aggFuncFrag, precisely so this local wrap never touches the shared
	// intReturningAggregates table (see the doc comment above).
	distinctSeries := chplan.AggFunc{
		Fn: chplan.FnUniqExact,
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Fn: chplan.FnTuple,
				Args: []chplan.Expr{
					&chplan.ColumnRef{Name: lwr.MetricNameCol},
					&chplan.ColumnRef{Name: lwr.AttributesCol},
				},
			},
		},
	}
	sb.Select(As(Call("toFloat64", aggFuncFrag(distinctSeries)), a.AggFuncs[0].Alias))

	sb.GroupBy(groupKeyFrags(a.GroupBy, a.GroupByAliases)...)
	return e.emitSelect(sb)
}
