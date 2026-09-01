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
	"log/slog"
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

	// ColumnTTL sets operator-opt-in COLUMN-level TTLs on individual heavy
	// payload columns — the logs table's Body and the traces spans table's
	// Events.Attributes / Links.Attributes Nested subcolumns (cerberus
	// issue #2769). The zero value emits no column TTL clause anywhere, so
	// an operator on today's schema sees byte-identical DDL. See ColumnTTL.
	ColumnTTL ColumnTTL

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

	// LogsSettings / TracesSettings append extra SETTINGS to ONLY the logs
	// table / ONLY the traces spans table respectively, continuing the same
	// SETTINGS tail Settings continues but WITHOUT reaching the metrics
	// tables or the traces trace_id_ts lookup table (which carries no Map
	// column at all). This is the per-table application machinery cerberus
	// issue #2774 needed: Settings alone applies uniformly to every
	// auto-created table, so a setting that must land on the two
	// Attributes-bearing signals and nowhere else (chopt's
	// map_bucketed_serialization) cannot be expressed through it. Order is
	// Settings first, then the per-table slice, so a duplicate key across
	// the two is deterministic about which wins the render position (though
	// internal/schemaboot.DDLConfig rejects that duplication outright — see
	// its own guard). The zero value (nil) appends nothing to either table,
	// same backward-compat contract as Settings.
	LogsSettings   []schema.KV
	TracesSettings []schema.KV

	// DeltaPrefixEnabled gates provisioning the DELTA-temporality
	// prefix-reconstruction aggregate table + its materialized view
	// (Tables.MetricsDeltaPrefix; cerberus issue #2389) alongside the five
	// upstream metrics tables. False (the default, matching
	// config.SchemaProvisioning.DeltaPrefixEnabled) renders no DELTA-prefix
	// statements at all — an operator on today's schema sees byte-identical
	// DDL. Unlike the five upstream tables this one is entirely
	// cerberus-authored (see renderDeltaPrefixTable / renderDeltaPrefixView),
	// so it does not need the upstream sqltemplates fork.
	DeltaPrefixEnabled bool

	// DeltaPrefixBucketColumn / DeltaPrefixSumColumn name
	// Tables.MetricsDeltaPrefix's two cerberus-invented columns (the base
	// tables' column names are fixed upstream constants — see
	// metricNameColumn and friends — because auto-create always renders the
	// canonical upstream shape; these two have no upstream template to
	// fix them, so they come from schema.Metrics.DeltaPrefixBucketColumn /
	// DeltaPrefixSumColumn via internal/schemaboot). Empty falls back to
	// "BucketStart" / "PartialSum" (see withDefaults).
	DeltaPrefixBucketColumn string
	DeltaPrefixSumColumn    string

	// ColumnStatisticsEnabled gates the curated `ADD STATISTICS IF NOT
	// EXISTS` ALTER registry (cerberus issue #2766) that installs ClickHouse
	// column statistics on the metrics/logs/traces fact tables' highest-value
	// filter and join columns — real cardinality/selectivity estimates for
	// PREWHERE-pushdown and join-ordering, in place of cerberus's own
	// hand-rolled static heuristic (internal/chsql/prewhere.go). False (the
	// default) renders no statistics statements at all — an operator on
	// today's schema sees byte-identical DDL. The chopt column_statistics
	// feature (version-gated at ClickHouse >= 26.3, the first release with
	// the GA'd statistics optimizer) resolves this bit at boot — see
	// internal/schemaboot.DDLConfig — mirroring how DeltaPrefixEnabled and
	// SchemaMapBucketedSerialization are threaded from their own chopt/config
	// verdicts. Unlike every other statement this package renders, an ADD
	// STATISTICS ALTER can be REFUSED by the connected server — ClickHouse
	// Cloud does not support column statistics at all — so applySignal
	// tolerates that specific refusal instead of failing the whole apply; see
	// isColumnStatisticsUnsupported.
	ColumnStatisticsEnabled bool

	// TraceIDProjectionEnabled gates the curated `ADD PROJECTION IF NOT
	// EXISTS proj_trace_id (SELECT TraceId, _part_offset ORDER BY TraceId)`
	// ALTER (cerberus issue #2767) on the traces spans table and the logs
	// table. False (the default) renders no projection statements at all —
	// an operator on today's schema sees byte-identical DDL. The chopt
	// trace_id_projection feature (version-gated at ClickHouse >= 25.5, the
	// first release accepting `_part_offset` inside a normal projection's
	// SELECT list) resolves this bit at boot — see
	// internal/schemaboot.DDLConfig — mirroring exactly how
	// ColumnStatisticsEnabled is threaded from its own chopt verdict; a
	// deployment below the floor never sees the statement rendered at all,
	// not a statement that is rendered and then refused.
	TraceIDProjectionEnabled bool

	// TextIndexEnabled gates two things (cerberus issue #2773): on the
	// CREATE TABLE branch, it flips the upstream sqltemplates.
	// LogsCreateTableTmpl's HasFullTextSearch flag — see renderLogsTable —
	// which swaps `idx_lower_body` from `tokenbf_v1(32768, 3, 0)` to `TYPE
	// text(tokenizer = 'splitByNonAlpha')`, the SAME index name over the
	// SAME lower(Body) expression, so a brand-new table gets the text index
	// straight away. On an EXISTING table (already carrying the tokenbf
	// branch from an earlier boot), it additionally renders an idempotent
	// `ADD INDEX IF NOT EXISTS idx_body_text lower(Body) TYPE text(...)`
	// ALTER — see renderAddBodyTextIndex's doc comment for why this is a
	// SEPARATELY-named additive index rather than an in-place type swap of
	// idx_lower_body. False (the default) renders byte-identical DDL to
	// today: the tokenbf_v1 branch on CREATE, no additive ALTER. The chopt
	// full_text_index feature (version-gated at ClickHouse >= 26.2, the
	// first release with enable_full_text_index default-on — see the
	// registry entry's doc for the probed-version evidence) resolves this
	// bit at boot — see internal/schemaboot.DDLConfig — mirroring exactly
	// how TraceIDProjectionEnabled is threaded from its own chopt verdict.
	TextIndexEnabled bool

	// LokiLabelCatalogEnabled gates the curated `CREATE TABLE
	// loki_label_catalog` + `CREATE MATERIALIZED VIEW ... REFRESH EVERY 5
	// MINUTE ... TO loki_label_catalog` pair (cerberus issue #2770) that
	// maintains a per-label-key cardinality catalog on top of the logs
	// table, refreshed on a schedule instead of computed per request by
	// `/loki/api/v1/detected_labels` (internal/api/loki/detected_labels.go).
	// False (the default) renders neither statement at all — an operator on
	// today's schema sees byte-identical DDL. The chopt loki_catalog_mv
	// feature (version-gated at ClickHouse >= 24.10, the first release
	// carrying refreshable materialized views GA — upstream PR #70550)
	// resolves this bit at boot — see internal/schemaboot.DDLConfig —
	// mirroring exactly how TraceIDProjectionEnabled is threaded from its
	// own chopt verdict.
	LokiLabelCatalogEnabled bool

	// TempoTagCatalogEnabled gates the curated `CREATE TABLE
	// tempo_tag_catalog` + `CREATE MATERIALIZED VIEW ... REFRESH EVERY 5
	// MINUTE ... TO tempo_tag_catalog` pair (cerberus issue #2771, the
	// Tempo sibling of LokiLabelCatalogEnabled) that maintains a
	// per-(scope, tag-key) top-values catalog on top of the traces table,
	// refreshed on a schedule instead of scanned per request by
	// `/api/v2/search/tags` and `/api/search/tag/{name}/values`
	// (internal/api/tempo/search_tags.go, search_tag_values.go). False
	// (the default) renders neither statement at all — an operator on
	// today's schema sees byte-identical DDL. The chopt
	// tempo_tag_catalog_mv feature (version-gated at ClickHouse >= 24.10,
	// the same refreshable-materialized-view floor LokiLabelCatalogEnabled
	// uses) resolves this bit at boot — see internal/schemaboot.DDLConfig
	// — mirroring exactly how LokiLabelCatalogEnabled is threaded from its
	// own chopt verdict.
	TempoTagCatalogEnabled bool

	// DownsampleTierEnabled gates provisioning the operator opt-in
	// downsampled long-range tier (schema.DownsampleTierTable +
	// its materialized view; cerberus issue #2751) alongside the five
	// upstream metrics tables. False (the default) renders no
	// downsample-tier statements at all — an operator on today's schema
	// sees byte-identical DDL. Like DeltaPrefixEnabled this table is
	// entirely cerberus-authored (see renderDownsampleTierTable /
	// renderDownsampleTierView), so it needs no upstream sqltemplates fork.
	//
	// Unlike DeltaPrefixEnabled, this is threaded DIRECTLY from the resolved
	// chopt.FeatureDownsampleTier boot verdict (internal/schemaboot.
	// DDLConfig, cmd/cerberus's chOptResolution) — mirroring
	// ColumnStatisticsEnabled / TraceIDProjectionEnabled above, not
	// DeltaPrefixEnabled's own dedicated CERBERUS_SCHEMA_*_ENABLED env var —
	// because this feature genuinely has a ClickHouse version floor to
	// probe (timeSeriesLastTwoSamples, floor 25.9) the way those two do,
	// and because a not-yet-backfilled bucket degrades safely (see
	// schema.DownsampleTierTable's doc) rather than needing DeltaPrefix's
	// separate later "backfill verified" declaration.
	DownsampleTierEnabled bool
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

