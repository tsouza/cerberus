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

// TestLower_ExpHistogram_AggregationOverFloatVectorScalingBinop pins
// cerberus issue #2540: every aggregation family that gates on
// isExpHistogramValuedShape / lowerExpHistogramValuedShape (the shared
// recognizer/lowering pair histogram_native_scalar_binop.go's own
// isExpHistogramValuedShape and histogram_native_float_fn.go's own
// lowerExpHistogramValuedShape form) now recognises and lowers the
// exp-histogram/float-vector SCALING join shape
// expHistogramFloatVectorScalingBinop answers (cerberus issues
// #2339/#2342/#2537) as histogram-valued, instead of falling through to
// lowerRoot's generic lower() dispatch and hitting
// expHistogramSelectorRouting's catch-all rejection on the bare
// histogram operand underneath.
//
// Probing before this fix (see the issue) found the SAME catch-all
// rejection for every aggregation op tried — sum()/avg() (the
// histogram-VALUED merge family), count()/group() (the value-blind
// family), and min()/topk() (the histogram-DROPPING family) — which is
// why the fix widens the ONE shared gate rather than patching each
// aggregation recognizer separately, and why this test asserts across
// that same spread rather than only sum()/avg().
//
// It also pins the SAME root cause's chained/nested-scaling variant
// `(hist * float) * float2` (one of the issue's two explicitly open
// questions): once the inner MUL is recognised as histogram-valued, the
// OUTER MUL's own expHistogramFloatVectorScalingBinop recognizer (which
// itself calls isExpHistogramValuedShape on its LHS) resolves
// transitively, with no separate fix needed. The SUBQUERY-wrapping
// variant (the issue's other open question) is deliberately NOT covered
// here — probing found it is a genuinely separate, broader root cause
// (lowerSubquery's own per-inner-type dispatch calls the generic
// lowerBinary/lowerAggregate/lowerVectorSelector directly rather than
// lowerRoot's histogram-native dispatch, so even a BARE histogram
// selector loses native routing under a subquery) — tracked by its own
// issue, not this one.
func TestLower_ExpHistogram_AggregationOverFloatVectorScalingBinop(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	// The exact shape cerberus issue #2540 names, wrapped by the
	// aggregations below.
	const scalingShape = `demo_latency_exp_hist * on(service) group_left() histogram_quantile(0.5, demo_latency_exp_hist)`

	cases := []struct {
		name      string
		query     string
		wantShape chplan.RowShape
	}{
		{name: "sum", query: `sum(` + scalingShape + `)`, wantShape: chplan.HistogramRowShape},
		{name: "avg", query: `avg(` + scalingShape + `)`, wantShape: chplan.HistogramRowShape},
		{name: "count", query: `count(` + scalingShape + `)`, wantShape: chplan.SampleRowShape},
		{name: "group", query: `group(` + scalingShape + `)`, wantShape: chplan.SampleRowShape},
		{name: "count_values", query: `count_values("le", ` + scalingShape + `)`, wantShape: chplan.SampleRowShape},
		{name: "min", query: `min(` + scalingShape + `)`, wantShape: chplan.SampleRowShape},
		{name: "max", query: `max(` + scalingShape + `)`, wantShape: chplan.SampleRowShape},
		{name: "stddev", query: `stddev(` + scalingShape + `)`, wantShape: chplan.SampleRowShape},
		{name: "stdvar", query: `stdvar(` + scalingShape + `)`, wantShape: chplan.SampleRowShape},
		{name: "quantile", query: `quantile(0.9, ` + scalingShape + `)`, wantShape: chplan.SampleRowShape},
		{name: "topk", query: `topk(3, ` + scalingShape + `)`, wantShape: chplan.SampleRowShape},
		{name: "bottomk", query: `bottomk(3, ` + scalingShape + `)`, wantShape: chplan.SampleRowShape},
		{
			// Chained/nested scaling (open question 2): the outer MUL's
			// LHS is itself a scaling-join shape, not a bare selector.
			name:      "chained scaling",
			query:     `(demo_latency_exp_hist * on(service) histogram_quantile(0.5, demo_latency_exp_hist)) * on(service) histogram_quantile(0.9, demo_latency_exp_hist)`,
			wantShape: chplan.HistogramRowShape,
		},
		{
			// Same chained shape, wrapped by sum() — both open questions
			// (aggregation + chaining) composed together.
			name:      "sum over chained scaling",
			query:     `sum((demo_latency_exp_hist * on(service) histogram_quantile(0.5, demo_latency_exp_hist)) * on(service) histogram_quantile(0.9, demo_latency_exp_hist))`,
			wantShape: chplan.HistogramRowShape,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}

			instant, err := promql.LowerAt(context.Background(), expr, s, end, end)
			if err != nil {
				t.Fatalf("LowerAt(%q): %v", tc.query, err)
			}
			if got := chplan.RowShapeOf(instant); got != tc.wantShape {
				t.Errorf("LowerAt(%q): row shape = %v, want %v", tc.query, got, tc.wantShape)
			}

			rangeExpr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			ranged, err := promql.LowerAtRange(context.Background(), rangeExpr, s, start, end, 30*time.Second)
			if err != nil {
				t.Fatalf("LowerAtRange(%q): %v", tc.query, err)
			}
			if got := chplan.RowShapeOf(ranged); got != tc.wantShape {
				t.Errorf("LowerAtRange(%q): row shape = %v, want %v", tc.query, got, tc.wantShape)
			}
		})
	}
}

// TestLower_ExpHistogram_SubqueryOverFloatVectorScalingBinop documents the
// boundary the test above deliberately stayed inside of when it was
// written: a subquery wrapping the SAME scaling-join shape (cerberus
// issue #2540's own "subquery-wrapping variant" open question, split out
// as cerberus issue #2543 since it turned out to be a genuinely separate,
// broader root cause — lowerSubquery's own per-inner-type dispatch never
// consulted lowerRoot's histogram-native table for ANY histogram-native
// shape, not only this one, so a BARE `(demo_latency_exp_hist)[5m:1m]`
// and `sum(demo_latency_exp_hist)[5m:1m]` rejected the identical way).
// #2543's own fix taught lowerSubquery to try that table against a
// subquery's own inner expression, so this shape now lowers successfully
// too — this test was
// TestLower_ExpHistogram_SubqueryOverFloatVectorScalingBinopStillRejected
// before that fix landed. TestSubqueryHistogramFloatVectorScalingJoin_ChDB
// (subquery_histogram_native_chdb_test.go) covers the same shape with real
// chDB execution and numeric verification; this sibling stays as the fast,
// no-chDB row-shape pin the rest of this file's cases already use.
func TestLower_ExpHistogram_SubqueryOverFloatVectorScalingBinop(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	const query = `(demo_latency_exp_hist * on(service) group_left() histogram_quantile(0.5, demo_latency_exp_hist))[5m:1m]`
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAtRange(context.Background(), expr, s, start, end, 30*time.Second)
	if err != nil {
		t.Fatalf("LowerAtRange(%q): %v", query, err)
	}
	if got := chplan.RowShapeOf(plan); got != chplan.HistogramRowShape {
		t.Errorf("LowerAtRange(%q): row shape = %v, want %v", query, got, chplan.HistogramRowShape)
	}
}
