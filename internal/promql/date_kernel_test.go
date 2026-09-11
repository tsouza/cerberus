package promql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestDateKernelPayloadPreparation(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	for _, site := range []mixedAdmissionSite{mixedOperandAdmission, mixedPlanAdmission} {
		t.Run(string(site), func(t *testing.T) {
			key := mixedWrapperKey{family: mixedDateFamily, site: site}
			original := mixedOperandPolicies[key]
			t.Cleanup(func() { mixedOperandPolicies[key] = original })
			for _, policy := range []mixedOperandPolicy{mixedReject, mixedBespoke, mixedPreserve, mixedFloatOnly} {
				mixedOperandPolicies[key] = policy
				prepare, err := datePayloadPreparation(site)
				if policy != mixedFloatOnly {
					if err == nil || prepare != nil {
						t.Fatalf("unsupported policy %v: prepare nil=%v error=%v", policy, prepare == nil, err)
					}
					continue
				}
				if err != nil || prepare == nil {
					t.Fatalf("float policy: prepare nil=%v error=%v", prepare == nil, err)
				}
				mixed := &chplan.VectorSetOp{
					Mixed:            true,
					MetricNameColumn: s.MetricNameColumn,
					AttributesColumn: s.AttributesColumn,
					TimestampColumn:  s.TimestampColumn,
					ValueColumn:      s.ValueColumn,
				}
				filter, ok := prepare(mixed).(*chplan.Filter)
				if !ok || filter.Input != mixed || !chplan.IsMixedFloatNarrowing(filter) {
					t.Fatal("float preparation must narrow the original Mixed input")
				}
				ordinary := &chplan.OneRow{}
				if prepare(ordinary) != ordinary {
					t.Fatal("float preparation changed an ordinary input")
				}
			}
		})
	}
	if prepare, err := datePayloadPreparation(mixedRootAdmission); err == nil || prepare != nil {
		t.Fatal("date root admission must remain unsupported")
	}
}

func TestDateKernelPreservesOperandTopology(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	for _, tc := range []struct {
		operand string
		unless  int
		mixed   int
		narrow  int
	}{
		{`latency_exp_hist or num_cpus`, 1, 0, 0},
		{`num_cpus or latency_exp_hist`, 0, 0, 0},
		{`sort_by_label(latency_exp_hist or num_cpus, "job")`, 0, 1, 1},
		{`sort_by_label_desc(num_cpus or latency_exp_hist, "job")`, 0, 1, 1},
	} {
		for _, fn := range []string{"year", "month", "day_of_month", "day_of_week", "day_of_year", "days_in_month", "hour", "minute"} {
			t.Run(fn+"("+tc.operand+")", func(t *testing.T) {
				expr, err := p.ParseExpr(fn + "(" + tc.operand + ")")
				if err != nil {
					t.Fatal(err)
				}
				plan, err := LowerAt(context.Background(), expr, s, at, at)
				if err != nil {
					t.Fatal(err)
				}
				var unless, mixed, narrow int
				chplan.Walk(plan, func(n chplan.Node) bool {
					if set, ok := n.(*chplan.VectorSetOp); ok {
						if set.Op == chplan.VectorSetUnless {
							unless++
						}
						if set.Mixed {
							mixed++
						}
					}
					if filter, ok := n.(*chplan.Filter); ok && chplan.IsMixedFloatNarrowing(filter) {
						narrow++
					}
					return true
				})
				if unless != tc.unless || mixed != tc.mixed || narrow != tc.narrow {
					t.Fatalf("topology unless/mixed/narrow=%d/%d/%d want %d/%d/%d", unless, mixed, narrow, tc.unless, tc.mixed, tc.narrow)
				}
			})
		}
	}
}

