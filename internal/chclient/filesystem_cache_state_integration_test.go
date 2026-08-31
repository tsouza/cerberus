//go:build integration

package chclient_test

import (
	"context"
	"testing"
	"time"

	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestQueryFilesystemCacheState_DefaultServerHasNoCacheConfigured runs
// filesystemCacheStateSQL against a REAL ClickHouse server (cerberus issue
// #2780), not chDB's embedded engine — a real server's aggregate-over-empty-
// table NULL handling and system.filesystem_cache_settings availability are
// worth pinning independently of the chdb-tagged unit test
// (TestFilesystemCacheStateSQL_NoCacheConfigured), the same reasoning
// TestProbeResultCacheCapability_HealthyServerIsAvailable's own doc gives for
// re-running a chDB-proven query against real ClickHouse. The stock
// testcontainers image carries no storage_configuration override, so — like
// chDB — it has no named filesystem cache configured, and the expected
// reading is the same all-zero Configured=false baseline. Gated by the
// `integration` build tag (requires Docker); the E2E workflow runs it,
// regular CI doesn't.
func TestQueryFilesystemCacheState_DefaultServerHasNoCacheConfigured(t *testing.T) {
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
		t.Fatalf("new default-bound client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if _, err := client.ProbeVersion(ctx); err != nil {
		t.Fatalf("ProbeVersion over healthy server failed: %v", err)
	}

	got, err := client.QueryFilesystemCacheState(ctx)
	if err != nil {
		t.Fatalf("QueryFilesystemCacheState: %v", err)
	}
	want := chclient.FilesystemCacheState{}
	if got != want {
		t.Fatalf("QueryFilesystemCacheState on a stock server = %+v; want %+v", got, want)
	}
}
