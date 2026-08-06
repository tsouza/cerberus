package solver_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// TestSlice_PreservesAggregateHavingGuard pins that a plan-carried abort
// survives slicing into K shards.
//
// `chplan.Aggregate.Having` is where cerberus plants the aborts that have to
// fire unconditionally — the duplicate-labelset rejection a name-dropping
// range function raises when two source series collapse onto one label set,
// and info()'s tie check. HAVING rather than a SELECT column, because
// ClickHouse's analyzer prunes an unreferenced SELECT expression and would
// prune the throwIf with it.
//
// The slicer re-anchors each shard by cloning the spine and descending
// off-spine subtrees through chplan.CloneNode. When CloneNode's Aggregate arm
// silently dropped Having, route B emitted K shards with no guard at all: the
// same query that route A correctly refused came back 200 with the two series
// merged, and nothing anywhere failed. That is the only failure mode this
// test exists for, so it asserts on the emitted SQL — the artefact the shard
// actually runs — rather than on plan structure.
func TestSlice_PreservesAggregateHavingGuard(t *testing.T) {
	ctx := context.Background()

	// Two metrics matched by one selector, under a name-dropping range
	// function: dropping __name__ collapses them onto one label set, so
	// lowering plants the duplicate-labelset abort.
	const query = `rate({__name__=~"cpu_temp|gpu_temp"}[5m])`

	plan := guardOptimizedPlan(t, ctx, query)

	// Route A must carry the guard first. Without this the shard assertions
	// below could pass on a plan that never had a guard to lose.
	routeASQL, _, err := chsql.Emit(ctx, plan)
	if err != nil {
		t.Fatalf("emit route-A plan: %v", err)
	}
	if !strings.Contains(routeASQL, chplan.DuplicateLabelsetMessage) {
		t.Fatalf("route-A SQL does not carry the duplicate-labelset abort; "+
			"the fixture no longer plants a guard, so the shard assertions "+
			"below would be vacuous. SQL:\n%s", routeASQL)
	}

	dec := guardRoute(t, plan)
	for i, s := range dec.Slices {
		shardSQL, _, err := chsql.Emit(ctx, s.Plan)
		if err != nil {
			t.Fatalf("emit shard %d: %v", i, err)
		}
		if !strings.Contains(shardSQL, chplan.DuplicateLabelsetMessage) {
			t.Errorf("shard %d of %d lost the duplicate-labelset abort — route B "+
				"would answer a query route A refuses. SQL:\n%s", i, len(dec.Slices), shardSQL)
		}
	}
}
