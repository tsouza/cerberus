//go:build integration

package ddl_test

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

func TestApplyWithConfig_DownsampleTierExperimentalGate(t *testing.T) {
	conn, database := startClickHouse(t)
	const schemaTimeout = time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), schemaTimeout)
	defer cancel()
	cfg := ddl.Config{Database: database, DownsampleTierEnabled: true}
	settingQuery, settingArgs := chsql.NewQuery().Select(chsql.Call("toUInt64", chsql.Call("getSetting",
		chsql.InlineLit("allow_experimental_time_series_aggregate_functions")))).Build()
	assertDefaultSetting := func() {
		t.Helper()
		var enabled uint64
		if err := conn.QueryRow(ctx, settingQuery, settingArgs...).Scan(&enabled); err != nil {
			t.Fatalf("read experimental setting: %v", err)
		}
		if enabled != 0 {
			t.Fatalf("experimental setting outside downsample DDL = %d, want 0", enabled)
		}
	}
	assertDefaultSetting()
	for range 2 {
		if err := ddl.ApplyWithConfig(ctx, conn, cfg, []ddl.Signal{ddl.Metrics}); err != nil {
			t.Fatalf("apply downsample schema with default server settings: %v", err)
		}
	}
	if tables := listTables(ctx, t, conn, database); !slices.Contains(tables, schema.DownsampleTierTable) {
		t.Fatalf("downsample table missing after reconciliation: %v", tables)
	}
	assertDefaultSetting()
}

