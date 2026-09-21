package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// emitHistogramQuantileRankWalkNative applies the ClickHouse aggregate to one
// prepared bucket array per input row. arrayReduce keeps histogram identity
// intact without a second ARRAY JOIN / GROUP BY, and lets the native function
// perform its own rank search instead of searching once before calling it.
func (e *emitter) emitHistogramQuantileRankWalkNative(h *chplan.HistogramQuantile) error {
	if h.Input == nil {
		return fmt.Errorf("%w: HistogramQuantile.Input is nil", ErrUnsupported)
	}
	if h.BucketCountsColumn == "" || h.ExplicitBoundsColumn == "" {
		return fmt.Errorf("%w: HistogramQuantile requires BucketCountsColumn and ExplicitBoundsColumn", ErrUnsupported)
	}
	sub, err := e.subqueryFrag(h.Input)
	if err != nil {
		return err
	}
	raw := newHQClassicWriters(h, hqClassicHelperColumns{})
	scanned := NewQuery().Select(
		Star(),
		As(raw.keptBoundIdx(), hqClassicKeptIdxColumn),
		As(raw.buckets(), hqClassicBucketsColumn),
	).From(materializeHistogramInput(sub, h.Input.RowType()))
	helpers := hqClassicHelperColumns{keptIdx: hqClassicKeptIdxColumn, buckets: hqClassicBucketsColumn}
	coalescing := newHQClassicWriters(h, helpers)
	coalesced := NewQuery().Select(
		Star(),
		As(coalescing.bounds(), hqClassicBoundsColumn),
		As(coalescing.cum(), hqClassicCumColumn),
	).From(Subquery(scanned))
	helpers.bounds = hqClassicBoundsColumn
	helpers.cum = hqClassicCumColumn
	counted := NewQuery().Select(
		Star(),
		As(newHQClassicWriters(h, helpers).observations(), hqClassicObservationsColumn),
	).From(Subquery(coalesced))
	helpers.observations = hqClassicObservationsColumn
	query := NewQuery().From(Subquery(counted))
	for i, group := range h.GroupBy {
		alias := ""
		if i < len(h.GroupByAliases) {
			alias = h.GroupByAliases[i]
		}
		query.SelectAs(func(b *Builder) { _ = b.Expr(group) }, alias)
	}
	query.SelectAs(histogramQuantileRankWalkNativeValueFrag(h, helpers), "Value")
	return e.emitSelect(query)
}

// Duplicate finite bounds and cumulative monotonicity are handled by the same
// preparation as the legacy implementation. ClickHouse additionally requires
// exactly one terminal +Inf rung and a finite phi parameter in [0,1]. Preserve
// Prometheus's empty/zero-total and phi-domain answers outside the aggregate.
func histogramQuantileRankWalkNativeValueFrag(h *chplan.HistogramQuantile, helpers hqClassicHelperColumns) Frag {
	w := newHQClassicWriters(h, helpers)
	nan, negInf, posInf := verbatim("nan"), verbatim("-inf"), verbatim("inf")
	zero, one := InlineLit(0.0), InlineLit(1.0)
	phi := Call("greatest", zero, Call("least", one, If(Call("isNaN", w.phi()), zero, w.phi())))
	aggregate := Call("concat", InlineLit("quantilePrometheusHistogram("), Call("toString", phi), InlineLit(")"))
	bounds := Call("arrayPushBack", w.bounds(), posInf)
	counts := If(Neq(w.cumCount(), w.boundCount()), w.cum(), Call("arrayPushBack", w.cum(), w.observations()))
	value := Call("arrayReduce", aggregate, bounds, counts)
	core := If(Eq(w.lengthBC(), InlineLit(0)), nan,
		If(Eq(w.observations(), InlineLit(0)), nan,
			If(Lt(w.phi(), zero), negInf, If(Gt(w.phi(), one), posInf, value))))
	if h.PhiExpr != nil {
		return If(Call("isNaN", w.phi()), nan, core)
	}
	return core
}
