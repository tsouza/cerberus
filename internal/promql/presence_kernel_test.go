package promql

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestPresenceKernelPayloadPolicy(t *testing.T) {
	rootKey := mixedWrapperKey{family: mixedCountGroupFamily, site: mixedRootAdmission}
	rootOriginal := mixedOperandPolicies[rootKey]
	t.Cleanup(func() { mixedOperandPolicies[rootKey] = rootOriginal })
	for _, policy := range []mixedOperandPolicy{mixedReject, mixedBespoke, mixedFloatOnly, mixedPreserve} {
		mixedOperandPolicies[rootKey] = policy
		wantPlan := &chplan.VectorSetOp{Mixed: true}
		wantErr := errors.New("operand failure")
		calls := 0
		got, err := lowerWithMixedPreservePolicy(rootKey.family, rootKey.site, func() (chplan.Node, error) {
			calls++
			return wantPlan, wantErr
		})
		if policy == mixedPreserve {
			if calls != 1 || got != wantPlan || err != wantErr {
				t.Fatalf("root preserve changed loader result: calls=%d plan=%T err=%v", calls, got, err)
			}
		} else if calls != 0 || got != nil || err == nil {
			t.Fatalf("root policy %v admitted: calls=%d plan=%T err=%v", policy, calls, got, err)
		}
	}
	delete(mixedOperandPolicies, rootKey)
	if plan, err := lowerWithMixedPreservePolicy(rootKey.family, rootKey.site, func() (chplan.Node, error) {
		t.Fatal("missing root policy loaded operand")
		return nil, nil
	}); plan != nil || err == nil {
		t.Fatal("missing root policy admitted")
	}

	planKey := mixedWrapperKey{family: mixedCountGroupFamily, site: mixedPlanAdmission}
	planOriginal := mixedOperandPolicies[planKey]
	t.Cleanup(func() { mixedOperandPolicies[planKey] = planOriginal })
	for _, policy := range []mixedOperandPolicy{mixedReject, mixedBespoke, mixedFloatOnly, mixedPreserve} {
		mixedOperandPolicies[planKey] = policy
		input := &chplan.VectorSetOp{Mixed: true}
		got, err := preserveMixedPlan(input, mixedCountGroupFamily)
		if policy == mixedPreserve {
			if got != input || err != nil {
				t.Fatalf("plan preserve changed input: plan=%T err=%v", got, err)
			}
		} else if got != nil || err == nil {
			t.Fatalf("plan policy %v admitted: plan=%T err=%v", policy, got, err)
		}
	}
	delete(mixedOperandPolicies, planKey)
	if plan, err := preserveMixedPlan(&chplan.VectorSetOp{Mixed: true}, mixedCountGroupFamily); plan != nil || err == nil {
		t.Fatal("missing plan policy admitted")
	}
	for _, input := range []chplan.Node{&chplan.Scan{}, &chplan.HistogramProjection{Input: &chplan.OneRow{}}} {
		got, err := preserveMixedPlan(input, mixedCountGroupFamily)
		if got != input || err != nil {
			t.Fatalf("ordinary input %T consulted mixed policy: plan=%T err=%v", input, got, err)
		}
	}
}

