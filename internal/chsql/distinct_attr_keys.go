package chsql

// DistinctAttrKeys renders the discovery-query idiom "every distinct key
// present in the attribute-map column col across the matched rows" — the
// SELECT-list shape /loki/api/v1/labels and /api/search/tags' per-scope
// key enumeration share — resolved against strategies (cerberus issues
// #3063 / #3065; nil means AttrStrategyMap everywhere).
//
// Map-typed column: "DISTINCT arrayJoin(`<col>`.`keys`)", spelled against
// the column's virtual `.keys` subcolumn rather than mapKeys(<col>) so the
// key-only decode is explicit rather than depending on the server-side
// optimize_functions_to_subcolumns rewrite (cerberus issue #2775). Safe
// unconditionally, with no version gate: key enumeration never consults a
// skip index.
//
// JSON-strategy column: "DISTINCT arrayJoin(JSONAllPaths(`<col>`))", which
// reports the SAME flat dot-joined leaf key strings the Map .keys subcolumn
// would for an equivalent flat key set — see jsonFullMapReconstruction's
// doc (attr_strategy_fullmap.go) for why JSONAllPaths already does this
// without the bounded-depth-flatten machinery: a KEY enumeration is exactly
// the shape ClickHouse's own JSONAllPaths natively reports; only per-key
// VALUE extraction needed that machinery.
//
// DISTINCT is part of the SELECT list (CH's flavour), not a separate
// keyword, so it folds into the Frag for the QueryBuilder slot.
func DistinctAttrKeys(strategies AttrStrategies, col string) Frag {
	if !AttrKeysFromMap(strategies, col) {
		return Distinct(Call("arrayJoin", Call("JSONAllPaths", Col(col))))
	}
	return Distinct(Call("arrayJoin", Qual(col, "keys")))
}

// AttrKeysFromMap reports whether DistinctAttrKeys enumerates col's keys from
// a Map column's `.keys` subcolumn rather than from JSON paths.
func AttrKeysFromMap(strategies AttrStrategies, col string) bool {
	return strategies.Lookup(col) != AttrStrategyJSON
}
