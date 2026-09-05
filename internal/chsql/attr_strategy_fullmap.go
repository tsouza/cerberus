package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// This file is cerberus issue #3063's "full-map read" half of the
// AttrStrategyJSON work: chsql/attr_strategy.go's exprMapAccess and
// exprMapContainsKey (and exprFieldAccess, for TraceQL) already give a
// PER-KEY lookup against a JSON-typed attribute column the exact Map
// semantics the rest of the codebase was written against. That leaves
// every LogQL stage that reads the WHOLE map at once — mapConcat/mapFilter/
// mapApply/mapKeys/mapValues against a bare attribute-map ColumnRef, which
// internal/logql's detected_level synthesis (withDetectedLevelAndColumns),
// structured-metadata projection (structuredMetadataExpr) and parser-stage
// label merges (PipelineLabelsExpr) all build — still failing at query time
// with ILLEGAL_TYPE_OF_ARGUMENT against a JSON-typed column, exactly as
// before the per-key work landed. detected_level's mapConcat/mapFilter in
// particular runs on essentially EVERY LogQL log-stream query (it is only
// skipped when a pipeline explicitly drops the label), so this is not a
// niche gap.
//
// jsonFullMapReconstruction is the fix: given a bare JSON-strategy
// ColumnRef, it builds a chplan.Expr that evaluates to a genuine
// Map(String,String) with the SAME (key, value) pairs a Map-typed column
// holding the same logical attributes would — so mapConcat/mapFilter/
// mapApply/mapKeys/mapValues can run against the RECONSTRUCTED map exactly
// as they always have against a real Map column, with zero changes to any
// of internal/logql's map-shaped chplan lowering.
//
// Why this needs its own mechanism rather than reusing JSONAllPaths (the
// per-key existence check's building block, exprMapContainsKey): ClickHouse
// has no function that extracts a JSON value by a RUNTIME (per-row,
// non-constant) path against its native JSON type — verified empirically
// against chDB 26.5, the same substrate attr_strategy_json_chdb_test.go
// pins:
//
//	SELECT arrayMap(p -> JSONExtractString(<col>, p), JSONAllPaths(<col>))
//	-- Code: 43. DB::Exception: Function JSONExtractString with JSON type
//	-- input supports only constant string path arguments.
//
//	SELECT arrayMap(p -> getSubcolumn(<col>, p), JSONAllPaths(<col>))
//	-- Code: 44. DB::Exception: The second argument of function
//	-- getSubcolumn should be a constant string with the name of a subcolumn.
//
// This rules out the "JSONAllPaths(<col>) + per-path composition" mechanism
// cerberus issues #3063 and #3065 both proposed for this shape — the
// per-path COMPOSITION step is exactly what ClickHouse's JSON type cannot
// do dynamically. jsonFullMapReconstruction instead round-trips through
// toJSONString(<col>) (a String) and JSONExtractKeysAndValuesRaw, which IS
// fully dynamic — it needs no compile-time key at all, decomposing
// WHATEVER top-level members a document happens to carry into
// Array(Tuple(String, String)) (key, raw-JSON-value) pairs — and
// re-applies it, bounded by maxJSONAttrFlattenDepth, to any pair whose raw
// value is itself a JSON object, joining keys with '.' as it descends. That
// reconstructs the exact flat dotted-key shape JSONAllPaths(<col>) reports
// for the same document (verified: {"k8s.pod.name":"p1"} nests to
// `k8s`->`pod`->`name` by ClickHouse's JSON default, and
// jsonFlattenPairs('{"k8s":{"pod":{"name":"p1"}}}', 2) rebuilds the single
// pair ("k8s.pod.name", "p1")) — see attr_strategy_fullmap_json_chdb_test.go.
//
// See exprMapAccess's own doc for why this targets ClickHouse's DEFAULT
// dot-nesting behaviour only, not json_type_escape_dots_in_keys=1 — that
// hazard (cerberus issue #3063 point 3 / #3065 point 3) is unrelated to
// and unaffected by the depth bound here.
const jsonFullMapAttrCastType = "Map(String,String)"

