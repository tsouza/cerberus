package tempo

import (
	"errors"
	"net/http"
	"time"

	"github.com/tsouza/cerberus/internal/telemetry"
	traceql "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// SearchTagValuesResponse is the body of
// /api/search/tag/<name>/values. Tempo returns every distinct value
// observed for one attribute key.
type SearchTagValuesResponse struct {
	TagValues []string `json:"tagValues"`
}

// SearchTagValuesResponseV2 is the body of
// /api/v2/search/tag/<name>/values. V2 wraps each value in an object so
// the type info can be threaded through; cerberus reports "string" for
// dynamic attributes (CH Map(String, String)) and the matching CH
// column type for intrinsics.
type SearchTagValuesResponseV2 struct {
	TagValues []TagValueV2 `json:"tagValues"`
}

// TagValueV2 is one entry in the V2 response. Type echoes Tempo's
// vocabulary ("string", "int", "float", "duration", "status", "kind").
type TagValueV2 struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// intrinsicColumn maps a Tempo intrinsic name (as it appears in the
// URL path of /api/search/tag/<name>/values) to the schema.Traces field
// holding the underlying CH column name. Returns ("", false) for an
// unknown intrinsic — caller then falls through to the dynamic-attribute
// branch.
func intrinsicColumn(name string, s schema.Traces) (string, bool) {
	switch name {
	case "name":
		return s.SpanNameColumn, true
	case "kind":
		return s.SpanKindColumn, true
	case "status":
		return s.StatusCodeColumn, true
	case "statusMessage":
		return s.StatusMessageColumn, true
	case "duration":
		return s.DurationColumn, true
	case "parent":
		return s.ParentSpanIDColumn, true
	}
	return "", false
}

// handleSearchTagValues implements
// `GET /api/search/tag/{name}/values`. The tag name lives in the URL
// path; if it matches an intrinsic the handler queries the dedicated
// column, otherwise it unions SpanAttributes[name] and
// ResourceAttributes[name] (a key can live in either map).
//
// The `q` narrowing parameter is a V2 parameter and this route ignores
// it, as upstream does — a V1 request answers the whole window's value
// set whatever `q` says, including a malformed one (see the route
// contract in search_tags_filter.go).
func (h *Handler) handleSearchTagValues(w http.ResponseWriter, r *http.Request) {
	h.respondTagValues(w, r, TagValuesRouteV1)
}

// handleSearchTagValuesV2 implements
// `GET /api/v2/search/tag/{name}/values`. Same data as V1, wrapped per
// Tempo V2's typed envelope.
//
// This is the value-side route that takes the optional `q` parameter: it
// narrows the answer to the values the requested key takes on the spans a
// TraceQL query selects, which is what backs Grafana's value-completion
// dropdown. It contributes one more conjunct to the lookup's WHERE;
// absent, the SQL is unchanged (see search_tags_filter.go).
func (h *Handler) handleSearchTagValuesV2(w http.ResponseWriter, r *http.Request) {
	h.respondTagValues(w, r, TagValuesRouteV2)
}

// respondTagValues is the shared core of V1 + V2.
//
// The {name} segment of the URL accepts the TraceQL identifier grammar
// plus, for backward compatibility with Grafana clients that splice
// dotted attribute keys directly into the path, a bare opaque-key
// fallback:
//
//   - intrinsics: `name`, `kind`, `status`, `statusMessage`, `duration`,
//     `parent` — query the dedicated CH column.
//   - scoped attribute: `resource.x` → ResourceAttributes only,
//     `span.x` → SpanAttributes only.
//   - auto-scope leading-dot attribute: `.x`, `.x.y` → both maps.
//   - bare dotted attribute (e.g. `service.name`) — Tempo's V2 parser
//     rejects it, but our V1 endpoint historically accepts it and the
//     V2 endpoint stays permissive too; we treat it as auto-scope
//     against the bare key. The compatibility harness picks scoped
//     forms for fixtures specifically so both backends parse them
//     identically; this fallback exists for direct cerberus callers.
//
// resolveTagName runs the parse once; callers downstream switch on
// whether it landed on an intrinsic column or a map lookup, and which
// scope the map lookup targets.
func (h *Handler) respondTagValues(w http.ResponseWriter, r *http.Request, route TagsRoute) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "", "", errors.New("missing tag name"))
		return
	}
	start, end, err := parseTempoStartEnd(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "", "", err)
		return
	}
	// windowless is captured BEFORE BoundDiscoveryWindow defaults start/end
	// below — the tag-catalog eligibility signal (cerberus issue #2771);
	// see tagValuesCatalogEligible's doc comment (in tag_catalog.go,
	// alongside tagsCatalogEligible's fuller writeup) for why.
	windowless := start.IsZero() && end.IsZero()
	// Bound a windowless tag-value lookup to the recent window so the
	// per-key scan part-prunes otel_traces instead of full-scanning the
	// fact table (same map-explosion failure as /search/tags).
	start, end = BoundDiscoveryWindow(start, end)

	ctx, cancel, ok := h.applyQueryTimeout(w, r)
	if !ok {
		return
	}
	defer cancel()

	// The optional `?q=` narrowing filter — the same one the tag-NAME
	// routes take, resolved through the same tagQueryFilter so a `q`
	// selects the same spans wherever it is sent. It is read after the
	// window so a request that gets both wrong reports the window first,
	// matching upstream's parameter order.
	filter, err := h.tagQueryFilter(ctx, route, r.URL.Query().Get("q"), start, end)
	if err != nil {
		writeError(w, tagsErrStatus(err), "", "", err)
		return
	}

	resolved, _ := resolveTagName(name, h.Schema)

	// Catalog-eligible fast path (cerberus issue #2771): see
	// tagValuesCatalogEligible's doc comment for the exact rule. A miss
	// falls straight through to the SAME live per-key lookup every other
	// request already takes — that path is untouched below.
	var values []string
	valueTyp := "string"
	fromCatalog := false
	if h.TagCatalogEnabled && tagValuesCatalogEligible(resolved, filter, windowless) {
		values, fromCatalog = h.tagValuesFromCatalog(ctx, resolved)
	}
	if !fromCatalog {
		var (
			sqlStr string
			args   []any
		)
		if resolved.IsIntrinsic {
			sqlStr, args = buildIntrinsicValuesSQL(h.Schema, resolved.IntrinsicCol, filter, start, end)
			valueTyp = intrinsicType(resolved.IntrinsicName)
		} else {
			sqlStr, args = buildAttributeValuesSQL(h.Schema, resolved.Key, resolved.MapScope, filter, start, end)
		}
		h.Logger.Debug("cerberus tempo /search/tag/values",
			"tag", name,
			"intrinsic", resolved.IsIntrinsic,
			"map_scope", resolved.MapScope,
			"key", resolved.Key,
			"sql", sqlStr,
			"args", telemetry.SanitizeArgsForLog(args))

		values, err = h.Client.QueryStrings(ctx, sqlStr, args...)
		if err != nil {
			h.Logger.Error("cerberus tempo /search/tag/values CH query failed", "err", err, "tag", name)
			writeError(w, tagsErrStatus(err), "", "", err)
			return
		}
		values = sortedUnique(values)
	}

	if route == TagValuesRouteV2 {
		out := make([]TagValueV2, 0, len(values))
		for _, v := range values {
			out = append(out, TagValueV2{Type: valueTyp, Value: v})
		}
		writeJSON(w, http.StatusOK, SearchTagValuesResponseV2{TagValues: out})
		return
	}
	writeJSON(w, http.StatusOK, SearchTagValuesResponse{TagValues: values})
}

