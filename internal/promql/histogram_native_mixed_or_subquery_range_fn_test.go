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

// TestLower_ExpHistogram_MixedOrSubqueryOuterFn pins cerberus issue #2577:
// every one of the fifteen SELECT/FOLD-family names
// TestLower_ExpHistogram_SelectFamilyOverSubquery (histogram_native_subquery_select_test.go)
// and its FOLD-family sibling already pin for a PURE histogram-native
// subquery inner also lowers — without error, and to the correct
// [chplan.RowShapeOf] — over a subquery whose own inner is instead a
// mixed float/histogram `or` ([mixedOrSubqueryOuterFn],
// histogram_native_mixed_or_subquery_range_fn.go).
//
// The six always-float-output names (count_over_time, present_over_time,
// resets, changes, ts_of_first_over_time, ts_of_last_over_time) publish a
// plain [chplan.SampleRowShape] — both arms already agree in shape, so no
// Mixed construction is needed. The remaining nine (the two
// histogram-preserving SELECT names plus the whole FOLD family) publish
// [chplan.MixedRowShape]: the histogram arm folds to a
// [chplan.HistogramRowShape] row and the float arm to a
// [chplan.SampleRowShape] row, unioned by the SAME
// [chplan.VectorSetOp]-with-Mixed construction [mixedExpHistogramSetOp] /
// [lowerMixedExpHistogramSetOp] build for a bare `(a) or (b)`.
func TestLower_ExpHistogram_MixedOrSubqueryOuterFn(t *testing.T) {
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

			instant := fmt.Sprintf(`%s(((latency_exp_hist) or (num_cpus))[5m:1m])`, tc.fn)
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

			pinned := fmt.Sprintf(`%s(((latency_exp_hist) or (num_cpus))[5m:1m] @ %s)`, tc.fn, pinnedAt)
			pexpr := parseExprExp(t, pinned)
			pplan, err := promql.LowerAtRange(context.Background(), pexpr, s, start, end, time.Minute)
			if err != nil {
				t.Fatalf("lower(%q) pinned: %v", pinned, err)
			}
			if got := chplan.RowShapeOf(pplan); got != wantShape {
				t.Errorf("lower(%q) pinned RowShape = %s, want %s", pinned, got, wantShape)
			}

			// Nested under a further wrapper (label_replace) — the shape
			// this issue's own evidence queries stress, and #2569's own
			// pure-histogram sibling pins for the same reason: the
			// composition must work through the generic lower() path, not
			// only at the query root.
			wrapped := fmt.Sprintf(`label_replace(%s, "extra", "yes", "", "")`, instant)
			wexpr := parseExprExp(t, wrapped)
			wplan, err := promql.LowerAt(context.Background(), wexpr, s, end, end)
			if err != nil {
				t.Fatalf("lower(%q) label_replace-wrapped: %v", wrapped, err)
			}
			if got := chplan.RowShapeOf(wplan); got != wantShape {
				t.Errorf("lower(%q) label_replace-wrapped RowShape = %s, want %s", wrapped, got, wantShape)
			}
		})
	}
}
