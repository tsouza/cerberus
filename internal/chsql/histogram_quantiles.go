package chsql

import (
	"fmt"
	"math"

	"github.com/tsouza/cerberus/internal/chplan"
)

const (
	hqLevelsRawColumn = "_cerb_hq_levels_raw"
	hqLevelsColumn    = "_cerb_hq_levels"
	hqLevelColumn     = "_cerb_hq_level"
)

// emitHistogramQuantiles prepares one classic bucket ladder and expands all
// requested constant quantiles from it. The plural ClickHouse aggregate is
// used when the singular native aggregate was selected; otherwise the legacy
// Prometheus-compatible interpolation shares every preparation stage and only
// repeats the irreducibly phi-dependent rank search.
func (e *emitter) emitHistogramQuantiles(q *chplan.HistogramQuantiles) error {
	if q.Histogram == nil || q.Histogram.Input == nil || len(q.Levels) == 0 {
		return fmt.Errorf("%w: HistogramQuantiles requires a histogram input and levels", ErrUnsupported)
	}
	h := *q.Histogram
	counts, err := histogramFieldChildColumn("HistogramQuantiles", h.Input, chplan.HistogramFieldBucketCounts, false)
	if err != nil {
		return err
	}
	bounds, err := histogramFieldChildColumn("HistogramQuantiles", h.Input, chplan.HistogramFieldExplicitBounds, false)
	if err != nil {
		return err
	}
	h.BucketCountsColumn, h.ExplicitBoundsColumn = counts, bounds
	sub, err := e.subqueryFrag(h.Input)
	if err != nil {
		return err
	}

	raw := newHQClassicWriters(&h, hqClassicHelperColumns{})
	scanned := NewQuery().Select(
		Star(),
		As(raw.keptBoundIdx(), hqClassicKeptIdxColumn),
		As(raw.buckets(), hqClassicBucketsColumn),
	).From(materializeHistogramInput(sub, h.Input.RowType()))
	helpers := hqClassicHelperColumns{keptIdx: hqClassicKeptIdxColumn, buckets: hqClassicBucketsColumn}
	coalescing := newHQClassicWriters(&h, helpers)
	coalesced := NewQuery().Select(
		Star(),
		As(coalescing.bounds(), hqClassicBoundsColumn),
		As(coalescing.cum(), hqClassicCumColumn),
	).From(Subquery(scanned))
	helpers.bounds, helpers.cum = hqClassicBoundsColumn, hqClassicCumColumn
	counted := NewQuery().Select(
		Star(),
		As(newHQClassicWriters(&h, helpers).observations(), hqClassicObservationsColumn),
	).From(Subquery(coalesced))
	helpers.observations = hqClassicObservationsColumn

	var valued *QueryBuilder
	if h.UseNativeQuantileAggregate {
		rawValues := NewQuery().Select(Star(), As(sharedPrometheusQuantilesAggregate(&h, q.Levels, helpers), hqLevelsRawColumn)).From(Subquery(counted))
		valued = NewQuery().Select(Star(), As(sharedPrometheusQuantilesValues(&h, q.Levels, helpers), hqLevelsColumn)).From(Subquery(rawValues))
	} else {
		values := make([]Frag, len(q.Levels))
		for i, level := range q.Levels {
			levelHistogram := h
			levelHistogram.Phi, levelHistogram.PhiExpr = level.Phi, nil
			values[i] = histogramQuantileValueFrag(&levelHistogram, helpers)
		}
		valued = NewQuery().Select(Star(), As(Array(values...), hqLevelsColumn)).From(Subquery(counted))
	}

	labels := make([]Frag, len(q.Levels))
	for i, level := range q.Levels {
		labels[i] = InlineLit(level.Label)
	}
	expanded := NewQuery().Select(Star(), As(
		Call("arrayJoin", Call("arrayZip", Array(labels...), Col(hqLevelsColumn))),
		hqLevelColumn,
	)).From(Subquery(valued))

	query := NewQuery().From(Subquery(expanded))
	for i, group := range h.GroupBy {
		alias := ""
		if i < len(h.GroupByAliases) {
			alias = h.GroupByAliases[i]
		}
		groupFrag := func(b *Builder) { _ = b.Expr(group) }
		if alias == h.AttributesColumn {
			query.Select(As(Call("mapConcat", groupFrag, Call("map", InlineLit(q.LabelName), TupleIndex(Col(hqLevelColumn), 1))), alias))
		} else {
			query.SelectAs(groupFrag, alias)
		}
	}
	query.Select(As(TupleIndex(Col(hqLevelColumn), 2), "Value"))
	return e.emitSelect(query)
}