// buildIntrinsicValuesSQL builds the SELECT for an intrinsic-column
// values lookup:
//
//	SELECT DISTINCT toString(`<col>`) AS `value`
//	FROM `otel_traces`
//	WHERE `Timestamp` >= ? AND `Timestamp` <= ?
//
// toString is the CH conversion that handles both string-typed columns
// (no-op) and numeric / enum columns (Duration, StatusCode) uniformly,
// so the chclient.QueryStrings binder is happy regardless of the
// underlying type.
//
// `filter` is the optional `?q=` span-row predicate (see
// search_tags_filter.go); a nil filter appends no clause, so a request
// without `q` renders exactly the SQL it always did.
func buildIntrinsicValuesSQL(s schema.Traces, col string, filter chsql.Frag, start, end time.Time) (string, []any) {
	sb := chsql.NewQuery().
		Select(distinctToStringFrag(col)).
		From(chsql.Col(s.SpansTable))
	if !start.IsZero() {
		sb.Where(tempoTimeGteFrag(s.TimestampColumn, start))
	}
	if !end.IsZero() {
		sb.Where(tempoTimeLteFrag(s.TimestampColumn, end))
	}
	if filter != nil {
		sb.Where(filter)
	}
	return sb.Build()
}

// buildAttributeValuesSQL builds the SELECT for a dynamic-attribute
// values lookup. The tag key may live in SpanAttributes (`span.x`),
// ResourceAttributes (`resource.x`), or either of the two when the
// caller used the auto-scope leading-dot form (`.x`).
//
// For the both-maps form the CH idiom unions both maps via arrayJoin
// on a two-element array, then filters empties:
//
//	SELECT DISTINCT v AS value FROM (
//	    SELECT arrayJoin([`SpanAttributes`[?], `ResourceAttributes`[?]]) AS v
//	    FROM `otel_traces`
//	    WHERE (mapContains(`SpanAttributes`, ?) OR mapContains(`ResourceAttributes`, ?))
//	          [AND time bounds]
//	)
//	WHERE v != ''
//
// The mapContains pre-filter prunes most rows before the arrayJoin
// fan-out, and the outer v != ” drops the empty-string slot for rows
// where the key only exists in one of the two maps.
//
// For the single-scope forms we collapse the arrayJoin/union and emit
// a direct DISTINCT projection against the matching column:
//
//	SELECT DISTINCT v AS value FROM (
//	    SELECT `<col>`[?] AS v FROM `otel_traces`
//	    WHERE mapContains(`<col>`, ?) [AND time bounds]
//	)
//	WHERE v != ''
//
// `filter` is the optional `?q=` span-row predicate (see
// search_tags_filter.go). It joins the INNER query's conjuncts, not the
// outer one: it is a predicate over the span row, and the outer SELECT
// sees only the exploded value column `v`. A nil filter appends no
// clause, so a request without `q` renders exactly the SQL it always did.
//
// For the single-scope forms, a materialized attribute column (cerberus
// issue #2776) short-circuits both the projection and the pre-filter to a
// direct DISTINCT read of the narrow column instead of the map subscript —
// see buildMaterializedAttributeValuesSQL.
//
// For the auto-scope (both-maps) form, a materialized key in EITHER map
// (cerberus issue #2870) routes to buildAutoScopeUnionAttributeValuesSQL,
// which reads whichever side has a materialized column directly and falls
// back to the map subscript for the other side — see that function's doc
// for the four routing cases. A key materialized in neither map keeps
// today's single arrayJoin-over-both-maps shape, unchanged, below.
//
// attrMapScopeEvent / attrMapScopeLink (cerberus issue #2850) route
// straight to buildNestedAttributeValuesSQL before any of the above: the
// Nested Events.Attributes / Links.Attributes families are
// Array(Map(String, String)), not a flat Map, so neither the materialized-
// column short-circuit (#2776/#2870 only ever provision materialized
// columns for SpanAttributes/ResourceAttributes keys) nor the scalar
// mapAtFrag/mapContainsFrag shape below applies to them.
func buildAttributeValuesSQL(s schema.Traces, name string, scope attrMapScope, filter chsql.Frag, start, end time.Time) (string, []any) {
	switch scope {
	case attrMapScopeEvent:
		return buildNestedAttributeValuesSQL(s, s.EventsColumn, name, filter, start, end)
	case attrMapScopeLink:
		return buildNestedAttributeValuesSQL(s, s.LinksColumn, name, filter, start, end)
	}
	switch scope {
	case attrMapScopeResource:
		if col, ok := s.MaterializedResourceAttributeColumns[name]; ok {
			return buildMaterializedAttributeValuesSQL(s, col, materializedColumnNumeric(name), filter, start, end)
		}
	case attrMapScopeSpan:
		if col, ok := s.MaterializedSpanAttributeColumns[name]; ok {
			return buildMaterializedAttributeValuesSQL(s, col, materializedColumnNumeric(name), filter, start, end)
		}
	case attrMapScopeAny:
		spanCol, spanMaterialized := s.MaterializedSpanAttributeColumns[name]
		resCol, resMaterialized := s.MaterializedResourceAttributeColumns[name]
		if spanMaterialized || resMaterialized {
			return buildAutoScopeUnionAttributeValuesSQL(
				s, name,
				spanCol, spanMaterialized,
				resCol, resMaterialized,
				filter, start, end,
			)
		}
	}
	var (
		selFrag   chsql.Frag
		whereFrag chsql.Frag
	)
	switch scope {
	case attrMapScopeResource:
		selFrag = chsql.As(mapAtFrag(s.ResourceAttributesColumn, name), "v")
		whereFrag = mapContainsFrag(s.ResourceAttributesColumn, name)
	case attrMapScopeSpan:
		selFrag = chsql.As(mapAtFrag(s.AttributesColumn, name), "v")
		whereFrag = mapContainsFrag(s.AttributesColumn, name)
	default: // attrMapScopeAny
		selFrag = attrValueArrayJoinFrag(s.AttributesColumn, s.ResourceAttributesColumn, name)
		whereFrag = mapContainsAnyFrag(s.AttributesColumn, s.ResourceAttributesColumn, name)
	}
	inner := chsql.NewQuery().
		Select(selFrag).
		From(chsql.Col(s.SpansTable)).
		Where(whereFrag)
	if !start.IsZero() {
		inner.Where(tempoTimeGteFrag(s.TimestampColumn, start))
	}
	if !end.IsZero() {
		inner.Where(tempoTimeLteFrag(s.TimestampColumn, end))
	}
	if filter != nil {
		inner.Where(filter)
	}

	outer := chsql.NewQuery().
		Select(chsql.Distinct(chsql.Col("v"))).
		From(inner.Frag()).
		Where(nonEmptyFrag("v"))
	return outer.Build()
}

