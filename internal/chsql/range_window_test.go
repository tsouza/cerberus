package chsql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// TestRangeWindowGapFunctionsEmit asserts the per-function value
// expression for the five compatibility-lane gaps closed in this PR:
//
//   - absent_over_time → if(length(window_vals) > 0, nan, 1.0)
//   - stdvar_over_time → arrayReduce('varPop', window_vals)
//   - deriv            → tupleElement(arrayReduce('simpleLinearRegression', xs, ys), 1)
//   - resets           → count of adjacent pairs where curr < prev
//   - changes          → count of adjacent pairs where curr != prev
//
// The substring assertions check the bits the new switch cases add;
// the surrounding windowed-array skeleton (groupArray + arrayFilter
// + ts-pair fan-out) is covered by the existing rate / increase /
// delta fixtures and is intentionally NOT re-asserted here.
func TestRangeWindowGapFunctionsEmit(t *testing.T) {
	t.Parallel()

	base := func(fn string) *chplan.RangeWindow {
		return &chplan.RangeWindow{
			Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
			Func:            fn,
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
			GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		}
	}

	cases := []struct {
		name        string
		fn          string
		wantSubstrs []string
	}{
		{
			name: "absent_over_time",
			fn:   "absent_over_time",
			wantSubstrs: []string{
				// Per-series guard: empty window → 1.0, populated window → nan.
				"if(length(window_vals) > 0, nan, 1.0)",
				// minWindowSize=0 — empty windows DO produce a row;
				// the outer SELECT must NOT carry a WHERE filter.
				// Spot-check that no `WHERE length(...)` clause appears.
			},
		},
		{
			name: "stdvar_over_time",
			fn:   "stdvar_over_time",
			wantSubstrs: []string{
				// Population variance — divides squared deviations by N
				// (matches Prom's Welford estimator). CH's `varPop`
				// aggregate is the matching kernel.
				"arrayReduce('varPop', window_vals)",
				// Empty-window drop in outer SELECT.
				"length(`window_vals`) >= 1",
			},
		},
		{
			name: "deriv",
			fn:   "deriv",
			wantSubstrs: []string{
				// Slope (tupleElement index 1) of the linear regression
				// over (seconds-from-anchor, value) pairs.
				"tupleElement(arrayReduce('simpleLinearRegression'",
				", 1)",
				// Uses `window_pairs` directly (the pairs-path emitter)
				// so the per-sample timestamps are available for the
				// dateDiff x-axis projection.
				"dateDiff('second'",
				"length(`window_pairs`) >= 2",
			},
		},
		{
			name: "resets",
			fn:   "resets",
			wantSubstrs: []string{
				// Reset detection: per-adjacent-pair `if(c < p, 1, 0)`.
				"if(c < p, 1, 0)",
				// arraySum reduces the indicator array to a scalar.
				"arraySum(arrayMap((p, c) -> if(c < p, 1, 0), arrayPopBack(window_vals), arrayPopFront(window_vals)))",
				// Float64 wire type for the projected Value column.
				"CAST(arraySum",
				"AS Float64)",
			},
		},
		{
			name: "changes",
			fn:   "changes",
			wantSubstrs: []string{
				// Change detection: per-adjacent-pair `if(c != p, 1, 0)`.
				"if(c != p, 1, 0)",
				"arraySum(arrayMap((p, c) -> if(c != p, 1, 0), arrayPopBack(window_vals), arrayPopFront(window_vals)))",
				"CAST(arraySum",
				"AS Float64)",
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan := base(tc.fn)
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%s): %v", tc.fn, err)
			}
			for _, want := range tc.wantSubstrs {
				if !strings.Contains(sql, want) {
					t.Errorf("Emit(%s) missing substring %q\nSQL: %s", tc.fn, want, sql)
				}
			}
		})
	}
}

// TestRangeWindowAbsentOverTimeNoWindowFilter pins the minWindowSize=0
// contract for absent_over_time: the outer SELECT must NOT carry a
// `WHERE length(window_vals) >= N` filter, because the entire point of
// the function is to project a value when the window is empty. Every
// other RangeWindow function has the filter on; this test guards that
// inversion.
func TestRangeWindowAbsentOverTimeNoWindowFilter(t *testing.T) {
	t.Parallel()
	plan := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
		Func:            "absent_over_time",
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if strings.Contains(sql, "WHERE length(") {
		t.Errorf("absent_over_time must not filter empty windows out; SQL=%s", sql)
	}
}
