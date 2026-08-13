package solver

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
)

// routeBMismatchedResponseShape is a response shape that is NOT
// chclient.ResponseShapeMatrix — the Loki log-stream pivot key an adapter
// really does declare (internal/logql/lang.go). chclient's bindColumns
// declines the columnar matrix decode for exactly this input
// (chclient/columnar_shape_test.go pins that half), so a test that proves
// this value reaches a shard's QueryCursor ctx proves the AND-gate has the
// input it needs to fire on route B.
const routeBMismatchedResponseShape = "loki-streams"

// responseShapeOfShards reads back the ResponseShape each shard's
// QueryCursor call actually observed on its own ctx, failing the test if any
// shard never opened a cursor at all (which would make an "every shard
// carries the shape" assertion vacuously true).
func responseShapeOfShards(t *testing.T, q *fakeQuerier, k int) []string {
	t.Helper()
	q.mu.Lock()
	defer q.mu.Unlock()
	shapes := make([]string, k)
	for shard := 0; shard < k; shard++ {
		shardCtx, ok := q.ctxByShard[shard]
		if !ok {
			t.Fatalf("shard %d never opened a cursor — nothing to assert about its ctx", shard)
		}
		shapes[shard] = chclient.ResponseShapeFromContext(shardCtx)
	}
	return shapes
}

// runShardsWithResponseShape dispatches a k-shard routed Decision through a
// real Executor whose ctx carries shape (when non-empty), drains the composed
// cursor to completion so every shard has certainly opened, and returns the
// per-shard observed shapes.
func runShardsWithResponseShape(t *testing.T, shape string, k int) []string {
	t.Helper()

	q := newFakeQuerier(2)
	cfg := testCfg()
	cfg.Parallel = k // kEff == k, so all k shards really do open a cursor
	x := newExec(q, newFakeEmitter(), cfg, 32, newFakeBreaker(BreakerClosed), nil)

	ctx := context.Background()
	if shape != "" {
		ctx = chclient.WithResponseShape(ctx, shape)
	}
	cur, _, err := x.Execute(ctx, "promql", makeDecision(k), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer cur.Close()
	if _, err := drainAll(cur); err != nil {
		t.Fatalf("drain: %v", err)
	}
	return responseShapeOfShards(t, q, k)
}

// TestExecute_ResponseShapeReachesEveryShardCursor is the route-B half of
// #1429's defense-in-depth wiring (#1753). The engine stamps the adapter's
// declared ResponseShape onto the ctx it hands Execute; Execute derives a
// cancel-cause ctx, a breaker-dedup ctx and an errgroup ctx from it, and
// runShard then re-stamps a per-shard progress recorder, query_id, sample
// budget and memory setting onto its own derived ctx. This pins that the
// declared shape survives that whole chain to EVERY shard's QueryCursor —
// the ordinary-ctx-nesting claim the fix rests on, which is precisely what a
// future refactor rebuilding a shard ctx from scratch would silently break.
func TestExecute_ResponseShapeReachesEveryShardCursor(t *testing.T) {
	t.Parallel()

	const k = 4
	for shard, got := range runShardsWithResponseShape(t, chclient.ResponseShapeMatrix, k) {
		if got != chclient.ResponseShapeMatrix {
			t.Errorf("shard %d: ResponseShapeFromContext(shard ctx) = %q, want %q — the engine's declared shape never reached this shard's CursorQuerier",
				shard, got, chclient.ResponseShapeMatrix)
		}
	}
}

// TestExecute_MismatchedResponseShapeReachesEveryShardCursor is the pin that
// the AND-gate can actually FIRE on route B, not merely that a matching shape
// rides along harmlessly: a non-matrix declaration must arrive at every
// shard's QueryCursor ctx VERBATIM, since that is the single input
// chclient.bindColumns tests to decline the columnar matrix decode. If the
// value were dropped anywhere in the Execute -> errgroup -> runShard chain
// the shards would present "" instead, which the gate reads as "unknown,
// defer to the structural test alone" — the exact hollow-gate outcome #1753
// exists to close.
func TestExecute_MismatchedResponseShapeReachesEveryShardCursor(t *testing.T) {
	t.Parallel()

	const k = 3
	for shard, got := range runShardsWithResponseShape(t, routeBMismatchedResponseShape, k) {
		if got != routeBMismatchedResponseShape {
			t.Errorf("shard %d: ResponseShapeFromContext(shard ctx) = %q, want %q — a mismatched shape must reach the gate, or route B's AND-gate can never decline",
				shard, got, routeBMismatchedResponseShape)
		}
	}
}

// TestExecute_UnsetResponseShapeStaysEmptyOnEveryShard is the negative
// pairing: a caller that never declared a shape must leave every shard ctx
// observably EMPTY, never a stale or defaulted value. "" is chclient's
// incremental-adoption safety valve ("unknown, defer to the structural check
// alone"), so inventing a value here would change what the columnar decode
// accepts for not-yet-migrated callers.
func TestExecute_UnsetResponseShapeStaysEmptyOnEveryShard(t *testing.T) {
	t.Parallel()

	const k = 2
	for shard, got := range runShardsWithResponseShape(t, "", k) {
		if got != "" {
			t.Errorf("shard %d: ResponseShapeFromContext(shard ctx) = %q, want \"\" for a caller that never declared a shape", shard, got)
		}
	}
}
