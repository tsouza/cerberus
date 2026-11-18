package chopt

// PrewhereReorderedSince is the first ClickHouse release line that reorders
// the conditions it moves into a multi-step PREWHERE
// (allow_reorder_prewhere_conditions, on by default). An older build runs the
// steps in the order of the WHERE conjuncts and reads each column at the
// first step that needs it, for the rows the steps ahead of it keep.
var PrewhereReorderedSince = Version{Major: 24, Minor: 10}

// PrewhereKeepsWhereOrder reports whether server runs a multi-step PREWHERE
// in the order of the WHERE conjuncts. Like AnalyzerMandatory it is a
// property of the build, independent of any selection.
func PrewhereKeepsWhereOrder(server Version) bool {
	return !server.AtLeast(PrewhereReorderedSince)
}
