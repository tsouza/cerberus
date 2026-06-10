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
//	go run ./test/e2e/seed/cmd/seed                            # one-shot
//	go run ./test/e2e/seed/cmd/seed --re-seed-interval=30s     # rolling
//
// (typically invoked through `just e2e-seed`).
//
// # Rolling re-seeder
//
// When `--re-seed-interval` is non-zero the program performs the initial
// seed and then stays alive, re-running every INSERT once per tick. Each
// re-insert re-anchors the metric/log windows on the new `now64(9)`, so
// the dataset slides with wall-clock time. This replaced the previous
// "widen the static seed window every time Playwright gains runtime"
// arms-race (PRs #590, #615, #617, #693): with fresh data arriving
// continuously, the static window only has to cover the gap between two
// ticks (30 s) plus the longest query lookback (5 m) plus headroom.
//
// The previous static window spanned ±15 min around the initial seed
// timestamp to survive a ~12 min Playwright suite. The rolling re-seeder
// drops that to ±5 min: any query at `t = seed + N min` sees a fresh
// re-anchored window centered on `seed + N min - δ` where δ ≤ 30 s.
//
// SIGTERM / SIGINT triggers a clean shutdown — the Playwright fixture
// (or `just e2e-down`) signals teardown and the goroutine exits before
// the connection closes.
//
// Implementation note: every INSERT below uses unqualified table names. The
// `Database` field set on the clickhouse-go Auth struct resolves them on the
// server side, so no `fmt.Sprintf("INSERT INTO %s.foo", database)` needed —
// keeps the seeder on the right side of the "no Sprintf-on-SQL" rule
// (CLAUDE.md § "No raw SQL strings").
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tsouza/cerberus/internal/schema/ddl"
)

func main() {
	reSeedInterval := flag.Duration("re-seed-interval", 0,
		"if non-zero, after the initial seed re-insert all rows every interval "+
			"(re-anchoring the metric/log windows on the new now64(9)) until "+
			"SIGTERM/SIGINT. The Playwright fixture passes 30s.")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := run(ctx, *reSeedInterval); err != nil {
		log.Fatalf("seed: %v", err)
	}
}

func run(ctx context.Context, reSeedInterval time.Duration) error {
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

	if err := seedAll(ctx, conn); err != nil {
		return err
	}

	if err := verifyRowcounts(ctx, conn); err != nil {
		return fmt.Errorf("verify rowcounts: %w", err)
	}
	log.Printf("seed: done")

	if reSeedInterval <= 0 {
		return nil
	}

	log.Printf("seed: entering rolling re-seed loop (interval=%s; SIGTERM/SIGINT to stop)", reSeedInterval)
	ticker := time.NewTicker(reSeedInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("seed: rolling loop exiting on %v", ctx.Err())
			return nil
		case t := <-ticker.C:
			// Use a fresh background context for the re-seed body so a
			// teardown signal mid-INSERT lets the in-flight statement
			// finish rather than leaving the table half-written; the
			// loop itself still observes ctx.Done() above on the next
			// iteration.
			tickCtx, tickCancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := seedAll(tickCtx, conn); err != nil {
				tickCancel()
				log.Printf("seed: re-seed tick at %s failed: %v (continuing)", t.Format(time.RFC3339), err)
				continue
			}
			tickCancel()
			log.Printf("seed: re-seed tick at %s ok", t.Format(time.RFC3339))
		}
	}
}

// seedAll runs every INSERT once against the live connection. The
// metrics + logs constants below all use `now64(9)` for their time
// columns, so re-running this function re-anchors the seed window on
// the current wall-clock time without any further parameterisation.
func seedAll(ctx context.Context, conn driver.Conn) error {
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
	return nil
}

