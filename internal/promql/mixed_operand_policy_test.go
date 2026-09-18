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

const (
	mixedPolicyDirect = `(latency_exp_hist or num_cpus)`
	mixedPolicyNested = `sort_by_label(latency_exp_hist or num_cpus, "job")`
)

// mixedPolicyDispatchInventory names, for every row of the policy table, a
// query whose lowering consults that row. TestMixedOperandPolicyControlsActualDispatch
// proves each entry load-bearing in both directions and that no row is left
// unnamed; TestMixedRelationShapeAgreesWithEveryPolicyConsumer reuses the
// same queries to hold the static mixed-relation predicate to what the
// lowerings actually produce.
var mixedPolicyDispatchInventory = []struct {
	query  string
	family mixedWrapperFamily
	site   mixedAdmissionSite
}{
	{mixedPolicyDirect, mixedLeafFamily, mixedRootAdmission},
	{`sum(` + mixedPolicyDirect + `)`, mixedSumAvgFamily, mixedRootAdmission},
	{`count(` + mixedPolicyDirect + `)`, mixedCountGroupFamily, mixedRootAdmission},
	{`min(` + mixedPolicyDirect + `)`, mixedFloatAggregateFamily, mixedRootAdmission},
	{`topk(2, ` + mixedPolicyDirect + `)`, mixedTopKFamily, mixedRootAdmission},
	{`count_values("v", ` + mixedPolicyDirect + `)`, mixedCountValuesFamily, mixedRootAdmission},
	{`label_replace(` + mixedPolicyDirect + `, "dst", "x", "job", ".*")`, mixedLabelFamily, mixedRootAdmission},
	{`label_join(` + mixedPolicyDirect + `, "dst", "-", "job")`, mixedLabelFamily, mixedRootAdmission},
	{`abs(` + mixedPolicyDirect + `)`, mixedMathFamily, mixedRootAdmission},
	{mixedPolicyDirect + ` * 2`, mixedScaleFamily, mixedRootAdmission},
	{mixedPolicyDirect + ` + 1`, mixedArithmeticFamily, mixedRootAdmission},
	{mixedPolicyDirect + ` > 1`, mixedComparisonFamily, mixedRootAdmission},
	{mixedPolicyDirect + ` + up`, mixedVectorArithmeticFamily, mixedRootAdmission},
	{mixedPolicyDirect + ` + ` + mixedPolicyDirect, mixedVectorArithmeticFamily, mixedRootAdmission},
	{mixedPolicyDirect + ` > bool up`, mixedVectorComparisonFamily, mixedRootAdmission},
	{mixedPolicyDirect + ` == ` + mixedPolicyDirect, mixedVectorComparisonFamily, mixedRootAdmission},
	{`sum(rate(` + mixedPolicyDirect + `[5m:1m]))`, mixedSubqueryFamily, mixedRootAdmission},
	{`scalar(` + mixedPolicyDirect + `)`, mixedScalarFamily, mixedOperandAdmission},
	{`sort(` + mixedPolicyDirect + `)`, mixedSortFamily, mixedOperandAdmission},
	{mixedPolicyNested, mixedSortByLabelFamily, mixedOperandAdmission},
	{`year(` + mixedPolicyDirect + `)`, mixedDateFamily, mixedOperandAdmission},
	{`timestamp(` + mixedPolicyDirect + `)`, mixedTimestampFamily, mixedOperandAdmission},
	{`info(` + mixedPolicyDirect + `)`, mixedInfoFamily, mixedOperandAdmission},
	{`histogram_count(` + mixedPolicyDirect + `)`, mixedHistogramValueFamily, mixedOperandAdmission},
	{`limitk(5, ` + mixedPolicyDirect + `)`, mixedLimitFamily, mixedOperandAdmission},
	{`-` + mixedPolicyDirect, mixedUnaryFamily, mixedOperandAdmission},
	{`+` + mixedPolicyDirect, mixedUnaryFamily, mixedOperandAdmission},
	{mixedPolicyDirect + ` and up`, mixedSetOperandFamily, mixedOperandAdmission},
	{`last_over_time((sum(` + mixedPolicyDirect + `))[5m:1m])`, mixedSubqueryFamily, mixedOperandAdmission},
	{`absent(` + mixedPolicyDirect + `)`, mixedAbsentFamily, mixedOperandAdmission},
	{`abs(` + mixedPolicyNested + `)`, mixedMathFamily, mixedPlanAdmission},
	{`clamp(` + mixedPolicyNested + `, 2, 1)`, mixedMathFamily, mixedPlanAdmission},
	{`year(` + mixedPolicyNested + `)`, mixedDateFamily, mixedPlanAdmission},
	{`timestamp(` + mixedPolicyNested + `)`, mixedTimestampFamily, mixedPlanAdmission},
	{`+` + mixedPolicyNested, mixedUnaryFamily, mixedPlanAdmission},
	{`-` + mixedPolicyNested, mixedUnaryFamily, mixedPlanAdmission},
	{mixedPolicyNested + ` * 2`, mixedScaleFamily, mixedPlanAdmission},
	{`2 * ` + mixedPolicyNested, mixedScaleFamily, mixedPlanAdmission},
	{mixedPolicyNested + ` / 2`, mixedScaleFamily, mixedPlanAdmission},
	{mixedPolicyNested + ` * scalar(vector(2))`, mixedScaleFamily, mixedPlanAdmission},
	{mixedPolicyNested + ` + 1`, mixedArithmeticFamily, mixedPlanAdmission},
	{mixedPolicyNested + ` > 1`, mixedComparisonFamily, mixedPlanAdmission},
	{mixedPolicyNested + ` + up`, mixedVectorArithmeticFamily, mixedPlanAdmission},
	{`up - ` + mixedPolicyNested, mixedVectorArithmeticFamily, mixedPlanAdmission},
	{mixedPolicyNested + ` + ` + mixedPolicyNested, mixedVectorArithmeticFamily, mixedPlanAdmission},
	{mixedPolicyNested + ` > bool up`, mixedVectorComparisonFamily, mixedPlanAdmission},
	{mixedPolicyNested + ` == ` + mixedPolicyNested, mixedVectorComparisonFamily, mixedPlanAdmission},
	{`label_replace(` + mixedPolicyNested + `, "dst", "x", "job", ".*")`, mixedLabelFamily, mixedPlanAdmission},
	{`label_join(` + mixedPolicyNested + `, "dst", "-", "job")`, mixedLabelFamily, mixedPlanAdmission},
	{`scalar(` + mixedPolicyNested + `)`, mixedScalarFamily, mixedPlanAdmission},
	{`last_over_time(` + mixedPolicyDirect + `[5m:1m])`, mixedSubqueryFamily, mixedPlanAdmission},
	{`sort(` + mixedPolicyNested + `)`, mixedSortFamily, mixedPlanAdmission},
	{`sort_by_label(` + mixedPolicyNested + `, "instance")`, mixedSortByLabelFamily, mixedPlanAdmission},
	{`info(` + mixedPolicyNested + `)`, mixedInfoFamily, mixedPlanAdmission},
	{`limitk(5, ` + mixedPolicyNested + `)`, mixedLimitFamily, mixedPlanAdmission},
	{mixedPolicyNested + ` and up`, mixedSetOperandFamily, mixedPlanAdmission},
	{`absent(` + mixedPolicyNested + `)`, mixedAbsentFamily, mixedPlanAdmission},
	{`histogram_count(` + mixedPolicyNested + `)`, mixedHistogramValueFamily, mixedPlanAdmission},
	{`sum(` + mixedPolicyNested + `)`, mixedSumAvgFamily, mixedPlanAdmission},
	{`count(` + mixedPolicyNested + `)`, mixedCountGroupFamily, mixedPlanAdmission},
	{`min(` + mixedPolicyNested + `)`, mixedFloatAggregateFamily, mixedPlanAdmission},
	{`topk(2, ` + mixedPolicyNested + `)`, mixedTopKFamily, mixedPlanAdmission},
	{`count_values("v", ` + mixedPolicyNested + `)`, mixedCountValuesFamily, mixedPlanAdmission},
}

