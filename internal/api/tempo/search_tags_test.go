package tempo_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/tempo"
)

// TestSearchTags_UnionsDynamicAttributes — the V1 endpoint returns the
// sorted, de-duplicated union of:
//   - keys returned by the ResourceAttributes lookup,
//   - keys returned by the SpanAttributes lookup.
//
// Intrinsics are NOT included on the default V1 envelope — upstream
// Tempo only adds them when the caller passes `?scope=intrinsic` (see
// `pkg/tempopb`'s SearchTagsRequest handling). The cerberus V1 path
// mirrors that carve-out so the tags_v1_all compatibility case passes.
//
// stringsBySQL routes each CH call by the column it qualifies on, so
// the test can give distinct row sets to the two lookups even though
// they share the chclient.QueryStrings entry point.
func TestSearchTags_UnionsDynamicAttributes(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{stringsBySQL: map[string][]string{
		"`ResourceAttributes`": {"service.name", "host"},
		"`SpanAttributes`":     {"http.method", "host"}, // `host` duplicates resource
	}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search/tags")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var body tempo.SearchTagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Dynamic keys must surface; intrinsics MUST NOT — default V1 only
	// carries the dynamic attribute set (upstream parity).
	for _, want := range []string{"service.name", "host", "http.method"} {
		if !contains(body.TagNames, want) {
			t.Errorf("missing dynamic tag %q in response: %v", want, body.TagNames)
		}
	}
	for _, leaked := range []string{"name", "kind", "status", "duration", "statusMessage"} {
		if contains(body.TagNames, leaked) {
			t.Errorf("intrinsic %q leaked into default V1 envelope: %v", leaked, body.TagNames)
		}
	}
	// De-dup: `host` appears in both maps but only once.
	if c := count(body.TagNames, "host"); c != 1 {
		t.Errorf("tag `host` should appear exactly once, got %d in %v", c, body.TagNames)
	}
	// Sorted ascending.
	for i := 1; i < len(body.TagNames); i++ {
		if body.TagNames[i-1] > body.TagNames[i] {
			t.Errorf("response not sorted at %d: %q > %q", i, body.TagNames[i-1], body.TagNames[i])
		}
	}
}

// TestSearchTags_SQLShape — pins the SQL the handler emits so the
// no-fmt-Sprintf-on-SQL rule (CLAUDE.md) doesn't quietly regress.
// Builder must emit a parameterised `arrayJoin(mapKeys(<col>))`
// SELECT — never a raw concatenated table name or column literal.
func TestSearchTags_SQLShape(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{strings: []string{"a"}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search/tags")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	for _, want := range []string{
		"SELECT DISTINCT",
		"arrayJoin(",
		"mapKeys(",
		"FROM `otel_traces`",
	} {
		if !strings.Contains(q.lastSQL, want) {
			t.Errorf("SQL missing %q\n  got: %s", want, q.lastSQL)
		}
	}
}

// TestSearchTags_TimeBounds — `start` / `end` query parameters thread
// into the WHERE clause as toDateTime64 literals. Tempo accepts unix
// seconds; the handler also accepts nanoseconds.
func TestSearchTags_TimeBounds(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{strings: []string{"a"}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search/tags?start=1700000000&end=1700003600")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if !strings.Contains(q.lastSQL, "WHERE") {
		t.Errorf("expected WHERE clause, got: %s", q.lastSQL)
	}
	if !strings.Contains(q.lastSQL, "toDateTime64") {
		t.Errorf("expected toDateTime64 bound, got: %s", q.lastSQL)
	}
}

// TestSearchTags_BadTimeBound — non-numeric, non-RFC3339 input yields
// a 400 with the Tempo error envelope.
func TestSearchTags_BadTimeBound(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search/tags?start=notatime")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", resp.StatusCode)
	}
	var er tempo.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !er.Error {
		t.Errorf("expected error=true, got %+v", er)
	}
}

// TestSearchTags_CHFailure — CH error bubbles up as 502 with the Tempo
// error envelope (parity with /api/search/recent).
func TestSearchTags_CHFailure(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{err: errors.New("clickhouse: connection refused")}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search/tags")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d want 502", resp.StatusCode)
	}
}

