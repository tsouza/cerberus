//go:build e2e

// Live row-count-stability assertion for the rolling re-seeder's bounded
// stale-row cleanup (issue #1527), living next to the re-seeder it tests
// rather than in test/regression (which stays pure static source-scan)
// or a new harness layer.
//
// Runs against the SAME live ClickHouse the rolling re-seeder
// (`just e2e-seed-rolling`, already running in the background by the
// time `just e2e-run` executes this suite) is continuously re-anchoring.
// It deliberately does not depend on waiting out real wall-clock time to
// prove the bound — every family's staleMargin() is ~10 minutes wide
// (see stale.go), far more than this suite's budget. Instead, for each
// family it plants a single sentinel row whose Timestamp is manually
// backdated past that family's own staleMargin() cutoff, drives a
// handful of re-seed ticks by calling seedAll (the exact function both
// this test and the background rolling process call), and asserts the
// sentinel is gone — proving the DELETE fires and is scoped to the right
// table/predicate regardless of when in the lane this test happens to
// run. A secondary check confirms the family's total row count after
// those ticks stays within a generous multiple of one tick's insert
// size, rather than growing without bound.
package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// reseedStabilityTicks is the number of seedAll ticks this test drives
// itself before checking row-count bounds — small enough to keep the
// test fast, large enough to exercise the DELETE-then-INSERT cycle more
// than once per family.
const reseedStabilityTicks = 3

// reseedStabilityTickInterval only has to be long enough for
// ClickHouse's now64(9) to advance measurably between this test's own
// ticks; it does not need to match the production
// --re-seed-interval=30s (docker-compose.yml / Justfile's
// e2e-seed-rolling).
const reseedStabilityTickInterval = 2 * time.Second

// reseedStabilitySentinelSlack pushes a sentinel's backdated Timestamp
// safely past its family's own staleMargin(), so the very first tick's
// DELETE — whichever process runs it first, this test's own seedAll or
// the concurrently-running background rolling re-seeder — is guaranteed
// to catch it.
const reseedStabilitySentinelSlack = 5 * time.Second

// reseedStabilityBoundSlackTicks is the generous multiple applied to a
// family's own single-tick row count when bounding its post-test total:
// covers this test's own reseedStabilityTicks plus whatever the
// concurrently-running background rolling re-seeder (30s cadence) could
// plausibly add during this test's short run.
const reseedStabilityBoundSlackTicks = reseedStabilityTicks + 3

// Per-family row counts, expressed as multiples of the SAME
// cadence/sample declarations stale.go derives its DELETE margins from
// (see main.go / showcase_logql.go for the underlying `FROM numbers(N)`
// INSERTs), rather than independently hand-picked totals.
const (
	// up (x2 job series) + target_info (x2) + showcase_flapping (x1) +
	// showcase_multilabel (x3 colors).
	reseedStabilityGaugeRowsPerTick = 8 * metricsNarrowSamples
	// http_server_request_duration_count + showcase_restarting_total.
	reseedStabilitySumRowsPerTick = 2 * metricsWideSamples
	// http_server_request_duration.
	reseedStabilityHistogramRowsPerTick = metricsWideSamples
	// showcase_latency_exp_hist.
	reseedStabilityExpHistRowsPerTick = metricsNarrowSamples
	// base 3-service seed + the five showcase-logql streams.
	reseedStabilityLogsRowsPerTick = 6 * logsSamples
	// insertTracesSQL's fixed 7-row VALUES list (3 traces, not a
	// numbers()-driven family — see tracesMinOffset/tracesMaxOffset in
	// stale.go).
	reseedStabilityTracesRowsPerTick = 7
)

