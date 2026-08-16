package tempo_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/tempo"
)

// This file pins the optional `?q=<TraceQL>` narrowing filter on the
// tag-VALUE discovery routes (#1932), and the asymmetry between them:
// like the tag-NAME pair, `q` is a V2 parameter.
//
// The value routes back the VALUE half of Grafana's tag autocomplete —
// the dropdown the user is mid-way through completing — which is where
// the unfiltered answer is both the largest set and the least useful
// one: every value the key takes anywhere in the window, rather than the
// values it takes on the spans the query already selects. The name
// routes learned to narrow in #1820; the value routes were not in that
// change's scope and kept answering the whole window.
//
// V1 answers that window-wide value set by design, and upstream is
// unusually explicit about it: `q` is parsed for BOTH value routes by
// one shared parser and even re-emitted onto the sub-requests, so it
// physically reaches the queriers on V1 — and then dies at every leaf,
// because each V1 executor takes a bare tag name with no slot for a
// filter. Only the V2 executors extract condition groups from the query.
// The V1 half of the tables below asserts exactly that.

// The two tag-VALUE routes, named because the tests below split on which
// one honours `q`. Each carries a `%s` for the {name} path segment.
const (
	tagValuesRouteV1Path = "/api/search/tag/%s/values"
	tagValuesRouteV2Path = "/api/v2/search/tag/%s/values"
)

// tagValuesRoutes is the single enumeration of the tag-VALUE routes. A
// third one added without joining this list is a missing row.
var tagValuesRoutes = []string{tagValuesRouteV1Path, tagValuesRouteV2Path}

// The tag whose values every case below enumerates, and the narrowing
// query. `.child.index` is the leading-dot auto-scope form, so the
// lookup takes the dynamic-attribute branch (the arrayJoin subquery over
// both maps) — the shape where the narrowing conjunct has to land in the
// INNER query to be able to see the span row at all.
const (
	tagValuesKey   = ".child.index"
	tagValuesQuery = `{ span.http.method = "GET" }`
	// tagValuesIntrinsicKey takes the OTHER values-SQL branch: a
	// dedicated column read by a flat DISTINCT projection, with no
	// subquery for the conjunct to land inside.
	tagValuesIntrinsicKey = "name"
	// tagValuesConjunct is the rendered shape of tagValuesQuery's
	// predicate. It appears in the lookup SQL only when the filter was
	// applied: the unfiltered dynamic-attribute SQL reads the same map
	// column, but as a subscript in the projection and inside
	// mapContains, never as an equality.
	tagValuesConjunct = "`SpanAttributes`[?] = ?"
	// tagValuesOuterWhere is the outer query's empty-value filter. The
	// narrowing conjunct must appear BEFORE it, which is what proves it
	// landed in the inner query rather than being applied to the already
	// exploded value column.
	tagValuesOuterWhere = "`v` != ?"
)

// tagValuesURL builds the request for one route. A `hasQ` of false omits
// the parameter entirely, which is the "behaviour before `q` existed"
// case. The window is the shared tagsFilter* pair from
// search_tags_filter_test.go: explicit bounds keep two renders of the
// same request byte-comparable, which the identical-SQL assertions need.
func tagValuesURL(route, name, q string, hasQ bool) string {
	vals := url.Values{}
	vals.Set("start", strconv.FormatInt(tagsFilterStart.Unix(), 10))
	vals.Set("end", strconv.FormatInt(tagsFilterEnd.Unix(), 10))
	if hasQ {
		vals.Set("q", q)
	}
	return strings.Replace(route, "%s", url.PathEscape(name), 1) + "?" + vals.Encode()
}

