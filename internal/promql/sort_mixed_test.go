package promql_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/spec"
)

func TestSortNestedMixedOuterProjection(t *testing.T) {
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, format := range []string{
		`abs(%s)`,
		`round(%s)`,
		`clamp_min(%s, 0)`,
		`label_replace(%s, "series", "$1", "series", "(.*)")`,
	} {
		query := fmt.Sprintf(format, `sort(sort_by_label(latency_exp_hist or num_cpus, "job"))`)
		t.Run(query, func(t *testing.T) {
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, schema.DefaultOTelMetrics(), at, at)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := chsql.Emit(context.Background(), plan); err != nil {
				t.Fatalf("raw emission: %v", err)
			}
			optimized := spec.AssertScanTimeBoundAccepts(t, plan)
			if _, _, err := chsql.Emit(context.Background(), optimized); err != nil {
				t.Fatalf("optimized emission: %v", err)
			}
		})
	}
}

func TestSortNestedMixedNarrowsBeforeOrdering(t *testing.T) {
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	standard := schema.DefaultOTelMetrics()
	aliased := standard
	aliased.MetricNameColumn = "metric_id"
	aliased.AttributesColumn = "labels_map"
	aliased.TimestampColumn = "sample_time"
	aliased.ValueColumn = "sample_value"
	for _, s := range []schema.Metrics{standard, aliased} {
		for _, step := range []time.Duration{0, time.Minute} {
			for _, fn := range []string{"sort", "sort_desc"} {
				for _, nested := range []string{"sort_by_label", "sort_by_label_desc"} {
					for _, operand := range []string{"latency_exp_hist or num_cpus", "num_cpus or latency_exp_hist"} {
						query := fn + "(" + nested + "(" + operand + `, "series"))`
						t.Run(fmt.Sprintf("%s/%s/%s", s.ValueColumn, step, query), func(t *testing.T) {
							expr, err := p.ParseExpr(query)
							if err != nil {
								t.Fatal(err)
							}
							plan, err := promql.LowerAtRange(context.Background(), expr, s, at.Add(-step), at, step)
							if err != nil {
								t.Fatal(err)
							}
							order, ok := plan.(*chplan.OrderBy)
							if !ok {
								t.Fatalf("root=%T, want OrderBy", plan)
							}
							filter, ok := order.Input.(*chplan.Filter)
							if !ok || !chplan.IsMixedFloatNarrowing(filter) {
								t.Fatalf("sort input=%T, want strict float narrowing", order.Input)
							}
							if got := chplan.RowShapeOf(filter.Input); got != chplan.MixedRowShape {
								t.Fatalf("filter input=%s, want the complete mixed operand", got)
							}
							if len(order.Keys) != 1 || order.Keys[0].Desc != (fn == "sort_desc") {
								t.Fatalf("sort keys changed: %#v", order.Keys)
							}
							column, ok := order.Keys[0].Expr.(*chplan.ColumnRef)
							if !ok || column.Name != s.ValueColumn {
								t.Fatalf("sort value role=%#v, want %s", order.Keys[0].Expr, s.ValueColumn)
							}
						})
					}
				}
			}
		}
	}
}
