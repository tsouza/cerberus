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

	// Tiering moves aged parts onto a colder VOLUME of the table's storage
	// policy before TTL deletes them — the `TTL … TO VOLUME '<name>'` action
	// that makes a multi-volume storage_policy do anything at all. The zero
	// value emits no move action, leaving the TTL clause byte-identical to a
	// retention-only deployment. See Tiering.
	Tiering Tiering

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
	// otel_metrics_exponential_histogram, otel_metrics_summary).
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

// Tiering carries the hot/cold storage-tiering rule applied to the
// auto-created tables: the destination VOLUME and the per-signal age at which
// a part moves there.
//
// A ClickHouse `storage_policy` only declares which volumes a table MAY use;
// on its own it never moves anything. Parts are written to the policy's first
// volume and stay there until retention deletes them, so a multi-volume
// (hot/cold) policy with no move rule is inert — the expensive volume holds
// data that should have aged onto the cheap one. Tiering emits the missing
// half: a `TTL <age> TO VOLUME '<Volume>'` action alongside the delete-TTL.
//
// The age is per-signal for the same reason retention is (see TTL): logs age
// out of the hot volume in days where metrics stay for weeks. A zero age for a
// signal emits no move action for that signal's tables; an empty Volume
// disables tiering entirely. Volume must name a volume of the storage policy
// the tables carry (Settings' `storage_policy`, or the server's `default`
// policy when none is set) — Config.Validate cannot check that without a
// server, so internal/preflight verifies it against system.storage_policies at
// boot.
type Tiering struct {
	// Volume is the storage-policy volume aged parts move TO, e.g. "cold".
	// Empty (the default) emits no move action anywhere.
	Volume string

	// Metrics / Logs / Traces are the per-signal ages at which a part moves
	// to Volume, mirroring TTL's per-signal split table-for-table. Each must
	// be shorter than the same signal's retention, or the part is deleted
	// before it ever moves (Config.Validate rejects that).
	Metrics time.Duration
	Logs    time.Duration
	Traces  time.Duration
}