// buildNestedAttributeValuesSQL is buildAttributeValuesSQL's single-scope
// branch one nesting level up (cerberus issue #2850): nestedCol names a
// Nested event/link attribute family (Events / Links), whose Attributes
// sub-column is Array(Map(String, String)) rather than a flat Map, so the
// per-event/per-link key and value arrays are flattened
// (nestedMapKeysFlatFrag / nestedMapValuesFlatFrag — same helpers
// distinctNestedMapKeysFrag in search_tags.go builds on) and ARRAY JOINed
// together to zip them into (k, v) pairs, filtered to the requested key:
//
//	SELECT DISTINCT v FROM (
//	    SELECT v FROM `otel_traces`
//	    ARRAY JOIN
//	      arrayFlatten(arrayMap(m -> mapKeys(m), `<nestedCol>`.`Attributes`)) AS k,
//	      arrayFlatten(arrayMap(m -> mapValues(m), `<nestedCol>`.`Attributes`)) AS v
//	    WHERE k = ? [AND time bounds] [AND filter]
//	)
//	WHERE v != ''
//
// No materialized-column short-circuit applies here (see
// buildAttributeValuesSQL's doc) and there is no "both nested maps at once"
// auto-scope form — an event./link. prefix is always a single-scope
// request (see resolveTagName) — so unlike its flat-Map sibling this
// builder has no attrMapScopeAny-shaped union branch to consider.
func buildNestedAttributeValuesSQL(s schema.Traces, nestedCol, name string, filter chsql.Frag, start, end time.Time) (string, []any) {
	inner := chsql.NewQuery().
		Select(chsql.Col("v")).
		From(chsql.Col(s.SpansTable)).
		ArrayJoin(
			chsql.As(nestedMapKeysFlatFrag(nestedCol), "k"),
			chsql.As(nestedMapValuesFlatFrag(nestedCol), "v"),
		).
		Where(chsql.Eq(chsql.Col("k"), chsql.Lit(name)))
	if !start.IsZero() {
		inner.Where(tempoTimeGteFrag(s.TimestampColumn, start))
	}
	if !end.IsZero() {
		inner.Where(tempoTimeLteFrag(s.TimestampColumn, end))
	}
	if filter != nil {
		inner.Where(filter)
	}

	outer := chsql.NewQuery().
		Select(chsql.Distinct(chsql.Col("v"))).
		From(inner.Frag()).
		Where(nonEmptyFrag("v"))
	return outer.Build()
}

