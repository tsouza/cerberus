package tempo_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
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
		sqlStr, args, ok := tempo.BuildTagCatalogValuesSQLForTest("http.method", tempo.AttrMapScopeSpanForTest)
		if !ok {
			t.Fatalf("expected ok=true for attrMapScopeSpan")
		}
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
		sqlStr, args, ok := tempo.BuildTagCatalogValuesSQLForTest("service.name", tempo.AttrMapScopeAnyForTest)
		if !ok {
			t.Fatalf("expected ok=true for attrMapScopeAny")
		}
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
		sqlStr, args, ok := tempo.BuildTagCatalogValuesSQLForTest("exception.type", tempo.AttrMapScopeEventForTest)
		if !ok {
			t.Fatalf("expected ok=true for attrMapScopeEvent")
		}
		if !strings.Contains(sqlStr, "`Scope` = ?") {
			t.Errorf("expected a Scope predicate for a single-scope lookup, got: %s", sqlStr)
		}
		if len(args) != 2 || args[0] != "exception.type" || args[1] != "event" {
			t.Errorf("expected args=[key, scope], got %v", args)
		}
	})

	t.Run("link scope adds Scope predicate", func(t *testing.T) {
		t.Parallel()
		sqlStr, args, ok := tempo.BuildTagCatalogValuesSQLForTest("opentracing.ref_type", tempo.AttrMapScopeLinkForTest)
		if !ok {
			t.Fatalf("expected ok=true for attrMapScopeLink")
		}
		if !strings.Contains(sqlStr, "`Scope` = ?") {
			t.Errorf("expected a Scope predicate for a single-scope lookup, got: %s", sqlStr)
		}
		if len(args) != 2 || args[0] != "opentracing.ref_type" || args[1] != "link" {
			t.Errorf("expected args=[key, scope], got %v", args)
		}
	})

	t.Run("instrumentation scope refuses to build SQL at all", func(t *testing.T) {
		t.Parallel()
		// cerberus issue #3019: the catalog carries no instrumentation-scope
		// arm, so buildTagCatalogValuesSQL now refuses this scope itself
		// (catalogScopeForMapScope's catalogScopeUncovered) instead of
		// depending on tagValuesCatalogEligible having already excluded it
		// upstream — see catalogScopeForMapScope's doc for the full history.
		sqlStr, args, ok := tempo.BuildTagCatalogValuesSQLForTest("service.name", tempo.AttrMapScopeInstrumentationForTest)
		if ok {
			t.Fatalf("expected ok=false for attrMapScopeInstrumentation, got sql=%q args=%v", sqlStr, args)
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

// --- scope=none catalog-hit: event/link buckets, set-equal to live (cerberus milestone M2) ---
//
// TestSearchTags_CatalogHit above only ever drives `scope=resource` —
// the catalog was widened to cover event/link too (cerberus issue
// #2850), but nothing yet pinned that a `scope=none` catalog-HIT answer
// actually surfaces those two buckets, or that it agrees with what the
// live per-scope scan would say for the identical seeded tags. This
// closes that gap.

// tagCatalogNoneSeed seeds the SAME tag keys for all four catalog-covered
// scope buckets, reused by both drivers below: the catalog-hit driver
// (scopeArgCatalogQuerier, keyed by the bound `Scope` arg) and the
// live-scan comparison driver (stubQuerier.stringsBySQL, keyed by the
// live per-scope SQL substring, mirroring TestSearchTagsV2_ScopeFilter's
// own stub). Reusing one seed for both is what makes the eventual
// set-equal assertion meaningful — two independently made-up tag lists
// could "match" only by accident.
var tagCatalogNoneSeed = map[string][]string{
	schema.TagCatalogScopeResource: {"service.name", "service.version"},
	schema.TagCatalogScopeSpan:     {"http.method", "http.route"},
	schema.TagCatalogScopeEvent:    {"exception.type", "exception.message"},
	schema.TagCatalogScopeLink:     {"link.trace_id", "link.kind"},
}

// scopeArgCatalogQuerier answers a tempo_tag_catalog keys lookup
// (buildTagCatalogKeysSQL) per the BOUND `Scope` arg rather than the SQL
// text: every scope bucket's query renders byte-identical SQL text for a
// given scope=none request (only the arg differs — see
// buildTagCatalogKeysSQL's doc comment), so the substring-keyed
// stubQuerier used everywhere else in this file cannot tell the four
// buckets apart. queriedScopes records every scope arg queried, in
// arrival order, so the test can assert the catalog path visited all
// four buckets rather than answering from a subset; nonCatalogSQL
// records anything queried that was NOT the catalog table, so the test
// can assert the request never fell through to the live scan.
type scopeArgCatalogQuerier struct {
	seed          map[string][]string
	queriedScopes []string
	nonCatalogSQL []string
}

func (q *scopeArgCatalogQuerier) Query(context.Context, string, ...any) ([]chclient.Sample, error) {
	return nil, nil
}

func (q *scopeArgCatalogQuerier) QueryStrings(_ context.Context, sql string, args ...any) ([]string, error) {
	if !strings.Contains(sql, "tempo_tag_catalog") {
		q.nonCatalogSQL = append(q.nonCatalogSQL, sql)
		return nil, nil
	}
	scope, _ := args[len(args)-1].(string)
	q.queriedScopes = append(q.queriedScopes, scope)
	return q.seed[scope], nil
}

// tagPair is one (scope, tag) member of a /api/v2/search/tags answer,
// flattened for set comparison — order (of scopes within the response,
// and of tags within a scope) carries no meaning here: a catalog read
// via topKMerge and a live groupUniqArray-style scan have no reason to
// agree on it.
type tagPair struct{ scope, tag string }

func tagPairSet(scopes []tempo.TagScope) map[tagPair]bool {
	out := make(map[tagPair]bool)
	for _, s := range scopes {
		for _, tag := range s.Tags {
			out[tagPair{s.Name, tag}] = true
		}
	}
	return out
}

// assertTagPairSetsEqual fails with the specific missing/unexpected
// (scope, tag) members on either side — set-equal, not order-equal.
func assertTagPairSetsEqual(t *testing.T, got, want map[tagPair]bool) {
	t.Helper()
	for p := range want {
		if !got[p] {
			t.Errorf("catalog-hit answer missing (scope=%q, tag=%q) that the live answer has", p.scope, p.tag)
		}
	}
	for p := range got {
		if !want[p] {
			t.Errorf("catalog-hit answer has (scope=%q, tag=%q) the live answer does not", p.scope, p.tag)
		}
	}
}

// TestSearchTags_CatalogHit_ScopeNone_SetEqualToLive is the M2 milestone
// exit criterion for #2850's Part A: a `scope=none` /api/v2/search/tags
// request must return event AND link buckets on the catalog-HIT path,
// and that catalog-hit answer must be SET-EQUAL to what the live
// per-scope scan would say for the identical seeded tags.
// TestSearchTagsV2_ScopeFilter's none_default/none_explicit cases
// already pin event/link showing up on the LIVE path (see newServer,
// no catalog involved); this is the catalog-path sibling that was
// missing — TestSearchTags_CatalogHit above never varies scope past
// "resource" and never compares against the live answer at all.
//
// Both the default (no `?scope=`) and the explicit `?scope=none` forms
// are covered — parseTagScope collapses them to the identical
// tagScopeNone internally, and TestSearchTagsV2_ScopeFilter draws the
// same none_default/none_explicit distinction on the live path, so the
// catalog path gets the same two-form coverage here.
func TestSearchTags_CatalogHit_ScopeNone_SetEqualToLive(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		query string
	}{
		{"none_default", ""},
		{"none_explicit", "?scope=none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Catalog-hit driver.
			catQ := &scopeArgCatalogQuerier{seed: tagCatalogNoneSeed}
			catSrv := newCatalogServer(catQ, "v1.0.0-test")
			t.Cleanup(catSrv.Close)

			catResp, err := http.Get(catSrv.URL + "/api/v2/search/tags" + tc.query)
			if err != nil {
				t.Fatalf("GET (catalog): %v", err)
			}
			defer catResp.Body.Close()
			if catResp.StatusCode != http.StatusOK {
				t.Fatalf("catalog status=%d body=%s", catResp.StatusCode, readBody(t, catResp))
			}
			var catBody tempo.SearchTagsResponseV2
			if err := json.NewDecoder(catResp.Body).Decode(&catBody); err != nil {
				t.Fatalf("decode (catalog): %v", err)
			}

			// Proves the CATALOG path actually ran, for all four
			// covered buckets, and never fell through to the live
			// scan — otherwise a pass below would prove nothing about
			// the catalog path at all.
			for _, wantScope := range []string{
				schema.TagCatalogScopeResource, schema.TagCatalogScopeSpan,
				schema.TagCatalogScopeEvent, schema.TagCatalogScopeLink,
			} {
				if !contains(catQ.queriedScopes, wantScope) {
					t.Errorf("expected the catalog to be queried for scope %q, queried scopes: %v", wantScope, catQ.queriedScopes)
				}
			}
			if len(catQ.nonCatalogSQL) != 0 {
				t.Errorf("expected every query to hit tempo_tag_catalog, live-scan SQL also ran: %v", catQ.nonCatalogSQL)
			}

			// Live-scan comparison driver: the SAME seeded tags,
			// through the live per-scope columns
			// (TestSearchTagsV2_ScopeFilter's own stub pattern).
			liveQ := &stubQuerier{stringsBySQL: map[string][]string{
				"`ResourceAttributes`":  tagCatalogNoneSeed[schema.TagCatalogScopeResource],
				"`SpanAttributes`":      tagCatalogNoneSeed[schema.TagCatalogScopeSpan],
				"`Events`.`Attributes`": tagCatalogNoneSeed[schema.TagCatalogScopeEvent],
				"`Links`.`Attributes`":  tagCatalogNoneSeed[schema.TagCatalogScopeLink],
			}}
			liveSrv := newServer(liveQ, "v1.0.0-test") // TagCatalogEnabled=false: always live.
			t.Cleanup(liveSrv.Close)

			liveResp, err := http.Get(liveSrv.URL + "/api/v2/search/tags" + tc.query)
			if err != nil {
				t.Fatalf("GET (live): %v", err)
			}
			defer liveResp.Body.Close()
			if liveResp.StatusCode != http.StatusOK {
				t.Fatalf("live status=%d body=%s", liveResp.StatusCode, readBody(t, liveResp))
			}
			var liveBody tempo.SearchTagsResponseV2
			if err := json.NewDecoder(liveResp.Body).Decode(&liveBody); err != nil {
				t.Fatalf("decode (live): %v", err)
			}

			// Sanity: the live comparison answer itself must carry
			// event/link (and intrinsic), or it is not a faithful
			// oracle to compare the catalog answer against.
			for _, wantScope := range []string{"resource", "span", "event", "link", "intrinsic"} {
				found := false
				for _, s := range liveBody.Scopes {
					if s.Name == wantScope {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("live comparison answer missing scope %q entirely: %+v", wantScope, liveBody.Scopes)
				}
			}

			assertTagPairSetsEqual(t, tagPairSet(catBody.Scopes), tagPairSet(liveBody.Scopes))
		})
	}
}
