package promql

// executeMixedSelectorPolicy is the sole mixed-payload policy boundary for
// topk/bottomk and limitk/limit_ratio. The closed key-to-mode switch is
// intentional: adding a plausible table row cannot silently authorize a new
// selector site or make one family execute the other family's payload rule.
//
// Authority is checked before the caller validates or loads its operand. The
// returned transform runs only after loading succeeds: ranked selectors narrow
// the completed plan through mixedRowsFloatOnly, after union shadowing has
// resolved, while limit selectors preserve the loaded plan byte-for-byte.
func executeMixedSelectorPolicy(family mixedWrapperFamily, site mixedAdmissionSite) (mixedPlanTransform, error) {
	key := mixedWrapperKey{family: family, site: site}
	expected := mixedPolicyClosed
	switch key {
	case mixedWrapperKey{family: mixedTopKFamily, site: mixedRootAdmission},
		mixedWrapperKey{family: mixedTopKFamily, site: mixedPlanAdmission}:
		expected = mixedFloatOnly
	case mixedWrapperKey{family: mixedLimitFamily, site: mixedOperandAdmission},
		mixedWrapperKey{family: mixedLimitFamily, site: mixedPlanAdmission}:
		expected = mixedPreserve
	}
	if site == mixedPlanAdmission {
		if err := requireMixedPlanPolicy(nil, family, expected); err != nil {
			return nil, err
		}
		return mixedPlanTransformForPolicy(expected), nil
	}
	return lowerWithMixedOperandPolicy(family, site, expected)
}

func mixedSelectorPolicyFirstError(policyErr, validationErr error) error {
	if policyErr != nil {
		return policyErr
	}
	return validationErr
}