// maxJSONAttrFlattenDepth bounds how many additional levels of ClickHouse's
// default JSON dot-nesting jsonFlattenPairs re-expands beyond the first, so
// a key with up to maxJSONAttrFlattenDepth+1 dot segments (e.g.
// "k8s.container.status.last_terminated_reason", 4 segments, needs 3
// levels beyond the first) reconstructs correctly. OTel semantic-convention
// attribute names are not observed to nest deeper than that in practice.
// A key nested past the bound keeps its remaining structure as an
// UNPARSED raw JSON substring under its partial dotted key in the
// reconstructed map, rather than being silently dropped — a real, bounded,
// documented limitation (see docs/operations.md's JSON-typed-attribute
// section), not a silent wrong answer, and exercised explicitly by
// TestJSONFullMapReconstruction_BeyondDepthBound_ChDB.
const maxJSONAttrFlattenDepth = 3

// jsonRawPairKeyIdx / jsonRawPairValueIdx are the 1-based tupleElement
// indices into the Array(Tuple(String,String)) JSONExtractKeysAndValuesRaw
// returns — (key, raw JSON value text) — matching ClickHouse's own
// left-to-right tuple order.
const (
	jsonRawPairKeyIdx   = 1
	jsonRawPairValueIdx = 2
)

// jsonObjectPrefix is the first byte of any ClickHouse-serialised JSON
// object, and the marker jsonFlattenPairs uses to tell an object-valued
// pair (needs another recursion level) from a scalar one (ready to decode
// via JSONExtractString) — mirroring toJSONString/JSONExtractKeysAndValuesRaw's
// own well-defined JSON object grammar, not an attribute-content heuristic.
const jsonObjectPrefix = "{"

// projectedExpr returns the expression emitProject should actually render
// for one SELECT projection: jsonFullMapReconstruction(col) when expr is a
// bare JSON-strategy ColumnRef, or expr unchanged otherwise.
//
// This covers the one full-map shape substituteJSONFullMapArgs cannot: a
// LogQL query whose pipeline neither runs a label-mutating stage nor
// synthesizes detected_level (both HasLabelMutatingStage and
// detectedLevelIdentityExpr false — an explicit `| drop detected_level`/
// `| keep` with no parser stage) projects the bare ResourceAttributes
// column as the query's ENTIRE "Attributes" output with no
// mapConcat/mapFilter wrapper at all (internal/logql/lang.go's
// Lang.ProjectSamples). Substituting here means the chclient cursor always
// scans a genuine Map(String,String) for that column, on every log-stream
// query shape, regardless of which pipeline stages ran.
func projectedExpr(b *Builder, expr chplan.Expr) chplan.Expr {
	if col, ok := jsonAttrColumn(b, expr); ok {
		return jsonFullMapReconstruction(col)
	}
	return expr
}

// jsonFullMapFns are the chplan Fn identifiers that read or build a WHOLE
// Map(String,String) rather than resolving one key (chplan.MapAccess /
// FnMapContainsKey, already covered by exprMapAccess / exprMapContainsKey).
// exprFunc substitutes any bare JSON-strategy ColumnRef argument to one of
// these with jsonFullMapReconstruction(col) before the normal name
// resolution / argument rendering runs — see substituteJSONFullMapArgs.
// FnMapSort is deliberately excluded: cerberus issue #2777's own doc notes
// metrics attribute maps never carry AttrStrategyJSON (preflight FATALs on
// anything but Map there), and FnMapSort (CanonicalMapFunc) is a
// metrics-only series-identity concern.
var jsonFullMapFns = map[chplan.Fn]bool{
	chplan.FnMapKeys:   true,
	chplan.FnMapValues: true,
	chplan.FnMapMerge:  true,
	chplan.FnMapFilter: true,
	chplan.FnMapApply:  true,
	chplan.FnMapUpdate: true,
}

