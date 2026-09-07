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
// `max(<time column>) - margin` DELETE run AFTER the INSERT, never before —
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
// both the step-A cutoff SELECT and the step-B DELETE to the
// MetricName/ServiceName/TraceId set this seeder owns in that table —
// the time column itself is NOT uniform across tables: the OTel-CH
// exporter's schema names it `TimeUnix` on every otel_metrics_* table
// (matching the INSERT column lists above — insertGaugeSQL et al. never
// write a `Timestamp` column, because that column doesn't exist there;
// `DESCRIBE TABLE otel_metrics_gauge` has no `Timestamp` row) and
// `Timestamp` on otel_logs/otel_traces. The four metrics DELETEs below
// use TimeUnix; the logs/base-traces DELETEs after them use Timestamp —
// unscoped would either eat rows the dogfood self-telemetry pipeline
// wrote into the same table (see showcase_traceql.go / showcase_logql.go
// doc comments) or anchor the cutoff on foreign rows. Both DateTime64(9)
// columns (see internal/schema/ddl/ddl.go's column-codec comments) accept
// a Go time.Time directly via clickhouse-go v2's native driver — the type
// resolveStaleCutoff (sharded_mutation.go) scans the SELECT's result into,
// and every step-B template below binds back as {cutoff:DateTime64(9)}.
// The margin is a bound query parameter on the step-A SELECT
// (`{margin:UInt64}`), not a literal, so it stays in lockstep with the
// Go-side staleMargin() computation above — the same `{name:Type}`
// parameter binding already used by compatibility/prometheus/cmd/seed/main.go.
// Public table names the DELETEs below target. Declared once so
// deleteStaleMetrics/-Logs/-BaseTraces (which call resolveMutationTarget
// against exactly these names) and showcase_traceql.go's own
// otel_traces DELETE can't drift apart on the literal string.
const (
	metricsGaugeTable     = "otel_metrics_gauge"
	metricsSumTable       = "otel_metrics_sum"
	metricsHistogramTable = "otel_metrics_histogram"
	metricsExpHistTable   = "otel_metrics_exponential_histogram"
	logsTable             = "otel_logs"
	tracesTable           = "otel_traces"
)

