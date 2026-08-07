package tempo_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
)

// This file pins the optional `?q=<TraceQL>` narrowing filter on the
// tag-NAME discovery routes (#1820), and the asymmetry between them:
// `q` is a V2 parameter. Grafana's autocomplete sends it as soon as the
// user has typed part of a query, and cerberus used to ignore it and
// answer the same window-wide key set every time, so the completion list
// was wider than the data it was completing against.
//
// V1 answers that window-wide key set by design. Upstream Tempo's API
// reference documents `q` under "Search tags V2" only, and its V1 route
// drops the parameter rather than rejecting it — LiveStore.SearchTags
// forwards nothing but the scope into SearchTagsV2, so the query is
// empty by the time the blocks are read. A V1 request therefore answers
// 200 with the unfiltered keys whatever `q` says, and the V1 half of the
// table below asserts exactly that.

// The two tag-NAME routes, named because the tests below split on which
// one honours `q`.
const (
	tagsRouteV1Path = "/api/search/tags"
	tagsRouteV2Path = "/api/v2/search/tags"
)

// tagsRoutes is the single enumeration of the tag-NAME routes. A third
// one added without joining this list is a missing row.
var tagsRoutes = []string{tagsRouteV1Path, tagsRouteV2Path}

// The window every case is driven over. Explicit bounds matter: a
// windowless request is clamped to a now-relative recent window, whose
// rendered timestamps differ between two calls, and the byte-identical
// SQL assertion below needs the two renders to be comparable.
var (
	tagsFilterStart = time.Date(2026, 5, 16, 14, 0, 0, 0, time.UTC)
	tagsFilterEnd   = tagsFilterStart.Add(time.Hour)
)

// tagsURL builds the request for one route. A `hasQ` of false omits the
// parameter entirely, which is the "behaviour before `q` existed" case.
func tagsURL(route, scope, q string, hasQ bool) string {
	vals := url.Values{}
	vals.Set("scope", scope)
	vals.Set("start", strconv.FormatInt(tagsFilterStart.Unix(), 10))
	vals.Set("end", strconv.FormatInt(tagsFilterEnd.Unix(), 10))
	if hasQ {
		vals.Set("q", q)
	}
	return route + "?" + vals.Encode()
}

