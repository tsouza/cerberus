package chclient

import (
	"context"
	"testing"
)

// TestEffectiveMaxQueryMemoryBytes_IsTheStampedCap pins that the cap the
// engine sizes its spill / join / compare bounds from is the SAME value the
// data-plane path stamps as max_memory_usage — the configured cap apportioned
// by DataShardCount — so a cap-relative threshold can never sit at or above
// the limit a statement actually runs under (cerberus issue #3128 audit). The
// configured accessor stays un-apportioned for the solver, which apportions
// by kEff x DataShardCount itself.
func TestEffectiveMaxQueryMemoryBytes_IsTheStampedCap(t *testing.T) {
	t.Parallel()
	const (
		configured     = int64(1 << 30)
		dataShardCount = 4
	)
	conn := &execRecordingConn{}
	m, _ := newTestConnMetrics(t)
	cfg := Config{DataShardCount: dataShardCount, MaxOpenConns: 8, MaxQueryMemoryBytes: configured}
	c := assembleClientFromConn(cfg, conn, m)
	t.Cleanup(func() { _ = c.Close() })

	stamped, ok := c.querySettings(context.Background())["max_memory_usage"].(int64)
	if !ok {
		t.Fatalf("querySettings did not stamp an int64 max_memory_usage: %#v", c.querySettings(context.Background())["max_memory_usage"])
	}
	if stamped != configured/dataShardCount {
		t.Fatalf("stamped max_memory_usage = %d, want the configured cap apportioned by DataShardCount (%d)", stamped, configured/dataShardCount)
	}
	if got := c.EffectiveMaxQueryMemoryBytes(); got != stamped {
		t.Fatalf("EffectiveMaxQueryMemoryBytes = %d, want the stamped %d", got, stamped)
	}
	if got := c.MaxQueryMemoryBytes(); got != configured {
		t.Fatalf("MaxQueryMemoryBytes = %d, want the configured, un-apportioned %d", got, configured)
	}
}

// TestEffectiveMaxQueryMemoryBytes_NoCapStaysZero pins the no-cap sentinel:
// 0 must stay 0 (the engine reads 0 as "use the absolute no-cap spill
// threshold"), never become the 1-byte floor ApportionMemoryBytes clamps to.
func TestEffectiveMaxQueryMemoryBytes_NoCapStaysZero(t *testing.T) {
	t.Parallel()
	conn := &execRecordingConn{}
	m, _ := newTestConnMetrics(t)
	c := assembleClientFromConn(Config{DataShardCount: 4, MaxOpenConns: 8}, conn, m)
	t.Cleanup(func() { _ = c.Close() })
	if got := c.EffectiveMaxQueryMemoryBytes(); got != 0 {
		t.Fatalf("EffectiveMaxQueryMemoryBytes with no cap = %d, want 0", got)
	}
}
