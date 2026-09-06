package engine

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
)

// searchTraceLimitTestPlan builds a minimal SearchTraceLimit-shaped plan —
// the plain-search row source (a bare Scan) wrapped by SearchTraceLimit, the
// exact shape internal/traceql/search_limit.go's stampSearchTraceLimit
// produces for a LIMIT-bounded /api/search request.
func searchTraceLimitTestPlan() chplan.Node {
	return &chplan.SearchTraceLimit{
		Input:           &chplan.Scan{Table: "otel_traces"},
		TraceIDColumn:   "TraceId",
		TimestampColumn: "Timestamp",
		TraceLimit:      20,
	}
}

// TestPlanHasSearchTraceLimit pins the positive and negative cases: a plan
// carrying a chplan.SearchTraceLimit node (even nested below the root, the
// way a `| select(...)` wrap or similar would place it) is detected, and an
// unrelated plan is not.
func TestPlanHasSearchTraceLimit(t *testing.T) {
	t.Parallel()

	t.Run("bare SearchTraceLimit root", func(t *testing.T) {
		t.Parallel()
		if !planHasSearchTraceLimit(searchTraceLimitTestPlan()) {
			t.Fatal("planHasSearchTraceLimit == false for a bare SearchTraceLimit plan")
		}
	})

	t.Run("nested below a Project", func(t *testing.T) {
		t.Parallel()
		plan := &chplan.Project{Input: searchTraceLimitTestPlan()}
		if !planHasSearchTraceLimit(plan) {
			t.Fatal("planHasSearchTraceLimit == false for a SearchTraceLimit nested below a Project")
		}
	})

	t.Run("no SearchTraceLimit node", func(t *testing.T) {
		t.Parallel()
		plan := &chplan.Project{Input: &chplan.Scan{Table: "otel_traces"}}
		if planHasSearchTraceLimit(plan) {
			t.Fatal("planHasSearchTraceLimit == true for a plan with no SearchTraceLimit node")
		}
	})
}

// TestExecContext_SearchTraceLimit_StampsFanoutMultiplier is the direct
// regression test for cerberus issue #3128 round 4: a SearchTraceLimit-shaped
// plan must carry chclient.WithDataShardFanoutMultiplier(ctx,
// searchTraceLimitFanoutMultiplier) after execContext runs, and an unrelated
// plan must NOT carry it at all — the pre-round-4 default
// (dataShardFanoutMultiplierFromContext's own fallback) must apply
// unchanged.
func TestExecContext_SearchTraceLimit_StampsFanoutMultiplier(t *testing.T) {
	t.Parallel()
	e := &Engine{}

	t.Run("SearchTraceLimit plan", func(t *testing.T) {
		t.Parallel()
		ctx, _ := e.execContext(context.Background(), searchTraceLimitTestPlan(), "tempo", nil)
		got, ok := chclient.DataShardFanoutMultiplierFromContext(ctx)
		if !ok {
			t.Fatal("execContext did not stamp a data-shard fanout multiplier for a SearchTraceLimit plan")
		}
		if got != searchTraceLimitFanoutMultiplier {
			t.Fatalf("stamped multiplier = %d, want %d", got, searchTraceLimitFanoutMultiplier)
		}
	})

	t.Run("unrelated plan", func(t *testing.T) {
		t.Parallel()
		plan := &chplan.Project{Input: &chplan.Scan{Table: "otel_traces"}}
		ctx, _ := e.execContext(context.Background(), plan, "tempo", nil)
		if _, ok := chclient.DataShardFanoutMultiplierFromContext(ctx); ok {
			t.Fatal("execContext stamped a data-shard fanout multiplier for a plan with no SearchTraceLimit node")
		}
	})
}
