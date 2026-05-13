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
// columns. Upstream Tempo emits them alongside the dynamic attribute
// keys in the /api/search/tags response so the autocomplete UI knows
// about them.
//
// The list mirrors the documented intrinsics: name, status (StatusCode),
// statusMessage, kind (SpanKind), duration, rootServiceName, rootName,
// traceDuration. Cerberus surfaces the subset we can actually answer
// /search/tag/<name>/values queries for today; rootServiceName /
// rootName / traceDuration would require a per-trace pivot that lands
// with the rest of the trace-summary plumbing in RC2.
var intrinsicTags = []string{
	"name",
	"kind",
	"status",
	"statusMessage",
	"duration",
	"parent",
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
// All identifiers and bound values flow through chsql.SelectBuilder —
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
func (h *Handler) respondTags(w http.ResponseWriter, r *http.Request, v2 bool) {
	start, end, err := parseTempoStartEnd(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "", "", err)
		return
	}

	resourceTags, err := h.fetchTagKeys(r.Context(), h.Schema.ResourceAttributesColumn, start, end)
	if err != nil {
		h.Logger.Warn("cerberus tempo /search/tags resource CH query failed", "err", err.Error())
		writeError(w, http.StatusBadGateway, "", "", err)
		return
	}
	spanTags, err := h.fetchTagKeys(r.Context(), h.Schema.AttributesColumn, start, end)
	if err != nil {
		h.Logger.Warn("cerberus tempo /search/tags span CH query failed", "err", err.Error())
		writeError(w, http.StatusBadGateway, "", "", err)
		return
	}

	if v2 {
		writeJSON(w, http.StatusOK, SearchTagsResponseV2{
			Scopes: []TagScope{
				{Name: "resource", Tags: sortedUnique(resourceTags)},
				{Name: "span", Tags: sortedUnique(spanTags)},
				{Name: "intrinsic", Tags: append([]string(nil), intrinsicTags...)},
			},
		})
		return
	}

	all := make([]string, 0, len(resourceTags)+len(spanTags)+len(intrinsicTags))
	all = append(all, resourceTags...)
	all = append(all, spanTags...)
	all = append(all, intrinsicTags...)
	writeJSON(w, http.StatusOK, SearchTagsResponse{TagNames: sortedUnique(all)})
}

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
	sb := chsql.NewSelect().
		Select(distinctMapKeysFrag(mapCol)).
		From(chsql.Col(s.SpansTable))
	if !start.IsZero() {
		sb.Where(tempoTimeBoundFrag(s.TimestampColumn, ">=", start))
	}
	if !end.IsZero() {
		sb.Where(tempoTimeBoundFrag(s.TimestampColumn, "<=", end))
	}
	return sb.Build()
}

// distinctMapKeysFrag emits "DISTINCT arrayJoin(mapKeys(`<col>`))" — the
// CH idiom for "every distinct attribute key seen". DISTINCT is part of
// the SELECT list (CH's flavour), not a separate keyword, so it folds
// into the Frag for the SelectBuilder slot.
func distinctMapKeysFrag(col string) chsql.Frag {
	return func(b *chsql.Builder) {
		b.WriteSQL("DISTINCT arrayJoin(")
		b.MapKeys(col)
		b.WriteSQL(")")
	}
}

// tempoTimeBoundFrag mirrors loki/index_stats.go's helper. Duplicated
// here rather than exported so the two API packages stay independent.
func tempoTimeBoundFrag(col, op string, t time.Time) chsql.Frag {
	return func(b *chsql.Builder) {
		b.Ident(col)
		b.WriteSQL(" ")
		b.WriteSQL(op)
		b.WriteSQL(" ")
		b.DateTime64Lit(t)
	}
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
