package loki

import "github.com/tsouza/cerberus/internal/chsql"

// This file is cerberus issue #3063 point 2's fix: /loki/api/v1/labels,
// /series, /label/<name>/values, /detected_fields and /detected_labels all
// build their SQL directly against chsql.NewQuery() rather than through a
// chplan tree lowered by logql.Lang.Parse, so they never reach
// engine.emitForHead / chsql.Emit's ctx-based AttrStrategies threading —
// wiring Handler.AttrStrategies alone (cerberus issue #2777 / #3064) had
// no effect on any of them. Each build*SQL function in this package now
// takes the resolved chsql.AttrStrategies explicitly and threads it onto
// every chsql.QueryBuilder it constructs via .WithAttrStrategies — that
// alone makes every per-key JSON rendering chsql already has
// (exprMapAccess, exprMapContainsKey) reach the selector-matcher WHERE
// clause every one of these builders shares (applySelectorAndWindow), and
// the two helpers below cover the WHOLE-MAP shapes these builders read
// that per-key rendering never touched.
//
// /patterns (patterns.go) is not in this list: it only projects
// Timestamp/Body/SeverityText, never an attribute-map column, so it has
// no JSON-strategy exposure at all.

// attrMapFrag renders col as a genuine Map(String,String) Frag: the bare
// column reference when strategies resolves it to AttrStrategyMap (the
// default, and every pre-#3063 call site's byte-identical behaviour), or
// chsql.JSONAttrMapReconstruction(col) when it resolves to
// AttrStrategyJSON. Used wherever one of these builders reads or GROUPs
// BY a whole attribute map — series.go / detected_labels.go's
// canonicalLabelsFrag input (the stream label-set identity) and
// detected_fields.go's stream_labels / log_attributes projections (the
// per-line structured-metadata peek).
func attrMapFrag(strategies chsql.AttrStrategies, col string) chsql.Frag {
	if strategies.Lookup(col) == chsql.AttrStrategyJSON {
		return chsql.JSONAttrMapReconstruction(col)
	}
	return chsql.Col(col)
}

// distinctAttrKeysFrag renders the discovery-query idiom "every distinct
// key present in col across the matched rows" — /labels' whole shape. The
// Map-typed branch is the .keys-subcolumn shape labels.go's own former
// distinctMapKeysFrag used (folded into this function), unchanged
// (labels.go's own doc explains why arrayJoin(col.keys) is used instead
// of mapKeys(col)); the JSON-strategy branch uses JSONAllPaths(col)
// instead, which reports the SAME flat dot-joined leaf key strings the
// Map .keys subcolumn would for an equivalent flat key set — see
// chsql's jsonFullMapReconstruction doc for why JSONAllPaths already
// does this without needing chsql's full bounded-depth-flatten machinery
// (a KEY enumeration is exactly the shape ClickHouse's own JSONAllPaths
// natively reports; only per-key VALUE extraction needed that machinery).
func distinctAttrKeysFrag(strategies chsql.AttrStrategies, col string) chsql.Frag {
	if strategies.Lookup(col) == chsql.AttrStrategyJSON {
		return chsql.Distinct(chsql.Call("arrayJoin", chsql.Call("JSONAllPaths", chsql.Col(col))))
	}
	return chsql.Distinct(chsql.Call("arrayJoin", chsql.Qual(col, "keys")))
}