// substituteJSONFullMapArgs returns f unchanged if none of its arguments is
// a bare JSON-strategy ColumnRef, or a shallow copy of f with each such
// argument replaced by jsonFullMapReconstruction(col) otherwise. Only a
// bare ColumnRef is substituted — jsonAttrColumn's own precondition — an
// already-composed Map expression (a nested mapConcat/mapUpdate/map(...)
// result) really is a ClickHouse Map at the SQL level regardless of the
// strategy of the column that seeded it, exactly as exprMapAccess's own
// doc explains for the per-key case.
func (b *Builder) substituteJSONFullMapArgs(f *chplan.FuncCall) *chplan.FuncCall {
	var out []chplan.Expr
	for i, arg := range f.Args {
		if col, ok := jsonAttrColumn(b, arg); ok {
			if out == nil {
				out = make([]chplan.Expr, len(f.Args))
				copy(out, f.Args)
			}
			out[i] = jsonFullMapReconstruction(col)
		}
	}
	if out == nil {
		return f
	}
	return &chplan.FuncCall{Fn: f.Fn, Args: out}
}

// JSONAttrMapReconstruction is jsonFullMapReconstruction's exported,
// Frag-returning twin for callers outside this package that build
// chsql.Frag trees directly rather than a chplan tree — cerberus issue
// #3063 point 2's ad-hoc metadata/discovery query builders
// (internal/api/loki's /series, /detected_labels, /detected_fields) never
// construct a chplan.FuncCall or chplan.Project, so neither
// substituteJSONFullMapArgs (exprFunc) nor projectedExpr (emitProject)
// ever runs for them; they must call this directly for a bare JSON-strategy
// attribute column they need to read or GROUP BY as a genuine
// Map(String,String).
func JSONAttrMapReconstruction(col string) Frag {
	expr := jsonFullMapReconstruction(&chplan.ColumnRef{Name: col})
	return func(b *Builder) { _ = b.Expr(expr) }
}

// jsonFullMapReconstruction returns the chplan.Expr a bare JSON-strategy
// attribute-map ColumnRef must be substituted with wherever a full-map
// operation needs a genuine Map(String,String) operand — see the file doc
// for why. col is referenced exactly once (inside toJSONString), so unlike
// jsonFlattenPairs's internal fan-out this does not duplicate the column
// reference itself.
func jsonFullMapReconstruction(col *chplan.ColumnRef) chplan.Expr {
	pairs := jsonFlattenPairs(
		&chplan.FuncCall{Fn: chplan.FnToJSONString, Args: []chplan.Expr{col}},
		maxJSONAttrFlattenDepth,
	)
	return &chplan.FuncCall{
		Fn:   chplan.FnCast,
		Args: []chplan.Expr{pairs, &chplan.LitString{V: jsonFullMapAttrCastType}},
	}
}

