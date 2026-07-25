package solver

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestExecute_MandatoryPerShardMemoryApportion pins the safety property a
// route-B fan-out must have: total server-side memory exposure across all K
// shards must never exceed route A's own single-statement cap. Every shard's
// stamped max_memory_usage must equal exactly routeAMemoryCapBytes/kEff —
// not the unapportioned full cap, and unconditionally (no config toggle).
func TestExecute_MandatoryPerShardMemoryApportion(t *testing.T) {
	t.Parallel()

	const routeACapBytes = int64(8_000_000_000) // 8 GB, an arbitrary but realistic cap
	const k = 4

	q := newFakeQuerier(3)
	q.maxMemoryBytes = routeACapBytes
	cfg := testCfg()
	cfg.Parallel = k // kEff = min(K, Parallel, gate/2); pin Parallel so kEff == k unambiguously
	x := newExec(q, newFakeEmitter(), cfg, 32, newFakeBreaker(BreakerClosed), nil)

	cur, _, err := x.Execute(context.Background(), "promql", makeDecision(k), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer cur.Close()
	if _, err := drainAll(cur); err != nil {
		t.Fatalf("drain: %v", err)
	}

	wantPerShard := routeACapBytes / k
	for shard := 0; shard < k; shard++ {
		shardCtx, ok := q.ctxByShard[shard]
		if !ok {
			t.Fatalf("shard %d never opened a cursor", shard)
		}
		settings := chclient.QuerySettingsFromContext(shardCtx)
		got, ok := settings["max_memory_usage"]
		if !ok {
			t.Fatalf("shard %d: no max_memory_usage setting stamped at all", shard)
		}
		gotBytes, ok := got.(int64)
		if !ok {
			t.Fatalf("shard %d: max_memory_usage = %v (%T), want an int64", shard, got, got)
		}
		if gotBytes != wantPerShard {
			t.Errorf("shard %d: max_memory_usage = %d, want routeACapBytes/kEff = %d/%d = %d",
				shard, gotBytes, routeACapBytes, k, wantPerShard)
		}
		// The property this whole mechanism exists to guarantee: no shard's
		// individual cap may exceed route A's cap divided by shard count —
		// otherwise K concurrent shards could together exceed route A's
		// total exposure.
		if gotBytes*k > routeACapBytes {
			t.Errorf("shard %d: max_memory_usage=%d, k=%d => total exposure %d exceeds routeACapBytes=%d",
				shard, gotBytes, k, gotBytes*k, routeACapBytes)
		}
	}
}

// TestExecute_MemoryApportion_DividesByEffectiveConcurrency pins a specific,
// easy-to-get-wrong detail: apportionment must divide by kEff (the clamped
// effective concurrency after the Parallel/gate cap — the actual number of
// shards running CONCURRENTLY and therefore actually competing for server
// memory at once), not by the Decision's raw K. Dividing by K here would
// UNDER-apportion whenever Parallel < K: kEff-many concurrent shards would
// each get more than routeACapBytes/kEff, so their combined exposure could
// exceed routeACapBytes — the exact safety property this mechanism exists
// to hold.
func TestExecute_MemoryApportion_DividesByEffectiveConcurrency(t *testing.T) {
	t.Parallel()

	const routeACapBytes = int64(9_000_000_000)
	const k = 6
	const parallel = 3 // deliberately < k, so kEff clamps to Parallel, not K

	q := newFakeQuerier(2)
	q.maxMemoryBytes = routeACapBytes
	cfg := testCfg()
	cfg.Parallel = parallel
	x := newExec(q, newFakeEmitter(), cfg, 32, newFakeBreaker(BreakerClosed), nil)

	cur, info, err := x.Execute(context.Background(), "promql", makeDecision(k), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer cur.Close()
	if _, err := drainAll(cur); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if info.Parallelism != parallel {
		t.Fatalf("Parallelism = %d, want %d — fixture no longer exercises Parallel < K", info.Parallelism, parallel)
	}

	wantPerShard := routeACapBytes / parallel // NOT routeACapBytes / k
	for shard := 0; shard < k; shard++ {
		shardCtx, ok := q.ctxByShard[shard]
		if !ok {
			t.Fatalf("shard %d never opened a cursor", shard)
		}
		got, _ := chclient.QuerySettingsFromContext(shardCtx)["max_memory_usage"].(int64)
		if got != wantPerShard {
			t.Errorf("shard %d: max_memory_usage = %d, want routeACapBytes/kEff(=Parallel) = %d, NOT routeACapBytes/K = %d",
				shard, got, wantPerShard, routeACapBytes/k)
		}
	}
}

// TestExecute_MemoryApportion_UnconfiguredCapLeftUnapportioned pins the
// other half: when route A itself carries no cap (MaxQueryMemoryBytes()==0,
// "not configured"), Execute must not invent a per-shard cap — a routed
// dispatch must never become MORE restrictive than route A, only ever
// bounded by it.
func TestExecute_MemoryApportion_UnconfiguredCapLeftUnapportioned(t *testing.T) {
	t.Parallel()

	q := newFakeQuerier(3) // maxMemoryBytes left at its zero value
	x := newExec(q, newFakeEmitter(), testCfg(), 32, newFakeBreaker(BreakerClosed), nil)

	cur, _, err := x.Execute(context.Background(), "promql", makeDecision(2), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer cur.Close()
	if _, err := drainAll(cur); err != nil {
		t.Fatalf("drain: %v", err)
	}

	for shard := 0; shard < 2; shard++ {
		shardCtx, ok := q.ctxByShard[shard]
		if !ok {
			t.Fatalf("shard %d never opened a cursor", shard)
		}
		if settings := chclient.QuerySettingsFromContext(shardCtx); settings["max_memory_usage"] != nil {
			t.Errorf("shard %d: max_memory_usage = %v, want unset when route A has no configured cap",
				shard, settings["max_memory_usage"])
		}
	}
}
