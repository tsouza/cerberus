package tempo_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/tempo"
)

// TestSearchTagValues_Intrinsic_Name — the path `/api/search/tag/name/values`
// maps to the SpanName CH column. The SQL must qualify that column via
// toString (so the CH binder happily decodes any underlying type) and
// it must NOT reach into SpanAttributes / ResourceAttributes.
func TestSearchTagValues_Intrinsic_Name(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{strings: []string{"GET /a", "GET /b"}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search/tag/name/values")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var body tempo.SearchTagValuesResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := body.TagValues; len(got) != 2 || got[0] != "GET /a" || got[1] != "GET /b" {
		t.Errorf("unexpected values: %v", got)
	}
	if !strings.Contains(q.lastSQL, "toString(`SpanName`)") {
		t.Errorf("expected intrinsic projection on SpanName, got: %s", q.lastSQL)
	}
	if strings.Contains(q.lastSQL, "SpanAttributes") {
		t.Errorf("intrinsic path should NOT query attribute maps, got: %s", q.lastSQL)
	}
}

// TestSearchTagValues_Intrinsic_AllMapped — every entry in the
// intrinsics list resolves to a column the schema knows about. Guards
// against drift between the intrinsicTags list (used by /search/tags)
// and the intrinsicColumn lookup (used by /search/tag/<n>/values).
func TestSearchTagValues_Intrinsic_AllMapped(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"name", "kind", "status", "statusMessage", "duration", "parent"} {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q := &stubQuerier{strings: []string{"a"}}
			srv := newServer(q, "v1.0.0-test")
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + "/api/search/tag/" + name + "/values")
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			resp.Body.Close()
			if !strings.Contains(q.lastSQL, "toString(`") {
				t.Errorf("intrinsic %q should hit toString column path, got: %s", name, q.lastSQL)
			}
		})
	}
}

// TestSearchTagValues_DynamicAttribute — a non-intrinsic key fans the
// row out across both maps. The SQL must:
//   - bind the key as a `?` arg (NOT splice as a literal),
//   - reference both SpanAttributes and ResourceAttributes,
//   - drop the empty-string slot via the outer v != ” predicate.
func TestSearchTagValues_DynamicAttribute(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{strings: []string{"frontend", "backend"}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search/tag/service.name/values")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var body tempo.SearchTagValuesResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.TagValues) != 2 {
		t.Errorf("expected 2 values, got %v", body.TagValues)
	}
	// Sorted (alphabetical).
	if body.TagValues[0] != "backend" || body.TagValues[1] != "frontend" {
		t.Errorf("expected sorted (backend, frontend), got %v", body.TagValues)
	}
	for _, want := range []string{
		"`SpanAttributes`[",
		"`ResourceAttributes`[",
		"arrayJoin(",
		"mapContains(",
		"`v` != ?",
	} {
		if !strings.Contains(q.lastSQL, want) {
			t.Errorf("SQL missing %q\n  got: %s", want, q.lastSQL)
		}
	}
	// Tag name flows as a positional arg, not spliced. There are three
	// `?` slots: SpanAttributes[?], ResourceAttributes[?] (inside
	// arrayJoin), and the two mapContains(?) — plus the outer v != ''.
	// All four key-related args must equal the URL path tag name.
	hits := 0
	for _, a := range q.lastArgs {
		if s, ok := a.(string); ok && s == "service.name" {
			hits++
		}
	}
	if hits < 4 {
		t.Errorf("expected tag name bound >=4 times as positional arg, got %d in %v", hits, q.lastArgs)
	}
}

// TestSearchTagValues_EmptyName — bare `/api/search/tag//values` URL
// (empty name segment) returns 400.
//
// Note: net/http's ServeMux refuses to install a pattern matching
// /api/search/tag//values, so this test posts to a path that the
// router won't dispatch; we just want a non-OK response. The
// surrounding 404 from the mux still proves the handler isn't called
// with an empty name.
func TestSearchTagValues_EmptyName(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search/tag//values")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("expected non-OK for empty tag name, got 200")
	}
}

// TestSearchTagValues_CHFailure — CH error bubbles up as 502.
func TestSearchTagValues_CHFailure(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{err: errors.New("clickhouse: connection refused")}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search/tag/service.name/values")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d want 502", resp.StatusCode)
	}
}

// TestSearchTagValuesV2_Envelope — V2 wraps each value in a typed
// object. For dynamic attributes the type is "string"; for intrinsics
// it switches on the kind (duration / status / kind / string).
func TestSearchTagValuesV2_Envelope(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{strings: []string{"frontend"}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v2/search/tag/service.name/values")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var body tempo.SearchTagValuesResponseV2
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.TagValues) != 1 || body.TagValues[0].Value != "frontend" || body.TagValues[0].Type != "string" {
		t.Errorf("unexpected V2 body: %+v", body.TagValues)
	}
}

// TestSearchTagValuesV2_IntrinsicType — duration's V2 type label is
// "duration", not "string". Pin the dispatch so future intrinsic
// additions don't quietly regress.
func TestSearchTagValuesV2_IntrinsicType(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{strings: []string{"150000000"}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v2/search/tag/duration/values")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var body tempo.SearchTagValuesResponseV2
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.TagValues) != 1 || body.TagValues[0].Type != "duration" {
		t.Errorf("expected type=duration, got %+v", body.TagValues)
	}
}