// tagsLookup drives one route and reports the status, the response body,
// and the last SQL + args the handler sent to ClickHouse. `scope=span`
// keeps it to a single lookup so lastSQL is unambiguous.
func tagsLookup(t *testing.T, q string, hasQ bool, route string, keys map[string][]string) (int, string, string, []any) {
	t.Helper()
	stub := &stubQuerier{strings: []string{"http.method"}, stringsBySQL: keys}
	srv := newServer(stub, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + tagsURL(route, "span", q, hasQ))
	if err != nil {
		t.Fatalf("GET %s: %v", route, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readBody(t, resp), stub.lastSQL, stub.lastArgs
}

// TestSearchTagsQuery_NarrowsLookupSQL is the #1820 regression: a `q`
// must reach the per-scope key lookup as one more WHERE conjunct over
// the same window, with its operand values bound as parameters rather
// than spliced into the SQL text.
func TestSearchTagsQuery_NarrowsLookupSQL(t *testing.T) {
	t.Parallel()

	const query = `{ span.http.method = "GET" }`

	base, _, baseSQL, baseArgs := tagsLookup(t, "", false, tagsRouteV2Path, nil)
	if base != http.StatusOK {
		t.Fatalf("unfiltered lookup: status=%d", base)
	}
	status, body, gotSQL, gotArgs := tagsLookup(t, query, true, tagsRouteV2Path, nil)
	if status != http.StatusOK {
		t.Fatalf("filtered lookup: status=%d body=%s", status, body)
	}
	// The filter is appended to the window conjuncts, never a
	// replacement for them: the narrowed SQL is the unfiltered SQL
	// plus one trailing AND. Asserting the prefix pins both facts at
	// once — the window survives, and nothing else moved.
	if !strings.HasPrefix(gotSQL, baseSQL+" AND ") {
		t.Fatalf("narrowed SQL is not the unfiltered SQL plus one conjunct\n  base: %s\n   got: %s", baseSQL, gotSQL)
	}
	extra := strings.TrimPrefix(gotSQL, baseSQL+" AND ")
	if !strings.Contains(extra, "`SpanAttributes`") {
		t.Errorf("added conjunct does not read the span attribute map: %s", extra)
	}
	// The literal rides as a bound parameter (invariant 10: no SQL
	// text carries user values).
	if strings.Contains(gotSQL, "GET") {
		t.Errorf("query literal spliced into SQL text: %s", gotSQL)
	}
	if !argsBind(gotArgs, "http.method", "GET") {
		t.Errorf("args do not bind the attribute key and value: %v (base args %v)", gotArgs, baseArgs)
	}
}

// TestSearchTagsV1Query_Ignored is the parity fact the V2 tests above
// cannot state: V1 does not take `q`, so every shape of it — one that
// would narrow, one that cannot be parsed, one that lowers to something
// no row predicate describes — answers 200 with the same key set and the
// same SQL a request without `q` gets.
//
// The narrowing case is the compatibility corpus case
// tags_v1_scoped_by_query in miniature: reference Tempo answers
// `{ span.http.method = "GET" }` on V1 with the whole window's keys,
// including the ones carried only by spans that query does not select.
func TestSearchTagsV1Query_Ignored(t *testing.T) {
	t.Parallel()

	// Answered only when the lookup carries the narrowing conjunct, so a
	// V1 route that pushed `q` down would return this shorter set.
	narrowed := map[string][]string{"`SpanAttributes`[?] = ?": {"http.method"}}
	// Carried by spans `{ span.http.method = "GET" }` does not select.
	const unfiltered = "child.index"

	baseStatus, baseBody, baseSQL, _ := tagsLookup(t, "", false, tagsRouteV1Path, narrowed)
	if baseStatus != http.StatusOK {
		t.Fatalf("unfiltered lookup: status=%d body=%s", baseStatus, baseBody)
	}

	for name, query := range map[string]string{
		"narrowing":        `{ span.http.method = "GET" }`,
		"malformed":        "{{{",
		"metrics pipeline": `{} | rate()`,
		"structural":       `{ span.a = 1 } >> { span.b = 2 }`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := &stubQuerier{
				strings:      []string{"http.method", unfiltered},
				stringsBySQL: narrowed,
			}
			srv := newServer(stub, "v1.0.0-test")
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + tagsURL(tagsRouteV1Path, "span", query, true))
			if err != nil {
				t.Fatalf("GET %s: %v", tagsRouteV1Path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status: want 200, got %d body=%s", resp.StatusCode, readBody(t, resp))
			}
			got := decodeTagNames(t, tagsRouteV1Path, resp)
			if !contains(got, unfiltered) {
				t.Errorf("V1 narrowed its answer on q=%q: %v", query, got)
			}
			if stub.lastSQL != baseSQL {
				t.Errorf("V1 q=%q changed the SQL\n  base: %s\n   got: %s", query, baseSQL, stub.lastSQL)
			}
		})
	}
}

// TestSearchTagsQuery_AbsentRendersIdenticalSQL is the other half of the
// contract: `q` is opt-in, so a request that omits it — and a request
// that sends it empty, which is what Grafana does before the user has
// typed anything — must render byte-identical SQL to the pre-#1820
// handler. A filter that leaked a no-op `AND 1` would change part
// pruning on a table this endpoint already has to be careful with.
func TestSearchTagsQuery_AbsentRendersIdenticalSQL(t *testing.T) {
	t.Parallel()

	for _, route := range tagsRoutes {
		t.Run(route, func(t *testing.T) {
			t.Parallel()
			_, _, absentSQL, absentArgs := tagsLookup(t, "", false, route, nil)
			_, _, emptySQL, emptyArgs := tagsLookup(t, "", true, route, nil)
			if emptySQL != absentSQL {
				t.Errorf("empty q changed the SQL\n  absent: %s\n   empty: %s", absentSQL, emptySQL)
			}
			if len(emptyArgs) != len(absentArgs) {
				t.Errorf("empty q changed the bound args: %v vs %v", emptyArgs, absentArgs)
			}
		})
	}
}

