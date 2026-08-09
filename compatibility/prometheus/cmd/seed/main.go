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
// promql-test-queries.yml — see compatibility/prometheus/README for context.
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

// fixtureStepSeconds is the scrape interval every fixture series is written
// at. fixtureSteps × fixtureStepSeconds must equal TESTER_RANGE in
// scripts/run-compatibility.sh, or the tester's window runs off the fixture.
const fixtureStepSeconds = 15

// fixtureSteps is the number of samples per series. 240 × fixtureStepSeconds
// = 1h — matches TESTER_RANGE=3600 in run-compatibility.sh.
const fixtureSteps = 240

// sparseFixtureSteps ends demo_sparse_memory_bytes a quarter of the way
// into the window, while every other family runs its full length. Set
// operators between a sparse and a dense family therefore have to agree
// per evaluation timestamp — the arms overlap early and diverge later,
// so a match key that ignores the timestamp diverges from Prometheus.
const sparseFixtureSteps = fixtureSteps / 4

// batchIntervalSteps is how often the simulated batch job succeeds:
// 80 × fixtureStepSeconds = 20m. Three runs land inside the 1h window, so
// changes(...[1h]) sees two transitions, and `time() - max(...)` sweeps
// 0..1200s inside every interval — crossing the corpus's `< 1000` threshold
// in both directions rather than sitting on one side of it.
const batchIntervalSteps = 80

// batchInstanceLagSteps staggers the three instances by 60s so max() has a
// distinguishable winner AND the trailing instances' timestamps fall on the
// previous day: hour / day_of_month / day_of_week are then non-constant
// across series, which is the only way the 7 date functions are actually
// discriminated.
const batchInstanceLagSteps = 4

// intermittentPeriodSteps writes demo_intermittent_metric every 3rd step
// (45s). That is inside the 5m PromQL lookback, so ordinary sparseness
// exercises the repeat-last-sample path without producing a staleness gap.
const intermittentPeriodSteps = 3

// intermittentGapStartStep / intermittentGapSteps carve one contiguous
// blackout of 48 × fixtureStepSeconds = 12m — longer than the 5m lookback,
// so both backends must report a genuine staleness gap rather than a
// carried sample.
const (
	intermittentGapStartStep = 80
	intermittentGapSteps     = 48
)

// intermittentInstanceOffset separates the three instances' value ranges by
// more than the whole step range (240), so `topk` / `max` / `sort` order the
// series by instance deterministically instead of by an accident of ties.
const intermittentInstanceOffset = 1000

// shiftingExpHistSparsePeriodSteps writes the third instance of
// demo_shifting_latency_exp_hist every 6th step. 6 × fixtureStepSeconds =
// 90s, which straddles the two range windows the corpus asks that family
// about on purpose:
//
//	90s > 60s  → a [1m] window is narrower than the sample spacing, so that
//	             instance can NEVER hold the two samples rate() requires and
//	             is dropped from every [1m] rate;
//	90s ≤ 150s → a [5m] window always holds at least three of its samples,
//	             so it always participates in a [5m] rate.
//
// The instance is seeded at Scale -1 while the other two are at Scale 0, and
// Prometheus merges native histograms at the COARSEST schema of the group.
// Admitting the sparse instance into a [1m] rate therefore drags the merged
// schema from 0 to -1 and moves the quantile, so the two-sample floor is
// observable in the answer rather than only in the series count.
const shiftingExpHistSparsePeriodSteps = 6

// nanRunStartStep / nanRunSteps carve a 2-sample NaN-NaN run out of
// demo_gauge_with_nan_run. Two consecutive NaN samples are required to
// exercise Prometheus's changes() NaN-both-sides carve-out (#1489) — a
// single NaN surrounded by finite values only exercises the ordinary
// (already-agreeing) finite<->NaN transition.
const (
	nanRunStartStep = 100
	nanRunSteps     = 2
)

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