// configured reports whether any signal carries a move age.
func (t Tiering) configured() bool {
	return t.Metrics > 0 || t.Logs > 0 || t.Traces > 0
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
	defaultMetricsExpHistogramTable = "otel_metrics_exponential_histogram"
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

// ttlExpr renders the TTL clause upstream templates expect as one slot per
// signal, or "" when the signal has neither retention nor a move rule. column
// is the bare time column the lifecycle keys on — Metrics use TimeUnix, Logs
// and Traces spans use Timestamp, the traces lookup uses Start.
//
// With no tiering configured this is the retention-only
// `TTL toDateTime(<column>) + toIntervalXxx(N)` that reproduces upstream's
// `internal.GenerateTTLExpr` shape byte-for-byte. With a move age and a
// destination volume it renders both actions in the one clause ClickHouse
// allows: `TTL <move-age> TO VOLUME '<volume>', <retention-age> DELETE`.
// Built via the typed chsql.TableTTLTiered constructor — no hand-assembled SQL.
func (c Config) ttlExpr(column string, retention, moveAfter time.Duration) string {
	frag := chsql.TableTTLTiered(column, retention, moveAfter, c.Tiering.Volume)
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

// Validate rejects the config combinations that would render DDL doing
// something other than what the operator asked for. It is pure — it never
// touches a connection — so both the applying path (ApplyWithConfig), the
// offline rendering path (RenderAll) and the config mapping that feeds them
// (internal/schemaboot) run it, and a misconfiguration fails at boot rather
// than at the first CREATE.
//
// The tiering rules all guard the same failure shape: a setting that is
// ACCEPTED but INERT. A move age with no destination volume, or a volume with
// no age, emits no `TO VOLUME` action at all; a move age at or past the
// signal's retention emits one the delete-TTL always beats. Each is silent —
// the tables are created, queries work, and the only symptom is the storage
// bill — so each is rejected here where it is free and certain to detect.
func (c Config) Validate() error {
	if !c.SkipDatabaseCreate && c.DatabaseEngine.Replicated && c.DatabaseEngine.ReplicatedZooPath == "" {
		return fmt.Errorf("ddl: replicated database engine requires a ZooKeeper/Keeper path (DatabaseEngine.ReplicatedZooPath)")
	}
	if c.Tiering.Volume == "" && c.Tiering.configured() {
		return fmt.Errorf(
			"ddl: storage-tiering age configured with no destination volume (Tiering.Volume) — " +
				"no TTL ... TO VOLUME clause would be emitted and nothing would move",
		)
	}
	if c.Tiering.Volume != "" && !c.Tiering.configured() {
		return fmt.Errorf(
			"ddl: storage-tiering volume %q configured with no move age (Tiering.Metrics / .Logs / .Traces) — "+
				"no TTL ... TO VOLUME clause would be emitted and nothing would move",
			c.Tiering.Volume,
		)
	}
	for _, s := range []struct {
		signal    string
		moveAfter time.Duration
		retention time.Duration
	}{
		{"metrics", c.Tiering.Metrics, c.TTL.Metrics},
		{"logs", c.Tiering.Logs, c.TTL.Logs},
		{"traces", c.Tiering.Traces, c.TTL.Traces},
	} {
		if s.moveAfter > 0 && s.retention > 0 && s.moveAfter >= s.retention {
			return fmt.Errorf(
				"ddl: %s storage-tiering age %s is not shorter than its retention TTL %s — "+
					"parts would be deleted before they ever move to volume %q",
				s.signal, s.moveAfter, s.retention, c.Tiering.Volume,
			)
		}
	}
	return nil
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
	// so a misconfiguration is rejected regardless of which signals are
	// requested. Validation is pure (it never touches conn), so it's safe ahead
	// of the nil-conn no-op path below; doing it here means a misconfiguration
	// can't hide behind a zero-signal call.
	if err := cfg.Validate(); err != nil {
		return err
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

// RenderAll returns, in execution order, the CREATE statements
// ApplyWithConfig would run for the given signals — WITHOUT a ClickHouse
// connection. It exists so offline tooling (the migration preview) can show
// operators the exact schema cerberus expects before they provision anything.
//
// The statement text, their order, the CREATE DATABASE gating, and the
// Replicated-engine validation are identical to ApplyWithConfig; the only
// difference is that the statements are returned instead of Exec'd. Keeping
// the two in lockstep is the contract TestRenderAll_MatchesApply pins: it runs
// ApplyWithConfig against a recording conn and asserts the recorded statements
// equal RenderAll's output.
func RenderAll(cfg Config, signals []Signal) ([]string, error) {
	cfg = cfg.withDefaults()
	// Validate eagerly — before the empty-signals short-circuit — so a
	// misconfiguration is rejected the same way whether the caller renders or
	// applies (mirrors ApplyWithConfig).
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// No signals → no tables → no database. Return before emitting a stray
	// CREATE DATABASE, matching ApplyWithConfig's nil-conn no-op contract.
	if len(signals) == 0 {
		return nil, nil
	}
	var stmts []string
	if !cfg.SkipDatabaseCreate {
		stmts = append(stmts, renderCreateDatabase(cfg))
	}
	for _, s := range signals {
		sig, err := renderSignal(cfg, s)
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, sig...)
	}
	return stmts, nil
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
		ttl := cfg.ttlExpr("TimeUnix", cfg.TTL.Metrics, cfg.Tiering.Metrics)
		stmts := []string{
			renderMetricsTable(sqltemplates.MetricsGaugeCreateTable, cfg, cfg.Tables.MetricsGauge, ttl),
			renderMetricsTable(sqltemplates.MetricsSumCreateTable, cfg, cfg.Tables.MetricsSum, ttl),
			renderMetricsTable(sqltemplates.MetricsHistogramCreateTable, cfg, cfg.Tables.MetricsHistogram, ttl),
			renderMetricsTable(sqltemplates.MetricsExpHistogramCreateTable, cfg, cfg.Tables.MetricsExpHistogram, ttl),
			renderMetricsTable(sqltemplates.MetricsSummaryCreateTable, cfg, cfg.Tables.MetricsSummary, ttl),
		}
		// Each CREATE is immediately followed by the curated registry's
		// idempotent ADD PROJECTION ALTERs on the same table — CREATE precedes
		// ALTER in the slice and applySignal executes sequentially, so an ALTER
		// never races a missing table. Only the catalog tables the metadata
		// enumeration reads (gauge/sum/histogram, see metricTables() in
		// internal/api/prom/metadata.go) carry the projections.
		for _, table := range []string{cfg.Tables.MetricsGauge, cfg.Tables.MetricsSum, cfg.Tables.MetricsHistogram} {
			// Only the sum table carries isMonotonicColumn (see its doc
			// comment) — metadataProjection's body widens for it alone.
			hasMonotonic := table == cfg.Tables.MetricsSum
			for _, p := range metricCatalogProjections {
				stmts = append(stmts, renderAddMetricProjection(cfg, table, p, hasMonotonic))
			}
		}
		return stmts, nil
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

const (
	// seriesProjection is the curated aggregating projection over
	// (MetricName, Attributes) carrying max(TimeUnix). It serves every
	// windowless metadata-enumeration shape the catalog tables answer —
	// label_values(<label>), label-names, label_values(__name__), and series
	// cardinality — from a tiny pre-aggregated part instead of full-scanning
	// the metrics fact table (see internal/api/prom/metadata.go). The coarser
	// __name__ enumeration (`GROUP BY MetricName`) is served from this finer
	// (MetricName, Attributes) projection by ClickHouse's max-of-maxes
	// re-aggregation, so it subsumes the narrower per-name projection #1105
	// shipped — one projection covers all four shapes.
	seriesProjection = "proj_series"
	// metadataProjection carries any(MetricDescription)/any(MetricUnit) per
	// MetricName so the windowless /api/v1/metadata listing reads a
	// pre-aggregated part instead of grouping the whole fact table. It also
	// carries max(TimeUnix) so the same windowless HAVING-bounded shape the
	// enumeration emits routes here too.
	metadataProjection = "proj_metric_metadata"
	// The OTel-CH metric-table columns the projections aggregate over; fixed
	// by the upstream exporter schema, not configurable in this package. The
	// histogram table has no top-level Value column (Value lives only inside
	// the Exemplars Nested block), so the series projection deliberately omits
	// any Value aggregate — none of the routed enumeration shapes read it, and
	// a uniform body keeps one registry entry valid across gauge/sum/histogram.
	metricNameColumn        = "MetricName"
	metricTimeColumn        = "TimeUnix"
	metricAttributesColumn  = "Attributes"
	metricDescriptionColumn = "MetricDescription"
	metricUnitColumn        = "MetricUnit"
	// isMonotonicColumn is the sum table's fixed OTel-CH exporter column
	// distinguishing monotonic Sums (Prom counters) from non-monotonic Sums /
	// UpDownCounters (Prom gauges — see internal/api/prom/metadata.go). It
	// exists ONLY on the sum table, so metadataProjection's body widens for
	// that one table only (see metricCatalogProjections / hasMonotonic).
	isMonotonicColumn = "IsMonotonic"
)

// metricProjection is one curated aggregating projection the DDL apply path
// installs on each metrics catalog table. body is built fresh per call so each
// rendered ALTER owns its QueryBuilder (no shared mutable state across
// tables). hasMonotonic is true only when rendering against the sum table —
// the only catalog table carrying isMonotonicColumn — so a projection can
// widen its aggregated columns for that one table without breaking on
// gauge/histogram, which have no such column.
type metricProjection struct {
	name string
	body func(hasMonotonic bool) *chsql.QueryBuilder
}

// metricCatalogProjections is the curated registry of aggregating projections
// installed (idempotently, ADD PROJECTION IF NOT EXISTS) on each of the
// gauge/sum/histogram catalog tables at boot. Adding a projection here adds it
// to every catalog table; the read-side emitters in internal/api/prom decide
// which enumeration shapes route onto which projection (ClickHouse picks the
// projection at plan time). Backfilling existing parts is a separate
// MATERIALIZE PROJECTION runbook (see docs/operations.md), kept out of the hot
// DDL path so boot stays metadata-only.
var metricCatalogProjections = []metricProjection{
	{
		name: seriesProjection,
		body: func(bool) *chsql.QueryBuilder {
			return chsql.NewQuery().
				Select(
					chsql.Col(metricNameColumn),
					chsql.Col(metricAttributesColumn),
					chsql.Call("max", chsql.Col(metricTimeColumn)),
				).
				GroupBy(chsql.Col(metricNameColumn), chsql.Col(metricAttributesColumn))
		},
	},
	{
		name: metadataProjection,
		body: func(hasMonotonic bool) *chsql.QueryBuilder {
			q := chsql.NewQuery().
				Select(
					chsql.Col(metricNameColumn),
					chsql.Call("any", chsql.Col(metricDescriptionColumn)),
					chsql.Call("any", chsql.Col(metricUnitColumn)),
					chsql.Call("max", chsql.Col(metricTimeColumn)),
				)
			if hasMonotonic {
				// any(IsMonotonic) — monotonicity is invariant per metric
				// name in practice (a property of the OTel metric
				// definition, not a per-sample value), the same assumption
				// any(MetricDescription)/any(MetricUnit) already lean on
				// above. Carrying it lets the sum table's counter/gauge
				// split filter via an aggregate HAVING predicate (routes to
				// this projection) instead of a raw WHERE IsMonotonic
				// (which cannot — see internal/api/prom/metadata.go).
				q.Select(chsql.Call("any", chsql.Col(isMonotonicColumn)))
			}
			return q.GroupBy(chsql.Col(metricNameColumn))
		},
	},
}

// renderAddMetricProjection builds the idempotent ADD PROJECTION ALTER for one
// curated projection on one metrics fact table. ON CLUSTER is threaded so the
// ALTER replicates the same way the CREATE statements do. ADD PROJECTION IF
// NOT EXISTS is metadata-only and idempotent, so the same Apply path covers
// both freshly-created and pre-existing tables.
func renderAddMetricProjection(cfg Config, table string, p metricProjection, hasMonotonic bool) string {
	stmt := chsql.AlterTableAddProjection(cfg.Database, table, p.name, p.body(hasMonotonic))
	if cfg.Cluster != "" {
		stmt.OnCluster(cfg.Cluster)
	}
	return stmt.SQL()
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
		TTL:               cfg.ttlExpr("Timestamp", cfg.TTL.Logs, cfg.Tiering.Logs),
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
		cfg.ttlExpr("Timestamp", cfg.TTL.Traces, cfg.Tiering.Traces),
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
		cfg.ttlExpr("Start", cfg.TTL.Traces, cfg.Tiering.Traces),
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
