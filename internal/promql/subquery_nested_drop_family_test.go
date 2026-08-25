package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_DropFamilyEmptyOverNestedSubquery pins a
// pre-release audit finding: lowerSubqueryOverCallSubquery
// (internal/promql/subquery.go, the `<outer-fn>(<inner-sub>)[<outer-
// range>:<step>]` shape, e.g. `max_over_time(rate(m[1m])[5m:30s])[1h:5m]`)
// built its RangeWindow directly over lowerSubquery's output with NO
// RowShapeOf guard — unlike its one-level-shallower sibling
// lowerOuterRangeFnOverSubquery, which guards exactly this. A drop-family
// range-vector fn (max_over_time and friends — see
// histogramSubqueryFloatOnlyDropFunc) over a bare-histogram-selector
// subquery, itself nested in another subquery
// (`max_over_time((latency_exp_hist)[5m:1m])[1h:5m]`), resolved to a
// HistogramRowShape node and the outer RangeWindow referenced
// TimestampColumn/ValueColumn that emitHistogramProjection never emits —
// a ClickHouse "Unknown identifier" 502 instead of the clean empty-drop
// the one-level-shallower composition
// (`max_over_time((latency_exp_hist)[5m:1m])`, pinned by
// TestLower_ExpHistogram_DropFamilyEmptyOverSubquery) already gets.
func TestLower_ExpHistogram_DropFamilyEmptyOverNestedSubquery(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)

	for _, tc := range []struct {
		name  string
		query string
	}{
		{name: "max_over_time", query: `max_over_time((latency_exp_hist)[5m:1m])[1h:5m]`},
		{name: "min_over_time", query: `min_over_time((latency_exp_hist)[5m:1m])[1h:5m]`},
		{name: "deriv", query: `deriv((latency_exp_hist)[5m:1m])[1h:5m]`},
		{name: "quantile_over_time", query: `quantile_over_time(0.5, (latency_exp_hist)[5m:1m])[1h:5m]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr := parseExprExp(t, tc.query)

			plan, err := promql.LowerAt(context.Background(), expr, s, end, end)
			if err != nil {
				t.Fatalf("lower(%q) instant: %v", tc.query, err)
			}
			if got := chplan.RowShapeOf(plan); got != chplan.SampleRowShape {
				t.Errorf("lower(%q) instant RowShape = %s, want %s", tc.query, got, chplan.SampleRowShape)
			}

			// The bug this test pins produced a plan that lowered
			// successfully (no error at LOWER time) but referenced
			// nonexistent columns at EMIT time — a ClickHouse "Unknown
			// identifier" 502 at the emitter, not the lowering. Confirm the
			// fix reaches an emittable plan, not merely that Lower() itself
			// didn't error.
			if _, _, err := chsql.Emit(context.Background(), plan); err != nil {
				t.Errorf("Emit(%q) instant: %v", tc.query, err)
			}

			rplan, err := promql.LowerAtRange(context.Background(), expr, s, start, end, time.Minute)
			if err != nil {
				t.Fatalf("lower(%q) range: %v", tc.query, err)
			}
			if got := chplan.RowShapeOf(rplan); got != chplan.SampleRowShape {
				t.Errorf("lower(%q) range RowShape = %s, want %s", tc.query, got, chplan.SampleRowShape)
			}
			if _, _, err := chsql.Emit(context.Background(), rplan); err != nil {
				t.Errorf("Emit(%q) range: %v", tc.query, err)
			}
		})
	}
}