// insertFixture writes every logical series family the PromQL-compliance
// suite queries. Each block mirrors a section of the previous
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
		if err := conn.Exec(
			ctx, s.sql,
			clickhouse.Named("anchor", anchor),
			clickhouse.Named("steps", uint64(fixtureSteps)),
			clickhouse.Named("sparse_steps", uint64(sparseFixtureSteps)),
			clickhouse.Named("step_seconds", uint64(fixtureStepSeconds)),
			clickhouse.Named("batch_interval_steps", uint64(batchIntervalSteps)),
			clickhouse.Named("batch_lag_steps", uint64(batchInstanceLagSteps)),
			clickhouse.Named("intermittent_period_steps", uint64(intermittentPeriodSteps)),
			clickhouse.Named("intermittent_gap_start_step", uint64(intermittentGapStartStep)),
			clickhouse.Named("intermittent_gap_end_step", uint64(intermittentGapStartStep+intermittentGapSteps)),
			clickhouse.Named("intermittent_instance_offset", uint64(intermittentInstanceOffset)),
			clickhouse.Named("nan_run_start_step", uint64(nanRunStartStep)),
			clickhouse.Named("nan_run_steps", uint64(nanRunSteps)),
			clickhouse.Named("shifting_exp_hist_sparse_period_steps",
				uint64(shiftingExpHistSparsePeriodSteps)),
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
	// demo_resource_latency_seconds is a classic histogram whose namespace
	// lives only in ResourceAttributes. The compatibility query groups its
	// bucket rate by the Prom-sanitized resource label.
	{
		name: "demo_resource_latency_seconds",
		sql: `INSERT INTO otel_metrics_histogram
            (ResourceAttributes, MetricName, MetricDescription, MetricUnit,
             Attributes, StartTimeUnix, TimeUnix, Count, Sum, BucketCounts,
             ExplicitBounds)
        SELECT
            map('k8s.namespace.name', 'prod'),
            'demo_resource_latency_seconds',
            'Resource label histogram',
            'seconds',
            map('route', '/api'),
            toDateTime64({anchor:String}, 9),
            toDateTime64({anchor:String}, 9) + INTERVAL step * 15 SECOND,
            toUInt64(3 * (step + 1)),
            toFloat64(3 * (step + 1)),
            arrayMap(x -> toUInt64(step + 1), range(3)),
            [0.1, 0.5]
        FROM (SELECT number AS step FROM numbers({steps:UInt64})) AS s`,
	},
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
            toDateTime64({anchor:String}, 9) + INTERVAL step * {step_seconds:UInt64} SECOND,
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
	// demo_delta_requests_total: a DELTA-temporality (AggregationTemporality
	// = 1) counter, the direct sibling of demo_cpu_usage_seconds_total for
	// issue #1628 — same 3-instance shape, but here EVERY row is the raw
	// increase since the PREVIOUS row only (a constant `1 + instance_idx`
	// per step), not a running total. rate() / increase() over this must
	// sum those raw per-step values, not diff them Prometheus-counter-style.
	//
	// prom_remote.go's fixtureSources entry for this metric sets
	// accumulateToCumulative so the reference Prometheus mirror instead
	// receives the running SUM of these same per-step values — the
	// "equivalent cumulative-converted series" the compatibility corpus
	// query below diffs cerberus's DELTA answer against.
	{
		name: "demo_delta_requests_total",
		sql: `INSERT INTO otel_metrics_sum
            (ResourceAttributes, MetricName, MetricDescription, MetricUnit,
             Attributes, StartTimeUnix, TimeUnix, Value,
             Flags, AggregationTemporality, IsMonotonic)
        SELECT
            map('service.name', 'demo'),
            'demo_delta_requests_total',
            'HTTP requests received, per collection interval',
            'requests',
            map('instance', instance, 'job', 'demo'),
            toDateTime64({anchor:String}, 9),
            toDateTime64({anchor:String}, 9) + INTERVAL step * {step_seconds:UInt64} SECOND,
            toFloat64(1 + instance_idx),
            0,
            1,
            true
        FROM (
            SELECT step, instance, instance_idx
            FROM (SELECT number AS step FROM numbers({steps:UInt64})) AS s
            CROSS JOIN (
                SELECT arrayJoin(
                    ['demo.promlabs.com:10000','demo.promlabs.com:10001','demo.promlabs.com:10002']
                ) AS instance,
                indexOf(
                    ['demo.promlabs.com:10000','demo.promlabs.com:10001','demo.promlabs.com:10002'],
                    instance) - 1 AS instance_idx
            ) AS i
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
            toDateTime64({anchor:String}, 9) + INTERVAL step * {step_seconds:UInt64} SECOND,
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
	// demo_sparse_memory_bytes: same 12-series label shape as
	// demo_memory_usage_bytes, but only over the first quarter of the
	// window. Set operators between the two share a match signature
	// (the default signature excludes the metric name) while covering
	// different spans of the evaluation grid.
	{
		name: "demo_sparse_memory_bytes",
		sql: `INSERT INTO otel_metrics_gauge
            (ResourceAttributes, MetricName, MetricDescription, MetricUnit,
             Attributes, StartTimeUnix, TimeUnix, Value)
        SELECT
            map('service.name', 'demo'),
            'demo_sparse_memory_bytes',
            'Memory in use, reported for part of the window only',
            'bytes',
            map('instance', instance, 'job', 'demo', 'type', type),
            toDateTime64({anchor:String}, 9),
            toDateTime64({anchor:String}, 9) + INTERVAL step * {step_seconds:UInt64} SECOND,
            toFloat64(1024 * 1024 * 1024 + (step * 2048) + (instance_idx * 20000000) + (type_idx * 2000000))
        FROM (
            SELECT step, instance, instance_idx, type, type_idx
            FROM (SELECT number AS step FROM numbers({sparse_steps:UInt64})) AS s
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
            toDateTime64({anchor:String}, 9) + INTERVAL step * {step_seconds:UInt64} SECOND,
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
            toDateTime64({anchor:String}, 9) + INTERVAL step * {step_seconds:UInt64} SECOND,
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
            toDateTime64({anchor:String}, 9) + INTERVAL step * {step_seconds:UInt64} SECOND,
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
            toDateTime64({anchor:String}, 9) + INTERVAL step * {step_seconds:UInt64} SECOND,
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
	// demo_api_request_duration_seconds: 2 instances × 2 methods = 4
	// classic-histogram series. The OTel-CH exporter writes ONE row per
	// observation window under the BARE base name with Count / Sum /
	// BucketCounts / ExplicitBounds as columns; cerberus's lowering fans
	// that row out into the Prom-wire `_bucket` / `_count` / `_sum`
	// companions, so 4 CH series surface as 36 Prom series
	// (4 × [7 bucket + count + sum]).
	//
	// Geometry, stated explicitly because every quantile the corpus asks
	// for has to land strictly INSIDE a finite bucket — a rank that falls
	// exactly on a cumulative boundary makes the interpolation degenerate
	// and hides interpolation bugs behind an exact bound:
	//
	//	ExplicitBounds       = [0.1, 0.5, 1, 2.5, 5, 10]   (6 finite edges)
	//	per-step increments  = [5, 20, 60, 45, 40, 29, 1]  (7 buckets, +Inf last)
	//	cumulative           = [5, 25, 85, 130, 170, 199, 200]
	//
	// 200 observations per step per unit of `mult`. Landing ranks for the
	// quantiles the corpus drives — 0.1→20, 0.5→100, 0.75→150, 0.9→180,
	// 0.95→190, 0.99→198 — are 20, 100, 150, 180, 190, 198; none equals a
	// cumulative entry, so all six interpolate. q=1 resolves to the
	// highest finite bound (10) and q outside [0,1] to ∓Inf.
	//
	// BucketCounts is cumulative OVER TIME (`incr × (step+1) × mult`) so
	// `_bucket` behaves as a Prometheus counter under rate(). `mult` is
	// `instance_idx * 2 + method_idx + 1` ∈ {1,2,3,4}: a per-series scale
	// factor, so quantiles (a shape property) are identical across series
	// while rate() values differ — a label-mixup bug still shows.
	//
	// Sum = 509.5 × (step+1) × mult is the count-weighted sum of bucket
	// midpoints; 509.5 is exactly representable in binary, so `_sum`
	// carries no float drift.
	{
		name: "demo_api_request_duration_seconds",
		sql: `INSERT INTO otel_metrics_histogram
            (ResourceAttributes, MetricName, MetricDescription, MetricUnit,
             Attributes, StartTimeUnix, TimeUnix,
             Count, Sum, BucketCounts, ExplicitBounds,
             Flags, AggregationTemporality)
        SELECT
            map('service.name', 'demo'),
            'demo_api_request_duration_seconds',
            'API request duration',
            'seconds',
            map('instance', instance, 'job', 'demo', 'method', method),
            toDateTime64({anchor:String}, 9),
            toDateTime64({anchor:String}, 9) + INTERVAL step * {step_seconds:UInt64} SECOND,
            toUInt64(200 * (step + 1) * (instance_idx * 2 + method_idx + 1)),
            toFloat64(509.5 * (step + 1) * (instance_idx * 2 + method_idx + 1)),
            arrayMap(d -> toUInt64(d * (step + 1) * (instance_idx * 2 + method_idx + 1)),
                     [5, 20, 60, 45, 40, 29, 1]),
            [toFloat64(0.1), toFloat64(0.5), toFloat64(1), toFloat64(2.5),
             toFloat64(5), toFloat64(10)],
            0,
            2
        FROM (
            SELECT step, instance, instance_idx, method, method_idx
            FROM (SELECT number AS step FROM numbers({steps:UInt64})) AS s
            CROSS JOIN (
                SELECT arrayJoin(['demo.promlabs.com:10000','demo.promlabs.com:10001']) AS instance,
                indexOf(['demo.promlabs.com:10000','demo.promlabs.com:10001'], instance) - 1 AS instance_idx
            ) AS i
            CROSS JOIN (
                SELECT arrayJoin(['GET','POST']) AS method,
                indexOf(['GET','POST'], method) - 1 AS method_idx
            ) AS me
        )`,
	},
	// demo_latency_exp_hist: 2 instances, exponential (native) histograms.
	// The `_exp_hist` suffix is what routes the name to
	// otel_metrics_exponential_histogram (schema.Metrics.ExpHistogramSuffix);
	// on the reference side prom_remote.go mirrors these rows as native
	// histogram samples over remote-write, so the corpus's histogram_*
	// value functions compare two populated histograms rather than two
	// empty vectors.
	//
	// Geometry, stated explicitly because every property the corpus asks
	// about is a consequence of it:
	//
	//	Scale = 0                        → bucket base 2, edges at 2^k
	//	ZeroCount = 0, no negative buckets
	//	instance :10000 → PositiveOffset 0, per-step counts [1, 2, 3, 4]
	//	                  edges (1,2] (2,4] (4,8] (8,16], cumulative 1/3/6/10
	//	instance :10001 → PositiveOffset 1, per-step counts [2, 5, 3]
	//	                  edges (2,4] (4,8] (8,16], cumulative 2/7/10
	//
	// Both series observe 10 events per unit of `step + 1`, so a quantile
	// (a shape property) is stable across the window while the counts
	// themselves grow — a lowering that folded the time axis into one
	// collapse would still move the quantile. Every bucket count is
	// strictly positive: an empty interior bucket is skipped by the
	// reference's quantile iterator, which would make the case test the
	// skip path rather than the interpolation.
	//
	// The two offsets differ so a series mix-up cannot pass: :10000's
	// lowest populated edge is 1 and :10001's is 2, so the two disagree on
	// every quantile below the median.
	//
	// Sum is a per-series constant × (step + 1) — 35.0 and 50.0, both
	// exactly representable in binary, so histogram_sum / histogram_avg
	// and the Sum/Count mean that histogram_stddev centres on carry no
	// float drift. prom_remote.go mirrors the same literal.
	//
	// Labels are `instance` only — deliberately no `job` and no `type`.
	// The corpus carries label-only selectors ({type="free", instance!=…},
	// {job="demo", __name__!~…}) whose match sets are pinned by the float
	// families; a native histogram joining either group would change what
	// those cases return through a channel unrelated to what they test.
	{
		name: "demo_latency_exp_hist",
		sql: `INSERT INTO otel_metrics_exponential_histogram
            (ResourceAttributes, MetricName, MetricDescription, MetricUnit,
             Attributes, StartTimeUnix, TimeUnix,
             Count, Sum, Scale, ZeroCount,
             PositiveOffset, PositiveBucketCounts,
             NegativeOffset, NegativeBucketCounts,
             Flags, AggregationTemporality)
        SELECT
            map('service.name', 'demo'),
            'demo_latency_exp_hist',
            'Request latency as an exponential histogram',
            'seconds',
            map('instance', instance),
            toDateTime64({anchor:String}, 9),
            toDateTime64({anchor:String}, 9) + INTERVAL step * {step_seconds:UInt64} SECOND,
            toUInt64(10 * (step + 1)),
            toFloat64(if(instance_idx = 0, 35.0, 50.0) * (step + 1)),
            0,
            0,
            toInt32(instance_idx),
            arrayMap(c -> toUInt64(c * (step + 1)),
                     if(instance_idx = 0, [1, 2, 3, 4], [2, 5, 3])),
            0,
            emptyArrayUInt64(),
            0,
            2
        FROM (
            SELECT step, instance, instance_idx
            FROM (SELECT number AS step FROM numbers({steps:UInt64})) AS s
            CROSS JOIN (
                SELECT arrayJoin(['demo.promlabs.com:10000','demo.promlabs.com:10001']) AS instance,
                indexOf(['demo.promlabs.com:10000','demo.promlabs.com:10001'], instance) - 1 AS instance_idx
            ) AS i
        )`,
	},
	// demo_shifting_latency_exp_hist: 3 instances, exponential (native)
	// histograms whose RELATIVE bucket shape moves across the window.
	//
	// demo_latency_exp_hist above holds its shape fixed and scales it by
	// (step + 1). That makes it useless as an oracle for the aggregated
	// range-vector shape `histogram_quantile(phi, sum(rate(<exp_hist>[w])))`,
	// because the two candidate lowerings of that shape — the correct
	// two-stage "per-series rate, then merge" and the flat single-stage fold
	// that sums every in-window sample's raw cumulative counts — differ there
	// only by a POSITIVE SCALAR factor, and histogram_quantile reads bucket
	// RATIOS only. Same array, different scalar, identical quantile: the
	// harness would score green through the bug and through a regression of
	// it. This family removes the scalar-invariance.
	//
	// Geometry:
	//
	//	per-step INCREMENT at step s = [steps - s, 1, 1, s + 1]
	//	                             → 243 events every step, constant total,
	//	                               mass migrating lowest bucket → highest
	//	stored CUMULATIVE counts at step s
	//	  b1 = steps*(s+1) - s*(s+1)/2      b2 = b3 = s + 1
	//	  b4 = (s+1)*(s+2)/2                Count = 243*(s+1)
	//
	//	instance :10000 → Scale  0, PositiveOffset 0 → edges (1,2] (2,4] (4,8] (8,16]
	//	instance :10001 → Scale  0, PositiveOffset 1 → edges (2,4] (4,8] (8,16] (16,32]
	//	instance :10002 → Scale -1, PositiveOffset 0 → edges (1,4] (4,16] (16,64] (64,256]
	//	                  written every shiftingExpHistSparsePeriodSteps step
	//
	// Why the two candidate answers differ NUMERICALLY, by construction. Take
	// the eval step e = 200 and a [1m] window: at fixtureStepSeconds = 15 that
	// is four samples (steps 197..200), three increments, and only the two
	// dense instances clear the two-sample floor.
	//
	//	correct  per-series delta = increments at 198,199,200
	//	                          = [42,1,1,199]+[41,1,1,200]+[40,1,1,201]
	//	                          = [123, 3, 3, 600]      (729 events)
	//	wrong    per-series fold  = C(197)+C(198)+C(199)+C(200)
	//	                          = [112316, 798, 798, 80002]  (193914 events)
	//
	// Merging :10000 with :10001 (whose identical counts sit one bucket up):
	//
	//	correct  (1,2]:123 (2,4]:126 (4,8]:6 (8,16]:603 (16,32]:600
	//	         half = 729 falls in (8,16]  → q ≈ 8 * 2^(474/603)  ≈ 13.79
	//	wrong    (1,2]:112316 (2,4]:113114 (4,8]:1596 (8,16]:80800 (16,32]:80002
	//	         half = 193914 falls in (2,4] → q ≈ 2 * 2^(81598/113114) ≈ 3.30
	//
	// Different BUCKET, ~4.2x apart — a gap no interpolation convention can
	// close. The separation grows monotonically with the eval step as the mass
	// migrates, and the tester evaluates instant queries at the end of the
	// window, so the corpus reads it at its widest.
	//
	// The third instance is the two-sample floor made observable. Its 90s
	// spacing puts at most one sample in a [1m] window, so rate() must drop
	// it; its Scale -1 is coarser than the other two, and Prometheus merges a
	// group of native histograms at the group's COARSEST schema. A lowering
	// that admitted a one-sample series would therefore not merely add a
	// series — it would drag the merged schema from 0 to -1 and move the
	// merged quantile, which is the mechanism
	// test/spec/promql/histogram_quantile_native_agg_min_samples.txtar pins
	// against chDB and this family pins against a real reference.
	//
	// Sum is derived from the bucket UPPER BOUNDS rather than stated as a
	// literal, so it stays consistent with a moving shape: bucket k of a
	// series at scale <= 0 with offset o has upper bound 2^((o+k) * 2^-scale)
	// — 2/4/8/16, 4/8/16/32 and 4/16/64/256 for the three instances, all exact
	// powers of two, so histogram_sum / histogram_avg carry no float drift.
	//
	// Labels are `instance` only, for the reason demo_latency_exp_hist states.
	{
		name: "demo_shifting_latency_exp_hist",
		sql: `INSERT INTO otel_metrics_exponential_histogram
            (ResourceAttributes, MetricName, MetricDescription, MetricUnit,
             Attributes, StartTimeUnix, TimeUnix,
             Count, Sum, Scale, ZeroCount,
             PositiveOffset, PositiveBucketCounts,
             NegativeOffset, NegativeBucketCounts,
             Flags, AggregationTemporality)
        SELECT
            map('service.name', 'demo'),
            'demo_shifting_latency_exp_hist',
            'Request latency as an exponential histogram whose bucket shape moves',
            'seconds',
            map('instance', instance),
            toDateTime64({anchor:String}, 9),
            toDateTime64({anchor:String}, 9) + INTERVAL step * {step_seconds:UInt64} SECOND,
            arraySum(counts),
            arraySum(arrayMap((c, k) -> toFloat64(c) * pow(2, (offset + k) * pow(2, -scale)),
                              counts, arrayEnumerate(counts))),
            toInt32(scale),
            0,
            toInt32(offset),
            counts,
            0,
            emptyArrayUInt64(),
            0,
            2
        FROM (
            SELECT
                step,
                instance,
                if(instance = 'demo.promlabs.com:10001', 1, 0) AS offset,
                if(instance = 'demo.promlabs.com:10002', -1, 0) AS scale,
                [toUInt64({steps:UInt64} * (step + 1) - intDiv(step * (step + 1), 2)),
                 toUInt64(step + 1),
                 toUInt64(step + 1),
                 toUInt64(intDiv((step + 1) * (step + 2), 2))] AS counts
            FROM (SELECT number AS step FROM numbers({steps:UInt64})) AS s
            CROSS JOIN (
                SELECT arrayJoin(['demo.promlabs.com:10000',
                                  'demo.promlabs.com:10001',
                                  'demo.promlabs.com:10002']) AS instance
            ) AS i
            WHERE instance != 'demo.promlabs.com:10002'
               OR step % {shifting_exp_hist_sparse_period_steps:UInt64} = 0
        )`,
	},
	// demo_batch_last_success_timestamp_seconds: 3 instances, gauge whose
	// VALUE is a unix timestamp — the shape `time() - max(...)` and the
	// seven date functions are written against.
	//
	// Value = anchor_epoch + floor(step / batch_interval_steps) ×
	// batch_interval_steps × step_seconds − instance_idx ×
	// batch_lag_steps × step_seconds. It is derived from the SAME anchor
	// string as TimeUnix and parsed in the same server timezone, so the
	// sample time and the value can't diverge on a timezone difference.
	//
	// With anchor 2026-05-11 00:00:00 (a Monday) instance 0 reports
	// 00:00 / 00:20 / 00:40 on the 11th while instances 1 and 2 start at
	// 23:59 / 23:58 on the 10th (a Sunday). hour, day_of_month and
	// day_of_week are therefore non-constant across the series, which is
	// what makes the 21 `{{.dateFunc}}(... offset ...)` cases discriminate
	// anything at all.
	{
		name: "demo_batch_last_success_timestamp_seconds",
		sql: `INSERT INTO otel_metrics_gauge
            (ResourceAttributes, MetricName, MetricDescription, MetricUnit,
             Attributes, StartTimeUnix, TimeUnix, Value)
        SELECT
            map('service.name', 'demo'),
            'demo_batch_last_success_timestamp_seconds',
            'Unix timestamp of the last successful batch run',
            'seconds',
            map('instance', instance, 'job', 'demo'),
            toDateTime64({anchor:String}, 9),
            toDateTime64({anchor:String}, 9) + INTERVAL step * {step_seconds:UInt64} SECOND,
            toFloat64(
                toInt64(toUnixTimestamp(toDateTime({anchor:String})))
                + toInt64(intDiv(step, {batch_interval_steps:UInt64})
                    * {batch_interval_steps:UInt64} * {step_seconds:UInt64})
                - toInt64(instance_idx * {batch_lag_steps:UInt64} * {step_seconds:UInt64}))
        FROM (
            SELECT step, instance, instance_idx
            FROM (SELECT number AS step FROM numbers({steps:UInt64})) AS s
            CROSS JOIN (
                SELECT arrayJoin(
                    ['demo.promlabs.com:10000','demo.promlabs.com:10001','demo.promlabs.com:10002']
                ) AS instance,
                indexOf(
                    ['demo.promlabs.com:10000','demo.promlabs.com:10001','demo.promlabs.com:10002'],
                    instance) - 1 AS instance_idx
            ) AS i
        )`,
	},
	// demo_intermittent_metric: 3 instances, gauge on a sparse cadence
	// with one blackout longer than the PromQL lookback.
	//
	// Samples land every intermittentPeriodSteps (45s), which is inside
	// the 5m lookback, so the ordinary sparseness exercises the
	// repeat-last-sample path. Steps [intermittent_gap_start_step,
	// intermittent_gap_end_step) are dropped entirely — a 12m hole, longer
	// than the lookback, so both backends must report a genuine staleness
	// gap there rather than carrying the previous sample.
	//
	// Deliberate: samples sit at multiples of 45s while the tester's grid
	// is 10s-aligned from the window start, so grid points EXACTLY 300s
	// after a sample do occur (sample at 90s ↔ grid at 390s). The fixture
	// therefore exercises Prometheus's exclusive lookback boundary
	// (`t − sampleTime > 5m` ⇒ stale), not only the interior.
	{
		name: "demo_intermittent_metric",
		sql: `INSERT INTO otel_metrics_gauge
            (ResourceAttributes, MetricName, MetricDescription, MetricUnit,
             Attributes, StartTimeUnix, TimeUnix, Value)
        SELECT
            map('service.name', 'demo'),
            'demo_intermittent_metric',
            'Gauge reported on a sparse cadence with one blackout longer than the lookback',
            '1',
            map('instance', instance, 'job', 'demo'),
            toDateTime64({anchor:String}, 9),
            toDateTime64({anchor:String}, 9) + INTERVAL step * {step_seconds:UInt64} SECOND,
            toFloat64(step + (instance_idx * {intermittent_instance_offset:UInt64}))
        FROM (
            SELECT step, instance, instance_idx
            FROM (SELECT number AS step FROM numbers({steps:UInt64})) AS s
            CROSS JOIN (
                SELECT arrayJoin(
                    ['demo.promlabs.com:10000','demo.promlabs.com:10001','demo.promlabs.com:10002']
                ) AS instance,
                indexOf(
                    ['demo.promlabs.com:10000','demo.promlabs.com:10001','demo.promlabs.com:10002'],
                    instance) - 1 AS instance_idx
            ) AS i
            WHERE step % {intermittent_period_steps:UInt64} = 0
              AND (step < {intermittent_gap_start_step:UInt64}
                OR step >= {intermittent_gap_end_step:UInt64})
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
            toDateTime64({anchor:String}, 9) + INTERVAL step * {step_seconds:UInt64} SECOND,
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
	// demo_gauge_with_nan_run: 1 instance, gauge carrying a 2-sample
	// NaN-NaN run in the middle of the window. `changes()`'s Prom-parity
	// carve-out (#1489) only fires on a NaN-adjacent-to-NaN pair — a lone
	// NaN surrounded by finite values already agreed with Prometheus, so
	// that shape proves nothing. Placed well inside the window (not at
	// either edge) so every {{.range}} variant's lookback can observe it
	// from some evaluation timestamp within the tester's scan.
	{
		name: "demo_gauge_with_nan_run",
		sql: `INSERT INTO otel_metrics_gauge
            (ResourceAttributes, MetricName, MetricDescription, MetricUnit,
             Attributes, StartTimeUnix, TimeUnix, Value)
        SELECT
            map('service.name', 'demo'),
            'demo_gauge_with_nan_run',
            'Gauge with a NaN-NaN run, pinning the changes() carve-out (#1489) against a real Prometheus backend',
            '1',
            map('instance', 'demo.promlabs.com:10000', 'job', 'demo'),
            toDateTime64({anchor:String}, 9),
            toDateTime64({anchor:String}, 9) + INTERVAL step * {step_seconds:UInt64} SECOND,
            if(step >= {nan_run_start_step:UInt64} AND step < {nan_run_start_step:UInt64} + {nan_run_steps:UInt64},
                nan,
                toFloat64(step))
        FROM (SELECT number AS step FROM numbers({steps:UInt64})) AS s`,
	},
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
