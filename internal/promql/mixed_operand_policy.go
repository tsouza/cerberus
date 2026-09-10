package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
)

// mixedOperandPolicy governs admission independently of physical row schema.
// A missing entry rejects; a schema that can carry a histogram is not authority
// to expose that histogram to a wrapper that has no registered payload rule.
type mixedOperandPolicy uint8

const (
	mixedReject mixedOperandPolicy = iota
	mixedBespoke
	mixedFloatOnly
	mixedPreserve
)

type (
	mixedWrapperFamily string
	mixedAdmissionSite string
)

const (
	mixedRootAdmission    mixedAdmissionSite = "root"
	mixedOperandAdmission mixedAdmissionSite = "operand"
	mixedPlanAdmission    mixedAdmissionSite = "existing-plan"
)

const (
	mixedLeafFamily             mixedWrapperFamily = "leaf"
	mixedSumAvgFamily           mixedWrapperFamily = "sum-avg"
	mixedCountGroupFamily       mixedWrapperFamily = "count-group"
	mixedFloatAggregateFamily   mixedWrapperFamily = "float-aggregate"
	mixedTopKFamily             mixedWrapperFamily = "topk-bottomk"
	mixedCountValuesFamily      mixedWrapperFamily = "count-values"
	mixedLabelFamily            mixedWrapperFamily = "label-rewrite"
	mixedMathFamily             mixedWrapperFamily = "math-round-clamp"
	mixedScaleFamily            mixedWrapperFamily = "scalar-scale"
	mixedArithmeticFamily       mixedWrapperFamily = "scalar-arithmetic"
	mixedComparisonFamily       mixedWrapperFamily = "scalar-comparison"
	mixedVectorArithmeticFamily mixedWrapperFamily = "vector-arithmetic"
	mixedVectorComparisonFamily mixedWrapperFamily = "vector-comparison"
	mixedSubqueryFamily         mixedWrapperFamily = "subquery-window"
	mixedScalarFamily           mixedWrapperFamily = "scalar"
	mixedSortFamily             mixedWrapperFamily = "sort"
	mixedSortByLabelFamily      mixedWrapperFamily = "sort-by-label"
	mixedDateFamily             mixedWrapperFamily = "date"
	mixedTimestampFamily        mixedWrapperFamily = "timestamp-eval"
	mixedInfoFamily             mixedWrapperFamily = "info"
	mixedHistogramValueFamily   mixedWrapperFamily = "histogram-value"
	mixedLimitFamily            mixedWrapperFamily = "limitk-ratio"
	mixedUnaryFamily            mixedWrapperFamily = "unary"
	mixedSetOperandFamily       mixedWrapperFamily = "vector-set-operand"
	mixedAbsentFamily           mixedWrapperFamily = "absent"
)

type mixedWrapperKey struct {
	family mixedWrapperFamily
	site   mixedAdmissionSite
}

