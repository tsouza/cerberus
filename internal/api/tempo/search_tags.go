package tempo

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/telemetry"
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
// upstream Tempo's `pkg/api.ParseSearchTagsRequest` semantics — the
// whole `traceql.AttributeScopeFromString` set plus the `intrinsic`
// carve-out that function does not know about:
//
//   - "resource"        → only the resource-attribute bucket
//   - "span"            → only the span-attribute bucket
//   - "event"           → only the span-event attribute bucket
//   - "link"            → only the span-link attribute bucket
//   - "instrumentation" → only the instrumentation-scope attribute bucket
//   - "intrinsic"       → only the intrinsic bucket
//   - "trace"           → a scope with no attribute family behind it
//   - "none" or ""      → every scope (default; matches AttributeScopeNone)
//
// Anything else is a 400. In particular "all" is *not* a scope:
// upstream's `traceql.AttributeScopeFromString` folds it to
// AttributeScopeUnknown and ParseSearchTagsRequest turns that into
// `invalid scope: all`. Accepting it here would make cerberus the more
// permissive backend, which silently trains clients into a request
// shape that breaks the moment they point at real Tempo.
const (
	tagScopeResource        = "resource"
	tagScopeSpan            = "span"
	tagScopeEvent           = "event"
	tagScopeLink            = "link"
	tagScopeInstrumentation = "instrumentation"
	tagScopeIntrinsic       = "intrinsic"
	tagScopeTrace           = "trace"
	tagScopeNone            = "none"
)

// nestedAttributesSubfield is the sub-column of the OTel-CH `Events` /
// `Links` Nested columns that carries the per-event / per-link
// attribute map, stored as `Array(Map(String, String))` — one map per
// event / link on the span row.
const nestedAttributesSubfield = "Attributes"

// handleSearchTags implements `GET /api/search/tags`. Returns the union
// of every dynamic attribute key across the scopes the request selects,
// sorted ascending. Honours optional `start`/`end` time bounds (Unix
// seconds; nanoseconds also accepted via the same heuristic Loki uses,
// see parseTempoTime).
//
// The `q` narrowing parameter is a V2 parameter and this route ignores
// it, as upstream does — a V1 request answers the whole window's key set
// whatever `q` says, including a malformed one (see the route contract in
// search_tags_filter.go).
//
// SQL shape — one query per scope bucket, e.g. for `span`:
//
//	SELECT DISTINCT arrayJoin(mapKeys(`SpanAttributes`))
//	FROM `otel_traces`
//	WHERE `Timestamp` >= ? AND `Timestamp` <= ?
//
// All identifiers and bound values flow through chsql.QueryBuilder —
// no fmt.Sprintf-on-SQL (CLAUDE.md "no raw SQL strings" rule).
func (h *Handler) handleSearchTags(w http.ResponseWriter, r *http.Request) {
	h.respondTags(w, r, TagsRouteV1)
}

// handleSearchTagsV2 implements `GET /api/v2/search/tags`. Same data
// as V1, partitioned by scope (one bucket per attribute family the
// schema carries, plus intrinsic). Grafana's Tempo datasource queries
// V2 when available.
//
// This is the route that takes the optional `q` parameter: it narrows the
// answer to the keys carried by the spans a TraceQL query selects, which
// is what Grafana's tag autocomplete sends as the user types. It
// contributes one more conjunct to the WHERE above; absent, the SQL is
// unchanged (see search_tags_filter.go).
func (h *Handler) handleSearchTagsV2(w http.ResponseWriter, r *http.Request) {
	h.respondTags(w, r, TagsRouteV2)
}

