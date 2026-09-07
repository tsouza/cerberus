package tempo

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
// chsql.DistinctAttrKeys (shared with internal/api/loki's /labels) covers
// the one WHOLE-MAP discovery shape (search_tags.go's per-scope key
// enumeration) that per-key rendering never touched.
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
