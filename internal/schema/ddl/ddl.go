// Package ddl applies the upstream OTel ClickHouse Exporter's DDL templates
// against a cerberus ClickHouse connection. Schema source-of-truth lives in
// github.com/open-telemetry/opentelemetry-collector-contrib (via the
// tsouza/opentelemetry-collector-contrib:cerberus-ddl fork wired via go.mod
// replace, see PR #154). Cerberus does NOT maintain a parallel schema; this
// package just executes upstream's `CREATE TABLE IF NOT EXISTS` against the
// configured CH connection.
//
// The upstream templates are `fmt.Sprintf`-style with `%s` placeholders for
// (database, table, on-cluster clause, engine, TTL expression). Cerberus
// renders them via a small [Config] struct that defaults to MergeTree, no
// cluster, no TTL — matching the cerberus single-node ClickHouse deployment.
// The materialized-view template for traces has a wider placeholder shape
// (7 fields) which is handled specially in [renderTracesCreateTsView].
package ddl

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/clickhouseexporter/sqltemplates"
)

// Config carries the rendering inputs for the upstream DDL templates. The
// zero value renders against the `default` database with `MergeTree()` and
// no TTL — the cerberus single-node default.
type Config struct {
	// Database names the ClickHouse database to create tables in.
	// Defaults to "default" when empty.
	Database string

	// Cluster, when non-empty, renders an `ON CLUSTER "<name>"` clause
	// into the templates. Cerberus's single-node deployment leaves it
	// empty.
	Cluster string

	// Engine overrides the ClickHouse table engine. Defaults to
	// "MergeTree()" — matches the upstream exporter default.
	Engine string

	// TTL, when non-zero, renders a `TTL <timeField> + toIntervalXxx(N)`
	// clause into each template. Cerberus leaves TTL to the operator.
	TTL time.Duration

	// Tables overrides the per-signal table names. The zero values fall
	// back to the upstream defaults (otel_logs, otel_traces,
	// otel_metrics_gauge, otel_metrics_sum, otel_metrics_histogram,
	// otel_metrics_exp_histogram, otel_metrics_summary).
	Tables Tables
}

// Tables overrides the per-signal table name used when rendering each
// upstream DDL template. Empty fields fall back to the upstream defaults.
type Tables struct {
	Logs                string
	Traces              string
	MetricsGauge        string
	MetricsSum          string
	MetricsHistogram    string
	MetricsExpHistogram string
	MetricsSummary      string
}

// Defaults mirror the upstream OTel ClickHouse Exporter's table names. They
// are also what cerberus's internal/schema package returns from the
// DefaultOTel{Metrics,Logs,Traces} helpers.
const (
	defaultDatabase = "default"
	defaultEngine   = "MergeTree()"

	defaultLogsTable                = "otel_logs"
	defaultTracesTable              = "otel_traces"
	defaultMetricsGaugeTable        = "otel_metrics_gauge"
	defaultMetricsSumTable          = "otel_metrics_sum"
	defaultMetricsHistogramTable    = "otel_metrics_histogram"
	defaultMetricsExpHistogramTable = "otel_metrics_exp_histogram"
	defaultMetricsSummaryTable      = "otel_metrics_summary"
)

// withDefaults returns a copy of cfg with empty string fields filled in
// from the upstream defaults. This is the single source of "what's empty
// mean" for the package — everything else reads pre-defaulted fields.
func (c Config) withDefaults() Config {
	if c.Database == "" {
		c.Database = defaultDatabase
	}
	if c.Engine == "" {
		c.Engine = defaultEngine
	}
	if c.Tables.Logs == "" {
		c.Tables.Logs = defaultLogsTable
	}
	if c.Tables.Traces == "" {
		c.Tables.Traces = defaultTracesTable
	}
	if c.Tables.MetricsGauge == "" {
		c.Tables.MetricsGauge = defaultMetricsGaugeTable
	}
	if c.Tables.MetricsSum == "" {
		c.Tables.MetricsSum = defaultMetricsSumTable
	}
	if c.Tables.MetricsHistogram == "" {
		c.Tables.MetricsHistogram = defaultMetricsHistogramTable
	}
	if c.Tables.MetricsExpHistogram == "" {
		c.Tables.MetricsExpHistogram = defaultMetricsExpHistogramTable
	}
	if c.Tables.MetricsSummary == "" {
		c.Tables.MetricsSummary = defaultMetricsSummaryTable
	}
	return c
}

