package gen

type ShapeID string

const (
	promQLSelectorShape ShapeID = "promql.instant.selector"
	promQLSumShape      ShapeID = "promql.instant.sum"
	promQLSumByShape    ShapeID = "promql.instant.sum-by"
	promQLRateShape     ShapeID = "promql.instant.rate"
	promQLSumRateShape  ShapeID = "promql.instant.sum-rate"

	promQLRangeSelectorShape ShapeID = "promql.range.selector"
	promQLRangeSumByShape    ShapeID = "promql.range.sum-by"
	promQLRangeRateShape     ShapeID = "promql.range.rate"

	expHistogramCountFunctionShape         ShapeID = "promql.native-histogram.function.count"
	expHistogramSumFunctionShape           ShapeID = "promql.native-histogram.function.sum"
	expHistogramAverageFunctionShape       ShapeID = "promql.native-histogram.function.avg"
	expHistogramStddevFunctionShape        ShapeID = "promql.native-histogram.function.stddev"
	expHistogramStdvarFunctionShape        ShapeID = "promql.native-histogram.function.stdvar"
	expHistogramFractionShape              ShapeID = "promql.native-histogram.fraction"
	expHistogramSelectorShape              ShapeID = "promql.native-histogram.selector"
	expHistogramSumShape                   ShapeID = "promql.native-histogram.sum"
	expHistogramSumByShape                 ShapeID = "promql.native-histogram.sum-by"
	expHistogramRateShape                  ShapeID = "promql.native-histogram.rate"
	expHistogramIncreaseShape              ShapeID = "promql.native-histogram.increase"
	expHistogramQuantileSelectorShape      ShapeID = "promql.native-histogram.quantile-selector"
	expHistogramQuantileSumShape           ShapeID = "promql.native-histogram.quantile-sum"
	expHistogramQuantileSumByShape         ShapeID = "promql.native-histogram.quantile-sum-by"
	expHistogramQuantileRateShape          ShapeID = "promql.native-histogram.quantile-rate"
	expHistogramQuantileIncreaseShape      ShapeID = "promql.native-histogram.quantile-increase"
	expHistogramQuantileSumRateShape       ShapeID = "promql.native-histogram.quantile-sum-rate"
	expHistogramQuantileSumByRateShape     ShapeID = "promql.native-histogram.quantile-sum-by-rate"
	expHistogramQuantileSumIncreaseShape   ShapeID = "promql.native-histogram.quantile-sum-increase"
	expHistogramQuantileSumByIncreaseShape ShapeID = "promql.native-histogram.quantile-sum-by-increase"

	logQLSelectorShape              ShapeID = "logql.stream.selector"
	logQLLineContainsShape          ShapeID = "logql.stream.line-contains"
	logQLLineExcludesShape          ShapeID = "logql.stream.line-excludes"
	logQLLabelFormatShape           ShapeID = "logql.stream.label-format"
	logQLIPContainsShape            ShapeID = "logql.stream.ip-contains"
	logQLIPExcludesShape            ShapeID = "logql.stream.ip-excludes"
	logQLPatternContainsShape       ShapeID = "logql.stream.pattern-contains"
	logQLPatternExcludesShape       ShapeID = "logql.stream.pattern-excludes"
	logQLPatternContainsPrefixShape ShapeID = "logql.stream.pattern-contains-prefix"
	logQLPatternContainsSuffixShape ShapeID = "logql.stream.pattern-contains-suffix"
	logQLPatternExcludesPrefixShape ShapeID = "logql.stream.pattern-excludes-prefix"
	logQLPatternExcludesSuffixShape ShapeID = "logql.stream.pattern-excludes-suffix"

	traceQLServiceShape              ShapeID = "traceql.selector.service"
	traceQLCountShape                ShapeID = "traceql.pipeline.count"
	traceQLResourceAttributeShape    ShapeID = "traceql.selector.resource-attribute"
	traceQLSpanAttributeShape        ShapeID = "traceql.selector.span-attribute"
	traceQLDurationShape             ShapeID = "traceql.intrinsic.duration"
	traceQLStatusShape               ShapeID = "traceql.intrinsic.status"
	traceQLNameShape                 ShapeID = "traceql.intrinsic.name"
	traceQLRegexShape                ShapeID = "traceql.selector.regex"
	traceQLNegatedAttributeShape     ShapeID = "traceql.selector.negated-attribute"
	traceQLConjunctionShape          ShapeID = "traceql.selector.conjunction"
	traceQLStructuralChildShape      ShapeID = "traceql.structural.child"
	traceQLStructuralDescendantShape ShapeID = "traceql.structural.descendant"
	traceQLDurationAggregateShape    ShapeID = "traceql.pipeline.duration-aggregate"
	traceQLDurationAggregateMinShape ShapeID = "traceql.pipeline.duration-aggregate-min"
	traceQLDurationAggregateMaxShape ShapeID = "traceql.pipeline.duration-aggregate-max"
	traceQLDurationAggregateSumShape ShapeID = "traceql.pipeline.duration-aggregate-sum"
	traceQLSelectShape               ShapeID = "traceql.pipeline.select"

	instantWindowWaveSumShape                       ShapeID = "promql.instant-window.wave.sum-over-time"
	instantWindowWaveCountShape                     ShapeID = "promql.instant-window.wave.count-over-time"
	instantWindowWaveAverageShape                   ShapeID = "promql.instant-window.wave.avg-over-time"
	instantWindowWaveMaximumShape                   ShapeID = "promql.instant-window.wave.max-over-time"
	instantWindowWaveMinimumShape                   ShapeID = "promql.instant-window.wave.min-over-time"
	instantWindowWaveRateShape                      ShapeID = "promql.instant-window.wave.rate"
	instantWindowWaveIncreaseShape                  ShapeID = "promql.instant-window.wave.increase"
	instantWindowWaveDeltaShape                     ShapeID = "promql.instant-window.wave.delta"
	instantWindowPositiveIncrementsSumShape         ShapeID = "promql.instant-window.positive-increments.sum-over-time"
	instantWindowPositiveIncrementsCountShape       ShapeID = "promql.instant-window.positive-increments.count-over-time"
	instantWindowPositiveIncrementsAverageShape     ShapeID = "promql.instant-window.positive-increments.avg-over-time"
	instantWindowPositiveIncrementsMaximumShape     ShapeID = "promql.instant-window.positive-increments.max-over-time"
	instantWindowPositiveIncrementsMinimumShape     ShapeID = "promql.instant-window.positive-increments.min-over-time"
	instantWindowPositiveIncrementsRateShape        ShapeID = "promql.instant-window.positive-increments.rate"
	instantWindowPositiveIncrementsIncreaseShape    ShapeID = "promql.instant-window.positive-increments.increase"
	instantWindowPositiveIncrementsDeltaShape       ShapeID = "promql.instant-window.positive-increments.delta"
	instantWindowMonotonicRunningTotalSumShape      ShapeID = "promql.instant-window.monotonic-running-total.sum-over-time"
	instantWindowMonotonicRunningTotalCountShape    ShapeID = "promql.instant-window.monotonic-running-total.count-over-time"
	instantWindowMonotonicRunningTotalAverageShape  ShapeID = "promql.instant-window.monotonic-running-total.avg-over-time"
	instantWindowMonotonicRunningTotalMaximumShape  ShapeID = "promql.instant-window.monotonic-running-total.max-over-time"
	instantWindowMonotonicRunningTotalMinimumShape  ShapeID = "promql.instant-window.monotonic-running-total.min-over-time"
	instantWindowMonotonicRunningTotalRateShape     ShapeID = "promql.instant-window.monotonic-running-total.rate"
	instantWindowMonotonicRunningTotalIncreaseShape ShapeID = "promql.instant-window.monotonic-running-total.increase"
	instantWindowMonotonicRunningTotalDeltaShape    ShapeID = "promql.instant-window.monotonic-running-total.delta"
)