// tagValuesLookup drives one route and reports the status, the response
// body, and the last SQL + args the handler sent to ClickHouse. A
// tag-values request issues exactly one CH query, so lastSQL is
// unambiguous without any scope narrowing.
func tagValuesLookup(t *testing.T, name, q string, hasQ bool, route string, byS map[string][]string) (int, string, string, []any) {
	t.Helper()
	stub := &stubQuerier{strings: []string{"0"}, stringsBySQL: byS}
	srv := newServer(stub, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + tagValuesURL(route, name, q, hasQ))
	if err != nil {
		t.Fatalf("GET %s: %v", route, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readBody(t, resp), stub.lastSQL, stub.lastArgs
}

// TestSearchTagValuesQuery_NarrowsLookupSQL is the #1932 regression on
// the dynamic-attribute branch: a `q` must reach the value lookup as one
// more WHERE conjunct over the same window, inside the subquery that
// still has the span row in scope, with its operand values bound as
// parameters rather than spliced into the SQL text.
func TestSearchTagValuesQuery_NarrowsLookupSQL(t *testing.T) {
	t.Parallel()

	base, _, baseSQL, baseArgs := tagValuesLookup(t, tagValuesKey, "", false, tagValuesRouteV2Path, nil)
	if base != http.StatusOK {
		t.Fatalf("unfiltered lookup: status=%d", base)
	}
	status, body, gotSQL, gotArgs := tagValuesLookup(t, tagValuesKey, tagValuesQuery, true, tagValuesRouteV2Path, nil)
	if status != http.StatusOK {
		t.Fatalf("filtered lookup: status=%d body=%s", status, body)
	}

	// The unfiltered lookup reads the same map column, so the assertion
	// has to be that the EQUALITY appeared — not merely the column.
	if strings.Contains(baseSQL, tagValuesConjunct) {
		t.Fatalf("unfiltered SQL already carries the narrowing conjunct, the test proves nothing: %s", baseSQL)
	}
	if !strings.Contains(gotSQL, tagValuesConjunct) {
		t.Fatalf("narrowed SQL does not read the span attribute map as a predicate\n  base: %s\n   got: %s", baseSQL, gotSQL)
	}
	// Inside the inner query, not the outer one: the outer SELECT sees
	// only the exploded value column `v`, so a conjunct applied there
	// could not reference a span attribute at all.
	conjunctAt := strings.Index(gotSQL, tagValuesConjunct)
	outerAt := strings.Index(gotSQL, tagValuesOuterWhere)
	if outerAt < 0 {
		t.Fatalf("outer empty-value filter missing from the lookup: %s", gotSQL)
	}
	if conjunctAt > outerAt {
		t.Errorf("narrowing conjunct landed in the OUTER query, where the span row is out of scope: %s", gotSQL)
	}
	// The filter is appended to the window conjuncts, never a
	// replacement for them.
	if got, want := strings.Count(gotSQL, "`Timestamp`"), strings.Count(baseSQL, "`Timestamp`"); got != want {
		t.Errorf("window conjuncts changed: %d vs %d\n  base: %s\n   got: %s", got, want, baseSQL, gotSQL)
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

// TestSearchTagValuesQuery_NarrowsIntrinsicLookupSQL covers the other
// values-SQL branch. An intrinsic reads a dedicated column through a flat
// DISTINCT projection — there is no subquery — so the conjunct lands as a
// trailing AND on the one query. Asserting the prefix pins both facts at
// once: the window survives, and nothing else moved.
func TestSearchTagValuesQuery_NarrowsIntrinsicLookupSQL(t *testing.T) {
	t.Parallel()

	_, _, baseSQL, _ := tagValuesLookup(t, tagValuesIntrinsicKey, "", false, tagValuesRouteV2Path, nil)
	status, body, gotSQL, gotArgs := tagValuesLookup(
		t, tagValuesIntrinsicKey, tagValuesQuery, true, tagValuesRouteV2Path, nil,
	)
	if status != http.StatusOK {
		t.Fatalf("filtered lookup: status=%d body=%s", status, body)
	}
	if !strings.HasPrefix(gotSQL, baseSQL+" AND ") {
		t.Fatalf("narrowed SQL is not the unfiltered SQL plus one conjunct\n  base: %s\n   got: %s", baseSQL, gotSQL)
	}
	extra := strings.TrimPrefix(gotSQL, baseSQL+" AND ")
	if !strings.Contains(extra, "`SpanAttributes`") {
		t.Errorf("added conjunct does not read the span attribute map: %s", extra)
	}
	if strings.Contains(gotSQL, "GET") {
		t.Errorf("query literal spliced into SQL text: %s", gotSQL)
	}
	if !argsBind(gotArgs, "http.method", "GET") {
		t.Errorf("args do not bind the attribute key and value: %v", gotArgs)
	}
}

// TestSearchTagValuesQuery_NarrowsTheAnswer drives the narrowing end to
// end: the stub answers the narrowed lookup with a strictly smaller value
// set than the unfiltered one, and the assertion is that the RESPONSE —
// not just the SQL — carries the smaller set. This is the in-process twin
// of the compatibility corpus case tag_values_v2_scoped_by_query, which
// pins the same subset relation against reference Tempo.
func TestSearchTagValuesQuery_NarrowsTheAnswer(t *testing.T) {
	t.Parallel()

	// Keyed on the conjunct only the filtered lookup carries.
	narrowed := map[string][]string{tagValuesConjunct: {"0"}}
	// Carried by spans `{ span.http.method = "GET" }` does not select.
	const unfiltered = "3"

	stub := &stubQuerier{strings: []string{"0", unfiltered}, stringsBySQL: narrowed}
	srv := newServer(stub, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + tagValuesURL(tagValuesRouteV2Path, tagValuesKey, tagValuesQuery, true))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	got := decodeTagValues(t, tagValuesRouteV2Path, resp)
	if len(got) == 0 {
		t.Fatal("no values returned")
	}
	if !contains(got, "0") {
		t.Errorf("narrowed answer dropped the selected value: %v", got)
	}
	if contains(got, unfiltered) {
		t.Errorf("value %q the query does not select leaked into the answer: %v", unfiltered, got)
	}
}

// TestSearchTagValuesV1Query_Ignored is the parity fact the V2 tests
// above cannot state: V1 does not take `q`, so every shape of it — one
// that would narrow, one that cannot be parsed, one that lowers to
// something no row predicate describes — answers 200 with the same value
// set and the same SQL a request without `q` gets.
//
// This is the compatibility corpus case tag_values_v1_query_ignored in
// miniature: reference Tempo answers `{ span.http.method = "GET" }` on
// the V1 values route with the whole window's values, including the ones
// carried only by spans that query does not select.
func TestSearchTagValuesV1Query_Ignored(t *testing.T) {
	t.Parallel()

	// Answered only when the lookup carries the narrowing conjunct, so a
	// V1 route that pushed `q` down would return this shorter set.
	narrowed := map[string][]string{tagValuesConjunct: {"0"}}
	// Carried by spans `{ span.http.method = "GET" }` does not select.
	const unfiltered = "3"

	baseStatus, baseBody, baseSQL, _ := tagValuesLookup(t, tagValuesKey, "", false, tagValuesRouteV1Path, narrowed)
	if baseStatus != http.StatusOK {
		t.Fatalf("unfiltered lookup: status=%d body=%s", baseStatus, baseBody)
	}

	for name, query := range map[string]string{
		"narrowing":        tagValuesQuery,
		"malformed":        "{{{",
		"metrics pipeline": `{} | rate()`,
		"structural":       `{ span.a = 1 } >> { span.b = 2 }`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := &stubQuerier{strings: []string{"0", unfiltered}, stringsBySQL: narrowed}
			srv := newServer(stub, "v1.0.0-test")
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + tagValuesURL(tagValuesRouteV1Path, tagValuesKey, query, true))
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status: want 200, got %d body=%s", resp.StatusCode, readBody(t, resp))
			}
			got := decodeTagValues(t, tagValuesRouteV1Path, resp)
			if !contains(got, unfiltered) {
				t.Errorf("V1 narrowed its answer on q=%q: %v", query, got)
			}
			if stub.lastSQL != baseSQL {
				t.Errorf("V1 q=%q changed the SQL\n  base: %s\n   got: %s", query, baseSQL, stub.lastSQL)
			}
		})
	}
}

