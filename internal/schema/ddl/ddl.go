// Package ddl applies the upstream OTel ClickHouse Exporter's DDL templates
// against a cerberus ClickHouse connection. Schema source-of-truth lives in
// github.com/open-telemetry/opentelemetry-collector-contrib (via the
// tsouza/opentelemetry-collector-contrib:cerberus-ddl fork wired via go.mod
// replace, see PR #154). Cerberus does NOT maintain a parallel schema; this
// package just executes upstream's `CREATE DATABASE IF NOT EXISTS` followed by
// `CREATE TABLE IF NOT EXISTS` against the configured CH connection.
//
// # The database must be created first
//
// The configured database (CERBERUS_CH_DATABASE) is NOT guaranteed to exist.
// A fresh ClickHouse only ships the built-in `default` database; any other
// target — e.g. the `otel` database the demo and compat stacks pin on both
// the collector and cerberus — must be created. Every table template emits a
// fully-qualified `<database>.<table>` name, so a CREATE TABLE against a
// non-existent database fails with "Database otel does not exist" — which is
// exactly what bit a deployment on a clean cluster. So Apply issues
// `CREATE DATABASE IF NOT EXISTS <database>` BEFORE any table statement
// (matching upstream's exporter, which creates the database in its start()
// path before the tables). The whole sequence is idempotent: the database
// create carries IF NOT EXISTS just like the table creates, so re-running over
// an already-provisioned cluster is a no-op.
//
// The upstream traces + metrics templates are `fmt.Sprintf`-style with `%s`
// placeholders for (database, table, on-cluster clause, engine, TTL
// expression). The logs template moved to `text/template` upstream in
// v0.152.0 ([sqltemplates.LogsCreateTableTmpl] executed against
// [sqltemplates.CreateTableData]) — see [renderLogsTable]. Cerberus renders
// everything via a small [Config] struct that defaults to MergeTree, no
// cluster, no TTL — matching the cerberus single-node ClickHouse deployment.
// The materialized-view template for traces has a wider placeholder shape
// (7 fields) which is handled specially in [renderTracesCreateTsView].
package ddl

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/clickhouseexporter/sqltemplates"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// Config carries the rendering inputs for the upstream DDL templates. The
// zero value renders against the `default` database with `MergeTree()` and
// no TTL — the cerberus single-node default.
type Config struct {
	// Database names the ClickHouse database to create tables in.
	// Defaults to "default" when empty.
	Database string

	// Cluster, when non-empty, renders an ON CLUSTER clause (with the
	// name backtick-quoted, matching upstream's Config.clusterString)
	// into the templates. Cerberus's single-node deployment leaves it
	// empty.
	Cluster string

	// Engine overrides the ClickHouse table engine. When empty it defaults to
	// "MergeTree()" (the upstream exporter default) — or, when
	// DatabaseEngine.Replicated is set, to the BARE "ReplicatedMergeTree" (no
	// arguments): a Replicated database does NOT auto-convert MergeTree, so the
	// tables need a replicated engine to replicate their DATA, and inside a
	// Replicated database the engine's Keeper path / replica are supplied
	// automatically — explicit arguments are rejected (code 36). A non-empty
	// Engine wins over both; that's how a classic ON CLUSTER cluster pins an
	// explicit ReplicatedMergeTree('/path', '{replica}').
	Engine string

	// TTL sets per-signal retention on the created tables — a zero duration
	// for a signal emits no TTL clause (operator-managed retention).
	// Retention is conventionally keyed on the signal (logs short, metrics
	// long), not the individual table, so the five metrics tables share
	// TTL.Metrics and the spans + lookup tables share TTL.Traces. See TTL.
	TTL TTL

	// DatabaseEngine selects the ClickHouse engine for the CREATE DATABASE
	// statement. The zero value emits no ENGINE clause (server default
	// Atomic — the single-node shape); set Replicated to create the
	// database with the Replicated engine for a clustered deployment.
	DatabaseEngine DatabaseEngine

	// SkipDatabaseCreate, when true, omits the CREATE DATABASE statement and
	// creates only the tables (which are fully qualified, so they land in the
	// configured database). Use it when the database is provisioned externally
	// — e.g. a Replicated database managed by cluster tooling. The zero value
	// (false) creates the database, the default cold-cluster bootstrap.
	SkipDatabaseCreate bool

	// Tables overrides the per-signal table names. The zero values fall
	// back to the upstream defaults (otel_logs, otel_traces,
	// otel_metrics_gauge, otel_metrics_sum, otel_metrics_histogram,
	// otel_metrics_exp_histogram, otel_metrics_summary).
	Tables Tables

	// Settings appends extra MergeTree SETTINGS to every auto-created table,
	// continuing the `SETTINGS index_granularity=..., ttl_only_drop_parts=1`
	// tail the upstream templates already bake — the escape hatch for
	// deployment-specific MergeTree knobs (e.g. an S3 `storage_policy`, or
	// `min_bytes_for_wide_part`). It is an ORDERED slice, not a map, so the
	// emitted DDL is deterministic. The zero value (nil/empty) appends
	// nothing, leaving the DDL byte-identical to the bare template — strict
	// backward compatibility. The continuation is orthogonal to the
	// engine / ON CLUSTER mode: it lands on the SETTINGS tail in both
	// MergeTree and ReplicatedMergeTree shapes. Only the four MergeTree
	// tables carry a SETTINGS tail; the traces materialized view has none, so
	// Settings does not apply to it.
	Settings []schema.KV
}

