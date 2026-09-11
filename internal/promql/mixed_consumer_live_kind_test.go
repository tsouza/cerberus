package promql

import (
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestMixedConsumersConsultPolicyOnlyForLiveMixedRows(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	live, floatRows := mixedConsumerTestRows(s)
	arg := mustParse(t, "up")
	year := mustParse(t, "year(up)").(*parser.Call)
	timestamp := mustParse(t, "timestamp(up)").(*parser.Call)

	tests := []struct {
		name string
		key  mixedWrapperKey
		run  func(chplan.Node) (chplan.Node, error)
	}{
		{
			name: "preserve",
			key:  mixedWrapperKey{family: mixedCountGroupFamily, site: mixedPlanAdmission},
			run: func(input chplan.Node) (chplan.Node, error) {
				return preserveMixedPlan(input, mixedCountGroupFamily)
			},
		},
		{
			name: "scalar",
			key:  mixedWrapperKey{family: mixedScalarFamily, site: mixedPlanAdmission},
			run:  scalarFloatRows,
		},
		{
			name: "scalar_arithmetic",
			key:  mixedWrapperKey{family: mixedArithmeticFamily, site: mixedPlanAdmission},
			run: func(input chplan.Node) (chplan.Node, error) {
				return finishScalarArithmetic(input, arg, s, lowerCtx{}, chplan.OpAdd, 1, false, scalarArithmeticGuarded)
			},
		},
		{
			name: "scalar_comparison",
			key:  mixedWrapperKey{family: mixedComparisonFamily, site: mixedPlanAdmission},
			run: func(input chplan.Node) (chplan.Node, error) {
				return finishScalarComparison(input, arg, s, lowerCtx{}, chplan.OpGt, 1, false, false, scalarComparisonGuarded)
			},
		},
		{
			name: "date",
			key:  mixedWrapperKey{family: mixedDateFamily, site: mixedPlanAdmission},
			run: func(input chplan.Node) (chplan.Node, error) {
				return projectDateFnOverInner(year, input, s, lowerCtx{})
			},
		},
		{
			name: "timestamp",
			key:  mixedWrapperKey{family: mixedTimestampFamily, site: mixedPlanAdmission},
			run: func(input chplan.Node) (chplan.Node, error) {
				return projectDateFnOverInner(timestamp, input, s, lowerCtx{})
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policy, ok := mixedOperandPolicies[tc.key]
			if !ok {
				t.Fatal("fixture policy is missing")
			}
			delete(mixedOperandPolicies, tc.key)
			t.Cleanup(func() { mixedOperandPolicies[tc.key] = policy })

			if plan, err := tc.run(live); plan != nil || err == nil || !strings.Contains(err.Error(), "mixed operand is not admitted") {
				t.Fatalf("live mixed input bypassed policy: plan=%T error=%v", plan, err)
			}
			plan, err := tc.run(floatRows)
			if plan == nil || err != nil {
				t.Fatalf("proven float rows consulted mixed policy: plan=%T error=%v", plan, err)
			}
		})
	}
}

func TestTimestampConsumerPreservesLiveMixedRowsAndAcceptsFloatProof(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	live, floatRows := mixedConsumerTestRows(s)
	call := mustParse(t, "timestamp(up)").(*parser.Call)
	for _, tc := range []struct {
		name       string
		input      chplan.Node
		wantNarrow int
	}{
		{name: "live_mixed", input: live},
		{name: "float_proof", input: floatRows, wantNarrow: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := projectDateFnOverInner(call, tc.input, s, lowerCtx{})
			if err != nil {
				t.Fatal(err)
			}
			narrow := 0
			chplan.Walk(plan, func(node chplan.Node) bool {
				if chplan.IsMixedFloatNarrowing(node) {
					narrow++
				}
				return true
			})
			if narrow != tc.wantNarrow {
				t.Fatalf("float narrowing count = %d, want %d", narrow, tc.wantNarrow)
			}
		})
	}
}

func mixedConsumerTestRows(s schema.Metrics) (chplan.Node, chplan.Node) {
	columns := append(metricRoles(s), chplan.HistogramPayloadColumns()...)
	columns = append(columns, chplan.Column{Name: "source_kind", Role: chplan.RoleDiscriminator})
	live := sampleForwardTestInput(columns...)
	floatRows := &chplan.Filter{Input: live, Predicate: &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.ColumnRef{Name: "source_kind"},
		Right: &chplan.LitInt{V: mixedDiscriminatorFloat},
	}}
	return live, floatRows
}
