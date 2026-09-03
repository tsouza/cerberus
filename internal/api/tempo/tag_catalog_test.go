package tempo_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// newCatalogServer is newServer with TagCatalogEnabled=true — the
// cerberus issue #2771 fast path this file exercises.
func newCatalogServer(q tempo.Querier, version string) *testServer {
	return newCatalogServerWithSchema(q, schema.DefaultOTelTraces(), version)
}

// newCatalogServerWithSchema is newCatalogServer for the tests that need
// a traces schema other than the OTel default — e.g. one that points
// ScopeAttributesColumn at a real column (cerberus issue #3010), mirroring
// handler_test.go's newServerWithSchema.
func newCatalogServerWithSchema(q tempo.Querier, s schema.Traces, version string) *testServer {
	h := tempo.New(q, s, version, nil)
	h.TagCatalogEnabled = true
	mux := http.NewServeMux()
	h.Mount(mux)
	return &testServer{Server: httptest.NewServer(mux)}
}

// nonNilFrag is a trivial non-nil chsql.Frag standing in for "a real q=
// narrowing predicate was resolved" in the eligibility unit tests below —
// tagsCatalogEligible / tagValuesCatalogEligible only ever check filter
// for nil-ness, never render it.
func nonNilFrag() chsql.Frag { return func(*chsql.Builder) {} }

// --- Eligibility rule (pure function, no handler round-trip) ---

// TestTagsCatalogEligible pins internal/api/tempo's tagsCatalogEligible
// rule: eligible for the catalog-covered scopes
// (none/resource/span/event/link — cerberus issue #2850 widened the
// catalog to the latter two), a nil filter (no `q=` narrowing), and a
// windowless request. "none" additionally requires the schema to carry
// no instrumentation-scope column (scopeAttrsConfigured == false) — see
// tagsCatalogEligible's doc comment for why.
func TestTagsCatalogEligible(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name                 string
		scope                string
		filter               chsql.Frag
		windowless           bool
		scopeAttrsConfigured bool
		want                 bool
	}{
		{"none scope, no filter, windowless", "none", nil, true, false, true},
		{"resource scope, no filter, windowless", "resource", nil, true, false, true},
		{"span scope, no filter, windowless", "span", nil, true, false, true},
		{"event scope, no filter, windowless", "event", nil, true, false, true},
		{"link scope, no filter, windowless", "link", nil, true, false, true},
		{"instrumentation scope stays live", "instrumentation", nil, true, false, false},
		{"intrinsic scope stays live", "intrinsic", nil, true, false, false},
		{"trace scope stays live", "trace", nil, true, false, false},
		{"filtered request stays live", "none", nonNilFrag(), true, false, false},
		{"explicit window stays live", "none", nil, false, false, false},
		{"filtered AND windowed stays live", "resource", nonNilFrag(), false, false, false},
		{"none scope with instrumentation-scope column configured stays live", "none", nil, true, true, false},
		{"scoped resource request unaffected by instrumentation-scope column", "resource", nil, true, true, true},
		{"scoped event request unaffected by instrumentation-scope column", "event", nil, true, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tempo.TagsCatalogEligibleForTest(tt.scope, tt.filter, tt.windowless, tt.scopeAttrsConfigured); got != tt.want {
				t.Errorf("tagsCatalogEligible(%q, filter=%v, windowless=%v, scopeAttrsConfigured=%v) = %v, want %v",
					tt.scope, tt.filter != nil, tt.windowless, tt.scopeAttrsConfigured, got, tt.want)
			}
		})
	}
}

