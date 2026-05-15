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
//
// Label schema matches the PromLabs demo service that the upstream
// `prometheus/compliance/promql/promql-test-queries.yml` was written
// against. The compatibility tester hits `on(instance, job, type)` and
// equivalent matchers; series tagged with `host=host-a|host-b` only would
// share the empty match group `{}` and trip the "duplicate series for the
// match group" abort in the upstream tester. Canonical instance values
// are `demo.promlabs.com:10000` / `:10001` / `:10002`; every series
// carries `job=demo` (the Prom-default label PromLabs's scrape config
// would inject). `ResourceAttributes` stays `service.name=demo` — that's
// the OTel resource layer and is unrelated to the Prom-side wire labels
// the tester matches on.
var fixtureInserts = []namedStmt{
	// demo_cpu_usage_seconds_total: 3 instances × 3 modes = 9 series, counters.
	//
	// CROSS JOIN the instance + mode dimensions against the step axis so every
	// (instance, mode) pair gets one sample per step. A `(number % 3)`
	// derivation for both dimensions would correlate them — only 3 series
	// (not 9) would land in CH and the suite's `by(instance, mode)` queries
	// would silently degenerate.
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
            map('instance', instance, 'job', 'demo', 'mode', mode),
            toDateTime64({anchor:String}, 9),
            toDateTime64({anchor:String}, 9) + INTERVAL step * 15 SECOND,
            toFloat64(step + (instance_idx * 1000) + (mode_idx * 100)),
            0,
            2,
            true
        FROM (
            SELECT step, instance, instance_idx, mode, mode_idx
            FROM (SELECT number AS step FROM numbers({steps:UInt64})) AS s
            CROSS JOIN (
                SELECT arrayJoin(
                    ['demo.promlabs.com:10000','demo.promlabs.com:10001','demo.promlabs.com:10002']
                ) AS instance,
                indexOf(
                    ['demo.promlabs.com:10000','demo.promlabs.com:10001','demo.promlabs.com:10002'],
                    instance) - 1 AS instance_idx
            ) AS i
            CROSS JOIN (
                SELECT arrayJoin(['user','system','idle']) AS mode,
                indexOf(['user','system','idle'], mode) - 1 AS mode_idx
            ) AS m
        )`,
	},
	// demo_memory_usage_bytes: 3 instances × 4 types = 12 series, gauge.
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
            map('instance', instance, 'job', 'demo', 'type', type),
            toDateTime64({anchor:String}, 9),
            toDateTime64({anchor:String}, 9) + INTERVAL step * 15 SECOND,
            toFloat64(2 * 1024 * 1024 * 1024 + (step * 1024) + (instance_idx * 10000000) + (type_idx * 1000000))
        FROM (
            SELECT step, instance, instance_idx, type, type_idx
            FROM (SELECT number AS step FROM numbers({steps:UInt64})) AS s
            CROSS JOIN (
                SELECT arrayJoin(
                    ['demo.promlabs.com:10000','demo.promlabs.com:10001','demo.promlabs.com:10002']
                ) AS instance,
                indexOf(
                    ['demo.promlabs.com:10000','demo.promlabs.com:10001','demo.promlabs.com:10002'],
                    instance) - 1 AS instance_idx
            ) AS i
            CROSS JOIN (
                SELECT arrayJoin(['cached','free','buffers','used']) AS type,
                indexOf(['cached','free','buffers','used'], type) - 1 AS type_idx
            ) AS t
        )`,
	},
	// demo_http_requests_total: 2 instances × 2 methods × 2 paths × 2 statuses = 16 series,
	// counter reset.
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
            map('instance', instance, 'job', 'demo',
                'method', method, 'path', path, 'status', status),
            toDateTime64({anchor:String}, 9),
            toDateTime64({anchor:String}, 9) + INTERVAL step * 15 SECOND,
            if(step < 120,
                toFloat64(step * 10 + (instance_idx * 1000) + (method_idx * 500) + (path_idx * 250) + (status_idx * 100)),
                toFloat64((step - 120) * 10 + (instance_idx * 1000) + (method_idx * 500) + (path_idx * 250) + (status_idx * 100))),
            0,
            2,
            true
        FROM (
            SELECT step, instance, instance_idx, method, method_idx, path, path_idx, status, status_idx
            FROM (SELECT number AS step FROM numbers({steps:UInt64})) AS s
            CROSS JOIN (
                SELECT arrayJoin(['demo.promlabs.com:10000','demo.promlabs.com:10001']) AS instance,
                indexOf(['demo.promlabs.com:10000','demo.promlabs.com:10001'], instance) - 1 AS instance_idx
            ) AS i
            CROSS JOIN (
                SELECT arrayJoin(['GET','POST']) AS method,
                indexOf(['GET','POST'], method) - 1 AS method_idx
            ) AS me
            CROSS JOIN (
                SELECT arrayJoin(['/api','/web']) AS path,
                indexOf(['/api','/web'], path) - 1 AS path_idx
            ) AS p
            CROSS JOIN (
                SELECT arrayJoin(['200','500']) AS status,
                indexOf(['200','500'], status) - 1 AS status_idx
            ) AS st
        )`,
	},
	// demo_disk_usage_bytes: 2 instances × 2 devices = 4 series, gauge.
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
            map('instance', instance, 'job', 'demo', 'device', device),
            toDateTime64({anchor:String}, 9),
            toDateTime64({anchor:String}, 9) + INTERVAL step * 15 SECOND,
            toFloat64(10 * 1024 * 1024 * 1024 + step * (device_idx + 1) * 1024 + (instance_idx * 4096))
        FROM (
            SELECT step, instance, instance_idx, device, device_idx
            FROM (SELECT number AS step FROM numbers({steps:UInt64})) AS s
            CROSS JOIN (
                SELECT arrayJoin(['demo.promlabs.com:10000','demo.promlabs.com:10001']) AS instance,
                indexOf(['demo.promlabs.com:10000','demo.promlabs.com:10001'], instance) - 1 AS instance_idx
            ) AS i
            CROSS JOIN (
                SELECT arrayJoin(['/dev/sda1','/dev/sda2']) AS device,
                indexOf(['/dev/sda1','/dev/sda2'], device) - 1 AS device_idx
            ) AS d
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
            map('instance', instance, 'job', 'demo', 'device', device),
            toDateTime64({anchor:String}, 9),
            toDateTime64({anchor:String}, 9) + INTERVAL step * 15 SECOND,
            toFloat64(100 * 1024 * 1024 * 1024)
        FROM (
            SELECT step, instance, device
            FROM (SELECT number AS step FROM numbers({steps:UInt64})) AS s
            CROSS JOIN (
                SELECT arrayJoin(['demo.promlabs.com:10000','demo.promlabs.com:10001']) AS instance
            ) AS i
            CROSS JOIN (
                SELECT arrayJoin(['/dev/sda1','/dev/sda2']) AS device
            ) AS d
        )`,
	},
	// demo_num_cpus: 3 instances, gauge. Value = 4 cores per instance, constant.
	//
	// Originally absent from the seed (see the cerberus-test-queries.yml header
	// for the removal rationale); restored to cover the 28 query mentions in
	// the test file plus the 3 `should_fail: true` label_replace / label_join
	// entries that the header documented as gated on this metric returning
	// non-empty data.
	{
		name: "demo_num_cpus",
		sql: `INSERT INTO otel_metrics_gauge
            (ResourceAttributes, MetricName, MetricDescription, MetricUnit,
             Attributes, StartTimeUnix, TimeUnix, Value)
        SELECT
            map('service.name', 'demo'),
            'demo_num_cpus',
            'Number of CPU cores on the target',
            '1',
            map('instance', instance, 'job', 'demo'),
            toDateTime64({anchor:String}, 9),
            toDateTime64({anchor:String}, 9) + INTERVAL step * 15 SECOND,
            4.0
        FROM (
            SELECT step, instance
            FROM (SELECT number AS step FROM numbers({steps:UInt64})) AS s
            CROSS JOIN (
                SELECT arrayJoin(
                    ['demo.promlabs.com:10000','demo.promlabs.com:10001','demo.promlabs.com:10002']
                ) AS instance
            ) AS i
        )`,
	},
	// up: 3 instances, all up.
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
            toDateTime64({anchor:String}, 9) + INTERVAL step * 15 SECOND,
            1.0
        FROM (
            SELECT step, instance
            FROM (SELECT number AS step FROM numbers({steps:UInt64})) AS s
            CROSS JOIN (
                SELECT arrayJoin(
                    ['demo.promlabs.com:10000','demo.promlabs.com:10001','demo.promlabs.com:10002']
                ) AS instance
            ) AS i
        )`,
	},
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