// respondTags is the shared core of V1 + V2: it runs one independent CH
// lookup per attribute-backed scope so the V2 response can keep the
// per-scope split, then unions them for the V1 envelope.
//
// The optional `?scope=` query parameter filters which buckets are
// fetched and returned. Honouring it on V2 was missing before — the
// handler emitted every scope regardless of what the client asked for,
// which made Grafana's per-scope autocomplete iterate over irrelevant
// keys.
func (h *Handler) respondTags(w http.ResponseWriter, r *http.Request, route TagsRoute) {
	start, end, err := parseTempoStartEnd(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "", "", err)
		return
	}
	// Bound a windowless discovery request to the recent window so the
	// mapKeys fan-out part-prunes otel_traces instead of full-scanning +
	// exploding the attribute Map (the multi-minute / timeout failure).
	start, end = BoundDiscoveryWindow(start, end)

	scope, err := parseTagScope(r.URL.Query().Get("scope"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "", "", err)
		return
	}

	// The optional `?q=` narrowing filter, which only the V2 route takes —
	// tagQueryFilter discards it on V1. It is read after `scope` so a
	// request that gets both wrong still reports the scope first, matching
	// upstream's parameter order.
	filter, err := h.tagQueryFilter(r.Context(), route, r.URL.Query().Get("q"), start, end)
	if err != nil {
		writeError(w, tagsErrStatus(err), "", "", err)
		return
	}

	scopes, err := h.collectAttributeTagScopes(r.Context(), scope, filter, start, end)
	if err != nil {
		writeError(w, tagsErrStatus(err), "", "", err)
		return
	}
	var all []string
	for _, s := range scopes {
		all = append(all, s.Tags...)
	}

	if route == TagsRouteV2 {
		if scope == tagScopeNone || scope == tagScopeIntrinsic {
			scopes = append(scopes, TagScope{Name: tagScopeIntrinsic, Tags: append([]string(nil), intrinsicTags...)})
		}
		if scopes == nil {
			scopes = []TagScope{}
		}
		writeJSON(w, http.StatusOK, SearchTagsResponseV2{Scopes: scopes})
		return
	}

	// V1 mirrors upstream Tempo: intrinsics only surface when the caller
	// asks for `scope=intrinsic` explicitly. The default (and `scope=none`)
	// returns dynamic attributes only — leaking intrinsics here puts the
	// V1 envelope out of parity with reference Tempo on tags_v1_all.
	if scope == tagScopeIntrinsic {
		all = append(all, intrinsicTags...)
	}
	writeJSON(w, http.StatusOK, SearchTagsResponse{TagNames: sortedUnique(all)})
}

// collectAttributeTagScopes runs the key lookup for every
// attribute-backed scope the requested `scope` selects, and returns
// the buckets that carry at least one key, sorted and de-duplicated.
//
// A scope with no keys in the window contributes no bucket: upstream's
// tags-V2 combiner materialises a bucket only from keys the storage
// layer actually reported, so emitting an empty one would put cerberus
// a scope ahead of reference Tempo.
func (h *Handler) collectAttributeTagScopes(
	ctx context.Context, scope string, filter chsql.Frag, start, end time.Time,
) ([]TagScope, error) {
	var scopes []TagScope
	for _, src := range attributeTagScopes(h.Schema) {
		if scope != tagScopeNone && scope != src.name {
			continue
		}
		tags, err := h.fetchTagKeys(ctx, src.name, src.keys, filter, start, end)
		if err != nil {
			h.Logger.Error("cerberus tempo tag-key lookup failed", "scope", src.name, "err", err)
			return nil, err
		}
		if tags = sortedUnique(tags); len(tags) == 0 {
			continue
		}
		scopes = append(scopes, TagScope{Name: src.name, Tags: tags})
	}
	return scopes, nil
}

// attributeTagScope binds one scope bucket to the CH projection that
// enumerates its attribute keys.
type attributeTagScope struct {
	name string
	keys chsql.Frag
}

// attributeTagScopes returns the scope buckets whose keys come from a
// column on the spans table, in the order upstream Tempo's storage
// layer scans them (`vparquet4/block_search_tags.go`).
//
// A scope the configured schema carries no column for is absent from
// the slice rather than present-and-empty: `trace` has no attribute
// family at all in either backend, and the upstream OTel-CH traces DDL
// declares no instrumentation-scope attribute map (a custom schema may
// point ScopeAttributesColumn at one, which puts the bucket back).
// Requesting such a scope is still a 200 — upstream accepts the value
// and answers with whatever its storage reports, which is nothing.
func attributeTagScopes(s schema.Traces) []attributeTagScope {
	out := make([]attributeTagScope, 0, 5)
	add := func(name string, keys chsql.Frag) {
		out = append(out, attributeTagScope{name: name, keys: keys})
	}
	if s.ResourceAttributesColumn != "" {
		add(tagScopeResource, distinctMapKeysFrag(s.ResourceAttributesColumn))
	}
	if s.ScopeAttributesColumn != "" {
		add(tagScopeInstrumentation, distinctMapKeysFrag(s.ScopeAttributesColumn))
	}
	if s.AttributesColumn != "" {
		add(tagScopeSpan, distinctMapKeysFrag(s.AttributesColumn))
	}
	if s.EventsColumn != "" {
		add(tagScopeEvent, distinctNestedMapKeysFrag(s.EventsColumn))
	}
	if s.LinksColumn != "" {
		add(tagScopeLink, distinctNestedMapKeysFrag(s.LinksColumn))
	}
	return out
}

