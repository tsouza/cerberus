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

func TestSortByLabelPreservePolicy(t *testing.T) {
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, site := range []mixedAdmissionSite{mixedOperandAdmission, mixedPlanAdmission} {
		t.Run(string(site), func(t *testing.T) {
			key := mixedWrapperKey{family: mixedSortByLabelFamily, site: site}
			original := mixedOperandPolicies[key]
			t.Cleanup(func() { mixedOperandPolicies[key] = original })
			query := "sort_by_label(latency_exp_hist or num_cpus)"
			if site == mixedPlanAdmission {
				query = "sort_by_label(" + query + ")"
			}
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatal(err)
			}
			for _, policy := range []mixedOperandPolicy{mixedReject, mixedBespoke, mixedFloatOnly, mixedPreserve} {
				mixedOperandPolicies[key] = policy
				calls := 0
				wanted := &chplan.VectorSetOp{Mixed: true}
				sentinel := errors.New("operand failure")
				got, gotErr := lowerWithMixedPreservePolicy(key.family, key.site, func() (chplan.Node, error) { calls++; return wanted, sentinel })
				if policy == mixedPreserve {
					if calls != 1 || got != wanted || gotErr != sentinel {
						t.Fatal("preserve changed loader result/error")
					}
				} else if calls != 0 || got != nil || gotErr == nil {
					t.Fatal("invalid policy invoked operand loader")
				}
				_, err = LowerAt(context.Background(), expr, s, at, at)
				if (err == nil) != (policy == mixedPreserve) {
					t.Fatalf("policy %v dispatch error=%v", policy, err)
				}
			}
			delete(mixedOperandPolicies, key)
			_, err = LowerAt(context.Background(), expr, s, at, at)
			if err == nil || !strings.Contains(err.Error(), "not admitted") {
				t.Fatal("missing policy admitted")
			}
		})
	}
	ordinary := &chplan.Scan{}
	hist := &chplan.HistogramProjection{Input: &chplan.OneRow{}}
	for _, input := range []chplan.Node{ordinary, hist} {
		got, err := preserveMixedPlan(input, "unknown-family")
		if got != input || err != nil {
			t.Fatal("ordinary/histogram-only path changed")
		}
	}
	calls := 0
	_, err := lowerWithMixedPreservePolicy(mixedSortByLabelFamily, mixedRootAdmission, func() (chplan.Node, error) { calls++; return ordinary, nil })
	if err == nil || calls != 0 {
		t.Fatal("unregistered root admitted")
	}
}

func TestSortByLabelPreserveTopology(t *testing.T) {
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, ranged := range []bool{false, true} {
		for _, fn := range []string{"sort_by_label", "sort_by_label_desc"} {
			for _, vector := range []string{"latency_exp_hist or num_cpus", "num_cpus or latency_exp_hist", "sort_by_label(latency_exp_hist or num_cpus)", "num_cpus", "latency_exp_hist"} {
				for _, labels := range []string{"", `, "job", "__name__"`} {
					query := fn + "(" + vector + labels + ")"
					t.Run(fmt.Sprintf("range_%t/%s", ranged, query), func(t *testing.T) {
						lower := func(q string) chplan.Node {
							t.Helper()
							expr, err := p.ParseExpr(q)
							if err != nil {
								t.Fatal(err)
							}
							var plan chplan.Node
							if ranged {
								const step = 15 * time.Second
								plan, err = LowerAtRange(context.Background(), expr, s, at.Add(-time.Minute), at, step)
							} else {
								plan, err = LowerAt(context.Background(), expr, s, at, at)
							}
							if err != nil {
								t.Fatal(err)
							}
							return plan
						}
						original := lower(vector)
						got := lower(query)
						if !got.RowType().Equal(original.RowType()) {
							t.Fatal("row roles changed")
						}
						if labels == "" {
							if !got.Equal(original) {
								t.Fatal("zero-label identity changed")
							}
							return
						}
						order, ok := got.(*chplan.OrderBy)
						if !ok || !order.Input.Equal(original) {
							t.Fatal("sort operand changed")
						}
						if len(order.Keys) != 2 {
							t.Fatal("sort keys changed")
						}
						for _, key := range order.Keys {
							if key.Desc != (fn == "sort_by_label_desc") {
								t.Fatal("sort direction changed")
							}
						}
					})
				}
			}
		}
	}
}

func TestSortByLabelPreserveReturnBoolAndErrorOrder(t *testing.T) {
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	s := schema.DefaultOTelMetrics()
	expr, err := p.ParseExpr("latency_exp_hist or num_cpus")
	if err != nil {
		t.Fatal(err)
	}
	binary := expr.(*parser.BinaryExpr)
	binary.ReturnBool = true
	_, err = lowerSortByLabelArg(binary, s, lowerCtx{})
	if err == nil || err.Error() != "promql: 'bool' modifier is only allowed on comparison binary ops" {
		t.Fatalf("ReturnBool rejection changed: %v", err)
	}
	operand, err := p.ParseExpr(`count_values("", up)`)
	if err != nil {
		t.Fatal(err)
	}
	_, want := lowerSortByLabelArg(operand, s, lowerCtx{})
	if want == nil {
		t.Fatal("operand sentinel must fail")
	}
	call := &parser.Call{Func: parser.MustGetFunction("sort_by_label"), Args: parser.Expressions{operand, &parser.NumberLiteral{Val: 1}}}
	_, err = lowerSortByLabel(call, s, lowerCtx{})
	if err == nil || err.Error() != want.Error() {
		t.Fatalf("label validation moved before operand: %v", err)
	}
}
