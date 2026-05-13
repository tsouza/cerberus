package tempo_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/tempo"
)

// TestSearchTags_UnionsMapsAndIntrinsics — the V1 endpoint returns the
// sorted, de-duplicated union of:
//   - keys returned by the ResourceAttributes lookup,
//   - keys returned by the SpanAttributes lookup,
//   - the static intrinsic-span list.
//
// stringsBySQL routes each CH call by the column it qualifies on, so
// the test can give distinct row sets to the two lookups even though
// they share the chclient.QueryStrings entry point.
func TestSearchTags_UnionsMapsAndIntrinsics(t *testing.T) {
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

	for _, want := range []string{
		"service.name", "host", "http.method", // dynamic
		"name", "kind", "status", "duration", // intrinsics
	} {
		if !contains(body.TagNames, want) {
			t.Errorf("missing tag %q in response: %v", want, body.TagNames)
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
// on `null`. Plus the static intrinsic list still surfaces.
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
	// Intrinsics are static — should always be present.
	if !contains(body.TagNames, "name") {
		t.Errorf("expected intrinsic `name` even with empty CH result: %v", body.TagNames)
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
