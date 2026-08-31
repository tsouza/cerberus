//go:build integration

package chclient_test

import (
	"context"
	"testing"
	"time"

	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
)

// TestProbeResultCacheCapability_HealthyServerIsAvailable is
// ProbeTSGridCapability's own regression test (see that test's doc for the
// UInt8-vs-String-column scan bug this shape guards against), run against
// the result-cache canary instead: a real, unconstrained ClickHouse accepts
// use_query_cache/query_cache_ttl, so the verdict must be Available. Gated
// by the `integration` build tag (requires Docker); the E2E workflow runs
// it, regular CI doesn't.
func TestProbeResultCacheCapability_HealthyServerIsAvailable(t *testing.T) {
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

	// Bind to the always-present `default` database, exactly as the boot probe
	// does over bootstrapClickHouseConfig.
	client, err := chclient.New(chclient.Config{
		Addr:     host + ":" + port.Port(),
		Username: "cerberus",
		Password: "cerberus",
		Database: "default",
	})
	if err != nil {
		t.Fatalf("new default-bound client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if _, err := client.ProbeVersion(ctx); err != nil {
		t.Fatalf("ProbeVersion over healthy server failed: %v", err)
	}

	got := client.ProbeResultCacheCapability(ctx)
	if got != chopt.CapabilityAvailable {
		t.Fatalf("ProbeResultCacheCapability on a healthy, unconstrained server = %v; want %v",
			got, chopt.CapabilityAvailable)
	}
}