// TestSearchTags_EmptyArray — an empty CH result still produces
// `{"tagNames":[]}` (non-nil) so Grafana's JSON decoder doesn't choke
// on `null`. Default V1 has no intrinsics, so the slice is fully
// empty when CH returns no rows.
func TestSearchTags_EmptyArray(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{strings: nil}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search/tags")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var body tempo.SearchTagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.TagNames == nil {
		t.Fatalf("expected non-nil tagNames slice (empty CH result), got nil")
	}
	// Default V1 must not surface intrinsics — they're reserved for the
	// `?scope=intrinsic` carve-out.
	for _, leaked := range []string{"name", "kind", "status", "duration", "statusMessage"} {
		if contains(body.TagNames, leaked) {
			t.Errorf("intrinsic %q leaked into default V1 envelope: %v", leaked, body.TagNames)
		}
	}
}

// TestSearchTagsV2_ScopePartition — the V2 endpoint groups tags by
// scope (resource / span / intrinsic). The two map lookups feed
// distinct scope buckets, and the static intrinsic list lives in its
// own.
func TestSearchTagsV2_ScopePartition(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{stringsBySQL: map[string][]string{
		"`ResourceAttributes`": {"service.name"},
		"`SpanAttributes`":     {"http.method"},
	}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v2/search/tags")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var body tempo.SearchTagsResponseV2
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	scopes := map[string][]string{}
	for _, s := range body.Scopes {
		scopes[s.Name] = s.Tags
	}
	if !contains(scopes["resource"], "service.name") {
		t.Errorf("resource scope missing service.name: %+v", scopes)
	}
	if !contains(scopes["span"], "http.method") {
		t.Errorf("span scope missing http.method: %+v", scopes)
	}
	if !contains(scopes["intrinsic"], "name") {
		t.Errorf("intrinsic scope missing `name`: %+v", scopes)
	}
}

// TestSearchTagsV2_ScopeFilter — the `?scope=` query parameter on
// /api/v2/search/tags must restrict the response to the requested
// bucket. Mirrors upstream Tempo's pkg/api.ParseSearchTagsRequest
// semantics (resource / span / intrinsic / none-or-empty / "all" alias).
// Before this fix the handler silently emitted every scope, so
// Grafana's per-scope autocomplete iterated over irrelevant keys.
func TestSearchTagsV2_ScopeFilter(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		query      string
		wantScopes []string
		// wantTag is one tag the response *must* contain; absent means
		// "no positive-content assertion, just the scope partition
		// matters".
		wantTag string
	}{
		{name: "resource_only", query: "?scope=resource", wantScopes: []string{"resource"}, wantTag: "service.name"},
		{name: "span_only", query: "?scope=span", wantScopes: []string{"span"}, wantTag: "http.method"},
		{name: "intrinsic_only", query: "?scope=intrinsic", wantScopes: []string{"intrinsic"}, wantTag: "name"},
		{name: "none_default", query: "", wantScopes: []string{"resource", "span", "intrinsic"}},
		{name: "none_explicit", query: "?scope=none", wantScopes: []string{"resource", "span", "intrinsic"}},
		{name: "all_alias", query: "?scope=all", wantScopes: []string{"resource", "span", "intrinsic"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := &stubQuerier{stringsBySQL: map[string][]string{
				"`ResourceAttributes`": {"service.name"},
				"`SpanAttributes`":     {"http.method"},
			}}
			srv := newServer(q, "v1.0.0-test")
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + "/api/v2/search/tags" + tc.query)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
			}
			var body tempo.SearchTagsResponseV2
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}

			gotNames := make([]string, 0, len(body.Scopes))
			gotByName := map[string][]string{}
			for _, s := range body.Scopes {
				gotNames = append(gotNames, s.Name)
				gotByName[s.Name] = s.Tags
			}
			if len(gotNames) != len(tc.wantScopes) {
				t.Fatalf("scope buckets=%v want=%v", gotNames, tc.wantScopes)
			}
			for _, want := range tc.wantScopes {
				if _, ok := gotByName[want]; !ok {
					t.Errorf("missing scope %q in %v", want, gotNames)
				}
			}
			if tc.wantTag != "" {
				// The single requested scope must carry the canary tag.
				bucket := tc.wantScopes[0]
				if !contains(gotByName[bucket], tc.wantTag) {
					t.Errorf("scope %q missing %q: %+v", bucket, tc.wantTag, gotByName[bucket])
				}
			}
		})
	}
}

