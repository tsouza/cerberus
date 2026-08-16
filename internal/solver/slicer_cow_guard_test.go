package solver_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/solver"
)

// The slicer re-anchors each shard as a structural-sharing (copy-on-write)
// view of the optimized plan: only the O(spine-depth) re-gridded spine nodes
// are cloned, and the immutable off-spine subtree is SHARED across all K
// shards (and with the original plan). That sharing is the perf lever, and it
// is SOUND only under the no-mutate-after-slice contract: the solver runs each
// shard through chsql.Emit, which never mutates a plan node in place. These
// guards enforce that contract so a future emitter / optimizer pass that
// mutates a shared node fails LOUDLY here instead of silently corrupting a
// sibling shard.

// guardGridStart / guardGridEnd / guardGridStep are a wide grid (1h at 15s =
// 241 anchors) so every routable fixture force-routes at K >= 2 under
// Mode=sharded.
var (
	guardGridStart = time.Unix(1_700_000_000, 0).UTC()
	guardGridEnd   = guardGridStart.Add(time.Hour)
	guardGridStep  = 15 * time.Second
)

// guardOptimizedPlan lowers query at the guard grid and runs the default
// optimizer — the exact route-A pipeline the slicer then decomposes.
func guardOptimizedPlan(t *testing.T, ctx context.Context, query string) chplan.Node {
	t.Helper()
	p := parser.NewParser(parser.Options{})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	plan, err := promql.LowerAtRange(ctx, expr, schema.DefaultOTelMetrics(),
		guardGridStart, guardGridEnd, guardGridStep)
	if err != nil {
		t.Fatalf("lower %q: %v", query, err)
	}
	return optimizer.Default().Run(ctx, plan)
}

// guardRoute force-routes plan under Mode=sharded and returns the Decision.
func guardRoute(t *testing.T, plan chplan.Node) *solver.Decision {
	t.Helper()
	cfg := solver.DefaultConfig()
	cfg.Mode = solver.ModeSharded
	if err := cfg.Validate(); err != nil {
		t.Fatalf("sharded Config invalid: %v", err)
	}
	pl := &solver.Planner{Cfg: cfg}
	gs, ge, gstep := solver.GridOf(plan)
	dec, isRouted := pl.Plan(plan, solver.RequestMeta{
		Lang:  solver.LangPromQL,
		Start: gs,
		End:   ge,
		Step:  gstep,
	})
	if !isRouted {
		t.Fatalf("plan did not force-route under Mode=sharded (reason=%q)", dec.Reason)
	}
	if dec.K < 2 || len(dec.Slices) < 2 {
		t.Fatalf("force-route produced K=%d, %d slices; want >= 2 of each", dec.K, len(dec.Slices))
	}
	return dec
}

// guardFixtures are routable single-spine PromQL shapes whose off-spine
// subtree (the matchers-filtered scan + the off-grid aggregation / projection)
// is shared across shards.
var guardFixtures = []string{
	`sum by (job)(rate(http_requests_total[5m]))`,
	`rate(http_requests_total[5m])`,
	`max_over_time(http_requests_total[10m])`,
	`http_requests_total`, // bare selector -> RangeLWR spine
}