// TestSearchTagsQuery_NarrowsTheAnswer drives the narrowing end to end:
// the stub answers the narrowed lookup with a strictly smaller key set
// than the unfiltered one, and the assertion is that the RESPONSE — not
// just the SQL — carries the smaller set. This is the in-process twin of
// the compatibility corpus case tags_v2_span_scoped_by_query, which pins
// the same subset relation against reference Tempo.
func TestSearchTagsQuery_NarrowsTheAnswer(t *testing.T) {
	t.Parallel()

	const query = `{ span.http.method = "GET" }`
	// Keyed on the conjunct only the filtered lookup carries.
	keys := map[string][]string{
		"`SpanAttributes`[?] = ?": {"http.method", "http.target"},
	}
	const unfiltered = "child.index"

	stub := &stubQuerier{
		strings:      []string{"http.method", "http.target", unfiltered},
		stringsBySQL: keys,
	}
	srv := newServer(stub, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + tagsURL(tagsRouteV2Path, "span", query, true))
	if err != nil {
		t.Fatalf("GET %s: %v", tagsRouteV2Path, err)
	}
	defer resp.Body.Close()
	got := decodeTagNames(t, tagsRouteV2Path, resp)
	if len(got) == 0 {
		t.Fatal("no keys returned")
	}
	for _, k := range []string{"http.method", "http.target"} {
		if !contains(got, k) {
			t.Errorf("narrowed answer dropped %q: %v", k, got)
		}
	}
	if contains(got, unfiltered) {
		t.Errorf("key %q the query does not select leaked into the answer: %v", unfiltered, got)
	}
}

// TestSearchTagsQuery_Malformed pins the parse rejection. `q` is TraceQL,
// so an unparseable one is a 400 carrying the same parse-stage message
// /api/search answers for the identical input — one classification, two
// routes (errclass.go). V1 has no `q` to reject and is covered by
// TestSearchTagsV1Query_Ignored instead.
func TestSearchTagsQuery_Malformed(t *testing.T) {
	t.Parallel()

	status, body, sql, _ := tagsLookup(t, "{{{", true, tagsRouteV2Path, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d body=%s", status, body)
	}
	if !strings.Contains(body, "parse") {
		t.Errorf("body does not name the parse stage: %s", body)
	}
	if sql != "" {
		t.Errorf("a rejected q still hit ClickHouse: %s", sql)
	}
}

// TestSearchTagsQuery_NonRowShapeRejected covers the queries that parse
// and lower but whose matching spans are defined by a join or an
// aggregate rather than a predicate over one row. There is no conjunct
// to push into a key lookup for those, and answering the unfiltered key
// set would be a silently WIDER answer than the caller asked for — so
// they are refused with a 422 that names why. V2 only: V1 promises the
// unfiltered set to begin with, so there is nothing to refuse.
func TestSearchTagsQuery_NonRowShapeRejected(t *testing.T) {
	t.Parallel()

	queries := map[string]string{
		"metrics pipeline": `{} | rate()`,
		"structural":       `{ span.a = 1 } >> { span.b = 2 }`,
	}
	for name, query := range queries {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			status, body, sql, _ := tagsLookup(t, query, true, tagsRouteV2Path, nil)
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("status: want 422, got %d body=%s", status, body)
			}
			if !strings.Contains(body, "row predicate") {
				t.Errorf("body does not explain the shape requirement: %s", body)
			}
			if sql != "" {
				t.Errorf("a rejected q still hit ClickHouse: %s", sql)
			}
		})
	}
}

// decodeTagNames flattens either envelope onto the key list, so the
// route table above can assert on both with one comparison.
func decodeTagNames(t *testing.T, route string, resp *http.Response) []string {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status=%d", route, resp.StatusCode)
	}
	if strings.Contains(route, "/v2/") {
		var body tempo.SearchTagsResponseV2
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode V2: %v", err)
		}
		var out []string
		for _, s := range body.Scopes {
			out = append(out, s.Tags...)
		}
		return out
	}
	var body tempo.SearchTagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode V1: %v", err)
	}
	return body.TagNames
}

// argsBind reports whether every want appears among the bound args.
func argsBind(args []any, want ...string) bool {
	for _, w := range want {
		found := false
		for _, a := range args {
			if s, ok := a.(string); ok && s == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