// parseTagScope normalises the `?scope=` query parameter against the
// upstream Tempo allowlist. Returns the canonical scope keyword the
// rest of the handler branches on, or an error suitable for a 400. The
// empty string and the literal "none" both collapse to "none" (= every
// scope, matching upstream's AttributeScopeNone). Every other value —
// "all" included — is rejected with the same shape upstream Tempo uses.
func parseTagScope(raw string) (string, error) {
	switch raw {
	case "", tagScopeNone:
		return tagScopeNone, nil
	case tagScopeResource, tagScopeSpan, tagScopeEvent, tagScopeLink,
		tagScopeInstrumentation, tagScopeIntrinsic, tagScopeTrace:
		return raw, nil
	default:
		return "", &tagScopeError{raw: raw}
	}
}

// tagScopeError carries the rejected scope value into writeError so the
// 400 body matches upstream Tempo's `invalid scope: <v>` phrasing.
type tagScopeError struct{ raw string }

func (e *tagScopeError) Error() string { return "invalid scope: " + e.raw }

// fetchTagKeys runs the distinct-key lookup for a single scope bucket.
// Keeping one query per scope (rather than one with arrayConcat) is
// what lets the V2 partition skip the buckets the caller didn't ask
// for — V1 unions the surviving slices in Go.
func (h *Handler) fetchTagKeys(
	ctx context.Context,
	scope string,
	keys, filter chsql.Frag,
	start, end time.Time,
) ([]string, error) {
	sqlStr, args := buildSearchTagsSQL(h.Schema, keys, filter, start, end)
	h.Logger.Debug("cerberus tempo /search/tags", "scope", scope, "sql", sqlStr,
		"args", telemetry.SanitizeArgsForLog(args))
	return h.Client.QueryStrings(ctx, sqlStr, args...)
}

// buildSearchTagsSQL builds the SELECT for one scope bucket's
// key-enumerating projection. Exposed for tests so the SQL shape is
// pinned without spinning up the HTTP layer.
//
// `filter` is the optional `?q=` span-row predicate (see
// search_tags_filter.go); a nil filter appends no clause, so a request
// without `q` renders exactly the SQL it always did.
func buildSearchTagsSQL(s schema.Traces, keys, filter chsql.Frag, start, end time.Time) (string, []any) {
	sb := chsql.NewQuery().
		Select(keys).
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

// distinctNestedMapKeysFrag is the same idea one nesting level up:
// `Events.Attributes` / `Links.Attributes` are Array(Map(...)) — one
// map per event / link on the span row — so the keys of every element
// are collected with arrayMap+mapKeys, flattened into a single key
// array, and then unrolled by arrayJoin.
//
// Emits "DISTINCT arrayJoin(arrayFlatten(arrayMap(m -> mapKeys(m),
// `<col>`.`Attributes`)))". `nestedMapParam` is the lambda's bound
// element. The sub-column is addressed with the same qualified-identifier
// spelling every other Nested reference in the emitter uses (see
// Builder.exprNestedArrayExists).
func distinctNestedMapKeysFrag(col string) chsql.Frag {
	const nestedMapParam = "m"
	return chsql.Distinct(
		chsql.Call(
			"arrayJoin",
			chsql.Call(
				"arrayFlatten",
				chsql.Call(
					"arrayMap",
					chsql.Lambda1(nestedMapParam, chsql.Call("mapKeys", chsql.BareIdent(nestedMapParam))),
					chsql.Qual(col, nestedAttributesSubfield),
				),
			),
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
