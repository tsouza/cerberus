package chclient

import (
	"context"
	"testing"
	"time"
)

// TestAcquireDataShardFanout_WiderThanCap_AdmittedAlone pins the clamp in
// acquireDataShardFanout (cerberus issue #3128): a statement whose own
// fan-out (multiplier x DataShardCount) exceeds the whole cap is admitted
// with the cap as its weight — alone, saturating the gate — rather than
// parked until its deadline (semaphore.Weighted never admits a weight above
// its size). Both halves are pinned: it IS admitted, and nothing else is
// while it holds the gate.
func TestAcquireDataShardFanout_WiderThanCap_AdmittedAlone(t *testing.T) {
	t.Parallel()
	const (
		dataShardCount = 4
		multiplier     = 3 // 12 wide against a cap of 8
		cap            = 8
	)
	conn := &execRecordingConn{}
	m, _ := newTestConnMetrics(t)
	capOverride := int64(cap)
	cfg := Config{DataShardCount: dataShardCount, MaxOpenConns: cap, DataShardFanoutCapOverride: &capOverride}
	c := assembleClientFromConn(cfg, conn, m)
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ctx = WithDataShardFanoutMultiplier(ctx, multiplier)

	release, err := c.acquireDataShardFanout(ctx)
	if err != nil {
		t.Fatalf("a statement wider than the cap must be admitted (alone), got: %v", err)
	}
	if c.dataShardFanoutGate.TryAcquire(1) {
		t.Fatal("gate admitted another unit alongside a statement wider than the cap — the clamp did not saturate the gate")
	}
	release()
	if !c.dataShardFanoutGate.TryAcquire(cap) {
		t.Fatal("gate did not release the full clamped weight")
	}
}
