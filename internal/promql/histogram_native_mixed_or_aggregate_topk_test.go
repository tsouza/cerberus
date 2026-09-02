package promql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_MixedSetOpOr_TopKWrapped is the untagged
// ACCEPTANCE pin for [topKOverMixedExpHistogramSetOp]
// (histogram_native_mixed_or_aggregate_topk.go, cerberus issue #2600):
// `topk`/`bottomk` [by/without] directly wrapping a mixed float/histogram
// `or` lowers to a chplan.TopK over the shadow-resolved float arm instead
// of falling through to internal/promql/binary.go's lowerVectorSetOp
// rejection.
//
// The shape already had a chDB execution proof
// (histogram_native_mixed_or_aggregate_topk_chdb_test.go), but that file
// is behind the `chdb` build tag, so nothing in the untagged build ever
// asked the recognizer to ACCEPT. Inverting the `&&` in its guard
//
//	if !ok || (agg.Op != parser.TOPK && agg.Op != parser.BOTTOMK) {
//
// to `||` makes the parenthesised clause a tautology for every agg.Op —
// the recognizer then reports "not mine" for every input and the whole
// lowering is dead — and the untagged suite did not notice
// (cerberus issue #2943; the mutant reached a verdict for the first time
// once #2940 stopped `go vet`'s bools analyzer rejecting it as
// "suspect and" before it could be adjudicated).
//
// The rejection counter-case below is what makes the acceptance
// assertions discriminating: `sum` over the identical `or` must NOT take
// this route, so the test cannot pass by the recognizer accepting
// everything either.
func TestLower_ExpHistogram_MixedSetOpOr_TopKWrapped(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		query string
		k     int64
		desc  bool
		byLen int
	}{
		{
			name:  "topk, histogram on left",
			query: `topk(2, latency_exp_hist or histogram_quantile(0.5, latency_exp_hist))`,
			k:     2,
			desc:  true,
		},
		{
			name:  "topk, float on left",
			query: `topk(3, histogram_quantile(0.5, latency_exp_hist) or latency_exp_hist)`,
			k:     3,
			desc:  true,
		},
		{
			name:  "bottomk, histogram on left",
			query: `bottomk(2, latency_exp_hist or histogram_quantile(0.5, latency_exp_hist))`,
			k:     2,
			desc:  false,
		},
		{
			name:  "topk by (...)",
			query: `topk by (service) (2, latency_exp_hist or histogram_quantile(0.5, latency_exp_hist))`,
			k:     2,
			desc:  true,
			byLen: 1,
		},
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
			topK, ok := plan.(*chplan.TopK)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.TopK — the topk-over-mixed-`or` recognizer did not accept the shape it exists to accept",
					tc.query, plan)
			}
			if topK.K != tc.k {
				t.Errorf("lower(%q): TopK.K = %d, want %d", tc.query, topK.K, tc.k)
			}
			if topK.Desc != tc.desc {
				t.Errorf("lower(%q): TopK.Desc = %v, want %v", tc.query, topK.Desc, tc.desc)
			}
			if len(topK.By) != tc.byLen {
				t.Errorf("lower(%q): len(TopK.By) = %d, want %d", tc.query, len(topK.By), tc.byLen)
			}
			// Reference's aggregationK drops every histogram sample from
			// K-selection, so the ranked input is the FLOAT arm alone and
			// the result rows are float-shaped.
			if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
				t.Errorf("lower(%q): plan root publishes %s, want %s — topk/bottomk rank float rows only",
					tc.query, shape, chplan.SampleRowShape)
			}
		})
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_NonTopKAggregateNotRouted is the
// counter-case for TestLower_ExpHistogram_MixedSetOpOr_TopKWrapped: an
// aggregate that is NEITHER topk NOR bottomk over the identical mixed
// `or` must not take the topk route. It keeps that test honest — a
// recognizer that accepted every aggregate would satisfy the acceptance
// assertions but fail here.
func TestLower_ExpHistogram_MixedSetOpOr_NonTopKAggregateNotRouted(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	const query = `sum(latency_exp_hist or histogram_quantile(0.5, latency_exp_hist))`
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
	if err != nil {
		t.Fatalf("LowerAt(%q): unexpected error: %v", query, err)
	}
	if _, ok := plan.(*chplan.TopK); ok {
		t.Fatalf("lower(%q): plan root is a *chplan.TopK — sum() must not be routed through the topk/bottomk recognizer", query)
	}
	if _, ok := plan.(*chplan.VectorSetOp); !ok {
		t.Fatalf("lower(%q): plan root is %T, want *chplan.VectorSetOp", query, plan)
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_TopKBoolModifierRejected pins the
// `bool` guard [lowerTopKOverMixedExpHistogramSetOp] carries: `bool` is
// only legal on a comparison binary op, never on the `or` this
// composition lowers, so the shape is rejected rather than silently
// lowered.
//
// The rejection message is NOT unique — thirteen sites in this package
// emit the same "'bool' modifier is only allowed on comparison binary
// ops" text, so matching it alone would pass even with this recognizer
// disabled entirely and the rejection coming from some other route. The
// test therefore establishes the route FIRST: the identical AST with
// ReturnBool cleared must lower to a *chplan.TopK, which is reachable
// only through this file's recognizer. Only then is ReturnBool set and
// the rejection asserted, so the second half is known to be this guard's
// answer and not a stranger's.
func TestLower_ExpHistogram_MixedSetOpOr_TopKBoolModifierRejected(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// `bool` on a set op is rejected by the upstream parser itself, so
	// the guard is reached only via a hand-built AST with ReturnBool set.
	const query = `topk(2, latency_exp_hist or histogram_quantile(0.5, latency_exp_hist))`
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	agg, ok := expr.(*parser.AggregateExpr)
	if !ok {
		t.Fatalf("ParseExpr(%q) produced %T, want *parser.AggregateExpr", query, expr)
	}
	bin, ok := agg.Expr.(*parser.BinaryExpr)
	if !ok {
		t.Fatalf("topk operand is %T, want *parser.BinaryExpr", agg.Expr)
	}

	// Route control: without ReturnBool this AST reaches
	// lowerTopKOverMixedExpHistogramSetOp and lowers. If this fails, the
	// rejection below proves nothing about that function.
	plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
	if err != nil {
		t.Fatalf("LowerAt(%q) without ReturnBool: unexpected error: %v — the rejection assertion below would not be about this recognizer", query, err)
	}
	if _, isTopK := plan.(*chplan.TopK); !isTopK {
		t.Fatalf("LowerAt(%q) without ReturnBool: plan root is %T, want *chplan.TopK — this AST is not on the topk-over-mixed-`or` route, so the rejection below would be some other site's",
			query, plan)
	}

	bin.ReturnBool = true
	_, err = promql.LowerAt(context.Background(), expr, s, at, at)
	if err == nil {
		t.Fatalf("LowerAt(%q) with ReturnBool set: expected an error, got none", query)
	}
	if !strings.Contains(err.Error(), "'bool' modifier") {
		t.Fatalf("LowerAt(%q) with ReturnBool set: error = %v, want the 'bool' modifier rejection", query, err)
	}
}