// TestTagValuesCatalogEligible pins tagValuesCatalogEligible: eligible
// only for a non-intrinsic resolved name, a nil filter, and a windowless
// request — MapScope (resource/span/event/link/any) never restricts
// eligibility, all five are catalog-servable (cerberus issue #2850 added
// event/link). attrMapScopeInstrumentation is the one exception (cerberus
// issue #3010): the catalog MV never carries an instrumentation-scope
// arm at all, so that scope stays off the fast path regardless of filter
// or window — the case below pins it alongside a nil-filter/windowless
// combination that would otherwise be eligible for every other scope.
func TestTagValuesCatalogEligible(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		isIntrinsic bool
		mapScope    tempo.AttrMapScopeForTest
		filter      chsql.Frag
		windowless  bool
		want        bool
	}{
		{"dynamic attribute, any scope, no filter, windowless", false, tempo.AttrMapScopeAnyForTest, nil, true, true},
		{"dynamic attribute, resource scope, no filter, windowless", false, tempo.AttrMapScopeResourceForTest, nil, true, true},
		{"dynamic attribute, span scope, no filter, windowless", false, tempo.AttrMapScopeSpanForTest, nil, true, true},
		{"dynamic attribute, event scope, no filter, windowless", false, tempo.AttrMapScopeEventForTest, nil, true, true},
		{"dynamic attribute, link scope, no filter, windowless", false, tempo.AttrMapScopeLinkForTest, nil, true, true},
		{"dynamic attribute, instrumentation scope stays live even when otherwise eligible", false, tempo.AttrMapScopeInstrumentationForTest, nil, true, false},
		{"intrinsic stays live", true, tempo.AttrMapScopeAnyForTest, nil, true, false},
		{"filtered request stays live", false, tempo.AttrMapScopeAnyForTest, nonNilFrag(), true, false},
		{"explicit window stays live", false, tempo.AttrMapScopeAnyForTest, nil, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			resolved := tempo.ResolvedTagNameForTest(tt.isIntrinsic, "some.key", tt.mapScope)
			if got := tempo.TagValuesCatalogEligibleForTest(resolved, tt.filter, tt.windowless); got != tt.want {
				t.Errorf("tagValuesCatalogEligible(...) = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- SQL shape (no-fmt-Sprintf-on-SQL rule; CLAUDE.md invariant 10) ---

func TestBuildTagCatalogKeysSQL_SQLShape(t *testing.T) {
	t.Parallel()
	sqlStr, args := tempo.BuildTagCatalogKeysSQLForTest("resource")
	if !strings.Contains(sqlStr, "FROM `tempo_tag_catalog`") {
		t.Errorf("expected FROM tempo_tag_catalog, got: %s", sqlStr)
	}
	if !strings.Contains(sqlStr, "GROUP BY `TagKey`") {
		t.Errorf("expected GROUP BY TagKey, got: %s", sqlStr)
	}
	if len(args) != 1 || args[0] != "resource" {
		t.Errorf("expected the scope bound as an arg, got args=%v", args)
	}
}

func TestBuildTagCatalogValuesSQL_SQLShape(t *testing.T) {
	t.Parallel()

	t.Run("single scope adds Scope predicate", func(t *testing.T) {
		t.Parallel()
		sqlStr, args := tempo.BuildTagCatalogValuesSQLForTest("http.method", tempo.AttrMapScopeSpanForTest)
		if !strings.Contains(sqlStr, "topKMerge(50)(`TopValuesState`)") {
			t.Errorf("expected topKMerge(50)(...), got: %s", sqlStr)
		}
		if !strings.Contains(sqlStr, "`Scope` = ?") {
			t.Errorf("expected a Scope predicate for a single-scope lookup, got: %s", sqlStr)
		}
		if len(args) != 2 || args[0] != "http.method" || args[1] != "span" {
			t.Errorf("expected args=[key, scope], got %v", args)
		}
	})

	t.Run("any scope filters Scope IN (resource, span), not event/link", func(t *testing.T) {
		t.Parallel()
		// cerberus issue #2850: since the catalog now also carries
		// event/link rows, auto-scope must filter explicitly rather than
		// omit the Scope predicate — an unfiltered read would silently
		// widen the merge to include event/link states too, which
		// auto-scope has never meant (see resolveTagName).
		sqlStr, args := tempo.BuildTagCatalogValuesSQLForTest("service.name", tempo.AttrMapScopeAnyForTest)
		if strings.Contains(sqlStr, "`Scope` = ?") {
			t.Errorf("auto-scope lookup must use an IN-list, not an equality predicate, got: %s", sqlStr)
		}
		if !strings.Contains(sqlStr, "`Scope` IN (?, ?)") {
			t.Errorf("expected an explicit Scope IN-list, got: %s", sqlStr)
		}
		if len(args) != 3 || args[0] != "service.name" || args[1] != "resource" || args[2] != "span" {
			t.Errorf("expected args=[key, resource, span], got %v", args)
		}
	})

	t.Run("event scope adds Scope predicate", func(t *testing.T) {
		t.Parallel()
		sqlStr, args := tempo.BuildTagCatalogValuesSQLForTest("exception.type", tempo.AttrMapScopeEventForTest)
		if !strings.Contains(sqlStr, "`Scope` = ?") {
			t.Errorf("expected a Scope predicate for a single-scope lookup, got: %s", sqlStr)
		}
		if len(args) != 2 || args[0] != "exception.type" || args[1] != "event" {
			t.Errorf("expected args=[key, scope], got %v", args)
		}
	})

	t.Run("link scope adds Scope predicate", func(t *testing.T) {
		t.Parallel()
		sqlStr, args := tempo.BuildTagCatalogValuesSQLForTest("opentracing.ref_type", tempo.AttrMapScopeLinkForTest)
		if !strings.Contains(sqlStr, "`Scope` = ?") {
			t.Errorf("expected a Scope predicate for a single-scope lookup, got: %s", sqlStr)
		}
		if len(args) != 2 || args[0] != "opentracing.ref_type" || args[1] != "link" {
			t.Errorf("expected args=[key, scope], got %v", args)
		}
	})
}

// --- HTTP-level wiring: catalog hit / miss / disabled / ineligible ---

func TestSearchTags_CatalogHit(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{stringsBySQL: map[string][]string{
		"tempo_tag_catalog": {"service.name", "k8s.pod.name"},
	}}
	srv := newCatalogServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v2/search/tags?scope=resource")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	if !strings.Contains(q.lastSQL, "tempo_tag_catalog") {
		t.Errorf("expected the catalog SQL to have run, last SQL was: %s", q.lastSQL)
	}
	if strings.Contains(q.lastSQL, "ResourceAttributes") {
		t.Errorf("catalog hit must NOT fall through to the live attribute-map scan, last SQL was: %s", q.lastSQL)
	}
	var body tempo.SearchTagsResponseV2
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Scopes) != 1 || body.Scopes[0].Name != "resource" {
		t.Fatalf("unexpected scopes: %+v", body.Scopes)
	}
	if !contains(body.Scopes[0].Tags, "service.name") || !contains(body.Scopes[0].Tags, "k8s.pod.name") {
		t.Errorf("expected catalog-sourced tags in response: %+v", body.Scopes[0])
	}
}

func TestSearchTags_CatalogMiss_FallsBackToLive(t *testing.T) {
	t.Parallel()
	// The catalog query for BOTH scope buckets returns zero rows (table
	// provisioned but never refreshed, or simply empty) — see
	// tagsFromCatalog's doc: an empty combined result is treated the same
	// as a miss, exactly like internal/api/loki's detectedLabelsFromCatalog.
	q := &stubQuerier{stringsBySQL: map[string][]string{
		"tempo_tag_catalog":    {},
		"`ResourceAttributes`": {"service.name"},
		"`SpanAttributes`":     {"http.method"},
	}}
	srv := newCatalogServer(q, "v1.0.0-test")
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
	if !contains(body.TagNames, "service.name") || !contains(body.TagNames, "http.method") {
		t.Errorf("expected the fallback live scan's tags in response: %v", body.TagNames)
	}
}

func TestSearchTags_CatalogDisabled_UsesLiveUnconditionally(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{stringsBySQL: map[string][]string{
		"tempo_tag_catalog":    {"should-never-surface"},
		"`ResourceAttributes`": {"service.name"},
	}}
	srv := newServer(q, "v1.0.0-test") // TagCatalogEnabled=false (zero value)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search/tags?scope=resource")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var body tempo.SearchTagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if contains(body.TagNames, "should-never-surface") {
		t.Errorf("TagCatalogEnabled=false must never query the catalog table, got: %v", body.TagNames)
	}
}

func TestSearchTags_ExplicitWindow_StaysOnLivePath(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{stringsBySQL: map[string][]string{
		"tempo_tag_catalog":    {"should-never-surface"},
		"`ResourceAttributes`": {"service.name"},
	}}
	srv := newCatalogServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	// An explicit start/end (a real historical window, not a
	// datasource-open-probe) must never be served from the catalog's own
	// narrower trailing window — see tagsCatalogEligible's doc comment.
	resp, err := http.Get(srv.URL + "/api/search/tags?scope=resource&start=1700000000&end=1700003600")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var body tempo.SearchTagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if contains(body.TagNames, "should-never-surface") {
		t.Errorf("an explicit window must stay on the live path, got: %v", body.TagNames)
	}
}

func TestSearchTagValues_CatalogHit(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{stringsBySQL: map[string][]string{
		"tempo_tag_catalog": {"GET", "POST"},
	}}
	srv := newCatalogServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search/tag/span.http.method/values")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if !strings.Contains(q.lastSQL, "tempo_tag_catalog") {
		t.Errorf("expected the catalog SQL to have run, last SQL was: %s", q.lastSQL)
	}
	var body tempo.SearchTagValuesResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !contains(body.TagValues, "GET") || !contains(body.TagValues, "POST") {
		t.Errorf("expected catalog-sourced values in response: %v", body.TagValues)
	}
}