// The site is load-bearing: direct-root math admission does not implicitly
// enable an otherwise rejected nested math expression. Existing recursive
// operand helpers and already-lowered consumption seams are registered
// separately. Bespoke preserves each family's established input topology;
// float-only semantics alone do not choose union/filter versus shadow resolution.
//
// Semantic provenance is family-specific. Prometheus's function implementations
// (promql/functions.go) discard histograms for math/round/clamp, dateWrapper,
// funcScalar and filterFloats-based sort; they preserve samples for label rewrites
// and label sorting. Their existing lowering contracts distinguish discarding
// histogram rows from shadow-resolving the float arm. See the mixed math,
// scalar, sort and date lowerers in histogram_native_mixed_or_{math_fn,scalar,
// sort,datefn}.go for the corresponding input-topology contracts.
//
// Prometheus's evaluator (promql/engine.go) supplies the other operator contracts:
// vectorElemBinop and scalar/vector evaluation distinguish arithmetic, scaling
// and comparisons; UnaryExpr evaluation preserves/scales histograms; aggregation
// and aggregationK distinguish sum/avg, presence, float-only reductions, topk,
// count_values and limit sampling. Set operators compare membership, not sample
// payload. Our mixed scalar/vector, aggregate and set-op lowerers document these
// rules alongside their shadow-resolution and grouping layouts.
//
// Timestamp converts both sample kinds to evaluation-time floats; histogram-value
// functions select histogram samples; evalInfo preserves/enriches both kinds;
// absent counts existence. Subquery-window rules depend on the individual range
// function and its step grid; lowerHistogramOrMixedSubqueryOuterFnInput and its
// call-subquery sibling are the established SELECT/FOLD continuation contracts.
//
// Bespoke entries record existing admission, NOT a proof that every accepted
// composition implements those reference contracts correctly. In particular,
// generic already-lowered aggregate inputs retain their existing behavior here;
// the nested sum/avg mismatch is tracked separately in cerberus issue #3297.
var mixedOperandPolicies = map[mixedWrapperKey]mixedOperandPolicy{
	{mixedLeafFamily, mixedRootAdmission}:              mixedBespoke,
	{mixedSumAvgFamily, mixedRootAdmission}:            mixedBespoke,
	{mixedCountGroupFamily, mixedRootAdmission}:        mixedBespoke,
	{mixedFloatAggregateFamily, mixedRootAdmission}:    mixedBespoke,
	{mixedTopKFamily, mixedRootAdmission}:              mixedBespoke,
	{mixedCountValuesFamily, mixedRootAdmission}:       mixedBespoke,
	{mixedLabelFamily, mixedRootAdmission}:             mixedBespoke,
	{mixedMathFamily, mixedRootAdmission}:              mixedFloatOnly,
	{mixedScaleFamily, mixedRootAdmission}:             mixedBespoke,
	{mixedArithmeticFamily, mixedRootAdmission}:        mixedBespoke,
	{mixedComparisonFamily, mixedRootAdmission}:        mixedBespoke,
	{mixedVectorArithmeticFamily, mixedRootAdmission}:  mixedBespoke,
	{mixedVectorComparisonFamily, mixedRootAdmission}:  mixedBespoke,
	{mixedSubqueryFamily, mixedRootAdmission}:          mixedBespoke,
	{mixedScalarFamily, mixedOperandAdmission}:         mixedBespoke,
	{mixedSortFamily, mixedOperandAdmission}:           mixedBespoke,
	{mixedSortByLabelFamily, mixedOperandAdmission}:    mixedBespoke,
	{mixedDateFamily, mixedOperandAdmission}:           mixedBespoke,
	{mixedTimestampFamily, mixedOperandAdmission}:      mixedBespoke,
	{mixedInfoFamily, mixedOperandAdmission}:           mixedBespoke,
	{mixedHistogramValueFamily, mixedOperandAdmission}: mixedBespoke,
	{mixedLimitFamily, mixedOperandAdmission}:          mixedBespoke,
	{mixedUnaryFamily, mixedOperandAdmission}:          mixedBespoke,
	{mixedSetOperandFamily, mixedOperandAdmission}:     mixedBespoke,
	{mixedSubqueryFamily, mixedOperandAdmission}:       mixedBespoke,
	{mixedAbsentFamily, mixedOperandAdmission}:         mixedBespoke,
	{mixedMathFamily, mixedPlanAdmission}:              mixedFloatOnly,
	{mixedDateFamily, mixedPlanAdmission}:              mixedBespoke,
	{mixedTimestampFamily, mixedPlanAdmission}:         mixedBespoke,
	{mixedUnaryFamily, mixedPlanAdmission}:             mixedBespoke,
	{mixedArithmeticFamily, mixedPlanAdmission}:        mixedBespoke,
	{mixedComparisonFamily, mixedPlanAdmission}:        mixedBespoke,
	{mixedScaleFamily, mixedPlanAdmission}:             mixedBespoke,
	{mixedLabelFamily, mixedPlanAdmission}:             mixedBespoke,
	{mixedScalarFamily, mixedPlanAdmission}:            mixedBespoke,
	{mixedSubqueryFamily, mixedPlanAdmission}:          mixedBespoke,
	{mixedSortFamily, mixedPlanAdmission}:              mixedBespoke,
	{mixedSortByLabelFamily, mixedPlanAdmission}:       mixedBespoke,
	{mixedInfoFamily, mixedPlanAdmission}:              mixedBespoke,
	{mixedLimitFamily, mixedPlanAdmission}:             mixedBespoke,
	{mixedSetOperandFamily, mixedPlanAdmission}:        mixedBespoke,
	{mixedAbsentFamily, mixedPlanAdmission}:            mixedBespoke,
	{mixedHistogramValueFamily, mixedPlanAdmission}:    mixedBespoke,
	{mixedVectorArithmeticFamily, mixedPlanAdmission}:  mixedBespoke,
	{mixedVectorComparisonFamily, mixedPlanAdmission}:  mixedBespoke,
	{mixedSumAvgFamily, mixedPlanAdmission}:            mixedBespoke,
	{mixedCountGroupFamily, mixedPlanAdmission}:        mixedBespoke,
	{mixedFloatAggregateFamily, mixedPlanAdmission}:    mixedBespoke,
	{mixedTopKFamily, mixedPlanAdmission}:              mixedBespoke,
	{mixedCountValuesFamily, mixedPlanAdmission}:       mixedBespoke,
}