func sharedPrometheusQuantilesAggregate(h *chplan.HistogramQuantile, levels []chplan.HistogramQuantileLevel, helpers hqClassicHelperColumns) Frag {
	w := newHQClassicWriters(h, helpers)
	nameParts := []Frag{InlineLit("quantilesPrometheusHistogram(")}
	for i, level := range levels {
		// The ClickHouse aggregate's levels must lie in [0, 1]; an
		// out-of-domain or NaN phi is answered by sharedPrometheusQuantilesValues.
		phi := 0.0
		if !math.IsNaN(level.Phi) {
			phi = min(max(level.Phi, 0), 1)
		}
		if i > 0 {
			nameParts = append(nameParts, InlineLit(","))
		}
		nameParts = append(nameParts, Call("toString", InlineLit(phi)))
	}
	nameParts = append(nameParts, InlineLit(")"))
	aggregate := Call("concat", nameParts...)
	inf := verbatim("inf")
	bounds := Call("arrayPushBack", w.bounds(), inf)
	counts := If(Neq(w.cumCount(), w.boundCount()), w.cum(), Call("arrayPushBack", w.cum(), w.observations()))
	return Call("arrayReduce", aggregate, bounds, counts)
}

func sharedPrometheusQuantilesValues(h *chplan.HistogramQuantile, levels []chplan.HistogramQuantileLevel, helpers hqClassicHelperColumns) Frag {
	w := newHQClassicWriters(h, helpers)
	nan, negInf, posInf := verbatim("nan"), verbatim("-inf"), verbatim("inf")
	values := make([]Frag, len(levels))
	for i, level := range levels {
		value := Subscript(Col(hqLevelsRawColumn), InlineLit(i+1))
		switch {
		case math.IsNaN(level.Phi):
			value = nan
		case level.Phi < 0:
			value = negInf
		case level.Phi > 1:
			value = posInf
		}
		values[i] = If(Eq(w.lengthBC(), InlineLit(0)), nan, If(Eq(w.observations(), InlineLit(0)), nan, value))
	}
	return Array(values...)
}

// emitHistogramQuantilesNative shares the expensive exponential-histogram
// bucket normalisation, cumulative walks and populated-bound discovery. Each
// phi still has its own rank position and interpolation, but those operate on
// the shared prepared arrays instead of rebuilding the histogram N times.
func (e *emitter) emitHistogramQuantilesNative(q *chplan.HistogramQuantilesNative) error {
	if q.Histogram == nil || q.Histogram.Input == nil || len(q.Levels) == 0 {
		return fmt.Errorf("%w: HistogramQuantilesNative requires a histogram input and levels", ErrUnsupported)
	}
	h := *q.Histogram
	fields := []struct {
		field    chplan.HistogramField
		target   *string
		optional bool
	}{
		{chplan.HistogramFieldCount, &h.CountColumn, false},
		{chplan.HistogramFieldSum, &h.SumColumn, false},
		{chplan.HistogramFieldScale, &h.ScaleColumn, false},
		{chplan.HistogramFieldZeroThreshold, &h.ZeroThresholdColumn, true},
		{chplan.HistogramFieldZeroCount, &h.ZeroCountColumn, false},
		{chplan.HistogramFieldPositiveOffset, &h.PositiveOffsetColumn, false},
		{chplan.HistogramFieldPositiveBucketCounts, &h.PositiveBucketCountsColumn, false},
		{chplan.HistogramFieldNegativeOffset, &h.NegativeOffsetColumn, false},
		{chplan.HistogramFieldNegativeBucketCounts, &h.NegativeBucketCountsColumn, false},
	}
	for _, requirement := range fields {
		column, err := histogramFieldChildColumn("HistogramQuantilesNative", h.Input, requirement.field, requirement.optional)
		if err != nil {
			return err
		}
		*requirement.target = column
	}
	sub, err := e.subqueryFrag(h.Input)
	if err != nil {
		return err
	}

	needsReverse := false
	for _, level := range q.Levels {
		levelHistogram := h
		levelHistogram.Phi, levelHistogram.PhiExpr = level.Phi, nil
		needsReverse = needsReverse || reachesReverseArm(&levelHistogram)
	}
	labels := make([]Frag, len(q.Levels))
	for i, level := range q.Levels {
		labels[i] = InlineLit(level.Label)
	}
	// Every level reads the same once-bound walk arrays (hqNativeBindPrepared)
	// inside this single SELECT; only the rank position and interpolation are
	// per level.
	values := hqNativeBindPrepared(&h, needsReverse, func(helpers hqNativeHelperColumns) Frag {
		perLevel := make([]Frag, len(q.Levels))
		for i, level := range q.Levels {
			levelHistogram := h
			levelHistogram.Phi, levelHistogram.PhiExpr = level.Phi, nil
			perLevel[i] = histogramQuantileNativeValueFrag(&levelHistogram, helpers)
		}
		return Array(perLevel...)
	})
	expanded := NewQuery().Select(Star(), As(
		Call("arrayJoin", Call("arrayZip", Array(labels...), values)),
		hqLevelColumn,
	)).From(sub)
	query := NewQuery().From(Subquery(expanded))
	for i, group := range h.GroupBy {
		alias := ""
		if i < len(h.GroupByAliases) {
			alias = h.GroupByAliases[i]
		}
		groupFrag := func(b *Builder) { _ = b.Expr(group) }
		if alias == h.AttributesColumn {
			query.Select(As(Call("mapConcat", groupFrag, Call("map", InlineLit(q.LabelName), TupleIndex(Col(hqLevelColumn), 1))), alias))
		} else {
			query.SelectAs(groupFrag, alias)
		}
	}
	query.Select(As(TupleIndex(Col(hqLevelColumn), 2), "Value"))
	return e.emitSelect(query)
}
