package promql

import (
	"errors"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// No parallel cases: these tests temporarily replace the shared policy table.
func TestMixedMathPolicyDrivesPreparation(t *testing.T) {
	const unknownPolicy mixedOperandPolicy = 255
	s := schema.DefaultOTelMetrics()
	for _, site := range []mixedAdmissionSite{mixedRootAdmission, mixedPlanAdmission} {
		t.Run(string(site), func(t *testing.T) {
			key := mixedWrapperKey{family: mixedMathFamily, site: site}
			original := mixedOperandPolicies[key]
			t.Cleanup(func() { mixedOperandPolicies[key] = original })
			for _, policy := range []mixedOperandPolicy{mixedReject, mixedBespoke, mixedPreserve, unknownPolicy} {
				mixedOperandPolicies[key] = policy
				called := false
				plan, err := prepareMixedMathOperand(site, func() (chplan.Node, error) {
					called = true
					return &chplan.VectorSetOp{Mixed: true}, nil
				})
				if called || plan != nil || err == nil {
					t.Fatalf("unsupported policy %v reached loader: called=%v plan=%v err=%v", policy, called, plan, err)
				}
			}
			delete(mixedOperandPolicies, key)
			called := false
			plan, err := prepareMixedMathOperand(site, func() (chplan.Node, error) {
				called = true
				return &chplan.OneRow{}, nil
			})
			if called || plan != nil || err == nil {
				t.Fatalf("missing policy reached loader: called=%v plan=%v err=%v", called, plan, err)
			}
			mixedOperandPolicies[key] = mixedFloatOnly
			originalUnion := &chplan.VectorSetOp{
				Mixed:            true,
				MetricNameColumn: s.MetricNameColumn,
				AttributesColumn: s.AttributesColumn,
				TimestampColumn:  s.TimestampColumn,
				ValueColumn:      s.ValueColumn,
			}
			calls := 0
			plan, err = prepareMixedMathOperand(site, func() (chplan.Node, error) {
				calls++
				return originalUnion, nil
			})
			narrow, ok := plan.(*chplan.Filter)
			if err != nil || calls != 1 || !ok || !chplan.IsMixedFloatNarrowing(narrow) || narrow.Input != originalUnion {
				t.Fatalf("float-only mode must narrow original union once: plan=%#v calls=%d err=%v", plan, calls, err)
			}
			sentinel := errors.New("math operand sentinel")
			plan, err = prepareMixedMathOperand(site, func() (chplan.Node, error) {
				return nil, sentinel
			})
			if plan != nil || err != sentinel {
				t.Fatalf("loader error changed: plan=%v err=%v", plan, err)
			}
			again, err := prepareMixedMathOperand(site, func() (chplan.Node, error) { return narrow, nil })
			if err != nil || again != narrow {
				t.Fatalf("prepared input narrowed twice: plan=%v err=%v", again, err)
			}
		})
	}
}