// TestSearchTagsV2_InvalidScope — anything outside the upstream
// allowlist must surface as a 400 with the Tempo error envelope, so
// clients see the same shape they would hitting reference Tempo.
func TestSearchTagsV2_InvalidScope(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v2/search/tags?scope=bogus")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 body=%s", resp.StatusCode, readBody(t, resp))
	}
	var er tempo.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !er.Error {
		t.Errorf("expected error=true, got %+v", er)
	}
	if !strings.Contains(er.Message, "invalid scope") {
		t.Errorf("expected error message to mention %q, got %q", "invalid scope", er.Message)
	}
}

// TestSearchTagsV2_ScopeSkipsCH — when the caller asks for `scope=intrinsic`
// the handler must NOT issue the resource / span CH lookups. Pins the
// no-extra-roundtrips behaviour so future refactors can't quietly
// regress to always-fetch-all.
func TestSearchTagsV2_ScopeSkipsCH(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{stringsBySQL: map[string][]string{
		"`ResourceAttributes`": {"resource.should.not.appear"},
		"`SpanAttributes`":     {"span.should.not.appear"},
	}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v2/search/tags?scope=intrinsic")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	if q.lastSQL != "" {
		t.Errorf("intrinsic-only request must not hit CH, got SQL=%q", q.lastSQL)
	}
}

// TestSearchTags_V1ScopeFilter — V1 mirrors V2's parsing: invalid
// scopes still 400, and `scope=intrinsic` collapses the envelope to
// just the static intrinsic list (no CH fetches).
func TestSearchTags_V1ScopeFilter(t *testing.T) {
	t.Parallel()
	t.Run("intrinsic_only", func(t *testing.T) {
		t.Parallel()
		q := &stubQuerier{stringsBySQL: map[string][]string{
			"`ResourceAttributes`": {"resource.should.not.appear"},
			"`SpanAttributes`":     {"span.should.not.appear"},
		}}
		srv := newServer(q, "v1.0.0-test")
		t.Cleanup(srv.Close)

		resp, err := http.Get(srv.URL + "/api/search/tags?scope=intrinsic")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		var body tempo.SearchTagsResponse
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if contains(body.TagNames, "resource.should.not.appear") {
			t.Errorf("intrinsic-only V1 envelope leaked resource tag: %v", body.TagNames)
		}
		if !contains(body.TagNames, "name") {
			t.Errorf("intrinsic-only V1 envelope missing intrinsic %q: %v", "name", body.TagNames)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		t.Parallel()
		q := &stubQuerier{}
		srv := newServer(q, "v1.0.0-test")
		t.Cleanup(srv.Close)

		resp, err := http.Get(srv.URL + "/api/search/tags?scope=bogus")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status=%d want 400", resp.StatusCode)
		}
	})
}

// TestSearchTagsV2_IntrinsicListMatchesTempo — pins the V2 intrinsic
// list to exactly the 25-element inventory upstream Tempo emits from
// `pkg/search.GetVirtualIntrinsicValues()`. Reference Tempo on /tags/v2
// surfaces this set verbatim regardless of dynamic data; the
// compatibility differ requires set-equality with that list for the
// tags_v2_intrinsic case to pass.
//
// Bare + scoped forms (e.g. `name` AND `span:name`) coexist because
// upstream surfaces both — the autocomplete UI accepts either.
func TestSearchTagsV2_IntrinsicListMatchesTempo(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{stringsBySQL: map[string][]string{
		"`ResourceAttributes`": nil,
		"`SpanAttributes`":     nil,
	}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v2/search/tags?scope=intrinsic")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var body tempo.SearchTagsResponseV2
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Scopes) != 1 || body.Scopes[0].Name != "intrinsic" {
		t.Fatalf("expected exactly one `intrinsic` scope, got %+v", body.Scopes)
	}
	got := body.Scopes[0].Tags

	// Canonical Tempo list, kept in lockstep with upstream
	// pkg/search.GetVirtualIntrinsicValues().
	want := []string{
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
		"span:duration",
		"span:id",
		"span:kind",
		"span:name",
		"span:parentID",
		"span:status",
		"span:statusMessage",
		"status",
		"statusMessage",
		"trace:duration",
		"trace:id",
		"trace:rootName",
		"trace:rootService",
		"traceDuration",
	}
	if len(got) != len(want) {
		t.Errorf("V2 intrinsic count=%d want=%d got=%v", len(got), len(want), got)
	}
	gotSet := map[string]bool{}
	for _, s := range got {
		gotSet[s] = true
	}
	for _, w := range want {
		if !gotSet[w] {
			t.Errorf("missing canonical intrinsic %q in V2 response: %v", w, got)
		}
	}
	wantSet := map[string]bool{}
	for _, s := range want {
		wantSet[s] = true
	}
	for _, g := range got {
		if !wantSet[g] {
			t.Errorf("V2 emitted unexpected intrinsic %q (not in upstream list): %v", g, got)
		}
	}
	// `parent` was the legacy cerberus alias for `span:parentID`. It's
	// NOT part of upstream's list and must not leak — otherwise cerberus
	// goes one tag ahead of Tempo on the set-equality diff.
	if contains(got, "parent") {
		t.Errorf("V2 leaked legacy `parent` intrinsic (not in upstream): %v", got)
	}
}

