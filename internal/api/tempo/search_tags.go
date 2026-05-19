package tempo

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// intrinsicTags is the static list of Tempo "intrinsic" attribute names
// that aren't stored as map entries on the span row — they're dedicated
// columns or per-trace derived fields. Upstream Tempo emits them in the
// /api/v2/search/tags response (the intrinsic scope) so the autocomplete
// UI knows about them.
//
// The list mirrors upstream `pkg/search.GetVirtualIntrinsicValues()` —
// the 25-element canonical Tempo intrinsic inventory split into the
// "bare" form (`name`, `duration`, …) and the scoped form (`span:name`,
// `span:duration`, …). Keeping cardinality + spelling identical is the
// gate the compatibility differ uses to assert parity on tags_v2.
//
// `parent` (the bare alias for `span:parentID`) is intentionally
// omitted: upstream does not surface it from `GetVirtualIntrinsicValues`,
// so emitting it would put cerberus one tag ahead of Tempo and fail the
// set-equality diff.
var intrinsicTags = []string{
	// Bare intrinsics (legacy / unscoped form).
	"duration",
	"event:name",
	"event:timeSinceStart",
	"instrumentation:name",
	"instrumentation:version",
	"kind",
	"link:spanID",
	"link:traceID",
	"name",
	"rootName",
	"rootServiceName",
	"status",
	"statusMessage",
	"traceDuration",
	// Scoped intrinsics (span: / trace: prefixed canonical form).
	"span:duration",
	"span:id",
	"span:kind",
	"span:name",
	"span:parentID",
	"span:status",
	"span:statusMessage",
	"trace:duration",
	"trace:id",
	"trace:rootName",
	"trace:rootService",
}

// SearchTagsResponse is the V1 response body of /api/search/tags. Tempo
// returns the union of every span attribute key seen plus the intrinsic
// span fields.
type SearchTagsResponse struct {
	TagNames []string `json:"tagNames"`
}

// SearchTagsResponseV2 is the V2 response body of /api/v2/search/tags.
// Tempo V2 groups tags by scope so the autocomplete UI can surface
// `resource.` / `span.` / intrinsic prefixes separately.
type SearchTagsResponseV2 struct {
	Scopes []TagScope `json:"scopes"`
}

// TagScope is one group in the V2 response — one of "resource", "span",
// or "intrinsic". `Tags` carries the attribute keys for that scope.
type TagScope struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

// scope query-param values accepted by /api/v2/search/tags. Mirrors
// upstream Tempo's `pkg/api.ParseSearchTagsRequest` semantics:
//
//   - "resource"  → only the resource scope bucket
//   - "span"      → only the span scope bucket
//   - "intrinsic" → only the intrinsic bucket
//   - "none" or "" → every scope (default; matches AttributeScopeNone)
//
// Anything else is a 400. We accept "all" as a friendly alias for the
// default — upstream Tempo doesn't define it, but the parameter is
// otherwise free-form to clients and "all" is the obvious user
// intuition for "give me everything", so we let it through.
const (
	tagScopeResource  = "resource"
	tagScopeSpan      = "span"
	tagScopeIntrinsic = "intrinsic"
	tagScopeNone      = "none"
	tagScopeAll       = "all"
)

// handleSearchTags implements `GET /api/search/tags`. Returns the union
// of every dynamic attribute key (span + resource maps) plus the static
// intrinsic-span list, sorted ascending. Honours optional `start`/`end`
// time bounds (Unix seconds; nanoseconds also accepted via the same
// heuristic Loki uses, see parseTempoTime).
//
// SQL shape:
//
//	SELECT DISTINCT arrayJoin(arrayConcat(
//	    mapKeys(`SpanAttributes`),
//	    mapKeys(`ResourceAttributes`)
//	)) AS `tag`
//	FROM `otel_traces`
//	WHERE `Timestamp` >= ? AND `Timestamp` <= ?
//
// All identifiers and bound values flow through chsql.QueryBuilder —
// no fmt.Sprintf-on-SQL (CLAUDE.md "no raw SQL strings" rule).
func (h *Handler) handleSearchTags(w http.ResponseWriter, r *http.Request) {
	h.respondTags(w, r, false)
}

