package chopt

// PrewhereReorderedSince is the first ClickHouse release line that reorders
// the conditions it moves into a multi-step PREWHERE
// (allow_reorder_prewhere_conditions, on by default). An older build runs the
// steps in the order of the WHERE conjuncts and reads each column at the
// first step that needs it, for the rows the steps ahead of it keep.
// system.settings_changes records the setting's introduction at 24.10, and
// 24.8 has no such setting; a MergeTree scan whose WHERE writes a filter on
// a wide column ahead of a selective one on a narrow column reads the wide
// column for every row on 24.8 and for the narrow filter's rows on 24.10,
// 25.3 and 26.8, and for every row again on 25.3 and 26.8 under
// allow_reorder_prewhere_conditions=0. The order concerns the conditions the
// server moves to PREWHERE itself; an explicit PREWHERE the emitter writes
// keeps a conjunct on a wide column out of it (chsql's partitionPrewhere),
// so that conjunct runs after the PREWHERE on every build.
var PrewhereReorderedSince = Version{Major: 24, Minor: 10}

// PrewhereKeepsWhereOrder reports whether server runs a multi-step PREWHERE
// in the order of the WHERE conjuncts. Like AnalyzerMandatory it is a
// property of the build, independent of any selection.
func PrewhereKeepsWhereOrder(server Version) bool {
	return !server.AtLeast(PrewhereReorderedSince)
}
