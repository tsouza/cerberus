package promql

import (
	"errors"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// No parallel cases: these tests temporarily replace the shared policy table.
func TestSortPolicyDrivesPreparation(t *testing.T) {
	const unknownPolicy mixedOperandPolicy = 255
	for _, site := range []mixedAdmissionSite{mixedOperandAdmission, mixedPlanAdmission} {
		t.Run(string(site), func(t *testing.T) {
			key := mixedWrapperKey{family: mixedSortFamily, site: site}
			original := mixedOperandPolicies[key]
			t.Cleanup(func() { mixedOperandPolicies[key] = original })
			for _, policy := range []mixedOperandPolicy{mixedReject, mixedBespoke, mixedPreserve, unknownPolicy} {
				mixedOperandPolicies[key] = policy
				called := false
				plan, err := prepareSortOperand(site, func() (chplan.Node, error) {
					called = true
					return &chplan.VectorSetOp{Mixed: true}, nil
				})
				if called || plan != nil || err == nil {
					t.Fatalf("unsupported policy %v reached loader: called=%v plan=%v err=%v", policy, called, plan, err)
				}
			}
			delete(mixedOperandPolicies, key)
			called := false
			plan, err := prepareSortOperand(site, func() (chplan.Node, error) {
				called = true
				return &chplan.OneRow{}, nil
			})
			if called || plan != nil || err == nil {
				t.Fatalf("missing policy reached loader: called=%v plan=%v err=%v", called, plan, err)
			}
			mixedOperandPolicies[key] = mixedFloatOnly
			floatInput := &chplan.OneRow{}
			floatPlan, floatErr := prepareSortOperand(site, func() (chplan.Node, error) { return floatInput, nil })
			if floatErr != nil || floatPlan != floatInput {
				t.Fatalf("already-float input changed: plan=%v err=%v", floatPlan, floatErr)
			}
			originalUnion := &chplan.VectorSetOp{Mixed: true}
			calls := 0
			plan, err = prepareSortOperand(site, func() (chplan.Node, error) {
				calls++
				return originalUnion, nil
			})
			narrow, ok := plan.(*chplan.Filter)
			if err != nil || calls != 1 || !ok || !chplan.IsMixedFloatNarrowing(narrow) || narrow.Input != originalUnion {
				t.Fatalf("float-only mode must narrow original union once: plan=%#v calls=%d err=%v", plan, calls, err)
			}
			sentinel := errors.New("sort operand sentinel")
			plan, err = prepareSortOperand(site, func() (chplan.Node, error) {
				return nil, sentinel
			})
			if plan != nil || err != sentinel {
				t.Fatalf("loader error changed: plan=%v err=%v", plan, err)
			}
			again, err := prepareSortOperand(site, func() (chplan.Node, error) { return narrow, nil })
			if err != nil || again != narrow {
				t.Fatalf("prepared input narrowed twice: plan=%v err=%v", again, err)
			}
		})
	}
}

func TestSortPolicyOrdinaryBypassAndLoweringError(t *testing.T) {
	key := mixedWrapperKey{family: mixedSortFamily, site: mixedPlanAdmission}
	original := mixedOperandPolicies[key]
	t.Cleanup(func() { mixedOperandPolicies[key] = original })
	s := schema.DefaultOTelMetrics()
	invalid := &parser.Call{Args: parser.Expressions{&parser.StringLiteral{Val: "not a vector"}}}
	_, wantError := lowerSortFloatOperand(invalid, s, lowerCtx{})
	if wantError == nil {
		t.Fatal("invalid operand unexpectedly lowered")
	}
	mixedOperandPolicies[key] = mixedReject
	_, gotError := lowerSortFloatOperand(invalid, s, lowerCtx{})
	if gotError == nil || gotError.Error() != wantError.Error() {
		t.Fatalf("plan policy replaced original lowering error: got=%v want=%v", gotError, wantError)
	}
	ordinary := &parser.Call{Args: parser.Expressions{mustParse(t, "num_cpus")}}
	if _, err := lowerSortFloatOperand(ordinary, s, lowerCtx{}); err != nil {
		t.Fatalf("ordinary float consulted mixed plan policy: %v", err)
	}
}