func TestPresenceKernelLayouts(t *testing.T) {
	const timestampProjectionIndex = 2
	for _, custom := range []bool{false, true} {
		s := schema.DefaultOTelMetrics()
		if custom {
			s.MetricNameColumn = "metric_id"
			s.AttributesColumn = "labels_map"
			s.TimestampColumn = "sample_time"
			s.ValueColumn = "sample_value"
		}
		for _, step := range []time.Duration{0, time.Minute} {
			for _, op := range []parser.ItemType{parser.COUNT, parser.GROUP} {
				for _, layout := range []plainAggregateLayout{ordinaryPlainAggregateLayout, mixedPlainAggregateLayout} {
					t.Run(fmt.Sprintf("%v/%v/%s/%v", custom, step, op, layout), func(t *testing.T) {
						input := &chplan.VectorSetOp{Mixed: true}
						expr := &parser.AggregateExpr{Op: op, Grouping: []string{"job", "instance"}}
						plan, err := lowerPlainAggregateOverInput(expr, input, s, lowerCtx{step: step}, layout)
						if err != nil {
							t.Fatal(err)
						}
						project, ok := plan.(*chplan.Project)
						if !ok {
							t.Fatalf("plan %T", plan)
						}
						agg, ok := project.Input.(*chplan.Aggregate)
						if !ok || agg.Input != input || !agg.DropEmptyOnNoGroup {
							t.Fatalf("reduction lost untouched mixed input: %#v", project.Input)
						}
						wantAliases := groupKeyAliases(len(expr.Grouping))
						if step > 0 {
							index, alias := len(expr.Grouping), rangeBucketAlias
							if layout == mixedPlainAggregateLayout {
								index, alias = 0, mixedAggregateBucketAlias
								wantAliases = append([]string{alias}, wantAliases...)
							} else {
								wantAliases = append(wantAliases, alias)
							}
							column, ok := agg.GroupBy[index].(*chplan.ColumnRef)
							if !ok || column.Name != s.TimestampColumn {
								t.Fatalf("bucket position %d: %#v", index, agg.GroupBy)
							}
							ts, ok := project.Projections[timestampProjectionIndex].Expr.(*chplan.ColumnRef)
							if !ok || ts.Name != alias {
								t.Fatalf("timestamp projection %#v", project.Projections[timestampProjectionIndex])
							}
						}
						if strings.Join(agg.GroupByAliases, ",") != strings.Join(wantAliases, ",") {
							t.Fatalf("aliases %v want %v", agg.GroupByAliases, wantAliases)
						}
						if len(agg.GroupBy) != len(wantAliases) {
							t.Fatalf("group keys %d want %d", len(agg.GroupBy), len(wantAliases))
						}
					})
				}
			}
		}
	}
}

func TestPresenceKernelCanonicalizationAndNativeGrid(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	a := &parser.AggregateExpr{Op: parser.COUNT}
	t.Run("quantile-cannot-bypass-domain-guard", func(t *testing.T) {
		quantile := &parser.AggregateExpr{Op: parser.QUANTILE, Param: &parser.NumberLiteral{Val: -1}}
		plan, err := lowerPlainAggregateOverInput(quantile, &chplan.RangeWindowGridNative{}, s, lowerCtx{step: time.Minute, lowerers: RangeLowerers{VectorAgg: true}}, ordinaryPlainAggregateLayout)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := plan.(*chplan.Project).Input.(*chplan.Aggregate); !ok {
			t.Fatal("quantile acquired native-grid early return")
		}
	})
	for _, layout := range []plainAggregateLayout{ordinaryPlainAggregateLayout, mixedPlainAggregateLayout} {
		t.Run(fmt.Sprint(layout), func(t *testing.T) {
			reduced := &chplan.RangeWindow{}
			plan, err := lowerPlainAggregateOverInput(a, reduced, s, lowerCtx{}, layout)
			if err != nil {
				t.Fatal(err)
			}
			input := plan.(*chplan.Project).Input.(*chplan.Aggregate).Input
			if layout == ordinaryPlainAggregateLayout {
				if input != reduced {
					t.Fatal("ordinary input canonicalized")
				}
			} else {
				canonical, ok := input.(*chplan.Project)
				if !ok || canonical.Input != reduced {
					t.Fatal("mixed reduced input lost canonicalization")
				}
			}
			for _, enabled := range []bool{false, true} {
				grid := &chplan.RangeWindowGridNative{}
				plan, err := lowerPlainAggregateOverInput(a, grid, s, lowerCtx{step: time.Minute, lowerers: RangeLowerers{VectorAgg: enabled}}, layout)
				if err != nil {
					t.Fatal(err)
				}
				_, native := plan.(*chplan.Project).Input.(*chplan.RangeWindowGridNativeVectorAgg)
				if native != (enabled && layout == ordinaryPlainAggregateLayout) {
					t.Fatalf("native=%v enabled=%v layout=%v", native, enabled, layout)
				}
			}
		})
	}
}