// Sentinel INSERTs mirror the column list of the real production INSERT
// for that table (main.go / showcase_logql.go) with a single VALUES row
// instead of a `SELECT ... FROM numbers(N)`. Each carries a
// 'reseed-stability-sentinel' marker (job / thread / sentinel attribute,
// depending on the table's shape) so the corresponding count query can
// find it precisely, and a MetricName/ServiceName/TraceId already
// covered by that table's stale-row DELETE predicate (stale.go) so
// planting it exercises the real production WHERE clause.
const (
	reseedStabilityPlantGaugeSentinelSQL = `INSERT INTO otel_metrics_gauge
  (ResourceAttributes, ServiceName, MetricName, MetricDescription, MetricUnit, Attributes, StartTimeUnix, TimeUnix, Value)
VALUES
  (map('service.name', 'reseed-stability-sentinel'), 'reseed-stability-sentinel', 'up', 'reseed stability sentinel', '1',
   map('job', 'reseed-stability-sentinel'),
   now64(9) - INTERVAL {backdate:UInt64} SECOND,
   now64(9) - INTERVAL {backdate:UInt64} SECOND,
   1.0)`

	reseedStabilityPlantSumSentinelSQL = `INSERT INTO otel_metrics_sum
  (ResourceAttributes, ServiceName, MetricName, MetricDescription, MetricUnit, Attributes, StartTimeUnix, TimeUnix, Value, Flags, AggregationTemporality, IsMonotonic)
VALUES
  (map('service.name', 'reseed-stability-sentinel'), 'reseed-stability-sentinel', 'http_server_request_duration_count', 'reseed stability sentinel', '1',
   map('job', 'reseed-stability-sentinel'),
   now64(9) - INTERVAL {backdate:UInt64} SECOND,
   now64(9) - INTERVAL {backdate:UInt64} SECOND,
   toFloat64(1), toUInt32(0), toInt32(2), true)`

	reseedStabilityPlantHistogramSentinelSQL = `INSERT INTO otel_metrics_histogram
  (ResourceAttributes, ServiceName, MetricName, MetricDescription, MetricUnit, Attributes, StartTimeUnix, TimeUnix, Count, Sum, BucketCounts, ExplicitBounds, Flags, AggregationTemporality)
VALUES
  (map('service.name', 'reseed-stability-sentinel'), 'reseed-stability-sentinel', 'http_server_request_duration', 'reseed stability sentinel', 's',
   map('job', 'reseed-stability-sentinel'),
   now64(9) - INTERVAL {backdate:UInt64} SECOND,
   now64(9) - INTERVAL {backdate:UInt64} SECOND,
   toUInt64(1), toFloat64(1), [toUInt64(1)], [toFloat64(1)], toUInt32(0), toInt32(2))`

	reseedStabilityPlantExpHistSentinelSQL = `INSERT INTO otel_metrics_exponential_histogram
  (ResourceAttributes, ServiceName, MetricName, MetricDescription, MetricUnit, Attributes, StartTimeUnix, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts, Flags, Min, Max, AggregationTemporality)
VALUES
  (map('service.name', 'reseed-stability-sentinel'), 'reseed-stability-sentinel', 'showcase_latency_exp_hist', 'reseed stability sentinel', 's',
   map('job', 'reseed-stability-sentinel'),
   now64(9) - INTERVAL {backdate:UInt64} SECOND,
   now64(9) - INTERVAL {backdate:UInt64} SECOND,
   toUInt64(1), toFloat64(1), toInt32(0), toUInt64(0), toInt32(0), [toUInt64(1)], toInt32(0), [], toUInt32(0), toFloat64(0), toFloat64(1), toInt32(2))`

	// TraceId/SpanId are a plain 32/16-char hex sentinel — the logs
	// stale-row DELETE scopes on ServiceName only, not TraceId.
	reseedStabilityPlantLogsSentinelSQL = `INSERT INTO otel_logs
  (Timestamp, TraceId, SpanId, SeverityText, SeverityNumber, ServiceName, Body, ResourceAttributes, LogAttributes)
VALUES
  (now64(9) - INTERVAL {backdate:UInt64} SECOND,
   'ff000000000000000000000000000000', 'ff00000000000000',
   'INFO', 9, 'api', 'reseed stability sentinel',
   map('service_name', 'api'),
   map('thread', 'reseed-stability-sentinel'))`

	// TraceId starts with the same 'a0...' prefix
	// deleteStaleBaseTracesSQL's `LIKE 'a00000000000000000000000000000%'`
	// scopes on, so planting it exercises the real predicate.
	reseedStabilityPlantTracesSentinelSQL = `INSERT INTO otel_traces
  (Timestamp, TraceId, SpanId, ParentSpanId, SpanName, SpanKind, ServiceName, ResourceAttributes, SpanAttributes, Duration, StatusCode)
VALUES
  (now64(9) - INTERVAL {backdate:UInt64} SECOND,
   'a00000000000000000000000000000ff', 'ff00000000000000', '',
   'reseed-stability-sentinel', 'Internal', 'reseed-stability-sentinel',
   map('service.name', 'reseed-stability-sentinel'),
   map('sentinel', 'reseed-stability-sentinel'),
   1000000, 'Ok')`
)

