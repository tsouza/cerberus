package chplan

// AggregatePreservesMatrixGrid reports whether a is an Aggregate that
// leaves the matrix grid axis of its input intact: every output row still
// belongs to exactly one (series, anchor) cell, and the anchor is still
// readable downstream under [RangeWindowAnchorColumn].
//
// A consumer that walks a plan root looking for the matrix RangeWindow
// underneath asks two things of every node on the way down — "does this
// node still expose the per-row anchor column" and "does it still emit one
// row per series". An Aggregate can answer yes to both or no to both, and
// which one it is, is decided entirely by its group key:
//
//   - The key names [RangeWindowAnchorColumn] as an output alias. The grid
//     axis survives the grouping AND stays referenceable by that name, so a
//     wrapper can still project it into the timestamp slot. An Aggregate
//     that keys on the anchor without re-exposing the alias has collapsed
//     the column out of its output scope, which is the same as not having
//     it.
//   - The key names the Attributes column as an output alias. The label set
//     is the series identity, so keying on the whole column — rather than
//     on expressions derived from it — is what keeps distinct series in
//     distinct groups.
//
// Both together mean each group refines one (series, anchor) cell, so the
// grouping can only merge rows that already shared that cell.
//
// A PromQL aggregation (`sum by (job) (rate(m[5m]))`) fails both halves and
// must: it keys on `Attributes['job']` under a synthetic `gkey_N` alias and
// on the schema timestamp under `bucket_ts`, deliberately folding many
// series onto one and re-publishing the step axis under its own name. That
// shape reports on the grid through its own wrapper, so a walk that crossed
// it would read an anchor column its output scope does not carry.
//
// The predicate is derived from the node rather than declared on it because
// the group key IS the property: an Aggregate that keys this way preserves
// the cell whatever built it, and one that does not cannot be made to by
// setting a field.
func AggregatePreservesMatrixGrid(a *Aggregate, cols SampleColumns) bool {
	return aggregateKeysOnAlias(a, RangeWindowAnchorColumn) &&
		aggregateKeysOnAlias(a, cols.Attributes)
}

// aggregateKeysOnAlias reports whether a groups on a key it re-exposes
// under the output name col, i.e. whether col is both part of the group
// identity and referenceable in a's output scope.
//
// The alias has to line up with a GroupBy entry positionally —
// GroupByAliases is the parallel naming of GroupBy — so an alias list that
// has run out of entries names nothing. An empty col names no column at
// all, so nothing exposes it; without that guard a schema override that
// mapped the Attributes column away would make every Aggregate answer yes.
func aggregateKeysOnAlias(a *Aggregate, col string) bool {
	if col == "" {
		return false
	}
	for i, alias := range a.GroupByAliases {
		if alias == col && i < len(a.GroupBy) {
			return true
		}
	}
	return false
}
