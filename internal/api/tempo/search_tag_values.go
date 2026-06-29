package tempo

import (
	"errors"
	"net/http"
	"time"

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
func (h *Handler) handleSearchTagValues(w http.ResponseWriter, r *http.Request) {
	h.respondTagValues(w, r, false)
}

// handleSearchTagValuesV2 implements
// `GET /api/v2/search/tag/{name}/values`. Same data as V1, wrapped per
// Tempo V2's typed envelope.
func (h *Handler) handleSearchTagValuesV2(w http.ResponseWriter, r *http.Request) {
	h.respondTagValues(w, r, true)
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
func (h *Handler) respondTagValues(w http.ResponseWriter, r *http.Request, v2 bool) {
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
	// Bound a windowless tag-value lookup to the recent window so the
	// per-key scan part-prunes otel_traces instead of full-scanning the
	// fact table (same map-explosion failure as /search/tags).
	start, end = BoundDiscoveryWindow(start, end)

	resolved, _ := resolveTagName(name, h.Schema)
	var (
		sqlStr   string
		args     []any
		valueTyp string
	)
	if resolved.IsIntrinsic {
		sqlStr, args = buildIntrinsicValuesSQL(h.Schema, resolved.IntrinsicCol, start, end)
		valueTyp = intrinsicType(resolved.IntrinsicName)
	} else {
		sqlStr, args = buildAttributeValuesSQL(h.Schema, resolved.Key, resolved.MapScope, start, end)
		valueTyp = "string"
	}
	h.Logger.Debug("cerberus tempo /search/tag/values",
		"tag", name,
		"intrinsic", resolved.IsIntrinsic,
		"map_scope", resolved.MapScope,
		"key", resolved.Key,
		"sql", sqlStr,
		"args", args)

	values, err := h.Client.QueryStrings(r.Context(), sqlStr, args...)
	if err != nil {
		h.Logger.Error("cerberus tempo /search/tag/values CH query failed", "err", err, "tag", name)
		writeError(w, http.StatusBadGateway, "", "", err)
		return
	}
	values = sortedUnique(values)

	if v2 {
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
func buildIntrinsicValuesSQL(s schema.Traces, col string, start, end time.Time) (string, []any) {
	sb := chsql.NewQuery().
		Select(distinctToStringFrag(col)).
		From(chsql.Col(s.SpansTable))
	if !start.IsZero() {
		sb.Where(tempoTimeGteFrag(s.TimestampColumn, start))
	}
	if !end.IsZero() {
		sb.Where(tempoTimeLteFrag(s.TimestampColumn, end))
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
func buildAttributeValuesSQL(s schema.Traces, name string, scope attrMapScope, start, end time.Time) (string, []any) {
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

	outer := chsql.NewQuery().
		Select(chsql.Distinct(chsql.Col("v"))).
		From(inner.Frag()).
		Where(nonEmptyFrag("v"))
	return outer.Build()
}

// attrMapScope expresses which attribute map(s) a tag-values lookup
// should consult. Driven by parsing the URL tag-name as a TraceQL
// identifier — see resolveTagName.
type attrMapScope int

const (
	// attrMapScopeAny unions both SpanAttributes and ResourceAttributes
	// (Tempo's auto-scope form: bare `service.name`, leading-dot
	// `.service.name`).
	attrMapScopeAny attrMapScope = iota
	// attrMapScopeResource consults only ResourceAttributes
	// (Tempo's `resource.x` scoped form).
	attrMapScopeResource
	// attrMapScopeSpan consults only SpanAttributes
	// (Tempo's `span.x` scoped form).
	attrMapScopeSpan
)

func (s attrMapScope) String() string {
	switch s {
	case attrMapScopeResource:
		return "resource"
	case attrMapScopeSpan:
		return "span"
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
