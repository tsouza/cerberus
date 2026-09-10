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

func TestNestedMixedComparisonNarrowsAfterOperand(t *testing.T) {
	t.Parallel()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, rangeQuery := range []bool{false, true} {
		for _, sortFn := range []string{"sort_by_label", "sort_by_label_desc"} {
			for _, operand := range []string{"latency_exp_hist or num_cpus", "num_cpus or latency_exp_hist", "num_cpus"} {
				for _, scalar := range []string{"0", "scalar(vector(0))"} {
					for _, scalarLeft := range []bool{false, true} {
						vector := fmt.Sprintf("%s(%s, \"job\")", sortFn, operand)
						query := vector + " < " + scalar
						if scalarLeft {
							query = scalar + " < " + vector
						}
						t.Run(fmt.Sprintf("range_%t/%s", rangeQuery, query), func(t *testing.T) {
							lower := func(query string) chplan.Node {
								t.Helper()
								expr, err := p.ParseExpr(query)
								if err != nil {
									t.Fatal(err)
								}
								var plan chplan.Node
								if rangeQuery {
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
							plan := lower(query)
							comparison, ok := plan.(*chplan.Filter)
							if !ok {
								t.Fatalf("comparison is %T, want Filter", plan)
							}
							predicate, ok := comparison.Predicate.(*chplan.Binary)
							if !ok || predicate.Op != chplan.OpLt {
								t.Fatalf("comparison predicate = %#v", comparison.Predicate)
							}
							value := predicate.Left
							if scalarLeft {
								value = predicate.Right
							}
							column, ok := value.(*chplan.ColumnRef)
							if !ok || column.Name != s.ValueColumn {
								t.Fatalf("vector value/order changed: %#v", predicate)
							}
							if operand == "num_cpus" {
								if !comparison.Input.Equal(original) {
									t.Fatal("ordinary float operand changed")
								}
								return
							}
							if !chplan.IsMixedFloatNarrowing(comparison.Input) {
								t.Fatalf("comparison input %T does not strictly narrow floats", comparison.Input)
							}
							narrow := comparison.Input.(*chplan.Filter)
							if chplan.RowShapeOf(narrow.Input) != chplan.MixedRowShape {
								t.Fatal("narrowing must consume mixed operand")
							}
							if !narrow.Input.Equal(original) {
								t.Fatal("mixed operand changed before narrowing: union shadowing/order must be preserved")
							}
							if chplan.RowShapeOf(plan) != chplan.SampleRowShape {
								t.Fatal("comparison result must contain only floats")
							}
						})
					}
				}
			}
		}
	}
}