// TestSearchTags_V1OmitsIntrinsicsByDefault — pins the V1 no-leak rule:
// without explicit `?scope=intrinsic`, the envelope must carry zero
// intrinsics regardless of whether the caller passed `?scope=none`,
// `?scope=all`, or no scope at all. Matches upstream Tempo's
// `tag_handlers.go` carve-out (intrinsics only when `Scope ==
// ParamScopeIntrinsic`) so the tags_v1_all compat case passes.
func TestSearchTags_V1OmitsIntrinsicsByDefault(t *testing.T) {
	t.Parallel()
	// Every intrinsic that previously leaked into V1.
	leakedBefore := []string{
		"name", "kind", "status", "statusMessage", "duration",
		// Plus all the new ones — none of them should appear either.
		"rootName", "rootServiceName", "traceDuration",
		"span:name", "span:kind", "trace:id",
		"event:name", "link:spanID", "instrumentation:name",
	}
	for _, query := range []string{"", "?scope=none", "?scope=all"} {
		query := query
		t.Run("query="+query, func(t *testing.T) {
			t.Parallel()
			q := &stubQuerier{stringsBySQL: map[string][]string{
				"`ResourceAttributes`": {"service.name"},
				"`SpanAttributes`":     {"http.method"},
			}}
			srv := newServer(q, "v1.0.0-test")
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + "/api/search/tags" + query)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
			}
			var body tempo.SearchTagsResponse
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			// Dynamic keys must still surface.
			if !contains(body.TagNames, "service.name") {
				t.Errorf("default V1 missing dynamic key `service.name`: %v", body.TagNames)
			}
			// No intrinsic must surface.
			for _, leaked := range leakedBefore {
				if contains(body.TagNames, leaked) {
					t.Errorf("default V1 leaked intrinsic %q: %v", leaked, body.TagNames)
				}
			}
		})
	}
}

// TestSearchTagsV2_WireShape — pins the V2 envelope so future
// regressions in JSON-marshal land surface here rather than in the
// compat differ. The envelope must be `{"scopes":[{"name":...,
// "tags":[...]}]}` with each requested scope present exactly once.
func TestSearchTagsV2_WireShape(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{stringsBySQL: map[string][]string{
		"`ResourceAttributes`": {"service.name"},
		"`SpanAttributes`":     {"http.method"},
	}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v2/search/tags")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `"scopes":`) {
		t.Errorf("V2 envelope missing top-level `scopes` key: %s", body)
	}
	if !strings.Contains(body, `"name":`) || !strings.Contains(body, `"tags":`) {
		t.Errorf("V2 scope entry missing `name`/`tags` keys: %s", body)
	}
}

// TestSearchTags_V1WireShape — pins the V1 envelope so future JSON
// regressions surface here rather than in the compat differ. The
// envelope must be `{"tagNames":[...]}` — never `null`, even when CH
// returns no rows.
func TestSearchTags_V1WireShape(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{strings: nil}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search/tags")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, `"tagNames":`) {
		t.Errorf("V1 envelope missing `tagNames` key: %s", body)
	}
	if strings.Contains(body, `"tagNames":null`) {
		t.Errorf("V1 envelope emitted null `tagNames` (must be `[]`): %s", body)
	}
}

// contains is a small helper so test failures point at the missing
// string rather than dumping the whole slice diff.
func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func count(haystack []string, needle string) int {
	n := 0
	for _, s := range haystack {
		if s == needle {
			n++
		}
	}
	return n
}