// materializedColumnNumeric reports whether the materialized column for a
// span/resource attribute key is a real ClickHouse numeric type
// (schema.MaterializedColumnKindNumeric, e.g. http.status_code's
// Nullable(Int32) — cerberus issue #2869) rather than the default
// LowCardinality(String). The tag-values SQL builders below consult this
// to project/filter a numeric materialized column correctly: `!= ”`
// against a numeric column is a type error, and the V1/V2 response shapes
// both want a STRING value regardless of the column's real CH type (see
// distinctToStringFrag's doc and intrinsicType's "string" default).
func materializedColumnNumeric(key string) bool {
	return schema.MaterializedAttributeColumnKindFor(key) == schema.MaterializedColumnKindNumeric
}

// materializedColumnPresenceFrag emits the row-level pre-filter for a
// materialized column: `<col> != ”` for the default String-typed column
// (the empty string is the map's own absent-key value, carried through by
// the column's DEFAULT), or `<col> IS NOT NULL` for a numeric column
// (cerberus issue #2869) — toInt32OrNull's absent/non-numeric sentinel is
// a real NULL, not an empty string, and comparing a numeric column to the
// empty string literal is a ClickHouse type error rather than a false
// filter.
func materializedColumnPresenceFrag(col string, numeric bool) chsql.Frag {
	if numeric {
		return chsql.IsNotNull(chsql.Col(col))
	}
	return nonEmptyFrag(col)
}

