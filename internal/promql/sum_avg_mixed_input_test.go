package promql

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestSumAvgMixedInputKeepsTypePartitions(t *testing.T) {
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	standard := schema.DefaultOTelMetrics()
	custom := standard
	custom.MetricNameColumn, custom.AttributesColumn, custom.TimestampColumn, custom.ValueColumn = "metric_id", "labels_map", "sample_time", "sample_value"
	for _, s := range []schema.Metrics{standard, custom} {
		for _, step := range []time.Duration{0, time.Second} {
			for _, op := range []string{"sum", "avg"} {
				for _, operand := range []string{`sort_by_label(latency_exp_hist or num_cpus,"job")`, `sort_by_label_desc(num_cpus or latency_exp_hist,"job")`} {
					t.Run(fmt.Sprintf("%s/%s/%s/%s", s.ValueColumn, step, op, operand), func(t *testing.T) {
						lowerQuery := func(q string) chplan.Node {
							e, err := p.ParseExpr(q)
							if err != nil {
								t.Fatal(err)
							}
							n, err := LowerAtRange(context.Background(), e, s, at, at.Add(step), step)
							if err != nil {
								t.Fatal(err)
							}
							return n
						}
						original := lowerQuery(operand)
						got := lowerQuery(op + " by(job)(" + operand + ")")
						union, ok := got.(*chplan.VectorSetOp)
						if !ok || !union.Mixed || !union.MixedDropCollisions || union.StepAligned != (step > 0) {
							t.Fatalf("sum/avg must drop mixed groups after separate reductions: %T", got)
						}
						for _, branch := range []chplan.Node{union.Left, union.Right} {
							found := false
							chplan.Walk(branch, func(n chplan.Node) bool {
								if n.Equal(original) {
									found = true
								}
								return true
							})
							if !found {
								t.Fatal("partition replaced the already-shadowed operand")
							}
						}
					})
				}
			}
		}
	}
}

func TestSumAvgMixedAuthorizationPrecedesPartition(t *testing.T) {
	key := mixedWrapperKey{family: mixedSumAvgFamily, site: mixedPlanAdmission}
	old := mixedOperandPolicies[key]
	delete(mixedOperandPolicies, key)
	t.Cleanup(func() { mixedOperandPolicies[key] = old })
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	for _, op := range []string{"sum", "avg"} {
		e, err := p.ParseExpr(op + `(sort_by_label(latency_exp_hist or num_cpus,"job"))`)
		if err != nil {
			t.Fatal(err)
		}
		if n, err := Lower(context.Background(), e, schema.DefaultOTelMetrics()); n != nil || err == nil {
			t.Fatalf("missing policy admitted %s: %T %v", op, n, err)
		}
	}
}
