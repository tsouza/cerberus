package promql

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestCountValuesMixedSerializationPlan(t *testing.T) {
	standard := schema.DefaultOTelMetrics()
	custom := standard
	custom.MetricNameColumn, custom.AttributesColumn = "metric_id", "label_map"
	custom.TimestampColumn, custom.ValueColumn = "sample_time", "sample_value"
	for _, s := range []schema.Metrics{standard, custom} {
		for _, mixed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/mixed=%v", s.ValueColumn, mixed), func(t *testing.T) {
				operand := "num_cpus"
				if mixed {
					operand = `sort_by_label(latency_exp_hist or num_cpus, "series")`
				}
				plan := lowerCountValuesMixedTest(t, `count_values("v", `+operand+`)`, s)
				if chplan.RowShapeOf(plan) != chplan.SampleRowShape || plan.RowType().HasHistogramPayload() {
					t.Fatal("count_values must return ordinary float counts")
				}
				var aggregate *chplan.Aggregate
				var valueKey chplan.Expr
				chplan.Walk(plan, func(node chplan.Node) bool {
					if agg, ok := node.(*chplan.Aggregate); ok {
						for i, alias := range agg.GroupByAliases {
							if alias == "cv_val" {
								aggregate, valueKey = agg, agg.GroupBy[i]
							}
						}
					}
					return true
				})
				if aggregate == nil {
					t.Fatal("missing value-label grouping")
				}
				floatKey := promFixedFloatStringExpr(&chplan.ColumnRef{Name: s.ValueColumn})
				if !mixed {
					if !reflect.DeepEqual(valueKey, floatKey) {
						t.Fatal("ordinary float serialization changed")
					}
					return
				}
				if _, ok := aggregate.Input.(*chplan.OrderBy); !ok || !aggregate.Input.RowType().HasHistogramPayload() {
					t.Fatal("serialization must consume the complete shadow-resolved mixed operand")
				}
				conditional, ok := valueKey.(*chplan.FuncCall)
				const conditionalArity = 3
				if !ok || conditional.Fn != chplan.FnIf || len(conditional.Args) != conditionalArity {
					t.Fatalf("mixed key must choose a serializer, got %T", valueKey)
				}
				wantKind := &chplan.Binary{
					Op:    chplan.OpEq,
					Left:  &chplan.ColumnRef{Name: chplan.MixedDiscriminatorColumn},
					Right: &chplan.LitInt{V: mixedDiscriminatorFloat},
				}
				if !reflect.DeepEqual(conditional.Args[0], wantKind) ||
					!reflect.DeepEqual(conditional.Args[1], floatKey) ||
					!reflect.DeepEqual(conditional.Args[2], nativeHistogramStringExpr(s)) {
					t.Fatal("mixed value-label key does not use the sample-kind serializers")
				}
			})
		}
	}
}

func TestCountValuesMixedAdmissionRemainsRequired(t *testing.T) {
	key := mixedWrapperKey{family: mixedCountValuesFamily, site: mixedPlanAdmission}
	saved := mixedOperandPolicies[key]
	delete(mixedOperandPolicies, key)
	t.Cleanup(func() { mixedOperandPolicies[key] = saved })
	expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(`count_values("v", sort_by_label(latency_exp_hist or num_cpus, "series"))`)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(0, 0)
	plan, err := LowerAt(context.Background(), expr, schema.DefaultOTelMetrics(), at, at)
	if plan != nil || err == nil || !strings.Contains(err.Error(), "mixed operand is not admitted for count-values at existing-plan") {
		t.Fatalf("missing admission: plan=%T error=%v", plan, err)
	}
}

func TestCountValuesMixedSerializationResolvesDiscriminatorRole(t *testing.T) {
	const conditionalArity = 3
	s := schema.DefaultOTelMetrics()
	live, _ := mixedConsumerTestRows(s)
	floatKey := promFixedFloatStringExpr(&chplan.ColumnRef{Name: s.ValueColumn})
	conditional, ok := mixedCountValuesValueKey(live, floatKey, s).(*chplan.FuncCall)
	if !ok || len(conditional.Args) != conditionalArity {
		t.Fatalf("mixed value key = %#v", conditional)
	}
	predicate, ok := conditional.Args[0].(*chplan.Binary)
	if !ok {
		t.Fatalf("mixed discriminator predicate = %T", conditional.Args[0])
	}
	discriminator, ok := predicate.Left.(*chplan.ColumnRef)
	if !ok || discriminator.Name != "source_kind" {
		t.Fatalf("mixed discriminator = %#v, want source_kind role", predicate.Left)
	}
}

func lowerCountValuesMixedTest(t *testing.T, query string, s schema.Metrics) chplan.Node {
	t.Helper()
	expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(query)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(0, 0)
	plan, err := LowerAt(context.Background(), expr, s, at, at)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}