// handleSearchTagsV2 implements `GET /api/v2/search/tags`. Same data
// as V1, partitioned by scope (resource / span / intrinsic). Grafana's
// Tempo datasource queries V2 when available.
func (h *Handler) handleSearchTagsV2(w http.ResponseWriter, r *http.Request) {
	h.respondTags(w, r, true)
}

// respondTags is the shared core of V1 + V2: it runs two independent
// CH lookups (one per attribute map) so the V2 response can keep the
// resource-vs-span split, then unions them for the V1 envelope.
//
// The optional `?scope=` query parameter filters which buckets are
// fetched and returned. Honouring it on V2 was missing before — the
// handler emitted every scope regardless of what the client asked for,
// which made Grafana's per-scope autocomplete iterate over irrelevant
// keys.
func (h *Handler) respondTags(w http.ResponseWriter, r *http.Request, v2 bool) {
	start, end, err := parseTempoStartEnd(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "", "", err)
		return
	}

	scope, err := parseTagScope(r.URL.Query().Get("scope"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "", "", err)
		return
	}

	var resourceTags, spanTags []string
	if scope == tagScopeNone || scope == tagScopeResource {
		resourceTags, err = h.fetchTagKeys(r.Context(), h.Schema.ResourceAttributesColumn, start, end)
		if err != nil {
			h.Logger.Error("cerberus tempo /search/tags resource CH query failed", "err", err)
			writeError(w, http.StatusBadGateway, "", "", err)
			return
		}
	}
	if scope == tagScopeNone || scope == tagScopeSpan {
		spanTags, err = h.fetchTagKeys(r.Context(), h.Schema.AttributesColumn, start, end)
		if err != nil {
			h.Logger.Error("cerberus tempo /search/tags span CH query failed", "err", err)
			writeError(w, http.StatusBadGateway, "", "", err)
			return
		}
	}

	if v2 {
		scopes := make([]TagScope, 0, 3)
		if scope == tagScopeNone || scope == tagScopeResource {
			scopes = append(scopes, TagScope{Name: tagScopeResource, Tags: sortedUnique(resourceTags)})
		}
		if scope == tagScopeNone || scope == tagScopeSpan {
			scopes = append(scopes, TagScope{Name: tagScopeSpan, Tags: sortedUnique(spanTags)})
		}
		if scope == tagScopeNone || scope == tagScopeIntrinsic {
			scopes = append(scopes, TagScope{Name: tagScopeIntrinsic, Tags: append([]string(nil), intrinsicTags...)})
		}
		writeJSON(w, http.StatusOK, SearchTagsResponseV2{Scopes: scopes})
		return
	}

	all := make([]string, 0, len(resourceTags)+len(spanTags)+len(intrinsicTags))
	all = append(all, resourceTags...)
	all = append(all, spanTags...)
	// V1 mirrors upstream Tempo: intrinsics only surface when the caller
	// asks for `scope=intrinsic` explicitly. The default (and `scope=none`)
	// returns dynamic attributes only — leaking intrinsics here puts the
	// V1 envelope out of parity with reference Tempo on tags_v1_all.
	if scope == tagScopeIntrinsic {
		all = append(all, intrinsicTags...)
	}
	writeJSON(w, http.StatusOK, SearchTagsResponse{TagNames: sortedUnique(all)})
}

// parseTagScope normalises the `?scope=` query parameter against the
// upstream Tempo allowlist. Returns the canonical scope keyword the
// rest of the handler branches on, or an error suitable for a 400. The
// empty string and the literal "none" both collapse to "none" (= every
// scope, matching upstream's AttributeScopeNone). "all" is accepted as
// a synonym for "none" because it's the obvious user intuition; every
// other value is rejected with the same shape upstream Tempo uses.
func parseTagScope(raw string) (string, error) {
	switch raw {
	case "", tagScopeNone, tagScopeAll:
		return tagScopeNone, nil
	case tagScopeResource, tagScopeSpan, tagScopeIntrinsic:
		return raw, nil
	default:
		return "", &tagScopeError{raw: raw}
	}
}

