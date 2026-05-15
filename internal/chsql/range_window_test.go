package chsql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// TestRangeWindowGapFunctionsEmit asserts the per-function value
// expression for the four compatibility-lane gaps closed in the
// original five-function batch that this test was carved out of.
// `absent_over_time` graduated to a dedicated chplan.AbsentOverTime
// node (see TestAbsentOverTimeEmit) because the per-series RangeWindow
// shape can't synthesise the matcher-derived label set Prom's
// funcAbsentOverTime emits.
//
//   - stdvar_over_time → two-pass Σ(x-μ)²/N (Welford-equivalent precision)
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
			name: "stdvar_over_time",
			fn:   "stdvar_over_time",
			wantSubstrs: []string{
				// Two-pass population variance: Σ(x − μ)² / N with μ
				// computed as arrayAvg(window_vals) and broadcast via
				// arrayWithConstant so the lambda sees a per-row scalar.
				// Replaces the one-pass `arrayReduce('varPop', ...)`
				// kernel which suffers catastrophic cancellation at
				// value scale 2^31 (issue #400 bucket 1).
				"arrayMap((x, m) -> (x - m) * (x - m), window_vals, arrayWithConstant(length(window_vals), arrayAvg(window_vals)))",
				"arraySum(arrayMap",
				"/ length(window_vals)",
				// Empty-window drop in outer SELECT.
				"length(`window_vals`) >= 1",
			},
		},
		{
			name: "stddev_over_time",
			fn:   "stddev_over_time",
			wantSubstrs: []string{
				// sqrt of the two-pass variance expression.
				"sqrt(arraySum(arrayMap",
				"arrayMap((x, m) -> (x - m) * (x - m), window_vals, arrayWithConstant(length(window_vals), arrayAvg(window_vals)))",
				"/ length(window_vals)",
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
