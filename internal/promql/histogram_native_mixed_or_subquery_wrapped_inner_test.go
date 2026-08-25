package promql_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_MixedOrSubqueryOuterFn_WrappedInner pins cerberus
// issue #2581, the narrower sibling of #2577
// (TestLower_ExpHistogram_MixedOrSubqueryOuterFn, this package): every one
// of the fifteen SELECT/FOLD-family names also lowers — without error, and
// to the correct [chplan.RowShapeOf] — when the subquery's own inner is a
// mixed float/histogram `or` DIRECTLY wrapped in label_replace, rather than
// a bare `X or Y` node ([wrapMixedOrSubqueryInner],
// histogram_native_mixed_or_subquery_range_fn.go).
//
// Same wantMixed split as #2577's own test: the six always-float-output
// names publish a plain [chplan.SampleRowShape] (both synthetic arms agree
// in shape); the remaining nine publish [chplan.MixedRowShape].
func TestLower_ExpHistogram_MixedOrSubqueryOuterFn_WrappedInner(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	pinnedAt := "1767225600" // 2026-01-01T00:00:00Z, matches `start`

	for _, tc := range []struct {
		fn        string
		wantMixed bool // histogram-preserving names publish a Mixed union
	}{
		{fn: "count_over_time"},
		{fn: "present_over_time"},
		{fn: "last_over_time", wantMixed: true},
		{fn: "first_over_time", wantMixed: true},
		{fn: "resets"},
		{fn: "changes"},
		{fn: "ts_of_first_over_time"},
		{fn: "ts_of_last_over_time"},
		{fn: "rate", wantMixed: true},
		{fn: "increase", wantMixed: true},
		{fn: "delta", wantMixed: true},
		{fn: "irate", wantMixed: true},
		{fn: "idelta", wantMixed: true},
		{fn: "sum_over_time", wantMixed: true},
		{fn: "avg_over_time", wantMixed: true},
	} {
		t.Run(tc.fn, func(t *testing.T) {
			t.Parallel()
			wantShape := chplan.SampleRowShape
			if tc.wantMixed {
				wantShape = chplan.MixedRowShape
			}

			instant := fmt.Sprintf(
				`%s((label_replace((latency_exp_hist) or (num_cpus), "extra", "yes", "", ""))[5m:1m])`,
				tc.fn,
			)
			expr := parseExprExp(t, instant)
			plan, err := promql.LowerAt(context.Background(), expr, s, end, end)
			if err != nil {
				t.Fatalf("lower(%q) instant: %v", instant, err)
			}
			if got := chplan.RowShapeOf(plan); got != wantShape {
				t.Errorf("lower(%q) instant RowShape = %s, want %s", instant, got, wantShape)
			}

			rplan, err := promql.LowerAtRange(context.Background(), expr, s, start, end, time.Minute)
			if err != nil {
				t.Fatalf("lower(%q) range: %v", instant, err)
			}
			if got := chplan.RowShapeOf(rplan); got != wantShape {
				t.Errorf("lower(%q) range RowShape = %s, want %s", instant, got, wantShape)
			}

			pinned := fmt.Sprintf(
				`%s((label_replace((latency_exp_hist) or (num_cpus), "extra", "yes", "", ""))[5m:1m] @ %s)`,
				tc.fn, pinnedAt,
			)
			pexpr := parseExprExp(t, pinned)
			pplan, err := promql.LowerAtRange(context.Background(), pexpr, s, start, end, time.Minute)
			if err != nil {
				t.Fatalf("lower(%q) pinned: %v", pinned, err)
			}
			if got := chplan.RowShapeOf(pplan); got != wantShape {
				t.Errorf("lower(%q) pinned RowShape = %s, want %s", pinned, got, wantShape)
			}

			// Nested under a further wrapper on the OUTER side too (sum by),
			// the same double-composition #2577's own test pins for its own
			// bare-inner shape: the wrapped-inner recognizer must also work
			// through the generic lower() path when the whole `<fn>(subquery)`
			// call itself is nested, not only at the query root.
			wrapped := fmt.Sprintf(`sum by (series) (%s)`, instant)
			wexpr := parseExprExp(t, wrapped)
			if _, err := promql.LowerAt(context.Background(), wexpr, s, end, end); err != nil {
				t.Fatalf("lower(%q) sum-wrapped: %v", wrapped, err)
			}
		})
	}
}

// TestLower_ExpHistogram_MixedOrSubqueryOuterFn_LabelJoinWrappedInner pins
// the label_join half of the same wrapper family alongside label_replace's
// own dedicated test above — [labelCallOverMixedExpHistogramSetOp] (and by
// extension [wrapMixedOrSubqueryInner]) recognises both names identically,
// this only checks label_join's own distinct arg-count/shape parsing didn't
// regress.
func TestLower_ExpHistogram_MixedOrSubqueryOuterFn_LabelJoinWrappedInner(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	instant := `count_over_time((label_join((latency_exp_hist) or (num_cpus), "extra", ",", "job"))[5m:1m])`
	expr := parseExprExp(t, instant)
	plan, err := promql.LowerAt(context.Background(), expr, s, end, end)
	if err != nil {
		t.Fatalf("lower(%q): %v", instant, err)
	}
	if got := chplan.RowShapeOf(plan); got != chplan.SampleRowShape {
		t.Errorf("lower(%q) RowShape = %s, want %s", instant, got, chplan.SampleRowShape)
	}
}