// DatabaseEngine selects the ClickHouse database engine for the
// auto-created database. The zero value (Replicated false) emits no
// ENGINE clause, so ClickHouse applies its default (Atomic) — the
// single-node shape cerberus ships by default.
//
// When Replicated is true the database is created with
// `ENGINE = Replicated(<path>, <shard>, <replica>)`. A Replicated database
// auto-replicates all DDL across replicas, so no ON CLUSTER clause is used
// (the two are mutually exclusive — the Replicated database replicates DDL
// itself). It does NOT auto-convert MergeTree tables to ReplicatedMergeTree,
// though: replicated DDL gives each replica an independent table, but only a
// ReplicatedMergeTree engine replicates the DATA. So withDefaults resolves an
// empty table Engine to the BARE ReplicatedMergeTree under a Replicated
// database — no explicit (path, replica) args, which the database rejects with
// code 36 (see defaultTableEngine).
type DatabaseEngine struct {
	// Replicated turns on the Replicated database engine. When false the
	// other fields are ignored and no ENGINE clause is emitted.
	Replicated bool

	// ReplicatedZooPath is the ZooKeeper/Keeper path the Replicated engine
	// coordinates on, e.g. "/clickhouse/databases/otel". Required when
	// Replicated is true (ApplyWithConfig rejects an empty path).
	ReplicatedZooPath string

	// ReplicatedShard / ReplicatedReplica are the shard and replica names
	// the engine identifies this node by. They default to the ClickHouse
	// server macros "{shard}" / "{replica}", which the server expands —
	// the conventional cluster setup, so most operators leave them unset.
	ReplicatedShard   string
	ReplicatedReplica string
}

