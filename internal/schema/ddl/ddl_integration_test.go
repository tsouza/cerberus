//go:build integration

package ddl_test

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// startClickHouse spins up a real ClickHouse via testcontainers and returns
// a driver.Conn bound to it plus a cleanup func. The image tracks the same
// `25-alpine` line used by chclient's integration test.
func startClickHouse(t *testing.T) (driver.Conn, string) {
	t.Helper()
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

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{fmt.Sprintf("%s:%s", host, port.Port())},
		Auth: clickhouse.Auth{
			Database: "otel",
			Username: "cerberus",
			Password: "cerberus",
		},
		DialTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
	})

	pingCtx, pingCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer pingCancel()
	if err := conn.Ping(pingCtx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return conn, "otel"
}

// listTables reads the current database's table list — used by Apply tests
// to assert what got created.
func listTables(ctx context.Context, t *testing.T, conn driver.Conn, database string) []string {
	t.Helper()
	rows, err := conn.Query(ctx, fmt.Sprintf("SELECT name FROM system.tables WHERE database = '%s' ORDER BY name", database))
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

// TestApply_CreatesAllTables runs Apply(ctx, conn, All) and checks every
// signal's upstream tables show up in system.tables. The MV is also a row
// in system.tables, so the expected total is 5 metrics + 1 logs + 1 spans
// + 1 lookup + 1 MV = 9.
func TestApply_CreatesAllTables(t *testing.T) {
	conn, database := startClickHouse(t)
	ctx := context.Background()

	cfg := ddl.Config{Database: database}
	if err := ddl.ApplyWithConfig(ctx, conn, cfg, ddl.All); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	tables := listTables(ctx, t, conn, database)
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
	if !sameStringSlice(tables, want) {
		t.Errorf("tables mismatch:\n got: %v\nwant: %v", tables, want)
	}
}

// TestApply_Idempotent runs Apply twice and confirms no error + identical
// table list. This is the contract that lets PR D wire auto-create at
// startup without guarding for "already exists".
func TestApply_Idempotent(t *testing.T) {
	conn, database := startClickHouse(t)
	ctx := context.Background()
	cfg := ddl.Config{Database: database}

	if err := ddl.ApplyWithConfig(ctx, conn, cfg, ddl.All); err != nil {
		t.Fatalf("Apply #1: %v", err)
	}
	first := listTables(ctx, t, conn, database)

	if err := ddl.ApplyWithConfig(ctx, conn, cfg, ddl.All); err != nil {
		t.Fatalf("Apply #2: %v", err)
	}
	second := listTables(ctx, t, conn, database)

	if !sameStringSlice(first, second) {
		t.Errorf("table list changed after second Apply:\n  before: %v\n  after:  %v", first, second)
	}
}

// TestApply_SignalSubset confirms a single-signal Apply only touches that
// signal's tables.
func TestApply_SignalSubset(t *testing.T) {
	conn, database := startClickHouse(t)
	ctx := context.Background()
	cfg := ddl.Config{Database: database}

	if err := ddl.ApplyWithConfig(ctx, conn, cfg, []ddl.Signal{ddl.Metrics}); err != nil {
		t.Fatalf("Apply(Metrics): %v", err)
	}

	tables := listTables(ctx, t, conn, database)
	want := []string{
		"otel_metrics_exp_histogram",
		"otel_metrics_gauge",
		"otel_metrics_histogram",
		"otel_metrics_sum",
		"otel_metrics_summary",
	}
	if !sameStringSlice(tables, want) {
		t.Errorf("metrics-only tables mismatch:\n got: %v\nwant: %v", tables, want)
	}
}

func sameStringSlice(a, b []string) bool {
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
