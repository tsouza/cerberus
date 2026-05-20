// Command seed populates the cerberus E2E ClickHouse with the deterministic
// fixture rows that the e2e test suite + Grafana Playwright smoke depend on.
//
// It is the Go replacement for the three pre-existing
// `test/e2e/seed/{otel_metrics,otel_logs,otel_traces}.sql` scripts. The DDL
// half now lives in [internal/schema/ddl] (PR C of the schema-source-of-truth
// sequence) which wraps the upstream OTel ClickHouse Exporter templates. The
// INSERT statements below are preserved verbatim from the old .sql files so
// the row set the regression tests + e2e tests see is unchanged.
//
// Connection inputs (all env-driven; the Justfile sets them via
// `kubectl port-forward`):
//
//	CH_ADDR     host:port of the ClickHouse native protocol port. Required.
//	CH_DATABASE database name. Defaults to "otel".
//	CH_USERNAME ClickHouse username. Required (no anonymous fallback).
//	CH_PASSWORD ClickHouse password.
//
// Usage:
//
//	go run ./test/e2e/seed/cmd/seed
//
// (typically invoked through `just e2e-seed`).
//
// Implementation note: every INSERT below uses unqualified table names. The
// `Database` field set on the clickhouse-go Auth struct resolves them on the
// server side, so no `fmt.Sprintf("INSERT INTO %s.foo", database)` needed —
// keeps the seeder on the right side of the "no Sprintf-on-SQL" rule
// (CLAUDE.md § "No raw SQL strings").
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tsouza/cerberus/internal/schema/ddl"
)

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatalf("seed: %v", err)
	}
}

