package chclient

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// TestBreakerRecord_AcquireConnTimeoutIsNeutral pins the rule that a
// pool acquire-timeout (clickhouse.ErrAcquireConnTimeout) never
// advances the breaker failure counter. The error signals local
// pool-sizing — every connection in the pool is busy and the acquire
// blocked past DialTimeout — not ClickHouse being down. The
// sharded-pushdown solver's fan-out makes acquire-timeout reachable
// under perfectly healthy CH; counting it would let a too-small pool
// trip the breaker and 503 traffic against a live backend. The fix for
// a recurring acquire-timeout is to raise MaxOpenConns, not to fail CH
// health.
func TestBreakerRecord_AcquireConnTimeoutIsNeutral(t *testing.T) {
	t.Parallel()

	b := &breaker{}

	// A storm of bare ErrAcquireConnTimeout never trips the breaker.
	for i := 0; i < breakerThreshold*3; i++ {
		b.record(context.Background(), clickhouse.ErrAcquireConnTimeout)
	}
	if got := b.currentState(); got != "closed" {
		t.Fatalf("breaker after bare acquire-timeout storm = %q, want closed", got)
	}

	// Same when wrapped the way the Client methods wrap it
	// ("chclient: query: %w").
	for i := 0; i < breakerThreshold*3; i++ {
		b.record(context.Background(), fmt.Errorf("chclient: query: %w", clickhouse.ErrAcquireConnTimeout))
	}
	if got := b.currentState(); got != "closed" {
		t.Fatalf("breaker after wrapped acquire-timeout storm = %q, want closed", got)
	}

	// Genuine backend failures still trip it — the neutral arm must not
	// neutralise everything.
	for i := 0; i < breakerThreshold; i++ {
		b.record(context.Background(), errors.New("dial tcp 127.0.0.1:9000: connection refused"))
	}
	if got := b.currentState(); got != "open" {
		t.Fatalf("breaker after genuine failures = %q, want open", got)
	}
}