func lowerWithMixedOperandPolicy(family mixedWrapperFamily, site mixedAdmissionSite, build func() (chplan.Node, error)) (chplan.Node, error) {
	key := mixedWrapperKey{family: family, site: site}
	if err := requireMixedBespokePolicy(key, mixedOperandPolicies[key]); err != nil {
		return nil, err
	}
	return build()
}

func requireMixedBespokePolicy(key mixedWrapperKey, policy mixedOperandPolicy) error {
	if policy != mixedBespoke {
		return fmt.Errorf("promql: mixed operand is not admitted for %s at %s", key.family, key.site)
	}
	return nil
}

// requireMixedPlanPolicy authorizes a wrapper's consumption of an already
// lowered mixed operand. Physical role resolution alone cannot authorize it.
// Ordinary float and histogram-only operands retain their existing path.
func requireMixedPlanPolicy(inner chplan.Node, family mixedWrapperFamily) error {
	if chplan.RowShapeOf(inner) != chplan.MixedRowShape {
		return nil
	}
	key := mixedWrapperKey{family: family, site: mixedPlanAdmission}
	return requireMixedBespokePolicy(key, mixedOperandPolicies[key])
}

func mixedVectorBinaryFamily(op chplan.BinaryOp) mixedWrapperFamily {
	if isComparison(op) {
		return mixedVectorComparisonFamily
	}
	switch op {
	case chplan.OpAdd, chplan.OpSub, chplan.OpMul, chplan.OpDiv, chplan.OpMod, chplan.OpPow, chplan.OpAtan2:
		return mixedVectorArithmeticFamily
	default:
		return ""
	}
}

func mixedScalarBinaryFamily(op chplan.BinaryOp, scalarOnLeft bool) mixedWrapperFamily {
	if isComparison(op) {
		return mixedComparisonFamily
	}
	if op == chplan.OpMul || (op == chplan.OpDiv && !scalarOnLeft) {
		return mixedScaleFamily
	}
	switch op {
	case chplan.OpAdd, chplan.OpSub, chplan.OpDiv, chplan.OpMod, chplan.OpPow, chplan.OpAtan2:
		return mixedArithmeticFamily
	default:
		return ""
	}
}

func mixedAggregateFamily(op parser.ItemType) mixedWrapperFamily {
	switch op {
	case parser.SUM, parser.AVG:
		return mixedSumAvgFamily
	case parser.COUNT, parser.GROUP:
		return mixedCountGroupFamily
	case parser.MIN, parser.MAX, parser.STDDEV, parser.STDVAR, parser.QUANTILE:
		return mixedFloatAggregateFamily
	case parser.TOPK, parser.BOTTOMK:
		return mixedTopKFamily
	case parser.LIMITK, parser.LIMIT_RATIO:
		return mixedLimitFamily
	case parser.COUNT_VALUES:
		return mixedCountValuesFamily
	default:
		return ""
	}
}