// TestSearchTagValuesV2_LeadingDotAutoScope — the scoped leading-dot
// form `.service.name` is the canonical TraceQL identifier shape that
// Tempo's V2 URL parser accepts. Cerberus must resolve it to the bare
// attribute key (`service.name`) and query both attribute maps (Tempo's
// auto-scope semantics). Compatibility-harness fixture uses this form.
func TestSearchTagValuesV2_LeadingDotAutoScope(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{strings: []string{"frontend", "backend"}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v2/search/tag/.service.name/values")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var body tempo.SearchTagValuesResponseV2
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.TagValues) != 2 {
		t.Errorf("expected 2 values, got %v", body.TagValues)
	}
	// Auto-scope must touch both maps via arrayJoin, identical to the
	// bare-name V1 dynamic-attribute path.
	for _, want := range []string{
		"`SpanAttributes`[",
		"`ResourceAttributes`[",
		"arrayJoin(",
	} {
		if !strings.Contains(q.lastSQL, want) {
			t.Errorf("SQL missing %q for `.service.name`\n  got: %s", want, q.lastSQL)
		}
	}
	// The bound key is the bare attribute name, NOT the leading-dot
	// form — the parser strips the scope sigil. Mirrors Tempo's
	// behaviour: `traceql.ParseIdentifier(".service.name")` yields
	// Attribute{Scope: None, Name: "service.name"}.
	hits := 0
	for _, a := range q.lastArgs {
		if s, ok := a.(string); ok && s == "service.name" {
			hits++
		}
	}
	if hits < 4 {
		t.Errorf("expected key %q bound >=4 times, got %d in %v",
			"service.name", hits, q.lastArgs)
	}
}

// TestSearchTagValuesV2_ResourceScope — `resource.service.name` is the
// explicit-scope TraceQL form. Cerberus must narrow the lookup to the
// ResourceAttributes column only (no SpanAttributes leg in the SQL).
func TestSearchTagValuesV2_ResourceScope(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{strings: []string{"frontend"}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v2/search/tag/resource.service.name/values")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	if !strings.Contains(q.lastSQL, "`ResourceAttributes`[") {
		t.Errorf("expected ResourceAttributes lookup, got: %s", q.lastSQL)
	}
	if strings.Contains(q.lastSQL, "`SpanAttributes`[") {
		t.Errorf("resource scope should NOT touch SpanAttributes, got: %s", q.lastSQL)
	}
	if strings.Contains(q.lastSQL, "arrayJoin(") {
		t.Errorf("single-scope path should NOT emit arrayJoin union, got: %s", q.lastSQL)
	}
	// Bound key is the bare attribute name after stripping the scope.
	hits := 0
	for _, a := range q.lastArgs {
		if s, ok := a.(string); ok && s == "service.name" {
			hits++
		}
	}
	if hits < 2 {
		t.Errorf("expected key %q bound >=2 times, got %d in %v",
			"service.name", hits, q.lastArgs)
	}
}

// TestSearchTagValuesV2_SpanScope — `span.http.method` narrows to the
// SpanAttributes column only.
func TestSearchTagValuesV2_SpanScope(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{strings: []string{"GET", "POST"}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v2/search/tag/span.http.method/values")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	if !strings.Contains(q.lastSQL, "`SpanAttributes`[") {
		t.Errorf("expected SpanAttributes lookup, got: %s", q.lastSQL)
	}
	if strings.Contains(q.lastSQL, "`ResourceAttributes`[") {
		t.Errorf("span scope should NOT touch ResourceAttributes, got: %s", q.lastSQL)
	}
	// Bound key is the bare attribute name after stripping the scope.
	hits := 0
	for _, a := range q.lastArgs {
		if s, ok := a.(string); ok && s == "http.method" {
			hits++
		}
	}
	if hits < 2 {
		t.Errorf("expected key %q bound >=2 times, got %d in %v",
			"http.method", hits, q.lastArgs)
	}
}

// TestSearchTagValues_ScopedFormParityWithBare — the scoped
// `.service.name` form (Tempo-acceptable) and the bare `service.name`
// form (V1 backward-compat) MUST produce equivalent SQL: identical
// arrayJoin shape over both maps with the same bare key bound as the
// positional arg. This is the cross-form parity contract the
// compatibility-harness fixture relies on — Tempo only accepts the
// scoped form on V2, but cerberus's two paths must converge on the
// same data.
func TestSearchTagValues_ScopedFormParityWithBare(t *testing.T) {
	t.Parallel()
	for _, urlPath := range []string{
		"/api/search/tag/service.name/values",
		"/api/search/tag/.service.name/values",
		"/api/v2/search/tag/service.name/values",
		"/api/v2/search/tag/.service.name/values",
	} {
		urlPath := urlPath
		t.Run(urlPath, func(t *testing.T) {
			t.Parallel()
			q := &stubQuerier{strings: []string{"frontend", "backend"}}
			srv := newServer(q, "v1.0.0-test")
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + urlPath)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d for %s", resp.StatusCode, urlPath)
			}
			// All four URLs land on the auto-scope arrayJoin path.
			for _, want := range []string{
				"`SpanAttributes`[",
				"`ResourceAttributes`[",
				"arrayJoin(",
			} {
				if !strings.Contains(q.lastSQL, want) {
					t.Errorf("%s: SQL missing %q\n  got: %s", urlPath, want, q.lastSQL)
				}
			}
			// Bound key is the bare `service.name`, regardless of which
			// URL form the caller used.
			hits := 0
			for _, a := range q.lastArgs {
				if s, ok := a.(string); ok && s == "service.name" {
					hits++
				}
			}
			if hits < 4 {
				t.Errorf("%s: expected key bound >=4 times, got %d in %v",
					urlPath, hits, q.lastArgs)
			}
		})
	}
}
