package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/spec"
)

func TestScalarWindowSubqueryEstablishesInstantScanBound(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	custom := schema.DefaultOTelMetrics()
	custom.MetricNameColumn = "metric_id"
	custom.AttributesColumn = "labels_map"
	custom.TimestampColumn = "sample_time"
	custom.ValueColumn = "sample_value"

	for _, tc := range []struct {
		name   string
		query  string
		schema schema.Metrics
	}{
		{name: "direct/default schema", query: `scalar(sum_over_time(num_cpus[5m]))`, schema: schema.DefaultOTelMetrics()},
		{name: "nested/default schema", query: `vector(scalar(sum_over_time(num_cpus[5m])))`, schema: schema.DefaultOTelMetrics()},
		{name: "mixed histogram float/default schema", query: `scalar(sum_over_time((latency_exp_hist or latency_float)[5m:1m]))`, schema: schema.DefaultOTelMetrics()},
		{name: "direct/aliased schema", query: `scalar(sum_over_time(num_cpus[5m]))`, schema: custom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			plan, err := promql.LowerAtRange(context.Background(), expr, tc.schema, at, at, 0)
			if err != nil {
				t.Fatalf("LowerAtRange: %v", err)
			}
			for _, candidate := range []chplan.Node{plan, spec.AssertScanTimeBoundAccepts(t, chplan.CloneNode(plan))} {
				if _, _, err := chsql.Emit(context.Background(), candidate); err != nil {
					t.Fatalf("Emit: %v", err)
				}
			}
		})
	}
}
