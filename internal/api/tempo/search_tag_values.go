package tempo

import (
	"errors"
	"net/http"
	"time"

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

	col, isIntrinsic := intrinsicColumn(name, h.Schema)
	var (
		sqlStr   string
		args     []any
		valueTyp string
	)
	if isIntrinsic {
		sqlStr, args = buildIntrinsicValuesSQL(h.Schema, col, start, end)
		valueTyp = intrinsicType(name)
	} else {
		sqlStr, args = buildAttributeValuesSQL(h.Schema, name, start, end)
		valueTyp = "string"
	}
	h.Logger.Debug("cerberus tempo /search/tag/values", "tag", name, "intrinsic", isIntrinsic, "sql", sqlStr, "args", args)

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
// values lookup. The tag key may live in SpanAttributes or
// ResourceAttributes; the CH idiom unions both maps via arrayJoin on a
// two-element array, then filters empties:
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
func buildAttributeValuesSQL(s schema.Traces, name string, start, end time.Time) (string, []any) {
	inner := chsql.NewQuery().
		Select(attrValueArrayJoinFrag(s.AttributesColumn, s.ResourceAttributesColumn, name)).
		From(chsql.Col(s.SpansTable)).
		Where(mapContainsAnyFrag(s.AttributesColumn, s.ResourceAttributesColumn, name))
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
