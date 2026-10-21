package chplan

import "github.com/tsouza/cerberus/internal/schema"

// SchemaStrategies is how the physical shape of the tables one statement
// reads changes the SQL that statement renders: the facts the boot
// requirements check (internal/preflight) derives from the deployed column
// types, rather than ones the configuration states.
//
// One value is scoped to the signal a statement reads — each signal's head
// reads its own (chsql.ResolvedSchemaStrategies) — so a field another
// signal's tables decide is left at its zero value. The zero value renders
// the SQL for the schema cerberus provisions by default: every attribute map
// a Map, every metrics timestamp DateTime64(9).
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
}