func run(ctx context.Context) error {
	addr := os.Getenv("CH_ADDR")
	if addr == "" {
		return fmt.Errorf("CH_ADDR is required (host:port of the ClickHouse native port)")
	}
	database := os.Getenv("CH_DATABASE")
	if database == "" {
		database = "otel"
	}
	user := os.Getenv("CH_USERNAME")
	password := os.Getenv("CH_PASSWORD")

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{addr},
		Auth: clickhouse.Auth{
			Database: database,
			Username: user,
			Password: password,
		},
		DialTimeout: 10 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("open clickhouse: %w", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := conn.Ping(pingCtx); err != nil {
		return fmt.Errorf("ping %s: %w", addr, err)
	}

	log.Printf("seed: applying upstream OTel-CH DDL to %s (database=%s)", addr, database) //nolint:gosec // G706: addr+database are CI/dev env config, not user input
	if err := ddl.ApplyWithConfig(ctx, conn, ddl.Config{Database: database}, ddl.All); err != nil {
		return fmt.Errorf("apply ddl: %w", err)
	}

	log.Printf("seed: inserting metrics fixtures")
	if err := insertMetrics(ctx, conn); err != nil {
		return fmt.Errorf("insert metrics: %w", err)
	}
	log.Printf("seed: inserting logs fixtures")
	if err := insertLogs(ctx, conn); err != nil {
		return fmt.Errorf("insert logs: %w", err)
	}
	log.Printf("seed: inserting traces fixtures")
	if err := insertTraces(ctx, conn); err != nil {
		return fmt.Errorf("insert traces: %w", err)
	}

	if err := verifyRowcounts(ctx, conn); err != nil {
		return fmt.Errorf("verify rowcounts: %w", err)
	}
	log.Printf("seed: done")
	return nil
}

// Static SQL — no string interpolation. Table names are unqualified; the
// clickhouse-go Auth.Database resolves them server-side.
const (
	// `up` is seeded as a 5-minute sliding window of samples (one per
	// 15 s = 20 samples per series, 2 series = 40 rows) so any
	// subquery / range query that runs within ~5 minutes of `e2e-seed`
	// finds at least one sample. A single-timestamp seed
	// (the previous shape) was timing-sensitive: by the time the
	// Playwright dashboard tests ran (~3–5 min after seed), the
	// subquery `up[1m:30s]` looked back 1 min from request_time and
	// missed the seed timestamp, returning 0 series intermittently.
	// See docs/test-audit-2026-05-20.md follow-up + e2e run
	// 26147340075 for the failure mode.
	insertGaugeSQL = `INSERT INTO otel_metrics_gauge
  (ResourceAttributes, MetricName, MetricDescription, MetricUnit, Attributes, StartTimeUnix, TimeUnix, Value)
SELECT
    map('service.name', 'api'),
    'up',
    'Is the scrape target up',
    '1',
    map('job', 'api'),
    now64(9) - INTERVAL number * 15 SECOND,
    now64(9) - INTERVAL number * 15 SECOND,
    1.0
FROM numbers(20)
UNION ALL
SELECT
    map('service.name', 'db'),
    'up',
    'Is the scrape target up',
    '1',
    map('job', 'db'),
    now64(9) - INTERVAL number * 15 SECOND,
    now64(9) - INTERVAL number * 15 SECOND,
    0.0
FROM numbers(20)`

	insertSumSQL = `INSERT INTO otel_metrics_sum
  (ResourceAttributes, MetricName, MetricDescription, MetricUnit, Attributes, StartTimeUnix, TimeUnix, Value, Flags, AggregationTemporality, IsMonotonic)
SELECT
    map('service.name', 'api'),
    'http_server_request_duration_count',
    'HTTP request count by status',
    '1',
    map('job', 'api', 'http_status', '200'),
    now64(9) - INTERVAL number SECOND,
    now64(9) - INTERVAL number SECOND,
    toFloat64(1000 + number * 5),
    toUInt32(0),
    toInt32(2),
    true
FROM numbers(60)`

	insertLogsSQL = `INSERT INTO otel_logs
  (Timestamp, TimestampTime, TraceId, SpanId, SeverityText, SeverityNumber, ServiceName, Body, ResourceAttributes, LogAttributes)
SELECT
    now64(9) - INTERVAL toUInt64((number % 60)) SECOND AS ts,
    ts,
    lpad(toString(number % 4), 32, '0'),
    lpad(toString(number % 4), 16, '0'),
    multiIf(number % 5 = 0, 'ERROR', number % 3 = 0, 'WARN', 'INFO'),
    multiIf(number % 5 = 0, 17, number % 3 = 0, 13, 9),
    arrayElement(['api', 'frontend', 'db'], number % 3 + 1),
    concat(
        arrayElement(['handled request', 'connection refused', 'slow query', 'cache hit', 'auth failed'], number % 5 + 1),
        ' id=', toString(number)
    ),
    map('service_name', arrayElement(['api', 'frontend', 'db'], number % 3 + 1)),
    map('thread', concat('worker-', toString(number % 4)))
FROM numbers(60)`

	insertTracesSQL = `INSERT INTO otel_traces
  (Timestamp, TraceId, SpanId, ParentSpanId, SpanName, SpanKind, ServiceName, ResourceAttributes, SpanAttributes, Duration, StatusCode)
VALUES
  (now64(9) - INTERVAL 10 SECOND, 'a0000000000000000000000000000001', '0000000000000001', '',                 'GET /home',        'Server', 'frontend', map('service.name', 'frontend'), map('http.method', 'GET',  'http.status_code', '200'), 150000000, 'Ok'),
  (now64(9) - INTERVAL 9 SECOND,  'a0000000000000000000000000000001', '0000000000000002', '0000000000000001', 'GET /api/users',   'Client', 'api',      map('service.name', 'api'),      map('http.method', 'GET',  'http.status_code', '200'),  80000000, 'Ok'),
  (now64(9) - INTERVAL 20 SECOND, 'a0000000000000000000000000000002', '0000000000000003', '',                 'POST /checkout',   'Server', 'frontend', map('service.name', 'frontend'), map('http.method', 'POST', 'http.status_code', '500'), 600000000, 'Error'),
  (now64(9) - INTERVAL 19 SECOND, 'a0000000000000000000000000000002', '0000000000000004', '0000000000000003', 'POST /api/order',  'Client', 'api',      map('service.name', 'api'),      map('http.method', 'POST', 'http.status_code', '500'), 450000000, 'Error'),
  (now64(9) - INTERVAL 19 SECOND, 'a0000000000000000000000000000002', '0000000000000005', '0000000000000004', 'orders.insert',    'Client', 'db',       map('service.name', 'db'),       map('db.system',   'postgres'),                            300000000, 'Error'),
  (now64(9) - INTERVAL 30 SECOND, 'a0000000000000000000000000000003', '0000000000000006', '',                 'cron.refresh',     'Server', 'api',      map('service.name', 'api'),      map('cron.name',   'refresh'),                              90000000, 'Ok'),
  (now64(9) - INTERVAL 29 SECOND, 'a0000000000000000000000000000003', '0000000000000007', '0000000000000006', 'cache.refresh',    'Client', 'db',       map('service.name', 'db'),       map('db.system',   'redis'),                                40000000, 'Ok')`
)

// insertMetrics inserts the two `up` gauge series + 60 counter samples for
// rate() — preserved verbatim from the previous test/e2e/seed/otel_metrics.sql.
// PR8's Playwright smoke queries these.
func insertMetrics(ctx context.Context, conn driver.Conn) error {
	if err := conn.Exec(ctx, insertGaugeSQL); err != nil {
		return fmt.Errorf("gauge: %w", err)
	}
	if err := conn.Exec(ctx, insertSumSQL); err != nil {
		return fmt.Errorf("sum: %w", err)
	}
	return nil
}

// insertLogs inserts 60 log records across 3 services in the last minute —
// preserved verbatim from the previous test/e2e/seed/otel_logs.sql. LogQL
// `{service_name="api"}` returns rows and `rate({service_name="api"}[5m])`
// returns a non-zero metric.
//
// Uses the underscored `service_name` map key because LogQL's matcher.Name is
// kept verbatim in cerberus's labelMatcherToExpr; the Prom/OTel naming bridge
// (`service_name` ↔ `service.name`) is not implemented.
func insertLogs(ctx context.Context, conn driver.Conn) error {
	return conn.Exec(ctx, insertLogsSQL)
}

// insertTraces inserts 3 traces with mixed services + durations — preserved
// verbatim from the previous test/e2e/seed/otel_traces.sql.
//
// Trace 1 (a0...001): frontend → api          (spans 0001 + 0002)
// Trace 2 (a0...002): frontend → api → db     (spans 0003 + 0004 + 0005)
// Trace 3 (a0...003): api → db                (spans 0006 + 0007)
func insertTraces(ctx context.Context, conn driver.Conn) error {
	return conn.Exec(ctx, insertTracesSQL)
}

// verifyRowcounts mirrors the per-table `count()` UNION that the previous
// shell-driven seed printed at the end — helps diagnose CI failures by
// showing whether INSERTs landed.
func verifyRowcounts(ctx context.Context, conn driver.Conn) error {
	tables := []struct {
		Label string
		SQL   string
	}{
		{"metrics_gauge", "SELECT count() FROM otel_metrics_gauge"},
		{"metrics_sum", "SELECT count() FROM otel_metrics_sum"},
		{"logs", "SELECT count() FROM otel_logs"},
		{"traces", "SELECT count() FROM otel_traces"},
	}
	for _, tc := range tables {
		var n uint64
		row := conn.QueryRow(ctx, tc.SQL)
		if err := row.Scan(&n); err != nil {
			return fmt.Errorf("count %s: %w", tc.Label, err)
		}
		log.Printf("seed: %-14s rows=%d", tc.Label, n)
	}
	return nil
}