// TTL carries per-signal retention durations for the auto-created tables.
// A zero duration leaves that signal's tables with no TTL clause. Retention
// is keyed on the signal rather than the individual table because that is
// how observability retention is actually set — logs are voluminous and
// short-lived, metrics are long-lived — and the tables within a signal
// (the five metrics tables; the traces spans + trace_id_ts lookup) share a
// lifecycle. An operator needing genuinely per-table retention runs the DDL
// themselves instead of via the auto-create hook.
type TTL struct {
	// Metrics applies to the five metrics tables (retention keyed on the
	// TimeUnix column).
	Metrics time.Duration
	// Logs applies to the logs table (keyed on Timestamp).
	Logs time.Duration
	// Traces applies to the spans table (keyed on Timestamp) and the
	// trace_id_ts lookup table (keyed on Start).
	Traces time.Duration
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

	// defaultReplicatedShard / defaultReplicatedReplica are the ClickHouse
	// server macros a Replicated database engine identifies a node by when
	// the operator doesn't pin explicit values — the conventional cluster
	// setup, where the server config defines {shard} / {replica}.
	defaultReplicatedShard   = "{shard}"
	defaultReplicatedReplica = "{replica}"

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
		c.Engine = defaultTableEngine(c.DatabaseEngine.Replicated)
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
	if c.DatabaseEngine.Replicated {
		if c.DatabaseEngine.ReplicatedShard == "" {
			c.DatabaseEngine.ReplicatedShard = defaultReplicatedShard
		}
		if c.DatabaseEngine.ReplicatedReplica == "" {
			c.DatabaseEngine.ReplicatedReplica = defaultReplicatedReplica
		}
	}
	return c
}

// defaultTableEngine resolves the table engine to use when Config.Engine is
// empty. With a Replicated database engine the tables must be ReplicatedMergeTree
// to replicate their DATA (a Replicated database does NOT auto-convert
// MergeTree), and inside a Replicated database the engine takes NO arguments —
// the database's Replicated(...) coordinates plus the server default_replica_path
// supply the Keeper path / replica, and explicit args are rejected (code 36). So
// it returns the bare `ReplicatedMergeTree`. Otherwise it returns the single-node
// `MergeTree()` default. Built via the typed chsql constructors — no
// hand-assembled SQL.
func defaultTableEngine(replicated bool) string {
	if !replicated {
		return defaultEngine
	}
	return chsql.RenderDDL(chsql.EngineReplicatedMergeTree())
}

// clusterClause renders the optional ON CLUSTER fragment that upstream
// templates expect as a single slot (`%s` in the Sprintf templates,
// `{{.ClusterString}}` in the logs template). Returns "" when no cluster
// is configured. Built via the typed chsql.OnCluster constructor — the
// name is backtick-quoted (embedded backticks doubled) by the builder, so
// this matches upstream's `Config.clusterString` semantics without any
// hand-rolled fmt.Sprintf / strings.ReplaceAll.
func (c Config) clusterClause() string {
	if c.Cluster == "" {
		return ""
	}
	return chsql.RenderDDL(chsql.OnCluster(c.Cluster))
}

// ttlExpr renders the optional `TTL toDateTime(<column>) + toIntervalXxx(N)`
// fragment that upstream templates expect as one slot per signal, or ""
// when ttl <= 0. column is the bare time column retention keys on — Metrics
// use TimeUnix, Logs and Traces spans use Timestamp, the traces lookup uses
// Start. Built via the typed chsql.TableTTL constructor (Add(Call(toDateTime,
// …), Call(toIntervalXxx, …))), which reproduces upstream's
// `internal.GenerateTTLExpr` shape byte-for-byte.
func ttlExpr(column string, ttl time.Duration) string {
	frag := chsql.TableTTL(column, ttl)
	if frag == nil {
		return ""
	}
	return chsql.RenderDDL(frag)
}

// settingsClause renders the leading-comma-continued SETTINGS tail
// (`, k = v, k2 = v2`) for cfg.Settings, or "" when none are configured.
// The fragment continues the `SETTINGS index_granularity=..., ttl_only_drop_parts=1`
// clause the upstream templates already bake, rather than opening a second
// SETTINGS clause. Built via the typed chsql.TableSettings constructor — no
// hand-assembled SQL — so the RHS quoting is type-inferred per entry.
func (c Config) settingsClause() string {
	frag := chsql.TableSettings(c.Settings...)
	if frag == nil {
		return ""
	}
	return chsql.RenderDDL(frag)
}

