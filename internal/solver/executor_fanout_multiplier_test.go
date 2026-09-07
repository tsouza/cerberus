package solver

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestExecute_StampsEachShardsFanoutMultiplierFromItsEmitCount pins the
// route-B half of the data-shard fan-out weight (cerberus issue #3128): the
// physical-scan count the emitter reports for a shard's statement must
// reach that shard's dispatch ctx as chclient's fan-out multiplier, so the
// gate charges scans x DataShardCount for it. The fake reports 3 (rate()'s
// three window arms), so the default of 1 — or no stamp at all — fails.
func TestExecute_StampsEachShardsFanoutMultiplierFromItsEmitCount(t *testing.T) {
	t.Parallel()

	const (
		k             = 3
		physicalScans = 3
	)
	q := newFakeQuerier(k)
	em := newFakeEmitter()
	em.physicalScans = physicalScans
	cfg := testCfg()
	cfg.Parallel = k
	x := newExec(q, em, cfg, 32, newFakeBreaker(BreakerClosed), nil)

	cur, info, err := x.Execute(context.Background(), "promql", makeDecision(k), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer cur.Close()
	if _, err := drainAll(cur); err != nil {
		t.Fatalf("drain: %v", err)
	}
	for shard := 0; shard < k; shard++ {
		if got := info.PhysicalScans[shard]; got != physicalScans {
			t.Fatalf("ExecInfo.PhysicalScans[%d] = %d, want the emitter's %d", shard, got, physicalScans)
		}
		shardCtx, ok := q.ctxByShard[shard]
		if !ok {
			t.Fatalf("shard %d never opened a cursor", shard)
		}
		got, stamped := chclient.DataShardFanoutMultiplierFromContext(shardCtx)
		if !stamped {
			t.Fatalf("shard %d: no data-shard fan-out multiplier on its dispatch ctx — the emitter's count never reached the gate", shard)
		}
		if got != physicalScans {
			t.Fatalf("shard %d: fan-out multiplier = %d, want the emitted statement's %d scans", shard, got, physicalScans)
		}
	}
}