// buildMaterializedAttributeValuesSQL builds the SELECT for a single-scope
// dynamic-attribute values lookup whose key is materialized (cerberus
// issue #2776):
//
//	SELECT DISTINCT toString(`<col>`) AS value
//	FROM `otel_traces`
//	WHERE `<col>` != '' [AND time bounds] [AND filter]
//
// col is either a plain LowCardinality(String) top-level column —
// value-identical to the map subscript it was provisioned from (see
// schema.Traces.MaterializedSpanAttributeColumns' doc) — or, for a
// numeric key like http.status_code, a Nullable(Int32) one (cerberus
// issue #2869); toString handles both uniformly (the same idiom
// buildIntrinsicValuesSQL already relies on for Duration/StatusCode), so
// the projection never needs to branch on the column's CH type, only its
// presence filter does (see materializedColumnPresenceFrag). Unlike
// buildAttributeValuesSQL's map-backed shape, this needs no arrayJoin
// fan-out, no mapContains pre-filter, and no inner/outer query split: a
// direct DISTINCT read is both the correct and the cheapest shape.
func buildMaterializedAttributeValuesSQL(s schema.Traces, col string, numeric bool, filter chsql.Frag, start, end time.Time) (string, []any) {
	sb := chsql.NewQuery().
		Select(distinctToStringFrag(col)).
		From(chsql.Col(s.SpansTable)).
		Where(materializedColumnPresenceFrag(col, numeric))
	if !start.IsZero() {
		sb.Where(tempoTimeGteFrag(s.TimestampColumn, start))
	}
	if !end.IsZero() {
		sb.Where(tempoTimeLteFrag(s.TimestampColumn, end))
	}
	if filter != nil {
		sb.Where(filter)
	}
	return sb.Build()
}

// buildAutoScopeUnionAttributeValuesSQL builds the SELECT for an
// auto-scope (`.x` leading-dot, or the bare-key V1 fallback) dynamic-
// attribute values lookup whose key is materialized in the span map, the
// resource map, or both (cerberus issue #2870).
//
// Each side reads via its OWN narrow materialized column when one exists
// for that side, or falls back to the map subscript otherwise — exactly
// the same per-side shape buildAttributeValuesSQL already uses for the
// single-scope (`span.x` / `resource.x`) forms, just built once per side
// here instead of chosen once for the whole query:
//
//	SELECT DISTINCT v FROM (
//	    (SELECT `<spanCol>`   AS v FROM `otel_traces` WHERE `<spanCol>` != ''            [AND time bounds] [AND filter])
//	    UNION ALL
//	    (SELECT `SpanAttributes`[?] AS v FROM `otel_traces` WHERE mapContains(`SpanAttributes`, ?) [AND time bounds] [AND filter])
//	    -- (whichever of the two SpanAttributes lines applies)
//	    UNION ALL
//	    -- the matching ResourceAttributes line, materialized or map-backed
//	) WHERE v != ''
//
// UNION ALL (not the arrayJoin-over-array-literal shape the map-only path
// uses) is the composition that reuses the existing per-side Frag
// builders as-is: attrValueArmFrag emits one complete arm — projection,
// its own row-level pre-filter, time bounds, the optional `?q=` filter —
// with no change needed to either mapAtFrag/mapContainsFrag (the map arm)
// or the plain-column read (the materialized arm). Mixing a
// LowCardinality(String) column and a Map subscript inside ONE arrayJoin
// array literal would need both slots coerced to a common element type
// up front; two independently-typed SELECT arms merged by UNION ALL let
// ClickHouse resolve that per-column, the same way the map-only path's
// own two-map arrayJoin already tolerates any per-key value-type drift
// between the two source maps.
//
// A materialized arm always projects `toString(<col>)`, never the bare
// column (see attrValueArmFrag) — load-bearing, not cosmetic, for a
// numeric materialized key like http.status_code (cerberus issue #2869):
// UNION ALL requires its arms to share one column type, and ClickHouse has
// no common supertype between Int32 and the map arm's String projection,
// so a bare Nullable(Int32) arm alongside a String arm fails the query
// with NO_COMMON_TYPE. Casting every materialized arm to String up front
// sidesteps the question for any key, numeric or not, rather than
// special-casing the one combination http.status_code happens to hit
// today. Verified against chDB in
// search_tag_values_numeric_materialized_chdb_test.go.
//
// The outer DISTINCT + `v != ”` wrapper is identical to every other
// branch of buildAttributeValuesSQL: a materialized arm's own presence
// filter (materializedColumnPresenceFrag) and a map arm's `mapContains`
// filter both admit only non-empty values already, so the outer filter is
// redundant on any single arm — but it is exactly what dedupes the two
// arms' results against EACH OTHER (a key present with the same value in
// both maps contributes one row here, not two), which no single arm can
// do alone.
func buildAutoScopeUnionAttributeValuesSQL(
	s schema.Traces, name string,
	spanCol string, spanMaterialized bool,
	resCol string, resMaterialized bool,
	filter chsql.Frag, start, end time.Time,
) (string, []any) {
	numeric := materializedColumnNumeric(name)
	spanArm := attrValueArmFrag(s, s.AttributesColumn, spanCol, spanMaterialized, numeric, name, filter, start, end)
	resArm := attrValueArmFrag(s, s.ResourceAttributesColumn, resCol, resMaterialized, numeric, name, filter, start, end)

	outer := chsql.NewQuery().
		Select(chsql.Distinct(chsql.Col("v"))).
		From(chsql.Paren(chsql.UnionAll(spanArm, resArm))).
		Where(nonEmptyFrag("v"))
	return outer.Build()
}