// Static SQL — no string interpolation. Table names are unqualified; the
// clickhouse-go Auth.Database resolves them server-side.
//
// Window sizing rationale (post-rolling-reseeder, 2026-05-22):
// The seed window only has to cover the gap between two re-seed ticks
// (30 s) plus the longest query lookback (Prom/Loki 5 m staleness) plus
// headroom. ±5 min is comfortably enough on both sides of `now`. Before
// the rolling re-seeder landed, the window was widened to ±15 min in
// four successive PRs (#590, #615, #617, #693) to absorb the entire
// ~12 min Playwright suite drift — see the package doc comment for the
// arms-race history that motivated the switch.
const (
	// `up` is seeded as a 10-minute sliding window of samples centred on
	// the (current) seed timestamp: 40 samples per series at 15 s cadence
	// = 80 rows spanning [seed_now - 300 s, seed_now + 285 s]. The
	// rolling re-seeder re-runs this INSERT every 30 s, so a query at
	// any wall-clock time sees a window that is at most 30 s stale.
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
    now64(9) + INTERVAL ((number - 20) * 15) SECOND,
    now64(9) + INTERVAL ((number - 20) * 15) SECOND,
    1.0
FROM numbers(40)
UNION ALL
SELECT
    map('service.name', 'db'),
    'db',
    'up',
    'Is the scrape target up',
    '1',
    map('job', 'db'),
    now64(9) + INTERVAL ((number - 20) * 15) SECOND,
    now64(9) + INTERVAL ((number - 20) * 15) SECOND,
    0.0
FROM numbers(40)`

	// 600 samples at 1 s cadence cover a 10-minute window centred on
	// the (current) seed timestamp: [seed_now - 300 s, seed_now + 299 s].
	// The rolling re-seeder re-runs this INSERT every 30 s, so any 1m /
	// 5m `rate()` window over `eval_time` keeps ≥1 sample inside the
	// window regardless of how long Playwright has been running.
	//
	// The value formula `1000 + number * 5` is monotone-with-time
	// (number = 0 is the earliest sample at seed_now - 300 s; number
	// = 599 is the latest at seed_now + 299 s), which is the shape
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
    now64(9) + INTERVAL (number - 300) SECOND,
    now64(9) + INTERVAL (number - 300) SECOND,
    toFloat64(1000 + number * 5),
    toUInt32(0),
    toInt32(2),
    true
FROM numbers(600)`

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
	// Shape mirrors `insertSumSQL`: 600 samples at 1 s cadence covering
	// [seed_now - 300 s, seed_now + 299 s] so any 1m / 5m `rate()` window
	// retains ≥2 samples relative to the current (rolling) seed anchor.
	// Count grows monotonically with sample index (100 + number * 5) so
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
    now64(9) + INTERVAL (number - 300) SECOND,
    now64(9) + INTERVAL (number - 300) SECOND,
    toUInt64(100 + number * 5),
    toFloat64((100 + number * 5) * 5) / 1000,
    [toUInt64(10), toUInt64(20), toUInt64(30), toUInt64(40)],
    [toFloat64(0.1), toFloat64(0.5), toFloat64(1.0)],
    toUInt32(0),
    toInt32(2)
FROM numbers(600)`

	// otel_logs seed spans a 10-minute window centred on (current)
	// seed_now: [seed_now - 300 s, seed_now + 285 s], with 40 rows at
	// 15 s cadence across 3 services. The rolling re-seeder re-runs
	// this INSERT every 30 s, so every Loki `/query` instant request
	// (5 m staleness lookback, see internal/api/loki/handler.go:235)
	// finds ≥1 row inside the lookback regardless of suite drift.
	//
	// The column list deliberately omits `TimestampTime`: upstream's
	// clickhouseexporter removed the column from the logs DDL in
	// v0.150.0. Before the fork bump to the v0.152 templates, which
	// schema `otel_logs` carried depended on who created it first —
	// this seeder's ddl.Apply (then on legacy fork templates with the
	// column present + materialized from Timestamp) or the k3d
	// otel-collector's own exporter (0.152.x, column gone). Cerberus's
	// startup warmup (#712) made the collector reliably win that race,
	// and an INSERT naming the column hard-failed against the new
	// schema ("No such column TimestampTime"). ddl.Apply now renders
	// the same column-free v0.152 schema as the collector, so the
	// column never exists; the INSERT keeps omitting it.
	insertLogsSQL = `INSERT INTO otel_logs
  (Timestamp, TraceId, SpanId, SeverityText, SeverityNumber, ServiceName, Body, ResourceAttributes, LogAttributes)
SELECT
    now64(9) + INTERVAL ((number - 20) * 15) SECOND AS ts,
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
FROM numbers(40)`

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

	// Showcase-PromQL seed shapes (PR feat/showcase-promql). The
	// showcase-promql dashboard's feature panels need data shapes the
	// dogfood self-telemetry can't guarantee deterministically:
	//
	//   - showcase_restarting_total — a counter that RESETS: the value
	//     follows a sawtooth (4·(number mod 150)) so every 5m window
	//     contains ≥1 reset and `resets()` returns a non-zero series.
	//   - showcase_flapping — a gauge alternating 0/1 every 15 s so
	//     `changes()` / `delta()` / `idelta()` have real movement.
	//   - showcase_multilabel — three series with a 3-key label set
	//     (color / shape / env) for `label_replace` / `label_join` /
	//     `count_values` and the vector-matching panels.
	//   - showcase_latency_exp_hist — exponential (native) histogram
	//     rows in otel_metrics_exp_histogram; the `_exp_hist` suffix
	//     routes `histogram_quantile(φ, showcase_latency_exp_hist)`
	//     onto the native-quantile lowering (schema.ExpHistogramSuffix).
	//
	// All four follow the rolling re-seed discipline: windows centred
	// on now64(9) so each 30 s tick re-anchors them.
	insertShowcaseResetsSQL = `INSERT INTO otel_metrics_sum
  (ResourceAttributes, ServiceName, MetricName, MetricDescription, MetricUnit, Attributes, StartTimeUnix, TimeUnix, Value, Flags, AggregationTemporality, IsMonotonic)
SELECT
    map('service.name', 'api'),
    'api',
    'showcase_restarting_total',
    'Sawtooth counter that resets every 150s',
    '1',
    map('job', 'api'),
    now64(9) + INTERVAL (number - 300) SECOND,
    now64(9) + INTERVAL (number - 300) SECOND,
    toFloat64((number % 150) * 4),
    toUInt32(0),
    toInt32(2),
    true
FROM numbers(600)`

	insertShowcaseFlappingSQL = `INSERT INTO otel_metrics_gauge
  (ResourceAttributes, ServiceName, MetricName, MetricDescription, MetricUnit, Attributes, StartTimeUnix, TimeUnix, Value)
SELECT
    map('service.name', 'api'),
    'api',
    'showcase_flapping',
    'Gauge that flaps between 0 and 1 every sample',
    '1',
    map('job', 'api'),
    now64(9) + INTERVAL ((number - 20) * 15) SECOND,
    now64(9) + INTERVAL ((number - 20) * 15) SECOND,
    toFloat64(number % 2)
FROM numbers(40)`

	insertShowcaseMultilabelSQL = `INSERT INTO otel_metrics_gauge
  (ResourceAttributes, ServiceName, MetricName, MetricDescription, MetricUnit, Attributes, StartTimeUnix, TimeUnix, Value)
SELECT
    map('service.name', 'api'),
    'api',
    'showcase_multilabel',
    'Three series with a multi-key label set',
    '1',
    map(
        'job', 'api',
        'color', arrayElement(['red', 'green', 'blue'], (number % 3) + 1),
        'shape', arrayElement(['circle', 'square', 'triangle'], (number % 3) + 1),
        'env', arrayElement(['dev', 'staging', 'prod'], (number % 3) + 1)
    ),
    now64(9) + INTERVAL ((intDiv(number, 3) - 20) * 15) SECOND,
    now64(9) + INTERVAL ((intDiv(number, 3) - 20) * 15) SECOND,
    toFloat64((number % 3) + 1)
FROM numbers(120)`

	// Exponential-histogram rows. Scale=0 means base-2 buckets
	// ((2^(i+PositiveOffset-1), 2^(i+PositiveOffset)]); the per-row
	// bucket counts grow with the sample index so the cumulative
	// distribution is monotone, Count = ZeroCount + sum(buckets), and
	// the native-quantile midpoint estimation always has mass to walk.
	insertShowcaseExpHistSQL = `INSERT INTO otel_metrics_exp_histogram
  (ResourceAttributes, ServiceName, MetricName, MetricDescription, MetricUnit, Attributes, StartTimeUnix, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts, Flags, Min, Max, AggregationTemporality)
SELECT
    map('service.name', 'api'),
    'api',
    'showcase_latency_exp_hist',
    'Exponential (native) histogram of synthetic latencies',
    's',
    map('job', 'api'),
    now64(9) + INTERVAL ((number - 20) * 15) SECOND,
    now64(9) + INTERVAL ((number - 20) * 15) SECOND,
    toUInt64(46 * (number + 1)),
    toFloat64(120 * (number + 1)) / 10,
    toInt32(0),
    toUInt64(1 * (number + 1)),
    toInt32(0),
    arrayMap(x -> toUInt64(x * (number + 1)), [5, 10, 15, 10, 5]),
    toInt32(0),
    [],
    toUInt32(0),
    toFloat64(0),
    toFloat64(16),
    toInt32(2)
FROM numbers(40)`
)

// insertMetrics inserts the two `up` gauge series + 600 counter samples for
// rate(). Both seeds span a 10-minute window centred on the (current) seed
// timestamp — the gauge with 40 samples × 15 s and the counter / histogram
// with 600 samples × 1 s. The rolling re-seeder re-runs this every 30 s so
// any 1m / 5m `rate()` or subquery in any Playwright spec retains ≥2 samples
// in its lookback window regardless of how much suite runtime has elapsed.
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
	if err := conn.Exec(ctx, insertShowcaseResetsSQL); err != nil {
		return fmt.Errorf("showcase resets counter: %w", err)
	}
	if err := conn.Exec(ctx, insertShowcaseFlappingSQL); err != nil {
		return fmt.Errorf("showcase flapping gauge: %w", err)
	}
	if err := conn.Exec(ctx, insertShowcaseMultilabelSQL); err != nil {
		return fmt.Errorf("showcase multilabel gauge: %w", err)
	}
	if err := conn.Exec(ctx, insertShowcaseExpHistSQL); err != nil {
		return fmt.Errorf("showcase exp histogram: %w", err)
	}
	return nil
}

// insertLogs inserts 40 log records across 3 services spanning a 10-minute
// window centred on the (current) seed timestamp (15 s cadence). LogQL
// `{service_name="api"}` returns rows and `rate({service_name="api"}[5m])`
// returns a non-zero metric for every Playwright test in the suite — the
// rolling re-seeder keeps the window slid up to wall-clock now.
//
// Uses the underscored `service_name` map key because LogQL's matcher.Name is
// kept verbatim in cerberus's labelMatcherToExpr; the Prom/OTel naming bridge
// (`service_name` ↔ `service.name`) is not implemented.
func insertLogs(ctx context.Context, conn driver.Conn) error {
	if err := conn.Exec(ctx, insertLogsSQL); err != nil {
		return err
	}
	// Showcase-LogQL streams (gateway / shop / proxy / painter /
	// packer) — see showcase_logql.go for the per-stream shapes the
	// showcase-logql dashboard's parser / filter / unwrap panels need.
	return insertShowcaseLogQLLogs(ctx, conn)
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
		{"metrics_exp_hist", "SELECT count() FROM otel_metrics_exp_histogram"},
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