var promQLShapeRoster = [...]ShapeID{
	promQLSelectorShape,
	promQLSumShape,
	promQLSumByShape,
	promQLRateShape,
	promQLSumRateShape,
}

var promQLRangeShapeRoster = [...]ShapeID{
	promQLRangeSelectorShape,
	promQLRangeSumByShape,
	promQLRangeRateShape,
}

var expHistogramShapeRoster = [...]ShapeID{
	expHistogramCountFunctionShape,
	expHistogramSumFunctionShape,
	expHistogramAverageFunctionShape,
	expHistogramStddevFunctionShape,
	expHistogramStdvarFunctionShape,
	expHistogramFractionShape,
	expHistogramSelectorShape,
	expHistogramSumShape,
	expHistogramSumByShape,
	expHistogramRateShape,
	expHistogramIncreaseShape,
	expHistogramQuantileSelectorShape,
	expHistogramQuantileSumShape,
	expHistogramQuantileSumByShape,
	expHistogramQuantileRateShape,
	expHistogramQuantileIncreaseShape,
	expHistogramQuantileSumRateShape,
	expHistogramQuantileSumByRateShape,
	expHistogramQuantileSumIncreaseShape,
	expHistogramQuantileSumByIncreaseShape,
}

// expHistogramRandomShapeFamilies preserves the generator's original
// high-level weighting: the five scalar value functions collectively occupy
// one family, while deterministic enrollment still executes each function as
// its own semantic shape.
var expHistogramRandomShapeFamilies = [][]ShapeID{
	{
		expHistogramCountFunctionShape,
		expHistogramSumFunctionShape,
		expHistogramAverageFunctionShape,
		expHistogramStddevFunctionShape,
		expHistogramStdvarFunctionShape,
	},
	{expHistogramFractionShape},
	{expHistogramSelectorShape},
	{expHistogramSumShape},
	{expHistogramSumByShape},
	{expHistogramRateShape},
	{expHistogramIncreaseShape},
	{expHistogramQuantileSelectorShape},
	{expHistogramQuantileSumShape},
	{expHistogramQuantileSumByShape},
	{expHistogramQuantileRateShape},
	{expHistogramQuantileIncreaseShape},
	{expHistogramQuantileSumRateShape},
	{expHistogramQuantileSumByRateShape},
	{expHistogramQuantileSumIncreaseShape},
	{expHistogramQuantileSumByIncreaseShape},
}

