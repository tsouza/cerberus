//go:build integration

package chclient_test

import (
	"context"
	"testing"
	"time"

	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/chclient"
)

// resultCacheHitProbeSQL is a deliberately non-trivial aggregate (a real scan
// over 200M rows, not a constant SELECT) so a cache HIT is externally
// observable two ways: the server-reported QueryCacheHits/read_rows AND
// wall-clock time. A cheap constant query would complete near-instantly
// either way and prove nothing about "the second dispatch skipped the scan".
const resultCacheHitProbeSQL = "SELECT toString(sum(number)) FROM numbers(200000000)"

// TestResultCache_RepeatedQuery_SecondDispatchIsAHit is the end-to-end proof
// the performance-verification directive on cerberus issue #2781 asks for: a
// dashboard re-issuing the SAME query_range SQL on refresh must show a REAL
// ClickHouse-side cache hit on the second dispatch, not merely cerberus's own
// eligibility bookkeeping.
//
// It dispatches resultCacheHitProbeSQL TWICE over the exact ctx wrapper the
// engine's result-cache rule installs (chclient.WithResultCacheSetting),
// through the SAME code path production traffic uses (Client.QueryStrings ->
// queryContext -> the ProfileEvents observer wired in client.go), and asserts:
//
//  1. chclient.ResultCacheStats() shows a miss on dispatch 1 and a hit on
//     dispatch 2 — the real server-reported QueryCacheHits/QueryCacheMisses
//     ProfileEvents, proving the observer wiring in queryContext works against
//     a real server, not just the unit-level settings-map checks.
//  2. dispatch 2 is dramatically faster than dispatch 1 — the actual
//     performance win a cached dashboard panel gets, not merely "the setting
//     rode the request".
//
// Gated by the `integration` build tag (requires Docker); not part of the
// required CI check set (informational verification), matching the sibling
// TestProbeResultCacheCapability_HealthyServerIsAvailable's own gating.
func TestResultCache_RepeatedQuery_SecondDispatchIsAHit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	container, err := tcclickhouse.Run(
		ctx,
		"clickhouse/clickhouse-server:25.9-alpine",
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
	)
	if err != nil {
		t.Fatalf("start clickhouse: %v", err)
	}
	t.Cleanup(func() {
		_ = container.Terminate(ctx)
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}

	client, err := chclient.New(chclient.Config{
		Addr:     host + ":" + port.Port(),
		Username: "cerberus",
		Password: "cerberus",
		Database: "default",
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if capability := client.ProbeResultCacheCapability(ctx); capability.String() != "available" {
		t.Fatalf("query cache not available on this server: %v", capability)
	}

	hitsBefore, missesBefore := chclient.ResultCacheStats()

	// Dispatch 1: the real scan, cache-cold. Exactly the ctx the engine's
	// eligibleForResultCache + apply() would build for an eligible query.
	rcCtx := chclient.WithResultCacheSetting(ctx, 60)
	start1 := time.Now()
	if _, err := client.QueryStrings(rcCtx, resultCacheHitProbeSQL); err != nil {
		t.Fatalf("dispatch 1 (cold): %v", err)
	}
	elapsed1 := time.Since(start1)

	hitsAfter1, missesAfter1 := chclient.ResultCacheStats()
	if missesAfter1-missesBefore != 1 {
		t.Errorf("dispatch 1 (cold): QueryCacheMisses delta = %d; want 1", missesAfter1-missesBefore)
	}
	if hitsAfter1 != hitsBefore {
		t.Errorf("dispatch 1 (cold): QueryCacheHits moved (%d -> %d); want unchanged on a cold cache", hitsBefore, hitsAfter1)
	}

	// Dispatch 2: byte-identical SQL + settings fingerprint, same as a
	// dashboard's refresh re-issue. Must be a server-reported HIT.
	start2 := time.Now()
	if _, err := client.QueryStrings(rcCtx, resultCacheHitProbeSQL); err != nil {
		t.Fatalf("dispatch 2 (should hit): %v", err)
	}
	elapsed2 := time.Since(start2)

	hitsAfter2, _ := chclient.ResultCacheStats()
	if hitsAfter2-hitsAfter1 != 1 {
		t.Errorf("dispatch 2 (repeat): QueryCacheHits delta = %d; want 1 (the real cache-hit win #2781 exists for)", hitsAfter2-hitsAfter1)
	}

	// The performance win: a cache hit answers from memory instead of
	// re-scanning 200M rows, so dispatch 2 should be at least an order of
	// magnitude faster. Loose bound (2x) to stay robust against CI/container
	// scheduling jitter while still catching a genuine regression (e.g. the
	// setting silently failing to ride the second dispatch).
	if elapsed2*2 >= elapsed1 {
		t.Errorf("cache hit not faster than cold dispatch: cold=%s hit=%s; want the hit meaningfully faster (the win this feature exists for)", elapsed1, elapsed2)
	}
	t.Logf("cold dispatch (miss): %s; repeat dispatch (hit): %s", elapsed1, elapsed2)
}
