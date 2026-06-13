//go:build integration

package main

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// TestAutoCreateSchema_StartupWiring exercises the exact wiring main.go
// uses when CERBERUS_AUTO_CREATE_SCHEMA=true: dial ClickHouse via
// chclient.New, pull the underlying driver.Conn via Client.Conn(), and
// hand it to ddl.ApplyWithConfig together with the configured database
// name. After the call the upstream OTel exporter table set must exist.
//
// Gated by the `integration` build tag — needs Docker. Regular `just
// test` skips it; CI's schema-ddl-test recipe (or its successor) picks it
// up via `go test -tags=integration ./...`.
func TestAutoCreateSchema_StartupWiring(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	container, err := tcclickhouse.Run(
		ctx,
		"clickhouse/clickhouse-server:25.8-alpine",
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
		tcclickhouse.WithDatabase("otel"),
	)
	if err != nil {
		t.Fatalf("start clickhouse: %v", err)
	}
	t.Cleanup(func() {
		_ = container.Terminate(context.Background())
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
		Addr:        fmt.Sprintf("%s:%s", host, port.Port()),
		Database:    "otel",
		Username:    "cerberus",
		Password:    "cerberus",
		DialTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("chclient.New: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Close()
	})

	// This mirrors the exact call sequence main.go uses when the env
	// flag is true.
	applyCfg := ddl.Config{Database: "otel"}
	if err := ddl.ApplyWithConfig(ctx, client.Conn(), applyCfg, ddl.All); err != nil {
		t.Fatalf("ddl.ApplyWithConfig: %v", err)
	}

	got := listTables(ctx, t, client, "otel")
	want := []string{
		"otel_logs",
		"otel_metrics_exp_histogram",
		"otel_metrics_gauge",
		"otel_metrics_histogram",
		"otel_metrics_sum",
		"otel_metrics_summary",
		"otel_traces",
		"otel_traces_trace_id_ts",
		"otel_traces_trace_id_ts_mv",
	}
	if !equalStrings(got, want) {
		t.Errorf("tables after auto-create:\n got: %v\nwant: %v", got, want)
	}

	// Re-run to confirm idempotency from the cerberus startup angle —
	// a process restart against an already-populated database must not
	// error out.
	if err := ddl.ApplyWithConfig(ctx, client.Conn(), applyCfg, ddl.All); err != nil {
		t.Fatalf("ddl.ApplyWithConfig (rerun): %v", err)
	}
}

// listTables reads the configured database's table list via the same
// chclient pool the startup hook uses — proves Client.Conn() exposes a
// usable driver.Conn.
func listTables(ctx context.Context, t *testing.T, client *chclient.Client, database string) []string {
	t.Helper()
	rows, err := client.Conn().Query(ctx,
		fmt.Sprintf("SELECT name FROM system.tables WHERE database = '%s' ORDER BY name", database))
	if err != nil {
		t.Fatalf("query tables: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