// reseedStabilityFamily bundles one fixture family's staleness margin,
// expected per-tick row count, and the three SQL statements the test
// needs: plant a backdated sentinel, count that sentinel, count the
// family's total in-scope rows.
type reseedStabilityFamily struct {
	name             string
	margin           time.Duration
	rowsPerTick      int
	plantSentinelSQL string
	countSentinelSQL string
	countFamilySQL   string
}

var reseedStabilityFamilies = []reseedStabilityFamily{
	{
		name:             "metrics-gauge-shaped",
		margin:           metricsNarrowStaleMargin,
		rowsPerTick:      reseedStabilityGaugeRowsPerTick,
		plantSentinelSQL: reseedStabilityPlantGaugeSentinelSQL,
		countSentinelSQL: `SELECT count() FROM otel_metrics_gauge WHERE Attributes['job'] = 'reseed-stability-sentinel'`,
		countFamilySQL:   `SELECT count() FROM otel_metrics_gauge WHERE MetricName IN ('up', 'target_info', 'showcase_flapping', 'showcase_multilabel')`,
	},
	{
		name:             "metrics-sum",
		margin:           metricsWideStaleMargin,
		rowsPerTick:      reseedStabilitySumRowsPerTick,
		plantSentinelSQL: reseedStabilityPlantSumSentinelSQL,
		countSentinelSQL: `SELECT count() FROM otel_metrics_sum WHERE Attributes['job'] = 'reseed-stability-sentinel'`,
		countFamilySQL:   `SELECT count() FROM otel_metrics_sum WHERE MetricName IN ('http_server_request_duration_count', 'showcase_restarting_total')`,
	},
	{
		name:             "metrics-histogram",
		margin:           metricsWideStaleMargin,
		rowsPerTick:      reseedStabilityHistogramRowsPerTick,
		plantSentinelSQL: reseedStabilityPlantHistogramSentinelSQL,
		countSentinelSQL: `SELECT count() FROM otel_metrics_histogram WHERE Attributes['job'] = 'reseed-stability-sentinel'`,
		countFamilySQL:   `SELECT count() FROM otel_metrics_histogram WHERE MetricName = 'http_server_request_duration'`,
	},
	{
		name:             "metrics-exponential-histogram",
		margin:           metricsNarrowStaleMargin,
		rowsPerTick:      reseedStabilityExpHistRowsPerTick,
		plantSentinelSQL: reseedStabilityPlantExpHistSentinelSQL,
		countSentinelSQL: `SELECT count() FROM otel_metrics_exponential_histogram WHERE Attributes['job'] = 'reseed-stability-sentinel'`,
		countFamilySQL:   `SELECT count() FROM otel_metrics_exponential_histogram WHERE MetricName = 'showcase_latency_exp_hist'`,
	},
	{
		name:             "logs",
		margin:           logsStaleMargin,
		rowsPerTick:      reseedStabilityLogsRowsPerTick,
		plantSentinelSQL: reseedStabilityPlantLogsSentinelSQL,
		countSentinelSQL: `SELECT count() FROM otel_logs WHERE LogAttributes['thread'] = 'reseed-stability-sentinel'`,
		countFamilySQL:   `SELECT count() FROM otel_logs WHERE ServiceName IN ('api', 'frontend', 'db', 'gateway', 'shop', 'proxy', 'painter', 'packer')`,
	},
	{
		name:             "base-traces",
		margin:           tracesStaleMargin,
		rowsPerTick:      reseedStabilityTracesRowsPerTick,
		plantSentinelSQL: reseedStabilityPlantTracesSentinelSQL,
		countSentinelSQL: `SELECT count() FROM otel_traces WHERE SpanAttributes['sentinel'] = 'reseed-stability-sentinel'`,
		countFamilySQL:   `SELECT count() FROM otel_traces WHERE TraceId LIKE 'a00000000000000000000000000000%'`,
	},
}