// TestSearchTagValuesQuery_AbsentRendersIdenticalSQL is the other half of
// the contract: `q` is opt-in, so a request that omits it — and a request
// that sends it empty, which is what Grafana does before the user has
// typed anything — must render byte-identical SQL to the pre-#1932
// handler. A filter that leaked a no-op `AND 1` would change part pruning
// on a table this endpoint already has to be careful with. Both value-SQL
// branches are driven, because they append the conjunct in different
// places and could each grow a different no-op.
func TestSearchTagValuesQuery_AbsentRendersIdenticalSQL(t *testing.T) {
	t.Parallel()

	for _, route := range tagValuesRoutes {
		for _, key := range []string{tagValuesKey, tagValuesIntrinsicKey} {
			t.Run(route+" "+key, func(t *testing.T) {
				t.Parallel()
				_, _, absentSQL, absentArgs := tagValuesLookup(t, key, "", false, route, nil)
				_, _, emptySQL, emptyArgs := tagValuesLookup(t, key, "", true, route, nil)
				if emptySQL != absentSQL {
					t.Errorf("empty q changed the SQL\n  absent: %s\n   empty: %s", absentSQL, emptySQL)
				}
				if len(emptyArgs) != len(absentArgs) {
					t.Errorf("empty q changed the bound args: %v vs %v", emptyArgs, absentArgs)
				}
			})
		}
	}
}

