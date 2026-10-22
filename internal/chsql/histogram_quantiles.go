package chsql

import (
	"fmt"
	"math"
	"strconv"

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
	if q.Histogram == nil || len(q.Levels) == 0 {
		return fmt.Errorf("%w: HistogramQuantiles requires a histogram and levels", ErrUnsupported)
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

	return e.emitSelect(hqLevelRows(valued, hqLevelGrouping{
		groupBy:          h.GroupBy,
		groupByAliases:   h.GroupByAliases,
		attributesColumn: h.AttributesColumn,
		labelName:        q.LabelName,
	}, q.Levels))
}

// hqLevelGrouping is the part of a plural quantile plan that shapes its
// output rows: the per-series group keys and the label that names each level.
type hqLevelGrouping struct {
	groupBy          []chplan.Expr
	groupByAliases   []string
	attributesColumn string
	labelName        string
}

// hqLevelRows expands valued's hqLevelsColumn array into one row per level.
// Each row projects the group keys under their aliases (a key past the end of
// the alias list stays unaliased), stamps labelName=Level.Label into the key
// aliased as the attributes column, and carries the level's quantile as
// `Value`.
func hqLevelRows(valued *QueryBuilder, g hqLevelGrouping, levels []chplan.HistogramQuantileLevel) *QueryBuilder {
	expanded := NewQuery().Select(Star(), As(
		Call("arrayJoin", Call("arrayZip", hqLevelLabels(levels), Col(hqLevelsColumn))),
		hqLevelColumn,
	)).From(Subquery(valued))

	query := NewQuery().From(Subquery(expanded))
	for i, group := range g.groupBy {
		alias := ""
		if i < len(g.groupByAliases) {
			alias = g.groupByAliases[i]
		}
		groupFrag := func(b *Builder) { _ = b.Expr(group) }
		if alias == g.attributesColumn {
			query.Select(As(g.levelKey(groupFrag, alias), alias))
		} else {
			query.SelectAs(groupFrag, alias)
		}
	}
	return query.Select(As(TupleIndex(Col(hqLevelColumn), 2), "Value"))
}

// hqLevelLabels is the array of level labels, positionally aligned with the
// per-level quantile array the plural emitters zip it with.
func hqLevelLabels(levels []chplan.HistogramQuantileLevel) Frag {
	labels := make([]Frag, len(levels))
	for i, level := range levels {
		labels[i] = InlineLit(level.Label)
	}
	return Array(labels...)
}

// levelKey is the output value of the group key published as alias, on a row
// whose expanded level is hqLevelColumn: the key itself, except that the key
// published as the attributes column also carries labelName=Level.Label.
func (g hqLevelGrouping) levelKey(key Frag, alias string) Frag {
	if alias != g.attributesColumn {
		return key
	}
	return Call("mapConcat", key, Call("map", InlineLit(g.labelName), TupleIndex(Col(hqLevelColumn), 1)))
}

func sharedPrometheusQuantilesAggregate(h *chplan.HistogramQuantile, levels []chplan.HistogramQuantileLevel, helpers hqClassicHelperColumns) Frag {
	w := newHQClassicWriters(h, helpers)
	nameParts := []Frag{InlineLit("quantilesPrometheusHistogram(")}
	for i, level := range levels {
		if i > 0 {
			nameParts = append(nameParts, InlineLit(","))
		}
		nameParts = append(nameParts, Call("toString", InlineLit(aggregatePhi(level.Phi))))
	}
	nameParts = append(nameParts, InlineLit(")"))
	aggregate := Call("concat", nameParts...)
	inf := verbatim("inf")
	bounds := Call("arrayPushBack", w.bounds(), inf)
	counts := If(Neq(w.cumCount(), w.boundCount()), w.cum(), Call("arrayPushBack", w.cum(), w.observations()))
	return Call("arrayReduce", aggregate, bounds, counts)
}

// aggregatePhi maps phi into the [0, 1] domain quantilesPrometheusHistogram
// accepts as a parameter; NaN maps to 0. The out-of-domain answer itself is
// decided by sharedPrometheusQuantilesValues, which never reads the aggregate
// for such a level.
func aggregatePhi(phi float64) float64 {
	if math.IsNaN(phi) {
		return 0
	}
	return math.Min(1, math.Max(0, phi))
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
	if q.Histogram == nil || len(q.Levels) == 0 {
		return fmt.Errorf("%w: HistogramQuantilesNative requires a histogram and levels", ErrUnsupported)
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
	// Every level reads the same once-bound walk arrays (hqNativeBindPrepared)
	// inside this single SELECT — including binding the reverse-walk array
	// only when needsReverse, so a plan that never reaches the backward arm
	// pays for no extra array walk. Only the rank position and interpolation
	// are per level, and each level binds its own stop index, running count
	// and value index once (hqNativeBoundValue), exactly as the single-phi
	// emitter does.
	values := hqNativeBindPrepared(&h, needsReverse, func(helpers hqNativeHelperColumns) Frag {
		perLevel := make([]Frag, len(q.Levels))
		for i, level := range q.Levels {
			levelHistogram := h
			levelHistogram.Phi, levelHistogram.PhiExpr = level.Phi, nil
			perLevel[i] = hqNativeBoundValue(&levelHistogram, helpers, hqNativeLevelSuffix(i))
		}
		return Array(perLevel...)
	})
	grouping := hqLevelGrouping{
		groupBy:          h.GroupBy,
		groupByAliases:   h.GroupByAliases,
		attributesColumn: h.AttributesColumn,
		labelName:        q.LabelName,
	}
	// As in the single-phi emitter, when every output key is an input column
	// the fields are bound straight off the ARRAY JOIN materialization
	// boundary's tuple, and the levels expand through this same SELECT's
	// ARRAY JOIN, so one SELECT sits over the folded-histogram subtree instead
	// of the three derived queries hqLevelRows stacks over it.
	row := h.Input.RowType()
	if keys, fields, ok := hqNativeInlineUnpack(&h, row); ok {
		levels := hqNativeLet(fields, func() Frag { return values })
		sb := NewQuery().From(histogramInputBoundary(sub, row)).
			ArrayJoin(As(Call("arrayZip", hqLevelLabels(q.Levels), levels), hqLevelColumn))
		for _, key := range keys {
			sb.Select(As(grouping.levelKey(histogramInputField(key.index), key.alias), key.alias))
		}
		return e.emitSelect(sb.Select(As(TupleIndex(Col(hqLevelColumn), 2), "Value")))
	}
	valued := NewQuery().Select(Star(), As(values, hqLevelsColumn)).From(sub)
	return e.emitSelect(hqLevelRows(valued, grouping, q.Levels))
}

// hqNativeLevelSuffix names level i's per-phi bindings apart from every other
// level's inside the one SELECT [emitHistogramQuantilesNative] renders.
func hqNativeLevelSuffix(i int) string {
	return "_" + strconv.Itoa(i+1)
}
