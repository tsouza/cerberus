// Command seed loads the deterministic OTel fixture used by the PromQL
// compatibility harness.
//
// It does two things:
//
//  1. Applies the upstream OTel ClickHouse Exporter DDL (metrics signal) via
//     internal/schema/ddl, so the harness's schema can't drift from what
//     cerberus's auto-create path produces.
//  2. Runs the INSERTs that materialize the fixture series — counters,
//     gauges, a counter reset, sparse series, and `up` — at 1h × 15s
//     resolution anchored at 2026-05-11T00:00:00Z so report diffs are
//     reproducible.
//
// Replaces the previous seed.sql + seed.sh shell pair. Invoked by
// scripts/run-compatibility.sh against a docker-compose ClickHouse exposed
// on localhost:29000 (override via CERBERUS_CH_ADDR).
//
// The fixture covers the metrics enumerated in upstream
// promql-test-queries.yml — see harness/prometheus-compliance/README for context.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// anchor matches the fixture timestamp used by every INSERT below and by
// run-compatibility.sh's TESTER_END_TIME default. Don't change one without
// changing the others — the upstream tester compares report diffs at this
// instant.
const anchor = "2026-05-11 00:00:00"

// fixtureSteps is the number of 15s samples per series. 240 × 15s = 1h —
// matches TESTER_RANGE=3600 in run-compatibility.sh.
const fixtureSteps = 240

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("seed failed", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	var (
		addr     = flag.String("addr", envOr("CERBERUS_CH_ADDR", "localhost:29000"), "ClickHouse host:port")
		database = flag.String("database", envOr("CERBERUS_CH_DATABASE", "otel"), "ClickHouse database")
		username = flag.String("user", envOr("CERBERUS_CH_USERNAME", "cerberus"), "ClickHouse username")
		password = flag.String("password", envOr("CERBERUS_CH_PASSWORD", "cerberus"), "ClickHouse password")
		timeout  = flag.Duration("timeout", 60*time.Second, "dial + ready timeout")
		promURL  = flag.String("prom-remote-write", promRemoteWriteURL(),
			"Prometheus remote_write URL; set empty to skip the Prom fan-out")
	)
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	logger.Info("dialing clickhouse", "addr", *addr, "database", *database)
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{*addr},
		Auth: clickhouse.Auth{
			Database: *database,
			Username: *username,
			Password: *password,
		},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if err := waitReady(ctx, conn, logger); err != nil {
		return err
	}

	logger.Info("applying ddl", "signal", "metrics")
	cfg := ddl.Config{Database: *database}
	if err := ddl.ApplyWithConfig(ctx, conn, cfg, []ddl.Signal{ddl.Metrics}); err != nil {
		return fmt.Errorf("ddl.Apply: %w", err)
	}

	logger.Info("inserting fixture", "anchor", anchor, "steps", fixtureSteps)
	if err := insertFixture(ctx, conn); err != nil {
		return fmt.Errorf("insert fixture: %w", err)
	}

	if *promURL != "" {
		logger.Info("mirroring fixture into prometheus via remote_write", "url", *promURL)
		if err := remoteWriteFixture(ctx, conn, *promURL, logger); err != nil {
			return fmt.Errorf("prom remote_write: %w", err)
		}
	} else {
		logger.Info("skipping prom remote_write fan-out (empty URL)")
	}

	logger.Info("seed done")
	return nil
}

// waitReady polls SELECT 1 until ClickHouse answers or ctx expires. The
// compose healthcheck already gates this, but the seeder may be invoked
// from run-compatibility.sh against a freshly started container — the
// extra poll absorbs the ~1s tail where ping passes but Exec doesn't.
func waitReady(ctx context.Context, conn driver.Conn, logger *slog.Logger) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := conn.Exec(ctx, "SELECT 1")
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("clickhouse not ready: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		logger.Debug("waiting for clickhouse", "err", err)
	}
}

// insertFixture writes the six logical series families used by the
// PromQL-compliance suite. Each block mirrors a section of the previous
// seed.sql; the SELECT-from-numbers shape is preserved so the resulting
// rows are byte-identical to the prior fixture. Column lists are explicit
// so the upstream-DDL columns we don't populate (ServiceName, ScopeName,
// Exemplars, ...) fall back to ClickHouse defaults.
//
// SQL strings are static literals — no string concatenation, no Sprintf
// templating, no Builder.WriteSQL. The CH connection is already bound to
// the target database, so unqualified table names suffice.
func insertFixture(ctx context.Context, conn driver.Conn) error {
	for _, s := range fixtureInserts {
		if err := conn.Exec(ctx, s.sql,
			clickhouse.Named("anchor", anchor),
			clickhouse.Named("steps", uint64(fixtureSteps)),
		); err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
	}
	return nil
}

// namedStmt is a (label, SQL) pair so error messages identify which fixture
// section failed without re-parsing the SQL.
type namedStmt struct {
	name string
	sql  string
}

