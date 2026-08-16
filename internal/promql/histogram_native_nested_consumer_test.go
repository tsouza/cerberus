package promql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestLower_ExpHistogram_NestedHistogramConsumersStayHistogramValued(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	queries := []string{
		`sum(rate(latency_exp_hist[5m]))`,
		`sum by (service) (increase(latency_exp_hist[5m]))`,
		`avg(delta(latency_exp_hist[5m]))`,
		`sum(irate(latency_exp_hist[5m]))`,
		`avg(idelta(latency_exp_hist[5m]))`,
		`sum(sum by (service) (latency_exp_hist))`,
		`label_replace(delta(latency_exp_hist[5m]), "copy", "$1-copy", "service", "(.*)")`,
		`label_join(rate(latency_exp_hist[5m]), "copy", "-", "service")`,
		`sum(rate(latency_exp_hist[5m]) * 2)`,
	}

	for _, query := range queries {
		query := query
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt(%q): %v", query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.HistogramRowShape {
				t.Fatalf("LowerAt(%q) row shape = %s, want histogram", query, shape)
			}
			if _, ok := plan.(*chplan.HistogramProjection); !ok {
				t.Fatalf("LowerAt(%q) root = %T, want *chplan.HistogramProjection", query, plan)
			}
		})
	}
}

func TestLower_ExpHistogram_NestedMergeConsumesThirteenColumnContract(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(
		`sum by (service) (rate(latency_exp_hist[5m]))`,
	)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
	if err != nil {
		t.Fatalf("LowerAt: %v", err)
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, want := range []string{
		"sum(`" + chplan.HistogramCountColumn + "`) AS `" + chplan.HistogramCountColumn + "`",
		"sum(`" + chplan.HistogramSumColumn + "`) AS `" + chplan.HistogramSumColumn + "`",
		"groupArray(`" + chplan.HistogramPositiveBucketCountsColumn + "`)",
		"`" + s.TimestampColumn + "` AS `" + s.TimestampColumn + "`",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("nested merge SQL does not consume the published histogram contract: missing %q\n%s", want, sql)
		}
	}
}

func TestLower_ExpHistogram_NestedDroppingAggregationsReturnEmpty(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, query := range []string{
		`topk(3, rate(latency_exp_hist[5m]))`,
		`bottomk(2, sum by (service) (latency_exp_hist))`,
		`max(delta(latency_exp_hist[5m]))`,
		`quantile(0.9, avg(latency_exp_hist))`,
	} {
		query := query
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt(%q): %v", query, err)
			}
			filter, ok := plan.(*chplan.Filter)
			if !ok {
				t.Fatalf("LowerAt(%q) root = %T, want empty-result *chplan.Filter", query, plan)
			}
			predicate, ok := filter.Predicate.(*chplan.LitBool)
			if !ok || predicate.V {
				t.Fatalf("LowerAt(%q) predicate = %#v, want false literal", query, filter.Predicate)
			}
			if shape := chplan.RowShapeOf(filter.Input); shape != chplan.HistogramRowShape {
				t.Fatalf("LowerAt(%q) filtered input row shape = %s, want histogram", query, shape)
			}
		})
	}
}
