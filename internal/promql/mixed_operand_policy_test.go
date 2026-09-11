package promql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// Deliberately non-parallel: each case temporarily removes one authorization
// from the package table, proving the real dispatch consults it before lowering
// or building its wrapper. Parallel tests run only after this test returns.
func TestMixedOperandPolicyControlsActualDispatch(t *testing.T) {
	const direct = `(latency_exp_hist or num_cpus)`
	const nested = `sort_by_label(latency_exp_hist or num_cpus, "job")`
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		query  string
		family mixedWrapperFamily
		site   mixedAdmissionSite
	}{
		{`abs(` + direct + `)`, mixedMathFamily, mixedRootAdmission},
		{`abs(` + nested + `)`, mixedMathFamily, mixedPlanAdmission},
		{`clamp(` + nested + `, 2, 1)`, mixedMathFamily, mixedPlanAdmission},
		{`label_replace(` + nested + `, "dst", "x", "job", ".*")`, mixedLabelFamily, mixedPlanAdmission},
		{`label_replace(` + direct + `, "dst", "x", "job", ".*")`, mixedLabelFamily, mixedRootAdmission},
		{`label_join(` + direct + `, "dst", "-", "job")`, mixedLabelFamily, mixedRootAdmission},
		{`label_join(` + nested + `, "dst", "-", "job")`, mixedLabelFamily, mixedPlanAdmission},
		{`sort(` + direct + `)`, mixedSortFamily, mixedOperandAdmission},
		{`scalar(` + direct + `)`, mixedScalarFamily, mixedOperandAdmission},
		{`absent(` + direct + `)`, mixedAbsentFamily, mixedOperandAdmission},
		{`sort(` + nested + `)`, mixedSortFamily, mixedPlanAdmission},
		{`scalar(` + nested + `)`, mixedScalarFamily, mixedPlanAdmission},
		{`absent(` + nested + `)`, mixedAbsentFamily, mixedPlanAdmission},
		{`histogram_count(` + nested + `)`, mixedHistogramValueFamily, mixedPlanAdmission},
		{nested + ` + up`, mixedVectorArithmeticFamily, mixedPlanAdmission},
		{nested + ` > bool up`, mixedVectorComparisonFamily, mixedPlanAdmission},
		{`sum(` + nested + `)`, mixedSumAvgFamily, mixedPlanAdmission},
		{`count(` + nested + `)`, mixedCountGroupFamily, mixedPlanAdmission},
		{`min(` + nested + `)`, mixedFloatAggregateFamily, mixedPlanAdmission},
		{`topk(2, ` + nested + `)`, mixedTopKFamily, mixedPlanAdmission},
		{`count_values("v", ` + nested + `)`, mixedCountValuesFamily, mixedPlanAdmission},
	} {
		t.Run(tc.query, func(t *testing.T) {
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := LowerAt(context.Background(), expr, s, at, at); err != nil {
				t.Fatalf("registered baseline rejected: %v", err)
			}
			key := mixedWrapperKey{family: tc.family, site: tc.site}
			policy, ok := mixedOperandPolicies[key]
			if !ok {
				t.Fatalf("missing baseline authorization %v", key)
			}
			delete(mixedOperandPolicies, key)
			t.Cleanup(func() { mixedOperandPolicies[key] = policy })
			_, err = LowerAt(context.Background(), expr, s, at, at)
			if err == nil || !strings.Contains(err.Error(), "mixed operand is not admitted for "+string(tc.family)+" at "+string(tc.site)) {
				t.Fatalf("missing authorization did not reject actual dispatch: %v", err)
			}
		})
	}
}

func mustProjectAttributesOverInner(t *testing.T, inner chplan.Node, s schema.Metrics, build func(sampleRoleRefs) chplan.Expr) *chplan.Project {
	t.Helper()
	project, err := projectAttributesOverInner(inner, s, mixedLabelFamily, build)
	if err != nil {
		t.Fatal(err)
	}
	return project
}

func TestMixedOperandPolicyComputedClampChecksOriginalOperand(t *testing.T) {
	const operand = `sort_by_label(latency_exp_hist or num_cpus, "job")`
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	innerExpr, err := p.ParseExpr(operand)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := LowerAt(context.Background(), innerExpr, s, at, at)
	if err != nil || chplan.RowShapeOf(inner) != chplan.MixedRowShape {
		t.Fatalf("operand must independently lower to Mixed: %T, %v", inner, err)
	}
	key := mixedWrapperKey{family: mixedMathFamily, site: mixedPlanAdmission}
	policy := mixedOperandPolicies[key]
	delete(mixedOperandPolicies, key)
	t.Cleanup(func() { mixedOperandPolicies[key] = policy })
	expr, err := p.ParseExpr(`clamp(` + operand + `, scalar(vector(2)), scalar(vector(1)))`)
	if err != nil {
		t.Fatal(err)
	}
	// Test denial before the unmarked bound Filter is constructed. This is not
	// an assertion that the admitted wrapper's downstream behavior is correct.
	_, err = LowerAt(context.Background(), expr, s, at, at)
	if err == nil || !strings.Contains(err.Error(), "mixed operand is not admitted for math-round-clamp at existing-plan") {
		t.Fatalf("computed clamp bypassed original-operand authorization: %v", err)
	}
}

