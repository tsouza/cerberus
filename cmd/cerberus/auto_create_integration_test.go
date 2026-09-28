//go:build integration

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// TestAutoCreateSchema_StartupWiring exercises the actual startup path against
// an absent target database, including capability probes before schema DDL.
func TestAutoCreateSchema_StartupWiring(t *testing.T) {
	const startupTestTimeout = 5 * time.Minute
	const dialTimeout = 10 * time.Second
	const unknownDatabaseCode = 81
	ctx, cancel := context.WithTimeout(t.Context(), startupTestTimeout)
	defer cancel()
	container, err := tcclickhouse.Run(
		ctx,
		"clickhouse/clickhouse-server:25.9-alpine",
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
		tcclickhouse.WithDatabase("default"),
	)
	if err != nil {
		t.Fatalf("start clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })
	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	for _, tc := range []struct {
		name       string
		downsample bool
	}{
		{name: "base"},
		{name: "downsample", downsample: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := "auto_create_" + tc.name
			cfg := chclient.Config{
				Addr:        fmt.Sprintf("%s:%s", host, port.Port()),
				Database:    database,
				Username:    "cerberus",
				Password:    "cerberus",
				DialTimeout: dialTimeout,
			}
			client, err := chclient.New(cfg)
			if err != nil {
				t.Fatalf("chclient.New: %v", err)
			}
			t.Cleanup(func() { _ = client.Close() })
			// A pre-created database would conceal the capability-probe ordering bug.
			err = client.Conn().Ping(ctx)
			var exception *clickhouse.Exception
			if !errors.As(err, &exception) || exception.Code != unknownDatabaseCode {
				t.Fatalf("target database must start absent; ping returned %v", err)
			}
			applyCfg := ddl.Config{Database: database, DownsampleTierEnabled: tc.downsample}
			metrics := schema.DefaultOTelMetrics()
			ready := setupSchema(ctx, logger, client, cfg, applyCfg, metrics, true, true)
			if !ready() {
				t.Fatal("schema is not ready after synchronous startup")
			}
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
			if tc.downsample {
				want = append(want, schema.DownsampleTierTable,
					"otel_metrics_sum_downsample_tier_mv", "otel_metrics_sum_downsample_tier_gauge_mv")
			}
			sort.Strings(want)
			got := listTables(ctx, t, client, database)
			if !equalStrings(got, want) {
				t.Errorf("tables after auto-create:\n got: %v\nwant: %v", got, want)
			}

			if tc.downsample {
				const expectedViews = 2
				query, args := chsql.NewQuery().
					Select(chsql.Col("name"), chsql.Col("create_table_query")).
					From(chsql.Qual("system", "tables")).
					Where(
						chsql.Eq(chsql.Col("database"), chsql.Lit(database)),
						chsql.Or(
							chsql.Eq(chsql.Col("name"), chsql.Lit("otel_metrics_sum_downsample_tier_mv")),
							chsql.Eq(chsql.Col("name"), chsql.Lit("otel_metrics_sum_downsample_tier_gauge_mv")),
						),
					).Build()
				views, err := client.QueryNameTypePairs(ctx, query, args...)
				if err != nil {
					t.Fatalf("read downsample views: %v", err)
				}
				if len(views) != expectedViews {
					t.Fatalf("downsample views: got %d, want %d", len(views), expectedViews)
				}
				for _, view := range views {
					if !strings.Contains(view.Type, metrics.FlagsColumn) {
						t.Errorf("downsample view %s does not read stale-marker Flags: %s", view.Name, view.Type)
					}
				}
			}
			// Reuse the now-existing database with database creation disabled: the
			// ordinary target-bound client must still provision idempotently.
			ready = setupSchema(ctx, logger, client, cfg, applyCfg, metrics, true, false)
			if !ready() {
				t.Fatal("schema is not ready after repeated startup with an externally managed database")
			}
		})
	}
}

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