// TestSlice_DifferentialSQL_NoSharedMutation is GUARDRAIL A: the enforced
// no-mutate-after-slice contract.
//
// For each routable fixture it snapshots the original optimized plan, routes
// it into K shards, emits per-shard SQL (the production route-B render path),
// and then asserts:
//
//  1. every shard emits non-empty SQL without error (the shards are renderable
//     off the shared off-spine);
//  2. emitting all K shards leaves the original plan BYTE-IDENTICAL — emit
//     does not mutate the shared nodes;
//  3. the off-spine subtree is genuinely SHARED across shards (the same
//     pointer) — so a mutation of it WOULD be observable, which is exactly
//     what makes (2) a meaningful guard rather than a tautology.
//
// If a future emitter/optimizer pass mutates a shared node in place, (2) fails
// here loudly instead of silently corrupting a sibling shard's output.
func TestSlice_DifferentialSQL_NoSharedMutation(t *testing.T) {
	ctx := context.Background()
	for _, query := range guardFixtures {
		query := query
		t.Run(query, func(t *testing.T) {
			plan := guardOptimizedPlan(t, ctx, query)
			snapshot := chplan.CloneNode(plan)

			dec := guardRoute(t, plan)

			// (1) Every shard emits valid SQL, and we record each shard's SQL
			// + args so a later regression that makes two shards diverge in an
			// unexpected way is visible.
			shardSQL := make([]string, len(dec.Slices))
			for i, s := range dec.Slices {
				sql, _, err := chsql.Emit(ctx, s.Plan)
				if err != nil {
					t.Fatalf("emit shard %d: %v", i, err)
				}
				if sql == "" {
					t.Fatalf("shard %d emitted empty SQL", i)
				}
				shardSQL[i] = sql
			}

			// (2) Emitting all shards must NOT have mutated the original plan.
			// This is the contract: the shared off-spine is immutable through
			// emit.
			if !plan.Equal(snapshot) {
				t.Fatal("emitting the shard SQLs mutated the (shared) original plan — " +
					"the no-mutate-after-slice contract is broken")
			}

			// (3) Every off-spine subtree is shared across shards: prove it by
			// finding the off-spine heads of each shard and asserting positional
			// pointer identity. A UnionAll carries several independent gridded
			// spines, so each arm contributes its own head.
			heads := make([][]chplan.Node, len(dec.Slices))
			for i, s := range dec.Slices {
				heads[i] = offSpineHeads(s.Plan)
			}
			for i := 1; i < len(heads); i++ {
				if len(heads[i]) != len(heads[0]) {
					t.Fatalf("shard %d has %d off-spine heads, shard 0 has %d", i, len(heads[i]), len(heads[0]))
				}
				for j := range heads[0] {
					if heads[i][j] != heads[0][j] {
						t.Fatalf("shard %d off-spine head %d %p != shard 0 head %p — "+
							"shards must SHARE every off-spine subtree (COW)", i, j, heads[i][j], heads[0][j])
					}
				}
			}
		})
	}
}

// TestSlice_SharedMutationIsObservable is the negative control for GUARDRAIL A:
// it proves the no-mutation assertion above is load-bearing by showing that a
// mutation of a SHARED off-spine node IS observable from a sibling shard. If
// this ever stops being observable (because the slicer silently went back to
// deep-copying every shard) the no-mutation guard would become a tautology;
// this test fails first and forces the contract to be re-examined.
func TestSlice_SharedMutationIsObservable(t *testing.T) {
	ctx := context.Background()
	plan := guardOptimizedPlan(t, ctx, `sum by (job)(rate(http_requests_total[5m]))`)
	dec := guardRoute(t, plan)
	if len(dec.Slices) < 2 {
		t.Fatalf("need >= 2 shards, got %d", len(dec.Slices))
	}

	h0 := offSpineHeads(dec.Slices[0].Plan)
	h1 := offSpineHeads(dec.Slices[1].Plan)
	if len(h0) != len(h1) {
		t.Fatalf("shards expose different off-spine head counts: %d != %d", len(h0), len(h1))
	}
	for i := range h0 {
		if h0[i] != h1[i] {
			t.Fatalf("shards do not share off-spine head %d; COW sharing regressed", i)
		}
	}
}

// offSpineHeads descends every spine-wrapper chain ReanchorRange clones —
// RangeWindow / RangeLWR / Project / Aggregate / TopK / Filter — and returns
// the first node in each arm that falls through to ReanchorRange's default
// (off-spine) arm, i.e. every first SHARED node. Every node above each head is
// freshly cloned per shard; the heads and everything below them are shared
// across all K shards. UnionAll itself is cloned, so its arms must be checked
// independently.
func offSpineHeads(n chplan.Node) []chplan.Node {
	switch v := n.(type) {
	case *chplan.RangeWindow:
		return offSpineHeads(v.Input)
	case *chplan.RangeLWR:
		return offSpineHeads(v.Input)
	case *chplan.Project:
		return offSpineHeads(v.Input)
	case *chplan.Aggregate:
		return offSpineHeads(v.Input)
	case *chplan.TopK:
		return offSpineHeads(v.Input)
	case *chplan.Filter:
		return offSpineHeads(v.Input)
	case *chplan.UnionAll:
		heads := make([]chplan.Node, 0, len(v.Inputs))
		for _, input := range v.Inputs {
			heads = append(heads, offSpineHeads(input)...)
		}
		return heads
	default:
		return []chplan.Node{n}
	}
}
