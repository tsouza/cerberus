package chsql_test

import (
	"context"
	"strings"
	"testing"
	"time"

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

// TestRangeWindowMatrixSurfacesTimestampColumn asserts the three
// matrix-shape emitters (rate / increase / delta extrapolation; the
// over-time family via emitWindowedArrayMatrix; deriv / irate via
// emitWindowedArrayPairsMatrix) all project `anchor_ts AS <TimestampColumn>`
// in the outer SELECT. Regression for the "Unknown expression identifier
// 'bucket_ts'" 400 that broke `sum by (X) (rate(metric[5m]))` in range
// mode — the wrapping Aggregate's per-step GROUP BY (injected by
// internal/promql/lower.go's `bucket_ts` branch) references
// `s.TimestampColumn`, which only existed because the inner RangeWindow
// surfaces it under that alias on top of the existing `anchor_ts`.
func TestRangeWindowMatrixSurfacesTimestampColumn(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	step := 30 * time.Second
	rangeDur := 5 * time.Minute

	base := func(fn string) *chplan.RangeWindow {
		return &chplan.RangeWindow{
			Input:           &chplan.Scan{Table: "otel_metrics_sum"},
			Func:            fn,
			Range:           rangeDur,
			Start:           start,
			End:             end,
			Step:            step,
			OuterRange:      end.Sub(start),
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
			GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		}
	}

	cases := []string{
		// extrapolated matrix (counter-reset arithmetic):
		"rate", "increase", "delta",
		// values-only matrix:
		"sum_over_time", "count_over_time", "min_over_time", "max_over_time",
		// pairs matrix:
		"deriv", "irate",
	}

	for _, fn := range cases {
		fn := fn
		t.Run(fn, func(t *testing.T) {
			t.Parallel()
			sql, _, err := chsql.Emit(context.Background(), base(fn))
			if err != nil {
				t.Fatalf("Emit(%s): %v", fn, err)
			}
			// Outer SELECT must surface anchor_ts under the schema's
			// timestamp-column name so a wrapping Aggregate's per-step
			// GROUP BY references resolve.
			want := "anchor_ts AS `TimeUnix`"
			if !strings.Contains(sql, want) {
				t.Errorf("Emit(%s) missing %q in outer SELECT — outer Aggregate "+
					"with `GroupBy: ColumnRef{TimeUnix}` will fail at CH with "+
					"`Unknown expression identifier`.\nSQL: %s", fn, want, sql)
			}
			// The bare `anchor_ts` passthrough must remain — downstream
			// `wrapWithSampleProjection` (api/prom/handler.go) and the
			// histogram/instant-fn callers still read it by that name.
			if !strings.Contains(sql, "`anchor_ts`") {
				t.Errorf("Emit(%s) dropped bare `anchor_ts` column\nSQL: %s", fn, sql)
			}
		})
	}
}
