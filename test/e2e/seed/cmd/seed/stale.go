package main

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Bounded stale-row cleanup for the rolling re-seeder (issue #1527).
//
// insertShowcaseTraces (showcase_traceql.go) was, until now, the ONLY
// fixture family with a stale-row DELETE: every other family re-inserted
// its window on every tick with nothing removing the previous tick's
// rows, so a long-running rolling re-seeder (`--re-seed-interval`, driven
// by `just e2e-seed-rolling` / docker-compose.yml's seed container)
// stacked one full copy of the metrics/logs/base-traces fixtures per
// tick, unboundedly, for as long as the stack stayed up.
//
// The fix mirrors the showcase-traces shape exactly (a data-anchored
// `max(Timestamp) - margin` DELETE run AFTER the INSERT, never before —
// see deleteStaleShowcaseTracesSQL's doc comment in showcase_traceql.go
// for the full race analysis this ordering avoids) for every other
// family. It accepts the same class of trade-off showcase-traces made:
// a margin wider than a family's own re-seed window bounds duplication
// to roughly (margin / tick-interval) copies rather than eliminating it
// — naive, but bounded beats unbounded, and a delta-insert redesign is
// unwarranted absent evidence the bound itself causes problems.
//
// Per-fixture margins are derived programmatically from each family's
// own declared cadence + sample-count window (staleMargin/windowSpan
// below) instead of hand-picked per-family constants — the metrics/logs
// families share a `(number - anchor) * cadence` window shape whose span
// is `(samples-1) * cadence` regardless of where the anchor sits, so one
// small helper covers all of them.

// staleMarginHeadroom is the safety slack added on top of a family's own
// window span to get its DELETE margin — the same 2s slack already
// implicit in deleteStaleShowcaseTracesSQL (18s insert spread → 20s
// margin; see that constant's doc comment for why headroom in this range
// is safe: it only has to absorb statement-gap timing and clock skew,
// not a whole extra tick). Reused here instead of re-deriving a
// per-family slack by hand.
const staleMarginHeadroom = 2 * time.Second

// windowSpan returns the width, in wall-clock time, of a
// `(number - anchor) * cadence` seeded window spanning `samples` rows:
// the distance between its earliest and latest sample. The result is
// independent of where the anchor sits inside the window, so it applies
// equally to the metrics/logs families (anchored mid-window, symmetric
// around "now") without each needing its own span computation.
func windowSpan(cadence time.Duration, samples int) time.Duration {
	return time.Duration(samples-1) * cadence
}

// staleMargin derives a fixture family's DELETE-cutoff margin from its
// own window span. The margin must exceed the span, or a freshly
// inserted tick's own oldest row (up to `span` behind that same tick's
// newest row) would be mistaken for a previous tick's stale row and
// deleted immediately after being inserted.
func staleMargin(span time.Duration) time.Duration {
	return span + staleMarginHeadroom
}

// marginSeconds converts a margin to the whole-second count the DELETE
// templates below bind as the `{margin:UInt64}` query parameter.
func marginSeconds(margin time.Duration) uint64 {
	return uint64(margin / time.Second) //nolint:gosec // G115: margin is a small compile-time-derived positive duration
}

// Re-seed window declarations. Each constant here names the exact
// cadence + sample count already baked into the corresponding `FROM
// numbers(N)` INSERT above (main.go / showcase_logql.go) — naming them
// once and feeding staleMargin(windowSpan(...)) is what keeps the
// margins below derived rather than hand-picked.
const (
	// metricsNarrowCadence/-Samples: the 15s-cadence, 40-sample
	// gauge-shaped families sharing the `(number - 20) * 15` window —
	// `up`, target_info, showcase_flapping, showcase_multilabel (all
	// otel_metrics_gauge) and showcase_latency_exp_hist
	// (otel_metrics_exponential_histogram). showcase_multilabel's
	// `numbers(120)` covers the same 40 distinct timestamps 3x over
	// (intDiv(number, 3)), so it shares this window too.
	metricsNarrowCadence = 15 * time.Second
	metricsNarrowSamples = 40

	// metricsWideCadence/-Samples: the 1s-cadence, 600-sample
	// counter/histogram-shaped families sharing the `(number - 300)`
	// window — http_server_request_duration(_count) and
	// showcase_restarting_total (otel_metrics_sum / otel_metrics_histogram).
	metricsWideCadence = 1 * time.Second
	metricsWideSamples = 600

	// logsCadence/-Samples: every otel_logs stream — the base 3-service
	// seed and the five showcase-logql streams — all share the same
	// `(number - 20) * 15` window as the narrow metrics.
	logsCadence = 15 * time.Second
	logsSamples = 40

	// tracesMinOffset/-MaxOffset are the earliest/latest `- INTERVAL n
	// SECOND` offsets used by the literal a0... trace rows in
	// insertTracesSQL (a fixed 7-row VALUES list, not a numbers()
	// formula, so its span is declared directly rather than via
	// cadence*samples).
	tracesMinOffset = 9 * time.Second
	tracesMaxOffset = 30 * time.Second
)

// Per-family margins, each derived from the window declarations above.
var (
	metricsNarrowStaleMargin = staleMargin(windowSpan(metricsNarrowCadence, metricsNarrowSamples))
	metricsWideStaleMargin   = staleMargin(windowSpan(metricsWideCadence, metricsWideSamples))
	logsStaleMargin          = staleMargin(windowSpan(logsCadence, logsSamples))
	tracesStaleMargin        = staleMargin(tracesMaxOffset - tracesMinOffset)
)

// Stale-row DELETEs, one per table these fixtures write to. Each scopes
// both the outer DELETE and the max(Timestamp) subquery to the
// MetricName/ServiceName/TraceId set this seeder owns in that table —
// unscoped would either eat rows the dogfood self-telemetry pipeline
// wrote into the same table (see showcase_traceql.go / showcase_logql.go
// doc comments) or anchor the cutoff on foreign rows. The margin is a
// bound query parameter (`{margin:UInt64}`), not a literal, so it stays
// in lockstep with the Go-side staleMargin() computation above — the
// same `{name:Type}` parameter binding already used by
// compatibility/prometheus/cmd/seed/main.go.
const (
	deleteStaleMetricsGaugeSQL = `ALTER TABLE otel_metrics_gauge DELETE
WHERE MetricName IN ('up', 'target_info', 'showcase_flapping', 'showcase_multilabel')
  AND Timestamp < (
    SELECT max(Timestamp) - INTERVAL {margin:UInt64} SECOND
    FROM otel_metrics_gauge
    WHERE MetricName IN ('up', 'target_info', 'showcase_flapping', 'showcase_multilabel')
  )`

	deleteStaleMetricsSumSQL = `ALTER TABLE otel_metrics_sum DELETE
WHERE MetricName IN ('http_server_request_duration_count', 'showcase_restarting_total')
  AND Timestamp < (
    SELECT max(Timestamp) - INTERVAL {margin:UInt64} SECOND
    FROM otel_metrics_sum
    WHERE MetricName IN ('http_server_request_duration_count', 'showcase_restarting_total')
  )`

	deleteStaleMetricsHistogramSQL = `ALTER TABLE otel_metrics_histogram DELETE
WHERE MetricName = 'http_server_request_duration'
  AND Timestamp < (
    SELECT max(Timestamp) - INTERVAL {margin:UInt64} SECOND
    FROM otel_metrics_histogram
    WHERE MetricName = 'http_server_request_duration'
  )`

	deleteStaleMetricsExpHistSQL = `ALTER TABLE otel_metrics_exponential_histogram DELETE
WHERE MetricName = 'showcase_latency_exp_hist'
  AND Timestamp < (
    SELECT max(Timestamp) - INTERVAL {margin:UInt64} SECOND
    FROM otel_metrics_exponential_histogram
    WHERE MetricName = 'showcase_latency_exp_hist'
  )`

	deleteStaleLogsSQL = `ALTER TABLE otel_logs DELETE
WHERE ServiceName IN ('api', 'frontend', 'db', 'gateway', 'shop', 'proxy', 'painter', 'packer')
  AND Timestamp < (
    SELECT max(Timestamp) - INTERVAL {margin:UInt64} SECOND
    FROM otel_logs
    WHERE ServiceName IN ('api', 'frontend', 'db', 'gateway', 'shop', 'proxy', 'painter', 'packer')
  )`

	deleteStaleBaseTracesSQL = `ALTER TABLE otel_traces DELETE
WHERE TraceId LIKE 'a00000000000000000000000000000%'
  AND Timestamp < (
    SELECT max(Timestamp) - INTERVAL {margin:UInt64} SECOND
    FROM otel_traces
    WHERE TraceId LIKE 'a00000000000000000000000000000%'
  )`
)

// Every DELETE below is written as `ALTER TABLE ... DELETE WHERE ...` (a
// classic, heavyweight mutation that rewrites affected parts — including
// their projections) rather than the newer lightweight `DELETE FROM ...
// WHERE ...` syntax. Whether a target table carries a projection is an
// environment property, not something this file controls: the plain
// docker-compose ClickHouse gets its schema from the OTel Collector's own
// clickhouseexporter DDL (CERBERUS_AUTO_CREATE_SCHEMA=false, see main.go's
// package doc comment), which defines no projections, while the k3d/bundled
// deployment gets its schema from cerberus's OWN auto-create hook
// (CERBERUS_AUTO_CREATE_SCHEMA defaults true under `clickhouse.bundled`,
// see deploy/helm/cerberus/values.yaml's `autoCreate.schema`), which adds
// the curated proj_series/proj_metric_metadata aggregating projections to
// otel_metrics_gauge/_sum/_histogram (internal/schema/ddl's
// metricCatalogProjections) — the exact three tables issue #1527's
// follow-up regression broke on push-to-main's bwc-minio/chaos/
// dashboard-shard jobs (run 30919284059): a lightweight DELETE unconditionally
// throws code 344 the moment its target table has any projection.
//
// The obvious-looking alternative — keep `DELETE FROM` and pin the
// `lightweight_mutation_projection_mode` setting to `rebuild` on the query —
// does NOT work on either ClickHouse version cerberus actually runs
// (25.8, the `clickhouse.bundled` default in deploy/helm/cerberus/values.yaml,
// and 26.6, docker-compose.yml's pin): that setting was made a no-op
// ("Obsolete setting, does nothing", confirmed live against both versions'
// system.settings) once ClickHouse hard-disabled lightweight deletes against
// projected tables outright. `ALTER TABLE ... DELETE` was verified live
// against both versions instead: it deletes cleanly whether or not the
// table carries a projection, and re-running the same statement against a
// projection-free table (the docker-compose shape) is unaffected — so this
// is not a k3d-only special case, it is the version of DELETE that works
// everywhere.
//
// mutationsSyncLocal (`mutations_sync=1`) keeps the call synchronous on the
// node the seeder is connected to, matching every caller's existing
// assumption that the DELETE has taken effect before the function returns
// (deleteStaleMetrics/-Logs/-BaseTraces are called right after their
// family's INSERTs specifically so readers never observe an empty or
// partially-deleted window — an async mutation would reopen exactly the
// race those callers already guard against).
const mutationsSyncLocal = 1

// staleDeleteContext scopes a stale-row ALTER TABLE ... DELETE to
// mutationsSyncLocal. Applied uniformly to every DELETE in this file — not
// just the three tables known to carry projections in the bundled/k3d
// schema today — so this whole class of statement stays synchronous
// regardless of which table a future fixture family adds a DELETE for.
func staleDeleteContext(ctx context.Context) context.Context {
	return clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
		"mutations_sync": mutationsSyncLocal,
	}))
}

