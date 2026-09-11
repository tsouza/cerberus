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
	mixedPolicyClosed
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
// scalar, sort and date lowerers in scalar_args.go and
// histogram_native_mixed_or_{math_fn,sort,datefn}.go for their input topologies.
//
// Prometheus's evaluator (promql/engine.go) supplies the other operator contracts:
// vectorElemBinop and scalar/vector evaluation distinguish arithmetic, scaling
// and comparisons; UnaryExpr evaluation preserves/scales histograms; aggregation
// and aggregationK distinguish sum/avg, presence, float-only reductions, topk,
// count_values and limit sampling. Count/group are payload-neutral: their shared
// presence kernel preserves the mixed relation and counts or marks every row.
// Set operators compare membership, not sample payload. Our mixed scalar/vector,
// aggregate and set-op lowerers document these rules alongside their
// shadow-resolution and grouping layouts.
//
// Timestamp converts both sample kinds to evaluation-time floats; histogram-value
// functions select histogram samples; evalInfo preserves/enriches both kinds;
// absent counts existence. Subquery-window rules depend on the individual range
// function and its step grid; lowerHistogramOrMixedSubqueryOuterFnInput and its
// call-subquery sibling are the established SELECT/FOLD continuation contracts.
//
// Bespoke entries select a family-specific payload transformation. Sum/avg, for
// example, partitions and recombines a mixed plan; count/group instead preserve
// that plan for their payload-neutral reduction. Float aggregates remain bespoke
// at the root, where their mixed union needs shadow resolution, while their
// existing-plan entries execute the shared float-only transform after that
// resolution. Scalar arithmetic follows the same policy-driven narrowing while
// preserving each projection boundary.
var mixedOperandPolicies = map[mixedWrapperKey]mixedOperandPolicy{
	{mixedLeafFamily, mixedRootAdmission}:              mixedBespoke,
	{mixedSumAvgFamily, mixedRootAdmission}:            mixedBespoke,
	{mixedCountGroupFamily, mixedRootAdmission}:        mixedPreserve,
	{mixedFloatAggregateFamily, mixedRootAdmission}:    mixedBespoke,
	{mixedTopKFamily, mixedRootAdmission}:              mixedFloatOnly,
	{mixedCountValuesFamily, mixedRootAdmission}:       mixedBespoke,
	{mixedLabelFamily, mixedRootAdmission}:             mixedPreserve,
	{mixedMathFamily, mixedRootAdmission}:              mixedFloatOnly,
	{mixedScaleFamily, mixedRootAdmission}:             mixedBespoke,
	{mixedArithmeticFamily, mixedRootAdmission}:        mixedFloatOnly,
	{mixedComparisonFamily, mixedRootAdmission}:        mixedFloatOnly,
	{mixedVectorArithmeticFamily, mixedRootAdmission}:  mixedBespoke,
	{mixedVectorComparisonFamily, mixedRootAdmission}:  mixedBespoke,
	{mixedSubqueryFamily, mixedRootAdmission}:          mixedBespoke,
	{mixedScalarFamily, mixedOperandAdmission}:         mixedFloatOnly,
	{mixedSortFamily, mixedOperandAdmission}:           mixedFloatOnly,
	{mixedSortByLabelFamily, mixedOperandAdmission}:    mixedPreserve,
	{mixedDateFamily, mixedOperandAdmission}:           mixedFloatOnly,
	{mixedTimestampFamily, mixedOperandAdmission}:      mixedBespoke,
	{mixedInfoFamily, mixedOperandAdmission}:           mixedBespoke,
	{mixedHistogramValueFamily, mixedOperandAdmission}: mixedBespoke,
	{mixedLimitFamily, mixedOperandAdmission}:          mixedPreserve,
	{mixedUnaryFamily, mixedOperandAdmission}:          mixedBespoke,
	{mixedSetOperandFamily, mixedOperandAdmission}:     mixedBespoke,
	{mixedSubqueryFamily, mixedOperandAdmission}:       mixedBespoke,
	{mixedAbsentFamily, mixedOperandAdmission}:         mixedBespoke,
	{mixedMathFamily, mixedPlanAdmission}:              mixedFloatOnly,
	{mixedDateFamily, mixedPlanAdmission}:              mixedFloatOnly,
	{mixedTimestampFamily, mixedPlanAdmission}:         mixedBespoke,
	{mixedUnaryFamily, mixedPlanAdmission}:             mixedBespoke,
	{mixedArithmeticFamily, mixedPlanAdmission}:        mixedFloatOnly,
	{mixedComparisonFamily, mixedPlanAdmission}:        mixedFloatOnly,
	{mixedScaleFamily, mixedPlanAdmission}:             mixedBespoke,
	{mixedLabelFamily, mixedPlanAdmission}:             mixedPreserve,
	{mixedScalarFamily, mixedPlanAdmission}:            mixedFloatOnly,
	{mixedSubqueryFamily, mixedPlanAdmission}:          mixedBespoke,
	{mixedSortFamily, mixedPlanAdmission}:              mixedFloatOnly,
	{mixedSortByLabelFamily, mixedPlanAdmission}:       mixedPreserve,
	{mixedInfoFamily, mixedPlanAdmission}:              mixedBespoke,
	{mixedLimitFamily, mixedPlanAdmission}:             mixedPreserve,
	{mixedSetOperandFamily, mixedPlanAdmission}:        mixedBespoke,
	{mixedAbsentFamily, mixedPlanAdmission}:            mixedBespoke,
	{mixedHistogramValueFamily, mixedPlanAdmission}:    mixedBespoke,
	{mixedVectorArithmeticFamily, mixedPlanAdmission}:  mixedBespoke,
	{mixedVectorComparisonFamily, mixedPlanAdmission}:  mixedBespoke,
	{mixedSumAvgFamily, mixedPlanAdmission}:            mixedBespoke,
	{mixedCountGroupFamily, mixedPlanAdmission}:        mixedPreserve,
	{mixedFloatAggregateFamily, mixedPlanAdmission}:    mixedFloatOnly,
	{mixedTopKFamily, mixedPlanAdmission}:              mixedFloatOnly,
	{mixedCountValuesFamily, mixedPlanAdmission}:       mixedBespoke,
}

