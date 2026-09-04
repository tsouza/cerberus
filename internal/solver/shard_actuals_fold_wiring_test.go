package solver

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/actuals"
	"github.com/tsouza/cerberus/internal/chclient"
)

// shardActualsFoldOfShards reads back the *chclient.ShardActualsFold each
// shard's QueryCursor call actually observed on its own ctx, failing the
// test if any shard never opened a cursor at all — mirrors
// responseShapeOfShards' own doc for why that would make an "every shard
// carries X" assertion vacuously true.
func shardActualsFoldOfShards(t *testing.T, q *fakeQuerier, k int) []*chclient.ShardActualsFold {
	t.Helper()
	q.mu.Lock()
	defer q.mu.Unlock()
	folds := make([]*chclient.ShardActualsFold, k)
	for shard := 0; shard < k; shard++ {
		shardCtx, ok := q.ctxByShard[shard]
		if !ok {
			t.Fatalf("shard %d never opened a cursor — nothing to assert about its ctx", shard)
		}
		fold, _ := chclient.ShardActualsFoldFromContext(shardCtx)
		folds[shard] = fold
	}
	return folds
}

// TestExecute_WiresSameShardActualsFoldOntoEveryShard pins issue #3033's
// wiring half: a K-shard routed dispatch with actuals capture active must
// thread the SAME *chclient.ShardActualsFold accumulator onto every shard's
// own ctx — not a fresh one per shard, which would defeat the fold entirely
// and reproduce the exact K-way undercount this issue exists to close.
func TestExecute_WiresSameShardActualsFoldOntoEveryShard(t *testing.T) {
	const k = 4
	q := newFakeQuerier(2)
	cfg := testCfg()
	cfg.Parallel = k // kEff == k, so every shard really does open a cursor
	x := newExec(q, newFakeEmitter(), cfg, 32, newFakeBreaker(BreakerClosed), nil)

	tracker := actuals.NewTracker(actuals.DefaultConfig())
	ctx := chclient.WithProgressFor(context.Background(), "promql")
	ctx = chclient.WithActualsCapture(ctx, tracker, "cerb:agg;rw")

	cur, _, err := x.Execute(ctx, "promql", makeDecision(k), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer cur.Close()
	if _, err := drainAll(cur); err != nil {
		t.Fatalf("drain: %v", err)
	}

	folds := shardActualsFoldOfShards(t, q, k)
	first := folds[0]
	if first == nil {
		t.Fatal("expected a non-nil *ShardActualsFold on shard 0's ctx for a K>1 actuals-capturing dispatch")
	}
	for i, f := range folds {
		if f != first {
			t.Fatalf("expected shard %d to carry the SAME fold pointer as shard 0, got a different (or nil) one: %p vs %p", i, f, first)
		}
	}
}

// TestExecute_NoShardActualsFoldForSingleShardDecision pins the k<2 no-op:
// a single-slice Decision (a degenerate route-B dispatch, or any test that
// exercises Execute directly with k==1) must not carry a fold at all — it
// already records correctly without one, and chclient.WithShardActualsFold's
// own doc says as much.
func TestExecute_NoShardActualsFoldForSingleShardDecision(t *testing.T) {
	q := newFakeQuerier(2)
	x := newExec(q, newFakeEmitter(), testCfg(), 32, newFakeBreaker(BreakerClosed), nil)

	tracker := actuals.NewTracker(actuals.DefaultConfig())
	ctx := chclient.WithProgressFor(context.Background(), "promql")
	ctx = chclient.WithActualsCapture(ctx, tracker, "cerb:agg;rw")

	cur, _, err := x.Execute(ctx, "promql", makeDecision(1), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer cur.Close()
	if _, err := drainAll(cur); err != nil {
		t.Fatalf("drain: %v", err)
	}

	folds := shardActualsFoldOfShards(t, q, 1)
	if folds[0] != nil {
		t.Fatal("expected no fold wired onto a single-shard dispatch's ctx")
	}
}

// TestExecute_NoShardActualsFoldWithoutActualsCapture pins the other no-op:
// a routed K-shard dispatch with actuals capture OFF (no WithActualsCapture
// call at all — the overwhelmingly common case, since the feature defaults
// off) must not wire a fold either.
func TestExecute_NoShardActualsFoldWithoutActualsCapture(t *testing.T) {
	const k = 3
	q := newFakeQuerier(2)
	cfg := testCfg()
	cfg.Parallel = k
	x := newExec(q, newFakeEmitter(), cfg, 32, newFakeBreaker(BreakerClosed), nil)

	cur, _, err := x.Execute(context.Background(), "promql", makeDecision(k), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer cur.Close()
	if _, err := drainAll(cur); err != nil {
		t.Fatalf("drain: %v", err)
	}

	folds := shardActualsFoldOfShards(t, q, k)
	for i, f := range folds {
		if f != nil {
			t.Fatalf("expected no fold on shard %d's ctx with actuals capture off", i)
		}
	}
}