// deleteStaleMetrics prunes previous-tick rows from every metrics table
// insertMetrics writes to. Called after all of insertMetrics' INSERTs
// have landed, so — like deleteStaleShowcaseTracesSQL — readers never
// observe an empty or partially-deleted window.
func deleteStaleMetrics(ctx context.Context, conn driver.Conn) error {
	ctx = staleDeleteContext(ctx)
	if err := conn.Exec(ctx, deleteStaleMetricsGaugeSQL,
		clickhouse.Named("margin", marginSeconds(metricsNarrowStaleMargin))); err != nil {
		return fmt.Errorf("gauge-shaped metrics stale delete: %w", err)
	}
	if err := conn.Exec(ctx, deleteStaleMetricsSumSQL,
		clickhouse.Named("margin", marginSeconds(metricsWideStaleMargin))); err != nil {
		return fmt.Errorf("sum metrics stale delete: %w", err)
	}
	if err := conn.Exec(ctx, deleteStaleMetricsHistogramSQL,
		clickhouse.Named("margin", marginSeconds(metricsWideStaleMargin))); err != nil {
		return fmt.Errorf("histogram metrics stale delete: %w", err)
	}
	if err := conn.Exec(ctx, deleteStaleMetricsExpHistSQL,
		clickhouse.Named("margin", marginSeconds(metricsNarrowStaleMargin))); err != nil {
		return fmt.Errorf("exponential histogram metrics stale delete: %w", err)
	}
	return nil
}

