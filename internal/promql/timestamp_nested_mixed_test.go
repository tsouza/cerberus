package promql

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/spec"
)

func TestTimestampMixedPlanNormalizesRolesBeforeGuard(t *testing.T) {
	standard := schema.DefaultOTelMetrics()
	custom := standard
	custom.MetricNameColumn, custom.AttributesColumn, custom.TimestampColumn, custom.ValueColumn = "metric_id", "label_map", "sample_time", "sample_value"
	for _, s := range []schema.Metrics{standard, custom} {
		for _, step := range []time.Duration{0, time.Second} {
			t.Run(fmt.Sprintf("%s/%s", s.ValueColumn, step), func(t *testing.T) {
				columns := []chplan.Column{
					{Name: "input_name", Role: chplan.RoleMetricName},
					{Name: "input_labels", Role: chplan.RoleAttributes},
					{Name: "input_time", Role: chplan.RoleTimestamp},
					{Name: "unread_placeholder", Role: chplan.RoleValue},
					{Name: "input_kind", Role: chplan.RoleDiscriminator},
				}
				inner := sampleForwardTestInput(append(columns, chplan.HistogramPayloadColumns()...)...)
				arg, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(`sort_by_label(latency_exp_hist or num_cpus, "job")`)
				if err != nil {
					t.Fatal(err)
				}
				at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
				ctx := lowerCtx{start: at, end: at, step: step}
				plan, err := lowerTimestampOverMixedPlan(inner, arg, s, ctx)
				if err != nil {
					t.Fatal(err)
				}
				output, ok := plan.(*chplan.Project)
				if !ok {
					t.Fatalf("output = %T", plan)
				}
				if chplan.RowShapeOf(output) != chplan.SampleRowShape {
					t.Fatal("timestamp output must be float samples")
				}
				name, ok := output.Projections[0].Expr.(*chplan.LitString)
				if !ok || name.V != "" {
					t.Fatal("output retained metric name")
				}
				guard, ok := output.Input.(*chplan.Aggregate)
				if !ok || guard.Having == nil {
					t.Fatalf("name drop missing collision guard: %T", output.Input)
				}
				named, ok := guard.Input.(*chplan.Project)
				if !ok || named.Input != inner {
					t.Fatal("conversion must retain exact original mixed operand")
				}
				if chplan.RowShapeOf(named) != chplan.SampleRowShape || len(named.Projections) != len(metricRoles(s)) {
					t.Fatal("conversion did not replace mixed payload with canonical floats")
				}
				var ts chplan.Expr = anchorBaseExpr(evalAnchor{End: at})
				if step > 0 {
					ts = &chplan.ColumnRef{Name: "input_time"}
				}
				want := &chplan.Project{Input: inner, Roles: metricRoles(s), Projections: []chplan.Projection{
					{Expr: &chplan.ColumnRef{Name: "input_name"}, Alias: s.MetricNameColumn},
					{Expr: &chplan.ColumnRef{Name: "input_labels"}, Alias: s.AttributesColumn},
					{Expr: ts, Alias: s.TimestampColumn},
					{Expr: asFloat64(dateFnExpr(timestampFunctionName, nil, ts)), Alias: s.ValueColumn},
				}}
				if !named.Equal(want) {
					t.Fatal("conversion changed role-derived names or evaluated a payload")
				}
			})
		}
	}
}

func TestTimestampMixedPlanAuthorizationPrecedesRoleResolution(t *testing.T) {
	key := mixedWrapperKey{family: mixedTimestampFamily, site: mixedPlanAdmission}
	old := mixedOperandPolicies[key]
	for _, mode := range []mixedOperandPolicy{mixedReject, mixedFloatOnly, mixedPreserve} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			mixedOperandPolicies[key] = mode
			t.Cleanup(func() { mixedOperandPolicies[key] = old })
			plan, err := lowerTimestampOverMixedPlan(&chplan.VectorSetOp{Mixed: true}, nil, schema.DefaultOTelMetrics(), lowerCtx{})
			if plan != nil || err == nil {
				t.Fatalf("denied malformed input reached conversion: %T %v", plan, err)
			}
		})
	}
}

func TestTimestampNestedMixedRetainsOriginalOperand(t *testing.T) {
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	standard := schema.DefaultOTelMetrics()
	custom := standard
	custom.MetricNameColumn, custom.AttributesColumn, custom.TimestampColumn, custom.ValueColumn = "metric_id", "label_map", "sample_time", "sample_value"
	at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	for _, s := range []schema.Metrics{standard, custom} {
		for _, step := range []time.Duration{0, time.Second} {
			for _, sort := range []string{"sort_by_label", "sort_by_label_desc"} {
				for _, union := range []string{"latency_exp_hist or num_cpus", "num_cpus or latency_exp_hist"} {
					t.Run(fmt.Sprintf("%s/%s/%s/%s", s.ValueColumn, step, sort, union), func(t *testing.T) {
						operand := sort + "(" + union + `, "job")`
						lowerQuery := func(query string) chplan.Node {
							expr, err := p.ParseExpr(query)
							if err != nil {
								t.Fatal(err)
							}
							plan, err := LowerAtRange(context.Background(), expr, s, at, at.Add(step), step)
							if err != nil {
								t.Fatal(err)
							}
							return plan
						}
						original := lowerQuery(operand)
						plan := lowerQuery("timestamp(" + operand + ")")
						found := false
						chplan.Walk(plan, func(n chplan.Node) bool {
							if f, ok := n.(*chplan.Filter); ok && chplan.IsMixedFloatNarrowing(f) {
								t.Fatal("timestamp discarded histogram samples")
							}
							if n.Equal(original) {
								found = true
							}
							return true
						})
						if !found {
							t.Fatal("timestamp replaced its original operand")
						}
						optimized := spec.AssertScanTimeBoundAccepts(t, plan)
						if _, _, err := chsql.Emit(context.Background(), optimized); err != nil {
							t.Fatal(err)
						}
					})
				}
			}
		}
	}
}
