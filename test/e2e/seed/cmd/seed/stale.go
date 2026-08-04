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
	deleteStaleMetricsGaugeSQL = `DELETE FROM otel_metrics_gauge
WHERE MetricName IN ('up', 'target_info', 'showcase_flapping', 'showcase_multilabel')
  AND Timestamp < (
    SELECT max(Timestamp) - INTERVAL {margin:UInt64} SECOND
    FROM otel_metrics_gauge
    WHERE MetricName IN ('up', 'target_info', 'showcase_flapping', 'showcase_multilabel')
  )`

	deleteStaleMetricsSumSQL = `DELETE FROM otel_metrics_sum
WHERE MetricName IN ('http_server_request_duration_count', 'showcase_restarting_total')
  AND Timestamp < (
    SELECT max(Timestamp) - INTERVAL {margin:UInt64} SECOND
    FROM otel_metrics_sum
    WHERE MetricName IN ('http_server_request_duration_count', 'showcase_restarting_total')
  )`

	deleteStaleMetricsHistogramSQL = `DELETE FROM otel_metrics_histogram
WHERE MetricName = 'http_server_request_duration'
  AND Timestamp < (
    SELECT max(Timestamp) - INTERVAL {margin:UInt64} SECOND
    FROM otel_metrics_histogram
    WHERE MetricName = 'http_server_request_duration'
  )`

	deleteStaleMetricsExpHistSQL = `DELETE FROM otel_metrics_exponential_histogram
WHERE MetricName = 'showcase_latency_exp_hist'
  AND Timestamp < (
    SELECT max(Timestamp) - INTERVAL {margin:UInt64} SECOND
    FROM otel_metrics_exponential_histogram
    WHERE MetricName = 'showcase_latency_exp_hist'
  )`

	deleteStaleLogsSQL = `DELETE FROM otel_logs
WHERE ServiceName IN ('api', 'frontend', 'db', 'gateway', 'shop', 'proxy', 'painter', 'packer')
  AND Timestamp < (
    SELECT max(Timestamp) - INTERVAL {margin:UInt64} SECOND
    FROM otel_logs
    WHERE ServiceName IN ('api', 'frontend', 'db', 'gateway', 'shop', 'proxy', 'painter', 'packer')
  )`

	deleteStaleBaseTracesSQL = `DELETE FROM otel_traces
WHERE TraceId LIKE 'a00000000000000000000000000000%'
  AND Timestamp < (
    SELECT max(Timestamp) - INTERVAL {margin:UInt64} SECOND
    FROM otel_traces
    WHERE TraceId LIKE 'a00000000000000000000000000000%'
  )`
)

// deleteStaleMetrics prunes previous-tick rows from every metrics table
// insertMetrics writes to. Called after all of insertMetrics' INSERTs
// have landed, so — like deleteStaleShowcaseTracesSQL — readers never
// observe an empty or partially-deleted window.
func deleteStaleMetrics(ctx context.Context, conn driver.Conn) error {
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
	if err := conn.Exec(ctx, deleteStaleLogsSQL,
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
	if err := conn.Exec(ctx, deleteStaleBaseTracesSQL,
		clickhouse.Named("margin", marginSeconds(tracesStaleMargin))); err != nil {
		return fmt.Errorf("base traces stale delete: %w", err)
	}
	return nil
}
