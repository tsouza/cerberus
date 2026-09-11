package promql

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

const mixedSelectorNestedInput = `sort_by_label(latency_exp_hist or num_cpus, "series")`

func TestMixedSelectorPlanPayload(t *testing.T) {
	for _, op := range []string{"topk", "bottomk", "limitk", "limit_ratio"} {
		for _, k := range []string{"1", "scalar(vector(1))"} {
			t.Run(op+"/"+k, func(t *testing.T) {
				plan, err := lowerMixedSelectorTestQuery(t, op+"("+k+", "+mixedSelectorNestedInput+")")
				if err != nil {
					t.Fatal(err)
				}
				preserves := op == "limitk" || op == "limit_ratio"
				if got := chplan.RowShapeOf(plan); (got == chplan.MixedRowShape) != preserves {
					t.Fatalf("output shape=%s, preserve=%v", got, preserves)
				}
				if got := plan.RowType(); got.HasHistogramPayload() != preserves || got.Has(chplan.RoleDiscriminator) != preserves {
					t.Fatalf("physical output=%#v, preserve=%v", got, preserves)
				}
				if preserves {
					for _, child := range plan.Children() {
						if chplan.IsMixedFloatNarrowing(child) {
							t.Fatal("preserving selector narrowed its operand")
						}
					}
					return
				}
				top, ok := plan.(*chplan.TopK)
				if !ok || !chplan.IsMixedFloatNarrowing(top.Input) {
					t.Fatalf("ranking must consume explicit float narrowing: %#v", plan)
				}
				filter := top.Input.(*chplan.Filter)
				if _, ok := filter.Input.(*chplan.OrderBy); !ok || !filter.Input.RowType().Has(chplan.RoleDiscriminator) {
					t.Fatal("narrowing must follow the already-lowered mixed sort")
				}
			})
		}
	}
}

// No parallel execution: each subtest restores its temporarily deleted row.
func TestMixedSelectorIndependentPolicySites(t *testing.T) {
	for _, op := range []string{"topk", "bottomk", "limitk", "limit_ratio"} {
		for _, computed := range []bool{false, true} {
			for _, nested := range []bool{false, true} {
				family := mixedTopKFamily
				directSite := mixedRootAdmission
				if op == "limitk" || op == "limit_ratio" {
					family, directSite = mixedLimitFamily, mixedOperandAdmission
				}
				for _, site := range []mixedAdmissionSite{directSite, mixedPlanAdmission} {
					t.Run(fmt.Sprintf("%s/computed=%v/nested=%v/%s", op, computed, nested, site), func(t *testing.T) {
						operand := "latency_exp_hist or num_cpus"
						if nested {
							operand = mixedSelectorNestedInput
						}
						k := "1"
						if computed {
							k = "scalar(vector(1))"
						}
						query := op + "(" + k + ", " + operand + ")"
						if _, err := lowerMixedSelectorTestQuery(t, query); err != nil {
							t.Fatalf("registered baseline: %v", err)
						}
						key := mixedWrapperKey{family: family, site: site}
						policy, exists := mixedOperandPolicies[key]
						if !exists {
							t.Fatal("missing baseline policy", key)
						}
						delete(mixedOperandPolicies, key)
						t.Cleanup(func() { mixedOperandPolicies[key] = policy })
						_, err := lowerMixedSelectorTestQuery(t, query)
						wantError := nested && site == mixedPlanAdmission || !nested && site == directSite
						// Existing computed limitk rechecks the resulting Mixed plan.
						wantError = wantError || op == "limitk" && computed && site == mixedPlanAdmission
						if wantError {
							if err == nil || !strings.Contains(err.Error(), "mixed operand is not admitted for "+string(family)+" at "+string(site)) {
								t.Fatalf("required site did not reject: %v", err)
							}
						} else if err != nil {
							t.Fatalf("unrelated policy site changed dispatch: %v", err)
						}
					})
				}
			}
		}
	}
}

