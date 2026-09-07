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
// clause every one of these builders shares (applySelectorAndWindow);
// attrMapFrag below and chsql.DistinctAttrKeys (shared with
// internal/api/tempo's /api/search/tags) cover the WHOLE-MAP shapes these
// builders read that per-key rendering never touched.
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
