package solver

import (
	"testing"

	"golang.org/x/sync/semaphore"
)

// TestExecutor_ShardMemoryDivisor pins the one number internal/engine
// divides every route-B fan-out ceiling by: kEff at its ceiling —
// min(K, Parallel, gate/2), floored at 1 — times DataShardCount. It is the
// largest divisor Execute can apply to a shard's max_memory_usage
// (perShardMemoryBytes = cap / (kEff x DataShardCount)), so a guard divided
// by it is never looser than the memory the shard actually runs under.
func TestExecutor_ShardMemoryDivisor(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                    string
		k, parallel, dataShards int
		gateCap                 int64
		want                    int64
	}{
		{name: "K below Parallel: kEff is K", k: 2, parallel: 3, dataShards: 1, gateCap: 32, want: 2},
		{name: "K above Parallel: kEff clamps to Parallel", k: 8, parallel: 3, dataShards: 1, gateCap: 32, want: 3},
		{name: "gate/2 below Parallel clamps kEff further", k: 8, parallel: 6, dataShards: 1, gateCap: 8, want: 4},
		{name: "DataShardCount multiplies in", k: 8, parallel: 3, dataShards: 2, gateCap: 32, want: 6},
		{name: "no gate: only Parallel clamps", k: 8, parallel: 5, dataShards: 1, gateCap: 0, want: 5},
		{name: "K of 1 is never divided below 1", k: 1, parallel: 3, dataShards: 1, gateCap: 32, want: 1},
		{name: "unvalidated zero Parallel floors to 1", k: 8, parallel: 0, dataShards: 1, gateCap: 32, want: 1},
		{name: "unvalidated zero DataShardCount floors to 1", k: 8, parallel: 3, dataShards: 0, gateCap: 32, want: 3},
		{name: "gate of 1 floors gate/2 to 1", k: 8, parallel: 3, dataShards: 1, gateCap: 1, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			x := &Executor{Cfg: Config{Parallel: tc.parallel, DataShardCount: tc.dataShards}}
			if tc.gateCap > 0 {
				x.Gate = semaphore.NewWeighted(tc.gateCap)
				x.GateCap = tc.gateCap
			}
			if got := x.ShardMemoryDivisor(tc.k); got != tc.want {
				t.Errorf("ShardMemoryDivisor(K=%d, Parallel=%d, D=%d, gate=%d) = %d, want %d",
					tc.k, tc.parallel, tc.dataShards, tc.gateCap, got, tc.want)
			}
		})
	}
}

// TestExecutor_ShardMemoryDivisor_BoundsTheAdmittedDivisor pins the
// relationship that makes the divisor safe to apportion a guard by: for
// every pEff the admission top-up can hand back (1..Parallel), the kEff
// Execute actually divides the cap by (effectiveShardCount, the clamp
// admitAndGate reads) is at most the ceiling ShardMemoryDivisor reports —
// so a shard's real memory share is never smaller than the share its guard
// was sized for.
func TestExecutor_ShardMemoryDivisor_BoundsTheAdmittedDivisor(t *testing.T) {
	t.Parallel()

	const (
		k          = 8
		parallel   = 5
		dataShards = 2
		gateCap    = int64(12)
	)
	x := &Executor{
		Cfg:     Config{Parallel: parallel, DataShardCount: dataShards},
		Gate:    semaphore.NewWeighted(gateCap),
		GateCap: gateCap,
	}
	ceiling := x.ShardMemoryDivisor(k)
	if ceiling != 10 {
		t.Fatalf("ShardMemoryDivisor(K=%d) = %d, want min(8, 5, 6) x 2 = 10", k, ceiling)
	}
	reached := false
	for pEff := 1; pEff <= parallel; pEff++ {
		admitted := int64(x.effectiveShardCount(k, pEff)) * x.dataShardCount()
		if admitted > ceiling {
			t.Errorf("pEff=%d: Execute would divide the cap by %d, more than the %d the guard was apportioned by",
				pEff, admitted, ceiling)
		}
		if admitted == ceiling {
			reached = true
		}
	}
	if !reached {
		t.Errorf("no admitted pEff reaches the ceiling %d — the divisor is not the tightest bound the clamp allows", ceiling)
	}
}