// fixtureInserts is the ordered list of seed INSERTs. Each SQL string is a
// static literal — see insertFixture's docstring for why.
var fixtureInserts = []namedStmt{
	// demo_cpu_usage_seconds_total: 2 hosts × 3 modes, counters.
	{
		name: "demo_cpu_usage_seconds_total",
		sql: `INSERT INTO otel_metrics_sum
            (ResourceAttributes, MetricName, MetricDescription, MetricUnit,
             Attributes, StartTimeUnix, TimeUnix, Value,
             Flags, AggregationTemporality, IsMonotonic)
        SELECT
            map('service.name', 'demo'),
            'demo_cpu_usage_seconds_total',
            'CPU seconds spent in mode',
            'seconds',
            map('host', host, 'mode', mode),
            toDateTime64({anchor:String}, 9),
            toDateTime64({anchor:String}, 9) + INTERVAL step SECOND,
            toFloat64(step + (host_idx * 100) + (mode_idx * 10)),
            0,
            2,
            true
        FROM (
            SELECT
                number AS step,
                arrayElement(['host-a','host-b'], (number % 2) + 1) AS host,
                arrayElement(['user','system','idle'], (number % 3) + 1) AS mode,
                (number % 2) AS host_idx,
                (number % 3) AS mode_idx
            FROM numbers({steps:UInt64})
        )`,
	},
	// demo_memory_usage_bytes: 2 hosts, gauge.
	{
		name: "demo_memory_usage_bytes",
		sql: `INSERT INTO otel_metrics_gauge
            (ResourceAttributes, MetricName, MetricDescription, MetricUnit,
             Attributes, StartTimeUnix, TimeUnix, Value)
        SELECT
            map('service.name', 'demo'),
            'demo_memory_usage_bytes',
            'Memory in use',
            'bytes',
            map('host', host),
            toDateTime64({anchor:String}, 9),
            toDateTime64({anchor:String}, 9) + INTERVAL step SECOND,
            toFloat64(2 * 1024 * 1024 * 1024 + (step * 1024) + (host_idx * 512))
        FROM (
            SELECT
                number AS step,
                arrayElement(['host-a','host-b'], (number % 2) + 1) AS host,
                (number % 2) AS host_idx
            FROM numbers({steps:UInt64})
        )`,
	},
	// demo_http_requests_total: 2 routes × 2 statuses, counter reset.
	{
		name: "demo_http_requests_total",
		sql: `INSERT INTO otel_metrics_sum
            (ResourceAttributes, MetricName, MetricDescription, MetricUnit,
             Attributes, StartTimeUnix, TimeUnix, Value,
             Flags, AggregationTemporality, IsMonotonic)
        SELECT
            map('service.name', 'demo'),
            'demo_http_requests_total',
            'HTTP requests by route + status',
            '1',
            map('route', route, 'status', status),
            toDateTime64({anchor:String}, 9),
            toDateTime64({anchor:String}, 9) + INTERVAL step SECOND,
            if(step < 120,
                toFloat64(step * 10 + (route_idx * 100) + (status_idx * 50)),
                toFloat64((step - 120) * 10 + (route_idx * 100) + (status_idx * 50))),
            0,
            2,
            true
        FROM (
            SELECT
                number AS step,
                arrayElement(['/api','/web'], (number % 2) + 1) AS route,
                arrayElement(['200','500'], (number % 2) + 1) AS status,
                (number % 2) AS route_idx,
                (number % 2) AS status_idx
            FROM numbers({steps:UInt64})
        )`,
	},
	// demo_disk_usage_bytes: 1 host × 2 mounts, gauge.
	{
		name: "demo_disk_usage_bytes",
		sql: `INSERT INTO otel_metrics_gauge
            (ResourceAttributes, MetricName, MetricDescription, MetricUnit,
             Attributes, StartTimeUnix, TimeUnix, Value)
        SELECT
            map('service.name', 'demo'),
            'demo_disk_usage_bytes',
            'Disk space in use',
            'bytes',
            map('host', 'host-a', 'mount', mount),
            toDateTime64({anchor:String}, 9),
            toDateTime64({anchor:String}, 9) + INTERVAL step SECOND,
            toFloat64(10 * 1024 * 1024 * 1024 + step * mount_idx * 1024)
        FROM (
            SELECT
                number AS step,
                arrayElement(['/','/data'], (number % 2) + 1) AS mount,
                ((number % 2) + 1) AS mount_idx
            FROM numbers({steps:UInt64})
        )`,
	},
	// demo_disk_total_bytes: companion to disk_usage_bytes, gauge.
	{
		name: "demo_disk_total_bytes",
		sql: `INSERT INTO otel_metrics_gauge
            (ResourceAttributes, MetricName, MetricDescription, MetricUnit,
             Attributes, StartTimeUnix, TimeUnix, Value)
        SELECT
            map('service.name', 'demo'),
            'demo_disk_total_bytes',
            'Total disk capacity',
            'bytes',
            map('host', 'host-a', 'mount', mount),
            toDateTime64({anchor:String}, 9),
            toDateTime64({anchor:String}, 9) + INTERVAL step SECOND,
            toFloat64(100 * 1024 * 1024 * 1024)
        FROM (
            SELECT
                number AS step,
                arrayElement(['/','/data'], (number % 2) + 1) AS mount
            FROM numbers({steps:UInt64})
        )`,
	},
	// up: 2 instances, both up.
	{
		name: "up",
		sql: `INSERT INTO otel_metrics_gauge
            (ResourceAttributes, MetricName, MetricDescription, MetricUnit,
             Attributes, StartTimeUnix, TimeUnix, Value)
        SELECT
            map('service.name', 'demo'),
            'up',
            'Is the scrape target up',
            '1',
            map('instance', instance, 'job', 'demo'),
            toDateTime64({anchor:String}, 9),
            toDateTime64({anchor:String}, 9) + INTERVAL step SECOND,
            1.0
        FROM (
            SELECT
                number AS step,
                arrayElement(['host-a:9100','host-b:9100'], (number % 2) + 1) AS instance
            FROM numbers({steps:UInt64})
        )`,
	},
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
