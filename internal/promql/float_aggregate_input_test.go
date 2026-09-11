package promql

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

func floatAggregateTestQuery(fn, operand string) string {
	if fn == "quantile" {
		operand = "0.5," + operand
	}
	return fn + "(" + operand + ")"
}

func TestFloatAggregateMixedInputNarrowing(t *testing.T) {
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, fn := range []string{"min", "max", "stddev", "stdvar", "quantile"} {
		for _, custom := range []bool{false, true} {
			for _, mirror := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/custom_%v/mirror_%v", fn, custom, mirror), func(t *testing.T) {
					s := schema.DefaultOTelMetrics()
					if custom {
						s.MetricNameColumn = "metric_id"
						s.AttributesColumn = "labels_map"
						s.TimestampColumn = "sample_time"
						s.ValueColumn = "sample_value"
					}
					operand := `sort_by_label(latency_exp_hist or num_cpus,"job")`
					if mirror {
						operand = `sort_by_label_desc(num_cpus or latency_exp_hist,"job")`
					}
					expr, err := p.ParseExpr(floatAggregateTestQuery(fn, operand))
					if err != nil {
						t.Fatal(err)
					}
					plan, err := LowerAt(context.Background(), expr, s, at, at)
					if err != nil {
						t.Fatal(err)
					}
					var narrowed int
					chplan.Walk(plan, func(node chplan.Node) bool {
						if filter, ok := node.(*chplan.Filter); ok && chplan.IsMixedFloatNarrowing(filter) {
							narrowed++
							if chplan.RowShapeOf(filter.Input) != chplan.MixedRowShape {
								t.Fatal("narrowing lost original mixed relation")
							}
							if _, ok := filter.Input.(*chplan.OrderBy); !ok {
								t.Fatalf("narrowing reconstructed ordered operand: %T", filter.Input)
							}
						}
						return true
					})
					if narrowed != 1 {
						t.Fatalf("float aggregate has %d discriminator filters, want one", narrowed)
					}
				})
			}
		}
	}
}

func TestFloatAggregateAuthorizationBeforeNarrowing(t *testing.T) {
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, fn := range []string{"min", "max", "stddev", "stdvar", "quantile"} {
		t.Run(fn, func(t *testing.T) {
			key := mixedWrapperKey{family: mixedFloatAggregateFamily, site: mixedPlanAdmission}
			policy := mixedOperandPolicies[key]
			delete(mixedOperandPolicies, key)
			t.Cleanup(func() { mixedOperandPolicies[key] = policy })
			expr, err := p.ParseExpr(floatAggregateTestQuery(fn, `sort_by_label(latency_exp_hist or num_cpus,"job")`))
			if err != nil {
				t.Fatal(err)
			}
			plan, err := LowerAt(context.Background(), expr, s, at, at)
			if plan != nil || err == nil || !strings.Contains(err.Error(), "mixed operand is not admitted for float-aggregate at existing-plan") {
				t.Fatalf("original Mixed authorization bypassed: %T %v", plan, err)
			}
			for _, operand := range []string{"up", "latency_exp_hist"} {
				expr, err := p.ParseExpr(floatAggregateTestQuery(fn, operand))
				if err != nil {
					t.Fatal(err)
				}
				if plan, err := LowerAt(context.Background(), expr, s, at, at); plan == nil || err != nil {
					t.Fatalf("ordinary/histogram-only path consulted mixed admission: %T %v", plan, err)
				}
			}
		})
	}
}

func TestFloatAggregateNarrowingLeavesOtherFamilies(t *testing.T) {
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, fn := range []string{"sum", "avg", "count", "group"} {
		t.Run(fn, func(t *testing.T) {
			expr, err := p.ParseExpr(fn + `(sort_by_label(latency_exp_hist or num_cpus,"job"))`)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatal(err)
			}
			if fn == "sum" || fn == "avg" {
				// Sum/avg now partition sample kinds rather than discarding the
				// histogram side. Only their float reduction narrows to floats.
				union, ok := plan.(*chplan.VectorSetOp)
				if !ok || !union.MixedDropCollisions || chplan.RowShapeOf(union.Left) != chplan.HistogramRowShape || chplan.RowShapeOf(plan) != chplan.MixedRowShape {
					t.Fatalf("%s lost histogram group semantics: %T", fn, plan)
				}
				return
			}
			chplan.Walk(plan, func(node chplan.Node) bool {
				if filter, ok := node.(*chplan.Filter); ok && chplan.IsMixedFloatNarrowing(filter) {
					t.Fatalf("%s acquired float-only semantics", fn)
				}
				return true
			})
		})
	}
}

func TestFloatAggregateNativeGridUnchanged(t *testing.T) {
	p := parser.NewParser(parser.Options{})
	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, fn := range []string{"min", "max", "stddev", "stdvar", "quantile"} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%v", fn, enabled), func(t *testing.T) {
				expr, err := p.ParseExpr(floatAggregateTestQuery(fn, "rate(cerberus_queries_total[5m])"))
				if err != nil {
					t.Fatal(err)
				}
				plan, err := LowerAtRangeOpts(context.Background(), expr, s, start, start.Add(5*time.Minute), time.Minute, LowerOpts{Lowerers: RangeLowerers{Rate: NativeRateLowerer{Fallback: FanoutRateLowerer{}}, VectorAgg: enabled}})
				if err != nil {
					t.Fatal(err)
				}
				var native bool
				chplan.Walk(plan, func(node chplan.Node) bool {
					if _, ok := node.(*chplan.RangeWindowGridNativeVectorAgg); ok {
						native = true
					}
					if filter, ok := node.(*chplan.Filter); ok && chplan.IsMixedFloatNarrowing(filter) {
						t.Fatal("ordinary native input acquired mixed narrowing")
					}
					return true
				})
				want := enabled && (fn == "min" || fn == "max")
				if native != want {
					t.Fatalf("native fold=%v want %v", native, want)
				}
			})
		}
	}
}
