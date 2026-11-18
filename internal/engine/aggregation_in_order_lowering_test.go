package engine

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestEligibleForAggregationInOrder_PlainInstantSelectorIsIneligible pins the
// plain instant selector, lowered and optimized the way a request is, as
// ineligible for optimize_aggregation_in_order. Its lookback Aggregate groups
// by MetricName, which reaches the Scan, and by the Attributes that the
// label-normalisation Project rebuilds with mapConcat (resource attributes
// merged in, keys sanitised, service_name added). That Attributes is not the
// stored column the sorting key orders by, so ClickHouse could only aggregate
// in order by the MetricName prefix.
func TestEligibleForAggregationInOrder_PlainInstantSelectorIsIneligible(t *testing.T) {
	t.Parallel()
	const query = "svc_http_requests_total"
	at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	expr, err := parser.NewParser(parser.Options{}).ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	lowered, err := promql.LowerAt(context.Background(), expr, schema.DefaultOTelMetrics(), at, at)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	plan := optimizer.Default().Run(context.Background(), lowered)
	f := shapeOf(plan)

	// The plan has the one Aggregate over the one table the rule requires, so
	// the verdict below rests on the GROUP BY keys alone.
	agg, ok := f.singleAggregate()
	if !ok {
		t.Fatalf("%s: want exactly one spine Aggregate", query)
	}
	if table, ok := f.singleScanTable(); !ok || table != "otel_metrics_sum" {
		t.Fatalf("%s: singleScanTable = %q, %v; want otel_metrics_sum", query, table, ok)
	}
	if stored, ok := storedColumnBelow(agg.Input, "MetricName"); !ok || stored != "MetricName" {
		t.Errorf("%s: MetricName follows to %q, %v; want the stored MetricName", query, stored, ok)
	}
	if stored, ok := storedColumnBelow(agg.Input, "Attributes"); ok {
		t.Errorf("%s: Attributes follows to the stored %q; want it rejected as rebuilt by the label-normalisation Project", query, stored)
	}

	if defaultRules().eligibleForAggregationInOrder(f, chsql.SchemaStrategies{}) {
		t.Errorf("%s: eligible for aggregation in order; want ineligible, its GROUP BY Attributes is rebuilt", query)
	}
}