// startClickHouse spins up a real ClickHouse via testcontainers and registers
// cleanup for the container and its connection.
func startClickHouse(t *testing.T) (driver.Conn, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	container, err := tcclickhouse.Run(
		ctx,
		"clickhouse/clickhouse-server:26.6-alpine",
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

// startClickHouseDefaultDB spins up a real ClickHouse whose ONLY database is
// the built-in `default`, and returns a driver.Conn bound to it. Unlike
// startClickHouse it deliberately does NOT pre-create an `otel` database — so
// a test can prove Apply creates a target database that does not yet exist,
// which is the real cold-cluster bootstrap path (k8s / compose against a plain
// ClickHouse). The earlier integration tests masked the missing-database bug
// by handing testcontainers WithDatabase("otel").
func startClickHouseDefaultDB(t *testing.T) driver.Conn {
	t.Helper()
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
			Database: "default",
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
	return conn
}

// databaseExists reports whether a database row exists in system.databases.
func databaseExists(ctx context.Context, t *testing.T, conn driver.Conn, database string) bool {
	t.Helper()
	rows, err := conn.Query(ctx, fmt.Sprintf("SELECT name FROM system.databases WHERE name = '%s'", database))
	if err != nil {
		t.Fatalf("query databases: %v", err)
	}
	defer rows.Close()
	return rows.Next()
}

// TestApply_CreatesDatabaseWhenAbsent is the regression test for the
// cold-cluster bug: cerberus's AUTO_CREATE_SCHEMA targeted the `otel` database
// but never created it, so on a fresh ClickHouse (only `default` exists) every
// CREATE TABLE failed with "Database otel does not exist". Apply must now
// create the database first, then the tables — driven from a connection bound
// to `default`, exactly like a real deployment dialing a plain ClickHouse.
func TestApply_CreatesDatabaseWhenAbsent(t *testing.T) {
	conn := startClickHouseDefaultDB(t)
	ctx := context.Background()

	const target = "otel"
	if databaseExists(ctx, t, conn, target) {
		t.Fatalf("precondition: database %q already exists; the test cannot prove it gets created", target)
	}

	cfg := ddl.Config{Database: target}
	if err := ddl.ApplyWithConfig(ctx, conn, cfg, ddl.All); err != nil {
		t.Fatalf("Apply against absent database: %v", err)
	}

	if !databaseExists(ctx, t, conn, target) {
		t.Fatalf("database %q was not created by Apply", target)
	}

	tables := listTables(ctx, t, conn, target)
	want := []string{
		"otel_logs",
		"otel_metrics_exponential_histogram",
		"otel_metrics_gauge",
		"otel_metrics_histogram",
		"otel_metrics_sum",
		"otel_metrics_summary",
		"otel_traces",
		"otel_traces_trace_id_ts",
		"otel_traces_trace_id_ts_mv",
	}
	if !sameStringSlice(tables, want) {
		t.Errorf("tables after create-database Apply:\n got: %v\nwant: %v", tables, want)
	}

	// Re-apply must stay a no-op now that the database AND tables exist —
	// the IF NOT EXISTS on the database create is what keeps a process
	// restart against a provisioned cluster clean.
	if err := ddl.ApplyWithConfig(ctx, conn, cfg, ddl.All); err != nil {
		t.Fatalf("Apply (rerun against provisioned database): %v", err)
	}
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
		"otel_metrics_exponential_histogram",
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

func countBodyIndexes(ctx context.Context, t *testing.T, conn driver.Conn, database, table, indexType string) uint64 {
	t.Helper()
	query, args := chsql.NewQuery().
		Select(chsql.Call("count")).
		From(chsql.Qual("system", "data_skipping_indices")).
		Where(chsql.And(
			chsql.Eq(chsql.Col("database"), chsql.Lit(database)),
			chsql.Eq(chsql.Col("table"), chsql.Lit(table)),
			chsql.Eq(chsql.Col("type"), chsql.Lit(indexType)),
			chsql.In(
				chsql.Col("expr"),
				chsql.Lit("lower(Body)"),
				chsql.Lit("lower(`Body`)"),
			),
		)).
		Build()
	var count uint64
	if err := conn.QueryRow(ctx, query, args...).Scan(&count); err != nil {
		t.Fatalf("query Body indexes: %v", err)
	}
	return count
}

// TestApply_FullTextIndexReconcilesFreshAndLegacySchemas is the ClickHouse
// 26.6 regression for the v1.21.0 readiness failure. A fresh table already
// receives idx_lower_body TYPE text from CREATE and must not receive a second
// text index on the same expression; a legacy tokenbf_v1 table still needs the
// separately named idx_body_text additive upgrade. Reapplying is idempotent.
func TestApply_FullTextIndexReconcilesFreshAndLegacySchemas(t *testing.T) {
	conn, database := startClickHouse(t)
	ctx := context.Background()

	freshCfg := ddl.Config{Database: database, TextIndexEnabled: true}
	if err := ddl.ApplyWithConfig(ctx, conn, freshCfg, []ddl.Signal{ddl.Logs}); err != nil {
		t.Fatalf("fresh full-text schema: %v", err)
	}
	if got := countBodyIndexes(ctx, t, conn, database, "otel_logs", "text"); got != 1 {
		t.Fatalf("fresh Body text indexes = %d, want 1", got)
	}
	if err := ddl.ApplyWithConfig(ctx, conn, freshCfg, []ddl.Signal{ddl.Logs}); err != nil {
		t.Fatalf("repeat full-text reconciliation: %v", err)
	}
	if got := countBodyIndexes(ctx, t, conn, database, "otel_logs", "text"); got != 1 {
		t.Fatalf("Body text indexes after repeat = %d, want 1", got)
	}

	const legacyTable = "otel_logs_legacy"
	legacyCfg := ddl.Config{Database: database, Tables: ddl.Tables{Logs: legacyTable}}
	if err := ddl.ApplyWithConfig(ctx, conn, legacyCfg, []ddl.Signal{ddl.Logs}); err != nil {
		t.Fatalf("create legacy logs schema: %v", err)
	}
	legacyCfg.TextIndexEnabled = true
	if err := ddl.ApplyWithConfig(ctx, conn, legacyCfg, []ddl.Signal{ddl.Logs}); err != nil {
		t.Fatalf("upgrade legacy logs schema: %v", err)
	}
	if got := countBodyIndexes(ctx, t, conn, database, legacyTable, "text"); got != 1 {
		t.Fatalf("upgraded legacy Body text indexes = %d, want 1", got)
	}
	if got := countBodyIndexes(ctx, t, conn, database, legacyTable, "tokenbf_v1"); got != 1 {
		t.Fatalf("upgraded legacy Body tokenbf_v1 indexes = %d, want 1", got)
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
		"otel_metrics_exponential_histogram",
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
