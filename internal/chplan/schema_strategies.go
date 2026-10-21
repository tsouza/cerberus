package chplan

import "github.com/tsouza/cerberus/internal/schema"

// SchemaStrategies is how the physical shape of the tables one statement
// reads changes the SQL that statement renders and the per-query settings it
// carries: the facts the boot requirements check (internal/preflight) derives
// from the deployed tables, rather than ones the configuration states.
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
}
