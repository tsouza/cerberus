package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_MixedOrSubqueryFoldFn_SumWrapped pins cerberus
// issue #3265 Part 2: `sum`/`avg` [by/without] wrapping a FOLD-family range
// function over a subquery whose own inner is a bare mixed float/histogram
// `or` (`sum(rate((a or b)[range:step]))`) now resolves to a genuine
// MixedRowShape plan through
// [promql.sumOrAvgOverMixedOrSubqueryFoldFn]/[promql.lowerSumOrAvgOverMixedOrSubqueryFoldFn]
// instead of falling through to the ordinary, non-histogram-aware aggregate
// path (which used to read a plain `Value` column off whatever the mixed-or
// fold happened to publish — silently discarding the histogram side, or
// rendering it as a float placeholder zero; see the chDB-backed
// discrimination test in the sibling _chdb_test.go file for the actual wire
// behavior this fixes).
func TestLower_ExpHistogram_MixedOrSubqueryFoldFn_SumWrapped(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})
	at := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)

	cases := []struct {
		name  string
		query string
	}{
		{"sum/rate, no by", `sum(rate((latency_exp_hist or latency_float)[5m:1m]))`},
		{"sum by (...)", `sum by (pod) (rate((latency_exp_hist or latency_float)[5m:1m]))`},
		{"sum without (...)", `sum without (instance) (rate((latency_exp_hist or latency_float)[5m:1m]))`},
		{"avg/increase", `avg(increase((latency_exp_hist or latency_float)[5m:1m]))`},
		{"sum/sum_over_time", `sum(sum_over_time((latency_exp_hist or latency_float)[5m:1m]))`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt(%q): unexpected error: %v", tc.query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.MixedRowShape {
				t.Fatalf("lower(%q): plan root publishes %s, want %s", tc.query, shape, chplan.MixedRowShape)
			}
			setOp, ok := plan.(*chplan.VectorSetOp)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.VectorSetOp", tc.query, plan)
			}
			if !setOp.Mixed || !setOp.MixedDropCollisions {
				t.Fatalf("lower(%q): VectorSetOp{Mixed: %v, MixedDropCollisions: %v}, want both true (the sum/avg drop-on-collision rule, not the fold family's own left-biased union)", tc.query, setOp.Mixed, setOp.MixedDropCollisions)
			}
		})
	}
}
