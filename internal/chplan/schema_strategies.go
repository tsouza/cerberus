package chplan

import "github.com/tsouza/cerberus/internal/schema"

// SchemaStrategies is how the physical shape of the tables one statement
// reads changes the SQL that statement renders and the per-query settings it
// carries: the facts the boot requirements check (internal/preflight) derives
// from the deployed tables, rather than ones the configuration states.
//
// PrewhereByServer and CanonicalMapTypeCast are the fields the deployed
// server, not its tables, decides; they ride here because every emission
// path already carries this value.
//
// One value is scoped to the signal a statement reads — each signal's head
// reads its own (chsql.ResolvedSchemaStrategies) — so a field another
// signal's tables decide is left at its zero value. The zero value renders
// the SQL for the schema cerberus provisions by default: every attribute map
// a Map, every metrics timestamp DateTime64(9), every table ordered by the
// sorting key cerberus provisions it with.
//
// Declared here rather than in internal/chsql for the same reason as
// AttrStrategy: internal/logql's Lang reports it to the engine, and logql
// may not depend on chsql.
type SchemaStrategies struct {
	// Attrs is how the signal's attribute-map columns are stored (logs and
	// traces; metrics attribute maps are always Map).
	Attrs AttrStrategies

	// MetricsTimestamp is the precision of the metrics tables' timestamp
	// columns, which decides how a timestamp read over them renders.
	MetricsTimestamp schema.MetricsTimestampPrecision

	// SumTimestampTimezone is the timezone the metrics sum table's sample
	// timestamp column is declared with (schema.TimestampTimezoneOfType),
	// "" for a column that names none. The DELTA-prefix materialized view
	// buckets that column by toStartOfDay, which runs in the column's
	// timezone, so the DELTA-prefix read renders each calendar day it
	// splits at in the same one.
	SumTimestampTimezone string

	// SortingKeyPrefixes maps each of the signal's tables, by physical
	// name, to the leading run of bare columns of its deployed sorting key
	// (schema.SortingKeyPrefixOf). internal/engine reads it to decide
	// whether a GROUP BY is a prefix of the table's sort key
	// (optimize_aggregation_in_order). A table with no entry — the check
	// did not run, or the tables were not introspected — reads the key
	// cerberus provisions (schema.Metrics.SortingKeyPrefix and siblings); a
	// table whose key could not be read maps to an empty prefix, which no
	// GROUP BY matches.
	SortingKeyPrefixes map[string][]string

	// TableShapes maps each of the signal's tables, by physical name, to the
	// shape the emitter's conjunct ordering and PREWHERE promotion read off a
	// scan of it (internal/chsql's prewhere.go): its wide and discriminator
	// columns from the configured schema, and the sort columns of its
	// deployed sorting key (schema.SortingKeyColumnsOf), or of the key
	// cerberus provisions where the deployed one was not read. A table with
	// no entry reads the provisioned shape of the default-named table of
	// that name, or none.
	TableShapes map[string]TableShape

	// PrewhereByServer leaves the PREWHERE/WHERE split of a filtered scan to
	// the server's optimize_move_to_prewhere: the emitter writes every
	// conjunct into WHERE and no PREWHERE clause. It is not a fact of the
	// tables but of the server they run on — set for every signal when the
	// resolved optimizations include chopt.FeatureMoveToPrewhere
	// (chsql.SchemaStrategiesCell.SetPrewhereByServer). The zero value keeps
	// the emitter's own partition (internal/chsql's prewhere.go).
	PrewhereByServer bool

	// CanonicalMapTypeCast casts every canonical series key — the
	// key-ordered label Map a GROUP BY, ORDER BY, PARTITION BY or match key
	// binds to — back to the type ClickHouse parses from its operand's type
	// name (internal/chsql's CanonicalMap). It is set for every signal when
	// the resolved optimizations include chopt.FeatureCanonicalMapTypeCast
	// (chsql.SchemaStrategiesCell.SetCanonicalMapTypeCast). The zero value
	// renders the bare key-ordering call.
	CanonicalMapTypeCast bool
}