// attrValueArmFrag builds one UNION ALL arm of
// buildAutoScopeUnionAttributeValuesSQL: a complete parenthesised SELECT
// projecting the requested key's value (aliased `v`) from one attribute
// map/column, with that key's own row-level pre-filter plus the shared
// time-bounds and `?q=` conjuncts.
//
// materialized selects between the two per-arm shapes: true reads
// materializedCol, always cast through toString (mirrors
// buildMaterializedAttributeValuesSQL's projection, and see
// buildAutoScopeUnionAttributeValuesSQL's doc for why the cast is
// load-bearing) and gated by materializedColumnPresenceFrag rather than a
// bare `!= ”` when numeric is true (cerberus issue #2869); false reads
// the map subscript mapCol[name] gated by mapContains(mapCol, name)
// (mirrors buildAttributeValuesSQL's single-scope map-backed branch,
// already String). materializedCol and numeric are both ignored when
// materialized is false.
func attrValueArmFrag(s schema.Traces, mapCol, materializedCol string, materialized, numeric bool, name string, filter chsql.Frag, start, end time.Time) chsql.Frag {
	arm := chsql.NewQuery().From(chsql.Col(s.SpansTable))
	if materialized {
		arm.Select(chsql.As(chsql.Call("toString", chsql.Col(materializedCol)), "v")).
			Where(materializedColumnPresenceFrag(materializedCol, numeric))
	} else {
		arm.Select(chsql.As(mapAtFrag(mapCol, name), "v")).
			Where(mapContainsFrag(mapCol, name))
	}
	if !start.IsZero() {
		arm.Where(tempoTimeGteFrag(s.TimestampColumn, start))
	}
	if !end.IsZero() {
		arm.Where(tempoTimeLteFrag(s.TimestampColumn, end))
	}
	if filter != nil {
		arm.Where(filter)
	}
	return arm.Frag()
}

// attrMapScope expresses which attribute map(s) a tag-values lookup
// should consult. Driven by parsing the URL tag-name as a TraceQL
// identifier — see resolveTagName.
type attrMapScope int

