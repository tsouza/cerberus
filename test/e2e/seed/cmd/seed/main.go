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
	// `up` is seeded as a 30-minute sliding window of samples centred on
	// the seed timestamp (one per 15 s = 120 samples per series, 2 series
	// = 240 rows) spanning [seed_now - 900 s, seed_now + 885 s]. The
	// earlier ±300 s span was timing-sensitive: the full Playwright suite
	// runs ~12 min of tests serially, so a query landing at seed + 11 min
	// found every gauge sample outside the 5 m instant-staleness lookback
	// `(eval - 5m, eval]` (see internal/promql/modifiers.go::instantLookback
	// + PR #655). With the wider window every test up to seed + 14 min
	// keeps ≥ 1 future sample inside the 5 m staleness envelope.
	// `ServiceName` is set explicitly alongside `ResourceAttributes`
	// so the rows match the shape the upstream OTel-CH exporter would
	// emit (the exporter copies `ResAttr['service.name']` into the
	// dedicated `ServiceName LowCardinality(String)` column on every
	// metrics insert — see `GetServiceName` in
	// internal/metrics/metrics_model.go of the collector-contrib
	// exporter). Skipping the column left `ServiceName=''` on every
	// seeded row, which broke the otel-fixture-explorer "Top services
	// by metric rate" panel after PR #679's `sum by (service_name)`
	// lowering started coalescing the dedicated column with the
	// Attributes-map key (the empty top-level column collapsed every
	// row into a single anonymous bucket — exactly the N2/N11/N14
	// shape the phase-1 sweep pins).
	insertGaugeSQL = `INSERT INTO otel_metrics_gauge
  (ResourceAttributes, ServiceName, MetricName, MetricDescription, MetricUnit, Attributes, StartTimeUnix, TimeUnix, Value)
SELECT
    map('service.name', 'api'),
    'api',
    'up',
    'Is the scrape target up',
    '1',
    map('job', 'api'),
    now64(9) + INTERVAL ((number - 60) * 15) SECOND,
    now64(9) + INTERVAL ((number - 60) * 15) SECOND,
    1.0
FROM numbers(120)
UNION ALL
SELECT
    map('service.name', 'db'),
    'db',
    'up',
    'Is the scrape target up',
    '1',
    map('job', 'db'),
    now64(9) + INTERVAL ((number - 60) * 15) SECOND,
    now64(9) + INTERVAL ((number - 60) * 15) SECOND,
    0.0
FROM numbers(120)`

	// 1800 samples at 1 s cadence cover a 30-minute window centred on
	// the seed timestamp: [seed_now - 900 s, seed_now + 899 s]. The
	// earlier ±300 s span was timing-sensitive: the full Playwright
	// suite runs ~12 min serially, so by the time `prom_ux.spec.ts:180`
	// (target B's `rate(http_server_request_duration_count[1m])`)
	// landed, the 1 min lookback at eval-time had slid past every
	// seeded sample and the query returned 0 series. Spanning ±15 min
	// keeps any 1m / 5m rate() window inside the seed envelope for
	// every test in the suite plus future tests added below this line.
	//
	// The value formula `1000 + number * 5` is monotone-with-time
	// (number = 0 is the earliest sample at seed_now - 900 s; number
	// = 1799 is the latest at seed_now + 899 s), which is the shape
	// rate() expects for a counter.
	// Same `ServiceName` discipline as `insertGaugeSQL` — the
	// otel_metrics_sum row needs the dedicated LowCardinality column
	// populated to match the upstream OTel-CH exporter shape, otherwise
	// `sum by (service_name)` partitions over an empty column.
	insertSumSQL = `INSERT INTO otel_metrics_sum
  (ResourceAttributes, ServiceName, MetricName, MetricDescription, MetricUnit, Attributes, StartTimeUnix, TimeUnix, Value, Flags, AggregationTemporality, IsMonotonic)
SELECT
    map('service.name', 'api'),
    'api',
    'http_server_request_duration_count',
    'HTTP request count by status',
    '1',
    map('job', 'api', 'http_status', '200'),
    now64(9) + INTERVAL (number - 900) SECOND,
    now64(9) + INTERVAL (number - 900) SECOND,
    toFloat64(1000 + number * 5),
    toUInt32(0),
    toInt32(2),
    true
FROM numbers(1800)`

	// Classic-histogram companion rows for `http_server_request_duration`.
	//
	// Since PR #645 (`HistogramCompanionColumn`,
	// internal/schema/otel.go:373), the PromQL lowering routes any
	// `<base>_count` / `<base>_sum` reference to `otel_metrics_histogram`
	// under the bare `<base>` MetricName, projecting `Count` / `Sum` as
	// the value. That's the correct production behaviour — the OTel-CH
	// exporter writes classic-histogram series as one row per anchor
	// with `Count`, `Sum`, `BucketCounts`, `ExplicitBounds` populated,
	// not as separate `_count` / `_sum` MetricName rows.
	//
	// The `otel_metrics_sum` seed above pre-dates that routing change
	// and is kept for any test that still reads the sum table directly.
	// This block adds the histogram rows the e2e Prom tests
	// (`TestPromQueryRangeRate`, `TestPromQuerySubqueryMaxOverTimeRate`)
	// now depend on: `rate(http_server_request_duration_count[1m])`
	// scans `otel_metrics_histogram` for MetricName='http_server_request_duration'
	// and projects `toFloat64(Count)` as the counter value.
	//
	// Shape mirrors `insertSumSQL`: 1800 samples at 1 s cadence covering
	// [seed_now - 900 s, seed_now + 899 s] so any 1m / 5m `rate()` window
	// retains ≥2 samples for every Playwright test in the suite. Count
	// grows monotonically with sample index (100 + number * 5) so
	// `rate(Count)` is positive; Sum tracks Count (5x scale) so the
	// `_sum` companion route is also exercised. BucketCounts = [10, 20,
	// 30, 40] (sum 100, matches the +100 base of Count) and
	// ExplicitBounds = [0.1, 0.5, 1.0] (three explicit edges + implicit
	// +Inf trailing bucket) give `histogram_quantile()` a non-trivial
	// distribution to interpolate over if a future spec exercises it.
	insertHistogramSQL = `INSERT INTO otel_metrics_histogram
  (ResourceAttributes, ServiceName, MetricName, MetricDescription, MetricUnit, Attributes, StartTimeUnix, TimeUnix, Count, Sum, BucketCounts, ExplicitBounds, Flags, AggregationTemporality)
SELECT
    map('service.name', 'api'),
    'api',
    'http_server_request_duration',
    'HTTP request duration histogram',
    's',
    map('job', 'api', 'http_status', '200'),
    now64(9) + INTERVAL (number - 900) SECOND,
    now64(9) + INTERVAL (number - 900) SECOND,
    toUInt64(100 + number * 5),
    toFloat64((100 + number * 5) * 5) / 1000,
    [toUInt64(10), toUInt64(20), toUInt64(30), toUInt64(40)],
    [toFloat64(0.1), toFloat64(0.5), toFloat64(1.0)],
    toUInt32(0),
    toInt32(2)
FROM numbers(1800)`

	// otel_logs seed spans a 30-minute window centred on seed_now,
	// [seed_now - 900 s, seed_now + 885 s], with 120 rows at 15 s
	// cadence across 3 services. The earlier 60-row past-only window
	// (`number % 60` seconds back) was timing-sensitive: every Loki
	// `/query` instant request lands with the same 5 m staleness
	// lookback `(eval - 5m, eval]` PromQL uses (see
	// internal/api/loki/handler.go:235), so once Playwright's serial
	// suite drifted more than ~5 min past seed (it now runs ~12 min
	// of tests), every seeded log row fell outside the lookback and
	// the Loki streams query returned 0 results. Spanning past +
	// future keeps ≥ 1 row inside the 5 m envelope for every test in
	// the suite.
	insertLogsSQL = `INSERT INTO otel_logs
  (Timestamp, TimestampTime, TraceId, SpanId, SeverityText, SeverityNumber, ServiceName, Body, ResourceAttributes, LogAttributes)
SELECT
    now64(9) + INTERVAL ((number - 60) * 15) SECOND AS ts,
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
FROM numbers(120)`

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

