package engine

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// tempoSearchRecentPlan builds the exact shape internal/api/tempo
// handler.go's /search/recent constructs: Limit(OrderBy(Filter(Scan))) —
// see eligibleForLazyMaterialization's doc for why the bare Limit(OrderBy(...))
// shape, not the full Project wrap, is what the eligibility check matches.
func tempoSearchRecentPlan(limit int64) chplan.Node {
	return &chplan.Limit{
		Count: limit,
		Input: &chplan.OrderBy{
			Input: &chplan.Filter{
				Input:     &chplan.Scan{Table: "otel_traces"},
				Predicate: &chplan.LitString{V: "x"},
			},
			Keys: []chplan.OrderKey{
				{Expr: &chplan.ColumnRef{Name: "Timestamp"}, Desc: true},
			},
		},
	}
}

// TestApply_StampsLazyMaterialization_OnOrderByLimitShape confirms that with
// the LazyMaterialization rule on (which only resolves in on server >= 25.11)
// and a plan carrying the Tempo search Limit(OrderBy(...)) shape, apply stamps
// both settings sized to the plan's own LIMIT, plus the analyzer co-stamp.
func TestApply_StampsLazyMaterialization_OnOrderByLimitShape(t *testing.T) {
	plan := tempoSearchRecentPlan(20)
	ctx := SettingsRules{LazyMaterialization: true}.apply(context.Background(), plan)
	if got := settingValue(ctx, settingQueryPlanOptimizeLazyMaterialization); got != 1 {
		t.Errorf("query_plan_optimize_lazy_materialization = %v; want 1", got)
	}
	if got := settingValue(ctx, settingQueryPlanMaxLimitForLazyMaterialization); got != int64(20) {
		t.Errorf("query_plan_max_limit_for_lazy_materialization = %v; want 20 (the plan's own LIMIT)", got)
	}
	if got := settingValue(ctx, settingEnableAnalyzer); got != 1 {
		t.Errorf("enable_analyzer = %v; want 1 (analyzer-gated co-stamp)", got)
	}
}

// TestApply_LazyMaterialization_SizesKnobToRequestLimit confirms the knob
// tracks whatever LIMIT the plan actually carries, not a fixed constant — a
// live chDB 26.5 probe showed a knob BELOW the query's LIMIT silently falls
// back to eager reads, so a fixed ceiling would stop helping once a caller's
// limit grew past it.
func TestApply_LazyMaterialization_SizesKnobToRequestLimit(t *testing.T) {
	for _, limit := range []int64{1, 20, 200} {
		plan := tempoSearchRecentPlan(limit)
		ctx := SettingsRules{LazyMaterialization: true}.apply(context.Background(), plan)
		if got := settingValue(ctx, settingQueryPlanMaxLimitForLazyMaterialization); got != limit {
			t.Errorf("limit=%d: query_plan_max_limit_for_lazy_materialization = %v; want %d", limit, got, limit)
		}
	}
}

// TestApply_LazyMaterialization_OffByDefault confirms the rule is DARK when
// the feature did not resolve in (e.g. server < 25.11): nothing is stamped,
// even on an eligible plan shape.
func TestApply_LazyMaterialization_OffByDefault(t *testing.T) {
	plan := tempoSearchRecentPlan(20)
	off := SettingsRules{}.apply(context.Background(), plan)
	if got := settingValue(off, settingQueryPlanOptimizeLazyMaterialization); got != nil {
		t.Errorf("LazyMaterialization off: setting = %v; want absent", got)
	}
	if got := settingValue(off, settingQueryPlanMaxLimitForLazyMaterialization); got != nil {
		t.Errorf("LazyMaterialization off: limit setting = %v; want absent", got)
	}
}

// TestEligibleForLazyMaterialization covers the plan-shape gate directly:
// Limit(OrderBy(...)) with a positive count qualifies; a bare Limit(Scan) (the
// shape the deleted late_mat.go rewrite matched), a Limit with Count<=0, and a
// plan with two independent Limit(OrderBy(...)) shapes (no single Count to
// size the knob to) do not.
func TestEligibleForLazyMaterialization(t *testing.T) {
	cases := []struct {
		name      string
		plan      chplan.Node
		wantOK    bool
		wantLimit int64
	}{
		{
			name:      "Limit(OrderBy(Filter(Scan))) qualifies",
			plan:      tempoSearchRecentPlan(20),
			wantOK:    true,
			wantLimit: 20,
		},
		{
			name: "Limit(OrderBy(Scan)) qualifies (no Filter)",
			plan: &chplan.Limit{
				Count: 5,
				Input: &chplan.OrderBy{
					Input: &chplan.Scan{Table: "otel_traces"},
					Keys:  []chplan.OrderKey{{Expr: &chplan.ColumnRef{Name: "Timestamp"}, Desc: true}},
				},
			},
			wantOK:    true,
			wantLimit: 5,
		},
		{
			name: "bare Limit(Scan), no OrderBy: does NOT qualify",
			plan: &chplan.Limit{
				Count: 20,
				Input: &chplan.Scan{Table: "otel_traces"},
			},
			wantOK: false,
		},
		{
			name: "Limit(OrderBy(...)) with Count<=0: does NOT qualify",
			plan: &chplan.Limit{
				Count: 0,
				Input: &chplan.OrderBy{
					Input: &chplan.Scan{Table: "otel_traces"},
					Keys:  []chplan.OrderKey{{Expr: &chplan.ColumnRef{Name: "Timestamp"}, Desc: true}},
				},
			},
			wantOK: false,
		},
		{
			name: "two independent Limit(OrderBy(...)) shapes: ambiguous, does NOT qualify",
			plan: &chplan.CrossJoin{
				Left:  tempoSearchRecentPlan(20),
				Right: tempoSearchRecentPlan(5),
			},
			wantOK: false,
		},
		{
			name:   "no Limit anywhere: does NOT qualify",
			plan:   &chplan.Scan{Table: "otel_traces"},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limit, ok := eligibleForLazyMaterialization(tc.plan)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v; want %v", ok, tc.wantOK)
			}
			if ok && limit != tc.wantLimit {
				t.Errorf("limit = %d; want %d", limit, tc.wantLimit)
			}
		})
	}
}
