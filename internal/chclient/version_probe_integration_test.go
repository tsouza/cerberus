//go:build integration

package chclient_test

import (
	"context"
	"strings"
	"testing"
	"time"

	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestProbeVersion_DatabaseAbsent is the regression test for the boot-order bug
// where cerberus pinned the optimization auto-picker to the 24.8 supported floor
// on a fresh/upgraded ClickHouse. The version probe runs BEFORE the schema
// auto-create step, so on a fresh server the configured `otel` database does not
// exist yet. ClickHouse rejects EVERY statement — SELECT version() included — on
// a session whose default database is absent (code 81, UNKNOWN_DATABASE), during
// session default-DB resolution before execution. The probe therefore failed and
// the picker fell back to the 24.8 floor for the pod's lifetime, defeating the
// upgrade until a manual restart.
//
// The fix re-scopes the probe to ClickHouse's always-present `default` database
// (the same one the auto-create DDL bootstraps over). This test stands up a real
// server whose ONLY database is `default` and proves both halves:
//
//  1. a client bound to the absent `otel` database fails the probe (the bug), and
//  2. a client bound to `default` succeeds and parses the REAL server version
//     (the fix) — independent of whether `otel` exists.
//
// Gated by the `integration` build tag (requires Docker); the E2E workflow runs
// it, regular CI doesn't.
func TestProbeVersion_DatabaseAbsent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Start a plain ClickHouse with only the built-in `default` database —
	// deliberately NOT pre-creating `otel`, so we reproduce the real
	// cold-cluster bootstrap path a fresh k8s/compose deployment hits.
	container, err := tcclickhouse.Run(
		ctx,
		"clickhouse/clickhouse-server:25.8-alpine",
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
	addr := host + ":" + port.Port()

	base := chclient.Config{
		Addr:     addr,
		Username: "cerberus",
		Password: "cerberus",
	}

	// Half 1 — bind to the ABSENT otel database. This is the pre-fix wiring:
	// the probe ran over cerberus's main client whose session default DB is the
	// configured otel, so version() is rejected with code 81 before it executes.
	t.Run("otel_absent_probe_fails", func(t *testing.T) {
		otelCfg := base
		otelCfg.Database = "otel"
		client, err := chclient.New(otelCfg)
		if err != nil {
			t.Fatalf("new otel-bound client: %v", err)
		}
		t.Cleanup(func() { _ = client.Close() })

		_, err = client.ProbeVersion(ctx)
		if err == nil {
			t.Fatal("expected ProbeVersion to fail against the absent otel database, got nil " +
				"(the bug only reproduces while otel does not exist)")
		}
		// The server-side rejection is code 81 / "Database otel does not exist".
		// Assert on the substring rather than the numeric code to stay robust to
		// driver error-wrapping; the point is the probe fails because the bound
		// DB is missing, which is exactly what the default-bound probe avoids.
		if !strings.Contains(err.Error(), "otel") {
			t.Fatalf("expected probe error to mention the absent otel database, got: %v", err)
		}
	})

	// Half 2 — bind to the always-present `default` database (the fix). The
	// probe must now succeed and parse the REAL server version regardless of
	// whether otel exists.
	t.Run("default_bound_probe_succeeds", func(t *testing.T) {
		defCfg := base
		defCfg.Database = "default"
		client, err := chclient.New(defCfg)
		if err != nil {
			t.Fatalf("new default-bound client: %v", err)
		}
		t.Cleanup(func() { _ = client.Close() })

		v, err := client.ProbeVersion(ctx)
		if err != nil {
			t.Fatalf("ProbeVersion over default-bound connection (otel absent): %v", err)
		}
		// The testcontainers image is the 25.8 line; the probe must resolve a
		// real, non-zero version rather than fall through to the 24.8 floor.
		if v.Major == 0 {
			t.Fatalf("probe resolved a zero version: %+v", v)
		}
		if v.Major != 25 || v.Minor != 8 {
			t.Errorf("probe version mismatch: got %s, want 25.8 (server image line)", v.String())
		}
	})
}