func TestSearchTagValues_Intrinsic_NeverUsesCatalog(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{stringsBySQL: map[string][]string{
		"tempo_tag_catalog": {"should-never-surface"},
	}, strings: []string{"Ok"}}
	srv := newCatalogServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search/tag/status/values")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if strings.Contains(q.lastSQL, "tempo_tag_catalog") {
		t.Errorf("an intrinsic lookup must never query the catalog table, last SQL was: %s", q.lastSQL)
	}
}

func TestSearchTagValues_WithQFilter_StaysOnLivePath(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{stringsBySQL: map[string][]string{
		"tempo_tag_catalog": {"should-never-surface"},
		"mapContains":       {"GET"},
	}}
	srv := newCatalogServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	// {span.http.status_code=200} is a real, lowerable span-row predicate
	// — tagQueryFilter resolves it to a non-nil Frag, so eligibility must
	// reject the catalog regardless of every other condition. This is the
	// filtered-tag-values-stays-live prediction cerberus issue #2771 asked
	// to be verified, not assumed.
	resp, err := http.Get(srv.URL + "/api/v2/search/tag/span.http.method/values?q=" +
		`{span.http.status_code=200}`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	if strings.Contains(q.lastSQL, "tempo_tag_catalog") {
		t.Errorf("a q= filtered lookup must stay on the live path, last SQL was: %s", q.lastSQL)
	}
}