// appendSettings splices the configured SETTINGS continuation into a rendered
// CREATE TABLE statement, immediately after the baked SETTINGS tail and before
// any trailing newline the template carried. When no extra settings are
// configured it returns stmt unchanged, so the auto-create DDL stays
// byte-identical to the bare upstream template (the backward-compat contract).
// Splicing before the trailing newline (rather than appending after it) keeps
// the continuation part of the SETTINGS line it extends.
func (c Config) appendSettings(stmt string) string {
	clause := c.settingsClause()
	if clause == "" {
		return stmt
	}
	body := strings.TrimRight(stmt, "\n")
	return body + clause + stmt[len(body):]
}

// Apply ensures the configured database exists, then runs CREATE TABLE IF
// NOT EXISTS for each requested signal against conn using the upstream OTel
// exporter's DDL templates. Idempotent: re-running over an existing schema is
// a no-op (the database create and every table template carry `IF NOT
// EXISTS`).
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
//
// The configured database is created first (CREATE DATABASE IF NOT EXISTS) so
// the fully-qualified `<database>.<table>` CREATE statements that follow never
// fail against a non-existent database — the cold-cluster bootstrap path.
func ApplyWithConfig(ctx context.Context, conn driver.Conn, cfg Config, signals []Signal) error {
	cfg = cfg.withDefaults()
	// Validate the config eagerly — BEFORE the empty-signals short-circuit —
	// so a Replicated database engine with no ZooKeeper/Keeper path is rejected
	// regardless of which signals are requested. Validation is pure (it never
	// touches conn), so it's safe ahead of the nil-conn no-op path below; doing
	// it here means a misconfiguration can't hide behind a zero-signal call.
	// Only meaningful when cerberus actually creates the database.
	if !cfg.SkipDatabaseCreate && cfg.DatabaseEngine.Replicated && cfg.DatabaseEngine.ReplicatedZooPath == "" {
		return fmt.Errorf("ddl: replicated database engine requires a ZooKeeper/Keeper path (DatabaseEngine.ReplicatedZooPath)")
	}
	// No signals requested → no tables to create → no database needed. Return
	// before touching conn so an empty-selector caller (and the nil-conn no-op
	// contract its tests pin) never issues a stray CREATE DATABASE.
	if len(signals) == 0 {
		return nil
	}
	// Create the database first (unless it's externally managed) so the
	// fully-qualified `<database>.<table>` table creates never fail against a
	// non-existent database.
	if !cfg.SkipDatabaseCreate {
		if err := conn.Exec(ctx, renderCreateDatabase(cfg)); err != nil {
			return fmt.Errorf("ddl: create database %s: %w", cfg.Database, err)
		}
	}
	for _, s := range signals {
		if err := applySignal(ctx, conn, cfg, s); err != nil {
			return err
		}
	}
	return nil
}

// renderCreateDatabase renders the `CREATE DATABASE IF NOT EXISTS <database>`
// statement via the typed chsql.CreateDatabase builder, mirroring upstream's
// exporter createDatabase. The database name is emitted bare (the upstream
// exporter does not quote it either, and the configured names are simple
// identifiers); IF NOT EXISTS keeps it idempotent. An ON CLUSTER clause is
// added when a cluster is configured, and a `ENGINE = Replicated(...)` clause
// when DatabaseEngine.Replicated is set — the two are mutually exclusive in
// practice (a Replicated database replicates DDL itself), but the builder
// leaves that policy to the caller / config validation.
func renderCreateDatabase(cfg Config) string {
	stmt := chsql.CreateDatabase(cfg.Database).IfNotExists()
	if cfg.Cluster != "" {
		stmt.OnCluster(cfg.Cluster)
	}
	if cfg.DatabaseEngine.Replicated {
		stmt.Engine(chsql.DatabaseEngineReplicated(
			cfg.DatabaseEngine.ReplicatedZooPath,
			cfg.DatabaseEngine.ReplicatedShard,
			cfg.DatabaseEngine.ReplicatedReplica,
		))
	}
	return stmt.SQL()
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
		ttl := ttlExpr("TimeUnix", cfg.TTL.Metrics)
		return []string{
			renderMetricsTable(sqltemplates.MetricsGaugeCreateTable, cfg, cfg.Tables.MetricsGauge, ttl),
			renderMetricsTable(sqltemplates.MetricsSumCreateTable, cfg, cfg.Tables.MetricsSum, ttl),
			renderMetricsTable(sqltemplates.MetricsHistogramCreateTable, cfg, cfg.Tables.MetricsHistogram, ttl),
			renderMetricsTable(sqltemplates.MetricsExpHistogramCreateTable, cfg, cfg.Tables.MetricsExpHistogram, ttl),
			renderMetricsTable(sqltemplates.MetricsSummaryCreateTable, cfg, cfg.Tables.MetricsSummary, ttl),
		}, nil
	case Logs:
		logs, err := renderLogsTable(cfg)
		if err != nil {
			return nil, err
		}
		return []string{logs}, nil
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
	return cfg.appendSettings(
		fmt.Sprintf(tmpl, cfg.Database, table, cfg.clusterClause(), cfg.Engine, ttl),
	)
}

