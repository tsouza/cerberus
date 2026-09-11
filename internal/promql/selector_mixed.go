package promql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// executeMixedSelectorPolicy is the sole mixed-payload policy boundary for
// topk/bottomk and limitk/limit_ratio. The closed key-to-mode switch is
// intentional: adding a plausible table row cannot silently authorize a new
// selector site or make one family execute the other family's payload rule.
//
// Authority is checked before load runs. An admitted loader runs exactly once;
// its error is returned unchanged. Ranked selectors narrow the completed plan
// through mixedRowsFloatOnly, after its own union shadowing has resolved, while
// limit selectors preserve the loaded plan byte-for-byte.
func executeMixedSelectorPolicy(family mixedWrapperFamily, site mixedAdmissionSite, load func() (chplan.Node, error)) (chplan.Node, error) {
	key := mixedWrapperKey{family: family, site: site}
	var expected mixedOperandPolicy
	switch key {
	case mixedWrapperKey{family: mixedTopKFamily, site: mixedRootAdmission},
		mixedWrapperKey{family: mixedTopKFamily, site: mixedPlanAdmission}:
		expected = mixedFloatOnly
	case mixedWrapperKey{family: mixedLimitFamily, site: mixedOperandAdmission},
		mixedWrapperKey{family: mixedLimitFamily, site: mixedPlanAdmission}:
		expected = mixedPreserve
	default:
		return nil, fmt.Errorf("promql: mixed operand is not admitted for %s at %s", key.family, key.site)
	}
	if policy, ok := mixedOperandPolicies[key]; !ok || policy != expected {
		return nil, fmt.Errorf("promql: mixed operand is not admitted for %s at %s", key.family, key.site)
	}
	inner, err := load()
	if err != nil {
		return nil, err
	}
	if expected == mixedFloatOnly {
		return mixedRowsFloatOnly(inner), nil
	}
	return inner, nil
}
