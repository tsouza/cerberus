package promql

import (
	"errors"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

func TestMixedSelectorPolicyExecutorModes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		family    mixedWrapperFamily
		site      mixedAdmissionSite
		floatOnly bool
	}{
		{"ranked root", mixedTopKFamily, mixedRootAdmission, true},
		{"ranked plan", mixedTopKFamily, mixedPlanAdmission, true},
		{"limit operand", mixedLimitFamily, mixedOperandAdmission, false},
		{"limit plan", mixedLimitFamily, mixedPlanAdmission, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := &chplan.VectorSetOp{Mixed: true}
			calls := 0
			got, err := executeMixedSelectorPolicy(tc.family, tc.site, func() (chplan.Node, error) {
				calls++
				return input, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("loader calls = %d, want 1", calls)
			}
			if tc.floatOnly {
				if !chplan.IsMixedFloatNarrowing(got) {
					t.Fatalf("ranked result = %#v, want mixed float narrowing", got)
				}
			} else if got != input {
				t.Fatalf("limit result = %#v, want exact loaded plan %#v", got, input)
			}
		})
	}
}

func TestMixedSelectorPolicyExecutorPassesLoaderErrorUnchanged(t *testing.T) {
	wantErr := errors.New("loader failure")
	for _, key := range []mixedWrapperKey{
		{family: mixedTopKFamily, site: mixedRootAdmission},
		{family: mixedTopKFamily, site: mixedPlanAdmission},
		{family: mixedLimitFamily, site: mixedOperandAdmission},
		{family: mixedLimitFamily, site: mixedPlanAdmission},
	} {
		t.Run(string(key.family)+"/"+string(key.site), func(t *testing.T) {
			calls := 0
			plan, err := executeMixedSelectorPolicy(key.family, key.site, func() (chplan.Node, error) {
				calls++
				return &chplan.OneRow{}, wantErr
			})
			if calls != 1 || plan != nil || err != wantErr {
				t.Fatalf("calls=%d plan=%v err=%v, want one call, nil plan, unchanged error", calls, plan, err)
			}
		})
	}
}

func TestMixedSelectorPolicyExecutorRejectsEveryWrongModeBeforeLoading(t *testing.T) {
	for _, key := range []mixedWrapperKey{
		{family: mixedTopKFamily, site: mixedRootAdmission},
		{family: mixedTopKFamily, site: mixedPlanAdmission},
		{family: mixedLimitFamily, site: mixedOperandAdmission},
		{family: mixedLimitFamily, site: mixedPlanAdmission},
	} {
		expected := mixedFloatOnly
		if key.family == mixedLimitFamily {
			expected = mixedPreserve
		}
		for _, mode := range []mixedOperandPolicy{mixedReject, mixedBespoke, mixedFloatOnly, mixedPreserve, mixedOperandPolicy(255)} {
			if mode == expected {
				continue
			}
			t.Run(string(key.family)+"/"+string(key.site)+"/mode", func(t *testing.T) {
				assertMixedSelectorPolicyRejected(t, key, &mode)
			})
		}
		t.Run(string(key.family)+"/"+string(key.site)+"/missing", func(t *testing.T) {
			assertMixedSelectorPolicyRejected(t, key, nil)
		})
	}
}

func TestMixedSelectorPolicyExecutorRejectsInventedPairsDespiteTableInjection(t *testing.T) {
	for _, tc := range []struct {
		key  mixedWrapperKey
		mode mixedOperandPolicy
	}{
		{mixedWrapperKey{family: mixedTopKFamily, site: mixedOperandAdmission}, mixedFloatOnly},
		{mixedWrapperKey{family: mixedLimitFamily, site: mixedRootAdmission}, mixedPreserve},
		{mixedWrapperKey{family: mixedTopKFamily, site: "invented-site"}, mixedFloatOnly},
		{mixedWrapperKey{family: "invented-family", site: mixedPlanAdmission}, mixedPreserve},
		// This is a real, valid table row for another family. Selector policy
		// remains closed even without test-only table injection.
		{mixedWrapperKey{family: mixedMathFamily, site: mixedRootAdmission}, mixedFloatOnly},
	} {
		t.Run(string(tc.key.family)+"/"+string(tc.key.site), func(t *testing.T) {
			assertMixedSelectorPolicyRejected(t, tc.key, &tc.mode)
		})
	}
}

func assertMixedSelectorPolicyRejected(t *testing.T, key mixedWrapperKey, injected *mixedOperandPolicy) {
	t.Helper()
	prior, existed := mixedOperandPolicies[key]
	if injected == nil {
		delete(mixedOperandPolicies, key)
	} else {
		mixedOperandPolicies[key] = *injected
	}
	t.Cleanup(func() {
		if existed {
			mixedOperandPolicies[key] = prior
		} else {
			delete(mixedOperandPolicies, key)
		}
	})
	calls := 0
	plan, err := executeMixedSelectorPolicy(key.family, key.site, func() (chplan.Node, error) {
		calls++
		return &chplan.OneRow{}, nil
	})
	if calls != 0 || plan != nil || err == nil {
		t.Fatalf("rejected policy called=%d plan=%v err=%v", calls, plan, err)
	}
}