var logQLShapeRoster = [...]ShapeID{
	logQLSelectorShape,
	logQLLineContainsShape,
	logQLLineExcludesShape,
	logQLLabelFormatShape,
	logQLIPContainsShape,
	logQLIPExcludesShape,
	logQLPatternContainsShape,
	logQLPatternContainsPrefixShape,
	logQLPatternContainsSuffixShape,
	logQLPatternExcludesShape,
	logQLPatternExcludesPrefixShape,
	logQLPatternExcludesSuffixShape,
}

var logQLRandomShapeFamilies = [][]ShapeID{
	{logQLSelectorShape},
	{logQLLineContainsShape},
	{logQLLineExcludesShape},
	{logQLLabelFormatShape},
	{logQLIPContainsShape},
	{logQLIPExcludesShape},
	{logQLPatternContainsShape, logQLPatternContainsPrefixShape, logQLPatternContainsSuffixShape},
	{logQLPatternExcludesShape, logQLPatternExcludesPrefixShape, logQLPatternExcludesSuffixShape},
}

var traceQLShapeRoster = [...]ShapeID{
	traceQLServiceShape,
	traceQLCountShape,
	traceQLResourceAttributeShape,
	traceQLSpanAttributeShape,
	traceQLDurationShape,
	traceQLStatusShape,
	traceQLNameShape,
	traceQLRegexShape,
	traceQLNegatedAttributeShape,
	traceQLConjunctionShape,
	traceQLStructuralChildShape,
	traceQLStructuralDescendantShape,
	traceQLDurationAggregateShape,
	traceQLDurationAggregateMinShape,
	traceQLDurationAggregateMaxShape,
	traceQLDurationAggregateSumShape,
	traceQLSelectShape,
}

var traceQLRandomShapeFamilies = [][]ShapeID{
	{traceQLServiceShape},
	{traceQLCountShape},
	{traceQLResourceAttributeShape},
	{traceQLSpanAttributeShape},
	{traceQLDurationShape},
	{traceQLStatusShape},
	{traceQLNameShape},
	{traceQLRegexShape},
	{traceQLNegatedAttributeShape},
	{traceQLConjunctionShape},
	{traceQLStructuralChildShape},
	{traceQLStructuralDescendantShape},
	{
		traceQLDurationAggregateShape,
		traceQLDurationAggregateMinShape,
		traceQLDurationAggregateMaxShape,
		traceQLDurationAggregateSumShape,
	},
	{traceQLSelectShape},
}

type instantWindowShapeSpec struct {
	id      ShapeID
	fn      string
	profile InstantWindowValueProfile
}