// clusterClause renders the optional `ON CLUSTER "<name>"` fragment that
// upstream templates expect as a single `%s` slot. Matches upstream's
// `Config.clusterString` semantics.
func (c Config) clusterClause() string {
	if c.Cluster == "" {
		return ""
	}
	return fmt.Sprintf(`ON CLUSTER %q`, c.Cluster)
}

// ttlExpr renders the optional `TTL <field> + toIntervalXxx(N)` fragment
// that upstream templates expect as a `%s` slot per signal. The time field
// differs per signal — Logs uses `TimestampTime`, Traces use
// `toDateTime(Timestamp)`, metrics use `toDateTime(TimeUnix)`. Matches
// upstream's `internal.GenerateTTLExpr` semantics.
func (c Config) ttlExpr(timeField string) string {
	ttl := c.TTL
	if ttl <= 0 {
		return ""
	}
	switch {
	case ttl%(24*time.Hour) == 0:
		return fmt.Sprintf("TTL %s + toIntervalDay(%d)", timeField, ttl/(24*time.Hour))
	case ttl%time.Hour == 0:
		return fmt.Sprintf("TTL %s + toIntervalHour(%d)", timeField, ttl/time.Hour)
	case ttl%time.Minute == 0:
		return fmt.Sprintf("TTL %s + toIntervalMinute(%d)", timeField, ttl/time.Minute)
	default:
		return fmt.Sprintf("TTL %s + toIntervalSecond(%d)", timeField, ttl/time.Second)
	}
}

// Apply runs CREATE TABLE IF NOT EXISTS for each requested signal against
// conn using the upstream OTel exporter's DDL templates. Idempotent:
// re-running over an existing schema is a no-op (every template carries
// `IF NOT EXISTS`).
//
// For Metrics, all 5 tables (gauge, sum, histogram, exp_histogram, summary)
// are created in one Apply call — they form the metrics signal as a unit.
// For Traces, the spans table plus the trace_id_ts lookup table and its
// materialized view are created together (matching upstream's
// createTraceTables).
//
// Apply uses Config's zero-value defaults (database=default, engine=
// MergeTree(), no cluster, no TTL, upstream table names). Callers needing
// non-default rendering should use ApplyWithConfig.
func Apply(ctx context.Context, conn driver.Conn, signals []Signal) error {
	return ApplyWithConfig(ctx, conn, Config{}, signals)
}

// ApplyWithConfig is the explicit-config form of Apply: it threads a Config
// through the upstream templates so callers can override database, engine,
// cluster, TTL, or table names. See Config for field semantics.
func ApplyWithConfig(ctx context.Context, conn driver.Conn, cfg Config, signals []Signal) error {
	cfg = cfg.withDefaults()
	for _, s := range signals {
		if err := applySignal(ctx, conn, cfg, s); err != nil {
			return err
		}
	}
	return nil
}

// applySignal renders + executes the DDL statements for one signal.
// Statement order within a signal matches the upstream exporter — for
// Traces in particular the lookup table must precede the materialized
// view (the MV references it).
func applySignal(ctx context.Context, conn driver.Conn, cfg Config, s Signal) error {
	stmts, err := renderSignal(cfg, s)
	if err != nil {
		return err
	}
	for _, stmt := range stmts {
		if err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("ddl: exec %s: %w", s, err)
		}
	}
	return nil
}

