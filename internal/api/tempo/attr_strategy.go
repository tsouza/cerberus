package tempo

import "github.com/tsouza/cerberus/internal/chsql"

// This file is cerberus issue #3065 point 2's fix: /api/search/tags,
// /api/v2/search/tags, /api/search/tag/{name}/values and
// /api/v2/search/tag/{name}/values (search_tags.go / search_tag_values.go)
// all build their SQL directly against chsql.NewQuery() rather than
// through a chplan tree lowered by traceqlLang.Parse, so they never reach
// engine.emitForHead / chsql.Emit's ctx-based AttrStrategies threading at
// all — wiring Handler.AttrStrategies alone (cerberus issue #3062) had no
// effect on any of them, mirroring internal/api/loki's identical gap
// (cerberus issue #3063 point 2, internal/api/loki/attr_strategy.go).
//
// Every build*SQL function in search_tags.go / search_tag_values.go now
// takes the resolved chsql.AttrStrategies explicitly and threads it onto
// every chsql.QueryBuilder it constructs via .WithAttrStrategies. That
// alone makes chsql.Builder.MapAt / Builder.MapContains (the ad-hoc
// equivalents of chplan.MapAccess / FnMapContainsKey — see MapAt's own
// doc in internal/chsql/builder.go) render correctly against a
// JSON-strategy column wherever mapAtFrag / mapContainsFrag
// (search_tag_values.go) already delegate to them, and
// distinctAttrKeysFrag below covers the one WHOLE-MAP discovery shape
// (search_tags.go's per-scope key enumeration) that per-key rendering
// never touched.
//
// The Nested Events.Attributes / Links.Attributes families
// (attrMapScopeEvent / attrMapScopeLink, distinctNestedMapKeysFrag /
// buildNestedAttributeValuesSQL) are NOT in scope here: cerberus issue
// #2777's JSON-strategy work only ever resolves AttrStrategyJSON for a
// flat Map(String, String) attribute column
// (SpanAttributes/ResourceAttributes/ScopeAttributes), never for a Nested
// column's per-element Array(Map(...)) subfield — there is no JSON
// encoding decision to make for those at all. Threading .WithAttrStrategies
// onto their QueryBuilders below is still done, for the same reason
// internal/api/loki threads it onto every builder regardless of whether a
// given call site touches an attribute-map column: a byte-identical
// no-op today, and one less place a future column addition could forget.

// distinctAttrKeysFrag renders the discovery-query idiom "every distinct
// key present in col across the matched rows" — /api/search/tags' per-scope
// key-enumeration shape. The Map-typed branch is the .keys-subcolumn shape
// distinctMapKeysFrag always used (kept byte-identical); the JSON-strategy
// branch uses JSONAllPaths(col) instead, which reports the SAME flat
// dot-joined leaf key strings the Map .keys subcolumn would for an
// equivalent flat key set — see chsql's jsonFullMapReconstruction doc
// (internal/chsql/attr_strategy_fullmap.go) for why JSONAllPaths already
// does this without needing chsql's full bounded-depth-flatten machinery
// (a KEY enumeration is exactly the shape ClickHouse's own JSONAllPaths
// natively reports; only per-key VALUE extraction needed that machinery).
// Mirrors internal/api/loki/attr_strategy.go's distinctAttrKeysFrag
// exactly.
func distinctAttrKeysFrag(strategies chsql.AttrStrategies, col string) chsql.Frag {
	if strategies.Lookup(col) == chsql.AttrStrategyJSON {
		return chsql.Distinct(chsql.Call("arrayJoin", chsql.Call("JSONAllPaths", chsql.Col(col))))
	}
	return distinctMapKeysFrag(col)
}