var instantWindowShapeRoster = [...]instantWindowShapeSpec{
	{id: instantWindowWaveSumShape, fn: "sum_over_time", profile: InstantWindowValueProfileWave},
	{id: instantWindowWaveCountShape, fn: "count_over_time", profile: InstantWindowValueProfileWave},
	{id: instantWindowWaveAverageShape, fn: "avg_over_time", profile: InstantWindowValueProfileWave},
	{id: instantWindowWaveMaximumShape, fn: "max_over_time", profile: InstantWindowValueProfileWave},
	{id: instantWindowWaveMinimumShape, fn: "min_over_time", profile: InstantWindowValueProfileWave},
	{id: instantWindowWaveRateShape, fn: "rate", profile: InstantWindowValueProfileWave},
	{id: instantWindowWaveIncreaseShape, fn: "increase", profile: InstantWindowValueProfileWave},
	{id: instantWindowWaveDeltaShape, fn: "delta", profile: InstantWindowValueProfileWave},
	{id: instantWindowPositiveIncrementsSumShape, fn: "sum_over_time", profile: InstantWindowValueProfilePositiveIncrements},
	{id: instantWindowPositiveIncrementsCountShape, fn: "count_over_time", profile: InstantWindowValueProfilePositiveIncrements},
	{id: instantWindowPositiveIncrementsAverageShape, fn: "avg_over_time", profile: InstantWindowValueProfilePositiveIncrements},
	{id: instantWindowPositiveIncrementsMaximumShape, fn: "max_over_time", profile: InstantWindowValueProfilePositiveIncrements},
	{id: instantWindowPositiveIncrementsMinimumShape, fn: "min_over_time", profile: InstantWindowValueProfilePositiveIncrements},
	{id: instantWindowPositiveIncrementsRateShape, fn: "rate", profile: InstantWindowValueProfilePositiveIncrements},
	{id: instantWindowPositiveIncrementsIncreaseShape, fn: "increase", profile: InstantWindowValueProfilePositiveIncrements},
	{id: instantWindowPositiveIncrementsDeltaShape, fn: "delta", profile: InstantWindowValueProfilePositiveIncrements},
	{id: instantWindowMonotonicRunningTotalSumShape, fn: "sum_over_time", profile: InstantWindowValueProfileMonotonicRunningTotal},
	{id: instantWindowMonotonicRunningTotalCountShape, fn: "count_over_time", profile: InstantWindowValueProfileMonotonicRunningTotal},
	{id: instantWindowMonotonicRunningTotalAverageShape, fn: "avg_over_time", profile: InstantWindowValueProfileMonotonicRunningTotal},
	{id: instantWindowMonotonicRunningTotalMaximumShape, fn: "max_over_time", profile: InstantWindowValueProfileMonotonicRunningTotal},
	{id: instantWindowMonotonicRunningTotalMinimumShape, fn: "min_over_time", profile: InstantWindowValueProfileMonotonicRunningTotal},
	{id: instantWindowMonotonicRunningTotalRateShape, fn: "rate", profile: InstantWindowValueProfileMonotonicRunningTotal},
	{id: instantWindowMonotonicRunningTotalIncreaseShape, fn: "increase", profile: InstantWindowValueProfileMonotonicRunningTotal},
	{id: instantWindowMonotonicRunningTotalDeltaShape, fn: "delta", profile: InstantWindowValueProfileMonotonicRunningTotal},
}

// PromQLShapeIDs returns the exact stable roster for the instant PromQL
// differential generator. The returned slice is a copy so callers cannot
// mutate the generator's source of truth.
func PromQLShapeIDs() []ShapeID { return copyShapeIDs(promQLShapeRoster[:]) }

// PromQLRangeShapeIDs returns the exact stable roster for the query-range
// PromQL differential generator.
func PromQLRangeShapeIDs() []ShapeID { return copyShapeIDs(promQLRangeShapeRoster[:]) }

// ExpHistogramShapeIDs returns the exact stable roster for the native-
// histogram PromQL differential generator.
func ExpHistogramShapeIDs() []ShapeID { return copyShapeIDs(expHistogramShapeRoster[:]) }

// LogQLShapeIDs returns the exact stable roster for the LogQL differential
// generator.
func LogQLShapeIDs() []ShapeID { return copyShapeIDs(logQLShapeRoster[:]) }

// TraceQLShapeIDs returns the exact stable roster for the TraceQL differential
// generator.
func TraceQLShapeIDs() []ShapeID { return copyShapeIDs(traceQLShapeRoster[:]) }

// InstantWindowShapeIDs returns the exact cross-product roster of range
// functions and gauge-table value profiles exercised by the instant-window
// property.
func InstantWindowShapeIDs() []ShapeID {
	ids := make([]ShapeID, 0, len(instantWindowShapeRoster))
	for _, spec := range instantWindowShapeRoster {
		ids = append(ids, spec.id)
	}
	return ids
}

func instantWindowShapeByID(id ShapeID) (instantWindowShapeSpec, bool) {
	for _, spec := range instantWindowShapeRoster {
		if spec.id == id {
			return spec, true
		}
	}
	return instantWindowShapeSpec{}, false
}

func copyShapeIDs(ids []ShapeID) []ShapeID {
	return append([]ShapeID(nil), ids...)
}

func containsShapeID(ids []ShapeID, id ShapeID) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}