// tagScopeError carries the rejected scope value into writeError so the
// 400 body matches upstream Tempo's `invalid scope: <v>` phrasing.
type tagScopeError struct{ raw string }

func (e *tagScopeError) Error() string { return "invalid scope: " + e.raw }

// fetchTagKeys runs the mapKeys-distinct lookup for a single attribute
// map column. Splitting into two queries (rather than one with
// arrayConcat) keeps the V2 scope partition cheap — V1 unions the two
// slices in Go.
func (h *Handler) fetchTagKeys(ctx context.Context, mapCol string, start, end time.Time) ([]string, error) {
	sqlStr, args := buildSearchTagsSQL(h.Schema, mapCol, start, end)
	h.Logger.Debug("cerberus tempo /search/tags", "col", mapCol, "sql", sqlStr, "args", args)
	return h.Client.QueryStrings(ctx, sqlStr, args...)
}

// buildSearchTagsSQL builds the SELECT for one attribute map column.
// Exposed for tests so the SQL shape is pinned without spinning up the
// HTTP layer.
func buildSearchTagsSQL(s schema.Traces, mapCol string, start, end time.Time) (string, []any) {
	sb := chsql.NewQuery().
		Select(distinctMapKeysFrag(mapCol)).
		From(chsql.Col(s.SpansTable))
	if !start.IsZero() {
		sb.Where(tempoTimeGteFrag(s.TimestampColumn, start))
	}
	if !end.IsZero() {
		sb.Where(tempoTimeLteFrag(s.TimestampColumn, end))
	}
	return sb.Build()
}

// distinctMapKeysFrag emits "DISTINCT arrayJoin(mapKeys(`<col>`))" — the
// CH idiom for "every distinct attribute key seen". DISTINCT is part of
// the SELECT list (CH's flavour), not a separate keyword, so it folds
// into the Frag for the QueryBuilder slot. `arrayJoin` + `mapKeys` are
// CH functions composed through the typed Call constructor; the column
// operand flows through chsql.Col.
func distinctMapKeysFrag(col string) chsql.Frag {
	return chsql.Distinct(
		chsql.Call(
			"arrayJoin",
			chsql.Call("mapKeys", chsql.Col(col)),
		),
	)
}

// tempoTimeGteFrag emits "`<col>` >= toDateTime64('...', 9)" — the
// lower-bound predicate of a Tempo `start` query parameter. The >=
// operator routes through the typed chsql.Gte constructor; the
// DateTime64 literal is rendered via a small Frag wrapper that
// delegates to Builder.DateTime64Lit (no typed Frag exists for that
// CH-specific literal shape).
func tempoTimeGteFrag(col string, t time.Time) chsql.Frag {
	return chsql.Gte(chsql.Col(col), dateTime64LitFrag(t))
}

// tempoTimeLteFrag emits "`<col>` <= toDateTime64('...', 9)" — the
// upper-bound counterpart to tempoTimeGteFrag.
func tempoTimeLteFrag(col string, t time.Time) chsql.Frag {
	return chsql.Lte(chsql.Col(col), dateTime64LitFrag(t))
}

// dateTime64LitFrag wraps Builder.DateTime64Lit as a typed Frag so
// callers can compose CH DateTime64(9) literals through chsql.Gte /
// chsql.Lte / etc. without dropping back to raw writes.
func dateTime64LitFrag(t time.Time) chsql.Frag {
	return func(b *chsql.Builder) { b.DateTime64Lit(t) }
}

// sortedUnique returns the de-duplicated, lexicographically sorted view
// of in. The returned slice is always non-nil so JSON marshals as `[]`
// (not `null`) when the input is empty.
func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		seen[s] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