// Deliberately non-parallel: each case temporarily removes one authorization
// from the package table, proving the real dispatch consults it before lowering
// or building its wrapper. Parallel tests run only after this test returns.
//
// The inventory is COMPLETE by assertion: every row of the table must be
// named by at least one query that lowers with the row present and rejects
// with exactly that row's key once it is removed. A row no consumer cites — a
// policy with no handler behind it, the shape that let a nested mixed plan
// reach a float-only consumer under a "bespoke" row (cerberus issue #3562) —
// fails this test, so the table can only ever hold rows a real consumer
// selects its transform through.
func TestMixedOperandPolicyControlsActualDispatch(t *testing.T) {
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	covered := map[mixedWrapperKey]bool{}
	for _, tc := range mixedPolicyDispatchInventory {
		covered[mixedWrapperKey{family: tc.family, site: tc.site}] = true
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
	for key := range mixedOperandPolicies {
		if !covered[key] {
			t.Errorf("policy row %v has no consumer in this inventory: a row nothing dispatches through is a policy without a handler", key)
		}
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
	s := schema.DefaultOTelMetrics()
	mixed := func() chplan.Node {
		return &chplan.VectorSetOp{
			Mixed:            true,
			MetricNameColumn: s.MetricNameColumn,
			AttributesColumn: s.AttributesColumn,
			TimestampColumn:  s.TimestampColumn,
			ValueColumn:      s.ValueColumn,
		}
	}
	for _, tc := range []struct {
		name      string
		inner     chplan.Node
		family    mixedWrapperFamily
		expected  mixedOperandPolicy
		wantError bool
	}{
		{"mixed unknown family", mixed(), "unlisted-wrapper", mixedBespoke, true},
		{"mixed root-only family", mixed(), mixedLeafFamily, mixedBespoke, true},
		{"mixed known bespoke consumer", mixed(), mixedTimestampFamily, mixedBespoke, false},
		{"date must use its payload preparation", mixed(), mixedDateFamily, mixedBespoke, true},
		{"math must use its payload preparation", mixed(), mixedMathFamily, mixedBespoke, true},
		{"math float-only consumer", mixed(), mixedMathFamily, mixedFloatOnly, false},
		{"unary bespoke consumer scales or forwards", mixed(), mixedUnaryFamily, mixedBespoke, false},
		{"unary float-only consumer drops histograms", mixed(), mixedUnaryFamily, mixedFloatOnly, true},
		{"scale bespoke consumer scales in place", mixed(), mixedScaleFamily, mixedBespoke, false},
		{"scale float-only consumer drops histograms", mixed(), mixedScaleFamily, mixedFloatOnly, true},
		{"vector arithmetic bespoke consumer joins by discriminator", mixed(), mixedVectorArithmeticFamily, mixedBespoke, false},
		{"vector arithmetic float-only consumer reads the placeholder Value", mixed(), mixedVectorArithmeticFamily, mixedFloatOnly, true},
		{"vector comparison bespoke consumer joins by discriminator", mixed(), mixedVectorComparisonFamily, mixedBespoke, false},
		{"vector comparison float-only consumer reads the placeholder Value", mixed(), mixedVectorComparisonFamily, mixedFloatOnly, true},
		{"reject sentinel never authorizes", mixed(), mixedTimestampFamily, mixedReject, true},
		{"closed sentinel never authorizes", mixed(), mixedTimestampFamily, mixedPolicyClosed, true},
		{"ordinary float unchanged", &chplan.Scan{}, "unlisted-wrapper", mixedBespoke, false},
		{"ordinary float unchanged under float-only", &chplan.Scan{}, mixedScaleFamily, mixedFloatOnly, false},
		{"histogram-only unchanged", &chplan.HistogramProjection{Input: &chplan.OneRow{}}, "unlisted-wrapper", mixedBespoke, false},
		{"nil inner is already-established mixed-ness", nil, "unlisted-wrapper", mixedBespoke, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := requireMixedPlanPolicy(tc.inner, tc.family, tc.expected)
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
			mixedScaleFamily, mixedArithmeticFamily, mixedComparisonFamily,
			mixedVectorArithmeticFamily, mixedVectorComparisonFamily,
			mixedLabelFamily, mixedScalarFamily, mixedSubqueryFamily,
			mixedSortFamily, mixedSortByLabelFamily, mixedInfoFamily,
			mixedLimitFamily, mixedSetOperandFamily,
			mixedAbsentFamily, mixedHistogramValueFamily,
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
			if key == (mixedWrapperKey{family: mixedFloatAggregateFamily, site: mixedPlanAdmission}) {
				wantPolicy = mixedFloatOnly
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

// A consumer's requested rule must match the table row exactly; every other
// mode — including the two fail-closed sentinels, even when a row happens to
// hold them — is refused.
func TestMixedOperandPolicyRequiresExactRuleAgreement(t *testing.T) {
	key := mixedWrapperKey{family: mixedMathFamily, site: mixedRootAdmission}
	if err := requireMixedOperandPolicy(key.family, key.site, mixedFloatOnly); err != nil {
		t.Fatalf("registered rule refused: %v", err)
	}
	for _, policy := range []mixedOperandPolicy{mixedReject, mixedBespoke, mixedPreserve, mixedPolicyClosed} {
		if err := requireMixedOperandPolicy(key.family, key.site, policy); err == nil {
			t.Fatalf("policy %v admitted against a %v row", policy, mixedOperandPolicies[key])
		}
	}
	for _, sentinel := range []mixedOperandPolicy{mixedReject, mixedPolicyClosed} {
		original := mixedOperandPolicies[key]
		mixedOperandPolicies[key] = sentinel
		err := requireMixedOperandPolicy(key.family, key.site, sentinel)
		mixedOperandPolicies[key] = original
		if err == nil {
			t.Fatalf("sentinel %v authorized itself through the table", sentinel)
		}
	}
}

// The static mixed-relation predicate [isMixedRelationShape] is what keeps
// the histogram-vs-float-vector recognisers from reading a mixed operand as
// a float vector. It must agree with the lowerings: over the dispatch
// inventory, every query whose plan is a live mixed relation is recognised,
// and every recognised query lowers to one. A disagreement in either
// direction is a shape that would be answered wrongly or refused although
// its root answers it.
func TestMixedRelationShapeAgreesWithEveryPolicyConsumer(t *testing.T) {
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range mixedPolicyDispatchInventory {
		t.Run(tc.query, func(t *testing.T) {
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatal(err)
			}
			live := mixedRowsNeedPreparation(plan)
			static := isMixedRelationShape(expr, s, lowerCtx{start: at, end: at})
			if live != static {
				t.Fatalf("plan is live mixed: %v, isMixedRelationShape: %v", live, static)
			}
		})
	}
}