func TestDateKernelAdmissionSitesRemainIndependent(t *testing.T) {
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		query  string
		family mixedWrapperFamily
		site   mixedAdmissionSite
	}{
		{`year(latency_exp_hist or num_cpus)`, mixedDateFamily, mixedOperandAdmission},
		{`year(sort_by_label(latency_exp_hist or num_cpus, "job"))`, mixedDateFamily, mixedPlanAdmission},
		{`timestamp(latency_exp_hist or num_cpus)`, mixedTimestampFamily, mixedOperandAdmission},
		{`timestamp(sort_by_label(latency_exp_hist or num_cpus, "job"))`, mixedTimestampFamily, mixedPlanAdmission},
	} {
		for _, removedFamily := range []mixedWrapperFamily{mixedDateFamily, mixedTimestampFamily} {
			for _, removedSite := range []mixedAdmissionSite{mixedOperandAdmission, mixedPlanAdmission} {
				t.Run(tc.query+"/delete/"+string(removedFamily)+"/"+string(removedSite), func(t *testing.T) {
					key := mixedWrapperKey{family: removedFamily, site: removedSite}
					policy := mixedOperandPolicies[key]
					delete(mixedOperandPolicies, key)
					t.Cleanup(func() { mixedOperandPolicies[key] = policy })
					expr, err := p.ParseExpr(tc.query)
					if err != nil {
						t.Fatal(err)
					}
					plan, err := LowerAt(context.Background(), expr, s, at, at)
					if tc.family == removedFamily && tc.site == removedSite {
						want := "mixed operand is not admitted for " + string(tc.family) + " at " + string(tc.site)
						if plan != nil || err == nil || !strings.Contains(err.Error(), want) {
							t.Fatalf("missing actual admission: plan=%T err=%v, want %q", plan, err, want)
						}
					} else if plan == nil || err != nil {
						t.Fatalf("unrelated admission affected dispatch: plan=%T err=%v", plan, err)
					}
				})
			}
		}
	}
}

func TestDateKernelErrorOrdering(t *testing.T) {
	p := parser.NewParser(parser.Options{})
	s := schema.DefaultOTelMetrics()
	ctx := lowerCtx{start: time.Unix(0, 0), end: time.Unix(0, 0)}
	makeCall := func(t *testing.T) *parser.Call {
		t.Helper()
		expr, err := p.ParseExpr(`year(latency_exp_hist or num_cpus)`)
		if err != nil {
			t.Fatal(err)
		}
		return expr.(*parser.Call)
	}
	assertError := func(t *testing.T, call *parser.Call, want string) {
		t.Helper()
		plan, err := lowerDateFn(call, s, ctx)
		if err == nil || !strings.Contains(err.Error(), want) || plan != nil {
			t.Fatalf("plan=%T error=%v, want %q", plan, err, want)
		}
	}
	t.Run("arity-before-loading", func(t *testing.T) {
		call := makeCall(t)
		call.Args = append(call.Args, &parser.StringLiteral{Val: "invalid"})
		assertError(t, call, "expects 0 or 1 argument")
	})
	t.Run("operand-before-unknown-name", func(t *testing.T) {
		call := &parser.Call{Func: &parser.Function{Name: "unknown_date"}, Args: parser.Expressions{
			&parser.Call{Func: parser.MustGetFunction("abs")},
		}}
		assertError(t, call, "abs with 0 arguments is unsupported")
	})
	for _, unknown := range []bool{false, true} {
		name := "known"
		if unknown {
			name = "unknown"
		}
		t.Run(name+"-bool-before-name-validation", func(t *testing.T) {
			call := makeCall(t)
			if unknown {
				call.Func = &parser.Function{Name: "unknown_date"}
			}
			call.Args[0].(*parser.BinaryExpr).ReturnBool = true
			assertError(t, call, "'bool' modifier is only allowed")
			key := mixedWrapperKey{family: mixedDateFamily, site: mixedOperandAdmission}
			policy := mixedOperandPolicies[key]
			delete(mixedOperandPolicies, key)
			t.Cleanup(func() { mixedOperandPolicies[key] = policy })
			assertError(t, call, "mixed operand is not admitted for date at operand")
		})
	}
}