const (
	// attrMapScopeAny unions both SpanAttributes and ResourceAttributes
	// (Tempo's auto-scope form: bare `service.name`, leading-dot
	// `.service.name`). Never event/link — those require an explicit
	// event./link. prefix, so a bare/dotted identifier can only ever mean
	// "resource or span" (see resolveTagName).
	attrMapScopeAny attrMapScope = iota
	// attrMapScopeResource consults only ResourceAttributes
	// (Tempo's `resource.x` scoped form).
	attrMapScopeResource
	// attrMapScopeSpan consults only SpanAttributes
	// (Tempo's `span.x` scoped form).
	attrMapScopeSpan
	// attrMapScopeEvent consults only the Nested Events.Attributes family
	// (Tempo's `event.x` scoped form) — cerberus issue #2850. Routed
	// through buildNestedAttributeValuesSQL rather than the flat-Map
	// mapAtFrag/mapContainsFrag shape attrMapScopeResource/Span use.
	attrMapScopeEvent
	// attrMapScopeLink is attrMapScopeEvent's sibling for the Nested
	// Links.Attributes family (Tempo's `link.x` scoped form).
	attrMapScopeLink
)

func (s attrMapScope) String() string {
	switch s {
	case attrMapScopeResource:
		return "resource"
	case attrMapScopeSpan:
		return "span"
	case attrMapScopeEvent:
		return "event"
	case attrMapScopeLink:
		return "link"
	default:
		return "any"
	}
}

// resolvedTagName carries the outcome of running the URL path segment
// through traceql.ParseIdentifier. Either it resolves to an intrinsic
// column (IsIntrinsic + IntrinsicCol / IntrinsicName) or to a dynamic
// attribute lookup against the right CH map column (Key + MapScope).
type resolvedTagName struct {
	IsIntrinsic   bool
	IntrinsicCol  string
	IntrinsicName string
	Key           string
	MapScope      attrMapScope
}

// resolveTagName parses the URL `name` segment as a TraceQL attribute
// identifier and maps it onto the cerberus tag-values pipeline.
//
// Accepted forms (all per Tempo's grammar):
//   - intrinsics: `name`, `kind`, `status`, `statusMessage`, `duration`,
//     `parent` — route to the matching dedicated CH column.
//   - scoped attribute: `resource.x`, `span.x` — route to that map only.
//   - auto-scope attribute: `.x`, `.x.y` — both maps.
//   - bare dotted attribute (V1 only): `service.name` — parser
//     rejects it; we treat it as auto-scope with the bare key. This
//     preserves V1 backward compatibility with Grafana clients that
//     splice attribute keys directly into the path without a scope
//     prefix.
//
// Returns the resolved layout plus the parser error, if any. Callers
// that want to enforce TraceQL strictness (V2) should reject when
// parseErr != nil.
func resolveTagName(name string, s schema.Traces) (resolvedTagName, error) {
	// Cheap intrinsic short-circuit: if `name` is a bare intrinsic
	// keyword we recognise we don't need to invoke the parser at all.
	if col, ok := intrinsicColumn(name, s); ok {
		return resolvedTagName{IsIntrinsic: true, IntrinsicCol: col, IntrinsicName: name}, nil
	}

	attr, err := traceql.ParseIdentifier(name)
	if err != nil {
		// Backward-compat V1 fallback: parser rejects a bare dotted
		// key like `service.name`, but the V1 endpoint historically
		// accepts it. Treat as auto-scope against the bare key.
		return resolvedTagName{Key: name, MapScope: attrMapScopeAny}, err
	}
	// Intrinsic resolved via the parser (covers scoped intrinsics like
	// `span:name`, `trace:duration` once the schema grows them; today
	// the intrinsicColumn lookup is keyed by the bare name).
	if attr.Intrinsic != traceql.IntrinsicNone {
		if col, ok := intrinsicColumn(attr.Intrinsic.String(), s); ok {
			return resolvedTagName{
				IsIntrinsic:   true,
				IntrinsicCol:  col,
				IntrinsicName: attr.Intrinsic.String(),
			}, nil
		}
		// Parser recognised an intrinsic we don't model yet — fall
		// through to the attribute path with the bare name so the
		// response is empty rather than 5xx.
		return resolvedTagName{Key: attr.Name, MapScope: attrMapScopeAny}, nil
	}
	var ms attrMapScope
	switch attr.Scope {
	case traceql.AttributeScopeResource:
		ms = attrMapScopeResource
	case traceql.AttributeScopeSpan:
		ms = attrMapScopeSpan
	case traceql.AttributeScopeEvent:
		// cerberus issue #2850: previously fell to attrMapScopeAny, which
		// silently searched SpanAttributes/ResourceAttributes only — an
		// explicit `event.x` tag-values request could never find a value
		// that only ever lives in Events.Attributes, and returned an
		// empty (not an error) result forever.
		ms = attrMapScopeEvent
	case traceql.AttributeScopeLink:
		ms = attrMapScopeLink // same fix, for Links.Attributes.
	default:
		ms = attrMapScopeAny
	}
	return resolvedTagName{Key: attr.Name, MapScope: ms}, nil
}