func TestMixedOperandPolicyAlreadyLoweredShape(t *testing.T) {
	for _, tc := range []struct {
		name      string
		inner     chplan.Node
		family    mixedWrapperFamily
		wantError bool
	}{
		{"mixed unknown family", &chplan.VectorSetOp{Mixed: true}, "unlisted-wrapper", true},
		{"mixed root-only family", &chplan.VectorSetOp{Mixed: true}, mixedLeafFamily, true},
		{"mixed known bespoke consumer", &chplan.VectorSetOp{Mixed: true}, mixedTimestampFamily, false},
		{"date must use its payload preparation", &chplan.VectorSetOp{Mixed: true}, mixedDateFamily, true},
		{"math must use its payload preparation", &chplan.VectorSetOp{Mixed: true}, mixedMathFamily, true},
		{"ordinary float unchanged", &chplan.Scan{}, "unlisted-wrapper", false},
		{"histogram-only unchanged", &chplan.HistogramProjection{Input: &chplan.OneRow{}}, "unlisted-wrapper", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := requireMixedPlanPolicy(tc.inner, tc.family)
			if (err != nil) != tc.wantError {
				t.Fatalf("requireMixedPlanPolicy() error = %v, want error %v", err, tc.wantError)
			}
		})
	}
}

func TestMixedOperandPolicyOperatorFamiliesFailClosed(t *testing.T) {
	if got := mixedVectorBinaryFamily(chplan.BinaryOp("unlisted")); got != "" {
		t.Fatalf("unknown vector operator acquired family %q", got)
	}
	for _, scalarOnLeft := range []bool{false, true} {
		if got := mixedScalarBinaryFamily(chplan.BinaryOp("unlisted"), scalarOnLeft); got != "" {
			t.Fatalf("unknown scalar operator acquired family %q", got)
		}
	}
	if got := mixedAggregateFamily(parser.ItemType(-1)); got != "" {
		t.Fatalf("unknown aggregate acquired family %q", got)
	}
	for _, tc := range []struct {
		op   chplan.BinaryOp
		left bool
		want mixedWrapperFamily
	}{
		{chplan.OpMul, false, mixedScaleFamily},
		{chplan.OpMul, true, mixedScaleFamily},
		{chplan.OpDiv, false, mixedScaleFamily},
		{chplan.OpDiv, true, mixedArithmeticFamily},
		{chplan.OpAdd, false, mixedArithmeticFamily},
		{chplan.OpGt, true, mixedComparisonFamily},
	} {
		if got := mixedScalarBinaryFamily(tc.op, tc.left); got != tc.want {
			t.Errorf("scalar family(%v, left=%v) = %q, want %q", tc.op, tc.left, got, tc.want)
		}
	}
}

func TestMixedOperandPolicyAdmissionInventory(t *testing.T) {
	want := map[mixedAdmissionSite][]mixedWrapperFamily{
		mixedRootAdmission: {
			mixedLeafFamily, mixedSumAvgFamily, mixedCountGroupFamily,
			mixedFloatAggregateFamily, mixedTopKFamily, mixedCountValuesFamily,
			mixedLabelFamily, mixedMathFamily, mixedScaleFamily, mixedArithmeticFamily,
			mixedComparisonFamily, mixedVectorArithmeticFamily, mixedVectorComparisonFamily,
			mixedSubqueryFamily,
		},
		mixedOperandAdmission: {
			mixedScalarFamily, mixedSortFamily, mixedSortByLabelFamily, mixedDateFamily,
			mixedTimestampFamily, mixedInfoFamily, mixedHistogramValueFamily,
			mixedLimitFamily, mixedUnaryFamily, mixedSetOperandFamily,
			mixedSubqueryFamily,
			mixedAbsentFamily,
		},
		mixedPlanAdmission: {
			mixedMathFamily, mixedDateFamily, mixedTimestampFamily, mixedUnaryFamily,
			mixedArithmeticFamily, mixedComparisonFamily, mixedScaleFamily,
			mixedLabelFamily, mixedScalarFamily, mixedSubqueryFamily,
			mixedSortFamily, mixedSortByLabelFamily, mixedInfoFamily,
			mixedLimitFamily, mixedSetOperandFamily,
			mixedAbsentFamily, mixedHistogramValueFamily,
			mixedVectorArithmeticFamily, mixedVectorComparisonFamily,
			mixedSumAvgFamily, mixedCountGroupFamily, mixedFloatAggregateFamily,
			mixedTopKFamily, mixedCountValuesFamily,
		},
	}
	count := 0
	for site, families := range want {
		for _, family := range families {
			count++
			key := mixedWrapperKey{family: family, site: site}
			wantPolicy := mixedBespoke
			switch family {
			case mixedMathFamily, mixedArithmeticFamily, mixedComparisonFamily, mixedScalarFamily, mixedSortFamily, mixedDateFamily, mixedTopKFamily:
				wantPolicy = mixedFloatOnly
			case mixedLabelFamily, mixedSortByLabelFamily, mixedCountGroupFamily, mixedLimitFamily:
				wantPolicy = mixedPreserve
			}
			if got := mixedOperandPolicies[key]; got != wantPolicy {
				t.Errorf("admission %v = %v, want %v", key, got, wantPolicy)
			}
		}
	}
	if len(mixedOperandPolicies) != count {
		t.Fatalf("admission table has %d entries, want %d", len(mixedOperandPolicies), count)
	}
}