func TestMixedSelectorParameterOrder(t *testing.T) {
	for _, op := range []string{"topk", "bottomk", "limitk"} {
		for _, value := range []float64{math.NaN(), math.Inf(1), math.MaxFloat64} {
			t.Run(fmt.Sprintf("%s/%v", op, value), func(t *testing.T) {
				agg := mixedSelectorTestAggregate(t, op+"(1, "+mixedSelectorNestedInput+")")
				agg.Param = &parser.NumberLiteral{Val: value}
				_, _, expected := topKDomain(value)
				if expected == nil {
					t.Fatal("test requires invalid literal K")
				}
				family := mixedAggregateFamily(agg.Op)
				key := mixedWrapperKey{family: family, site: mixedPlanAdmission}
				policy := mixedOperandPolicies[key]
				delete(mixedOperandPolicies, key)
				t.Cleanup(func() { mixedOperandPolicies[key] = policy })
				at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
				_, err := LowerAt(context.Background(), agg, schema.DefaultOTelMetrics(), at, at)
				if err == nil || err.Error() != expected.Error() {
					t.Fatalf("literal K must precede operand/policy: got %v, want %v", err, expected)
				}
			})
		}
	}
	for _, op := range []string{"topk", "bottomk", "limitk"} {
		t.Run(op+"/computed_policy_before_parameter", func(t *testing.T) {
			agg := mixedSelectorTestAggregate(t, op+"(scalar(vector(1)), "+mixedSelectorNestedInput+")")
			// This malformed computed parameter must not be visited before admission.
			agg.Param = &parser.StringLiteral{Val: "not a scalar"}
			key := mixedWrapperKey{family: mixedAggregateFamily(agg.Op), site: mixedPlanAdmission}
			policy := mixedOperandPolicies[key]
			delete(mixedOperandPolicies, key)
			t.Cleanup(func() { mixedOperandPolicies[key] = policy })
			at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
			_, err := LowerAt(context.Background(), agg, schema.DefaultOTelMetrics(), at, at)
			if err == nil || !strings.Contains(err.Error(), "mixed operand is not admitted") {
				t.Fatalf("computed admission must precede parameter building: %v", err)
			}
		})
		t.Run(op+"/direct_bool_vs_literal", func(t *testing.T) {
			agg := mixedSelectorTestAggregate(t, op+"(1, latency_exp_hist or num_cpus)")
			agg.Expr.(*parser.BinaryExpr).ReturnBool = true
			agg.Param = &parser.NumberLiteral{Val: math.NaN()}
			at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
			_, err := LowerAt(context.Background(), agg, schema.DefaultOTelMetrics(), at, at)
			if op == "limitk" {
				_, _, expected := topKDomain(math.NaN())
				if err == nil || err.Error() != expected.Error() {
					t.Fatalf("limitk literal validation must precede operand loading: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "'bool' modifier") {
				t.Fatalf("direct ranked adapter must retain bool-before-K ordering: %v", err)
			}
		})
		t.Run(op+"/computed_operand_before_parameter", func(t *testing.T) {
			agg := mixedSelectorTestAggregate(t, op+"(scalar(vector(1)), "+mixedSelectorNestedInput+")")
			agg.Expr.(*parser.Call).Args[0].(*parser.BinaryExpr).ReturnBool = true
			agg.Param = &parser.StringLiteral{Val: "not a scalar"}
			at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
			_, err := LowerAt(context.Background(), agg, schema.DefaultOTelMetrics(), at, at)
			if err == nil || !strings.Contains(err.Error(), "'bool' modifier") {
				t.Fatalf("computed selector must load operand before building K: %v", err)
			}
		})
	}
}

func mixedSelectorTestAggregate(t *testing.T, query string) *parser.AggregateExpr {
	t.Helper()
	expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(query)
	if err != nil {
		t.Fatal(err)
	}
	agg, ok := expr.(*parser.AggregateExpr)
	if !ok {
		t.Fatalf("expected aggregate, got %T", expr)
	}
	return agg
}

func lowerMixedSelectorTestQuery(t *testing.T, query string) (chplan.Node, error) {
	t.Helper()
	agg := mixedSelectorTestAggregate(t, query)
	at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	return LowerAt(context.Background(), agg, schema.DefaultOTelMetrics(), at, at)
}