// TestReSeedRowCountStability drives the rolling re-seeder's own seedAll
// a few times and asserts every fixture family's stale-row DELETE
// (stale.go) actually reaps old rows, and that row counts stay bounded
// rather than growing by a full fixture's worth on every tick forever
// (issue #1527).
func TestReSeedRowCountStability(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	conn := reseedStabilityConn(t, ctx)

	for _, fam := range reseedStabilityFamilies {
		backdate := marginSeconds(fam.margin) + uint64(reseedStabilitySentinelSlack/time.Second)
		if err := conn.Exec(ctx, fam.plantSentinelSQL, clickhouse.Named("backdate", backdate)); err != nil {
			t.Fatalf("%s: plant sentinel: %v", fam.name, err)
		}
	}

	// Drive a handful of re-seed ticks with the exact function the
	// background rolling re-seeder calls every 30s.
	for i := 0; i < reseedStabilityTicks; i++ {
		if err := seedAll(ctx, conn); err != nil {
			t.Fatalf("seedAll tick %d: %v", i, err)
		}
		if i < reseedStabilityTicks-1 {
			time.Sleep(reseedStabilityTickInterval)
		}
	}

	for _, fam := range reseedStabilityFamilies {
		fam := fam
		t.Run(fam.name, func(t *testing.T) {
			var sentinelCount uint64
			if err := conn.QueryRow(ctx, fam.countSentinelSQL).Scan(&sentinelCount); err != nil {
				t.Fatalf("count sentinel: %v", err)
			}
			if sentinelCount != 0 {
				t.Errorf("%s: sentinel row planted %ds behind now (past the %ds staleMargin) survived %d re-seed ticks — the stale-row DELETE isn't reaping old rows (issue #1527)",
					fam.name, marginSeconds(fam.margin)+uint64(reseedStabilitySentinelSlack/time.Second), marginSeconds(fam.margin), reseedStabilityTicks)
			}

			var total uint64
			if err := conn.QueryRow(ctx, fam.countFamilySQL).Scan(&total); err != nil {
				t.Fatalf("count family: %v", err)
			}
			if total == 0 {
				t.Errorf("%s: expected rows after seeding, got 0", fam.name)
			}
			bound := uint64(fam.rowsPerTick * reseedStabilityBoundSlackTicks) //nolint:gosec // G115: small, non-negative, compile-time-bounded product
			if total > bound {
				t.Errorf("%s: row count %d exceeds the bounded-duplication ceiling %d (%d rows/tick x %d ticks of slack) — growth looks unbounded",
					fam.name, total, bound, fam.rowsPerTick, reseedStabilityBoundSlackTicks)
			}
		})
	}
}

// reseedStabilityConn opens a connection using the same CH_ADDR /
// CH_DATABASE / CH_USERNAME / CH_PASSWORD contract as run() in main.go.
// `just e2e-run` (which drives this suite) exports these against the
// same port-forward `just e2e-seed-rolling` already opened, so a live
// ClickHouse is always reachable by the time this test runs.
func reseedStabilityConn(t *testing.T, ctx context.Context) driver.Conn {
	t.Helper()

	addr := os.Getenv("CH_ADDR")
	if addr == "" {
		t.Fatalf("CH_ADDR is required (host:port of the ClickHouse native port) — run via `just e2e-run` after `just e2e-seed-rolling`")
	}
	database := os.Getenv("CH_DATABASE")
	if database == "" {
		database = "otel"
	}

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{addr},
		Auth: clickhouse.Auth{
			Database: database,
			Username: os.Getenv("CH_USERNAME"),
			Password: os.Getenv("CH_PASSWORD"),
		},
		DialTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("open clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	pingCtx, pingCancel := context.WithTimeout(ctx, 10*time.Second)
	defer pingCancel()
	if err := conn.Ping(pingCtx); err != nil {
		t.Fatalf("ping %s: %v", addr, err)
	}
	return conn
}