// insertMetrics inserts the two `up` gauge series + 1800 counter samples for
// rate(). Both seeds span a 30-minute window centred on the seed timestamp —
// the gauge with 120 samples × 15 s and the counter with 1800 samples × 1 s —
// so a 1m/5m `rate()` or subquery in any Playwright spec retains ≥2 samples
// in its lookback window regardless of how much CI scheduling jitter lands
// between `e2e-seed` and the Playwright request (within ±14 min of seed,
// covering the full ~12 min serial-suite runtime + headroom).
func insertMetrics(ctx context.Context, conn driver.Conn) error {
	if err := conn.Exec(ctx, insertGaugeSQL); err != nil {
		return fmt.Errorf("gauge: %w", err)
	}
	if err := conn.Exec(ctx, insertSumSQL); err != nil {
		return fmt.Errorf("sum: %w", err)
	}
	if err := conn.Exec(ctx, insertHistogramSQL); err != nil {
		return fmt.Errorf("histogram: %w", err)
	}
	return nil
}

// insertLogs inserts 120 log records across 3 services spanning a 30-minute
// window centred on the seed timestamp (15 s cadence). LogQL
// `{service_name="api"}` returns rows and `rate({service_name="api"}[5m])`
// returns a non-zero metric for every Playwright test in the suite — the
// earlier past-only 60-record window slid past the Loki instant-query 5 m
// staleness lookback by the time the suite reached the Loki specs.
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
		{"metrics_histogram", "SELECT count() FROM otel_metrics_histogram"},
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