// distinctToStringFrag emits "DISTINCT toString(`<col>`)". `toString`
// flows through the typed Call constructor; DISTINCT is composed via
// the typed Distinct Frag so the whole expression is a single
// projection-slot Frag for the QueryBuilder.
func distinctToStringFrag(col string) chsql.Frag {
	return chsql.Distinct(chsql.Call("toString", chsql.Col(col)))
}

// attrValueArrayJoinFrag emits the per-row fan-out:
//
//	arrayJoin([`<attrCol>`[?], `<resCol>`[?]]) AS `v`
//
// `arrayJoin` flows through the typed Call constructor; the CH array
// literal flows through the typed chsql.Array constructor; the AS-alias
// suffix uses the typed chsql.As constructor.
func attrValueArrayJoinFrag(attrCol, resCol, key string) chsql.Frag {
	return chsql.As(
		chsql.Call("arrayJoin", chsql.Array(
			mapAtFrag(attrCol, key),
			mapAtFrag(resCol, key),
		)),
		"v",
	)
}

// mapContainsAnyFrag emits "(mapContains(`<attrCol>`, ?) OR
// mapContains(`<resCol>`, ?))" — the row-level pre-filter that prunes
// spans not carrying the requested attribute key in either map. The
// outer parens + OR composition use the typed Paren/Or constructors;
// each mapContains call flows through the typed chsql.Call constructor.
func mapContainsAnyFrag(attrCol, resCol, key string) chsql.Frag {
	return chsql.Paren(chsql.Or(
		mapContainsFrag(attrCol, key),
		mapContainsFrag(resCol, key),
	))
}

// mapContainsFrag emits "mapContains(`<col>`, ?)" with key bound as a
// positional argument. Composes through the typed Call constructor;
// column / key operands flow through Col / Lit.
//
// Deliberately NOT spelled as the explicit has(<col>.keys, ?) subcolumn
// form: cerberus issue #2775 verified (chDB 26.5.1.1, EXPLAIN QUERY TREE)
// that ClickHouse's analyzer already rewrites mapContains(<col>, ?) into
// exactly that has(<col>.keys, ?) shape internally, AND — unlike a
// hand-written has(<col>.keys, ?) — the analyzer-rewritten form is the
// ONLY one confirmed (via EXPLAIN indexes=1) to still match a
// conventionally-declared `INDEX ... mapKeys(<col>) TYPE bloom_filter`
// skip index; a directly-authored has(<col>.keys, ?) showed NO Skip
// section at all against that same index. So mapContains(<col>, ?) is
// strictly no worse (same key-only decode once the default-on
// optimize_functions_to_subcolumns fires) and strictly safer
// (index-compatible) than the subcolumn spelling the issue originally
// proposed — see the issue for the full investigation.
func mapContainsFrag(col, key string) chsql.Frag {
	return chsql.Call("mapContains", chsql.Col(col), chsql.Lit(key))
}

// mapAtFrag emits "`<col>`[?]" — CH's Map column access shape — with
// key bound as a positional argument. Composed via the typed
// typed chsql.Subscript constructor, with the column flowing through
// Col (backtick-quoted) and the key through Lit
// (`?`-bound). Equivalent to Builder.MapAt but exposed as a typed
// Frag for QueryBuilder slot composition.
func mapAtFrag(col, key string) chsql.Frag {
	return chsql.Subscript(chsql.Col(col), chsql.Lit(key))
}

// nonEmptyFrag emits "`<col>` != ?" binding the empty string as a
// positional argument; used to drop the empty-string slot the
// arrayJoin synthesises for rows where the key lives in only one of
// the two attribute maps. The != operator routes through the typed
// chsql.Neq constructor.
func nonEmptyFrag(col string) chsql.Frag {
	return chsql.Neq(chsql.Col(col), chsql.Lit(""))
}

// intrinsicType returns the Tempo V2 type label for an intrinsic. Used
// to populate TagValueV2.Type; V1 doesn't surface this.
func intrinsicType(name string) string {
	switch name {
	case "duration":
		return "duration"
	case "status":
		return "status"
	case "kind":
		return "kind"
	}
	return "string"
}