// renderSignal returns the ordered list of CREATE statements for a signal.
// Splitting this out from applySignal keeps the rendering logic testable
// without a live ClickHouse connection.
func renderSignal(cfg Config, s Signal) ([]string, error) {
	switch s {
	case Metrics:
		ttl := cfg.ttlExpr("toDateTime(TimeUnix)")
		return []string{
			renderMetricsTable(sqltemplates.MetricsGaugeCreateTable, cfg, cfg.Tables.MetricsGauge, ttl),
			renderMetricsTable(sqltemplates.MetricsSumCreateTable, cfg, cfg.Tables.MetricsSum, ttl),
			renderMetricsTable(sqltemplates.MetricsHistogramCreateTable, cfg, cfg.Tables.MetricsHistogram, ttl),
			renderMetricsTable(sqltemplates.MetricsExpHistogramCreateTable, cfg, cfg.Tables.MetricsExpHistogram, ttl),
			renderMetricsTable(sqltemplates.MetricsSummaryCreateTable, cfg, cfg.Tables.MetricsSummary, ttl),
		}, nil
	case Logs:
		return []string{renderLogsTable(cfg)}, nil
	case Traces:
		return []string{
			renderTracesTable(cfg),
			renderTracesCreateTsTable(cfg),
			renderTracesCreateTsView(cfg),
		}, nil
	default:
		return nil, fmt.Errorf("ddl: unknown signal: %d", int(s))
	}
}

// renderMetricsTable formats one of the five metrics table templates. The
// upstream template shape is `(database, table, cluster, engine, ttl)` —
// see metrics_*_table.sql in the fork and internal/metrics/metrics_model.go
// in upstream for the canonical Sprintf call.
func renderMetricsTable(tmpl string, cfg Config, table, ttl string) string {
	return fmt.Sprintf(tmpl, cfg.Database, table, cfg.clusterClause(), cfg.Engine, ttl)
}

// renderLogsTable formats the logs DDL. Upstream shape:
// `(database, table, cluster, engine, ttl)` — see exporter_logs.go's
// renderCreateLogsTableSQL. The TTL field is `TimestampTime`.
func renderLogsTable(cfg Config) string {
	return fmt.Sprintf(sqltemplates.LogsCreateTable,
		cfg.Database, cfg.Tables.Logs, cfg.clusterClause(),
		cfg.Engine,
		cfg.ttlExpr("TimestampTime"),
	)
}

// renderTracesTable formats the traces spans-table DDL. Upstream shape:
// `(database, table, cluster, engine, ttl)`. TTL field is
// `toDateTime(Timestamp)`.
func renderTracesTable(cfg Config) string {
	return fmt.Sprintf(sqltemplates.TracesCreateTable,
		cfg.Database, cfg.Tables.Traces, cfg.clusterClause(),
		cfg.Engine,
		cfg.ttlExpr("toDateTime(Timestamp)"),
	)
}

// renderTracesCreateTsTable formats the `<table>_trace_id_ts` lookup table
// DDL. Upstream shape mirrors the spans table (db, table, cluster, engine,
// ttl) — the `_trace_id_ts` suffix is hard-coded into the template, so the
// caller passes the base traces table name. TTL field is
// `toDateTime(Start)`.
func renderTracesCreateTsTable(cfg Config) string {
	return fmt.Sprintf(sqltemplates.TracesCreateTsTable,
		cfg.Database, cfg.Tables.Traces, cfg.clusterClause(),
		cfg.Engine,
		cfg.ttlExpr("toDateTime(Start)"),
	)
}

// renderTracesCreateTsView formats the `<table>_trace_id_ts_mv`
// materialized-view DDL. Upstream shape is *wider* than the table
// templates: 7 placeholders — (db, table, cluster, db, table, db, table)
// — because the MV references both the lookup table (TO clause) and the
// spans table (FROM clause). See exporter_traces.go's
// renderTraceIDTsMaterializedViewSQL.
func renderTracesCreateTsView(cfg Config) string {
	return fmt.Sprintf(sqltemplates.TracesCreateTsView,
		cfg.Database, cfg.Tables.Traces, cfg.clusterClause(),
		cfg.Database, cfg.Tables.Traces,
		cfg.Database, cfg.Tables.Traces,
	)
}
