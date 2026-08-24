package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_LimitKAndLimitRatioPreserveSamples pins cerberus
// issue #2518: reference Prometheus's LIMITK and LIMIT_RATIO arms of
// aggregationK (promql/engine.go) build every Sample — float or
// histogram — from ev.nextValues and push it onto the group heap / ratio
// sampler unconditionally; neither arm branches on `s.H`, unlike
// TOPK/BOTTOMK's explicit ignore-and-annotate branch just above them in
// the same function. `limitk`/`limit_ratio` therefore need the
// recognise-and-PRESERVE treatment [lowerSortByLabel] already applies
// (cerberus issue #2462), not the recognise-and-drop treatment
// topk/bottomk use (histogram_native_drop_aggregation.go) — before this
// fix, both fell through the generic lower() dispatch and hit
// expHistogramSelectorRouting's catch-all rejection instead.
func TestLower_ExpHistogram_LimitKAndLimitRatioPreserveSamples(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	queries := []string{
		// Literal K / literal ratio, the common case.
		`limitk(3, latency_exp_hist)`,
		`limit_ratio(0.5, latency_exp_hist)`,
		`limit_ratio(-0.5, latency_exp_hist)`,

		// by/without partitioning composes the same as topk/bottomk.
		`limitk(3, latency_exp_hist) by (job)`,
		`limitk(3, latency_exp_hist) without (instance)`,

		// Computed K / computed ratio (scalar(<vector>)) routes through
		// lowerTopKComputed / the runtime ratio predicate respectively —
		// both need the same histogram-preserving input.
		`limitk(scalar(up), latency_exp_hist)`,
		`limit_ratio(scalar(up), latency_exp_hist)`,

		// The consumer sees histogram-valued results from a nested
		// lowering, not only a bare selector.
		`limitk(3, sum(latency_exp_hist))`,
		`limit_ratio(0.5, rate(latency_exp_hist[5m]))`,

		// Composes under a further wrapper the same way sort_by_label
		// does (#2462).
		`sort_by_label(limitk(3, latency_exp_hist), "job")`,
	}

	for _, query := range queries {
		query := query
		t.Run(query, func(t *testing.T) {
			t.Parallel()

			expr := parseExprExp(t, query)
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt(%q): %v", query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.HistogramRowShape {
				t.Fatalf("LowerAt(%q) plan publishes %s, want histogram — the sample must be preserved, not dropped or rejected", query, shape)
			}
		})
	}
}

// TestLower_ExpHistogram_LimitKEmptyKStaysHistogramShaped pins the K < 1
// degenerate fold (topKDomain's "empty" case, shared with topk/bottomk):
// limitk still recognises a histogram-valued input first, so the
// resulting constant-false chplan.Filter still reports HistogramRowShape
// — the SQL stays column-set-compatible with its histogram-valued
// Input even though the predicate keeps zero rows.
func TestLower_ExpHistogram_LimitKEmptyKStaysHistogramShaped(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	expr := parseExprExp(t, `limitk(0, latency_exp_hist)`)
	plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
	if err != nil {
		t.Fatalf("LowerAt: %v", err)
	}
	filter, ok := plan.(*chplan.Filter)
	if !ok {
		t.Fatalf("plan is %T, want *chplan.Filter", plan)
	}
	if !filter.Histogram {
		t.Fatalf("Filter.Histogram = false, want true")
	}
	if shape := chplan.RowShapeOf(plan); shape != chplan.HistogramRowShape {
		t.Fatalf("RowShapeOf(plan) = %s, want histogram", shape)
	}
}

// TestLower_ExpHistogram_TopKBottomKUnaffected pins that topk/bottomk's
// existing histogram-DROPPING treatment (histogram_native_drop_
// aggregation.go) is unchanged by #2518's fix: a purely histogram-valued
// input still folds to an empty float-shaped result, never a
// chplan.TopK.Histogram-marked plan — only limitk/limit_ratio preserve
// the histogram shape.
func TestLower_ExpHistogram_TopKBottomKUnaffected(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	for _, query := range []string{`topk(3, latency_exp_hist)`, `bottomk(3, latency_exp_hist)`} {
		query := query
		t.Run(query, func(t *testing.T) {
			t.Parallel()

			expr := parseExprExp(t, query)
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt(%q): %v", query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
				t.Fatalf("LowerAt(%q) plan publishes %s, want sample (empty fold) — topk/bottomk must keep dropping histogram samples", query, shape)
			}
		})
	}
}