// ColumnTTL carries operator-opt-in COLUMN-level TTL durations for
// individual heavy payload columns (cerberus issue #2769) — the logs
// table's Body and the traces spans table's Events.Attributes /
// Links.Attributes Nested subcolumns. A zero duration leaves that column
// with no TTL clause, the same zero-means-off convention TTL and Tiering
// already use above — there is deliberately no separate enabled bool.
//
// This is a genuinely different retention shape from TTL: TTL's DELETE
// action drops the WHOLE row once it ages out, so keeping N days of
// queryable log aggregates means storing N days of full Body text (and
// span Events/Links) even though the row's other columns — severity,
// attributes, materialized k8s columns, trace correlation — are a fraction
// of the size. ColumnTTL lets the row survive the signal's own TTL.{Logs,
// Traces} retention while its heaviest column empties earlier, so an
// operator can keep 30d of queryable aggregates while paying full-text
// storage for only the most recent 7d.
//
// Body and Events/Links.Attributes are chosen the same way ColumnStatistics
// chose its columns (cerberus issue #2766): the curated, highest-value
// targets, not a blanket sweep. Body is the logs table's dominant column by
// size in virtually every real corpus (see docs/operations.md's storage
// notes); Events.Attributes / Links.Attributes are the traces table's
// analogous unbounded-width Map columns — Events.Name / Events.Timestamp /
// Links.TraceId / Links.SpanId / Links.TraceState are small, bounded-width
// identifiers a real deployment gets negligible storage benefit from
// TTL-ing, so they are deliberately left at the row's own full retention
// (needed for span-link / event-name aggregation over the whole window).
//
// A column TTL is accepted on a column carrying a data-skipping index
// (idx_lower_body / idx_body_text) and on a Nested subcolumn — both
// verified empirically against a real ClickHouse server; see
// chsql.ModifyColumnTTLBuilder's doc comment for the exact probes. Setting
// a column TTL that never fires before the row's own TTL deletes it first
// is inert and rejected by Config.Validate, mirroring the existing
// Tiering-vs-retention inert-combination check.
type ColumnTTL struct {
	// LogsBody is the logs table's Body column TTL, keyed on Timestamp —
	// the same time column TTL.Logs keys the row's own retention on.
	LogsBody time.Duration

	// TracesEventsLinks is the traces spans table's Events.Attributes AND
	// Links.Attributes Nested-subcolumn TTL, keyed on Timestamp — the same
	// time column TTL.Traces keys the row's own retention on. One knob
	// covers both subcolumns: they are each Nested block's own payload
	// column and share a lifecycle the same way the five metrics tables
	// share TTL.Metrics.
	TracesEventsLinks time.Duration
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
	// MetricsDeltaPrefix names the DELTA-temporality prefix-reconstruction
	// aggregate table (cerberus issue #2389). Unlike every other field on
	// this struct it has no upstream template — see DeltaPrefixEnabled.
	MetricsDeltaPrefix string
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

	// defaultMetricsDeltaPrefixTable / defaultDeltaPrefixBucketColumn /
	// defaultDeltaPrefixSumColumn mirror schema.DefaultOTelMetrics()'s
	// DeltaPrefixTable / DeltaPrefixBucketColumn / DeltaPrefixSumColumn
	// defaults (cerberus issue #2389) — kept in lockstep by
	// TestDeltaPrefixDefaultsMatchSchemaPackage.
	defaultMetricsDeltaPrefixTable = "otel_metrics_sum_delta_prefix"
	defaultDeltaPrefixBucketColumn = "BucketStart"
	defaultDeltaPrefixSumColumn    = "PartialSum"
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
	if c.Tables.MetricsDeltaPrefix == "" {
		c.Tables.MetricsDeltaPrefix = defaultMetricsDeltaPrefixTable
	}
	if c.DeltaPrefixBucketColumn == "" {
		c.DeltaPrefixBucketColumn = defaultDeltaPrefixBucketColumn
	}
	if c.DeltaPrefixSumColumn == "" {
		c.DeltaPrefixSumColumn = defaultDeltaPrefixSumColumn
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
// (`, k = v, k2 = v2`) for cfg.Settings plus any per-table extra entries, or
// "" when none are configured. The fragment continues the
// `SETTINGS index_granularity=..., ttl_only_drop_parts=1` clause the upstream
// templates already bake, rather than opening a second SETTINGS clause. Built
// via the typed chsql.TableSettings constructor — no hand-assembled SQL — so
// the RHS quoting is type-inferred per entry. extra is appended AFTER
// c.Settings — see LogsSettings / TracesSettings.
func (c Config) settingsClause(extra ...schema.KV) string {
	all := c.Settings
	if len(extra) > 0 {
		all = append(append([]schema.KV{}, c.Settings...), extra...)
	}
	frag := chsql.TableSettings(all...)
	if frag == nil {
		return ""
	}
	return chsql.RenderDDL(frag)
}

// appendSettings splices the configured SETTINGS continuation (c.Settings
// plus any per-table extra entries — see LogsSettings / TracesSettings) into
// a rendered CREATE TABLE statement, immediately after the baked SETTINGS
// tail and before any trailing newline the template carried. When no extra
// settings are configured it returns stmt unchanged, so the auto-create DDL
// stays byte-identical to the bare upstream template (the backward-compat
// contract). Splicing before the trailing newline (rather than appending
// after it) keeps the continuation part of the SETTINGS line it extends.
func (c Config) appendSettings(stmt string, extra ...schema.KV) string {
	clause := c.settingsClause(extra...)
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
	// A column TTL that never fires before the row's own retention TTL
	// deletes the row first is the same "accepted but inert" shape the
	// tiering checks above reject: the column ALTER applies, but no query
	// ever observes an emptied column before the whole row is gone.
	for _, s := range []struct {
		signal    string
		column    string
		columnTTL time.Duration
		rowTTL    time.Duration
	}{
		{"logs", "Body", c.ColumnTTL.LogsBody, c.TTL.Logs},
		{"traces", "Events.Attributes / Links.Attributes", c.ColumnTTL.TracesEventsLinks, c.TTL.Traces},
	} {
		if s.columnTTL > 0 && s.rowTTL > 0 && s.columnTTL >= s.rowTTL {
			return fmt.Errorf(
				"ddl: %s column TTL %s (%s) is not shorter than the row's own retention TTL %s — "+
					"the row would be deleted before the column ever empties",
				s.signal, s.column, s.columnTTL, s.rowTTL,
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
			// A column-statistics ALTER (issue #2766) can be legitimately
			// REFUSED by the connected server — ClickHouse Cloud supports no
			// statistics at all — and that refusal must not fail the whole
			// signal's apply (nor, via setupSchema's retry loop, wedge
			// /readyz "pending" forever on a Cloud deployment). Every other
			// statement in this slice is either a CREATE the boot path
			// genuinely cannot proceed without, or an idempotent ALTER
			// ClickHouse Cloud DOES support (ADD PROJECTION / ADD INDEX), so
			// this check is scoped tightly enough that it can run
			// unconditionally over every exec error rather than needing to
			// know in advance which statement produced it.
			if isColumnStatisticsUnsupported(err) {
				slog.Default().Warn(
					"ddl: column statistics ALTER rejected by server; skipping (unsupported on ClickHouse Cloud — cerberus issue #2766)",
					"signal", s, "err", err,
				)
				continue
			}
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
		// The AggregationTemporality skip index applies only to the two
		// tables that carry the column AND that a rate()/increase() range
		// window can route to natively (see
		// internal/promql.rangeVectorCounterTemporalityColumn) — the sum and
		// histogram tables. Gauge never carries AggregationTemporality;
		// exp_histogram carries it but sits outside every temporality-aware
		// native/fan-out routing path today (see #1628's scope note), so
		// indexing it would add write-path cost the read side never spends.
		for _, table := range []string{cfg.Tables.MetricsSum, cfg.Tables.MetricsHistogram} {
			stmts = append(stmts, renderAddTemporalityIndex(cfg, table))
		}
		// The DELTA-prefix aggregate table + its materialized view (cerberus
		// issue #2389) are entirely opt-in and entirely cerberus-authored — no
		// upstream template backs either statement, unlike every CREATE above.
		// CREATE precedes the MV (which references it in its TO clause) the
		// same way the traces lookup table precedes its own MV.
		if cfg.DeltaPrefixEnabled {
			stmts = append(
				stmts,
				renderDeltaPrefixTable(cfg),
				renderDeltaPrefixView(cfg),
			)
		}
		// The downsampled long-range tier (cerberus issue #2751) is entirely
		// opt-in and entirely cerberus-authored, the same shape as the
		// DELTA-prefix pair immediately above — CREATE precedes the MV
		// (which references it in its TO clause).
		if cfg.DownsampleTierEnabled {
			stmts = append(
				stmts,
				renderDownsampleTierTable(cfg),
				renderDownsampleTierView(cfg),
			)
		}
		// Column statistics (issue #2766) trail every other metrics
		// statement — CREATE, then projections, then the temporality index,
		// then the DELTA-prefix pair — for the same reason the temporality
		// index trails the projections: each ALTER targets a table a
		// preceding statement in this slice already created.
		if cfg.ColumnStatisticsEnabled {
			stmts = append(stmts, renderMetricsColumnStatistics(cfg)...)
		}
		return stmts, nil
	case Logs:
		logs, err := renderLogsTable(cfg)
		if err != nil {
			return nil, err
		}
		// The curated Body codec ALTER (issue #2768) trails the CREATE the
		// same way every other curated ALTER in this package trails its
		// table's CREATE — see renderLogsCodecs' doc comment.
		stmts := append([]string{logs}, renderLogsCodecs(cfg)...)
		// The curated proj_trace_id projection (issue #2767) trails the CREATE
		// + codec ALTERs the same way every other curated ALTER in this
		// package trails its table's CREATE — see TraceIDProjectionEnabled's
		// doc comment on why the logs table carries it too, not traces alone.
		if cfg.TraceIDProjectionEnabled {
			stmts = append(stmts, renderAddTraceIDProjection(cfg, cfg.Tables.Logs))
		}
		// The additive idx_body_text index (issue #2773) trails every other
		// curated ALTER in this package the same way — see
		// renderAddBodyTextIndex's doc comment for why it targets a
		// separately-named index rather than swapping idx_lower_body in
		// place.
		if cfg.TextIndexEnabled {
			stmts = append(stmts, renderAddBodyTextIndex(cfg))
		}
		if cfg.ColumnStatisticsEnabled {
			stmts = append(stmts, renderLogsColumnStatistics(cfg)...)
		}
		// The curated Body column TTL (issue #2769) trails every other
		// curated ALTER in this package the same way — see ColumnTTL's doc
		// comment. A zero LogsBody duration (the default) renders nothing
		// here, matching every other ColumnTTL/TTL/Tiering zero-means-off
		// field.
		if cfg.ColumnTTL.LogsBody > 0 {
			stmts = append(stmts, renderLogsBodyTTL(cfg))
		}
		// The loki label-cardinality catalog table + its refreshable MV
		// (issue #2770) trail every other logs statement, mirroring the
		// DELTA-prefix pair's CREATE-then-MV order on the metrics
		// signal — the catalog table must exist before the MV's TO clause
		// can reference it.
		if cfg.LokiLabelCatalogEnabled {
			stmts = append(
				stmts,
				renderLokiLabelCatalogTable(cfg),
				renderLokiLabelCatalogView(cfg),
			)
		}
		return stmts, nil
	case Traces:
		stmts := []string{
			renderTracesTable(cfg),
			renderTracesCreateTsTable(cfg),
			renderTracesCreateTsView(cfg),
		}
		// The curated Duration codec ALTER (issue #2768) targets the spans
		// table the first statement above just created — see
		// renderTracesCodecs' doc comment.
		stmts = append(stmts, renderTracesCodecs(cfg)...)
		// The curated proj_trace_id projection (issue #2767) targets the
		// spans table too — see TraceIDProjectionEnabled's doc comment.
		if cfg.TraceIDProjectionEnabled {
			stmts = append(stmts, renderAddTraceIDProjection(cfg, cfg.Tables.Traces))
		}
		if cfg.ColumnStatisticsEnabled {
			stmts = append(stmts, renderTracesColumnStatistics(cfg)...)
		}
		// The curated Events.Attributes / Links.Attributes column TTL
		// (issue #2769) trails every other curated ALTER in this package
		// the same way — see ColumnTTL's doc comment. A zero
		// TracesEventsLinks duration (the default) renders nothing here,
		// matching every other ColumnTTL/TTL/Tiering zero-means-off field.
		if cfg.ColumnTTL.TracesEventsLinks > 0 {
			stmts = append(stmts, renderTracesEventsLinksTTL(cfg)...)
		}
		// The Tempo tag catalog table + its refreshable MV (issue #2771)
		// trail every other traces statement, mirroring the loki
		// label-catalog pair's own CREATE-then-MV order on the logs
		// signal — the catalog table must exist before the MV's TO clause
		// can reference it.
		if cfg.TempoTagCatalogEnabled {
			stmts = append(
				stmts,
				renderTempoTagCatalogTable(cfg),
				renderTempoTagCatalogView(cfg),
			)
		}
		return stmts, nil
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
	// aggregationTemporalityColumn is the OTel-CH exporter's fixed
	// AggregationTemporality column on the sum and histogram tables — see
	// renderAddTemporalityIndex.
	aggregationTemporalityColumn = "AggregationTemporality"
	// isMonotonicColumn is the sum table's fixed OTel-CH exporter column
	// distinguishing monotonic Sums (Prom counters) from non-monotonic Sums /
	// UpDownCounters (Prom gauges — see internal/api/prom/metadata.go). It
	// exists ONLY on the sum table, so metadataProjection's body widens for
	// that one table only (see metricCatalogProjections / hasMonotonic).
	isMonotonicColumn = "IsMonotonic"
	// metricResourceAttributesColumn / metricServiceNameColumn /
	// metricValueColumn round out the sum table's identity + value columns
	// the DELTA-prefix table's CREATE + MV reference (renderDeltaPrefixTable
	// / renderDeltaPrefixView) — fixed OTel-CH exporter names, like every
	// other metricXxxColumn constant above.
	metricResourceAttributesColumn = "ResourceAttributes"
	metricServiceNameColumn        = "ServiceName"
	metricValueColumn              = "Value"
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

const (
	// traceIDProjectionName is the ALTER TABLE ADD PROJECTION identifier for
	// the exact-row TraceId lookup projection (cerberus issue #2767) — see
	// TraceIDProjectionEnabled's doc comment.
	traceIDProjectionName = "proj_trace_id"
	// partOffsetColumn is ClickHouse's per-part virtual row-offset column.
	// Selecting it (alongside the sort key) into a projection's body, rather
	// than every column of the base table, is what makes the projection a
	// lightweight secondary index — the merge engine stores only the sort
	// key + this pointer, not a second copy of every row — instead of a full
	// materialized copy of the table under a different sort order. Requires
	// ClickHouse >= 25.5 (upstream PR #78429); see FeatureTraceIDProjection.
	partOffsetColumn = "_part_offset"
)

// traceIDProjectionBody is the ADD PROJECTION body `SELECT TraceId,
// _part_offset ORDER BY TraceId` — no GROUP BY, unlike
// metricCatalogProjections' aggregating projections: this one re-sorts the
// same two columns rather than pre-aggregating them, which is exactly what
// makes it usable as a secondary index (ClickHouse can binary-search the
// projection's own TraceId-ordered part for the seek, then dereference
// _part_offset back into the base part for the matching rows).
func traceIDProjectionBody() *chsql.QueryBuilder {
	return chsql.NewQuery().
		Select(chsql.Col(traceIdColumn), chsql.Col(partOffsetColumn)).
		OrderBy(chsql.Col(traceIdColumn), false)
}

// renderAddTraceIDProjection builds the idempotent ADD PROJECTION ALTER
// installing proj_trace_id on table (otel_traces or otel_logs). ON CLUSTER is
// threaded so the ALTER replicates the same way the CREATE statements do; ADD
// PROJECTION IF NOT EXISTS is metadata-only and idempotent, so the same Apply
// path covers both freshly-created and pre-existing tables — mirroring
// renderAddMetricProjection / renderAddTemporalityIndex. Unlike ADD
// STATISTICS, ADD PROJECTION is supported on ClickHouse Cloud (see
// applySignal's own comment), so this needs no unsupported-server tolerance.
func renderAddTraceIDProjection(cfg Config, table string) string {
	stmt := chsql.AlterTableAddProjection(cfg.Database, table, traceIDProjectionName, traceIDProjectionBody())
	if cfg.Cluster != "" {
		stmt.OnCluster(cfg.Cluster)
	}
	return stmt.SQL()
}

const (
	// bodyTextIndexName is the ALTER TABLE ADD INDEX identifier
	// renderAddBodyTextIndex installs on an EXISTING logs table (cerberus
	// issue #2773) — see that function's doc comment for why it is a
	// SEPARATE name from idx_lower_body rather than an in-place type swap.
	bodyTextIndexName = "idx_body_text"
	// bodyTextIndexType is the ClickHouse index type clause: a word-level
	// text index over the tokenizer the upstream template's own
	// HasFullTextSearch=true branch uses for idx_lower_body (sqltemplates'
	// logs_table.sql), so a freshly-created table and an upgraded existing
	// one tokenize identically.
	bodyTextIndexType = "text(tokenizer = 'splitByNonAlpha')"
	// bodyTextIndexGranularity mirrors ClickHouse's OWN implicit default
	// for a `text` index type when no GRANULARITY clause is given —
	// confirmed by omitting the clause against a live ClickHouse 26.6.3.62
	// server and reading back `EXPLAIN indexes=1`, which reports "text
	// GRANULARITY 100000000" — and re-confirmed byte-for-byte identical
	// (same EXPLAIN Description, same pruning) when the clause is stamped
	// explicitly with this value. chsql.AlterTableAddIndex has no
	// omit-GRANULARITY mode (every other index type it renders — minmax,
	// bloom_filter — requires an explicit, meaningful one), so this
	// constant reproduces the upstream template's own implicit choice
	// rather than widening that builder's contract for one index type.
	bodyTextIndexGranularity = 100000000
)

// renderAddBodyTextIndex builds the idempotent ADD INDEX ALTER installing a
// text-type skip index over lower(Body) on an EXISTING logs table (cerberus
// issue #2773, cfg.TextIndexEnabled).
//
// A SEPARATELY-named index (idx_body_text), not an in-place upgrade of
// idx_lower_body: the upstream template's HasFullTextSearch branch reuses
// idx_lower_body's name for BOTH the tokenbf_v1 (false) and text (true)
// index types (sqltemplates' logs_table.sql), which only works because a
// CREATE TABLE ... IF NOT EXISTS never re-runs against a table that already
// exists. An ALTER TABLE ADD INDEX IF NOT EXISTS idx_lower_body against a
// table that already carries idx_lower_body as tokenbf_v1 is a silent
// NO-OP — ClickHouse matches on name, not type — so it could never install
// the text index on an upgraded deployment. Swapping the TYPE of an
// existing index requires DROP INDEX + ADD INDEX, which is destructive
// (existing MATERIALIZE'd granules are discarded, forcing a full backfill)
// and this render-time-only package has no live system.data_skipping_indexes
// read to tell whether idx_lower_body is ALREADY the text type (in which
// case dropping and re-adding it on every boot would be pure, repeated,
// backfill-losing churn). Installing a second, differently-named index is
// the only additive, idempotent, crash-safe option available here — the
// SAME reasoning every other ALTER in this package already follows (ADD
// PROJECTION, ADD STATISTICS, ADD INDEX all install NEW, non-colliding
// names). Retiring the now-redundant legacy idx_lower_body tokenbf index on
// upgraded tables is left to a dedicated follow-up (cerberus issue #2773's
// PR body links it) — dropping an index an operator's running queries may
// still be planning against is a real production-cluster risk this
// render-time DDL apply should not make unilaterally.
//
// Adding a skip index is metadata-only for NEW parts; EXISTING parts need
// the one-time `ALTER TABLE ... MATERIALIZE INDEX idx_body_text` backfill
// to benefit retroactively — see docs/operations.md, mirroring
// renderAddTemporalityIndex's own backfill note.
func renderAddBodyTextIndex(cfg Config) string {
	expr := chsql.Call("lower", chsql.Col(bodyColumn))
	stmt := chsql.AlterTableAddIndex(cfg.Database, cfg.Tables.Logs, bodyTextIndexName, expr, bodyTextIndexType, bodyTextIndexGranularity)
	if cfg.Cluster != "" {
		stmt.OnCluster(cfg.Cluster)
	}
	return stmt.SQL()
}

const (
	// temporalityIndexName is the ALTER TABLE ADD INDEX identifier for the
	// AggregationTemporality skip index — see renderAddTemporalityIndex.
	temporalityIndexName = "idx_agg_temporality"
	// temporalityIndexGranularity is the number of index-granule marks per
	// skip-index block: GRANULARITY 1 gives the finest possible skip
	// resolution (one mark per table index_granularity, 8192 rows by
	// default), maximising the odds that a granule ClickHouse can prove is
	// single-temporality gets skipped. A coarser granularity would only
	// widen each mark's [min,max] range across more granules, making a
	// skip less likely without saving anything else — minmax marks cost
	// bytes, not read time, so there is no tradeoff pushing the other way.
	temporalityIndexGranularity = 1
)

// renderAddTemporalityIndex builds the idempotent ADD INDEX ALTER installing
// a minmax skip index on the AggregationTemporality column of one metrics
// fact table.
//
// Issue #2458: NativeRateLowerer.LowerRate splits a temporality-bearing
// rate()/increase() range window into a CUMULATIVE arm (the native
// timeSeriesRateToGrid aggregate, fed only non-DELTA rows) and a DELTA arm
// (the fan-out emitter, fed only DELTA rows) — chplan.UnionAll{
// RangeWindowGridNative, RangeWindow}, the exact shape #2114/#2117/#2120's
// solver routing was built to slice and re-anchor. Both arms scan the SAME
// base table with the SAME MetricName/Attributes predicate, differing only
// in a trailing AggregationTemporality conjunct — a column absent from the
// table's ORDER BY, so ClickHouse cannot use the primary key to skip
// granules on it and reads every matching row from BOTH arms (a confirmed,
// reproducible 2.00x read_rows ratio on real production-shaped data). This
// is what a minmax skip index fixes: real OTel deployments set
// AggregationTemporality once per exporter configuration, so a given
// series' samples land in temporality-homogeneous runs almost always
// (confirmed empirically against ClickHouse 25.8 — an all-CUMULATIVE
// table's `AggregationTemporality = <DELTA>` scan skips every one of its
// granules once this index exists). The plan-level split's EXISTENCE is
// deliberately left unchanged (see the issue's own scope note): only the
// redundant physical read shrinks, addressed entirely at the schema layer
// with no chplan/chsql/promql change and hence no risk to the solver
// routing tests that depend on the split's shape.
//
// Adding a skip index is metadata-only for NEW parts — unlike ADD
// PROJECTION it does not store a second physical copy of the table — but
// EXISTING parts need a one-time `ALTER TABLE ... MATERIALIZE INDEX
// idx_agg_temporality` backfill to benefit retroactively; see
// docs/operations.md. ON CLUSTER is threaded so the ALTER replicates the
// same way the CREATE statements do. ADD INDEX IF NOT EXISTS is idempotent,
// so the same Apply path covers both freshly-created and pre-existing
// tables.
func renderAddTemporalityIndex(cfg Config, table string) string {
	stmt := chsql.AlterTableAddIndex(cfg.Database, table, temporalityIndexName,
		chsql.Col(aggregationTemporalityColumn), "minmax", temporalityIndexGranularity)
	if cfg.Cluster != "" {
		stmt.OnCluster(cfg.Cluster)
	}
	return stmt.SQL()
}

// cerberusOwnedViewSuffix is appended to a cerberus-owned target table's
// name to name its materialized view — matches the upstream traces lookup
// table's own `<table>_mv` convention (renderTracesCreateTsView), even
// though these MVs are entirely cerberus-authored. Used by the DELTA-prefix
// aggregate view; the loki label-catalog view uses the equivalent
// schema.LabelCatalogViewSuffix instead of this package-local constant
// because, unlike the DELTA-prefix view's name, its exact name is also
// needed OUTSIDE this package (internal/api/info, to query
// system.view_refreshes) — see that constant's own doc comment.
const cerberusOwnedViewSuffix = "_mv"

// renderDeltaPrefixTable renders the CREATE TABLE for the DELTA-temporality
// prefix-reconstruction aggregate table (cerberus issue #2389, plan §4.1,
// ORDER BY revised per the plan's follow-up "Cost 2" measurement: BucketStart
// moves to position 2, immediately after MetricName, so a query bounded to
// one metric + a date range prunes on the primary key instead of scanning
// every bucket of every series for that metric — measured 34% of the table
// read with this order vs 71% with BucketStart last).
//
// SimpleAggregateFunction(sum, Float64) on AggregatingMergeTree is what
// makes a plain `sum(PartialSum)` read correct regardless of how much
// background merging has happened — no FINAL, no staleness window (unlike
// ReplacingMergeTree, which would need one). The engine is ALWAYS
// AggregatingMergeTree / ReplicatedAggregatingMergeTree — never cfg.Engine's
// operator override, which targets the five upstream MergeTree-family
// tables. A SimpleAggregateFunction column requires the Aggregating family
// specifically: swapping engine families here is this table's own
// correctness requirement, not a deployment preference cfg.Engine can
// override.
func renderDeltaPrefixTable(cfg Config) string {
	lcString := chsql.TypeLowCardinality(chsql.TypeRaw("String"))
	mapType := chsql.Call("Map", lcString, chsql.TypeRaw("String"))
	engine := chsql.EngineAggregatingMergeTree()
	if cfg.DatabaseEngine.Replicated {
		engine = chsql.EngineReplicatedAggregatingMergeTree()
	}
	return chsql.CreateTable(cfg.Tables.MetricsDeltaPrefix).
		Database(cfg.Database).
		IfNotExists().
		Columns(
			chsql.ColumnDef{Name: metricNameColumn, Type: lcString},
			chsql.ColumnDef{Name: metricAttributesColumn, Type: mapType},
			chsql.ColumnDef{Name: metricResourceAttributesColumn, Type: mapType},
			chsql.ColumnDef{Name: metricServiceNameColumn, Type: lcString},
			chsql.ColumnDef{Name: cfg.DeltaPrefixBucketColumn, Type: chsql.Call("DateTime64", chsql.InlineLit(int64(9)))},
			chsql.ColumnDef{
				Name: cfg.DeltaPrefixSumColumn,
				Type: chsql.Call("SimpleAggregateFunction", chsql.BareIdent("sum"), chsql.TypeRaw("Float64")),
			},
		).
		Engine(engine).
		OrderBy(metricNameColumn, cfg.DeltaPrefixBucketColumn, metricAttributesColumn, metricResourceAttributesColumn, metricServiceNameColumn).
		TTL(chsql.TableTTLTiered(cfg.DeltaPrefixBucketColumn, cfg.TTL.Metrics, cfg.Tiering.Metrics, cfg.Tiering.Volume)).
		SQL()
}

// renderDeltaPrefixView renders the CREATE MATERIALIZED VIEW feeding
// renderDeltaPrefixTable from the sum table (plan §4.1): every
// DELTA-temporality row, bucketed to its calendar day. It captures only
// rows inserted from the moment this statement runs onward — ClickHouse
// does NOT retroactively process existing rows — so pre-existing history
// needs the separate `cerberus schema delta-prefix-backfill` one-time
// pass (see docs/operations.md and internal/deltaprefix).
func renderDeltaPrefixView(cfg Config) string {
	bucket := chsql.Call("toStartOfDay", chsql.Col(metricTimeColumn))
	body := chsql.NewQuery().
		Select(
			chsql.Col(metricNameColumn),
			chsql.Col(metricAttributesColumn),
			chsql.Col(metricResourceAttributesColumn),
			chsql.Col(metricServiceNameColumn),
			chsql.As(bucket, cfg.DeltaPrefixBucketColumn),
			chsql.As(chsql.Call("sum", chsql.Col(metricValueColumn)), cfg.DeltaPrefixSumColumn),
		).
		From(chsql.Qual(cfg.Database, cfg.Tables.MetricsSum)).
		Where(chsql.Eq(chsql.Col(aggregationTemporalityColumn), chsql.InlineLit(schema.AggregationTemporalityDelta))).
		GroupBy(
			chsql.Col(metricNameColumn),
			chsql.Col(metricAttributesColumn),
			chsql.Col(metricResourceAttributesColumn),
			chsql.Col(metricServiceNameColumn),
			bucket,
		)
	stmt := chsql.CreateMaterializedView(cfg.Tables.MetricsDeltaPrefix+cerberusOwnedViewSuffix).
		Database(cfg.Database).
		IfNotExists().
		To(cfg.Database, cfg.Tables.MetricsDeltaPrefix).
		As(body)
	if cfg.Cluster != "" {
		stmt.OnCluster(cfg.Cluster)
	}
	return stmt.SQL()
}

// --- Downsampled long-range tier (cerberus issue #2751) ---

// downsampleTierBucketEndExpr renders the tier's bucket-boundary expression
// over col (metricTimeColumn on the write side; see downsampletier's own
// backfillBucketEndExpr, which MUST render the identical expression so a
// backfilled row and a live-MV row for the same raw sample land in the same
// bucket — see that package's doc comment for why the two copies cannot
// share a Go func: internal/schema/ddl and internal/downsampletier are both
// leaf packages neither may import the other's exported surface for one
// shared helper alone).
//
// It computes the CEILING bucket boundary — `bucket * ceil(t / bucket)` —
// NOT ClickHouse's native toStartOfInterval FLOOR convention
// (deltaprefix's own dayBucket). This is deliberate: PromQL's range-vector
// window is HALF-OPEN LEFT, CLOSED RIGHT — `(anchor-range, anchor]` — so a
// raw sample landing exactly ON a bucket boundary must join the bucket
// ENDING at that instant, not the one STARTING there. A plain
// toStartOfInterval(t, bucket) bucket is closed-left/open-right, which is
// off-by-one at BOTH edges against that PromQL window; the ceiling
// convention here reproduces PromQL's half-open-left/closed-right window
// exactly (verified empirically against a real ClickHouse boundary-exact
// sample — see internal/chsql's downsample-tier chDB test): every raw
// sample is guaranteed to land in exactly the ONE bucket row whose
// `(BucketStart, BucketEnd]` interval contains its timestamp, never split
// or duplicated across two rows. That unambiguous per-sample assignment is
// what makes the read side's bucket-to-anchor mapping exact — a query whose
// range equals the bucket (internal/chsql's emitRangeWindowDownsampleTier,
// cerberus issue #2751) looks up a SINGLE bucket row per grid anchor with
// no merge or re-filter at all; a query spanning N buckets (the same
// function, extended by issue #2857) merges exactly the N bucket rows whose
// BucketEnd falls in the anchor's own window, with no gap or overlap
// between them, and re-filters the merged result as a defensive check
// against malformed data rather than a correctness requirement of this
// boundary expression.
//
// Computed as `toStartOfInterval(t - toIntervalNanosecond(1), bucket) +
// bucket`: subtracting one nanosecond (TimeUnix's own DateTime64(9)
// precision) before flooring turns ClickHouse's floor into the ceiling
// PromQL's window needs, EXCEPT for a sample exactly ON a bucket's OWN
// start (which correctly stays in that same, now-ending-here bucket rather
// than jumping to the one before it) — see the chDB test's boundary-exact
// case for the worked arithmetic.
func downsampleTierBucketEndExpr(col string, bucket time.Duration) chsql.Frag {
	bucketSeconds := int64(bucket / time.Second)
	epsilon := chsql.Sub(chsql.Col(col), chsql.Call("toIntervalNanosecond", chsql.InlineLit(int64(1))))
	floor := chsql.Call("toStartOfInterval", epsilon, chsql.Call("toIntervalSecond", chsql.InlineLit(bucketSeconds)))
	return chsql.Add(floor, chsql.Call("toIntervalSecond", chsql.InlineLit(bucketSeconds)))
}

// renderDownsampleTierTable renders the CREATE TABLE for the downsampled
// long-range tier (cerberus issue #2751): one row per (series, bucket),
// carrying an AggregateFunction(timeSeriesLastTwoSamples, ...) state folding
// every raw sample in that bucket down to its two most recent (timestamp,
// value) pairs. AggregatingMergeTree (not SimpleAggregateFunction, unlike
// renderDeltaPrefixTable's PartialSum column) because timeSeriesLastTwoSamples'
// combinator is a genuine two-state merge (re-selecting the top-2 by
// timestamp across states), not an idempotent max/sum — see
// renderLokiLabelCatalogTable's uniqState column for the same distinction.
//
// ORDER BY puts BucketEnd second, immediately after MetricName, for the SAME
// primary-key-pruning reason renderDeltaPrefixTable's own ORDER BY does (a
// query bounded to one metric + a bucket range prunes on the primary key
// instead of scanning every bucket of every series for that metric).
//
// The engine is ALWAYS AggregatingMergeTree / ReplicatedAggregatingMergeTree
// — never cfg.Engine's operator override — mirroring
// renderDeltaPrefixTable's own reasoning: an AggregateFunction column
// requires the Aggregating family specifically, a correctness requirement
// of this table, not a deployment preference.
func renderDownsampleTierTable(cfg Config) string {
	lcString := chsql.TypeLowCardinality(chsql.TypeRaw("String"))
	mapType := chsql.Call("Map", lcString, chsql.TypeRaw("String"))
	dt64ns := chsql.Call("DateTime64", chsql.InlineLit(int64(9)))
	engine := chsql.EngineAggregatingMergeTree()
	if cfg.DatabaseEngine.Replicated {
		engine = chsql.EngineReplicatedAggregatingMergeTree()
	}
	return chsql.CreateTable(schema.DownsampleTierTable).
		Database(cfg.Database).
		IfNotExists().
		Columns(
			chsql.ColumnDef{Name: metricNameColumn, Type: lcString},
			chsql.ColumnDef{Name: metricAttributesColumn, Type: mapType},
			chsql.ColumnDef{Name: metricResourceAttributesColumn, Type: mapType},
			chsql.ColumnDef{Name: metricServiceNameColumn, Type: lcString},
			chsql.ColumnDef{Name: schema.DownsampleTierBucketColumn, Type: dt64ns},
			chsql.ColumnDef{
				Name: schema.DownsampleTierSamplesColumn,
				Type: chsql.Call("AggregateFunction", chsql.BareIdent("timeSeriesLastTwoSamples"), dt64ns, chsql.TypeRaw("Float64")),
			},
			// SimpleAggregateFunction(any, Int32), not a plain Int32:
			// AggregatingMergeTree can hold multiple unmerged parts for the
			// same (series, bucket) key until a background merge runs (see
			// internal/chsql's emitRangeWindowDownsampleTier), and a plain
			// column has no merge semantics at all — a background MERGE
			// would otherwise pick an ARBITRARY row's value for a
			// non-aggregate column instead of the single well-defined
			// any() an OTel series' one-temporality-for-its-lifetime
			// invariant makes exact.
			chsql.ColumnDef{
				Name: schema.DownsampleTierTemporalityColumn,
				Type: chsql.Call("SimpleAggregateFunction", chsql.BareIdent("any"), chsql.TypeRaw("Int32")),
			},
		).
		Engine(engine).
		OrderBy(metricNameColumn, schema.DownsampleTierBucketColumn, metricAttributesColumn, metricResourceAttributesColumn, metricServiceNameColumn).
		TTL(chsql.TableTTLTiered(schema.DownsampleTierBucketColumn, cfg.TTL.Metrics, cfg.Tiering.Metrics, cfg.Tiering.Volume)).
		SQL()
}

// renderDownsampleTierView renders the CREATE MATERIALIZED VIEW feeding
// renderDownsampleTierTable from the Sum table (cerberus issue #2751): every
// raw sample, bucketed to its downsampleTierBucketEndExpr boundary, folded
// through timeSeriesLastTwoSamplesState. Scoped to the Sum table only
// (counters) — v1's deliberate scope decision, matching
// renderDeltaPrefixView's own single-source-table shape; a gauge metric's
// last_over_time() never routes to this tier (see
// internal/promql/lower_strategy.go's eligibility check).
//
// any(AggregationTemporality) also rides along, feeding
// schema.DownsampleTierTemporalityColumn — exact per bucket because a
// single OTel series carries ONE temporality for its lifetime (see that
// constant's own doc). Unlike renderDeltaPrefixView this carries NO
// AggregationTemporality WHERE filter: the tier folds every sample
// regardless of temporality, and irate()'s read branches on the carried
// column at query time instead (chsql.CounterOrDeltaPairDelta) — see
// chopt.FeatureDownsampleTier's own doc.
//
// Like every materialized view, this captures only rows inserted from the
// moment it is created onward; pre-existing history needs the separate
// `cerberus schema downsample-tier-backfill` one-time pass (see
// internal/downsampletier and docs/operations.md).
func renderDownsampleTierView(cfg Config) string {
	bucketEnd := downsampleTierBucketEndExpr(metricTimeColumn, schema.DownsampleTierBucket)
	body := chsql.NewQuery().
		Select(
			chsql.Col(metricNameColumn),
			chsql.Col(metricAttributesColumn),
			chsql.Col(metricResourceAttributesColumn),
			chsql.Col(metricServiceNameColumn),
			chsql.As(bucketEnd, schema.DownsampleTierBucketColumn),
			chsql.As(chsql.Call("timeSeriesLastTwoSamplesState", chsql.Col(metricTimeColumn), chsql.Col(metricValueColumn)), schema.DownsampleTierSamplesColumn),
			chsql.As(chsql.Call("any", chsql.Col(aggregationTemporalityColumn)), schema.DownsampleTierTemporalityColumn),
		).
		From(chsql.Qual(cfg.Database, cfg.Tables.MetricsSum)).
		GroupBy(
			chsql.Col(metricNameColumn),
			chsql.Col(metricAttributesColumn),
			chsql.Col(metricResourceAttributesColumn),
			chsql.Col(metricServiceNameColumn),
			bucketEnd,
		)
	stmt := chsql.CreateMaterializedView(schema.DownsampleTierTable+cerberusOwnedViewSuffix).
		Database(cfg.Database).
		IfNotExists().
		To(cfg.Database, schema.DownsampleTierTable).
		As(body)
	if cfg.Cluster != "" {
		stmt.OnCluster(cfg.Cluster)
	}
	return stmt.SQL()
}

// --- Loki label-cardinality catalog (cerberus issue #2770) ---
//
// The catalog table's own two column names (schema.LabelCatalogKeyColumn /
// schema.LabelCatalogCardinalityStateColumn) live in internal/schema, NOT
// here — see that package's doc comment for why: both this package (which
// creates the table) and internal/api/loki (which queries it) need the
// SAME literal, and schema is the one leaf package both already import.
//
// labelValueColumn names the second column the catalog view's ARRAY JOIN
// explodes ResourceAttributes into (see renderLokiLabelCatalogView) — it
// exists only inside this view's own body, so unlike the two schema.*
// columns above it has no read-side consumer and stays local.
//
// logsTimestampColumnName is the logs table's fixed OTel-CH exporter
// timestamp column — the same literal renderLogsTable's own ttlExpr calls
// pass inline; named here (rather than repeated inline) because this
// package's magic-constant convention (invariant 13) applies to a column
// name referenced from typed chsql builder code the same way it applies to
// a numeric literal.
const (
	labelValueColumn        = "LabelValue"
	logsTimestampColumnName = "Timestamp"

	// lokiLabelCatalogRefreshMinutes is the REFRESH EVERY interval for the
	// catalog view — cerberus issue #2770's proposed cadence: frequent
	// enough that a freshly-appearing label surfaces within one Grafana
	// logs-explorer session, infrequent enough that the refresh's own scan
	// cost (bounded by lokiLabelCatalogWindow below) stays a small fraction
	// of a 5-minute period even against a busy logs table.
	lokiLabelCatalogRefreshMinutes = 5

	// lokiLabelCatalogWindowHours bounds the catalog's aggregation window
	// to the most recent N hours (cerberus issue #2770's "keep the refresh
	// itself cheap" requirement) — a full-table scan on every refresh would
	// make the cadence above unaffordable on a retention-sized logs table.
	// 24h mirrors the issue's own proposed window: long enough that a
	// label used anywhere in a typical day's traffic pattern is caught,
	// short enough that the per-refresh scan is bounded by roughly one
	// day's ingest rather than the table's full retention.
	lokiLabelCatalogWindowHours = 24
)

// renderLokiLabelCatalogTable renders the CREATE TABLE for the
// label-cardinality catalog: one row per label key, carrying a uniqState
// sketch of the distinct values observed for that key in the catalog
// view's aggregation window. AggregatingMergeTree (not
// SimpleAggregateFunction, unlike the DELTA-prefix table's PartialSum
// column) because uniq's combinator is a genuine two-state merge, not an
// idempotent max/sum — see chsql's AggregateFunction usage here versus
// SimpleAggregateFunction in renderDeltaPrefixTable. ORDER BY LabelKey
// alone: the refreshable view's own atomic swap (not incremental
// MergeTree merges) is what keeps this table's content current, so the
// sort key only needs to make a per-key uniqMerge query
// (internal/api/loki's catalog read path) efficient, not express any
// retention/uniqueness contract.
func renderLokiLabelCatalogTable(cfg Config) string {
	engine := chsql.EngineAggregatingMergeTree()
	if cfg.DatabaseEngine.Replicated {
		engine = chsql.EngineReplicatedAggregatingMergeTree()
	}
	return chsql.CreateTable(schema.LabelCatalogTable).
		Database(cfg.Database).
		IfNotExists().
		Columns(
			chsql.ColumnDef{Name: schema.LabelCatalogKeyColumn, Type: chsql.TypeRaw("String")},
			chsql.ColumnDef{
				Name: schema.LabelCatalogCardinalityStateColumn,
				Type: chsql.Call("AggregateFunction", chsql.BareIdent("uniq"), chsql.TypeRaw("String")),
			},
		).
		Engine(engine).
		OrderBy(schema.LabelCatalogKeyColumn).
		SQL()
}

// renderLokiLabelCatalogView renders the REFRESH-scheduled CREATE
// MATERIALIZED VIEW feeding renderLokiLabelCatalogTable from the logs
// table: for every (key, value) pair in each row's ResourceAttributes map
// (mirroring buildDetectedLabelsSQL's own label-set shape — see
// internal/api/loki/detected_labels.go), a per-key uniqState of the
// observed values, over the trailing lokiLabelCatalogWindowHours. Empty
// values are excluded — an unset attribute reads as "" off the Map, the
// same convention summariseDetectedLabels applies Go-side on the fallback
// path — so this catalog and that path agree on what counts as "the label
// carries a value" even though their AGGREGATE numbers otherwise diverge
// by design (see FeatureLokiCatalogMV's doc comment).
//
// Unlike renderDeltaPrefixView this uses RefreshEveryMinutes rather than
// the on-insert trigger: the window bound (`now() - INTERVAL <N> HOUR`)
// must be re-evaluated at EACH refresh to stay "the trailing N hours", not
// frozen at the row's insert time the way an on-insert MV would freeze it.
func renderLokiLabelCatalogView(cfg Config) string {
	windowStart := chsql.Sub(chsql.Call("now"), chsql.Call("toIntervalHour", chsql.InlineLit(int64(lokiLabelCatalogWindowHours))))
	body := chsql.NewQuery().
		Select(
			chsql.Col(schema.LabelCatalogKeyColumn),
			chsql.As(chsql.Call("uniqState", chsql.Col(labelValueColumn)), schema.LabelCatalogCardinalityStateColumn),
		).
		From(chsql.Qual(cfg.Database, cfg.Tables.Logs)).
		ArrayJoin(
			chsql.As(chsql.Call("mapKeys", chsql.Col(metricResourceAttributesColumn)), schema.LabelCatalogKeyColumn),
			chsql.As(chsql.Call("mapValues", chsql.Col(metricResourceAttributesColumn)), labelValueColumn),
		).
		Where(chsql.Gte(chsql.Col(logsTimestampColumnName), windowStart)).
		Where(chsql.Neq(chsql.Col(labelValueColumn), chsql.InlineLit(""))).
		GroupBy(chsql.Col(schema.LabelCatalogKeyColumn))
	stmt := chsql.CreateMaterializedView(schema.LabelCatalogTable+schema.LabelCatalogViewSuffix).
		Database(cfg.Database).
		IfNotExists().
		RefreshEveryMinutes(lokiLabelCatalogRefreshMinutes).
		To(cfg.Database, schema.LabelCatalogTable).
		As(body)
	if cfg.Cluster != "" {
		stmt.OnCluster(cfg.Cluster)
	}
	return stmt.SQL()
}

// --- Tempo tag catalog (cerberus issue #2771) ---
//
// The Tempo sibling of the Loki label-cardinality catalog above: a
// refreshable MV maintaining, per (Scope, TagKey), a topK sketch of the
// most frequent values observed for that key in the aggregation window.
// The table's four columns live in internal/schema (TagCatalogTable and
// friends), not here — see that package's doc comment for why, mirroring
// LabelCatalogTable's own rationale.
//
// SCOPE COVERAGE: only the two flat-Map attribute scopes — resource
// (ResourceAttributes) and span (SpanAttributes) — feed the catalog.
// Event/Link attributes (Events.Attributes / Links.Attributes) are
// Array(Map(String, String)) — one map PER EVENT / PER LINK on a span
// row, not one map per row — so exploding them costs an extra
// arrayFlatten(arrayMap(...)) fan-out per row on top of the mapKeys/
// mapValues explosion this view already pays twice (see
// distinctNestedMapKeysFrag in internal/api/tempo/search_tags.go for the
// live-path shape this would have to mirror). That is not "cheap" the
// way issue #2771 asked the catalog to stay, and events/links are a
// materially rarer autocomplete target than resource/span attributes, so
// they are excluded — an honest scope-down, not a deferral: the live
// scan remains their permanent, unconditional path, the same posture the
// resource/span buckets keep for every OTHER Tempo request shape (see
// tagsCatalogEligible's doc in internal/api/tempo/tag_catalog.go).
// Instrumentation-scope attributes (ScopeAttributes) are excluded for a
// different reason: the upstream traces_table.sql template carries no
// such column at all (schema.Traces.ScopeAttributesColumn defaults to
// "" — see that field's doc comment), so a stock deployment has nothing
// to catalog there; a custom schema that populates it stays on the live
// path, consistent with how attributeTagScopes() already treats an empty
// ScopeAttributesColumn as "no bucket".
//
// VALUE SHAPE: topKState/topKMerge (a bounded top-N sketch, N =
// schema.TagCatalogTopValuesLimit) rather than uniqState (a cardinality-only
// sketch, what the Loki catalog stores). The two endpoints this catalog
// serves need actual VALUES back — /search/tags needs the key names,
// /search/tag/{name}/values needs a sample of the key's values — not a
// count, so a cardinality sketch alone would have no read-side consumer.
// topK is approximate (Space-Saving algorithm) and caps at N values per
// key: a key with more than N distinct values in the window returns only
// its N most frequent, not the exhaustive set the live scan would. This
// is the same kind of documented divergence FeatureLokiCatalogMV's own
// doc accepts for its cardinality numbers — exempted from exact parity
// with the live path, not merely undertested against it.
const (
	// tempoTagValueColumn names the second column the catalog view's
	// UNION ALL arms project — the exploded map value each arm's
	// ArrayJoin produces, before it feeds the outer topKState aggregate.
	// Local to this view's body (mirrors labelValueColumn's own scoping).
	tempoTagValueColumn = "TagValue"

	// tracesSpanAttributesColumn names the traces spans table's
	// span-level attribute Map column — fixed by the upstream OTel-CH
	// exporter template, like every other columnName constant in this
	// package. The view's OTHER two map/timestamp columns
	// (ResourceAttributes, Timestamp) reuse metricResourceAttributesColumn
	// / logsTimestampColumnName rather than duplicating the same literal
	// under a third name — the same cross-signal reuse
	// renderLokiLabelCatalogView and the spanNameColumn block's own doc
	// comment already establish for this package.
	tracesSpanAttributesColumn = "SpanAttributes"

	// tempoTagCatalogRefreshMinutes is the REFRESH EVERY interval for the
	// catalog view — the same 5-minute cadence lokiLabelCatalogRefreshMinutes
	// uses, for the same reason: frequent enough that a freshly-appearing
	// tag surfaces within one Grafana Explore session, infrequent enough
	// that the refresh's own scan cost stays a small fraction of the
	// period even against a busy traces table.
	tempoTagCatalogRefreshMinutes = 5

	// tempoTagCatalogWindowHours bounds the catalog's aggregation window to
	// the most recent N hours, keeping each refresh's scan cost bounded
	// the same way lokiLabelCatalogWindowHours does. 1 hour — narrower than
	// the Loki catalog's 24h — is deliberately chosen to match
	// internal/api/tempo.DefaultSearchLookback: the catalog answers exactly
	// the window the LIVE path already defaults a windowless discovery
	// request to (see BoundDiscoveryWindow), so a catalog hit and a
	// catalog-miss-fallback describe the same trailing hour rather than two
	// different windows.
	tempoTagCatalogWindowHours = 1
)

// tempoTagCatalogTopValuesType renders the `topK(N)` column-type
// descriptor `AggregateFunction(topK(N), String)` and the ALTER/CREATE
// column type share — built via the typed chsql.Call constructor (a
// parametric aggregate function name IS a function call in CH's type
// grammar), not a hand-formatted string. N is schema.TagCatalogTopValuesLimit
// — see that constant's doc for why it is the single source of truth
// shared with the read-side topKMerge in internal/api/tempo.
func tempoTagCatalogTopValuesType() chsql.Frag {
	return chsql.Call("topK", chsql.InlineLit(int64(schema.TagCatalogTopValuesLimit)))
}

// tempoTagCatalogTopValuesStateFrag renders `topKState(N)(<col>)` — the
// per-row aggregate state expression the catalog view's SELECT computes.
// ParamAgg is CH's parameterised-aggregate shape
// (`name(params...)(args...)`); Builder.ParamAgg takes plain
// func(*Builder) slices, so the typed chsql.Frag values are adapted via
// paramAggArgs.
func tempoTagCatalogTopValuesStateFrag(col chsql.Frag) chsql.Frag {
	return func(b *chsql.Builder) {
		b.ParamAgg("topKState", paramAggArgs(chsql.InlineLit(int64(schema.TagCatalogTopValuesLimit))), paramAggArgs(col))
	}
}

// paramAggArgs adapts a small fixed list of typed chsql.Frag values to
// the []func(*chsql.Builder) shape Builder.ParamAgg's params/args
// parameters take — Frag's underlying type is already func(*Builder), so
// this is a plain element-wise slice conversion, not a rendering step.
func paramAggArgs(frags ...chsql.Frag) []func(b *chsql.Builder) {
	out := make([]func(b *chsql.Builder), len(frags))
	for i, f := range frags {
		out[i] = f
	}
	return out
}

// renderTempoTagCatalogTable renders the CREATE TABLE for the tag
// catalog: one row per (Scope, TagKey), carrying a topKState sketch of
// the most frequent values observed for that key in the catalog view's
// aggregation window. AggregatingMergeTree for the same reason
// renderLokiLabelCatalogTable uses it: topK's combinator is a genuine
// multi-state merge, not an idempotent max/sum. ORDER BY (Scope, TagKey)
// mirrors renderLokiLabelCatalogTable's ORDER BY LabelKey: the refreshable
// view's atomic swap — not incremental MergeTree merges — keeps this
// table's content current, so the sort key only needs to make the
// per-(scope, key) read path (internal/api/tempo's catalog read) and the
// per-scope key listing efficient.
func renderTempoTagCatalogTable(cfg Config) string {
	engine := chsql.EngineAggregatingMergeTree()
	if cfg.DatabaseEngine.Replicated {
		engine = chsql.EngineReplicatedAggregatingMergeTree()
	}
	return chsql.CreateTable(schema.TagCatalogTable).
		Database(cfg.Database).
		IfNotExists().
		Columns(
			chsql.ColumnDef{Name: schema.TagCatalogScopeColumn, Type: chsql.TypeRaw("String")},
			chsql.ColumnDef{Name: schema.TagCatalogKeyColumn, Type: chsql.TypeRaw("String")},
			chsql.ColumnDef{
				Name: schema.TagCatalogTopValuesStateColumn,
				Type: chsql.Call("AggregateFunction", tempoTagCatalogTopValuesType(), chsql.TypeRaw("String")),
			},
		).
		Engine(engine).
		OrderBy(schema.TagCatalogScopeColumn, schema.TagCatalogKeyColumn).
		SQL()
}

// tempoTagCatalogScopeArm renders one UNION ALL arm of
// renderTempoTagCatalogView's source subquery: every (key, value) pair in
// mapCol across the trailing tempoTagCatalogWindowHours, tagged with the
// literal scope name. scope is baked in as a DDL-time literal
// (chsql.InlineLit), not a bound `?` parameter — the view body is stored
// DDL, not a per-request statement — mirroring how
// renderLokiLabelCatalogView's window bound is itself a literal
// expression re-evaluated at each refresh.
func tempoTagCatalogScopeArm(cfg Config, scope, mapCol string, windowStart chsql.Frag) *chsql.QueryBuilder {
	return chsql.NewQuery().
		Select(
			chsql.As(chsql.InlineLit(scope), schema.TagCatalogScopeColumn),
			chsql.As(chsql.Col("k"), schema.TagCatalogKeyColumn),
			chsql.As(chsql.Col("v"), tempoTagValueColumn),
		).
		From(chsql.Qual(cfg.Database, cfg.Tables.Traces)).
		ArrayJoin(
			chsql.As(chsql.Call("mapKeys", chsql.Col(mapCol)), "k"),
			chsql.As(chsql.Call("mapValues", chsql.Col(mapCol)), "v"),
		).
		Where(chsql.Gte(chsql.Col(logsTimestampColumnName), windowStart)).
		Where(chsql.Neq(chsql.Col("v"), chsql.InlineLit("")))
}

// renderTempoTagCatalogView renders the REFRESH-scheduled CREATE
// MATERIALIZED VIEW feeding renderTempoTagCatalogTable from the traces
// table: a UNION ALL of the resource-scope and span-scope arms (see
// tempoTagCatalogScopeArm), re-aggregated into one topKState per (Scope,
// TagKey). Like renderLokiLabelCatalogView this uses RefreshEveryMinutes
// rather than an on-insert trigger, because the window bound must be
// re-evaluated at EACH refresh to stay "the trailing N hours".
func renderTempoTagCatalogView(cfg Config) string {
	windowStart := chsql.Sub(chsql.Call("now"), chsql.Call("toIntervalHour", chsql.InlineLit(int64(tempoTagCatalogWindowHours))))
	resourceArm := tempoTagCatalogScopeArm(cfg, schema.TagCatalogScopeResource, metricResourceAttributesColumn, windowStart)
	spanArm := tempoTagCatalogScopeArm(cfg, schema.TagCatalogScopeSpan, tracesSpanAttributesColumn, windowStart)
	body := chsql.NewQuery().
		Select(
			chsql.Col(schema.TagCatalogScopeColumn),
			chsql.Col(schema.TagCatalogKeyColumn),
			chsql.As(tempoTagCatalogTopValuesStateFrag(chsql.Col(tempoTagValueColumn)), schema.TagCatalogTopValuesStateColumn),
		).
		From(chsql.Paren(chsql.UnionAll(resourceArm.Frag(), spanArm.Frag()))).
		GroupBy(chsql.Col(schema.TagCatalogScopeColumn), chsql.Col(schema.TagCatalogKeyColumn))
	stmt := chsql.CreateMaterializedView(schema.TagCatalogTable+schema.TagCatalogViewSuffix).
		Database(cfg.Database).
		IfNotExists().
		RefreshEveryMinutes(tempoTagCatalogRefreshMinutes).
		To(cfg.Database, schema.TagCatalogTable).
		As(body)
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
// from the schema). HasFullTextSearch threads cfg.TextIndexEnabled (cerberus
// issue #2773): false (the default) renders the bloom-filter/tokenbf_v1
// branch that works everywhere cerberus deploys; true renders the text-index
// branch, which needs ClickHouse >= 26.2 — see TextIndexEnabled's doc for the
// chopt full_text_index gate that resolves this bit.
func renderLogsTable(cfg Config) (string, error) {
	data := sqltemplates.CreateTableData{
		Database:          cfg.Database,
		TableName:         cfg.Tables.Logs,
		ClusterString:     cfg.clusterClause(),
		Engine:            cfg.Engine,
		TTL:               cfg.ttlExpr("Timestamp", cfg.TTL.Logs, cfg.Tiering.Logs),
		HasFullTextSearch: cfg.TextIndexEnabled,
	}
	var buf strings.Builder
	if err := sqltemplates.LogsCreateTableTmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("ddl: execute logs create-table template: %w", err)
	}
	return cfg.appendSettings(buf.String(), cfg.LogsSettings...), nil
}

// renderTracesTable formats the traces spans-table DDL. Upstream shape:
// `(database, table, cluster, engine, ttl)`. TTL field is
// `toDateTime(Timestamp)`. TracesSettings lands here (the spans table carries
// every Attributes-shaped Map column: ResourceAttributes, SpanAttributes,
// Events.Attributes, Links.Attributes) but NOT on renderTracesCreateTsTable —
// see that function's doc comment.
func renderTracesTable(cfg Config) string {
	return cfg.appendSettings(fmt.Sprintf(
		sqltemplates.TracesCreateTable,
		cfg.Database, cfg.Tables.Traces, cfg.clusterClause(),
		cfg.Engine,
		cfg.ttlExpr("Timestamp", cfg.TTL.Traces, cfg.Tiering.Traces),
	), cfg.TracesSettings...)
}

// renderTracesCreateTsTable formats the `<table>_trace_id_ts` lookup table
// DDL. Upstream shape mirrors the spans table (db, table, cluster, engine,
// ttl) — the `_trace_id_ts` suffix is hard-coded into the template, so the
// caller passes the base traces table name. TTL field is
// `toDateTime(Start)`.
//
// Deliberately does NOT apply cfg.TracesSettings: this lookup table's only
// columns are TraceId/Start/End (see the upstream
// traces_id_ts_lookup_table.sql template) — no Map column exists here for a
// map_bucketed_serialization-shaped setting to have anything to act on, so
// stamping it would be a pure no-op MergeTree setting on a table it cannot
// possibly affect.
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

// --- Column compression codecs (cerberus issue #2768) ---
//
// Codecs on the metrics/logs/traces fact tables are upstream fork-template
// defaults, not tuned to cerberus's own data. Every candidate below was
// MEASURED — not merely reasoned about — against real production-shaped
// sample data (test/perf/nightly/testdata/samples/*.parquet, cerberus issue
// #2411) via a real MergeTree engine (chDB), comparing whole-table
// compressed bytes before/after the codec swap; see the PR that introduced
// this section for the full transcript. Two candidates the issue proposed
// did NOT survive measurement and are deliberately NOT adopted here:
//
//   - DateTime64(9) scrape/sample timestamps (metrics TimeUnix, span
//     Timestamp): the issue's premise was that a near-constant scrape
//     interval leaves second-order regularity DoubleDelta captures and
//     Delta alone misses. Measured against three real metrics tables
//     (gauge/sum/histogram), DoubleDelta+ZSTD(1) was 43%-166% LARGER than
//     the current Delta+ZSTD(1) — a regression — because a near-constant
//     Delta stream is already maximally redundant (the same interval
//     repeated for most of a series' run), which ZSTD(1) alone already
//     exploits about as well as physically possible (232x-443x measured);
//     DoubleDelta's own per-value framing bytes break up that redundancy
//     more than they remove. GCD+Delta measured a small, INCONSISTENT
//     effect across the three real tables (-3.5% to -6.9% on sum/histogram,
//     but +9.2% on gauge) — not a safe blanket win across every metrics
//     table sharing one codec declaration. Bare GCD (no Delta) measured
//     catastrophically worse (+650% to +3515%). None of the three
//     candidates clears the bar, so TimeUnix / Timestamp keep their
//     current Delta, ZSTD(1) — unchanged by this issue.
//   - Value (gauge/sum) / Sum (histogram/summary/exp_histogram) Float64:
//     the issue gated Gorilla/FPC adoption on beating ZSTD(1) on real
//     gauge-shaped data. Measured against the same three tables, BOTH
//     regressed — Gorilla 27%-2182% larger, FPC 84%-2662% larger — so
//     neither is adopted; Value / Sum keep ZSTD(1), unchanged.
//
// Two candidates DID measure a real, safe win and are adopted below:
//
//   - Logs Body (String): ZSTD(3) measured ~1.3%-1.8% smaller than ZSTD(1)
//     on a representative leveled-log-line corpus, consistently across
//     repeated runs, at only the modest extra write-side CPU a higher ZSTD
//     level costs (ZSTD decode cost is level-independent, so read-side
//     query latency is unaffected).
//   - Span Duration (UInt64 nanoseconds): the issue's own rationale — a
//     ns-precision column whose real values carry coarser precision is
//     GCD's exact case — measured true ONLY for the codec's designed
//     shape, `GCD, ZSTD(1)` (no Delta stage): Duration values are
//     independent per-span measurements, not a running sequence, so
//     chaining a Delta stage ahead of ZSTD (`Delta, ZSTD(1)` or
//     `GCD, Delta, ZSTD(1)`, the issue's own proposed pairing) measured
//     3%-10% LARGER — against synthetic data spanning both a genuinely
//     nanosecond-precision scenario and a millisecond-rounded one (no real
//     trace sample data exists in this repo to benchmark against — traces
//     are outside the #2411 sample set's scope). Bare `GCD, ZSTD(1)`
//     measured a real 3.5% win on the coarse-precision scenario and was
//     statistically a no-op (+0.02%, measurement noise) on the
//     fine-precision one: GCD finds no common divisor when there genuinely
//     isn't one, so it costs nothing on a deployment whose real Duration
//     values don't carry the coarse-precision pattern this codec targets.
//
// All codecs used above — Delta, GCD, Gorilla, FPC (the last two named here
// only because the reasoning above needs to identify exactly what was
// rejected) — are supported at ClickHouse >= 22.9, well under this repo's
// 24.8 version floor (docs/toolchain.md), so — like ADD PROJECTION / ADD
// INDEX above, and unlike ADD STATISTICS's chopt-gated column_statistics
// feature below — this registry renders UNCONDITIONALLY: no Config flag, no
// chopt feature gate.
//
// A MODIFY COLUMN codec change is metadata-only for NEW parts; existing
// parts keep their prior codec until a background merge (or an
// operator-run `OPTIMIZE ... FINAL`) rewrites them — see
// docs/operations.md. Re-running the identical statement is a no-op:
// ClickHouse only schedules work when the declared codec actually differs
// from the column's current one, so applying this on every boot never
// re-triggers a conversion once converged — see
// chsql.ModifyColumnCodecBuilder's doc comment for why that (not any
// `IF EXISTS`-shaped guard) is what makes re-applying this safe.
const (
	// bodyColumn is the logs table's fixed OTel-CH exporter text column the
	// curated Body codec ALTER targets.
	bodyColumn = "Body"

	// zstdLevelLogsBody is the curated ZSTD level for the logs Body column —
	// see the package doc comment above for the measured win over the
	// upstream ZSTD(1) default.
	zstdLevelLogsBody = 3

	// zstdLevelDefault mirrors the upstream ZSTD(1) baseline level the span
	// Duration codec below keeps as its final compression stage — not
	// itself a curated change, just the existing level reused verbatim
	// alongside the new GCD stage.
	zstdLevelDefault = 1
)

// logsBodyCodec returns the curated logs Body codec: CODEC(ZSTD(3)).
func logsBodyCodec() chsql.Frag {
	return chsql.Codec(chsql.Call("ZSTD", chsql.InlineLit(zstdLevelLogsBody)))
}

// spanDurationCodec returns the curated span Duration codec:
// CODEC(GCD, ZSTD(1)) — deliberately no Delta stage; see the package doc
// comment above for why chaining Delta ahead of ZSTD measured worse on this
// column than GCD alone.
func spanDurationCodec() chsql.Frag {
	return chsql.Codec(chsql.BareIdent("GCD"), chsql.Call("ZSTD", chsql.InlineLit(zstdLevelDefault)))
}

// renderLogsCodecs returns the curated codec ALTER for the logs table's
// Body column (issue #2768).
func renderLogsCodecs(cfg Config) []string {
	return []string{renderModifyColumnCodec(cfg, cfg.Tables.Logs, bodyColumn, logsBodyCodec())}
}

// renderTracesCodecs returns the curated codec ALTER for the traces spans
// table's Duration column (issue #2768).
func renderTracesCodecs(cfg Config) []string {
	return []string{renderModifyColumnCodec(cfg, cfg.Tables.Traces, durationColumn, spanDurationCodec())}
}

// renderModifyColumnCodec builds one idempotent MODIFY COLUMN CODEC ALTER —
// see chsql.AlterTableModifyColumnCodec's doc comment for the exact
// idempotency contract (re-apply is a no-op once the codec has converged,
// not guard-based). ON CLUSTER is threaded so the ALTER replicates the same
// way the CREATE statements do.
func renderModifyColumnCodec(cfg Config, table, column string, codec chsql.Frag) string {
	stmt := chsql.AlterTableModifyColumnCodec(cfg.Database, table, column, codec)
	if cfg.Cluster != "" {
		stmt.OnCluster(cfg.Cluster)
	}
	return stmt.SQL()
}

// --- Column statistics (cerberus issue #2766) ---
//
// Zero statistics usage existed anywhere in production before this: PREWHERE
// conjunct ordering (internal/chsql/prewhere.go) is a hand-rolled static
// heuristic ("cheap and references no wide column"), and TraceQL structural
// joins pick a build side with no cardinality estimate at all. ClickHouse
// column statistics give the query planner real minmax/uniq/tdigest estimates
// to drive both — a substrate cerberus never asks the server for today. A
// live probe against ClickHouse 26.5 (via chDB) confirms `allow_statistics_
// optimize` DOES reorder an explicitly multi-condition PREWHERE clause by
// statistics-derived selectivity rather than only column byte-size (upstream
// RFC https://github.com/ClickHouse/ClickHouse/pull/53240), resolving the
// issue's own flagged uncertainty in this feature's favor.
//
// Column choice mirrors the issue's own curated list: the columns cerberus's
// OWN query patterns filter or join on most, not a blanket "every column"
// sweep that would only tax every insert and merge for no read-side benefit.
//
// STATISTICS-TYPE-PER-COLUMN-TYPE (verified against a live server, NOT
// assumed from the issue's text): `minmax` and `tdigest` are numeric-only —
// ClickHouse rejects ADD STATISTICS outright (code 708, ILLEGAL_STATISTICS)
// for either type on a String or LowCardinality(String) column. Only `uniq`
// applies to a string-typed column. So ServiceName/MetricName/SpanName/
// TraceId (all String or LowCardinality(String) in the upstream OTel-CH
// schema) carry `uniq` ONLY — which is also the semantically right stat for
// an equality-filtered identity column: `minmax` exists for RANGE predicates
// a string equality never issues anyway. AggregationTemporality (Int32) and
// SeverityNumber (UInt8) are numeric, so they carry `minmax, uniq`. Duration
// (UInt64) additionally carries `tdigest`, since Duration is filtered by
// RANGE (a latency threshold) far more than by equality, and tdigest is what
// lets the planner estimate a range predicate's selectivity rather than just
// its bounds.
const (
	statTypeMinMax  = "minmax"
	statTypeUniq    = "uniq"
	statTypeTDigest = "tdigest"

	// spanNameColumn / durationColumn / traceIdColumn / severityNumberColumn
	// name fixed OTel-CH exporter columns outside the metrics tables that the
	// statistics registry below targets — SpanName and Duration on the traces
	// spans table, TraceId on both the spans and logs tables, SeverityNumber
	// on the logs table. ServiceName is common to all three signals' base
	// OTel-CH tables, so metricServiceNameColumn's value ("ServiceName") is
	// reused directly for logs/traces rather than duplicated under a second
	// name.
	spanNameColumn       = "SpanName"
	durationColumn       = "Duration"
	traceIdColumn        = "TraceId"
	severityNumberColumn = "SeverityNumber"
)

// stringStatTypes is the TYPE list for every String / LowCardinality(String)
// statistics column below (ServiceName, MetricName, SpanName, TraceId) — see
// the package doc comment above for why `minmax`/`tdigest` are excluded
// (ClickHouse rejects them outright on a string-typed column).
var stringStatTypes = []string{statTypeUniq}

// numericMinMaxUniqStatTypes is the TYPE list for the numeric non-Duration
// statistics columns (AggregationTemporality, SeverityNumber).
var numericMinMaxUniqStatTypes = []string{statTypeMinMax, statTypeUniq}

// durationStatTypes additionally carries tdigest — see the package doc
// comment above for why Duration alone gets a third statistics type.
var durationStatTypes = []string{statTypeMinMax, statTypeUniq, statTypeTDigest}

// renderAddColumnStatistics builds one idempotent ADD STATISTICS ALTER
// installing every entry of types on every entry of columns of one table. ON
// CLUSTER is threaded so the ALTER replicates the same way the CREATE
// statements do. ADD STATISTICS IF NOT EXISTS is metadata-only and
// idempotent, so the same Apply path covers both freshly-created and
// pre-existing tables — mirroring renderAddMetricProjection /
// renderAddTemporalityIndex.
func renderAddColumnStatistics(cfg Config, table string, columns, types []string) string {
	stmt := chsql.AlterTableAddStatistics(cfg.Database, table, columns, types)
	if cfg.Cluster != "" {
		stmt.OnCluster(cfg.Cluster)
	}
	return stmt.SQL()
}

// renderMetricsColumnStatistics returns the curated ADD STATISTICS ALTERs for
// all five metrics fact tables: ServiceName + MetricName (`uniq` — both are
// String-family) everywhere, plus AggregationTemporality (`minmax, uniq` — an
// Int32 column, its own separate ALTER) on the SAME sum/histogram pair
// renderAddTemporalityIndex already indexes — the two tables a
// rate()/increase() range window can route natively onto (see that
// function's doc comment); gauge never carries the column at all, and
// exp_histogram carries it but sits outside every temporality-aware routing
// path, so statistics on it would only tax writes for a column no read path
// ever filters.
func renderMetricsColumnStatistics(cfg Config) []string {
	tables := []string{
		cfg.Tables.MetricsGauge,
		cfg.Tables.MetricsSum,
		cfg.Tables.MetricsHistogram,
		cfg.Tables.MetricsExpHistogram,
		cfg.Tables.MetricsSummary,
	}
	stmts := make([]string, 0, len(tables)+2)
	for _, table := range tables {
		stmts = append(stmts, renderAddColumnStatistics(cfg, table,
			[]string{metricServiceNameColumn, metricNameColumn}, stringStatTypes))
	}
	for _, table := range []string{cfg.Tables.MetricsSum, cfg.Tables.MetricsHistogram} {
		stmts = append(stmts, renderAddColumnStatistics(cfg, table,
			[]string{aggregationTemporalityColumn}, numericMinMaxUniqStatTypes))
	}
	return stmts
}

// renderLogsColumnStatistics returns the curated ADD STATISTICS ALTERs for
// the logs table: ServiceName + TraceId (`uniq` — both String-family; the
// highest-selectivity equality filter on almost every LogQL query, and the
// trace-to-logs correlation join key, respectively), and SeverityNumber
// (`minmax, uniq` — a UInt8 column) in its own separate ALTER.
func renderLogsColumnStatistics(cfg Config) []string {
	return []string{
		renderAddColumnStatistics(cfg, cfg.Tables.Logs,
			[]string{metricServiceNameColumn, traceIdColumn},
			stringStatTypes),
		renderAddColumnStatistics(cfg, cfg.Tables.Logs,
			[]string{severityNumberColumn},
			numericMinMaxUniqStatTypes),
	}
}

// renderTracesColumnStatistics returns the curated ADD STATISTICS ALTERs for
// the traces spans table: ServiceName + SpanName + TraceId in one ALTER
// (`uniq` — all three are String-family: ServiceName and SpanName are the
// structural query's own ORDER BY prefix, TraceId is the join key TraceQL
// structural queries and the logs/traces correlation both key on), and
// Duration in a second ALTER (`minmax, uniq, tdigest` — a UInt64 column; see
// the package doc comment for why it alone gets the extra tdigest type).
func renderTracesColumnStatistics(cfg Config) []string {
	return []string{
		renderAddColumnStatistics(cfg, cfg.Tables.Traces,
			[]string{metricServiceNameColumn, spanNameColumn, traceIdColumn},
			stringStatTypes),
		renderAddColumnStatistics(cfg, cfg.Tables.Traces,
			[]string{durationColumn},
			durationStatTypes),
	}
}

// isColumnStatisticsUnsupported reports whether err looks like ClickHouse
// refusing an ADD STATISTICS ALTER because column statistics are not
// supported on the connected server — most notably ClickHouse Cloud, which
// the upstream docs state does not support statistics at all
// (https://clickhouse.com/docs/sql-reference/statements/alter/statistics).
// cerberus could not pin ClickHouse's exact error code for that specific
// rejection (unlike, say, chclient.IsUnknownTable's typed code-60 check), so
// detection here is a narrow phrase match rather than a typed
// *clickhouse.Exception check: it requires "statist" together with a refusal
// word ("not supported" / "not implemented" / "disabled" / "cloud"), so a
// genuine connectivity or syntax error on an unrelated statement is never
// misclassified as this tolerable case.
func isColumnStatisticsUnsupported(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "statist") {
		return false
	}
	return strings.Contains(msg, "not supported") ||
		strings.Contains(msg, "not implemented") ||
		strings.Contains(msg, "disabled") ||
		strings.Contains(msg, "cloud")
}

// --- Column TTL (cerberus issue #2769) ---
//
// See ColumnTTL's doc comment for the design rationale (why Body /
// Events.Attributes / Links.Attributes, why a row's own retention TTL must
// stay strictly longer) and chsql.ModifyColumnTTLBuilder's doc comment for
// the empirically-verified ClickHouse behavior (type re-declaration
// required, indexed columns and Nested subcolumns both accept a TTL,
// ttl_only_drop_parts does not block it).
const (
	// eventsAttributesColumn / linksAttributesColumn name the traces spans
	// table's two Nested-subcolumn TTL targets, materialized by ClickHouse
	// as ordinary Array(...) columns addressable by their dotted name.
	eventsAttributesColumn = "Events.Attributes"
	linksAttributesColumn  = "Links.Attributes"

	// bodyColumnType / attributesArrayColumnType are the exact ClickHouse
	// types MODIFY COLUMN TTL must re-declare for each target column — see
	// chsql.ModifyColumnTTLBuilder's doc comment for why the type cannot be
	// omitted. Fixed by the upstream OTel-CH exporter template (Body) and
	// by ClickHouse's own Nested-to-Array materialization rule
	// (Events.Attributes / Links.Attributes both being
	// `Map(LowCardinality(String), String)` Nested members, per
	// sqltemplates' TracesCreateTable), not configurable.
	bodyColumnType            = "String"
	attributesArrayColumnType = "Array(Map(LowCardinality(String), String))"
)

// renderLogsBodyTTL builds the curated Body column TTL ALTER for the logs
// table, keyed on the same Timestamp column the table's own row-level TTL
// (TTL.Logs) uses.
func renderLogsBodyTTL(cfg Config) string {
	return renderModifyColumnTTL(cfg, cfg.Tables.Logs, bodyColumn, bodyColumnType, cfg.ColumnTTL.LogsBody)
}

// renderTracesEventsLinksTTL builds the curated Events.Attributes and
// Links.Attributes column TTL ALTERs for the traces spans table, both keyed
// on the same Timestamp column the table's own row-level TTL (TTL.Traces)
// uses and sharing the one TracesEventsLinks duration.
func renderTracesEventsLinksTTL(cfg Config) []string {
	return []string{
		renderModifyColumnTTL(cfg, cfg.Tables.Traces, eventsAttributesColumn, attributesArrayColumnType, cfg.ColumnTTL.TracesEventsLinks),
		renderModifyColumnTTL(cfg, cfg.Tables.Traces, linksAttributesColumn, attributesArrayColumnType, cfg.ColumnTTL.TracesEventsLinks),
	}
}

// renderModifyColumnTTL builds one idempotent MODIFY COLUMN TTL ALTER via
// chsql.AlterTableModifyColumnTTL — see that builder's doc comment for the
// exact idempotency contract (re-apply is a no-op once the TTL clause has
// converged, the same ClickHouse behavior renderModifyColumnCodec relies
// on) and for why colType cannot be omitted the way a codec-only MODIFY
// COLUMN omits it. ON CLUSTER is threaded so the ALTER replicates the same
// way the CREATE statements do. Both targets key on "Timestamp", matching
// the literal renderLogsTable / renderTracesTable already pass their own
// ttlExpr calls for the SAME tables' row-level TTL.
func renderModifyColumnTTL(cfg Config, table, column, colType string, ttl time.Duration) string {
	stmt := chsql.AlterTableModifyColumnTTL(cfg.Database, table, column, chsql.BareIdent(colType), "Timestamp", ttl)
	if cfg.Cluster != "" {
		stmt.OnCluster(cfg.Cluster)
	}
	return stmt.SQL()
}