func TestMixedOperandPolicyRejectsUnknownBeforeLowering(t *testing.T) {
	for _, key := range []mixedWrapperKey{
		{},
		{family: "unlisted-wrapper", site: mixedRootAdmission},
		{family: mixedMathFamily, site: "unlisted-site"},
		{family: mixedLeafFamily, site: mixedOperandAdmission},
		{family: mixedMathFamily, site: mixedOperandAdmission},
		{family: "unlisted-wrapper", site: mixedPlanAdmission},
	} {
		t.Run(string(key.family)+"/"+string(key.site), func(t *testing.T) {
			called := false
			plan, err := lowerWithBespokeMixedOperandPolicy(key.family, key.site, func() (chplan.Node, error) {
				called = true
				return &chplan.OneRow{}, nil
			})
			if called || plan != nil || err == nil {
				t.Fatalf("unlisted admission called=%v plan=%v err=%v", called, plan, err)
			}
		})
	}
}

// Deliberately non-parallel: each case installs the closed sentinel into the
// package policy table. A successful lookup must not turn that fail-closed
// value into an authorization merely because it equals the requested policy.
func TestMixedOperandPolicyClosedSentinelCannotBeAuthorizedByTable(t *testing.T) {
	t.Run("operand admission", func(t *testing.T) {
		key := mixedWrapperKey{family: mixedMathFamily, site: mixedRootAdmission}
		original := mixedOperandPolicies[key]
		mixedOperandPolicies[key] = mixedPolicyClosed
		t.Cleanup(func() { mixedOperandPolicies[key] = original })

		transform, err := lowerWithMixedOperandPolicy(key.family, key.site, mixedPolicyClosed)
		if transform != nil || err == nil {
			t.Fatalf("closed policy admitted: transform=%v err=%v", transform != nil, err)
		}
	})

	t.Run("plan admission", func(t *testing.T) {
		key := mixedWrapperKey{family: mixedMathFamily, site: mixedPlanAdmission}
		original := mixedOperandPolicies[key]
		mixedOperandPolicies[key] = mixedPolicyClosed
		t.Cleanup(func() { mixedOperandPolicies[key] = original })

		if err := requireMixedPlanPolicy(nil, key.family, mixedPolicyClosed); err == nil {
			t.Fatal("closed policy admitted")
		}
	})
}

func TestMixedOperandPolicyPreservesBespokeResultAndError(t *testing.T) {
	for key, policy := range mixedOperandPolicies {
		t.Run(string(key.family)+"/"+string(key.site), func(t *testing.T) {
			wantPlan := &chplan.OneRow{}
			wantError := errors.New("operand lowering error")
			calls := 0
			plan, err := lowerWithBespokeMixedOperandPolicy(key.family, key.site, func() (chplan.Node, error) {
				calls++
				return wantPlan, wantError
			})
			if policy != mixedBespoke {
				if calls != 0 || plan != nil || err == nil {
					t.Fatalf("migrated policy reached bespoke callback: calls=%d plan=%v err=%v", calls, plan, err)
				}
				return
			}
			if calls != 1 || plan != wantPlan || err != wantError {
				t.Fatalf("bespoke result calls=%d plan=%v err=%v", calls, plan, err)
			}
		})
	}
}

func TestMixedOperandPolicyDoesNotTreatUnimplementedModesAsBespoke(t *testing.T) {
	key := mixedWrapperKey{family: mixedMathFamily, site: mixedRootAdmission}
	for _, policy := range []mixedOperandPolicy{mixedReject, mixedFloatOnly, mixedPreserve, mixedPolicyClosed} {
		if err := requireMixedBespokePolicy(key, policy); err == nil {
			t.Fatalf("policy %v reached an unmigrated bespoke dispatcher", policy)
		}
	}
}