// Every family below is two separate templates (issue #3124's two-step
// literal-cutoff split — see sharded_mutation.go's package doc comment for
// the full replica-divergence hazard this avoids): a step-A cutoff SELECT
// run once against the PUBLIC table to resolve `max(<time column>) -
// margin` to a literal time.Time (resolveStaleCutoff, sharded_mutation.go),
// and a step-B ALTER TABLE ... DELETE that binds that literal as
// `{cutoff:DateTime64(9)}` — no subquery survives inside the mutation, so
// every replica's own copy of the ALTER compares against the exact same
// fixed value.
//
// Each template carries exactly ONE occurrence of the literal text
// "@@MUTATION_TABLE@@" (mutationTablePlaceholder's value,
// sharded_mutation.go): a step-A template substitutes it for the PUBLIC
// table name (a SELECT against a Distributed table aggregates cluster-wide
// regardless of which node runs it — see sharded_mutation.go), a step-B
// template substitutes it for the resolved mutation target (the "_local"
// table under the datashard lane, since a Distributed table rejects every
// mutation outright). A step-B template also carries one occurrence of
// "@@MUTATION_ON_CLUSTER@@" — empty outside the datashard lane, "
// ON CLUSTER '{cluster}'" within it, needed because a "_local" table's
// mutation must reach every shard's own copy, not just the node the seeder
// is connected to; a step-A template carries no such placeholder, since a
// plain SELECT needs no cluster broadcast.
//
// The placeholder is typed directly into each backtick string rather than
// built by concatenating separate backtick-quoted segments around
// mutationTablePlaceholder, for two reasons: it keeps each const a single
// unbroken backtick literal (test/regression/seed_test.go's
// extractBacktickConst scans exactly one backtick-delimited span per const
// name — a concatenated literal would silently truncate its scan to the
// first segment), and it avoids the '%%'-escaping a fmt.Sprintf %s verb
// would force onto these templates' existing LIKE '...%' wildcards
// (deleteStaleBaseTracesSQLTemplate below, deleteStaleShowcaseTracesSQLTemplate
// in showcase_traceql.go).
const (
	selectStaleMetricsGaugeCutoffSQLTemplate = `SELECT max(TimeUnix) - INTERVAL {margin:UInt64} SECOND
FROM @@MUTATION_TABLE@@
WHERE MetricName IN ('up', 'target_info', 'showcase_flapping', 'showcase_multilabel')`

	deleteStaleMetricsGaugeSQLTemplate = `ALTER TABLE @@MUTATION_TABLE@@@@MUTATION_ON_CLUSTER@@ DELETE
WHERE MetricName IN ('up', 'target_info', 'showcase_flapping', 'showcase_multilabel')
  AND TimeUnix < {cutoff:DateTime64(9)}`

	selectStaleMetricsSumCutoffSQLTemplate = `SELECT max(TimeUnix) - INTERVAL {margin:UInt64} SECOND
FROM @@MUTATION_TABLE@@
WHERE MetricName IN ('http_server_request_duration_count', 'showcase_restarting_total')`

	deleteStaleMetricsSumSQLTemplate = `ALTER TABLE @@MUTATION_TABLE@@@@MUTATION_ON_CLUSTER@@ DELETE
WHERE MetricName IN ('http_server_request_duration_count', 'showcase_restarting_total')
  AND TimeUnix < {cutoff:DateTime64(9)}`

	selectStaleMetricsHistogramCutoffSQLTemplate = `SELECT max(TimeUnix) - INTERVAL {margin:UInt64} SECOND
FROM @@MUTATION_TABLE@@
WHERE MetricName = 'http_server_request_duration'`

	deleteStaleMetricsHistogramSQLTemplate = `ALTER TABLE @@MUTATION_TABLE@@@@MUTATION_ON_CLUSTER@@ DELETE
WHERE MetricName = 'http_server_request_duration'
  AND TimeUnix < {cutoff:DateTime64(9)}`

	selectStaleMetricsExpHistCutoffSQLTemplate = `SELECT max(TimeUnix) - INTERVAL {margin:UInt64} SECOND
FROM @@MUTATION_TABLE@@
WHERE MetricName = 'showcase_latency_exp_hist'`

	deleteStaleMetricsExpHistSQLTemplate = `ALTER TABLE @@MUTATION_TABLE@@@@MUTATION_ON_CLUSTER@@ DELETE
WHERE MetricName = 'showcase_latency_exp_hist'
  AND TimeUnix < {cutoff:DateTime64(9)}`

	selectStaleLogsCutoffSQLTemplate = `SELECT max(Timestamp) - INTERVAL {margin:UInt64} SECOND
FROM @@MUTATION_TABLE@@
WHERE ServiceName IN ('api', 'frontend', 'db', 'gateway', 'shop', 'proxy', 'painter', 'packer')`

	deleteStaleLogsSQLTemplate = `ALTER TABLE @@MUTATION_TABLE@@@@MUTATION_ON_CLUSTER@@ DELETE
WHERE ServiceName IN ('api', 'frontend', 'db', 'gateway', 'shop', 'proxy', 'painter', 'packer')
  AND Timestamp < {cutoff:DateTime64(9)}`

	selectStaleBaseTracesCutoffSQLTemplate = `SELECT max(Timestamp) - INTERVAL {margin:UInt64} SECOND
FROM @@MUTATION_TABLE@@
WHERE TraceId LIKE 'a00000000000000000000000000000%'`

	deleteStaleBaseTracesSQLTemplate = `ALTER TABLE @@MUTATION_TABLE@@@@MUTATION_ON_CLUSTER@@ DELETE
WHERE TraceId LIKE 'a00000000000000000000000000000%'
  AND Timestamp < {cutoff:DateTime64(9)}`
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

// mutationsSyncEveryReplica (`mutations_sync=2`) waits for the mutation to
// finish on EVERY replica, not just the node the seeder is connected to.
//
// It is what an ON CLUSTER stale-row DELETE needs, and mutationsSyncLocal is
// not. Under the datashard lane a Distributed table's INSERT spreads rows
// across shards essentially at random (Config.DataShardingKey, default
// rand()), so a sentinel row lands on ONE shard — quite possibly not the one
// the seeder is connected to. ON CLUSTER broadcasts the DELETE to every
// node and the client waits for the DDL queue to accept it everywhere, but
// with mutations_sync=1 each node then waits only for ITSELF: the connected
// node's mutation is complete when Exec returns, while another shard's is
// merely queued. A reader that counts rows immediately afterwards can still
// see the row the DELETE was issued to reap — the exact "readers never
// observe a partially-deleted window" guarantee the callers below are
// written to rely on, holding on one shard and not the others.
//
// Only the low-row-count families expose it in practice (base-traces' fixed
// 7-row fixture), because a high-volume family's next tick lands fresh rows
// on every shard and re-runs the DELETE there anyway — which is why this
// surfaces as an intermittent TestReSeedRowCountStability/base-traces
// failure on the N=2 lane alone.
const mutationsSyncEveryReplica = 2

// staleDeleteContext scopes a stale-row ALTER TABLE ... DELETE to the
// synchrony its target needs: every replica when the statement is broadcast
// ON CLUSTER (the datashard lane's "_local" tables), the connected node
// alone otherwise (the single-node monolith shape, where they are the same
// thing and the stronger setting would only add a needless wait).
//
// Applied uniformly to every DELETE in this file — not just the three
// tables known to carry projections in the bundled/k3d schema today — so
// this whole class of statement stays synchronous regardless of which table
// a future fixture family adds a DELETE for.
func staleDeleteContext(ctx context.Context, onCluster bool) context.Context {
	return clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
		"mutations_sync": mutationsSyncSetting(onCluster),
	}))
}