type mixedPlanTransform func(chplan.Node) chplan.Node

func lowerWithMixedOperandPolicy(family mixedWrapperFamily, site mixedAdmissionSite, expected mixedOperandPolicy) (mixedPlanTransform, error) {
	key := mixedWrapperKey{family: family, site: site}
	policy, ok := mixedOperandPolicies[key]
	if !ok {
		return nil, requireMixedBespokePolicy(key, mixedReject)
	}
	if expected == mixedPolicyClosed {
		return nil, requireMixedBespokePolicy(key, mixedReject)
	}
	if policy != expected {
		return nil, requireMixedBespokePolicy(key, mixedReject)
	}
	if expected == mixedBespoke {
		if err := requireMixedBespokePolicy(key, policy); err != nil {
			return nil, err
		}
	}
	return mixedPlanTransformForPolicy(expected), nil
}

func lowerWithBespokeMixedOperandPolicy(family mixedWrapperFamily, site mixedAdmissionSite, build func() (chplan.Node, error)) (chplan.Node, error) {
	transform, err := lowerWithMixedOperandPolicy(family, site, mixedBespoke)
	if err != nil {
		return nil, err
	}
	inner, err := build()
	if err != nil {
		return inner, err
	}
	return transform(inner), nil
}

func preserveMixedPlanTransform(inner chplan.Node) chplan.Node { return inner }

func mixedPlanTransformForPolicy(policy mixedOperandPolicy) mixedPlanTransform {
	if policy == mixedFloatOnly {
		return mixedRowsFloatOnly
	}
	return preserveMixedPlanTransform
}

// prepareMixedAggregatePlan applies the registered existing-plan policy for
// the aggregate families which reach lowerAggregate's generic input path.
// Keeping this switch closed prevents a new aggregate family from inheriting a
// plausible payload transform merely because somebody added a table entry.
func prepareMixedAggregatePlan(inner chplan.Node, family mixedWrapperFamily) (chplan.Node, error) {
	if !mixedRowsNeedPreparation(inner) {
		return inner, nil
	}

	expected := mixedPolicyClosed
	switch family {
	case mixedSumAvgFamily:
		expected = mixedBespoke
	case mixedCountGroupFamily:
		expected = mixedPreserve
	case mixedFloatAggregateFamily:
		expected = mixedFloatOnly
	}
	if err := requireMixedPlanPolicy(inner, family, expected); err != nil {
		return nil, err
	}
	return mixedPlanTransformForPolicy(expected)(inner), nil
}

func requireMixedBespokePolicy(key mixedWrapperKey, policy mixedOperandPolicy) error {
	if policy != mixedBespoke {
		return fmt.Errorf("promql: mixed operand is not admitted for %s at %s", key.family, key.site)
	}
	return nil
}

// lowerWithMixedPreservePolicy authorizes a payload-neutral consumer before
// loading its operand. Identity preserves payload roles and union shadowing.
func lowerWithMixedPreservePolicy(family mixedWrapperFamily, site mixedAdmissionSite, load func() (chplan.Node, error)) (chplan.Node, error) {
	key := mixedWrapperKey{family: family, site: site}
	if mixedOperandPolicies[key] != mixedPreserve {
		return nil, fmt.Errorf("promql: mixed operand is not admitted for %s at %s", key.family, key.site)
	}
	return load()
}

func preserveMixedPlan(inner chplan.Node, family mixedWrapperFamily) (chplan.Node, error) {
	if !mixedRowsNeedPreparation(inner) {
		return inner, nil
	}
	return lowerWithMixedPreservePolicy(family, mixedPlanAdmission, func() (chplan.Node, error) { return inner, nil })
}

// requireMixedPlanPolicy authorizes a wrapper's consumption of an already
// lowered mixed operand. Physical role resolution alone cannot authorize it.
// Ordinary float and histogram-only operands retain their existing path.
func requireMixedPlanPolicy(inner chplan.Node, family mixedWrapperFamily, expectedPolicy ...mixedOperandPolicy) error {
	expected := mixedBespoke
	if len(expectedPolicy) == 0 {
		if !mixedRowsNeedPreparation(inner) {
			return nil
		}
	} else if len(expectedPolicy) == 1 {
		expected = expectedPolicy[0]
	} else {
		expected = mixedPolicyClosed
	}
	key := mixedWrapperKey{family: family, site: mixedPlanAdmission}
	policy, ok := mixedOperandPolicies[key]
	if !ok {
		return requireMixedBespokePolicy(key, mixedReject)
	}
	if expected == mixedPolicyClosed {
		return requireMixedBespokePolicy(key, mixedReject)
	}
	if policy != expected {
		return requireMixedBespokePolicy(key, mixedReject)
	}
	if expected == mixedBespoke {
		return requireMixedBespokePolicy(key, policy)
	}
	return nil
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
