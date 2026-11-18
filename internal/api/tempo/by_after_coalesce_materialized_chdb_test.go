//go:build chdb

package tempo_test

import (
	"net/url"
	"strings"
	"testing"
)

// TestByAfterCoalesceReadsMaterializedKey_ChDB groups by a materialized
// attribute after coalesce(), on /api/search and as a metrics query after
// by(). coalesce() merges each trace's spansets into one, and the rows it
// returns must still carry what the group key reads, so the answer must equal
// the one the same data gives with materialization off.
func TestByAfterCoalesceReadsMaterializedKey_ChDB(t *testing.T) {
	materialized := newMaterializedServer(t, true, materializedSelectSeed)
	mapOnly := newMaterializedServer(t, false, materializedSelectSeed)
	metrics := func(q string) string {
		return "/api/metrics/query_range?" + url.Values{
			"q": {q}, "start": {"1777633200"}, "end": {"1777636800"}, "step": {"3600s"},
		}.Encode()
	}
	search := func(q string) string {
		return "/api/search?q=" + url.QueryEscape(q) + "&start=1777593600&end=1777680000&limit=20&spss=20"
	}
	for _, tc := range []struct {
		query   string
		path    func(string) string
		carries string
	}{
		{`{ } | coalesce() | by(span.rpc.method)`, search, "c0000000000000000000000000000001"},
		{`{ } | coalesce() | by(span.http.status_code)`, search, "c0000000000000000000000000000001"},
		{`{ kind = client } | coalesce() | by(.http.status_code)`, search, "c0000000000000000000000000000001"},
		{`{ } | coalesce() | by(resource.k8s.namespace.name)`, search, "c0000000000000000000000000000001"},
		{`{ } | by(span.http.status_code) | count() > 0 | rate()`, metrics, `"samples":[{`},
		{`{ } | coalesce() | by(span.rpc.method) | count() > 0 | rate()`, metrics, `"samples":[{`},
		{`{ } | coalesce() | rate() by (span.http.status_code)`, metrics, "503"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			got := materializedAttributeBody(t, materialized, tc.path(tc.query))
			if want := materializedAttributeBody(t, mapOnly, tc.path(tc.query)); got != want {
				t.Errorf("materialized answer differs from the map-only answer\n materialized: %s\n map-only:     %s", got, want)
			}
			if !strings.Contains(got, tc.carries) {
				t.Errorf("answer does not carry %q: %s", tc.carries, got)
			}
		})
	}
}