// deleteStaleLogs prunes previous-tick rows from otel_logs across every
// stream insertLogs writes (the base 3-service seed + the five
// showcase-logql streams share one window, so one DELETE covers all of
// them). Called after both insertLogsSQL and insertShowcaseLogQLLogs
// have landed.
func deleteStaleLogs(ctx context.Context, conn driver.Conn) error {
	if err := conn.Exec(staleDeleteContext(ctx), deleteStaleLogsSQL,
		clickhouse.Named("margin", marginSeconds(logsStaleMargin))); err != nil {
		return fmt.Errorf("logs stale delete: %w", err)
	}
	return nil
}

// deleteStaleBaseTraces prunes previous-tick rows from the a0... base
// trace fixture insertTraces writes (the b0... showcase-trace range has
// its own DELETE — deleteStaleShowcaseTracesSQL in showcase_traceql.go —
// scoped separately so the two margins, sized for very different
// windows, never interact).
func deleteStaleBaseTraces(ctx context.Context, conn driver.Conn) error {
	if err := conn.Exec(staleDeleteContext(ctx), deleteStaleBaseTracesSQL,
		clickhouse.Named("margin", marginSeconds(tracesStaleMargin))); err != nil {
		return fmt.Errorf("base traces stale delete: %w", err)
	}
	return nil
}
