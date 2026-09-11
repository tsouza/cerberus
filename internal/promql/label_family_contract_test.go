package promql

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

func labelContractParse(t *testing.T, query string) parser.Expr {
	t.Helper()
	expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(query)
	if err != nil {
		t.Fatal(err)
	}
	return expr
}

func labelContractLower(t *testing.T, query string, s schema.Metrics, step time.Duration) (chplan.Node, error) {
	t.Helper()
	end := time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)
	return LowerAtRange(context.Background(), labelContractParse(t, query), s, end.Add(-time.Minute), end, step)
}

func TestLabelFamilyValidationBeforeOperand(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	const operand = `count_values("", up)`
	const badReplacement = `"service", "$dup", "host", "(?:(?P<dup>a?)|y)*(?P<dup>b)"`
	_, operandErr := labelContractLower(t, operand, s, 0)
	_, argumentErr := labelContractLower(t, `label_replace(up, `+badReplacement+`)`, s, 0)
	if operandErr == nil || argumentErr == nil || operandErr.Error() == argumentErr.Error() {
		t.Fatalf("independent sentinels must fail differently: operand=%v arguments=%v", operandErr, argumentErr)
	}
	for _, vector := range []string{operand, "latency_exp_hist or " + operand, operand + " or latency_exp_hist"} {
		t.Run(vector, func(t *testing.T) {
			_, err := labelContractLower(t, `label_replace(`+vector+`, `+badReplacement+`)`, s, 0)
			if err == nil || err.Error() != argumentErr.Error() {
				t.Fatalf("first error=%v, want label argument error %v", err, argumentErr)
			}
		})
	}
}

func TestLabelFamilyKernelValidationAndLoader(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	for _, query := range []string{`label_replace(up, "dst", "x", "job", ".*")`, `label_join(up, "dst", "-", "job")`} {
		call := labelContractParse(t, query).(*parser.Call)
		for _, malformed := range []string{"arity", "string"} {
			t.Run(call.Func.Name+"/"+malformed, func(t *testing.T) {
				bad := *call
				bad.Args = append(parser.Expressions(nil), call.Args...)
				if malformed == "arity" {
					bad.Args = bad.Args[:1]
				} else {
					bad.Args[1] = &parser.NumberLiteral{Val: 1}
				}
				var expected error
				if call.Func.Name == fnLabelReplace {
					_, expected = labelReplaceAttributesBuilder(&bad)
				} else {
					_, expected = labelJoinAttributesBuilder(&bad)
				}
				if expected == nil {
					t.Fatal("malformed builder sentinel did not independently fail")
				}
				calls := 0
				plan, err := lowerLabelCall(&bad, s, func() (chplan.Node, error) {
					calls++
					return nil, errors.New("operand must not run")
				})
				if plan != nil || err == nil || err.Error() != expected.Error() || calls != 0 {
					t.Fatalf("validation must precede loader: plan=%v error=%v calls=%d", plan, err, calls)
				}
			})
		}
		sentinel := errors.New("valid label operand sentinel")
		calls := 0
		plan, err := lowerLabelCall(call, s, func() (chplan.Node, error) { calls++; return nil, sentinel })
		if plan != nil || err != sentinel || calls != 1 {
			t.Fatalf("valid arguments must invoke loader once: plan=%v error=%v calls=%d", plan, err, calls)
		}
	}
}

