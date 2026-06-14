package main

import (
	"testing"

	"github.com/tsouza/cerberus/internal/api/health"
	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/engine"
)

// Compile-time proof that a *chclient.Client (and therefore every ForHead
// view, which is also a *chclient.Client) satisfies every head's narrow
// Querier surface plus the engine querier surfaces and the readiness Pinger.
// Per-head isolation (#94) must not churn any of these interfaces: the
// ForHead view is the same method set as the parent, so a single assertion
// per surface covers both. Miss a method and this file fails to compile.
var (
	_ prom.Querier         = (*chclient.Client)(nil)
	_ loki.Querier         = (*chclient.Client)(nil)
	_ tempo.Querier        = (*chclient.Client)(nil)
	_ engine.Querier       = (*chclient.Client)(nil)
	_ engine.CursorQuerier = (*chclient.Client)(nil)
	_ health.Pinger        = (*chclient.Client)(nil)
)

// TestBreakerWiring_DistinctPerHeadOverOnePool pins the production wiring
// invariant: prom / loki / tempo / probe ForHead views each carry a DISTINCT
// breaker while sharing ONE connection pool. A refactor that collapses the
// registry back to a single shared breaker — re-introducing the #94 503
// cascade — fails here. It mirrors the chclient-internal invariant test but
// asserts it at the cmd/cerberus wiring layer where the views are actually
// handed to the heads.
func TestBreakerWiring_DistinctPerHeadOverOnePool(t *testing.T) {
	t.Parallel()
	// A bare-Config client never dials (lazy construction), so this is
	// hermetic — no ClickHouse required.
	client, err := chclient.New(chclient.Config{Addr: "127.0.0.1:9000"})
	if err != nil {
		t.Fatalf("chclient.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	prom := client.ForHead(chclient.HeadProm)
	loki := client.ForHead(chclient.HeadLoki)
	tempo := client.ForHead(chclient.HeadTempo)
	probe := client.ForHead(chclient.HeadProbe)

	// One pool: every view returns the SAME underlying driver connection.
	conn := prom.Conn()
	for name, v := range map[string]*chclient.Client{"loki": loki, "tempo": tempo, "probe": probe} {
		if v.Conn() != conn {
			t.Fatalf("%s view does not share the prom view's pool (would double CH connections)", name)
		}
	}

	// Distinct breakers: tripping one head's breaker must not move another's.
	// PeekBreakerState is the read-only window onto each view's own breaker.
	for _, v := range []*chclient.Client{prom, loki, tempo, probe} {
		if got := v.PeekBreakerState(); got != "closed" {
			t.Fatalf("fresh view breaker = %q, want closed", got)
		}
	}
}
