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
		`rate(latency_exp_hist[5m:1m])`,
		`increase(sum by (service) (latency_exp_hist)[5m:1m])`,
		`delta(label_join(latency_exp_hist, "copy", "-", "service")[5m:1m])`,
		`irate((latency_exp_hist * 2)[5m:1m])`,
		`idelta(latency_exp_hist[5m:1m] offset 1m)`,
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
		"groupArray(`" + chplan.HistogramCountColumn + "`) AS `_hq_merge_counts`",
		"groupArray(`" + chplan.HistogramSumColumn + "`) AS `_hq_merge_sums`",
		"groupArray(`" + chplan.HistogramPositiveBucketCountsColumn + "`)",
		"arrayFold(",
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

func TestLower_ExpHistogram_NestedPresenceAggregationsAreFloatValued(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, query := range []string{
		`count(rate(latency_exp_hist[5m]))`,
		`count by (service) (sum by (service, instance) (latency_exp_hist))`,
		`group(rate(latency_exp_hist[5m]))`,
		`group without (instance) (avg by (service, instance) (latency_exp_hist))`,
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
			if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
				t.Fatalf("LowerAt(%q) row shape = %s, want sample", query, shape)
			}
			project, ok := plan.(*chplan.Project)
			if !ok {
				t.Fatalf("LowerAt(%q) root = %T, want *chplan.Project", query, plan)
			}
			reduced, ok := project.Input.(*chplan.Aggregate)
			if !ok {
				t.Fatalf("LowerAt(%q) Project.Input = %T, want *chplan.Aggregate", query, project.Input)
			}
			if shape := chplan.RowShapeOf(reduced.Input); shape != chplan.HistogramRowShape {
				t.Fatalf("LowerAt(%q) reduced input row shape = %s, want histogram", query, shape)
			}
		})
	}
}

func TestLower_ExpHistogram_CountValuesStringifiesPublishedHistogram(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, query := range []string{
		`count_values("hist", latency_exp_hist)`,
		`count_values("hist", rate(latency_exp_hist[5m])) by (service)`,
		`count_values("hist", sum by (service, instance) (latency_exp_hist)) without (instance)`,
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
			if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
				t.Fatalf("LowerAt(%q) row shape = %s, want sample", query, shape)
			}
			project, ok := plan.(*chplan.Project)
			if !ok {
				t.Fatalf("LowerAt(%q) root = %T, want *chplan.Project", query, plan)
			}
			reduced, ok := project.Input.(*chplan.Aggregate)
			if !ok {
				t.Fatalf("LowerAt(%q) Project.Input = %T, want *chplan.Aggregate", query, project.Input)
			}
			if shape := chplan.RowShapeOf(reduced.Input); shape != chplan.HistogramRowShape {
				t.Fatalf("LowerAt(%q) reduced input row shape = %s, want histogram", query, shape)
			}

			// clickhouse-go's native-parameter detector is text-only: a
			// `{...:...}` substring anywhere in SQL diverts every positional
			// argument into the named-parameter binder. Histogram.String starts
			// with "{count:", so its opening brace must be computed rather than
			// written literally into the query text.
			sql, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%q): %v", query, err)
			}
			if strings.Contains(sql, "{") {
				t.Fatalf("count_values(%q) SQL trips clickhouse-go's native-parameter detector:\n%s", query, sql)
			}
			if !strings.Contains(sql, "concat(char(?)") {
				t.Fatalf("count_values(%q) SQL does not compute Histogram.String's opening brace via char():\n%s", query, sql)
			}
			const wantHistogramOpenBraceCode = int64(123)
			hasOpenBraceCode := false
			for _, arg := range args {
				if arg == wantHistogramOpenBraceCode {
					hasOpenBraceCode = true
					break
				}
			}
			if !hasOpenBraceCode {
				t.Fatalf("count_values(%q) has no opening-brace char code argument: %#v", query, args)
			}
		})
	}
}

func TestLower_ExpHistogram_SubqueryMatrixPreservesHistogramContract(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Minute)

	for _, tc := range []struct {
		name        string
		query       string
		lower       func(parser.Expr) (chplan.Node, error)
		wantFanout  bool
		wantCross   bool
		wantMinRows int
	}{
		{
			name:  "instant offset",
			query: `rate(latency_exp_hist[2m:1m] offset 1m)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:        "query range",
			query:       `delta(latency_exp_hist[2m:1m])`,
			wantFanout:  true,
			wantMinRows: 2,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, time.Minute)
			},
		},
		{
			name:      "pinned query range broadcasts",
			query:     `irate(latency_exp_hist[2m:1m] @ 1767225600)`,
			wantCross: true,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, time.Minute)
			},
		},
		{
			name:        "two sample floor",
			query:       `rate(latency_exp_hist[1m:1m])`,
			wantFanout:  true,
			wantMinRows: 2,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, time.Minute)
			},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := tc.lower(expr)
			if err != nil {
				t.Fatalf("lower(%q): %v", tc.query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.HistogramRowShape {
				t.Fatalf("lower(%q) row shape = %s, want histogram", tc.query, shape)
			}
			if _, ok := plan.(*chplan.HistogramProjection); !ok {
				t.Fatalf("lower(%q) root = %T, want *chplan.HistogramProjection", tc.query, plan)
			}

			var fanout *chplan.RangeBucketFanout
			crosses := 0
			chplan.Walk(plan, func(n chplan.Node) bool {
				switch v := n.(type) {
				case *chplan.RangeBucketFanout:
					// The inner bare histogram evaluation has its own
					// staleness fanout. The outer subquery consumer is the
					// one whose input has already crossed HistogramRowShape.
					if chplan.RowShapeOf(v.Input) == chplan.HistogramRowShape {
						fanout = v
					}
				case *chplan.CrossJoin:
					crosses++
				}
				return true
			})
			if tc.wantFanout != (fanout != nil) {
				t.Errorf("lower(%q) fanout present=%v, want %v", tc.query, fanout != nil, tc.wantFanout)
			}
			if tc.wantCross != (crosses > 0) {
				t.Errorf("lower(%q) CrossJoin count=%d, want present=%v", tc.query, crosses, tc.wantCross)
			}
			if fanout != nil && tc.wantMinRows != 0 && fanout.MinSamples != tc.wantMinRows {
				t.Errorf("lower(%q) fanout MinSamples=%d, want %d", tc.query, fanout.MinSamples, tc.wantMinRows)
			}
		})
	}
}