// mutationsSyncSetting picks the `mutations_sync` value staleDeleteContext
// stamps: mutationsSyncEveryReplica when the DELETE is broadcast ON CLUSTER
// (so every replica of every shard is actually waited on before the caller
// proceeds), mutationsSyncLocal otherwise (the monolith shape, where the
// connected node and "every node the statement touched" are the same node,
// so the stronger setting would only add a needless wait). Split out as its
// own pure function so the ON-CLUSTER-picks-the-stronger-setting decision is
// unit-testable without a live ClickHouse connection or reaching into
// clickhouse-go's unexported query-options internals.
func mutationsSyncSetting(onCluster bool) int {
	if onCluster {
		return mutationsSyncEveryReplica
	}
	return mutationsSyncLocal
}

// deleteStaleFamily runs one fixture family's two-step literal-cutoff
// stale-row DELETE (issue #3124): step A calls resolveStaleCutoff
// (sharded_mutation.go) with selectTemplate against publicTable, binding
// margin as selectTemplate's {margin:UInt64} parameter, to resolve the
// cutoff to a literal time.Time; step B execs deleteTemplate — with that
// time.Time bound as {cutoff:DateTime64(9)} — against the resolved
// mutation target (resolveMutationTarget), wrapped in staleDeleteContext
// so the DELETE stays synchronous on every node it was broadcast to. Under the datashard lane (issue #3105)
// publicTable's resolved mutation target is the underlying "_local" table,
// since the Distributed public name itself rejects every mutation.
func deleteStaleFamily(ctx context.Context, conn driver.Conn, publicTable, selectTemplate, deleteTemplate string, margin time.Duration) error {
	target, err := resolveMutationTarget(ctx, conn, publicTable)
	if err != nil {
		return err
	}
	cutoff, err := resolveStaleCutoff(ctx, conn, selectTemplate, publicTable, clickhouse.Named("margin", marginSeconds(margin)))
	if err != nil {
		return err
	}
	sql := mutationTableSQL(deleteTemplate, target.table, target.onCluster)
	return conn.Exec(staleDeleteContext(ctx, target.onCluster != ""), sql, clickhouse.Named("cutoff", cutoff))
}

// deleteStaleMetrics prunes previous-tick rows from every metrics table
// insertMetrics writes to. Called after all of insertMetrics' INSERTs
// have landed, so — like deleteStaleShowcaseTracesSQL — readers never
// observe an empty or partially-deleted window.
func deleteStaleMetrics(ctx context.Context, conn driver.Conn) error {
	if err := deleteStaleFamily(ctx, conn, metricsGaugeTable, selectStaleMetricsGaugeCutoffSQLTemplate, deleteStaleMetricsGaugeSQLTemplate, metricsNarrowStaleMargin); err != nil {
		return fmt.Errorf("gauge-shaped metrics stale delete: %w", err)
	}
	if err := deleteStaleFamily(ctx, conn, metricsSumTable, selectStaleMetricsSumCutoffSQLTemplate, deleteStaleMetricsSumSQLTemplate, metricsWideStaleMargin); err != nil {
		return fmt.Errorf("sum metrics stale delete: %w", err)
	}
	if err := deleteStaleFamily(ctx, conn, metricsHistogramTable, selectStaleMetricsHistogramCutoffSQLTemplate, deleteStaleMetricsHistogramSQLTemplate, metricsWideStaleMargin); err != nil {
		return fmt.Errorf("histogram metrics stale delete: %w", err)
	}
	if err := deleteStaleFamily(ctx, conn, metricsExpHistTable, selectStaleMetricsExpHistCutoffSQLTemplate, deleteStaleMetricsExpHistSQLTemplate, metricsNarrowStaleMargin); err != nil {
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
	if err := deleteStaleFamily(ctx, conn, logsTable, selectStaleLogsCutoffSQLTemplate, deleteStaleLogsSQLTemplate, logsStaleMargin); err != nil {
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
	if err := deleteStaleFamily(ctx, conn, tracesTable, selectStaleBaseTracesCutoffSQLTemplate, deleteStaleBaseTracesSQLTemplate, tracesStaleMargin); err != nil {
		return fmt.Errorf("base traces stale delete: %w", err)
	}
	return nil
}
