package engine

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/solver"
)

// tsGridRouteBGrid is the shard grid the fixtures below sit on. The bounds are
// arbitrary — nothing in this file reads them back — but they are pinned so a
// shard plan is a realistic re-anchored node rather than a zero-valued one.
var (
	tsGridRouteBStart = time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC)
	tsGridRouteBEnd   = tsGridRouteBStart.Add(30 * time.Minute)
	tsGridRouteBStep  = 15 * time.Second
)

// tsGridRouteBDecision builds a two-shard routed Decision whose shard plans are
// each `wrap(<leaf>)`. It stands in for what the Planner hands the Executor: the
// setting rule must read THESE trees, since they are what gets emitted.
func tsGridRouteBDecision(wrap func() chplan.Node) *solver.Decision {
	return &solver.Decision{
		Strategy: solver.StrategyShardedTimeslice,
		K:        2,
		Reason:   solver.ReasonRouted,
		Slices: []solver.Slice{
			{Index: 0, Start: tsGridRouteBStart, End: tsGridRouteBEnd, Plan: wrap()},
			{Index: 1, Start: tsGridRouteBStart, End: tsGridRouteBEnd, Plan: wrap()},
		},
	}
}

func tsGridRouteBNativeShard() chplan.Node {
	return &chplan.Aggregate{
		Input: &chplan.RangeWindowGridNative{
			Input:           &chplan.Scan{Table: "otel_metrics_sum"},
			Func:            "rate",
			Range:           5 * time.Minute,
			Step:            tsGridRouteBStep,
			Start:           tsGridRouteBStart,
			End:             tsGridRouteBEnd,
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
		},
		AggFuncs: []chplan.AggFunc{{Fn: chplan.FnSum, Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}}},
	}
}

func tsGridRouteBFanoutShard() chplan.Node {
	return &chplan.Aggregate{
		Input: &chplan.RangeWindow{
			Input:           &chplan.Scan{Table: "otel_metrics_sum"},
			Func:            "rate",
			Range:           5 * time.Minute,
			Step:            tsGridRouteBStep,
			OuterRange:      tsGridRouteBEnd.Sub(tsGridRouteBStart),
			Start:           tsGridRouteBStart,
			End:             tsGridRouteBEnd,
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
		},
		AggFuncs: []chplan.AggFunc{{Fn: chplan.FnSum, Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}}},
	}
}

// TestRouteBExecCtx_StampsTSGridSettingForNativeShards pins that route B
// carries route A's per-plan experimental-aggregate rule.
//
// `timeSeries*ToGrid` is an experimental ClickHouse aggregate: without
// `allow_experimental_time_series_aggregate_functions=1` the server answers code
// 63 on EVERY shard, so a routed native plan fails outright rather than
// degrading. Route A stamps it in execContext; route B's dispatch ctx is built
// somewhere else entirely, and until RangeWindowGridNative joined the routable spine
// family (issue #2117) no route-B dispatch could carry the family, so the two
// paths were never both required to know the rule.
//
// The three cases are the three shapes that matter, and each fails differently
// if the rule is wrong: the native shard (missing setting -> code 63), the
// Expr-embedded interior (the WalkDeep case chplan.Walk cannot see, which the
// scalar-anchor-compatibility check now admits into a ROUTED plan), and the
// plain fan-out shard (a stray experimental setting on an unrelated query,
// which can itself error on an older server).
func TestRouteBExecCtx_StampsTSGridSettingForNativeShards(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		wrap  func() chplan.Node
		stamp bool
	}{
		{name: "native shard spine", wrap: tsGridRouteBNativeShard, stamp: true},
		{
			// The ts-grid node hangs off an Expr slot (a per-step scalar
			// parameter's ScalarSubquery), which chplan.Walk does not follow.
			// planHasTSGridNative uses WalkDeep precisely for this.
			name: "native only inside a scalar interior",
			wrap: func() chplan.Node {
				return &chplan.Filter{
					Input: tsGridRouteBFanoutShard(),
					Predicate: &chplan.Binary{
						Op:    chplan.OpGt,
						Left:  &chplan.ColumnRef{Name: "Value"},
						Right: &chplan.ScalarSubquery{Input: tsGridRouteBNativeShard()},
					},
				}
			},
			stamp: true,
		},
		{name: "fan-out shard spine", wrap: tsGridRouteBFanoutShard, stamp: false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := routeBExecCtx(context.Background(), "promql",
				chclient.ResponseShapeMatrix, tsGridRouteBDecision(tc.wrap), 0, false, ResourceBoundOverrides{}, 0, 0, nil, nil)

			settings := chclient.QuerySettingsFromContext(ctx)
			_, got := settings[chclient.SettingExperimentalTSGridAggregate]
			if got != tc.stamp {
				t.Fatalf("%s present = %v, want %v — a missing setting makes every shard "+
					"answer ClickHouse code 63; a stray one rides a query that does not need it",
					chclient.SettingExperimentalTSGridAggregate, got, tc.stamp)
			}

			// The pre-existing route-B ctx values must survive the addition:
			// the AND-gate for the columnar matrix decode reads the response
			// shape off this same ctx.
			if shape := chclient.ResponseShapeFromContext(ctx); shape != chclient.ResponseShapeMatrix {
				t.Errorf("ResponseShape = %q, want %q", shape, chclient.ResponseShapeMatrix)
			}
		})
	}
}

// TestRouteBExecCtx_NilDecisionStampsNothing pins the degenerate input. A nil
// Decision reaches this seam only from a wiring bug, and inventing an
// experimental setting for a dispatch nobody classified would put the knob on a
// query that may not tolerate it.
func TestRouteBExecCtx_NilDecisionStampsNothing(t *testing.T) {
	t.Parallel()

	ctx := routeBExecCtx(context.Background(), "promql", chclient.ResponseShapeMatrix, nil, 0, false, ResourceBoundOverrides{}, 0, 0, nil, nil)
	if settings := chclient.QuerySettingsFromContext(ctx); len(settings) != 0 {
		t.Fatalf("nil decision stamped settings %v", settings)
	}
}