func TestPresenceKernelAdmissionAndErrorOrder(t *testing.T) {
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, fn := range []string{"count", "group"} {
		for _, nested := range []bool{false, true} {
			for _, removed := range []mixedAdmissionSite{mixedRootAdmission, mixedPlanAdmission} {
				t.Run(fmt.Sprintf("%s/%v/delete/%s", fn, nested, removed), func(t *testing.T) {
					key := mixedWrapperKey{family: mixedCountGroupFamily, site: removed}
					policy := mixedOperandPolicies[key]
					delete(mixedOperandPolicies, key)
					t.Cleanup(func() { mixedOperandPolicies[key] = policy })
					operand := "latency_exp_hist or num_cpus"
					if nested {
						operand = `sort_by_label(` + operand + `,"job")`
					}
					expr, err := p.ParseExpr(fn + "(" + operand + ")")
					if err != nil {
						t.Fatal(err)
					}
					plan, err := LowerAt(context.Background(), expr, s, at, at)
					denied := (nested && removed == mixedPlanAdmission) || (!nested && removed == mixedRootAdmission)
					if denied {
						if plan != nil || err == nil || !strings.Contains(err.Error(), "mixed operand is not admitted") {
							t.Fatalf("missing admission: %T %v", plan, err)
						}
					} else if plan == nil || err != nil {
						t.Fatalf("unrelated site affected route: %T %v", plan, err)
					}
				})
			}
		}
	}
	t.Run("root-policy-before-bool", func(t *testing.T) {
		key := mixedWrapperKey{family: mixedCountGroupFamily, site: mixedRootAdmission}
		policy := mixedOperandPolicies[key]
		delete(mixedOperandPolicies, key)
		t.Cleanup(func() { mixedOperandPolicies[key] = policy })
		if plan, err := lowerCountOrGroupOverMixedExpHistogramSetOp(nil, &parser.BinaryExpr{ReturnBool: true}, s, lowerCtx{}); plan != nil || err == nil || !strings.Contains(err.Error(), "mixed operand is not admitted") {
			t.Fatalf("%T %v", plan, err)
		}
		if plan, err := lowerCountOrGroupOverMixedExpHistogramSetOp(nil, nil, s, lowerCtx{}); plan != nil || err == nil || !strings.Contains(err.Error(), "mixed operand is not admitted") {
			t.Fatalf("denied policy loaded nil operand: %T %v", plan, err)
		}
	})
	t.Run("bool-before-operands", func(t *testing.T) {
		if plan, err := lowerCountOrGroupOverMixedExpHistogramSetOp(nil, &parser.BinaryExpr{ReturnBool: true}, s, lowerCtx{}); plan != nil || err == nil || !strings.Contains(err.Error(), "'bool' modifier") {
			t.Fatalf("%T %v", plan, err)
		}
	})
	t.Run("operand-before-plan-admission", func(t *testing.T) {
		key := mixedWrapperKey{family: mixedCountGroupFamily, site: mixedPlanAdmission}
		policy := mixedOperandPolicies[key]
		delete(mixedOperandPolicies, key)
		t.Cleanup(func() { mixedOperandPolicies[key] = policy })
		a := &parser.AggregateExpr{Op: parser.COUNT, Expr: &parser.Call{Func: parser.MustGetFunction("abs")}}
		if plan, err := lowerAggregate(a, s, lowerCtx{}); plan != nil || err == nil || !strings.Contains(err.Error(), "abs with 0 arguments") {
			t.Fatalf("%T %v", plan, err)
		}
	})
	t.Run("plan-admission-before-parameter", func(t *testing.T) {
		key := mixedWrapperKey{family: mixedCountGroupFamily, site: mixedPlanAdmission}
		policy := mixedOperandPolicies[key]
		delete(mixedOperandPolicies, key)
		t.Cleanup(func() { mixedOperandPolicies[key] = policy })
		expr, err := p.ParseExpr(`count(sort_by_label(latency_exp_hist or num_cpus,"job"))`)
		if err != nil {
			t.Fatal(err)
		}
		a := expr.(*parser.AggregateExpr)
		a.Param = &parser.NumberLiteral{Val: 1}
		ctx := lowerCtx{start: at, end: at}
		if plan, err := lowerAggregate(a, s, ctx); plan != nil || err == nil || !strings.Contains(err.Error(), "mixed operand is not admitted") {
			t.Fatalf("%T %v", plan, err)
		}
		mixedOperandPolicies[key] = policy
		if plan, err := lowerAggregate(a, s, ctx); plan != nil || err == nil || !strings.Contains(err.Error(), "does not take a parameter") {
			t.Fatalf("%T %v", plan, err)
		}
	})
	t.Run("ordinary-and-histogram-only-bypass", func(t *testing.T) {
		for _, site := range []mixedAdmissionSite{mixedRootAdmission, mixedPlanAdmission} {
			key := mixedWrapperKey{family: mixedCountGroupFamily, site: site}
			policy := mixedOperandPolicies[key]
			delete(mixedOperandPolicies, key)
			t.Cleanup(func() { mixedOperandPolicies[key] = policy })
		}
		for _, query := range []string{"count(up)", "group(up)", "count(latency_exp_hist)", "group(latency_exp_hist)"} {
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatal(err)
			}
			if plan, err := LowerAt(context.Background(), expr, s, at, at); plan == nil || err != nil {
				t.Fatalf("%s: %T %v", query, plan, err)
			}
		}
	})
}