// renderLogsTable renders the logs DDL. The logs template became a
// text/template upstream in v0.152.0 — execute
// [sqltemplates.LogsCreateTableTmpl] against [sqltemplates.CreateTableData],
// mirroring exporter_logs.go's renderCreateLogsTableSQL. The TTL field is
// `toDateTime(Timestamp)` (the dedicated TimestampTime column was removed
// from the schema). HasFullTextSearch stays false: the text-index branch
// needs ClickHouse >= 26.2; false renders the bloom-filter index branch
// that works everywhere cerberus deploys.
func renderLogsTable(cfg Config) (string, error) {
	data := sqltemplates.CreateTableData{
		Database:          cfg.Database,
		TableName:         cfg.Tables.Logs,
		ClusterString:     cfg.clusterClause(),
		Engine:            cfg.Engine,
		TTL:               ttlExpr("Timestamp", cfg.TTL.Logs),
		HasFullTextSearch: false,
	}
	var buf strings.Builder
	if err := sqltemplates.LogsCreateTableTmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("ddl: execute logs create-table template: %w", err)
	}
	return cfg.appendSettings(buf.String()), nil
}

// renderTracesTable formats the traces spans-table DDL. Upstream shape:
// `(database, table, cluster, engine, ttl)`. TTL field is
// `toDateTime(Timestamp)`.
func renderTracesTable(cfg Config) string {
	return cfg.appendSettings(fmt.Sprintf(
		sqltemplates.TracesCreateTable,
		cfg.Database, cfg.Tables.Traces, cfg.clusterClause(),
		cfg.Engine,
		ttlExpr("Timestamp", cfg.TTL.Traces),
	))
}

// renderTracesCreateTsTable formats the `<table>_trace_id_ts` lookup table
// DDL. Upstream shape mirrors the spans table (db, table, cluster, engine,
// ttl) — the `_trace_id_ts` suffix is hard-coded into the template, so the
// caller passes the base traces table name. TTL field is
// `toDateTime(Start)`.
func renderTracesCreateTsTable(cfg Config) string {
	return cfg.appendSettings(fmt.Sprintf(
		sqltemplates.TracesCreateTsTable,
		cfg.Database, cfg.Tables.Traces, cfg.clusterClause(),
		cfg.Engine,
		ttlExpr("Start", cfg.TTL.Traces),
	))
}

// renderTracesCreateTsView formats the `<table>_trace_id_ts_mv`
// materialized-view DDL. Upstream shape is *wider* than the table
// templates: 7 placeholders — (db, table, cluster, db, table, db, table)
// — because the MV references both the lookup table (TO clause) and the
// spans table (FROM clause). See exporter_traces.go's
// renderTraceIDTsMaterializedViewSQL.
func renderTracesCreateTsView(cfg Config) string {
	return fmt.Sprintf(
		sqltemplates.TracesCreateTsView,
		cfg.Database, cfg.Tables.Traces, cfg.clusterClause(),
		cfg.Database, cfg.Tables.Traces,
		cfg.Database, cfg.Tables.Traces,
	)
}
