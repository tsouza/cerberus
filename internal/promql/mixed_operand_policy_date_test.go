package promql

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/schema"
)

// The generic date adapter receives an already-lowered Mixed operand here,
// unlike the direct set-operation recognizers. Its own family must authorize
// the projection; admitting the other date family is not sufficient.
func TestMixedOperandPolicyGenericDateFamily(t *testing.T) {
	const operand = `sort_by_label(latency_exp_hist or num_cpus, "job")`
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		family mixedWrapperFamily
		other  mixedWrapperFamily
	}{
		{"day_of_month", mixedDateFamily, mixedTimestampFamily},
		{"timestamp", mixedTimestampFamily, mixedDateFamily},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expr, err := p.ParseExpr(tc.name + "(" + operand + ")")
			if err != nil {
				t.Fatal(err)
			}
			for _, denied := range []mixedWrapperFamily{"", tc.other, tc.family} {
				t.Run("denied="+string(denied), func(t *testing.T) {
					if denied != "" {
						key := mixedWrapperKey{family: denied, site: mixedPlanAdmission}
						policy, exists := mixedOperandPolicies[key]
						if !exists {
							t.Fatalf("missing baseline family policy: %v", key)
						}
						delete(mixedOperandPolicies, key)
						t.Cleanup(func() { mixedOperandPolicies[key] = policy })
					}
					plan, err := LowerAt(context.Background(), expr, s, at, at)
					if denied == tc.family {
						want := fmt.Sprintf("mixed operand is not admitted for %s at %s", tc.family, mixedPlanAdmission)
						if err == nil || !strings.Contains(err.Error(), want) || plan != nil {
							t.Fatalf("own family denied: plan=%T error=%v, want %q before projection", plan, err, want)
						}
						return
					}
					if err != nil || plan == nil {
						t.Fatalf("authorized family must be independent of opposite family: plan=%T error=%v", plan, err)
					}
				})
			}
		})
	}
}

func TestMixedOperandPolicyGenericDateOrdinaryControls(t *testing.T) {
	for _, family := range []mixedWrapperFamily{mixedDateFamily, mixedTimestampFamily} {
		key := mixedWrapperKey{family: family, site: mixedPlanAdmission}
		policy, exists := mixedOperandPolicies[key]
		if !exists {
			t.Fatalf("missing baseline family policy: %v", key)
		}
		delete(mixedOperandPolicies, key)
		t.Cleanup(func() { mixedOperandPolicies[key] = policy })
	}
	p := parser.NewParser(parser.Options{})
	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, query := range []string{`day_of_month(sort(up))`, `timestamp(sort(up))`} {
		t.Run(query, func(t *testing.T) {
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := LowerAt(context.Background(), expr, s, at, at)
			if err != nil || plan == nil {
				t.Fatalf("ordinary date operand must not require Mixed admission: plan=%T error=%v", plan, err)
			}
		})
	}
}

func TestMixedOperandPolicyGenericDateRejectsUnknownFunction(t *testing.T) {
	p := parser.NewParser(parser.Options{})
	operand, err := p.ParseExpr(`sort(up)`)
	if err != nil {
		t.Fatal(err)
	}
	const unknown = "unregistered_date_function"
	call := &parser.Call{Func: &parser.Function{Name: unknown}, Args: parser.Expressions{operand}}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	plan, err := lowerDateFn(call, schema.DefaultOTelMetrics(), lowerCtx{start: at, end: at})
	want := "promql: unknown date function " + unknown
	if err == nil || err.Error() != want || plan != nil {
		t.Fatalf("unknown adapter function: plan=%T error=%v, want %q before projection", plan, err, want)
	}
}