// TestSearchTagValuesQuery_UnextractableFallsBackUnfiltered is the value-side
// twin of the tag-name fallback contract.
func TestSearchTagValuesQuery_UnextractableFallsBackUnfiltered(t *testing.T) {
	t.Parallel()

	_, _, baseSQL, baseArgs := tagValuesLookup(t, tagValuesKey, "", false, tagValuesRouteV2Path, nil)
	for name, query := range map[string]string{
		"malformed":        "{{{",
		"metrics pipeline": `{} | rate()`,
		"structural":       `{ span.a = 1 } >> { span.b = 2 }`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			status, body, sql, args := tagValuesLookup(t, tagValuesKey, query, true, tagValuesRouteV2Path, nil)
			if status != http.StatusOK {
				t.Fatalf("status: want 200, got %d body=%s", status, body)
			}
			if sql != baseSQL {
				t.Errorf("fallback query changed SQL\n  base: %s\n   got: %s", baseSQL, sql)
			}
			if len(args) != len(baseArgs) {
				t.Errorf("fallback query changed bound args: %v vs %v", args, baseArgs)
			}
		})
	}
}

func TestSearchTagValuesQuery_IncompleteMatcherKeepsValidConjunct(t *testing.T) {
	t.Parallel()

	query := `{ span.http.method = "GET" && resource.cluster = }`
	status, body, sql, args := tagValuesLookup(t, tagValuesKey, query, true, tagValuesRouteV2Path, nil)
	if status != http.StatusOK {
		t.Fatalf("status: want 200, got %d body=%s", status, body)
	}
	if !strings.Contains(sql, tagValuesConjunct) || !argsBind(args, "http.method", "GET") {
		t.Fatalf("valid conjunct did not narrow the lookup: sql=%s args=%v", sql, args)
	}
	if argsBind(args, "cluster") {
		t.Errorf("incomplete matcher survived in bound args: %v", args)
	}
}

// decodeTagValues flattens either envelope onto the value list, so the
// route tables above can assert on both with one comparison.
func decodeTagValues(t *testing.T, route string, resp *http.Response) []string {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status=%d", route, resp.StatusCode)
	}
	if strings.Contains(route, "/v2/") {
		var body tempo.SearchTagValuesResponseV2
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode V2: %v", err)
		}
		out := make([]string, 0, len(body.TagValues))
		for _, v := range body.TagValues {
			out = append(out, v.Value)
		}
		return out
	}
	var body tempo.SearchTagValuesResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode V1: %v", err)
	}
	return body.TagValues
}