// jsonFlattenPairs returns a chplan.Expr evaluating to
// Array(Tuple(String,String)) — every (dotted key, decoded string value)
// pair jsonString's JSON object carries, recursing into object-valued
// members up to depth additional levels beyond the first.
//
// The lambda parameter name is suffixed by depth so nested recursive calls
// (one per level) never shadow an enclosing level's parameter — not a
// ClickHouse correctness requirement (inner lambda scopes shadow outer ones
// exactly like any block-scoped language), but it keeps the emitted SQL
// readable when a query fails and someone has to read it.
func jsonFlattenPairs(jsonString chplan.Expr, depth int) chplan.Expr {
	param := fmt.Sprintf("jsonAttrPair%d", depth)
	pair := &chplan.BareIdent{Name: param}
	keyOf := &chplan.FuncCall{Fn: chplan.FnTupleElement, Args: []chplan.Expr{pair, &chplan.LitInt{V: jsonRawPairKeyIdx}}}
	rawValueOf := &chplan.FuncCall{Fn: chplan.FnTupleElement, Args: []chplan.Expr{pair, &chplan.LitInt{V: jsonRawPairValueIdx}}}
	isObject := &chplan.FuncCall{
		Fn:   chplan.FnStartsWith,
		Args: []chplan.Expr{rawValueOf, &chplan.LitString{V: jsonObjectPrefix}},
	}
	rawPairs := &chplan.FuncCall{Fn: chplan.FnJSONExtractKeysAndValuesRaw, Args: []chplan.Expr{jsonString}}

	scalarPairs := &chplan.FuncCall{
		Fn: chplan.FnArrayMap,
		Args: []chplan.Expr{
			&chplan.Lambda{Params: []string{param}, Body: &chplan.FuncCall{
				Fn: chplan.FnTuple,
				Args: []chplan.Expr{
					keyOf,
					&chplan.FuncCall{Fn: chplan.FnJSONExtractString, Args: []chplan.Expr{rawValueOf}},
				},
			}},
			&chplan.FuncCall{
				Fn: chplan.FnArrayFilter,
				Args: []chplan.Expr{
					&chplan.Lambda{Params: []string{param}, Body: &chplan.FuncCall{Fn: chplan.FnNot, Args: []chplan.Expr{isObject}}},
					rawPairs,
				},
			},
		},
	}

	if depth <= 0 {
		// Base case: an object-valued member this deep keeps its raw JSON
		// text as the value under its own (partial) key, rather than being
		// dropped — see maxJSONAttrFlattenDepth's doc.
		leftover := &chplan.FuncCall{
			Fn: chplan.FnArrayMap,
			Args: []chplan.Expr{
				&chplan.Lambda{Params: []string{param}, Body: &chplan.FuncCall{Fn: chplan.FnTuple, Args: []chplan.Expr{keyOf, rawValueOf}}},
				&chplan.FuncCall{
					Fn: chplan.FnArrayFilter,
					Args: []chplan.Expr{
						&chplan.Lambda{Params: []string{param}, Body: isObject},
						rawPairs,
					},
				},
			},
		}
		return &chplan.FuncCall{Fn: chplan.FnArrayConcat, Args: []chplan.Expr{scalarPairs, leftover}}
	}

	objectPairs := &chplan.FuncCall{
		Fn: chplan.FnArrayFilter,
		Args: []chplan.Expr{
			&chplan.Lambda{Params: []string{param}, Body: isObject},
			rawPairs,
		},
	}

	nestedParam := fmt.Sprintf("jsonAttrNested%d", depth)
	nested := &chplan.BareIdent{Name: nestedParam}
	prefixed := &chplan.FuncCall{
		Fn: chplan.FnArrayMap,
		Args: []chplan.Expr{
			&chplan.Lambda{Params: []string{nestedParam}, Body: &chplan.FuncCall{
				Fn: chplan.FnTuple,
				Args: []chplan.Expr{
					&chplan.FuncCall{
						Fn: chplan.FnConcat,
						Args: []chplan.Expr{
							keyOf,
							&chplan.LitString{V: "."},
							&chplan.FuncCall{Fn: chplan.FnTupleElement, Args: []chplan.Expr{nested, &chplan.LitInt{V: jsonRawPairKeyIdx}}},
						},
					},
					&chplan.FuncCall{Fn: chplan.FnTupleElement, Args: []chplan.Expr{nested, &chplan.LitInt{V: jsonRawPairValueIdx}}},
				},
			}},
			jsonFlattenPairs(rawValueOf, depth-1),
		},
	}
	perObject := &chplan.FuncCall{
		Fn: chplan.FnArrayMap,
		Args: []chplan.Expr{
			&chplan.Lambda{Params: []string{param}, Body: prefixed},
			objectPairs,
		},
	}
	flattenedNested := &chplan.FuncCall{Fn: chplan.FnArrayFlatten, Args: []chplan.Expr{perObject}}
	return &chplan.FuncCall{Fn: chplan.FnArrayConcat, Args: []chplan.Expr{scalarPairs, flattenedNested}}
}