// Reconstruct the established projection+guard boundary over an independently
// lowered operand. Comparing the complete tree catches extra filters, repeated
// collision guards, payload loss, name changes and temporal-envelope drift.
func TestLabelFamilyPreservesTopology(t *testing.T) {
	const rangeStep = 15 * time.Second
	for _, custom := range []bool{false, true} {
		s := schema.DefaultOTelMetrics()
		schemaName := "default"
		if custom {
			schemaName = "custom"
			s.MetricNameColumn, s.AttributesColumn = "metric_id", "labels_map"
			s.TimestampColumn, s.ValueColumn = "sample_time", "sample_value"
		}
		for _, step := range []time.Duration{0, rangeStep} {
			for _, operand := range []string{
				"up", "latency_exp_hist or num_cpus", "num_cpus or latency_exp_hist",
				`sort_by_label(latency_exp_hist or num_cpus, "job")`,
				`sort_by_label_desc(num_cpus or latency_exp_hist, "job")`,
			} {
				for _, fn := range []string{fnLabelReplace, fnLabelJoin} {
					t.Run(schemaName+"/"+step.String()+"/"+fn+"/"+operand, func(t *testing.T) {
						args := `, "dst", "x", "job", ".*")`
						if fn == fnLabelJoin {
							args = `, "dst", "-", "job")`
						}
						query := fn + "(" + operand + args
						inner, err := labelContractLower(t, operand, s, step)
						if err != nil {
							t.Fatal(err)
						}
						call := labelContractParse(t, query).(*parser.Call)
						var attrs func(chplan.Expr) chplan.Expr
						if fn == fnLabelReplace {
							attrs, err = labelReplaceAttributesBuilder(call)
						} else {
							attrs, err = labelJoinAttributesBuilder(call)
						}
						if err != nil {
							t.Fatal(err)
						}
						project, err := projectAttributesOverInner(inner, s, mixedLabelFamily, func(refs sampleRoleRefs) chplan.Expr { return attrs(refs.Attributes) })
						if err != nil {
							t.Fatal(err)
						}
						want := guardLabelRewriteCollision(project, s)
						got, err := labelContractLower(t, query, s, step)
						if err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(got, want) {
							t.Fatal("label rewrite changed the established operand/projection/collision topology")
						}
						if strings.Contains(operand, "latency_exp_hist") && chplan.RowShapeOf(got) != chplan.MixedRowShape {
							t.Fatal("label rewrite lost mixed shape")
						}
					})
				}
			}
		}
	}
}

// Mutates only this family's table rows; unrelated bespoke modes stay closed.
func TestLabelFamilyPolicyDrivesPayload(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	const unknownPolicy mixedOperandPolicy = 255
	for _, site := range []mixedAdmissionSite{mixedRootAdmission, mixedPlanAdmission} {
		t.Run(string(site), func(t *testing.T) {
			key := mixedWrapperKey{family: mixedLabelFamily, site: site}
			original := mixedOperandPolicies[key]
			t.Cleanup(func() { mixedOperandPolicies[key] = original })
			for _, mode := range []mixedOperandPolicy{mixedReject, mixedBespoke, mixedFloatOnly, unknownPolicy} {
				mixedOperandPolicies[key] = mode
				if _, err := labelPayloadPolicy(site); err == nil {
					t.Fatalf("unsupported mode %v admitted", mode)
				}
				if site == mixedPlanAdmission {
					called := false
					mixed := &chplan.VectorSetOp{
						Mixed:            true,
						MetricNameColumn: s.MetricNameColumn,
						AttributesColumn: s.AttributesColumn,
						TimestampColumn:  s.TimestampColumn,
						ValueColumn:      s.ValueColumn,
					}
					plan, err := projectAttributesOverInner(mixed, s, mixedLabelFamily, func(refs sampleRoleRefs) chplan.Expr { called = true; return refs.Attributes })
					if plan != nil || err == nil || called {
						t.Fatalf("policy reached payload callback: plan=%v err=%v called=%v", plan, err, called)
					}
				} else {
					// Nil union proves denial occurs before operand lowering. The invalid
					// call also proves root policy retains priority over argument validation.
					call := &parser.Call{Func: parser.Functions[fnLabelReplace]}
					plan, err := lowerLabelCallOverMixedExpHistogramSetOp(call, nil, s, lowerCtx{})
					if plan != nil || err == nil || !strings.Contains(err.Error(), "mixed operand is not admitted") {
						t.Fatalf("root policy lost priority: plan=%v err=%v", plan, err)
					}
				}
			}
			delete(mixedOperandPolicies, key)
			if _, err := labelPayloadPolicy(site); err == nil {
				t.Fatal("missing policy admitted")
			}
			mixedOperandPolicies[key] = mixedPreserve
			payload, err := labelPayloadPolicy(site)
			if err != nil || payload != preserveMixedSamplePayload {
				t.Fatalf("preserve mode did not select live payload forwarding: %v %v", payload, err)
			}
		})
	}
}
